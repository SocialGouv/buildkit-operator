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

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
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
	return &routeServer{c: c, cfg: builder.Config{Namespace: "buildkit-operator", Port: 1234}, coldStartSem: make(chan struct{}, 1)}
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

// /complete decrements the inflight counter that /route incremented, floored at 0.
func TestHandleComplete_DecrementsInflight(t *testing.T) {
	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	bp := &bkov1.BuildProject{}
	bp.Name, bp.Namespace = key, "buildkit-operator"
	bp.Status.InflightBuilds = 2
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
	srv := newTestServer(t, c)

	body, _ := json.Marshal(map[string]string{"key": key})
	rec := httptest.NewRecorder()
	srv.handleComplete(rec, httptest.NewRequest(http.MethodPost, "/complete", bytes.NewReader(body)))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rec.Code)
	}
	var got bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: key, Namespace: srv.cfg.Namespace}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status.InflightBuilds != 1 {
		t.Errorf("InflightBuilds = %d, want 1", got.Status.InflightBuilds)
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

func TestCacheFor_SkipsForks(t *testing.T) {
	srv := &routeServer{s3Bucket: "bucket", s3Region: "gra", s3Endpoint: "https://s3.example"}
	canonical := router.ProjectKey("github.com/org/repo", "", "", "amd64")

	if got := srv.cacheFor(canonical); got == nil || got.Name != canonical || got.Bucket != "bucket" {
		t.Fatalf("canonical cache = %#v, want S3 cache for %s", got, canonical)
	}
	if got := srv.cacheFor(router.ForkKey(canonical)); got != nil {
		t.Fatalf("fork cache = %#v, want nil", got)
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
