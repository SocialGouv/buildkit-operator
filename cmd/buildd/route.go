package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/socialgouv/buildkit-operator/internal/metrics"
	"github.com/socialgouv/buildkit-operator/internal/router"
)

const maxRouteRequestBytes int64 = 8 << 10

// decodeReq enforces auth, the rate limit and POST, then decodes a RouteRequest, writing the HTTP
// error itself. Returns ok=false when the caller should return immediately — the shared preamble for
// the POST handlers.
func (s *routeServer) decodeReq(w http.ResponseWriter, r *http.Request) (router.RouteRequest, bool) {
	id, status, err := s.identify(r)
	if err != nil {
		s.log.Info("denied", "path", r.URL.Path, "remote", clientIP(r), "err", err.Error())
		http.Error(w, http.StatusText(status), status)
		return router.RouteRequest{}, false
	}
	if !s.allow(w) {
		return router.RouteRequest{}, false
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return router.RouteRequest{}, false
	}
	var req router.RouteRequest
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return router.RouteRequest{}, false
	}
	// OIDC path: the verified token is the SOLE source of identity — overwrite the client's self-declared
	// repo with the forge-signed one, and only ever ADD untrusted isolation (a fork/PR build can't claim
	// trusted). This is what kills cross-repo cache poisoning. Validation runs on the final (bound) repo.
	if id.override {
		req.Repo = id.repo
		req.Untrusted = req.Untrusted || id.untrusted
	}
	if err := validateRouteRequest(req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return router.RouteRequest{}, false
	}
	return req, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRouteRequestBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("body must contain a single JSON object")
	}
	return nil
}

func validateRouteRequest(req router.RouteRequest) error {
	if router.NormalizeRepo(req.Repo) == "" {
		return errors.New("repo is required")
	}
	if len(req.Repo) > 512 {
		return errors.New("repo is too long")
	}
	if len(req.Name) > 128 {
		return errors.New("name is too long")
	}
	if len(req.Target) > 128 {
		return errors.New("target is too long")
	}
	switch arch := router.NormalizeArch(req.Arch); arch {
	case "amd64", "arm64":
		return nil
	default:
		return fmt.Errorf("unsupported arch %q", req.Arch)
	}
}

func validateCompleteRequest(key string) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("key is required")
	}
	if len(key) > 64 {
		return errors.New("key is too long")
	}
	return nil
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
		// Fork PR: ephemeral daemon derived read-only from the canonical snapshot — distinct key, so it
		// can never poison the canonical cache (anti cache-poisoning). The key is pure/deterministic here;
		// the provisioner derives the fork's spec (seed + DeriveChild) inside Ensure.
		key, result = router.ForkKey(canonical), "untrusted"
	}
	// Audit trail: every build access is logged with the resolved key + caller, so a security review
	// can reconstruct who built what (the bearer token is never logged).
	s.log.Info("route", "key", key, "repo", spec.Repo, "untrusted", req.Untrusted, "remote", clientIP(r))

	respond := func() {
		metrics.RoutesTotal.WithLabelValues(result).Inc()
		metrics.RouteDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
		writeJSON(s.log, w, router.RouteResponse{Key: key, Endpoint: s.prov.Endpoint(key), Namespace: s.cfg.Namespace, Ready: true, Cache: s.cacheFor(key)})
	}

	if err := s.prov.Ensure(ctx, spec, req.Untrusted); err != nil {
		metrics.RoutesTotal.WithLabelValues("error").Inc()
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Mark a build in flight: keeps the daemon pinned warm for the whole build (not just IdleTimeoutSec
	// from now), and is released by the client's /complete call. The reconciler ignores inflight older
	// than --max-build-seconds, so a missed /complete can't leak a hot daemon forever.
	s.prov.AddInflight(ctx, key, +1)
	// The client only calls /complete after a SUCCESSFUL /route, so on any error path below we must
	// release the inflight here — otherwise a failed cold start (504/499) pins the daemon warm for up
	// to --max-build-seconds. respond() (the success path) cancels this by setting routed=true; the
	// release uses a fresh context because the request ctx is already cancelled on the 499 path.
	routed := false
	defer func() {
		if !routed {
			s.prov.AddInflight(context.Background(), key, -1)
		}
	}()

	if s.prov.Ready(ctx, key) { // warm: no cold-start gating
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
	if err := s.prov.WaitReady(ctx, key); err != nil {
		metrics.RoutesTotal.WithLabelValues("error").Inc()
		http.Error(w, "daemon not ready: "+err.Error(), http.StatusGatewayTimeout)
		return
	}
	metrics.ColdStartSeconds.Observe(time.Since(coldStart).Seconds())
	routed = true
	respond()
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
	if err := s.prov.Ensure(r.Context(), spec, false); err != nil {
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.prov.AddInflight(r.Context(), key, 0) // touch LastBuildTime without counting an inflight build
	// Report readiness so a proxy-tunnelled client can poll /prewarm (cheap, non-blocking) until the
	// daemon is warm, then route — instead of holding a blocking /route past the proxy's tunnel timeout.
	ready := s.prov.Ready(r.Context(), key)
	w.WriteHeader(http.StatusAccepted)
	writeJSON(s.log, w, router.RouteResponse{Key: key, Endpoint: s.prov.Endpoint(key), Namespace: s.cfg.Namespace, Ready: ready, Cache: s.cacheFor(key)})
}

// handleComplete releases an inflight build counted by /route (the client calls it when buildx exits,
// success or fail), keyed by the resolved key /route returned. It is best-effort: a missed call is
// bounded by the reconciler's --max-build-seconds safety net, which stops stale inflight from pinning
// a daemon warm forever.
func (s *routeServer) handleComplete(w http.ResponseWriter, r *http.Request) {
	// /complete only decrements an inflight counter by key; it needs an authenticated caller but not repo
	// binding (the key was already returned by a verified /route), so the identity override is ignored.
	if _, status, err := s.identify(r); err != nil {
		s.log.Info("denied", "path", r.URL.Path, "remote", clientIP(r), "err", err.Error())
		http.Error(w, http.StatusText(status), status)
		return
	}
	if !s.allow(w) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Key string `json:"key"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateCompleteRequest(req.Key); err != nil {
		http.Error(w, "bad request: need {\"key\":\"...\"}", http.StatusBadRequest)
		return
	}
	s.prov.AddInflight(r.Context(), req.Key, -1)
	w.WriteHeader(http.StatusNoContent)
}
