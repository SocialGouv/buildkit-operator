package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/identity"
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
	// verifier, when non-nil, enforces OIDC identity verification (secure default): /route and /prewarm
	// require a forge-signed token whose repo claim BECOMES the request's repo (the client can no longer
	// self-declare it), and untrusted is derived server-side. Nil = OIDC off (in-cluster bearer/admin).
	verifier *identity.Verifier
	// authToken, when non-empty, is the LEGACY bearer required as `Authorization: Bearer <token>` when
	// OIDC is off (verifier == nil). Empty + no OIDC = open (in-cluster only).
	authToken string
	// adminToken, when non-empty, is the break-glass admin credential carried in a DISTINCT header
	// (adminTokenHeader). It bypasses OIDC and trusts the request's repo/untrusted as-is — for the manual
	// CLI / in-cluster ops, held only by operators who already control the buildd Deployment. Logged on use.
	adminToken string
	// limiter, when non-nil, caps the routing-API request rate (token bucket shared across endpoints)
	// so a single caller / compromised token can't churn BuildProjects + attaches without bound.
	limiter *rate.Limiter
	log     logr.Logger
}

// adminTokenHeader carries the break-glass admin token — a header DISTINCT from Authorization so admin
// use is unambiguous (no token-shape sniffing) and auditable.
const adminTokenHeader = "X-Buildkit-Operator-Admin-Token" //nolint:gosec // header name, not a credential

// identity carries the authenticated routing decision. When override is true (the OIDC path), repo and
// untrusted MUST replace the client-supplied values; otherwise (admin / legacy) the request is trusted.
type reqIdentity struct {
	repo      string
	untrusted bool
	override  bool
}

// identify authenticates the request and resolves its identity, in precedence order:
//  1. admin break-glass token (distinct header) — trusts the request as-is;
//  2. OIDC (when configured) — verifies the forge token, binds repo + derives untrusted, applies the
//     allowlist (this is the secure default);
//  3. legacy bearer / open — only when OIDC is off (in-cluster).
//
// It returns the resolved identity plus an HTTP status + error to write on rejection (status 0 = ok).
func (s *routeServer) identify(r *http.Request) (reqIdentity, int, error) {
	if s.adminToken != "" {
		if tok := r.Header.Get(adminTokenHeader); tok != "" {
			if subtle.ConstantTimeCompare([]byte(tok), []byte(s.adminToken)) == 1 {
				s.log.Info("admin-token used", "path", r.URL.Path, "remote", clientIP(r))
				return reqIdentity{override: false}, 0, nil
			}
			return reqIdentity{}, http.StatusUnauthorized, errors.New("invalid admin token")
		}
	}
	if s.verifier != nil {
		id, err := s.verifier.Verify(r.Context(), bearerToken(r))
		if err != nil {
			return reqIdentity{}, http.StatusUnauthorized, err
		}
		if !s.verifier.AllowRepo(id.Repo) {
			return reqIdentity{}, http.StatusForbidden, fmt.Errorf("repo %q is not in the OIDC allowlist", id.Repo)
		}
		return reqIdentity{repo: id.Repo, untrusted: id.Untrusted, override: true}, 0, nil
	}
	if s.authToken != "" && !bearerEquals(r, s.authToken) {
		return reqIdentity{}, http.StatusUnauthorized, errors.New("unauthorized")
	}
	return reqIdentity{override: false}, 0, nil
}

// bearerToken returns the token after "Bearer " in the Authorization header (empty if absent).
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) <= len(prefix) || !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return h[len(prefix):]
}

// bearerEquals constant-time compares the Authorization bearer against want.
func bearerEquals(r *http.Request, want string) bool {
	return subtle.ConstantTimeCompare([]byte(bearerToken(r)), []byte(want)) == 1
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
	created := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Key, Namespace: s.cfg.Namespace},
		Spec:       spec,
	}
	if err := s.c.Create(ctx, created); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // raced another /route or /prewarm for the same key — fine
		}
		return err
	}
	// Warm from birth: desiredReplicas only holds a warm-tier replica once LastBuildTime is set, so stamp
	// it now on the JUST-CREATED object (it already carries its ResourceVersion) — not via a fresh Get,
	// whose informer cache can still miss the new object and leave the daemon stuck Idle. That cache race
	// is the cold-start flake: addInflight's Get returned NotFound right after Create, so the touch was
	// dropped and the warm-tier project never scaled up.
	now := metav1.Now()
	created.Status.LastBuildTime = &now
	if err := s.c.Status().Update(ctx, created); err != nil {
		s.log.Error(err, "stamp LastBuildTime at create failed; relying on the addInflight touch", "key", spec.Key)
	}
	return nil
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
