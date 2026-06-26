package identity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

// forge is a fake OIDC issuer: it serves the discovery doc + JWKS and mints signed JWTs, so the tests
// exercise the REAL go-oidc verification path (signature, audience, expiry) without a live forge.
type forge struct {
	srv  *httptest.Server
	priv *rsa.PrivateKey
	kid  string
}

func newForge(t *testing.T) *forge {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := &forge{priv: priv, kid: "test-key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                f.srv.URL,
			"jwks_uri":                              f.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &priv.PublicKey, KeyID: f.kid, Algorithm: "RS256", Use: "sig"},
		}})
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *forge) issuer() string { return f.srv.URL }

// sign mints a JWT with the given claims (iss/exp default to this forge / +1h unless set).
func (f *forge) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	if _, ok := claims["iss"]; !ok {
		claims["iss"] = f.srv.URL
	}
	if _, ok := claims["exp"]; !ok {
		claims["exp"] = time.Now().Add(time.Hour).Unix()
	}
	if _, ok := claims["iat"]; !ok {
		claims["iat"] = time.Now().Add(-time.Minute).Unix()
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: f.priv},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", f.kid),
	)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	tok, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestVerify_GitHub(t *testing.T) {
	f := newForge(t)
	v, err := NewVerifier(Config{Providers: []Provider{
		{Type: "github", Issuer: f.issuer(), Audience: "buildkit-operator"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("push is trusted", func(t *testing.T) {
		tok := f.sign(t, map[string]any{"aud": "buildkit-operator", "repository": "SocialGouv/Foo", "ref": "refs/heads/main"})
		id, err := v.Verify(context.Background(), tok)
		if err != nil {
			t.Fatal(err)
		}
		if id.Repo != "github.com/socialgouv/foo" {
			t.Errorf("repo = %q, want github.com/socialgouv/foo", id.Repo)
		}
		if id.Untrusted {
			t.Error("push build should be trusted")
		}
	})

	t.Run("pull_request is untrusted", func(t *testing.T) {
		tok := f.sign(t, map[string]any{"aud": "buildkit-operator", "repository": "socialgouv/foo", "ref": "refs/pull/7/merge"})
		id, err := v.Verify(context.Background(), tok)
		if err != nil {
			t.Fatal(err)
		}
		if !id.Untrusted {
			t.Error("pull_request build should be untrusted")
		}
	})

	t.Run("wrong audience rejected", func(t *testing.T) {
		tok := f.sign(t, map[string]any{"aud": "someone-else", "repository": "socialgouv/foo", "ref": "refs/heads/main"})
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Fatal("expected audience mismatch to be rejected")
		}
	})

	t.Run("expired rejected", func(t *testing.T) {
		tok := f.sign(t, map[string]any{"aud": "buildkit-operator", "repository": "socialgouv/foo", "ref": "refs/heads/main", "exp": time.Now().Add(-time.Hour).Unix()})
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Fatal("expected expired token to be rejected")
		}
	})
}

func TestVerify_GitLab(t *testing.T) {
	f := newForge(t)
	v, err := NewVerifier(Config{Providers: []Provider{
		{Type: "gitlab", Issuer: f.issuer(), Audience: "buildkit-operator", Host: "gitlab.example.com"},
	}})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("protected ref trusted", func(t *testing.T) {
		tok := f.sign(t, map[string]any{"aud": "buildkit-operator", "project_path": "studio-tech/architecture/foo", "ref_protected": "true"})
		id, err := v.Verify(context.Background(), tok)
		if err != nil {
			t.Fatal(err)
		}
		if id.Repo != "gitlab.example.com/studio-tech/architecture/foo" {
			t.Errorf("repo = %q", id.Repo)
		}
		if id.Untrusted {
			t.Error("protected ref should be trusted")
		}
	})

	t.Run("unprotected ref untrusted", func(t *testing.T) {
		tok := f.sign(t, map[string]any{"aud": "buildkit-operator", "project_path": "studio-tech/architecture/foo", "ref_protected": "false"})
		id, err := v.Verify(context.Background(), tok)
		if err != nil {
			t.Fatal(err)
		}
		if !id.Untrusted {
			t.Error("unprotected ref should be untrusted")
		}
	})
}

func TestVerify_Forgejo(t *testing.T) {
	f := newForge(t)
	v, err := NewVerifier(Config{Providers: []Provider{
		{Type: "forgejo", Issuer: f.issuer(), Audience: "buildkit-operator", Host: "forge.example.com"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tok := f.sign(t, map[string]any{"aud": "buildkit-operator", "repository": "team/app", "ref": "refs/pull/2/head"})
	id, err := v.Verify(context.Background(), tok)
	if err != nil {
		t.Fatal(err)
	}
	if id.Repo != "forge.example.com/team/app" {
		t.Errorf("repo = %q", id.Repo)
	}
	if !id.Untrusted {
		t.Error("forgejo pull_request build should be untrusted")
	}
}

func TestVerify_UnknownIssuer(t *testing.T) {
	known := newForge(t)
	other := newForge(t) // a different issuer, NOT configured
	v, err := NewVerifier(Config{Providers: []Provider{
		{Type: "github", Issuer: known.issuer(), Audience: "buildkit-operator"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	tok := other.sign(t, map[string]any{"aud": "buildkit-operator", "repository": "socialgouv/foo", "ref": "refs/heads/main"})
	if _, err := v.Verify(context.Background(), tok); !errors.Is(err, ErrUntrustedIssuer) {
		t.Fatalf("err = %v, want ErrUntrustedIssuer", err)
	}
}

func TestVerify_ForgedSignatureRejected(t *testing.T) {
	// A token whose iss points at our trusted forge but signed by a DIFFERENT key (attacker) must fail
	// signature verification against the real JWKS.
	good := newForge(t)
	attacker := newForge(t)
	v, err := NewVerifier(Config{Providers: []Provider{
		{Type: "github", Issuer: good.issuer(), Audience: "buildkit-operator"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	// Sign with the attacker key but claim the good issuer.
	tok := attacker.sign(t, map[string]any{"iss": good.issuer(), "aud": "buildkit-operator", "repository": "socialgouv/foo", "ref": "refs/heads/main"})
	if _, err := v.Verify(context.Background(), tok); err == nil {
		t.Fatal("expected forged-signature token to be rejected")
	}
}

func TestAllowRepo(t *testing.T) {
	v := &Verifier{allowlist: normalizeAllowlist([]string{"github.com/socialgouv/*", "gitlab.example.com/studio-tech/architecture/*"})}
	cases := map[string]bool{
		"github.com/socialgouv/foo":                       true,
		"github.com/SocialGouv/Foo":                       true, // normalized
		"github.com/other/foo":                            false,
		"gitlab.example.com/studio-tech/architecture/api": true,
		"gitlab.example.com/studio-tech/other/api":        false,
	}
	for repo, want := range cases {
		if got := v.AllowRepo(repo); got != want {
			t.Errorf("AllowRepo(%q) = %v, want %v", repo, got, want)
		}
	}

	// Empty allowlist allows everything (OIDC alone is the gate).
	open := &Verifier{}
	if !open.AllowRepo("github.com/anyone/anything") {
		t.Error("empty allowlist should allow all verified repos")
	}
}

func TestNewVerifier_DisabledOrEmpty(t *testing.T) {
	for _, cfg := range []Config{{Disable: true, Providers: []Provider{{Type: "github", Issuer: "x", Audience: "y"}}}, {}} {
		v, err := NewVerifier(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if v != nil {
			t.Error("disabled/empty config should yield a nil verifier (OIDC off)")
		}
	}
}
