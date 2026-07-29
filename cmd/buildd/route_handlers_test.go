package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/identity"
	"github.com/socialgouv/buildkit-operator/internal/projectdefaults"
	k8sprov "github.com/socialgouv/buildkit-operator/internal/provisioner/k8s"
	"github.com/socialgouv/buildkit-operator/internal/router"
	"golang.org/x/time/rate"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := bkov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func newTestServer(t *testing.T, c client.Client) *routeServer {
	t.Helper()
	cfg := builder.Config{Namespace: "buildkit-operator", Port: 1234}
	// wait=0: with no Ready StatefulSet, WaitReady times out on the first poll (matches the prior
	// zero-value behaviour); the cold-start success test flips readiness via a Get interceptor.
	return &routeServer{
		prov:         k8sprov.New(c, cfg, 0, "", 0, 2*time.Hour, logr.Discard()),
		cfg:          cfg,
		coldStartSem: make(chan struct{}, 1),
	}
}

// /prewarm creates the BuildProject (so the daemon starts attaching ahead of the build) and returns
// 202 + the deterministic endpoint, WITHOUT counting an inflight build (it just touches LastBuildTime).
func TestHandlePrewarm_CreatesProjectNoInflight(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)

	body, _ := json.Marshal(router.RouteRequest{Repo: "github.com/org/repo", Arch: "amd64"})
	rec := httptest.NewRecorder()
	srv.handlePrewarm(rec, httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body)))

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var resp router.RouteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	if resp.Key != key {
		t.Errorf("key = %q, want %q", resp.Key, key)
	}

	var bp bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: key, Namespace: srv.cfg.Namespace}, &bp); err != nil {
		t.Fatalf("project not created: %v", err)
	}
	if bp.Status.InflightBuilds != 0 {
		t.Errorf("InflightBuilds = %d after prewarm, want 0", bp.Status.InflightBuilds)
	}
	// Warm from birth: LastBuildTime is stamped at create so desiredReplicas keeps a warm-tier replica —
	// the cold-start flake was a missing stamp (the daemon stayed Idle). It must be set and recent.
	if bp.Status.LastBuildTime == nil {
		t.Fatal("LastBuildTime not stamped by prewarm")
	}
	if time.Since(bp.Status.LastBuildTime.Time) > time.Minute {
		t.Errorf("LastBuildTime not recent: %v", bp.Status.LastBuildTime.Time)
	}
	// No daemon StatefulSet yet -> not ready; the client polls /prewarm on this until it flips true.
	if resp.Ready {
		t.Error("Ready = true with no daemon StatefulSet, want false")
	}
}

// Admin-declared project defaults seed the auto-created BuildProject's spec — the create-only
// Ensure means this is the only moment a platform rule (tier/idle/cache) can take effect.
func TestHandlePrewarm_AppliesProjectDefaults(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)
	srv.defaults = &projectdefaults.Config{Rules: []projectdefaults.Rule{
		{Repo: "github.com/org/*", Tier: bkov1.TierHot, CacheVolumeGi: 120},
	}}

	body, _ := json.Marshal(router.RouteRequest{Repo: "github.com/org/repo", Arch: "amd64"})
	rec := httptest.NewRecorder()
	srv.handlePrewarm(rec, httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}

	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	var bp bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: key, Namespace: srv.cfg.Namespace}, &bp); err != nil {
		t.Fatalf("project not created: %v", err)
	}
	if bp.Spec.Tier != bkov1.TierHot || bp.Spec.CacheVolumeGi != 120 {
		t.Errorf("spec = tier %q cache %d, want hot/120 (defaults not applied)", bp.Spec.Tier, bp.Spec.CacheVolumeGi)
	}
}

// /prewarm reports Ready=true once the daemon StatefulSet has a ready replica, so a proxy-tunnelled
// client can poll it (non-blocking) instead of holding a blocking /route open.
func TestHandlePrewarm_ReadyWhenDaemonReady(t *testing.T) {
	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: router.DaemonName(key), Namespace: "buildkit-operator"},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1, UpdatedReplicas: 1, CurrentRevision: "r1", UpdateRevision: "r1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(sts).Build()
	srv := newTestServer(t, c)

	body, _ := json.Marshal(router.RouteRequest{Repo: "github.com/org/repo", Arch: "amd64"})
	rec := httptest.NewRecorder()
	srv.handlePrewarm(rec, httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body)))

	var resp router.RouteResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !resp.Ready {
		t.Error("Ready = false with a ready daemon StatefulSet, want true")
	}
}

