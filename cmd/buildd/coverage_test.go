package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/router"
)

// TestValidateRouteRequest covers every rejection branch plus the accept paths.
func TestValidateRouteRequest(t *testing.T) {
	tests := []struct {
		name    string
		req     router.RouteRequest
		wantErr string
	}{
		{"ok amd64", router.RouteRequest{Repo: "github.com/o/r", Arch: "amd64"}, ""},
		{"ok arm64", router.RouteRequest{Repo: "github.com/o/r", Arch: "arm64"}, ""},
		{"empty repo", router.RouteRequest{Arch: "amd64"}, "repo is required"},
		{"repo too long", router.RouteRequest{Repo: strings.Repeat("a", 513), Arch: "amd64"}, "repo is too long"},
		{"name too long", router.RouteRequest{Repo: "github.com/o/r", Name: strings.Repeat("n", 129), Arch: "amd64"}, "name is too long"},
		{"target too long", router.RouteRequest{Repo: "github.com/o/r", Target: strings.Repeat("t", 129), Arch: "amd64"}, "target is too long"},
		{"bad arch", router.RouteRequest{Repo: "github.com/o/r", Arch: "riscv"}, "unsupported arch"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRouteRequest(tt.req)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("want nil, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("want error %q, got %v", tt.wantErr, err)
			}
		})
	}
}

// TestValidateCompleteRequest covers empty / too-long / valid keys.
func TestValidateCompleteRequest(t *testing.T) {
	if err := validateCompleteRequest(""); err == nil {
		t.Error("empty key: want error")
	}
	if err := validateCompleteRequest("   "); err == nil {
		t.Error("whitespace key: want error")
	}
	if err := validateCompleteRequest(strings.Repeat("k", 65)); err == nil {
		t.Error("oversized key: want error")
	}
	if err := validateCompleteRequest("pdeadbeef"); err != nil {
		t.Errorf("valid key: %v", err)
	}
}

// TestEndpointFor covers both endpoint shapes: gateway SNI host (off-cluster) vs in-cluster Service DNS.
func TestEndpointFor(t *testing.T) {
	// In-cluster: no gateway host configured.
	in := &routeServer{cfg: builder.Config{Namespace: "ns", Port: 1234}}
	if got, want := in.endpointFor("p1"), router.Endpoint("p1", "ns", 1234); got != want {
		t.Errorf("in-cluster endpointFor = %q, want %q", got, want)
	}

	// Gateway: SNI hostname <daemon>.<gatewayHost>, with the gateway port overriding cfg.Port.
	gw := &routeServer{cfg: builder.Config{Namespace: "ns", Port: 1234}, gatewayHost: "gw.example.com", gatewayPort: 443}
	got := gw.endpointFor("p1")
	want := router.EndpointHost(router.DaemonName("p1")+".gw.example.com", 443)
	if got != want {
		t.Errorf("gateway endpointFor = %q, want %q", got, want)
	}

	// Gateway host but no explicit gateway port => falls back to cfg.Port.
	gw2 := &routeServer{cfg: builder.Config{Namespace: "ns", Port: 1234}, gatewayHost: "gw.example.com"}
	if !strings.HasSuffix(gw2.endpointFor("p1"), ":1234") {
		t.Errorf("gateway w/o port should use cfg.Port: %q", gw2.endpointFor("p1"))
	}
}

// TestEnsureBuildProject_CreatesAndStamps: a missing project is created and its LastBuildTime stamped
// (warm-from-birth), so desiredReplicas holds a warm replica immediately.
func TestEnsureBuildProject_CreatesAndStamps(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)

	spec := bkov1.BuildProjectSpec{Key: "pnew", Repo: "github.com/o/r", Arch: "amd64"}
	if err := srv.ensureBuildProject(t.Context(), spec); err != nil {
		t.Fatalf("ensureBuildProject: %v", err)
	}
	var bp bkov1.BuildProject
	if err := c.Get(t.Context(), types.NamespacedName{Name: "pnew", Namespace: srv.cfg.Namespace}, &bp); err != nil {
		t.Fatalf("project not created: %v", err)
	}
	if bp.Status.LastBuildTime == nil {
		t.Error("LastBuildTime not stamped at creation")
	}
}

// TestEnsureBuildProject_Idempotent: an existing project is a no-op (no error, no second create).
func TestEnsureBuildProject_Idempotent(t *testing.T) {
	existing := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: "pexist", Namespace: "buildkit-operator"},
		Spec:       bkov1.BuildProjectSpec{Key: "pexist", Repo: "github.com/o/r", Arch: "amd64"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(existing).Build()
	srv := newTestServer(t, c)

	if err := srv.ensureBuildProject(t.Context(), existing.Spec); err != nil {
		t.Errorf("idempotent ensure: %v", err)
	}
}

