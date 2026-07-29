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
	"time"

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
	// StartInflight registers a routed build under id and stamps the project's last-build time,
	// keeping the daemon pinned warm for the build's duration. Best-effort: failures are logged.
	StartInflight(ctx context.Context, key, id string)
	// EndInflight releases the build registered under id (an empty id releases the oldest entry, for a
	// client that predates build IDs) and reports whether an entry actually went away. Naming a live
	// id is itself proof that the caller started that build, which is how /complete authorizes a caller
	// whose short-lived identity token expired mid-build. Nothing is written when nothing matched.
	EndInflight(ctx context.Context, key, id string) (released bool)
	// Touch stamps the project's last-build time without registering a build — the /prewarm path,
	// which must keep a warm-tier project from scaling to zero without counting as a build.
	Touch(ctx context.Context, key string)
	// ProjectRepo returns the normalized repo a key belongs to, and found=false when the key does not
	// exist. /complete uses it to check that a verified caller is releasing ITS OWN project's build, so
	// a lookup that FAILS must surface as an error and not as "no project" — that would read as
	// "skip the check" and turn a transient API blip into an authorization bypass.
	ProjectRepo(ctx context.Context, key string) (repo string, found bool, err error)
	// S3CacheDecision resolves the project's s3CachePolicy into (import, export) for one routed build.
	// Under the cadence policy an export is granted at most once per exportInterval (grantExport stamps
	// the cadence clock; pass false for non-build probes like /prewarm). exportInterval <= 0 means
	// export on every build (the historical behaviour).
	S3CacheDecision(ctx context.Context, key string, exportInterval time.Duration, grantExport bool) (imp, exp bool)
}
