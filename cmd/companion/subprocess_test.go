package main

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestPrune_Success: a buildctl that exits 0 yields no error.
func TestPrune_Success(t *testing.T) {
	stubExec(t, "pruned 3 records", 0)
	s := newState(&config{buildkitAddr: "unix:///x"}, quietLogger())
	if err := s.prune(t.Context()); err != nil {
		t.Errorf("prune: %v", err)
	}
}

// TestPrune_Error: a non-zero buildctl exit is surfaced as an error carrying the command output.
func TestPrune_Error(t *testing.T) {
	stubExec(t, "boom: cannot connect", 1)
	s := newState(&config{buildkitAddr: "unix:///x"}, quietLogger())
	err := s.prune(t.Context())
	if err == nil {
		t.Fatal("prune: want error on non-zero exit, got nil")
	}
	if !strings.Contains(err.Error(), "boom") {
		t.Errorf("prune error %q should include the command output", err)
	}
}

// TestCheckInodes_OverThresholdPrunes drives the prune branch: a negative threshold makes any ratio
// (>=0) exceed it, so checkInodes shells out to (stubbed) buildctl and re-samples without error.
func TestCheckInodes_OverThresholdPrunes(t *testing.T) {
	stubExec(t, "pruned", 0)
	s := newState(&config{cacheDir: t.TempDir(), inodeThreshold: -1, buildkitAddr: "unix:///x"}, quietLogger())
	s.checkInodes(t.Context()) // must not panic; prune succeeds and re-sample publishes
}

// TestCheckInodes_OverThresholdPruneFails covers the prune-error branch (logged, then returns).
func TestCheckInodes_OverThresholdPruneFails(t *testing.T) {
	stubExec(t, "fail", 1)
	s := newState(&config{cacheDir: t.TempDir(), inodeThreshold: -1, buildkitAddr: "unix:///x"}, quietLogger())
	s.checkInodes(t.Context())
}

// TestInodeLoop_StopsOnContextCancel: the loop samples once at startup then exits when ctx is done.
func TestInodeLoop_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already cancelled: the select returns after the initial sample
	s := newState(&config{
		cacheDir:           t.TempDir(),
		inodeThreshold:     0.99,
		inodeCheckInterval: time.Hour,
		buildkitAddr:       "unix:///x",
	}, quietLogger())

	done := make(chan struct{})
	go func() { s.inodeLoop(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("inodeLoop did not return after context cancel")
	}
}

// TestReady_ProbeSucceeds: not draining + a buildctl probe that exits 0 => ready.
func TestReady_ProbeSucceeds(t *testing.T) {
	stubExec(t, "", 0)
	s := newState(&config{buildkitAddr: "unix:///x"}, quietLogger())
	if !s.ready(t.Context()) {
		t.Error("ready = false, want true when the worker probe exits 0")
	}
}

// TestReady_ProbeFails: a non-zero probe => not ready.
func TestReady_ProbeFails(t *testing.T) {
	stubExec(t, "no workers", 1)
	s := newState(&config{buildkitAddr: "unix:///x"}, quietLogger())
	if s.ready(t.Context()) {
		t.Error("ready = true, want false when the worker probe exits non-zero")
	}
}
