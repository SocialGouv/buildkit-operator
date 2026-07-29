package v1alpha1

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func ids(entries []InflightBuild) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.ID)
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
	if !eq(ids(entries), []string{"a", "b"}) {
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

	if got, ok := EndInflight(base, "b"); !ok || !eq(ids(got), []string{"a", "c"}) {
		t.Errorf("EndInflight(b) = %v ok=%v, want [a c] true", ids(got), ok)
	}
	if got, ok := EndInflight(base, ""); !ok || !eq(ids(got), []string{"b", "c"}) {
		t.Errorf(`EndInflight("") = %v ok=%v, want the oldest gone -> [b c] true`, ids(got), ok)
	}
	if got, ok := EndInflight(base, "zzz"); ok || !eq(ids(got), []string{"a", "b", "c"}) {
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
