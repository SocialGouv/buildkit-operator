package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"time"

	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/router"
	"golang.org/x/time/rate"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// routeServer is the synchronous routing API (/route, /prewarm, /complete), run as a manager Runnable
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
	// gatewayPort, when > 0, is the EXTERNAL port /route advertises for the gateway endpoint (e.g. 443
	// when the gateway is fronted on 443 behind an egress proxy). 0 = use cfg.Port (the daemon port).
	gatewayPort int32
	// S3 cold cache (project policy): the shared bucket reference buildd hands to clients on /route.
	// Credentials are NOT here — they live on the daemons (cfg.S3CredsSecret).
	s3Bucket   string
	s3Region   string
	s3Endpoint string
	// authToken, when non-empty, is required as `Authorization: Bearer <token>` on every API call.
	// Empty = no auth (in-cluster only; do NOT expose /route off-cluster without a token).
	authToken string
	// limiter, when non-nil, caps the routing-API request rate (token bucket shared across endpoints)
	// so a single caller / compromised token can't churn BuildProjects + attaches without bound.
	limiter *rate.Limiter
	log     logr.Logger
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
	if s.s3Bucket == "" || router.IsForkKey(key) {
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

	writeTimeout := s.wait + 30*time.Second
	if writeTimeout < 30*time.Second {
		writeTimeout = 30 * time.Second
	}
	srv := &http.Server{
		Addr:              s.addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       60 * time.Second,
	}
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

// allow enforces the shared API rate limit, writing 429 itself when exhausted. Returns true to proceed.
func (s *routeServer) allow(w http.ResponseWriter) bool {
	if s.limiter == nil || s.limiter.Allow() {
		return true
	}
	http.Error(w, "rate limited", http.StatusTooManyRequests)
	return false
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
		port := s.cfg.Port
		if s.gatewayPort > 0 {
			port = s.gatewayPort
		}
		return router.EndpointHost(router.DaemonName(key)+"."+s.gatewayHost, port)
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
