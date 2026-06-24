// Command buildd is the buildcat control plane: a controller-runtime manager that
// reconciles BuildProject -> StatefulSet-of-1 vanilla buildkitd, plus an HTTP API
// (/route, /heartbeat) the CLI and companion sidecars call.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"net/http"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	buildcatv1 "github.com/socialgouv/buildcat/api/v1alpha1"
	"github.com/socialgouv/buildcat/internal/builder"
	"github.com/socialgouv/buildcat/internal/controller"
	"github.com/socialgouv/buildcat/internal/metrics"
	"github.com/socialgouv/buildcat/internal/router"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

var scheme = runtime.NewScheme()

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(buildcatv1.AddToScheme(scheme))
	utilruntime.Must(volumesnapshotv1.AddToScheme(scheme))
}

func main() {
	var (
		cfg         builder.Config
		apiListen   string
		routeWait   time.Duration
		kubeContext string
	)
	flag.StringVar(&kubeContext, "context", "", "kubeconfig context to target (empty = current-context)")
	flag.StringVar(&cfg.Namespace, "namespace", "buildcat", "namespace the daemons run in")
	flag.StringVar(&cfg.BuildkitImage, "buildkit-image", "moby/buildkit:v0.18.2-rootless", "buildkitd image (vanilla)")
	flag.StringVar(&cfg.CompanionImage, "companion-image", "ghcr.io/socialgouv/buildcat-companion:dev", "companion sidecar image")
	flag.StringVar(&cfg.DaemonCertsSecret, "daemon-certs-secret", "buildkit-daemon-certs", "mTLS server certs secret")
	flag.StringVar(&cfg.BuildkitdConfigMap, "buildkitd-configmap", "buildkitd-config", "ConfigMap holding buildkitd.toml")
	flag.StringVar(&cfg.BuilddURL, "buildd-url", "http://buildd.buildcat.svc:8080", "companion heartbeat target")
	flag.BoolVar(&cfg.Companion, "companion", true, "include the companion sidecar in builder pods")
	flag.StringVar(&cfg.DaemonServiceType, "daemon-service-type", "", "ClusterIP (default) | LoadBalancer (expose daemons externally for off-cluster CI)")
	flag.StringVar(&cfg.SnapshotClass, "snapshot-class", "", "VolumeSnapshotClass for durability snapshots (empty = disabled)")
	keepSnaps := flag.Int("keep-snapshots", 3, "durability snapshots retained per project")
	maxCold := flag.Int("max-cold-starts", 8, "max concurrent cold-start attaches (bench C backpressure)")
	metricsAddr := flag.String("metrics-addr", ":8081", "Prometheus metrics bind address")
	flag.StringVar(&apiListen, "api-listen", ":8080", "address for the /route + /heartbeat HTTP API")
	port := flag.Int("port", 1234, "buildkitd mTLS port")
	healthPort := flag.Int("health-port", 8080, "companion health port")
	flag.DurationVar(&routeWait, "route-wait", 180*time.Second, "max wait for a daemon to become Ready on /route")
	leaderElect := flag.Bool("leader-elect", false, "enable leader election for HA (run >1 replica; only the leader reconciles)")
	flag.Parse()
	cfg.Port = int32(*port)
	cfg.HealthPort = int32(*healthPort)

	ctrl.SetLogger(zap.New(zap.UseDevMode(true)))
	log := ctrl.Log.WithName("buildd")

	restCfg, err := crconfig.GetConfigWithContext(kubeContext)
	if err != nil {
		log.Error(err, "unable to load kubeconfig")
		panic(err)
	}
	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: *metricsAddr}, // M4 observability
		LeaderElection:          *leaderElect,                                     // HA: only the leader reconciles
		LeaderElectionID:        "buildcat-buildd.buildcat.dev",
		LeaderElectionNamespace: cfg.Namespace,
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		panic(err)
	}

	if err := (&controller.BuildProjectReconciler{
		Client:        mgr.GetClient(),
		Scheme:        mgr.GetScheme(),
		Cfg:           cfg,
		KeepSnapshots: *keepSnaps,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller")
		panic(err)
	}

	if err := mgr.Add(&routeServer{c: mgr.GetClient(), cfg: cfg, addr: apiListen, wait: routeWait, coldStartSem: make(chan struct{}, *maxCold)}); err != nil {
		log.Error(err, "unable to add route server")
		panic(err)
	}

	log.Info("starting buildd", "namespace", cfg.Namespace, "api", apiListen)
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		log.Error(err, "manager exited")
		panic(err)
	}
}

