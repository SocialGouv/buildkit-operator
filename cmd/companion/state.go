package main

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os/exec"
	"sync/atomic"
	"time"
)

// state is the shared, concurrency-safe runtime state for the companion: the
// readiness/draining flags and the last observed inode usage exposed on
// /metrics. It also owns the buildctl probe used by /readyz.
type state struct {
	cfg    *config
	logger *slog.Logger

	// draining is set once on SIGTERM and never cleared; once draining we
	// always report not-ready so no new work is scheduled onto us.
	draining atomic.Bool

	// last inode snapshot, published atomically for /metrics. Stored as bits
	// so the ratio can be loaded without a lock.
	inodeRatioBits atomic.Uint64
	inodeUsed      atomic.Uint64
	inodeTotal     atomic.Uint64
}

func newState(cfg *config, logger *slog.Logger) *state {
	return &state{
		cfg:    cfg,
		logger: logger,
	}
}

func (s *state) setDraining() { s.draining.Store(true) }

// ready reports whether the daemon should receive traffic: not draining and
// the buildctl worker probe succeeds within a short timeout.
func (s *state) ready(ctx context.Context) bool {
	if s.draining.Load() {
		return false
	}
	return s.probeWorkers(ctx) == nil
}

// probeWorkers runs `buildctl --addr <addr> debug workers` and returns nil iff
// it exits 0. This is the cheapest liveness signal vanilla buildkitd exposes
// over its socket.
func (s *state) probeWorkers(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "buildctl", "--addr", s.cfg.buildkitAddr, "debug", "workers")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildctl debug workers: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// mux builds the HTTP handler for the health/metrics endpoints.
func (s *state) mux() http.Handler {
	mux := http.NewServeMux()

	// /healthz is always 200 once the server is up: it reports that the
	// companion process itself is alive, independent of buildkitd.
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})

	// /readyz gates traffic on the buildctl probe (and the drain flag).
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.ready(r.Context()) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready\n"))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("not ready\n"))
	})

	// /metrics is a tiny plain-text exposition of the inode backstop state.
	// Deliberately no prometheus dependency; the line format is still
	// scrapeable by a prometheus textfile-style parser.
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, _ *http.Request) {
		ratio := math.Float64frombits(s.inodeRatioBits.Load())
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "inode_usage_ratio %g\n", ratio)
		fmt.Fprintf(w, "inode_used %d\n", s.inodeUsed.Load())
		fmt.Fprintf(w, "inode_total %d\n", s.inodeTotal.Load())
	})

	return mux
}
