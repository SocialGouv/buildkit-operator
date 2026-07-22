package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRecordBuildTime(t *testing.T) {
	now := metav1.Now()

	// Appends and prunes entries older than the window.
	old := metav1.NewTime(now.Add(-AdaptiveWindow - time.Hour))
	recent := metav1.NewTime(now.Add(-time.Hour))
	got := RecordBuildTime([]metav1.Time{old, recent}, now)
	if len(got) != 2 {
		t.Fatalf("RecordBuildTime = %d entries, want 2 (old pruned, recent kept, now appended)", len(got))
	}
	if !got[len(got)-1].Equal(&now) {
		t.Error("newest entry must be last")
	}

	// Enforces the cap, dropping the oldest first.
	ring := make([]metav1.Time, 0, RecentBuildTimesCap)
	for i := 0; i < RecentBuildTimesCap; i++ {
		ring = append(ring, metav1.NewTime(now.Add(-time.Duration(RecentBuildTimesCap-i)*time.Minute)))
	}
	got = RecordBuildTime(ring, now)
	if len(got) != RecentBuildTimesCap {
		t.Fatalf("capped ring = %d entries, want %d", len(got), RecentBuildTimesCap)
	}
	if got[0].Equal(&ring[0]) {
		t.Error("oldest entry must be dropped when the ring is full")
	}
}

func TestBuildsInWindow(t *testing.T) {
	now := time.Now()
	times := []metav1.Time{
		metav1.NewTime(now.Add(-AdaptiveWindow - time.Minute)), // outside
		metav1.NewTime(now.Add(-2 * time.Hour)),
		metav1.NewTime(now.Add(-time.Minute)),
	}
	if got := BuildsInWindow(times, now); got != 2 {
		t.Errorf("BuildsInWindow = %d, want 2", got)
	}
	if got := BuildsInWindow(nil, now); got != 0 {
		t.Errorf("BuildsInWindow(nil) = %d, want 0", got)
	}
}
