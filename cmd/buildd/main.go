// Command buildd is the buildkit-operator control plane: a controller-runtime manager that
// reconciles BuildProject -> StatefulSet-of-1 vanilla buildkitd, plus an HTTP API
// (/route, /prewarm) the CLI calls.
package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"os"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/controller"
	"github.com/socialgouv/buildkit-operator/internal/metrics"
	"github.com/socialgouv/buildkit-operator/internal/router"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	flag.StringVar(&kubeContext, "context", "", "kubeconfig context to target (empty = current-context)")
	flag.StringVar(&cfg.Namespace, "namespace", "buildkit-operator", "namespace the daemons run in")
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
	flag.StringVar(&cfg.SnapshotClass, "snapshot-class", "", "VolumeSnapshotClass for durability snapshots (empty = disabled)")
	keepSnaps := flag.Int("keep-snapshots", 3, "durability snapshots retained per project")
	maxCold := flag.Int("max-cold-starts", 8, "max concurrent cold-start attaches (bench C backpressure)")
	metricsAddr := flag.String("metrics-addr", ":8081", "Prometheus metrics bind address")
	flag.StringVar(&apiListen, "api-listen", ":8080", "address for the /route HTTP API")
	port := flag.Int("port", 1234, "buildkitd mTLS port")
	healthPort := flag.Int("health-port", 8080, "companion health port")
	flag.DurationVar(&routeWait, "route-wait", 180*time.Second, "max wait for a daemon to become Ready on /route")
	leaderElect := flag.Bool("leader-elect", false, "enable leader election for HA (run >1 replica; only the leader reconciles)")
	// Bearer-token auth for the /route API. Sourced from an env var (not a flag default visible in the
	// pod spec / process list) so it can come from a mounted Secret. Empty = no auth (in-cluster only).
	authToken := os.Getenv("BUILDKIT_OPERATOR_AUTH_TOKEN")
	maxBuildSec := flag.Int("max-build-seconds", 7200, "safety net: inflight builds older than this stop pinning a daemon warm (a missed /complete won't leak a hot daemon forever)")
	// S3 cold cache as a PROJECT policy: buildd hands the per-project cache reference to clients on
	// /route (no creds on the wire); the creds live on the daemons via --s3-creds-secret.
	flag.StringVar(&cfg.S3CredsSecret, "s3-creds-secret", "", "Secret with AWS_ACCESS_KEY_ID/SECRET, mounted as env on the daemons for the s3 cold cache")
	s3Bucket := flag.String("s3-bucket", "", "shared S3 bucket for the cold cache (empty = disabled); buildd returns the per-project reference to clients")
	s3Region := flag.String("s3-region", "us-east-1", "S3 region for the cold cache")
	s3Endpoint := flag.String("s3-endpoint", "", "S3 endpoint URL (OVH Object Storage / MinIO; empty = AWS default)")
	flag.Parse()
	cfg.Port = int32(*port)
	cfg.HealthPort = int32(*healthPort)

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("buildd")

	if err := cfg.SchedulingFromJSON(*daemonScheduling); err != nil {
		log.Error(err, "invalid --daemon-scheduling")
		panic(err)
	}

	restCfg, err := crconfig.GetConfigWithContext(kubeContext)
	if err != nil {
		log.Error(err, "unable to load kubeconfig")
		panic(err)
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr}, // M4 observability
		LeaderElection:          *leaderElect,                                     // HA: only the leader reconciles
		LeaderElectionID:        "buildkit-operator-buildd.buildkit-operator.socialgouv.github.io",
		LeaderElectionNamespace: cfg.Namespace,
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

	if err := mgr.Add(&routeServer{
		c: mgr.GetClient(), cfg: cfg, addr: apiListen, wait: routeWait,
		coldStartSem: make(chan struct{}, *maxCold), gatewayHost: gatewayHost,
		s3Bucket: *s3Bucket, s3Region: *s3Region, s3Endpoint: *s3Endpoint,
		authToken: authToken,
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

// routeServer is the synchronous routing API (/route, /prewarm), run as a manager Runnable
// so it shares the manager's lifecycle and (started) client cache.
type routeServer struct {
	c            client.Client
	cfg          builder.Config
	addr         string
	wait         time.Duration
	coldStartSem chan struct{} // bounds concurrent cold-start attaches (bench C backpressure)
	// gatewayHost, when set, makes /route return the deterministic SNI endpoint
	// <daemon>.<gatewayHost>:<port> for off-cluster CI (the shared SNI gateway). Empty = in-cluster.
	gatewayHost string
	// S3 cold cache (project policy): the shared bucket reference buildd hands to clients on /route.
	// Credentials are NOT here — they live on the daemons (cfg.S3CredsSecret).
	s3Bucket   string
	s3Region   string
	s3Endpoint string
	// authToken, when non-empty, is required as `Authorization: Bearer <token>` on every API call.
	// Empty = no auth (in-cluster only; do NOT expose /route off-cluster without a token).
	authToken string
}

// authorized reports whether a request carries the configured bearer token (constant-time compare).
// When no token is configured, every request is allowed (in-cluster default).
func (s *routeServer) authorized(r *http.Request) bool {
	if s.authToken == "" {
		return true
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || h[:len(prefix)] != prefix {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(h[len(prefix):]), []byte(s.authToken)) == 1
}

// cacheFor returns the project's cold-cache reference (prefix = the key) when an S3 bucket is
// configured, else nil. No credentials: the daemon holds them via cfg.S3CredsSecret.
func (s *routeServer) cacheFor(key string) *router.CacheConfig {
	if s.s3Bucket == "" {
		return nil
	}
	return &router.CacheConfig{
		Type:        "s3",
		Bucket:      s.s3Bucket,
		Region:      s.s3Region,
		EndpointURL: s.s3Endpoint,
		Name:        key,
	}
}

// NeedLeaderElection makes the /route API run on EVERY replica (not just the leader) so the
// Service load-balances across all buildd pods. Reconciliation still runs only on the leader.
func (s *routeServer) NeedLeaderElection() bool { return false }

func (s *routeServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/route", s.handleRoute)
	mux.HandleFunc("/prewarm", s.handlePrewarm)
	mux.HandleFunc("/complete", s.handleComplete)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })

	srv := &http.Server{Addr: s.addr, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// decodeReq enforces auth + POST and decodes a RouteRequest, writing the HTTP error itself. Returns
// ok=false when the caller should return immediately — the shared preamble for the POST handlers.
func (s *routeServer) decodeReq(w http.ResponseWriter, r *http.Request) (router.RouteRequest, bool) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return router.RouteRequest{}, false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return router.RouteRequest{}, false
	}
	var req router.RouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return router.RouteRequest{}, false
	}
	return req, true
}