// /complete releases the entry named by buildId — the caller's OWN build, not whichever happens to
// be first. A client that predates build IDs omits the field and releases the oldest entry.
func TestHandleComplete_ReleasesNamedBuild(t *testing.T) {
	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	older := metav1.NewTime(time.Now().Add(-time.Minute))
	complete := func(t *testing.T, body map[string]string) bkov1.BuildProject {
		t.Helper()
		bp := &bkov1.BuildProject{}
		bp.Name, bp.Namespace = key, "buildkit-operator"
		bp.Status.SetInflight([]bkov1.InflightBuild{
			{ID: bkov1.InflightID("1111111111111111"), Since: older},
			{ID: bkov1.InflightID("2222222222222222"), Since: metav1.Now()},
		})
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
		srv := newTestServer(t, c)

		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		srv.handleComplete(rec, httptest.NewRequest(http.MethodPost, "/complete", bytes.NewReader(raw)))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", rec.Code)
		}
		var got bkov1.BuildProject
		if err := c.Get(context.Background(), types.NamespacedName{Name: key, Namespace: srv.cfg.Namespace}, &got); err != nil {
			t.Fatal(err)
		}
		return got
	}

	got := complete(t, map[string]string{"key": key, "buildId": "2222222222222222"})
	if got.Status.InflightCount() != 1 || got.Status.Inflight[0].ID != bkov1.InflightID("1111111111111111") {
		t.Errorf("inflight = %v, want the named build released and first left", got.Status.Inflight)
	}
	got = complete(t, map[string]string{"key": key}) // no buildId: oldest goes
	if got.Status.InflightCount() != 1 || got.Status.Inflight[0].ID != bkov1.InflightID("2222222222222222") {
		t.Errorf("inflight = %v, want the OLDEST released when no buildId is sent", got.Status.Inflight)
	}
	if got.Status.InflightBuilds != 1 {
		t.Errorf("InflightBuilds projection = %d, want 1", got.Status.InflightBuilds)
	}
}

func TestHandleComplete_RejectsMissingKey(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)
	rec := httptest.NewRecorder()
	srv.handleComplete(rec, httptest.NewRequest(http.MethodPost, "/complete", bytes.NewReader([]byte(`{}`))))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for empty key", rec.Code)
	}
}

// cacheFor: forks get no S3 cache; the default cadence policy grants ONE export per window
// (later routes and prewarm probes import-only); s3CachePolicy=never yields no cache at all.
func TestCacheFor_PolicyForksAndCadence(t *testing.T) {
	canonical := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: canonical, Namespace: "buildkit-operator"},
		Spec:       bkov1.BuildProjectSpec{Key: canonical, Repo: "github.com/org/repo", Arch: "amd64"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
	srv := newTestServer(t, c)
	srv.s3Bucket, srv.s3Region, srv.s3Endpoint = "bucket", "gra", "https://s3.example"
	srv.s3ExportInterval = time.Hour
	ctx := context.Background()

	if got := srv.cacheFor(ctx, router.ForkKey(canonical), true); got != nil {
		t.Fatalf("fork cache = %#v, want nil", got)
	}
	// First routed build of the window: export granted.
	got := srv.cacheFor(ctx, canonical, true)
	if got == nil || got.Name != canonical || got.Bucket != "bucket" || got.SkipExport {
		t.Fatalf("first route cache = %#v, want export granted", got)
	}
	// Second build inside the window: import only.
	if got := srv.cacheFor(ctx, canonical, true); got == nil || !got.SkipExport {
		t.Fatalf("second route cache = %#v, want SkipExport", got)
	}
	// Prewarm probes never grant nor consume the window.
	if got := srv.cacheFor(ctx, canonical, false); got == nil || !got.SkipExport {
		t.Fatalf("prewarm cache = %#v, want SkipExport", got)
	}

	// Policy never: no cache object at all.
	var cur bkov1.BuildProject
	if err := c.Get(ctx, types.NamespacedName{Name: canonical, Namespace: "buildkit-operator"}, &cur); err != nil {
		t.Fatal(err)
	}
	cur.Spec.S3CachePolicy = bkov1.S3CacheNever
	if err := c.Update(ctx, &cur); err != nil {
		t.Fatal(err)
	}
	if got := srv.cacheFor(ctx, canonical, true); got != nil {
		t.Fatalf("never-policy cache = %#v, want nil", got)
	}
}

