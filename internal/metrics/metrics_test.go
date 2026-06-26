package metrics

import (
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// TestCollectorsRegisteredAndUsable exercises every collector: the package init must have registered
// them on the controller-runtime registry without panicking, and each must accept the labels/values
// the code uses. testutil.ToFloat64 reads back a single-series value to confirm the increment landed.
func TestCollectorsRegisteredAndUsable(t *testing.T) {
	RoutesTotal.WithLabelValues("warm").Inc()
	if got := testutil.ToFloat64(RoutesTotal.WithLabelValues("warm")); got != 1 {
		t.Errorf("RoutesTotal{warm} = %v, want 1", got)
	}

	RouteDuration.WithLabelValues("cold").Observe(1.5)

	ColdStartsInflight.Inc()
	if got := testutil.ToFloat64(ColdStartsInflight); got != 1 {
		t.Errorf("ColdStartsInflight = %v, want 1", got)
	}
	ColdStartsInflight.Dec()

	ColdStartSeconds.Observe(12)

	ScaleEvents.WithLabelValues("up").Inc()
	if got := testutil.ToFloat64(ScaleEvents.WithLabelValues("up")); got != 1 {
		t.Errorf("ScaleEvents{up} = %v, want 1", got)
	}

	SnapshotsTotal.Inc()
	if got := testutil.ToFloat64(SnapshotsTotal); got != 1 {
		t.Errorf("SnapshotsTotal = %v, want 1", got)
	}
}

// TestMetricNamesPinned pins the exported metric names — they are an external contract (dashboards,
// alerts) and must not drift silently. CollectAndCount with a metric name returns the number of
// series carrying that name; a rename would drop it to 0.
func TestMetricNamesPinned(t *testing.T) {
	RoutesTotal.WithLabelValues("error").Inc()
	if n := testutil.CollectAndCount(RoutesTotal, "buildkit_operator_routes_total"); n == 0 {
		t.Error("metric buildkit_operator_routes_total not found — name drifted")
	}
	SnapshotsTotal.Inc()
	if n := testutil.CollectAndCount(SnapshotsTotal, "buildkit_operator_snapshots_total"); n == 0 {
		t.Error("metric buildkit_operator_snapshots_total not found — name drifted")
	}
}
