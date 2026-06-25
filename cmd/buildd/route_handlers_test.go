package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/router"
	"golang.org/x/time/rate"
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
	if bp.Status.LastBuildTime == nil {
		t.Error("LastBuildTime not stamped by prewarm")
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
