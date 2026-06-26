package main

import (
	"net/http"
	"testing"

	"github.com/go-logr/logr"
)

// identify is the /route auth gate. With no OIDC verifier it falls back to: open when no token is
// configured (in-cluster default), a strict legacy Bearer match when authToken is set, and a break-glass
// admin token carried in a DISTINCT header.
func TestIdentify_LegacyAndAdmin(t *testing.T) {
	req := func(auth, admin string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "/route", nil)
		if auth != "" {
			r.Header.Set("Authorization", auth)
		}
		if admin != "" {
			r.Header.Set(adminTokenHeader, admin)
		}
		return r
	}
	ok := func(s *routeServer, r *http.Request) bool {
		_, status, err := s.identify(r)
		return status == 0 && err == nil
	}

	open := &routeServer{log: logr.Discard()} // no token, no OIDC => everything allowed (in-cluster)
	if !ok(open, req("", "")) || !ok(open, req("Bearer whatever", "")) {
		t.Error("no configured token must allow all requests")
	}

	legacy := &routeServer{authToken: "s3cr3t", log: logr.Discard()}
	for h, want := range map[string]bool{
		"Bearer s3cr3t": true,
		"Bearer wrong":  false,
		"s3cr3t":        false, // missing scheme
		"Bearer ":       false,
		"":              false,
	} {
		if got := ok(legacy, req(h, "")); got != want {
			t.Errorf("legacy authorize(%q) = %v, want %v", h, got, want)
		}
	}

	// Admin break-glass: distinct header, trusts the request (override=false).
	admin := &routeServer{adminToken: "adm1n", log: logr.Discard()}
	if id, status, err := admin.identify(req("", "adm1n")); err != nil || status != 0 || id.override {
		t.Errorf("valid admin token: status=%d err=%v override=%v, want 0/nil/false", status, err, id.override)
	}
	if _, status, _ := admin.identify(req("", "wrong")); status != http.StatusUnauthorized {
		t.Errorf("wrong admin token: status=%d, want 401", status)
	}
	// No admin header + no other auth configured => open (admin token is optional, not required).
	if !ok(admin, req("", "")) {
		t.Error("absent admin header should fall through to open when nothing else is configured")
	}
}
