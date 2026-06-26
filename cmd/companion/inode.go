package main

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"syscall"
	"time"
)

// inodeLoop is the inode GC backstop. buildkitd's own GC is byte-budget based,
// so a cache full of tiny files (the node_modules trap) can exhaust inodes on
// the Cinder volume long before the byte budget triggers — and an
// inode-exhausted volume fails every subsequent build with ENOSPC even though
// `df` shows free space. We statfs the cache dir and, past a threshold, prune
// aggressively so the daemon stays usable.
func (s *state) inodeLoop(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.inodeCheckInterval)
	defer ticker.Stop()

	// Sample once at startup so /metrics is populated immediately.
	s.checkInodes(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.checkInodes(ctx)
		}
	}
}

// inodeStats is a snapshot of inode accounting for the cache filesystem.
type inodeStats struct {
	used  uint64
	total uint64
	ratio float64 // used/total in [0,1]; 0 when total is 0.
}

// statInodes reads inode accounting for dir via statfs. Files is the total
// inode count and Ffree the free count, so used = Files-Ffree.
func statInodes(dir string) (inodeStats, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err != nil {
		return inodeStats{}, fmt.Errorf("statfs %q: %w", dir, err)
	}
	total := st.Files
	var used uint64
	if total >= st.Ffree {
		used = total - st.Ffree
	}
	ratio := 0.0
	if total > 0 {
		ratio = float64(used) / float64(total)
	}
	return inodeStats{used: used, total: total, ratio: ratio}, nil
}

// checkInodes samples inode usage, publishes it for /metrics, and prunes when
// usage exceeds the configured threshold.
func (s *state) checkInodes(ctx context.Context) {
	stats, err := statInodes(s.cfg.cacheDir)
	if err != nil {
		s.logger.Warn("inode statfs failed", "err", err, "dir", s.cfg.cacheDir)
		return
	}

	s.publishInodes(stats)
	s.logger.Debug("inode usage",
		"dir", s.cfg.cacheDir,
		"ratio", stats.ratio,
		"used", stats.used,
		"total", stats.total,
	)

	if stats.ratio <= s.cfg.inodeThreshold {
		return
	}

	s.logger.Warn("inode usage over threshold, pruning",
		"ratio", stats.ratio,
		"threshold", s.cfg.inodeThreshold,
		"used", stats.used,
		"total", stats.total,
	)

	if err := s.prune(ctx); err != nil {
		s.logger.Error("inode prune failed", "err", err)
		return
	}

	// Re-sample so the log (and /metrics) reflect the post-prune state.
	if after, err := statInodes(s.cfg.cacheDir); err != nil {
		s.logger.Warn("inode statfs after prune failed", "err", err)
	} else {
		s.publishInodes(after)
		s.logger.Info("inode prune complete",
			"ratio_before", stats.ratio,
			"ratio_after", after.ratio,
			"freed_inodes", int64(stats.used)-int64(after.used),
		)
	}
}

// prune runs a full GC of the buildkit cache. --all is intentional: when we've
// hit the inode wall, holding cache back is worse than a cold rebuild.
func (s *state) prune(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	cmd := execCommandContext(ctx, "buildctl", "--addr", s.cfg.buildkitAddr, "prune", "--all")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("buildctl prune --all: %w: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// publishInodes stores the snapshot for lock-free reads by /metrics.
func (s *state) publishInodes(stats inodeStats) {
	s.inodeRatioBits.Store(math.Float64bits(stats.ratio))
	s.inodeUsed.Store(stats.used)
	s.inodeTotal.Store(stats.total)
}
