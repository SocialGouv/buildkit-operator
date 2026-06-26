package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/identity"
	"github.com/socialgouv/buildkit-operator/internal/router"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// oidcForge is a minimal fake OIDC issuer for the handler-level binding test.
type oidcForge struct {
	srv  *httptest.Server
	priv *rsa.PrivateKey
}

func newOIDCForge(t *testing.T) *oidcForge {
	t.Helper()
	priv, _ := rsa.GenerateKey(rand.Reader, 2048)
	f := &oidcForge{priv: priv}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"issuer": f.srv.URL, "jwks_uri": f.srv.URL + "/jwks"})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &priv.PublicKey, KeyID: "k1", Algorithm: "RS256", Use: "sig"}}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *oidcForge) token(t *testing.T, repository, ref string) string {
	t.Helper()
	signer, _ := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: f.priv}, (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "k1"))
	payload, _ := json.Marshal(map[string]any{
		"iss": f.srv.URL, "aud": "bko", "exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Add(-time.Minute).Unix(),
		"repository": repository, "ref": ref,
	})
	obj, _ := signer.Sign(payload)
	tok, _ := obj.CompactSerialize()
	return tok
}

// THE core security property: a caller cannot self-declare another repo. The request body claims the
// victim's repo, but the OIDC token is for the attacker's repo — buildd must route to the ATTACKER's
// key (and create that project), never the victim's, so the victim's warm cache is untouchable.
func TestHandle_OIDCBindsRepoIgnoringBody(t *testing.T) {
	f := newOIDCForge(t)
	v, err := identity.NewVerifier(identity.Config{Providers: []identity.Provider{{Type: "github", Issuer: f.srv.URL, Audience: "bko"}}})
	if err != nil {
		t.Fatal(err)
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)
	srv.verifier = v
	srv.log = logr.Discard()

	// Body lies: claims the victim's repo. Token tells the truth: the attacker's repo, on a push.
	body, _ := json.Marshal(router.RouteRequest{Repo: "github.com/victim/secret", Arch: "amd64"})
	req := httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token(t, "attacker/foo", "refs/heads/main"))
	rec := httptest.NewRecorder()
	srv.handlePrewarm(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	var resp router.RouteResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	attackerKey := router.ProjectKey("github.com/attacker/foo", "", "", "amd64")
	victimKey := router.ProjectKey("github.com/victim/secret", "", "", "amd64")
	if resp.Key != attackerKey {
		t.Errorf("routed key = %q, want attacker key %q (body repo must be ignored)", resp.Key, attackerKey)
	}
	// The victim's project must NOT have been created.
	var bp bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: victimKey, Namespace: srv.cfg.Namespace}, &bp); err == nil {
		t.Fatal("victim BuildProject was created — repo binding failed")
	}
}

// With a verifier configured, a request WITHOUT a token is rejected (secure default).
func TestHandle_OIDCRequiresToken(t *testing.T) {
	f := newOIDCForge(t)
	v, _ := identity.NewVerifier(identity.Config{Providers: []identity.Provider{{Type: "github", Issuer: f.srv.URL, Audience: "bko"}}})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)
	srv.verifier = v
	srv.log = logr.Discard()

	body, _ := json.Marshal(router.RouteRequest{Repo: "github.com/org/repo", Arch: "amd64"})
	rec := httptest.NewRecorder()
	srv.handlePrewarm(rec, httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body)))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 (no token under OIDC)", rec.Code)
	}
}

// A non-allowlisted but validly-signed repo is forbidden (hard org gate).
func TestHandle_OIDCAllowlistRejects(t *testing.T) {
	f := newOIDCForge(t)
	v, _ := identity.NewVerifier(identity.Config{
		Providers:     []identity.Provider{{Type: "github", Issuer: f.srv.URL, Audience: "bko"}},
		RepoAllowlist: []string{"github.com/socialgouv/*"},
	})
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	srv := newTestServer(t, c)
	srv.verifier = v
	srv.log = logr.Discard()

	body, _ := json.Marshal(router.RouteRequest{Arch: "amd64"})
	req := httptest.NewRequest(http.MethodPost, "/prewarm", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.token(t, "outsider/repo", "refs/heads/main"))
	rec := httptest.NewRecorder()
	srv.handlePrewarm(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403 (repo not in allowlist)", rec.Code)
	}
}
