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
		prov:         k8sprov.New(c, cfg, 0, "", 0, logr.Discard()),
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
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
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
			{ID: "first", Since: older},
			{ID: "second", Since: metav1.Now()},
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

	got := complete(t, map[string]string{"key": key, "buildId": "second"})
	if got.Status.InflightCount() != 1 || got.Status.Inflight[0].ID != "first" {
		t.Errorf("inflight = %v, want the named build released and first left", got.Status.Inflight)
	}
	got = complete(t, map[string]string{"key": key}) // no buildId: oldest goes
	if got.Status.InflightCount() != 1 || got.Status.Inflight[0].ID != "second" {
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
