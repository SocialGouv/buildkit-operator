package main

import (
	"context"
	"log/slog"
	"os"
	"time"
)

// drain implements the graceful-drain window after SIGTERM.
//
// Scale-to-zero correctness hinges on the pod terminating only once the cache
// volume is at-rest, so that the Cinder CSI NodeUnstage/detach is clean and the
// next attach doesn't recover a torn cache. The companion's job is to hold the
// pod open long enough for in-flight work to settle before returning 0.
//
// NOTE (M1): precise in-flight build counting is deferred. buildkitd does not
// expose a trivial "active build count" over its socket — `buildctl debug
// workers` reports worker presence, not active solves, and the gateway/session
// accounting is internal. The robust signal in M1 is the orchestration layer:
//   - the kubelet's terminationGracePeriodSeconds bounds the window, and
//   - buildkitd installs its own SIGTERM handler and stops in-flight solves
//     and flushes the snapshotter, so the volume reaches a consistent at-rest
//     state on its own.
//
// So here we implement drain as a bounded wait: log "draining", then sleep in
// short steps until either the drain-seconds budget elapses OR an optional
// external "drain done" file appears (a hook for buildd or a future in-flight
// probe to short-circuit the wait once it knows the daemon is idle). Either way
// we return so run() can exit 0.
func drain(ctx context.Context, cfg *config, logger *slog.Logger) {
	deadline := time.Now().Add(cfg.drainSeconds)
	logger.Info("draining",
		"drain_seconds", cfg.drainSeconds.Seconds(),
		"drain_done_file", cfg.drainDoneFile,
		"deadline", deadline.UTC().Format(time.RFC3339),
	)

	const step = 2 * time.Second

	for {
		if cfg.drainDoneFile != "" && fileExists(cfg.drainDoneFile) {
			logger.Info("drain short-circuited by done-file", "file", cfg.drainDoneFile)
			return
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			logger.Info("drain window elapsed", "drain_seconds", cfg.drainSeconds.Seconds())
			return
		}

		sleep := step
		if remaining < sleep {
			sleep = remaining
		}

		select {
		case <-ctx.Done():
			// Honoured if a caller passes a cancellable context. NB: run() passes context.Background()
			// here on purpose — the signal context is already cancelled by the first SIGTERM, so passing
			// it would make this return immediately and skip the drain entirely. A second-SIGTERM cut
			// would need its own context wired in run(); today the drain runs to drain-seconds or the
			// done-file.
			logger.Warn("drain interrupted by context cancellation")
			return
		case <-time.After(sleep):
		}
	}
}

// fileExists reports whether path exists (any stat error other than absence is
// treated as "not present" so a transient stat failure doesn't end the drain
// early).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
