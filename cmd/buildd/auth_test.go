package main

import (
	"net/http"
	"testing"
)

// The /route API auth gate: open when no token is configured (in-cluster default), and a strict
// Bearer-token match otherwise (the guard that makes service.type: LoadBalancer safe).
func TestAuthorized(t *testing.T) {
	req := func(h string) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "/route", nil)
		if h != "" {
			r.Header.Set("Authorization", h)
		}
		return r
	}

	open := &routeServer{} // no token => everything allowed
	if !open.authorized(req("")) || !open.authorized(req("Bearer whatever")) {
		t.Error("no configured token must allow all requests")
	}

	s := &routeServer{authToken: "s3cr3t"}
	cases := map[string]bool{
		"Bearer s3cr3t": true,
		"Bearer wrong":  false,
		"s3cr3t":        false, // missing scheme
		"Bearer ":       false,
		"":              false,
	}
	for h, want := range cases {
		if got := s.authorized(req(h)); got != want {
			t.Errorf("authorized(%q) = %v, want %v", h, got, want)
		}
	}
}
