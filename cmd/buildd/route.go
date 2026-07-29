package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
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

// newBuildID mints the token that identifies ONE routed build across /route and /complete. Random
// (not a counter) because buildd runs several replicas with no shared sequence; 8 bytes make a
// collision within a project's inflight set irrelevant. crypto/rand.Read never fails on the
// platforms we run (it panics on a broken entropy source rather than returning short reads).
func newBuildID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

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
	s.defaults.Apply(&spec)
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

	// One token per routed build, echoed back on /complete so the release lands on THIS build's entry
	// and not on a sibling's — the project's inflight set is per-build, not a counter.
	buildID := newBuildID()

	respond := func() {
		metrics.RoutesTotal.WithLabelValues(result).Inc()
		metrics.RouteDuration.WithLabelValues(result).Observe(time.Since(start).Seconds())
		writeJSON(s.log, w, router.RouteResponse{Key: key, BuildID: buildID, Endpoint: s.prov.Endpoint(key), Namespace: s.cfg.Namespace, Ready: true, Cache: s.cacheFor(ctx, key, true)})
	}

	if err := s.prov.Ensure(ctx, spec, req.Untrusted); err != nil {
		metrics.RoutesTotal.WithLabelValues("error").Inc()
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// Mark a build in flight: keeps the daemon pinned warm for the whole build (not just IdleTimeoutSec
	// from now), and is released by the client's /complete call. The reconciler expires an entry older
	// than --max-build-seconds, so a missed /complete can't leak a hot daemon forever.
	s.prov.StartInflight(ctx, key, buildID)
	// The client only calls /complete after a SUCCESSFUL /route, so on any error path below we must
	// release the inflight here — otherwise a failed cold start (504/499) pins the daemon warm for up
	// to --max-build-seconds. respond() (the success path) cancels this by setting routed=true; the
	// release uses a fresh context because the request ctx is already cancelled on the 499 path.
	routed := false
	defer func() {
		if !routed {
			s.prov.EndInflight(context.Background(), key, buildID)
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
	s.defaults.Apply(&spec)
	key := spec.Key
	if err := s.prov.Ensure(r.Context(), spec, false); err != nil {
		http.Error(w, "ensure project: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.prov.Touch(r.Context(), key) // keep the project from idling out without counting a build
	// Report readiness so a proxy-tunnelled client can poll /prewarm (cheap, non-blocking) until the
	// daemon is warm, then route — instead of holding a blocking /route past the proxy's tunnel timeout.
	ready := s.prov.Ready(r.Context(), key)
	w.WriteHeader(http.StatusAccepted)
	// grantExport=false: a prewarm is not a build — it must not consume the cadence window.
	writeJSON(s.log, w, router.RouteResponse{Key: key, Endpoint: s.prov.Endpoint(key), Namespace: s.cfg.Namespace, Ready: ready, Cache: s.cacheFor(r.Context(), key, false)})
}

// handleComplete releases an inflight build registered by /route (the client calls it when buildx exits,
// success or fail), keyed by the resolved key and the buildId /route returned. It is best-effort: a
// missed call is bounded by the reconciler's --max-build-seconds safety net, which expires that build's
// entry on its own clock.
func (s *routeServer) handleComplete(w http.ResponseWriter, r *http.Request) {
	// /complete only releases one inflight entry by key; it needs an authenticated caller but not repo
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
		// BuildID is the token /route returned. A client that predates it omits the field; the
		// provisioner then releases the project's OLDEST entry, which is the pre-buildId behaviour.
		BuildID string `json:"buildId"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := validateCompleteRequest(req.Key); err != nil {
		http.Error(w, "bad request: need {\"key\":\"...\"}", http.StatusBadRequest)
		return
	}
	if len(req.BuildID) > 64 {
		http.Error(w, "bad request: buildId is too long", http.StatusBadRequest)
		return
	}
	s.prov.EndInflight(r.Context(), req.Key, req.BuildID)
	w.WriteHeader(http.StatusNoContent)
}
