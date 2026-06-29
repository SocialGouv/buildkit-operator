// Command buildd is the buildkit-operator control plane: a controller-runtime manager that
// reconciles BuildProject -> StatefulSet-of-1 vanilla buildkitd, plus an HTTP API
// (/route, /prewarm, /complete) the CLI calls.
//
// The routing API is split across files: server.go (routeServer type + lifecycle + helpers),
// route.go (the HTTP handlers), account.go (inflight accounting + response helpers).
package main

import (
	"flag"
	"os"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/controller"
	"github.com/socialgouv/buildkit-operator/internal/identity"
	k8sprov "github.com/socialgouv/buildkit-operator/internal/provisioner/k8s"
	"golang.org/x/time/rate"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
	crconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(bkov1.AddToScheme(scheme))
	utilruntime.Must(volumesnapshotv1.AddToScheme(scheme))
}

func main() {
	var (
		cfg         builder.Config
		apiListen   string
		routeWait   time.Duration
		kubeContext string
		gatewayHost string
	)
	backend := flag.String("backend", "k8s", "provisioning backend: k8s (Kubernetes StatefulSets) | local (single-host Incus + ZFS)")
	flag.StringVar(&kubeContext, "context", "", "kubeconfig context to target (empty = current-context)")
	flag.StringVar(&cfg.Namespace, "namespace", "buildkit-builds", "namespace the per-project daemons + BuildProjects + their certs/config live in (the 'builds' ns, distinct from the operator ns buildd runs in)")
	flag.StringVar(&cfg.BuildkitImage, "buildkit-image", "moby/buildkit:v0.31.1-rootless", "buildkitd image (vanilla)")
	flag.StringVar(&cfg.CompanionImage, "companion-image", "ghcr.io/socialgouv/buildkit-operator-companion:dev", "companion sidecar image")
	flag.StringVar(&cfg.DaemonCertsSecret, "daemon-certs-secret", "buildkit-daemon-certs", "mTLS server certs secret")
	flag.BoolVar(&cfg.CertManagerCerts, "cert-manager-certs", false, "the daemon certs Secret is cert-manager-issued (keys tls.crt/tls.key/ca.crt); remap them at mount to the .pem filenames buildkitd reads")
	flag.StringVar(&cfg.BuildkitdConfigMap, "buildkitd-configmap", "buildkitd-config", "ConfigMap holding buildkitd.toml")
	flag.BoolVar(&cfg.Companion, "companion", true, "include the companion sidecar in builder pods")
	flag.StringVar(&cfg.SandboxRuntimeClass, "sandbox-runtime-class", "", "RuntimeClass applied to UNTRUSTED fork daemons only (e.g. kata-clh / sysbox-runc); empty = the default runtime")
	flag.StringVar(&cfg.SandboxBuildkitImage, "sandbox-buildkit-image", "", "NON-rootless buildkit image for sandboxed (Kata) forks; empty = derived from --buildkit-image by stripping -rootless")
	daemonScheduling := flag.String("daemon-scheduling", "", `JSON {"nodeSelector":{},"tolerations":[],"affinity":{}} to pin daemon pods to a dedicated build nodepool (empty = cluster default)`)
	flag.StringVar(&gatewayHost, "gateway-host", "", "gateway domain for off-cluster CI: /route returns tcp://<daemon>.<gateway-host>:<port> (empty = in-cluster ClusterIP DNS)")
	gatewayPort := flag.Int("gateway-port", 0, "external port for the gateway SNI endpoint /route returns (0 = same as --port; set 443 when the gateway is fronted on 443, e.g. behind an egress proxy)")
	flag.StringVar(&cfg.SnapshotClass, "snapshot-class", "", "VolumeSnapshotClass for durability snapshots (empty = disabled)")
	keepSnaps := flag.Int("keep-snapshots", 3, "durability snapshots retained per project")
	maxCold := flag.Int("max-cold-starts", 8, "max concurrent cold-start attaches (bench C backpressure)")
	metricsAddr := flag.String("metrics-addr", ":8081", "Prometheus metrics bind address")
	flag.StringVar(&apiListen, "api-listen", ":8080", "address for the /route HTTP API")
	port := flag.Int("port", 1234, "buildkitd mTLS port")
	healthPort := flag.Int("health-port", 8080, "companion health port")
	flag.DurationVar(&routeWait, "route-wait", 180*time.Second, "max wait for a daemon to become Ready on /route")
	leaderElect := flag.Bool("leader-elect", false, "enable leader election for HA (run >1 replica; only the leader reconciles)")
	// OIDC identity verification (secure default): when providers are configured, /route + /prewarm
	// require a forge-signed token and the verified repo claim REPLACES the client's self-declared repo
	// (untrusted is derived server-side). Config comes from a mounted ConfigMap file (--oidc-config) and/or
	// BUILDKIT_OPERATOR_OIDC_* env — both admin-only surfaces, so relaxing/disabling it needs Deployment
	// access (the operator already holds that to run Kata). See internal/identity.
	oidcConfigPath := flag.String("oidc-config", "", "path to a mounted OIDC config file (ConfigMap, YAML/JSON: providers, repoAllowlist, disable). Overlaid by BUILDKIT_OPERATOR_OIDC_* env")
	// Legacy bearer-token auth for the /route API, used ONLY when OIDC is off. Sourced from an env var
	// (not a flag default visible in the pod spec / process list) so it can come from a mounted Secret.
	authToken := os.Getenv("BUILDKIT_OPERATOR_AUTH_TOKEN")
	// Break-glass admin token (distinct header X-Buildkit-Operator-Admin-Token): bypasses OIDC and trusts
	// the request's repo/untrusted — for the manual CLI / in-cluster ops. Env-sourced (mounted Secret).
	adminToken := os.Getenv("BUILDKIT_OPERATOR_ADMIN_TOKEN")
	maxBuildSec := flag.Int("max-build-seconds", 7200, "safety net: inflight builds older than this stop pinning a daemon warm (a missed /complete won't leak a hot daemon forever)")
	// Rate limit for the routing API: a token bucket shared across /route, /prewarm and /complete that
	// caps how fast a single caller (or a compromised token) can churn BuildProjects / attaches. 0 = off.
	apiRateLimit := flag.Float64("api-rate-limit", 50, "max routing-API requests/sec (token bucket; 0 = unlimited)")
	apiRateBurst := flag.Int("api-rate-burst", 100, "routing-API rate-limit burst size")
	// S3 cold cache as a PROJECT policy: buildd hands the per-project cache reference to clients on
	// /route (no creds on the wire); the creds live on the daemons via --s3-creds-secret.
	flag.StringVar(&cfg.S3CredsSecret, "s3-creds-secret", "", "Secret with AWS_ACCESS_KEY_ID/SECRET, mounted as env on the daemons for the s3 cold cache")
	s3Bucket := flag.String("s3-bucket", "", "shared S3 bucket for the cold cache (empty = disabled); buildd returns the per-project reference to clients")
	s3Region := flag.String("s3-region", "us-east-1", "S3 region for the cold cache")
	s3Endpoint := flag.String("s3-endpoint", "", "S3 endpoint URL (OVH Object Storage / MinIO; empty = AWS default)")
	// Single-host backend (--backend local) knobs: one buildkitd Incus instance per project, backed by a
	// retained ZFS dataset (the warm cache). Ignored by the k8s backend.
	incusPool := flag.String("incus-pool", "", "[backend=local] ZFS parent dataset for per-project caches, e.g. tank/bko")
	incusImage := flag.String("incus-image", "", "[backend=local] Incus image providing a vanilla buildkitd")
	incusVMImage := flag.String("incus-vm-image", "", "[backend=local] Incus VM image for untrusted fork isolation (empty = --incus-image)")
	localMountPath := flag.String("local-mount-path", "/var/lib/buildkit", "[backend=local] buildkitd data dir the cache dataset is mounted at")
	localIdle := flag.Duration("local-idle-timeout", 15*time.Minute, "[backend=local] scale an instance to zero after this much idle")
	flag.Parse()
	cfg.Port = int32(*port)
	cfg.HealthPort = int32(*healthPort)

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("buildd")

	// Backend selection: k8s (default, the Kubernetes manager path below) or local (single-host Incus +
	// ZFS, dispatched after the shared OIDC setup since it shares the routing API + identity).
	switch *backend {
	case "k8s", "local":
	default:
		log.Error(nil, "unsupported --backend", "backend", *backend, "supported", "k8s, local")
		panic("unsupported --backend " + *backend)
	}

	if err := cfg.SchedulingFromJSON(*daemonScheduling); err != nil {
		log.Error(err, "invalid --daemon-scheduling")
		panic(err)
	}

	oidcCfg, err := identity.LoadConfig(*oidcConfigPath)
	if err != nil {
		log.Error(err, "invalid OIDC config")
		panic(err)
	}
	verifier, err := identity.NewVerifier(oidcCfg)
	if err != nil {
		log.Error(err, "invalid OIDC providers")
		panic(err)
	}
	if verifier != nil {
		log.Info("OIDC identity verification ENABLED", "providers", len(oidcCfg.Providers), "allowlistSize", len(oidcCfg.RepoAllowlist))
	} else {
		log.Info("OIDC identity verification OFF — /route trusts the caller's repo (bearer/admin token auth only); enable it via --oidc-config for off-cluster CI")
	}

	// Single-host backend: no Kubernetes manager, no kubeconfig — a local provisioner + the same routing
	// API. Dispatched here so it reuses the OIDC verifier + auth tokens built above, then returns.
	if *backend == "local" {
		err := runLocalBackend(localParams{
			cfg: cfg, apiListen: apiListen, routeWait: routeWait, maxCold: *maxCold,
			apiRateLimit: *apiRateLimit, apiRateBurst: *apiRateBurst,
			s3Bucket: *s3Bucket, s3Region: *s3Region, s3Endpoint: *s3Endpoint,
			pool: *incusPool, image: *incusImage, vmImage: *incusVMImage,
			mountPath: *localMountPath, idleTimeout: *localIdle,
		}, verifier, authToken, adminToken, log)
		if err != nil {
			log.Error(err, "local backend exited")
			panic(err)
		}
		return
	}

	restCfg, err := crconfig.GetConfigWithContext(kubeContext)
	if err != nil {
		log.Error(err, "unable to load kubeconfig")
		panic(err)
	}
	// Off-cluster CI reaches every daemon through the SNI gateway at <daemon>.<gateway-host>, so the
	// daemon server cert MUST carry a *.<gateway-host> SAN or clients hit hard-to-debug TLS errors.
	// Warn loudly at boot rather than fail at build time. Non-fatal: in-cluster routing is unaffected.
	if gatewayHost != "" {
		warnIfDaemonCertMissingGatewaySAN(restCfg, cfg, gatewayHost, log)
	}

	// buildd creates daemons + BuildProjects in cfg.Namespace (the 'builds' ns), but its own
	// leader-election Lease belongs in the namespace buildd RUNS in (the 'operator' ns) — handed in via
	// the downward API (POD_NAMESPACE). Fall back to cfg.Namespace for a single-namespace install.
	leaderNS := os.Getenv("POD_NAMESPACE")
	if leaderNS == "" {
		leaderNS = cfg.Namespace
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Cache:                   ctrlcache.Options{DefaultNamespaces: map[string]ctrlcache.Config{cfg.Namespace: {}}},
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr}, // M4 observability
		LeaderElection:          *leaderElect,                                     // HA: only the leader reconciles
		LeaderElectionID:        "buildkit-operator-buildd.buildkit-operator.socialgouv.github.io",
		LeaderElectionNamespace: leaderNS,
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		panic(err)
	}

	if err := (&controller.BuildProjectReconciler{
		Client:          mgr.GetClient(),
		Scheme:          mgr.GetScheme(),
		Cfg:             cfg,
		KeepSnapshots:   *keepSnaps,
		MaxBuildSeconds: *maxBuildSec,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller")
		panic(err)
	}

	// Leader-only backstop GC for orphaned cache PVCs (a project's retained PVC outlives an
	// externally-deleted BuildProject or a crash mid-reap; this reclaims them).
	if err := mgr.Add(&controller.PVCSweeper{Client: mgr.GetClient(), Namespace: cfg.Namespace}); err != nil {
		log.Error(err, "unable to add pvc sweeper")
		panic(err)
	}

	var limiter *rate.Limiter
	if *apiRateLimit > 0 {
		limiter = rate.NewLimiter(rate.Limit(*apiRateLimit), *apiRateBurst)
	}
	prov := k8sprov.New(mgr.GetClient(), cfg, routeWait, gatewayHost, int32(*gatewayPort), ctrl.Log.WithName("provisioner"))
	if err := mgr.Add(&routeServer{
		prov: prov, cfg: cfg, addr: apiListen, wait: routeWait,
		coldStartSem: make(chan struct{}, *maxCold),
		s3Bucket:     *s3Bucket, s3Region: *s3Region, s3Endpoint: *s3Endpoint,
		verifier: verifier, authToken: authToken, adminToken: adminToken,
		limiter: limiter, log: ctrl.Log.WithName("route"),
	}); err != nil {
		log.Error(err, "unable to add route server")
		panic(err)
	}

	log.Info("starting buildd", "namespace", cfg.Namespace, "api", apiListen)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		panic(err)
	}
}
