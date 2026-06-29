// Package provisioner is the backend-agnostic contract the buildd routing API depends on to
// materialise and address the one hot buildkitd that serves a project key. The control plane
// (routing, identity, cache identity, rate-limiting) is substrate-neutral; only this contract knows
// HOW a daemon is provisioned — on Kubernetes (StatefulSet-of-1 + Service + PVC, see
// internal/provisioner/k8s) or, eventually, on a single host (Incus/LXD + ZFS).
//
// The router (internal/router) computes the key; this contract turns a key into a running, addressable
// daemon. Keeping it small and imperative is deliberate: the lifecycle wiring (reconcile/scale/snapshot)
// genuinely differs per backend and lives in cmd/buildd's per-backend setup, not behind this interface.
package provisioner

import (
	"context"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
)

// Provisioner is the imperative surface the /route, /prewarm and /complete handlers call. All methods
// are keyed by the router-computed project key and must be safe for concurrent use.
type Provisioner interface {
	// Ensure idempotently provisions the daemon for spec. When untrusted, it provisions the ephemeral
	// fork daemon (a distinct key, seeded read-only from the canonical snapshot) instead of the canonical
	// one — the anti cache-poisoning path. The caller derives the routing key itself via router.ForkKey,
	// so Ensure and the handler never disagree on the key.
	Ensure(ctx context.Context, spec bkov1.BuildProjectSpec, untrusted bool) error
	// Ready reports whether the daemon for key already has a ready replica (the warm fast path).
	Ready(ctx context.Context, key string) bool
	// WaitReady blocks until the daemon for key has a ready replica, or the backend's wait budget / ctx
	// elapses (the cold-start path).
	WaitReady(ctx context.Context, key string) error
	// Endpoint returns the deterministic mTLS address clients dial for key (in-cluster Service DNS, or
	// the shared SNI gateway hostname off-cluster).
	Endpoint(key string) string
	// AddInflight adjusts the inflight-build counter for key (floored at 0) and stamps its last-build
	// time, keeping the daemon pinned warm for the build's duration. Best-effort: failures are logged.
	AddInflight(ctx context.Context, key string, delta int32)
}
