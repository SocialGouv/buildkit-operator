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
	"github.com/socialgouv/buildkit-operator/internal/provisioner"
	"github.com/socialgouv/buildkit-operator/internal/router"
	"golang.org/x/time/rate"
)

// routeServer is the synchronous routing API (/route, /prewarm, /complete), run as a manager Runnable
// so it shares the manager's lifecycle and (started) client cache.
type routeServer struct {
	prov         provisioner.Provisioner
	cfg          builder.Config
	addr         string
	wait         time.Duration
	coldStartSem chan struct{} // bounds concurrent cold-start attaches (bench C backpressure)
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
//     allowlist (this is the secure default). If verification fails but a legacy bearer (authToken) is
//     still configured and matches, it is accepted as a TRANSITION fallback so callers can migrate to
//     OIDC with no downtime — drop auth.tokenSecret to enforce OIDC strictly;
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
		if err == nil {
			if !s.verifier.AllowRepo(id.Repo) {
				return reqIdentity{}, http.StatusForbidden, fmt.Errorf("repo %q is not in the OIDC allowlist", id.Repo)
			}
			return reqIdentity{repo: id.Repo, untrusted: id.Untrusted, override: true}, 0, nil
		}
		// Transition fallback: while a legacy bearer (authToken) is still configured, accept it so CI
		// consumers can migrate to OIDC at their own pace (zero-downtime rollout). Remove auth.tokenSecret
		// once everyone mints tokens to enforce OIDC strictly (the secure end state).
		if s.authToken != "" && bearerEquals(r, s.authToken) {
			s.log.Info("legacy-bearer fallback (OIDC enabled; migrate this caller)", "path", r.URL.Path, "remote", clientIP(r))
			return reqIdentity{override: false}, 0, nil
		}
		return reqIdentity{}, http.StatusUnauthorized, err
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