// The shared rate limiter returns 429 once the burst is exhausted, across the auth'd POST endpoints.
func TestRateLimit_Returns429WhenExhausted(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)
	srv.limiter = rate.NewLimiter(rate.Limit(0.0001), 1) // burst of 1, then deny

	body, _ := json.Marshal(router.RouteRequest{Repo: "github.com/org/repo", Arch: "amd64"})
	// First call consumes the single burst token.
	rec1 := httptest.NewRecorder()
	srv.handlePrewarm(rec1, httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body)))
	if rec1.Code == http.StatusTooManyRequests {
		t.Fatal("first request rate-limited, want it to pass")
	}
	// Second call is denied.
	rec2 := httptest.NewRecorder()
	srv.handlePrewarm(rec2, httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body)))
	if rec2.Code != http.StatusTooManyRequests {
		t.Fatalf("second request status = %d, want 429", rec2.Code)
	}
}

func TestAuthRunsBeforeRateLimit(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)
	srv.authToken = "s3cret"
	srv.limiter = rate.NewLimiter(rate.Limit(0.0001), 1) // one token; unauthorized callers must not burn it

	body, _ := json.Marshal(router.RouteRequest{Repo: "github.com/org/repo", Arch: "amd64"})
	bad := httptest.NewRecorder()
	srv.handlePrewarm(bad, httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body)))
	if bad.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want 401", bad.Code)
	}

	good := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer s3cret")
	srv.handlePrewarm(good, req)
	if good.Code == http.StatusTooManyRequests {
		t.Fatalf("authorized request was rate-limited after an unauthorized request")
	}
	if good.Code != http.StatusAccepted {
		t.Fatalf("authorized status = %d, want 202", good.Code)
	}
}

func TestDecodeReqRejectsUnknownOrOversizedBody(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)

	rec1 := httptest.NewRecorder()
	srv.handlePrewarm(rec1, httptest.NewRequest(http.MethodPost, "/prewarm", strings.NewReader(`{"repo":"github.com/org/repo","arch":"amd64","extra":true}`)))
	if rec1.Code != http.StatusBadRequest {
		t.Fatalf("unknown field status = %d, want 400", rec1.Code)
	}

	rec2 := httptest.NewRecorder()
	body := `{"repo":"` + strings.Repeat("a", int(maxRouteRequestBytes)) + `","arch":"amd64"}`
	srv.handlePrewarm(rec2, httptest.NewRequest(http.MethodPost, "/prewarm", strings.NewReader(body)))
	if rec2.Code != http.StatusBadRequest {
		t.Fatalf("oversized body status = %d, want 400", rec2.Code)
	}
}

// With auth configured, a missing/incorrect bearer token is rejected before any work.
func TestAuth_RejectsBadToken(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)
	srv.authToken = "s3cret"

	body, _ := json.Marshal(router.RouteRequest{Repo: "github.com/org/repo", Arch: "amd64"})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer wrong")
	srv.handlePrewarm(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
}

