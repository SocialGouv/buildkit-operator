package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// drain must run for the full budget when no done-file is configured: scale-to-zero correctness
// depends on holding the pod open long enough for in-flight work to settle.
func TestDrain_RunsForBudget(t *testing.T) {
	cfg := &config{drainSeconds: 120 * time.Millisecond}
	start := time.Now()
	drain(context.Background(), cfg, quietLogger())
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("drain returned after %v, want >= ~120ms (full budget)", elapsed)
	}
}

// A pre-existing done-file short-circuits the wait immediately.
func TestDrain_ShortCircuitsOnDoneFile(t *testing.T) {
	done := filepath.Join(t.TempDir(), "drain-done")
	if err := os.WriteFile(done, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config{drainSeconds: 10 * time.Second, drainDoneFile: done}
	start := time.Now()
	drain(context.Background(), cfg, quietLogger())
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("drain took %v with a present done-file, want a near-immediate return", elapsed)
	}
}

// A done-file that appears mid-drain ends the wait before the budget elapses.
func TestDrain_DoneFileAppearingMidDrain(t *testing.T) {
	done := filepath.Join(t.TempDir(), "drain-done")
	cfg := &config{drainSeconds: 10 * time.Second, drainDoneFile: done}
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(done, []byte("x"), 0o644)
	}()
	start := time.Now()
	drain(context.Background(), cfg, quietLogger())
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("drain took %v, want it to end shortly after the done-file appears", elapsed)
	}
	if elapsed < 140*time.Millisecond {
		t.Fatalf("drain ended after %v, before the done-file was written", elapsed)
	}
}

func TestFileExists(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "present")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if !fileExists(f) {
		t.Errorf("fileExists(%q) = false, want true", f)
	}
	if fileExists(filepath.Join(dir, "absent")) {
		t.Errorf("fileExists(absent) = true, want false")
	}
}
