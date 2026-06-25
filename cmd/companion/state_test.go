package main

import (
	"net/http/httptest"
	"testing"
)

// /healthz reports the companion process is alive, independent of buildkitd — always 200 once up.
func TestHealthz_AlwaysOK(t *testing.T) {
	s := newState(&config{}, quietLogger())
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest("GET", "/healthz", nil))
	if rec.Code != 200 {
		t.Fatalf("/healthz = %d, want 200", rec.Code)
	}
}

// Once draining, ready() short-circuits to false WITHOUT probing buildkitd (so the Service drops the
// pod immediately on SIGTERM), and /readyz reports 503.
func TestReadyz_DrainingIsNotReady(t *testing.T) {
	s := newState(&config{buildkitAddr: "unix:///nonexistent"}, quietLogger())
	s.setDraining()

	if s.ready(t.Context()) {
		t.Fatal("ready() = true while draining, want false")
	}
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("/readyz = %d while draining, want 503", rec.Code)
	}
}

// Not draining but no reachable daemon => the buildctl probe fails => not ready (503). This also
// exercises probeWorkers' timeout/error path without a real buildkitd.
func TestReadyz_NoDaemonIsNotReady(t *testing.T) {
	s := newState(&config{buildkitAddr: "unix:///run/nonexistent.sock"}, quietLogger())
	rec := httptest.NewRecorder()
	s.mux().ServeHTTP(rec, httptest.NewRequest("GET", "/readyz", nil))
	if rec.Code != 503 {
		t.Fatalf("/readyz = %d with no daemon, want 503", rec.Code)
	}
}