// With requireBuildId on, a release that names only the project key is refused: that id is what ties
// a release to the build that started it, and without it any authenticated caller could drain ANOTHER
// project's in-flight set and let its daemon scale down under a running build.
func TestHandleComplete_RequiresBuildIDWhenConfigured(t *testing.T) {
	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	post := func(t *testing.T, require bool, body map[string]string) int {
		t.Helper()
		bp := &bkov1.BuildProject{}
		bp.Name, bp.Namespace = key, "buildkit-operator"
		bp.Status.SetInflight([]bkov1.InflightBuild{{ID: bkov1.InflightID("b1b1b1b1b1b1b1b1"), Since: metav1.Now()}})
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
		srv := newTestServer(t, c)
		srv.requireBuildID = require
		raw, _ := json.Marshal(body)
		rec := httptest.NewRecorder()
		srv.handleComplete(rec, httptest.NewRequest(http.MethodPost, "/complete", bytes.NewReader(raw)))
		return rec.Code
	}
	if got := post(t, true, map[string]string{"key": key}); got != http.StatusBadRequest {
		t.Errorf("release with no buildId = %d, want 400 when requireBuildId is on", got)
	}
	if got := post(t, true, map[string]string{"key": key, "buildId": "b1b1b1b1b1b1b1b1"}); got != http.StatusNoContent {
		t.Errorf("named release = %d, want 204 even with requireBuildId on", got)
	}
	// Off by default, so clients that predate the id keep working through the migration.
	if got := post(t, false, map[string]string{"key": key}); got != http.StatusNoContent {
		t.Errorf("release with no buildId = %d, want 204 with requireBuildId off", got)
	}
}