// routeServer is the synchronous routing + heartbeat API, run as a manager Runnable
// so it shares the manager's lifecycle and (started) client cache.
type routeServer struct {
	c            client.Client
	cfg          builder.Config
	addr         string
	wait         time.Duration
	coldStartSem chan struct{} // bounds concurrent cold-start attaches (bench C backpressure)
}

// NeedLeaderElection makes the /route API run on EVERY replica (not just the leader) so the
// Service load-balances across all buildd pods. Reconciliation still runs only on the leader.
func (s *routeServer) NeedLeaderElection() bool { return false }

func (s *routeServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/route", s.handleRoute)
	mux.HandleFunc("/prewarm", s.handlePrewarm)
	mux.HandleFunc("/promote", s.handlePromote)
	mux.HandleFunc("/heartbeat", s.handleHeartbeat)
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

// forkIdleTimeoutSec is the short idle window for ephemeral fork-PR daemons — they serve a one-off
// untrusted build, not a warm project, so they scale to zero fast (vs the canonical default).
const forkIdleTimeoutSec = 300

// decodeReq enforces POST and decodes a RouteRequest, writing the HTTP error itself. Returns
// ok=false when the caller should return immediately — the shared preamble for the POST handlers.
func decodeReq(w http.ResponseWriter, r *http.Request) (router.RouteRequest, bool) {
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
func canonicalSpec(req router.RouteRequest) buildcatv1.BuildProjectSpec {
	return buildcatv1.BuildProjectSpec{
		Key:    router.ProjectKey(req.Repo, req.Target, req.Arch),
		Repo:   router.NormalizeRepo(req.Repo),
		Target: router.NormalizeTarget(req.Target),
		Arch:   router.NormalizeArch(req.Arch),
	}
}

// handleRoute resolves the project key, ensures a BuildProject exists, waits for the
// daemon to be Ready, and returns the mTLS endpoint to build against.
func (s *routeServer) handleRoute(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeReq(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	start := time.Now()
	spec := canonicalSpec(req)
	canonical := spec.Key
	key, result := canonical, "warm"
	if req.Untrusted {
		// Fork PR: ephemeral daemon seeded read-only from the canonical snapshot, no write-back
		// (SnapshotEverySec stays 0) — distinct key, so it can never poison the canonical cache.
		key, result = router.ForkKey(canonical), "untrusted"
		var canon buildcatv1.BuildProject
		if err := s.c.Get(ctx, types.NamespacedName{Name: canonical, Namespace: s.cfg.Namespace}, &canon); err == nil {
			spec.RestoreFromSnapshot = canon.Status.LastSnapshot
		}
		spec.Key = key
		spec.Tier = buildcatv1.TierWarm
		spec.IdleTimeoutSec = forkIdleTimeoutSec
	}

	respond := func() {
		metrics.RoutesTotal.WithLabelValues(result).Inc()
		metrics.RouteDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
		writeJSON(w, router.RouteResponse{Key: key, Endpoint: s.endpointFor(ctx, key), Namespace: s.cfg.Namespace})
	}

	if err := s.ensureBuildProject(ctx, spec); err != nil {
		metrics.RoutesTotal.WithLabelValues("error").Inc()
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.touchLastBuild(ctx, key)

	if s.ready(ctx, key) { // warm: no cold-start gating
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

	if err := s.waitReady(ctx, key); err != nil {
		metrics.RoutesTotal.WithLabelValues("error").Inc()
		http.Error(w, "daemon not ready: "+err.Error(), http.StatusGatewayTimeout)
		return
	}
	respond()
}

func (s *routeServer) ensureBuildProject(ctx context.Context, spec buildcatv1.BuildProjectSpec) error {
	var bp buildcatv1.BuildProject
	err := s.c.Get(ctx, types.NamespacedName{Name: spec.Key, Namespace: s.cfg.Namespace}, &bp)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	return s.c.Create(ctx, &buildcatv1.BuildProject{
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

// endpointFor returns the address clients dial: the daemon's external LoadBalancer endpoint when
// the gateway is enabled (off-cluster CI), else the in-cluster Service DNS.
func (s *routeServer) endpointFor(ctx context.Context, key string) string {
	internal := router.Endpoint(key, s.cfg.Namespace, s.cfg.Port)
	if s.cfg.DaemonServiceType != string(corev1.ServiceTypeLoadBalancer) {
		return internal
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		var svc corev1.Service
		if err := s.c.Get(ctx, types.NamespacedName{Name: router.DaemonName(key), Namespace: s.cfg.Namespace}, &svc); err == nil {
			for _, ing := range svc.Status.LoadBalancer.Ingress {
				host := ing.IP
				if host == "" {
					host = ing.Hostname
				}
				if host != "" {
					return router.EndpointHost(host, s.cfg.Port)
				}
			}
		}
		if time.Now().After(deadline) {
			return internal
		}
		select {
		case <-ctx.Done():
			return internal
		case <-time.After(3 * time.Second):
		}
	}
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
	req, ok := decodeReq(w, r)
	if !ok {
		return
	}
	spec := canonicalSpec(req)
	key := spec.Key
	if err := s.ensureBuildProject(r.Context(), spec); err != nil {
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.touchLastBuild(r.Context(), key)
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, router.RouteResponse{Key: key, Endpoint: router.Endpoint(key, s.cfg.Namespace, s.cfg.Port), Namespace: s.cfg.Namespace})
}

// handleHeartbeat is a liveness ack from a companion. It deliberately does NOT bump
// LastBuildTime: heartbeats fire continuously and would defeat idle scale-to-zero. Build
// activity is signalled by /route and /prewarm. (Long builds exceeding IdleTimeoutSec would need
// true in-flight tracking via status.inflightBuilds; until then keep IdleTimeoutSec generous.)
func (s *routeServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var hb router.Heartbeat
	_ = json.NewDecoder(r.Body).Decode(&hb)
	w.WriteHeader(http.StatusNoContent)
}

// handlePromote (M5) bumps the canonical lineage generation. The full pointer swap to the
// most-advanced clone PVC + old-lineage GC is the documented next step (no bbolt merge needed).
func (s *routeServer) handlePromote(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeReq(w, r)
	if !ok {
		return
	}
	key := router.ProjectKey(req.Repo, req.Target, req.Arch)
	var bp buildcatv1.BuildProject
	if err := s.c.Get(r.Context(), types.NamespacedName{Name: key, Namespace: s.cfg.Namespace}, &bp); err != nil {
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	}
	orig := bp.DeepCopy()
	bp.Status.VolumeGen++
	if err := s.c.Status().Patch(r.Context(), &bp, client.MergeFrom(orig)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"key": key, "volumeGen": bp.Status.VolumeGen})
}

// touchLastBuild marks the project active now, which keeps/brings desiredReplicas to 1.
func (s *routeServer) touchLastBuild(ctx context.Context, key string) {
	var bp buildcatv1.BuildProject
	if err := s.c.Get(ctx, types.NamespacedName{Name: key, Namespace: s.cfg.Namespace}, &bp); err != nil {
		return
	}
	now := metav1.Now()
	bp.Status.LastBuildTime = &now
	_ = s.c.Status().Update(ctx, &bp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
