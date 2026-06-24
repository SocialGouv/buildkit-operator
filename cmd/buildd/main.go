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

	buildcatv1 "github.com/devthejo/buildcat/api/v1alpha1"
	"github.com/devthejo/buildcat/internal/builder"
	"github.com/devthejo/buildcat/internal/controller"
	"github.com/devthejo/buildcat/internal/router"
	appsv1 "k8s.io/api/apps/v1"
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
	flag.StringVar(&cfg.CompanionImage, "companion-image", "ovh-registry/buildcat-companion:dev", "companion sidecar image")
	flag.StringVar(&cfg.DaemonCertsSecret, "daemon-certs-secret", "buildkit-daemon-certs", "mTLS server certs secret")
	flag.StringVar(&cfg.BuildkitdConfigMap, "buildkitd-configmap", "buildkitd-config", "ConfigMap holding buildkitd.toml")
	flag.StringVar(&cfg.BuilddURL, "buildd-url", "http://buildd.buildcat.svc:8080", "companion heartbeat target")
	flag.BoolVar(&cfg.Companion, "companion", true, "include the companion sidecar in builder pods")
	flag.StringVar(&apiListen, "api-listen", ":8080", "address for the /route + /heartbeat HTTP API")
	port := flag.Int("port", 1234, "buildkitd mTLS port")
	healthPort := flag.Int("health-port", 8080, "companion health port")
	flag.DurationVar(&routeWait, "route-wait", 180*time.Second, "max wait for a daemon to become Ready on /route")
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
		Scheme:  scheme,
		Metrics: metricsserver.Options{BindAddress: "0"}, // off in M1; obs lands in M4
	})
	if err != nil {
		log.Error(err, "unable to start manager")
		panic(err)
	}

	if err := (&controller.BuildProjectReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Cfg:    cfg,
	}).SetupWithManager(mgr); err != nil {
		log.Error(err, "unable to create controller")
		panic(err)
	}

	if err := mgr.Add(&routeServer{c: mgr.GetClient(), cfg: cfg, addr: apiListen, wait: routeWait}); err != nil {
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
	c    client.Client
	cfg  builder.Config
	addr string
	wait time.Duration
}

func (s *routeServer) Start(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/route", s.handleRoute)
	mux.HandleFunc("/prewarm", s.handlePrewarm)
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

// handleRoute resolves the project key, ensures a BuildProject exists, waits for the
// daemon to be Ready, and returns the mTLS endpoint to build against.
func (s *routeServer) handleRoute(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req router.RouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	key := router.ProjectKey(req.Repo, req.Target, req.Arch)
	ctx := r.Context()

	if err := s.ensureBuildProject(ctx, key, req); err != nil {
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.touchLastBuild(ctx, key) // mark active so the daemon scales up / stays warm
	if err := s.waitReady(ctx, key); err != nil {
		http.Error(w, "daemon not ready: "+err.Error(), http.StatusGatewayTimeout)
		return
	}
	writeJSON(w, router.RouteResponse{
		Key:       key,
		Endpoint:  router.Endpoint(key, s.cfg.Namespace, s.cfg.Port),
		Namespace: s.cfg.Namespace,
	})
}

func (s *routeServer) ensureBuildProject(ctx context.Context, key string, req router.RouteRequest) error {
	var bp buildcatv1.BuildProject
	err := s.c.Get(ctx, types.NamespacedName{Name: key, Namespace: s.cfg.Namespace}, &bp)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	bp = buildcatv1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: s.cfg.Namespace},
		Spec: buildcatv1.BuildProjectSpec{
			Key:    key,
			Repo:   router.NormalizeRepo(req.Repo),
			Target: router.NormalizeTarget(req.Target),
			Arch:   router.NormalizeArch(req.Arch),
		},
	}
	return s.c.Create(ctx, &bp)
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
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req router.RouteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	key := router.ProjectKey(req.Repo, req.Target, req.Arch)
	if err := s.ensureBuildProject(r.Context(), key, req); err != nil {
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.touchLastBuild(r.Context(), key)
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, router.RouteResponse{Key: key, Endpoint: router.Endpoint(key, s.cfg.Namespace, s.cfg.Port), Namespace: s.cfg.Namespace})
}

// handleHeartbeat is a liveness ack from a companion. It deliberately does NOT bump
// LastBuildTime: heartbeats fire continuously and would defeat idle scale-to-zero. Build
// activity is signalled by /route and /prewarm. (Long builds exceeding IdleTimeoutSec need
// true in-flight tracking — lands with the Build CR; until then keep IdleTimeoutSec generous.)
func (s *routeServer) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	var hb struct {
		Key   string `json:"key"`
		Ready bool   `json:"ready"`
		TS    string `json:"ts"`
	}
	_ = json.NewDecoder(r.Body).Decode(&hb)
	w.WriteHeader(http.StatusNoContent)
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