// A verified (OIDC) caller may only release ITS OWN project's builds. Releasing lets the daemon scale
// down, so honouring a bare key from any authenticated caller would hand everyone a lever on every
// other project. This needs no flag and no migration: the repo is already verified on the request.
func TestHandleComplete_VerifiedCallerCannotReleaseAnotherRepo(t *testing.T) {
	f := newOIDCForge(t)
	v, err := identity.NewVerifier(identity.Config{Providers: []identity.Provider{{Type: "github", Issuer: f.srv.URL, Audience: "bko"}}})
	if err != nil {
		t.Fatal(err)
	}
	mine := router.ProjectKey("github.com/attacker/foo", "", "", "amd64")
	theirs := router.ProjectKey("github.com/victim/secret", "", "", "amd64")
	post := func(t *testing.T, key string) int {
		t.Helper()
		var objs []client.Object
		for name, repo := range map[string]string{mine: "github.com/attacker/foo", theirs: "github.com/victim/secret"} {
			bp := &bkov1.BuildProject{}
			bp.Name, bp.Namespace = name, "buildkit-operator"
			bp.Spec = bkov1.BuildProjectSpec{Key: name, Repo: repo, Arch: "amd64"}
			bp.Status.SetInflight([]bkov1.InflightBuild{{ID: bkov1.InflightID("b1b1b1b1b1b1b1b1"), Since: metav1.Now()}})
			objs = append(objs, bp)
		}
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(objs...).Build()
		srv := newTestServer(t, c)
		srv.verifier, srv.log = v, logr.Discard()
		raw, _ := json.Marshal(map[string]string{"key": key, "buildId": "b1b1b1b1b1b1b1b1"})
		req := httptest.NewRequest(http.MethodPost, "/complete", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer "+f.token(t, "attacker/foo", "refs/heads/main"))
		rec := httptest.NewRecorder()
		srv.handleComplete(rec, req)
		return rec.Code
	}
	if got := post(t, mine); got != http.StatusNoContent {
		t.Errorf("releasing own project = %d, want 204", got)
	}
	if got := post(t, theirs); got != http.StatusForbidden {
		t.Errorf("releasing ANOTHER repo's project = %d, want 403", got)
	}
}

// A forge's OIDC token is minted when the job starts and lives about two minutes; a real build outlives
// it, so the release at the end arrives with a dead token. Requiring a live one rejected the release of
// every build longer than its own token — the ordinary case — and leaked an inflight entry each time.
// Naming a LIVE build id is proof enough on its own: the server minted those 8 random bytes and handed
// them to exactly one caller. A stale or absent id proves nothing, and is still refused.
func TestHandleComplete_ExpiredIdentityAuthorizedByBuildID(t *testing.T) {
	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	// A verifier configured against an issuer nothing can satisfy: identify() always fails, exactly as
	// it does for a token that has expired mid-build.
	deadVerifier := func(t *testing.T) *identity.Verifier {
		t.Helper()
		f := newOIDCForge(t)
		v, err := identity.NewVerifier(identity.Config{Providers: []identity.Provider{{Type: "github", Issuer: f.srv.URL, Audience: "bko"}}})
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	post := func(t *testing.T, body map[string]string) (int, []bkov1.InflightBuild) {
		t.Helper()
		bp := &bkov1.BuildProject{}
		bp.Name, bp.Namespace = key, "buildkit-operator"
		bp.Spec = bkov1.BuildProjectSpec{Key: key, Repo: "github.com/org/repo", Arch: "amd64"}
		bp.Status.SetInflight([]bkov1.InflightBuild{
			{ID: bkov1.InflightID("aaaaaaaaaaaaaaaa"), Since: metav1.NewTime(time.Now().Add(-time.Minute))},
			{ID: bkov1.InflightID("bbbbbbbbbbbbbbbb"), Since: metav1.Now()},
		})
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
		srv := newTestServer(t, c)
		srv.verifier, srv.log = deadVerifier(t), logr.Discard()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/complete", bytes.NewReader(raw))
		req.Header.Set("Authorization", "Bearer a-token-that-will-not-verify")
		rec := httptest.NewRecorder()
		srv.handleComplete(rec, req)
		var got bkov1.BuildProject
		_ = c.Get(t.Context(), types.NamespacedName{Name: key, Namespace: "buildkit-operator"}, &got)
		return rec.Code, got.Status.Inflight
	}

	code, left := post(t, map[string]string{"key": key, "buildId": "aaaaaaaaaaaaaaaa"})
	if code != http.StatusNoContent {
		t.Errorf("release naming a live build id = %d, want 204 — the id is the proof", code)
	}
	if len(left) != 1 || left[0].ID != bkov1.InflightID("bbbbbbbbbbbbbbbb") {
		t.Errorf("inflight = %v, want only the OTHER build left", left)
	}

	// An id that names nothing proves nothing.
	code, left = post(t, map[string]string{"key": key, "buildId": "cccccccccccccccc"})
	if code != http.StatusUnauthorized {
		t.Errorf("release naming an unknown id = %d, want 401", code)
	}
	if len(left) != 2 {
		t.Errorf("inflight = %v, want both entries untouched by an unauthorized release", left)
	}

	// No id at all would fall back to "retire the oldest build" — the one lever an unauthenticated
	// caller must never have.
	code, left = post(t, map[string]string{"key": key})
	if code != http.StatusUnauthorized {
		t.Errorf("release with no id and no identity = %d, want 401", code)
	}
	if len(left) != 2 {
		t.Errorf("inflight = %v, want both entries untouched", left)
	}
}

// A request carrying NO credential at all is refused before it can spend a rate-limit token. The
// bucket is shared with /route, so letting unauthenticated traffic through would hand anyone on the
// internet a way to 429 the whole fleet's builds.
func TestHandleComplete_NoCredentialRejectedBeforeRateLimit(t *testing.T) {
	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	bp := &bkov1.BuildProject{}
	bp.Name, bp.Namespace = key, "buildkit-operator"
	bp.Status.SetInflight(bkov1.StartInflight(nil, "b1b1b1b1b1b1b1b1", metav1.Now()))
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
	srv := newTestServer(t, c)
	srv.authToken, srv.log = "the-shared-bearer", logr.Discard()
	// A limiter that allows exactly one request: if the unauthenticated call consumed it, the
	// legitimate one behind it would be throttled.
	srv.limiter = rate.NewLimiter(rate.Every(time.Hour), 1)

	raw, _ := json.Marshal(map[string]string{"key": key, "buildId": "b1b1b1b1b1b1b1b1"})
	anon := httptest.NewRequest(http.MethodPost, "/complete", bytes.NewReader(raw))
	rec := httptest.NewRecorder()
	srv.handleComplete(rec, anon)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("credential-less release = %d, want 401", rec.Code)
	}

	authed := httptest.NewRequest(http.MethodPost, "/complete", bytes.NewReader(raw))
	authed.Header.Set("Authorization", "Bearer the-shared-bearer")
	rec = httptest.NewRecorder()
	srv.handleComplete(rec, authed)
	if rec.Code == http.StatusTooManyRequests {
		t.Error("the anonymous request spent the rate-limit token — a flood would 429 real builds")
	}
	if rec.Code != http.StatusNoContent {
		t.Errorf("authenticated release = %d, want 204", rec.Code)
	}
}
