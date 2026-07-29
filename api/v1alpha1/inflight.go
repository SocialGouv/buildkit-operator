package v1alpha1

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
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
	// ID identifies the build. It is the SHA-256 of the token /route handed the client, never the
	// token itself: releasing a build is authorized by presenting that token, and the status of a
	// BuildProject is readable by anything with get/list in the builds namespace. Storing the token
	// would put a live credential in plain sight there.
	ID string `json:"id"`
	// Since is when /route registered this build. The reconciler expires entries older than
	// --max-build-seconds.
	Since metav1.Time `json:"since"`
}

// InflightID is the stored form of a build token: its SHA-256, hex-encoded. The token is a
// credential, the status is not a secret store, so only this ever lands on the object.
func InflightID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SetInflight is the ONLY way to record inflight builds on the status: it writes the entries and
// the derived count in one step, so the projection can never drift from the source of truth.
func (s *BuildProjectStatus) SetInflight(entries []InflightBuild) {
	entries = capInflight(entries)
	s.Inflight = entries
	s.InflightBuilds = int32(len(entries)) // bounded by InflightCap
}

// capInflight keeps the newest InflightCap entries. The set is chronological (StartInflight appends,
// the removals preserve order), so dropping from the front sheds the oldest.
func capInflight(entries []InflightBuild) []InflightBuild {
	if len(entries) > InflightCap {
		return entries[len(entries)-InflightCap:]
	}
	return entries
}

// InflightCount reports how many routed builds have not been released yet.
func (s *BuildProjectStatus) InflightCount() int { return len(s.Inflight) }

// AdoptLegacyInflight converts a status written before inflight became a set — a bare count with no
// entries — into entries dated from the project's last build. Without it the upgrade reads as "no
// build is running" and a daemon serving a live build scales to zero under it; with it the adopted
// entries keep pinning the daemon and expire on the normal --max-build-seconds clock. Returns false
// when there is nothing to adopt (the steady state).
func AdoptLegacyInflight(s *BuildProjectStatus) bool {
	if len(s.Inflight) > 0 || s.InflightBuilds <= 0 || s.LastBuildTime == nil {
		return false
	}
	n := int(s.InflightBuilds)
	if n > InflightCap {
		n = InflightCap
	}
	adopted := make([]InflightBuild, 0, n)
	for i := 0; i < n; i++ {
		// The real per-build start times are gone; LastBuildTime is the only clock the old status
		// carried, and it is the most recent — so these expire no EARLIER than the truth, never later.
		adopted = append(adopted, InflightBuild{ID: fmt.Sprintf("adopted-%d", i), Since: *s.LastBuildTime})
	}
	s.SetInflight(adopted)
	return true
}

// StartInflight registers a routed build under the HASH of its token (see InflightID).
func StartInflight(inflight []InflightBuild, token string, now metav1.Time) []InflightBuild {
	out := make([]InflightBuild, len(inflight), len(inflight)+1)
	copy(out, inflight)
	return capInflight(append(out, InflightBuild{ID: InflightID(token), Since: now}))
}

// EndInflight releases the entry matching id and reports whether one was found. An EMPTY id — a
// client that predates build IDs, or one whose /route response was lost — has no entry to name, so
// it releases the oldest one. Callers pass the expiry window with it: an entry already past that
// window is preferred, because it is the one whose owner is certainly gone. Retiring a live entry on
// behalf of an unnamed caller is what would scale a running build's daemon down.
func EndInflight(inflight []InflightBuild, id string) ([]InflightBuild, bool) {
	return endInflight(inflight, id, nil)
}

// EndInflightBefore is EndInflight with the expiry cutoff for the unnamed case: the oldest entry
// older than cutoff goes first, and only if none is, the oldest live one.
func EndInflightBefore(inflight []InflightBuild, id string, cutoff time.Time) ([]InflightBuild, bool) {
	return endInflight(inflight, id, &cutoff)
}

func endInflight(inflight []InflightBuild, id string, cutoff *time.Time) ([]InflightBuild, bool) {
	if len(inflight) == 0 {
		return inflight, false
	}
	drop := -1
	if id == "" {
		drop = 0 // oldest — entries are chronological
		if cutoff != nil {
			for i := range inflight {
				if inflight[i].Since.Time.Before(*cutoff) {
					drop = i
					break
				}
			}
		}
	} else {
		hashed := InflightID(id)
		for i := range inflight {
			if inflight[i].ID == hashed {
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
	expired := 0
	for _, b := range inflight {
		if now.Sub(b.Since.Time) > maxAge {
			expired++
		}
	}
	if expired == 0 { // the common case: hand back the same slice, no copy
		return inflight, 0
	}
	out := make([]InflightBuild, 0, len(inflight)-expired)
	for _, b := range inflight {
		if now.Sub(b.Since.Time) <= maxAge {
			out = append(out, b)
		}
	}
	return out, expired
}