// canonicalSpec turns a RouteRequest into the normalized canonical BuildProject spec — the single
// place that derives (key, repo, target, arch), shared by /route and /prewarm.
func canonicalSpec(req router.RouteRequest) bkov1.BuildProjectSpec {
	return bkov1.BuildProjectSpec{
		Key:    router.ProjectKey(req.Repo, req.Name, req.Target, req.Arch),
		Repo:   router.NormalizeRepo(req.Repo),
		Name:   router.NormalizeName(req.Name),
		Target: router.NormalizeTarget(req.Target),
		Arch:   router.NormalizeArch(req.Arch),
	}
}

// handleRoute resolves the project key, ensures a BuildProject exists, waits for the
// daemon to be Ready, and returns the mTLS endpoint to build against.
func (s *routeServer) handleRoute(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeReq(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	start := time.Now()
	spec := canonicalSpec(req)
	canonical := spec.Key
	key, result := canonical, "warm"
	if req.Untrusted {
		// Fork PR: ephemeral daemon derived read-only from the canonical snapshot — distinct key, so
		// it can never poison the canonical cache (anti cache-poisoning). Same derivation policy as
		// fan-out clones, via bkov1.DeriveChild.
		key, result = router.ForkKey(canonical), "untrusted"
		seed := ""
		var canon bkov1.BuildProject
		if err := s.c.Get(ctx, types.NamespacedName{Name: canonical, Namespace: s.cfg.Namespace}, &canon); err == nil {
			seed = canon.Status.LastSnapshot
		}
		spec = bkov1.DeriveChild(spec, seed, bkov1.ForkChild, key)
	}

	respond := func() {
		metrics.RoutesTotal.WithLabelValues(result).Inc()
		metrics.RouteDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
		writeJSON(w, router.RouteResponse{Key: key, Endpoint: s.endpointFor(key), Namespace: s.cfg.Namespace, Cache: s.cacheFor(key)})
	}

	if err := s.ensureBuildProject(ctx, spec); err != nil {
		metrics.RoutesTotal.WithLabelValues("error").Inc()
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Mark a build in flight: keeps the daemon pinned warm for the whole build (not just IdleTimeoutSec
	// from now), and is released by the client's /complete call. The reconciler ignores inflight older
	// than --max-build-seconds, so a missed /complete can't leak a hot daemon forever.
	s.addInflight(ctx, key, +1)
	// The client only calls /complete after a SUCCESSFUL /route, so on any error path below we must
	// release the inflight here — otherwise a failed cold start (504/499) pins the daemon warm for up
	// to --max-build-seconds. respond() (the success path) cancels this by setting routed=true; the
	// release uses a fresh context because the request ctx is already cancelled on the 499 path.
	routed := false
	defer func() {
		if !routed {
			s.addInflight(context.Background(), key, -1)
		}
	}()

	if s.ready(ctx, key) { // warm: no cold-start gating
		routed = true
		respond()
		return
	}
	if result == "warm" {
		result = "cold"
	}
	// Backpressure: cap concurrent Cinder attaches (bench C: bursts serialize into minutes).
	select {
	case s.coldStartSem <- struct{}{}:
		defer func() { <-s.coldStartSem }()
	case <-ctx.Done():
		metrics.RoutesTotal.WithLabelValues("error").Inc()
		http.Error(w, "client gone", 499)
		return
	}
	metrics.ColdStartsInflight.Inc()
	defer metrics.ColdStartsInflight.Dec()

	coldStart := time.Now()
	if err := s.waitReady(ctx, key); err != nil {
		metrics.RoutesTotal.WithLabelValues("error").Inc()
		http.Error(w, "daemon not ready: "+err.Error(), http.StatusGatewayTimeout)
		return
	}
	metrics.ColdStartSeconds.Observe(time.Since(coldStart).Seconds())
	routed = true
	respond()
}

func (s *routeServer) ensureBuildProject(ctx context.Context, spec bkov1.BuildProjectSpec) error {
	var bp bkov1.BuildProject
	err := s.c.Get(ctx, types.NamespacedName{Name: spec.Key, Namespace: s.cfg.Namespace}, &bp)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return s.c.Create(ctx, &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Key, Namespace: s.cfg.Namespace},
		Spec:       spec,
	})
}

// ready reports whether the project's daemon already has a ready replica (warm fast path).
func (s *routeServer) ready(ctx context.Context, key string) bool {
	var sts appsv1.StatefulSet
	if err := s.c.Get(ctx, types.NamespacedName{Name: router.DaemonName(key), Namespace: s.cfg.Namespace}, &sts); err != nil {
		return false
	}
	return sts.Status.ReadyReplicas >= 1
}

// endpointFor returns the address clients dial: a DETERMINISTIC gateway SNI hostname when a gateway
// domain is configured (off-cluster CI reaches every daemon through the single shared SNI gateway),
// else the in-cluster Service DNS. No polling — the endpoint is computable from the key.
func (s *routeServer) endpointFor(key string) string {
	if s.gatewayHost != "" {
		return router.EndpointHost(router.DaemonName(key)+"."+s.gatewayHost, s.cfg.Port)
	}
	return router.Endpoint(key, s.cfg.Namespace, s.cfg.Port)
}

func (s *routeServer) waitReady(ctx context.Context, key string) error {
	deadline := time.Now().Add(s.wait)
	for {
		var sts appsv1.StatefulSet
		err := s.c.Get(ctx, types.NamespacedName{Name: router.DaemonName(key), Namespace: s.cfg.Namespace}, &sts)
		if err == nil && sts.Status.ReadyReplicas >= 1 {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for Ready replica")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// handlePrewarm scales a project toward warm in anticipation (git push / PR webhook) and
// returns immediately — it does NOT wait for readiness; it just masks the future attach latency
// (bench: isolated attach ~19s p50, so pre-warming on push hides it for the CI build that follows).
func (s *routeServer) handlePrewarm(w http.ResponseWriter, r *http.Request) {
	req, ok := s.decodeReq(w, r)
	if !ok {
		return
	}
	spec := canonicalSpec(req)
	key := spec.Key
	if err := s.ensureBuildProject(r.Context(), spec); err != nil {
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.addInflight(r.Context(), key, 0) // touch LastBuildTime without counting an inflight build
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, router.RouteResponse{Key: key, Endpoint: s.endpointFor(key), Namespace: s.cfg.Namespace, Cache: s.cacheFor(key)})
}

// handleComplete releases an inflight build counted by /route (the client calls it when buildx exits,
// success or fail), keyed by the resolved key /route returned. It is best-effort: a missed call is
// bounded by the reconciler's --max-build-seconds safety net, which stops stale inflight from pinning
// a daemon warm forever.
func (s *routeServer) handleComplete(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Key == "" {
		http.Error(w, "bad request: need {\"key\":\"...\"}", http.StatusBadRequest)
		return
	}
	s.addInflight(r.Context(), req.Key, -1)
	w.WriteHeader(http.StatusNoContent)
}

// addInflight adjusts Status.InflightBuilds by delta (floored at 0) and stamps LastBuildTime now. It
// re-Gets and retries on conflict: a single Status().Update that lost a 409 race with the reconciler
// would leave the count wrong, so the project could scale down mid-build (or never scale down).
func (s *routeServer) addInflight(ctx context.Context, key string, delta int32) {
	_ = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var bp bkov1.BuildProject
		if err := s.c.Get(ctx, types.NamespacedName{Name: key, Namespace: s.cfg.Namespace}, &bp); err != nil {
			return err
		}
		n := bp.Status.InflightBuilds + delta
		if n < 0 {
			n = 0
		}
		bp.Status.InflightBuilds = n
		now := metav1.Now()
		bp.Status.LastBuildTime = &now
		return s.c.Status().Update(ctx, &bp)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
