package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Adaptive keep-warm: the effective idle window of a warm project scales with
// its observed build cadence (spec idle × builds in the trailing window, capped
// by the operator's --adaptive-idle-max-seconds). The shared helpers below keep
// the writer (the routing API's AddInflight) and the reader (the reconciler's
// desiredReplicas) on one definition of the cadence record.
const (
	// AdaptiveWindow is the trailing window over which build cadence is counted.
	AdaptiveWindow = 24 * time.Hour
	// RecentBuildTimesCap bounds Status.RecentBuildTimes. 30 entries × the
	// default 900s idle already exceeds any sane adaptive cap, so a busier
	// history adds no signal — only status bytes.
	RecentBuildTimesCap = 30
)

// RecordBuildTime appends now to the cadence ring, pruning entries older than
// AdaptiveWindow and enforcing RecentBuildTimesCap (oldest dropped first).
func RecordBuildTime(times []metav1.Time, now metav1.Time) []metav1.Time {
	out := make([]metav1.Time, 0, len(times)+1)
	for _, t := range times {
		if now.Sub(t.Time) <= AdaptiveWindow {
			out = append(out, t)
		}
	}
	out = append(out, now)
	if len(out) > RecentBuildTimesCap {
		out = out[len(out)-RecentBuildTimesCap:]
	}
	return out
}

// BuildsInWindow counts cadence entries within AdaptiveWindow of now.
func BuildsInWindow(times []metav1.Time, now time.Time) int {
	n := 0
	for _, t := range times {
		if now.Sub(t.Time) <= AdaptiveWindow {
			n++
		}
	}
	return n
}