// TestHandleRoute_WarmReturnsEndpoint: a project whose daemon is already Ready takes the warm fast
// path — 200 with the in-cluster endpoint and Ready=true.
func TestHandleRoute_WarmReturnsEndpoint(t *testing.T) {
	key := canonicalSpec(router.RouteRequest{Repo: "github.com/o/r", Arch: "amd64"}).Key
	readySTS := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: router.DaemonName(key), Namespace: "buildkit-operator"},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(readySTS).Build()
	srv := newTestServer(t, c)

	rec := httptest.NewRecorder()
	body := strings.NewReader(`{"repo":"github.com/o/r","arch":"amd64"}`)
	srv.handleRoute(rec, httptest.NewRequest(http.MethodPost, "/route", body))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), key) {
		t.Errorf("response missing key %q: %s", key, rec.Body.String())
	}
}

// TestHandleRoute_InvalidArch: validation rejects before any K8s work — 400.
func TestHandleRoute_InvalidArch(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)
	rec := httptest.NewRecorder()
	srv.handleRoute(rec, httptest.NewRequest(http.MethodPost, "/route", strings.NewReader(`{"repo":"r","arch":"x"}`)))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestWaitReady_Success: a StatefulSet already reporting a ready replica returns nil immediately.
func TestWaitReady_Success(t *testing.T) {
	key := "pwarm"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: router.DaemonName(key), Namespace: "buildkit-operator"},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sts).Build()
	srv := newTestServer(t, c)
	srv.wait = time.Second
	if err := srv.waitReady(t.Context(), key); err != nil {
		t.Errorf("waitReady: %v", err)
	}
}

// TestWaitReady_Timeout: with no daemon and a zero wait budget, waitReady gives up with an error.
func TestWaitReady_Timeout(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	srv := newTestServer(t, c)
	srv.wait = 0 // deadline already passed on first miss
	if err := srv.waitReady(t.Context(), "pmissing"); err == nil {
		t.Error("waitReady: want timeout error, got nil")
	}
}

// TestAddInflight_UpdatesStatus: addInflight increments the inflight counter, stamps LastBuildTime,
// and floors a negative result at zero (a /complete that races ahead of /route can't go negative).
func TestAddInflight_UpdatesStatus(t *testing.T) {
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: "pcount", Namespace: "buildkit-operator"},
		Spec:       bkov1.BuildProjectSpec{Key: "pcount", Repo: "github.com/o/r", Arch: "amd64"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
	srv := newTestServer(t, c)

	srv.addInflight(t.Context(), "pcount", +2)
	var got bkov1.BuildProject
	_ = c.Get(t.Context(), types.NamespacedName{Name: "pcount", Namespace: "buildkit-operator"}, &got)
	if got.Status.InflightBuilds != 2 {
		t.Errorf("InflightBuilds = %d, want 2", got.Status.InflightBuilds)
	}
	if got.Status.LastBuildTime == nil {
		t.Error("LastBuildTime not stamped")
	}

	srv.addInflight(t.Context(), "pcount", -5) // floors at 0
	_ = c.Get(t.Context(), types.NamespacedName{Name: "pcount", Namespace: "buildkit-operator"}, &got)
	if got.Status.InflightBuilds != 0 {
		t.Errorf("InflightBuilds = %d after over-decrement, want 0 (floored)", got.Status.InflightBuilds)
	}
}

// TestClientIP covers the audit-log IP extraction: X-Forwarded-For first hop wins, else the TCP
// peer host, with a malformed RemoteAddr passed through verbatim.
func TestClientIP(t *testing.T) {
	xff := httptest.NewRequest(http.MethodPost, "/route", nil)
	xff.Header.Set("X-Forwarded-For", " 203.0.113.7 , 10.0.0.1")
	if got := clientIP(xff); got != "203.0.113.7" {
		t.Errorf("XFF: clientIP = %q, want 203.0.113.7", got)
	}

	peer := httptest.NewRequest(http.MethodPost, "/route", nil)
	peer.RemoteAddr = "192.0.2.5:54321"
	if got := clientIP(peer); got != "192.0.2.5" {
		t.Errorf("peer: clientIP = %q, want 192.0.2.5", got)
	}

	bad := httptest.NewRequest(http.MethodPost, "/route", nil)
	bad.RemoteAddr = "no-port"
	if got := clientIP(bad); got != "no-port" {
		t.Errorf("malformed: clientIP = %q, want no-port", got)
	}
}

// TestNeedLeaderElection: every replica serves /route, so the HTTP server must NOT be leader-gated.
func TestNeedLeaderElection(t *testing.T) {
	if (&routeServer{}).NeedLeaderElection() {
		t.Error("routeServer.NeedLeaderElection() = true, want false (all replicas serve /route)")
	}
}

// TestHandleComplete_MethodAndBody covers the non-POST and malformed-body rejections.
func TestHandleComplete_MethodAndBody(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)

	rec := httptest.NewRecorder()
	srv.handleComplete(rec, httptest.NewRequest(http.MethodGet, "/complete", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: status = %d, want 405", rec.Code)
	}

	rec = httptest.NewRecorder()
	srv.handleComplete(rec, httptest.NewRequest(http.MethodPost, "/complete", strings.NewReader("{not-json")))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("bad json: status = %d, want 400", rec.Code)
	}
}
