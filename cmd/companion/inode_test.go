package main

import (
	"math"
	"net/http/httptest"
	"strings"
	"testing"
)

// statInodes returns a plausible, internally-consistent snapshot for a real directory (used <= total,
// ratio in [0,1]). It's a backstop signal, so we assert the invariants rather than exact counts.
func TestStatInodes_RealDir(t *testing.T) {
	st, err := statInodes(t.TempDir())
	if err != nil {
		t.Fatalf("statInodes: %v", err)
	}
	if st.total == 0 {
		t.Skip("filesystem reports no inode accounting (e.g. tmpfs/overlay); nothing to assert")
	}
	if st.used > st.total {
		t.Errorf("used %d > total %d", st.used, st.total)
	}
	if st.ratio < 0 || st.ratio > 1 {
		t.Errorf("ratio %v out of [0,1]", st.ratio)
	}
	if want := float64(st.used) / float64(st.total); math.Abs(st.ratio-want) > 1e-9 {
		t.Errorf("ratio %v != used/total %v", st.ratio, want)
	}
}

func TestStatInodes_BadDir(t *testing.T) {
	if _, err := statInodes("/no/such/path/buildkit-operator-test"); err == nil {
		t.Fatal("statInodes on a missing dir: want error, got nil")
	}
}

// publishInodes must round-trip through the atomic /metrics exposition (ratio stored as float bits).
func TestPublishInodes_MetricsRoundTrip(t *testing.T) {
	s := newState(&config{cacheDir: "/x"}, quietLogger())
	s.publishInodes(inodeStats{used: 30, total: 100, ratio: 0.3})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/metrics", nil)
	s.mux().ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, want := range []string{"inode_usage_ratio 0.3", "inode_used 30", "inode_total 100"} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q in:\n%s", want, body)
		}
	}
}

// Below the threshold, checkInodes only publishes (no prune attempt), so it must not error/panic even
// though no real buildkitd is reachable — it never shells out.
func TestCheckInodes_BelowThresholdNoPrune(t *testing.T) {
	s := newState(&config{cacheDir: t.TempDir(), inodeThreshold: 0.99, buildkitAddr: "unix:///nonexistent"}, quietLogger())
	s.checkInodes(t.Context())
	// total may be 0 on some CI filesystems; either way ratio<=threshold so no prune is attempted.
	if r := math.Float64frombits(s.inodeRatioBits.Load()); r > 1 || r < 0 {
		t.Errorf("published ratio %v out of range", r)
	}
}
