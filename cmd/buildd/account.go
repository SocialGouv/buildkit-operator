package main

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
)

// addInflight adjusts Status.InflightBuilds by delta (floored at 0) and stamps LastBuildTime now. It
// re-Gets and retries on conflict AND not-found: a Status().Update that lost a 409 race with the
// reconciler would leave the count wrong (the project could scale down mid-build, or never scale down),
// and right after /route|/prewarm creates the project the informer cache can still miss it, so a plain
// Get returns NotFound — retrying lets the cache catch up instead of dropping the touch (which would
// leave a warm-tier project stuck Idle). A terminal failure (all retries exhausted) is logged.
func (s *routeServer) addInflight(ctx context.Context, key string, delta int32) {
	retriable := func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsNotFound(err) }
	// ~6.4s of retries (vs DefaultBackoff's ~40ms): the informer cache can lag etcd by a beat right after
	// the project is created, so a too-short backoff drops the touch — and for an ephemeral fork that
	// touch is what keeps it from being reaped before its build registers.
	backoff := wait.Backoff{Steps: 8, Duration: 100 * time.Millisecond, Factor: 1.6, Jitter: 0.1}
	err := retry.OnError(backoff, retriable, func() error {
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
