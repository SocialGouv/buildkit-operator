// Command companion is the buildkit-operator sidecar that runs next to a vanilla
// buildkitd container in a per-(project,arch) pod.
//
// One hot buildkitd serves a single cache that lives on a Cinder gen2 PVC
// mounted at the buildkitd data dir. The companion does the housekeeping that
// vanilla buildkitd does not:
//
//   - Health/readiness for the kubelet (probes the daemon with `buildctl debug
//     workers`); /readyz flips to not-ready on drain so the Service drops it.
//   - An inode GC backstop: statfs the cache dir and prune when inodes run low,
//     which is the classic node_modules inode-exhaustion trap that bytes-based
//     GC misses.
//   - A graceful drain on SIGTERM so scale-to-zero is correct: stop advertising
//     ready, let in-flight work settle, then exit 0 so the pod terminates and
//     the Cinder volume detaches cleanly (NodeUnstage) at-rest.
//
// It shells out to `buildctl` (assumed on PATH) and otherwise uses only the Go
// stdlib plus cobra for the CLI.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// config holds the resolved runtime configuration. Every field has an env
// fallback wired up in newRootCmd via envOr; cobra defaults are the env-aware
// defaults so `--help` shows the effective value.
type config struct {
	buildkitAddr       string
	cacheDir           string
	listen             string
	inodeCheckInterval time.Duration
	inodeThreshold     float64
	drainSeconds       time.Duration
	drainDoneFile      string
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		slog.Error("companion exited with error", "err", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cfg := &config{}

	cmd := &cobra.Command{
		Use:           "companion",
		Short:         "buildkit-operator buildkitd sidecar (health, inode GC backstop, graceful drain)",
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.buildkitAddr, "buildkit-addr",
		envOr("BUILDKIT_OPERATOR_BUILDKIT_ADDR", "unix:///run/user/1000/buildkit/buildkitd.sock"),
		"buildkitd address passed to buildctl --addr")
	f.StringVar(&cfg.cacheDir, "cache-dir",
		envOr("BUILDKIT_OPERATOR_CACHE_DIR", "/home/user/.local/share/buildkit"),
		"buildkitd data dir on the Cinder PVC; statfs'd for the inode backstop")
	f.StringVar(&cfg.listen, "listen",
		envOr("BUILDKIT_OPERATOR_LISTEN", ":8080"),
		"address for the health/metrics HTTP server")
	f.DurationVar(&cfg.inodeCheckInterval, "inode-check-interval",
		envDurationOr("BUILDKIT_OPERATOR_INODE_CHECK_INTERVAL", 60*time.Second),
		"interval between inode usage checks on the cache dir")
	f.Float64Var(&cfg.inodeThreshold, "inode-threshold",
		envFloatOr("BUILDKIT_OPERATOR_INODE_THRESHOLD", 0.95),
		"inode usage ratio above which the companion runs `buildctl prune --all`")
	f.DurationVar(&cfg.drainSeconds, "drain-seconds",
		envDurationOr("BUILDKIT_OPERATOR_DRAIN_SECONDS", 120*time.Second),
		"max time to drain in-flight work after SIGTERM before exiting")
	f.StringVar(&cfg.drainDoneFile, "drain-done-file",
		envOr("BUILDKIT_OPERATOR_DRAIN_DONE_FILE", ""),
		"optional path that, once it exists, short-circuits the drain wait")

	return cmd
}

// run wires up the health server, the periodic loops, and the signal-driven
// graceful drain, then blocks until drain completes.
func run(parent context.Context, cfg *config) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	logger.Info("companion starting",
		"buildkit_addr", cfg.buildkitAddr,
		"cache_dir", cfg.cacheDir,
		"listen", cfg.listen,
	)

	// ctx is cancelled on the first SIGTERM/SIGINT. We catch the signal
	// ourselves (rather than signal.NotifyContext) because draining is an
	// ordered shutdown: flip readiness, then wait, then return.
	ctx, stop := signal.NotifyContext(parent, syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	state := newState(cfg, logger)

	srv := &http.Server{
		Addr:              cfg.listen,
		Handler:           state.mux(),
		ReadHeaderTimeout: 5 * time.Second,
	}

	var wg sync.WaitGroup

	// HTTP health/metrics server.
	wg.Add(1)
	go func() {
		defer wg.Done()
		logger.Info("health server listening", "addr", cfg.listen)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health server failed", "err", err)
			stop() // a dead health server is fatal: trigger shutdown.
		}
	}()

	// Inode GC backstop loop. Stops when ctx is cancelled (first signal).
	wg.Add(1)
	go func() {
		defer wg.Done()
		state.inodeLoop(ctx)
	}()

	// Block until SIGTERM/SIGINT.
	<-ctx.Done()
	logger.Info("shutdown signal received, beginning graceful drain")

	// Stop advertising ready so the kubelet/buildd stop sending new work,
	// then drain in-flight builds for up to drain-seconds.
	state.setDraining()
	drain(context.Background(), cfg, logger)

	// Shut the HTTP server down with a short grace, then wait for the loops.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("health server shutdown", "err", err)
	}
	wg.Wait()

	logger.Info("drain complete, exiting 0 for clean volume detach")
	return nil
}
