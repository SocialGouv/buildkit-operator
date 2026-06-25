package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"

	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

// addInflight adjusts Status.InflightBuilds by delta (floored at 0) and stamps LastBuildTime now. It
// re-Gets and retries on conflict: a single Status().Update that lost a 409 race with the reconciler
// would leave the count wrong, so the project could scale down mid-build (or never scale down). A
// terminal failure (all retries exhausted) is logged — silently dropping it would skew the count, and
// only the --max-build-seconds safety net would eventually paper over it.
func (s *routeServer) addInflight(ctx context.Context, key string, delta int32) {
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var bp bkov1.BuildProject
		if err := s.c.Get(ctx, types.NamespacedName{Name: key, Namespace: s.cfg.Namespace}, &bp); err != nil {
			return err
		}
		n := bp.Status.InflightBuilds + delta
		if n < 0 {
			n = 0
		}
		bp.Status.InflightBuilds = n
		now := metav1.Now()
		bp.Status.LastBuildTime = &now
		return s.c.Status().Update(ctx, &bp)
	})
	if err != nil {
		s.log.Error(err, "addInflight failed; inflight count may be skewed until the max-build-seconds safety net", "key", key, "delta", delta)
	}
}

func writeJSON(log logr.Logger, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Status + headers are already committed, so we can't change the response — but a swallowed
		// encode error leaves the client with a truncated body and no signal in our logs.
		log.Error(err, "encode JSON response failed")
	}
}

// clientIP returns the caller address for audit logs (best-effort: X-Forwarded-For first hop when set
// behind the LB, else the TCP peer). The host only — no port — keeps log lines stable.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		first, _, _ := strings.Cut(xff, ",")
		return strings.TrimSpace(first)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
