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
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A /route that never reaches a Ready daemon (cold-start timeout) MUST release the inflight build it
// counted, otherwise the leaked counter pins the daemon warm for up to --max-build-seconds — the
// client only calls /complete after a SUCCESSFUL route, so buildd has to clean up its own error paths.
func TestHandleRoute_ReleasesInflightOnColdStartTimeout(t *testing.T) {
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := bkov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	ns := "buildkit-operator"
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&bkov1.BuildProject{}).Build()

	srv := &routeServer{
		c: c, cfg: builder.Config{Namespace: ns, Port: 1234},
		wait:         0, // no STS will ever be Ready => waitReady times out on the first poll
		coldStartSem: make(chan struct{}, 1),
	}

	body, _ := json.Marshal(router.RouteRequest{Repo: "github.com/org/repo", Arch: "amd64"})
	req := httptest.NewRequest(http.MethodPost, "/route", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	srv.handleRoute(rec, req)

	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want 504 (daemon never Ready)", rec.Code)
	}

	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	var bp bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: key, Namespace: ns}, &bp); err != nil {
		t.Fatalf("get buildproject: %v", err)
	}
	if bp.Status.InflightBuilds != 0 {
		t.Errorf("InflightBuilds = %d after failed cold start, want 0 (must be released)", bp.Status.InflightBuilds)
	}
}
