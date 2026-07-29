package v1alpha1

import (
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// InflightCap bounds Status.Inflight. A project genuinely running more than this many concurrent
// builds is already saturated (one daemon, one cache volume); past the cap the oldest entry is
// dropped rather than letting a pathological caller grow the status object without bound.
const InflightCap = 64

// InflightBuild is one routed build that has not been released yet. Each entry ages on its OWN
// clock, so a build whose /complete never arrives expires by itself — without touching the entries
// of builds that are still running.
type InflightBuild struct {
	// ID is the opaque token /route returned to the client, which /complete echoes back.
	ID string `json:"id"`
	// Since is when /route registered this build. The reconciler expires entries older than
	// --max-build-seconds.
	Since metav1.Time `json:"since"`
}

// SetInflight is the ONLY way to record inflight builds on the status: it writes the entries and
// the derived count in one step, so the projection can never drift from the source of truth.
func (s *BuildProjectStatus) SetInflight(entries []InflightBuild) {
	if len(entries) > InflightCap {
		entries = entries[len(entries)-InflightCap:]
	}
	s.Inflight = entries
	s.InflightBuilds = int32(len(entries)) // bounded by InflightCap above
}

// InflightCount reports how many routed builds have not been released yet.
func (s *BuildProjectStatus) InflightCount() int { return len(s.Inflight) }

// StartInflight registers a routed build and returns the updated status entries.
func StartInflight(inflight []InflightBuild, id string, now metav1.Time) []InflightBuild {
	out := append(append([]InflightBuild{}, inflight...), InflightBuild{ID: id, Since: now})
	if len(out) > InflightCap {
		out = out[len(out)-InflightCap:]
	}
	return out
}

// EndInflight releases the entry matching id and reports whether one was found. An EMPTY id (a
// client that predates build IDs, or one whose /route response was lost) releases the OLDEST entry:
// it is the one most likely to be finished, and dropping it is what the caller's /complete means.
func EndInflight(inflight []InflightBuild, id string) ([]InflightBuild, bool) {
	if len(inflight) == 0 {
		return inflight, false
	}
	drop := -1
	if id == "" {
		drop = 0
	} else {
		for i := range inflight {
			if inflight[i].ID == id {
				drop = i
				break
			}
		}
	}
	if drop < 0 {
		return inflight, false
	}
	out := make([]InflightBuild, 0, len(inflight)-1)
	out = append(out, inflight[:drop]...)
	out = append(out, inflight[drop+1:]...)
	return out, true
}

// ExpireInflight drops the entries older than maxAge and reports how many were dropped. This is the
// safety net for a /complete that never arrives: it is per-entry, so a build that has legitimately
// been running for hours is never released by a sibling's leak.
func ExpireInflight(inflight []InflightBuild, now time.Time, maxAge time.Duration) ([]InflightBuild, int) {
	out := make([]InflightBuild, 0, len(inflight))
	for _, b := range inflight {
		if now.Sub(b.Since.Time) <= maxAge {
			out = append(out, b)
		}
	}
	return out, len(inflight) - len(out)
}
