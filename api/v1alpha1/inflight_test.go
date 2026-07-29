package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ids maps entries back to the TOKENS they were registered with, so a test can talk in tokens while
// the status stores their hashes.
func ids(entries []InflightBuild) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
	}
	return out
}

// tokens is ids() expressed in token space: hash each expected token and compare.
func hashed(tokens ...string) []string {
	out := make([]string, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, InflightID(t))
	}
	return out
}

func eq(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// StartInflight appends one entry per routed build, oldest first, and never grows past InflightCap.
func TestStartInflight(t *testing.T) {
	now := metav1.Now()
	entries := StartInflight(StartInflight(nil, "a", now), "b", now)
	if !eq(ids(entries), hashed("a", "b")) {
		t.Errorf("entries = %v, want [a b]", ids(entries))
	}

	var many []InflightBuild
	for i := 0; i < InflightCap+5; i++ {
		many = StartInflight(many, "x", now)
	}
	if len(many) != InflightCap {
		t.Errorf("len = %d past the cap, want %d", len(many), InflightCap)
	}
}

// EndInflight releases the named build; an empty id releases the OLDEST (a client that predates
// build IDs); an unknown id changes nothing, so a duplicate /complete can't release a live build.
func TestEndInflight(t *testing.T) {
	now := metav1.Now()
	base := StartInflight(StartInflight(StartInflight(nil, "a", now), "b", now), "c", now)

	if got, ok := EndInflight(base, "b"); !ok || !eq(ids(got), hashed("a", "c")) {
		t.Errorf("EndInflight(b) = %v ok=%v, want [a c] true", ids(got), ok)
	}
	if got, ok := EndInflight(base, ""); !ok || !eq(ids(got), hashed("b", "c")) {
		t.Errorf(`EndInflight("") = %v ok=%v, want the oldest gone -> [b c] true`, ids(got), ok)
	}
	if got, ok := EndInflight(base, "zzz"); ok || !eq(ids(got), hashed("a", "b", "c")) {
		t.Errorf("EndInflight(unknown) = %v ok=%v, want unchanged and false", ids(got), ok)
	}
	if got, ok := EndInflight(nil, ""); ok || len(got) != 0 {
		t.Errorf("EndInflight on an empty set = %v ok=%v, want empty false", got, ok)
	}
}

// ExpireInflight drops entries on their OWN age — the point of the whole design: a leaked build
// expires while a sibling that is genuinely still running is left alone.
func TestExpireInflight(t *testing.T) {
	now := time.Now()
	entries := []InflightBuild{
		{ID: "leaked", Since: metav1.NewTime(now.Add(-3 * time.Hour))},
		{ID: "running", Since: metav1.NewTime(now.Add(-time.Minute))},
	}
	got, dropped := ExpireInflight(entries, now, 2*time.Hour)
	if dropped != 1 || !eq(ids(got), []string{"running"}) {
		t.Errorf("ExpireInflight = %v dropped=%d, want [running] 1", ids(got), dropped)
	}
	if got, dropped := ExpireInflight(entries, now, 4*time.Hour); dropped != 0 || len(got) != 2 {
		t.Errorf("nothing past the window: got %v dropped=%d, want both kept", ids(got), dropped)
	}
}

// SetInflight is the single writer: the InflightBuilds projection always matches the entries.
func TestSetInflightKeepsProjectionInSync(t *testing.T) {
	now := metav1.Now()
	var st BuildProjectStatus
	st.SetInflight(StartInflight(StartInflight(nil, "a", now), "b", now))
	if st.InflightCount() != 2 || st.InflightBuilds != 2 {
		t.Errorf("count = %d, projection = %d, want 2 and 2", st.InflightCount(), st.InflightBuilds)
	}
	entries, _ := EndInflight(st.Inflight, "a")
	st.SetInflight(entries)
	if st.InflightCount() != 1 || st.InflightBuilds != 1 {
		t.Errorf("count = %d, projection = %d, want 1 and 1", st.InflightCount(), st.InflightBuilds)
	}
	st.SetInflight(nil)
	if st.InflightCount() != 0 || st.InflightBuilds != 0 {
		t.Errorf("count = %d, projection = %d, want 0 and 0", st.InflightCount(), st.InflightBuilds)
	}
}

// AdoptLegacyInflight turns a pre-entries count into dated entries so an upgrade landing mid-build
// keeps pinning the daemon; it is inert once entries exist, and on a status that never built.
func TestAdoptLegacyInflight(t *testing.T) {
	last := metav1.NewTime(time.Now().Add(-10 * time.Minute))

	var legacy BuildProjectStatus
	legacy.InflightBuilds = 3
	legacy.LastBuildTime = &last
	if !AdoptLegacyInflight(&legacy) {
		t.Fatal("a bare count with a last-build time must be adopted")
	}
	if legacy.InflightCount() != 3 || legacy.InflightBuilds != 3 {
		t.Errorf("adopted = %v (projection %d), want 3 entries", legacy.Inflight, legacy.InflightBuilds)
	}
	for _, e := range legacy.Inflight {
		if !e.Since.Equal(&last) {
			t.Errorf("entry %q dated %v, want the last build time %v", e.ID, e.Since, last)
		}
	}
	// Adopted entries age on the last-build clock, so a count left over from a build older than the
	// window clears immediately instead of pinning the daemon for another full window.
	if live, dropped := ExpireInflight(legacy.Inflight, time.Now(), time.Minute); dropped != 3 || len(live) != 0 {
		t.Errorf("expiry of adopted entries = %v dropped=%d, want all gone", live, dropped)
	}

	// Inert once the set is authoritative, and inert with nothing to date entries from.
	current := BuildProjectStatus{Inflight: []InflightBuild{{ID: "a", Since: last}}, InflightBuilds: 1}
	if AdoptLegacyInflight(&current) {
		t.Error("a status that already holds entries must not be adopted")
	}
	never := BuildProjectStatus{InflightBuilds: 2}
	if AdoptLegacyInflight(&never) {
		t.Error("a count with no LastBuildTime has no clock to date entries from")
	}
}

// An unnamed release (a client that predates build IDs) retires an ALREADY-EXPIRED entry when there
// is one — retiring a live entry on its behalf is what would scale a running build's daemon down.
func TestEndInflightBefore(t *testing.T) {
	now := time.Now()
	cutoff := now.Add(-2 * time.Hour)
	entries := []InflightBuild{
		{ID: "livest", Since: metav1.NewTime(now.Add(-3 * time.Hour))}, // expired
		{ID: "running", Since: metav1.NewTime(now.Add(-time.Minute))},
	}
	got, ok := EndInflightBefore(entries, "", cutoff)
	if !ok || !eq(ids(got), []string{"running"}) {
		t.Errorf("unnamed release = %v ok=%v, want the expired entry gone", ids(got), ok)
	}
	// With nothing expired it falls back to the oldest, as before.
	live := []InflightBuild{
		{ID: "older", Since: metav1.NewTime(now.Add(-10 * time.Minute))},
		{ID: "newer", Since: metav1.NewTime(now.Add(-time.Minute))},
	}
	if got, ok := EndInflightBefore(live, "", cutoff); !ok || !eq(ids(got), []string{"newer"}) {
		t.Errorf("unnamed release with nothing expired = %v ok=%v, want the oldest gone", ids(got), ok)
	}
	// A named release is unaffected by the cutoff.
	if got, ok := EndInflightBefore(live, "newer", cutoff); !ok || !eq(ids(got), []string{"older"}) {
		t.Errorf("named release = %v ok=%v, want exactly the named entry gone", ids(got), ok)
	}
}

// The token /route hands the client is a credential; only its hash is ever written to the status,
// which anything with read access to the builds namespace can see. A release presents the token.
func TestInflightIDHashesTheToken(t *testing.T) {
	now := metav1.Now()
	entries := StartInflight(nil, "deadbeefdeadbeef", now)
	if entries[0].ID == "deadbeefdeadbeef" {
		t.Fatal("the raw token was stored on the status — it is a credential, store its hash")
	}
	if entries[0].ID != InflightID("deadbeefdeadbeef") {
		t.Errorf("stored id = %q, want the token's hash", entries[0].ID)
	}
	// Presenting the token releases it; presenting the stored hash does not (it is not the secret).
	if _, ok := EndInflight(entries, "deadbeefdeadbeef"); !ok {
		t.Error("presenting the token did not release the build")
	}
	if _, ok := EndInflight(entries, "beefdeadbeefdead"); ok {
		t.Error("a different token released the build")
	}
	// Entries written before hashing hold the token verbatim; they must still release, or a build in
	// flight across the upgrade leaks until it expires.
	legacy := []InflightBuild{{ID: "deadbeefdeadbeef", Since: now}}
	if _, ok := EndInflight(legacy, "deadbeefdeadbeef"); !ok {
		t.Error("a pre-hashing entry no longer releases")
	}
}
