// Package metrics holds buildcat's Prometheus collectors (M4 observability). They register
// with controller-runtime's global registry, so they are served on the manager's metrics
// endpoint alongside the standard controller metrics.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// RoutesTotal counts /route calls by outcome: warm (daemon already up), cold (had to
	// scale up + wait), untrusted (fork-PR ephemeral), error.
	RoutesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "buildcat_routes_total",
		Help: "Total /route requests by result (warm|cold|untrusted|error).",
	}, []string{"result"})

	// RouteDuration is the wall-clock of a /route call (dominated by Cinder attach on cold starts).
	RouteDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "buildcat_route_duration_seconds",
		Help:    "Latency of /route by result.",
		Buckets: []float64{0.1, 0.5, 1, 2, 5, 10, 20, 45, 90, 180},
	}, []string{"result"})

	// ColdStartsInflight is the number of cold starts currently waiting (gated by the rate limiter).
	ColdStartsInflight = prometheus.NewGauge(prometheus.GaugeOpts{
		Name: "buildcat_coldstarts_inflight",
		Help: "Cold-start waits currently in flight (bounded by --max-cold-starts).",
	})

	// ScaleEvents counts daemon scale transitions by direction (up|down).
	ScaleEvents = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "buildcat_scale_events_total",
		Help: "Daemon scale transitions by direction (up|down).",
	}, []string{"direction"})

	// SnapshotsTotal counts durability VolumeSnapshots created.
	SnapshotsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "buildcat_snapshots_total",
		Help: "Durability VolumeSnapshots created.",
	})
)

func init() {
	ctrlmetrics.Registry.MustRegister(RoutesTotal, RouteDuration, ColdStartsInflight, ScaleEvents, SnapshotsTotal)
}
