// Package k8s is the Kubernetes backend of the buildd provisioner: it materialises the one hot vanilla
// buildkitd per project as a BuildProject CRD (reconciled into a StatefulSet-of-1 + Service + retained
// PVC by internal/controller) and addresses it via Service DNS or the shared SNI gateway.
//
// The logic here is moved verbatim from cmd/buildd (ensureBuildProject/ready/waitReady/endpointFor/
// the inflight status writes + the fork derivation) behind the provisioner.Provisioner contract.
// The background reconcile/scale/snapshot loop stays wired as controller-runtime manager Runnables in
// cmd/buildd's k8s setup; this type is only the imperative surface the routing handlers call.
package k8s

import (
	"context"
	"errors"
	"time"

	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/provisioner"
	"github.com/socialgouv/buildkit-operator/internal/router"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Provisioner is the Kubernetes implementation of provisioner.Provisioner.
type Provisioner struct {
	c           client.Client
	namespace   string        // namespace the per-project daemons + BuildProjects live in
	port        int32         // buildkitd mTLS port advertised on /route
	wait        time.Duration // cold-start wait budget for WaitReady
	gatewayHost string        // when set, /route returns <daemon>.<gatewayHost> for off-cluster CI
	gatewayPort int32         // external port for the gateway endpoint (0 = use port)
	// defaultStorageClass is stamped onto BuildProjects created with no StorageClass (cloud-portable:
	// empty leaves it unset so the daemon PVC uses the cluster's DEFAULT StorageClass).
	defaultStorageClass string
	// maxBuild is the --max-build-seconds window. The routing API applies it on every status write, so
	// a leaked entry is shed at the next /route or /complete on that project instead of waiting for a
	// reconcile — which keeps the bounded inflight set from filling up with entries nobody will release.
	maxBuild time.Duration
	log      logr.Logger
}

// compile-time check that the k8s backend satisfies the contract.
var _ provisioner.Provisioner = (*Provisioner)(nil)

// New builds the Kubernetes provisioner from the shared builder.Config plus the routing-API knobs.
func New(c client.Client, cfg builder.Config, wait time.Duration, gatewayHost string, gatewayPort int32, maxBuild time.Duration, log logr.Logger) *Provisioner {
	return &Provisioner{
		c:                   c,
		namespace:           cfg.Namespace,
		port:                cfg.Port,
		wait:                wait,
		gatewayHost:         gatewayHost,
		gatewayPort:         gatewayPort,
		defaultStorageClass: cfg.DefaultStorageClass,
		maxBuild:            maxBuild,
		log:                 log,
	}
}

// Ensure provisions the canonical daemon, or the ephemeral fork daemon when untrusted: a fork PR gets a
// distinct key, derived read-only from the canonical snapshot, so it can never poison the canonical
// cache. Same derivation policy as fan-out clones, via bkov1.DeriveChild.
func (p *Provisioner) Ensure(ctx context.Context, spec bkov1.BuildProjectSpec, untrusted bool) error {
	if untrusted {
		canonical := spec.Key
		seed := ""
		var canon bkov1.BuildProject
		if err := p.c.Get(ctx, types.NamespacedName{Name: canonical, Namespace: p.namespace}, &canon); err == nil {
			seed = canon.Status.LastSnapshot
			// Derive from the LIVE canonical spec, not the request-reconstructed
			// one: auto-grow (and hand edits) move the canonical's CacheVolumeGi
			// past the request/rule value, and a fork PVC smaller than the
			// snapshot it restores from is refused by the CSI provisioner — the
			// untrusted build would then 504 forever.
			spec = canon.Spec
		}
		spec = bkov1.DeriveChild(spec, seed, bkov1.ForkChild, router.ForkKey(canonical))
	}
	return p.ensureBuildProject(ctx, spec)
}

func (p *Provisioner) ensureBuildProject(ctx context.Context, spec bkov1.BuildProjectSpec) error {
	var bp bkov1.BuildProject
	err := p.c.Get(ctx, types.NamespacedName{Name: spec.Key, Namespace: p.namespace}, &bp)
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	// Stamp the operator-wide default StorageClass when the project sets none (empty default = the
	// cluster's default StorageClass). Keeps cloud specifics in buildd config, not the API type.
	if spec.StorageClass == "" {
		spec.StorageClass = p.defaultStorageClass
	}
	created := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Key, Namespace: p.namespace},
		Spec:       spec,
	}
	if err := p.c.Create(ctx, created); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return nil // raced another /route or /prewarm for the same key — fine
		}
		return err
	}
	// Warm from birth: desiredReplicas only holds a warm-tier replica once LastBuildTime is set, so stamp
	// it now on the JUST-CREATED object (it already carries its ResourceVersion) — not via a fresh Get,
	// whose informer cache can still miss the new object and leave the daemon stuck Idle. That cache race
	// is the cold-start flake: the status write's Get returned NotFound right after Create, so the touch was
	// dropped and the warm-tier project never scaled up.
	now := metav1.Now()
	created.Status.LastBuildTime = &now
	if err := p.c.Status().Update(ctx, created); err != nil {
		p.log.Error(err, "stamp LastBuildTime at create failed; relying on the /route status write", "key", spec.Key)
	}
	return nil
}

// Ready reports whether the project's daemon is serving RIGHT NOW (warm fast path).
func (p *Provisioner) Ready(ctx context.Context, key string) bool {
	var sts appsv1.StatefulSet
	if err := p.c.Get(ctx, types.NamespacedName{Name: router.DaemonName(key), Namespace: p.namespace}, &sts); err != nil {
		return false
	}
	return daemonServing(&sts)
}

// daemonServing reports whether the daemon StatefulSet has a ready replica that is ALSO the pod we
// currently want. ReadyReplicas alone is not enough: for the whole first stretch of a pod-template
// roll it still counts the OUTGOING pod, so /route would hand a client the endpoint of a daemon
// Kubernetes is about to delete — the client then dials through the gateway and gets a connection
// refused mid-handshake ("failed to write client preface: EOF"). Requiring the observed generation to
// be current and the ready replica to be on the update revision makes a roll look like a cold start
// instead: /route waits for the new pod rather than advertising the dying one.
func daemonServing(sts *appsv1.StatefulSet) bool {
	return sts.Status.ObservedGeneration >= sts.Generation &&
		sts.Status.ReadyReplicas >= 1 &&
		sts.Status.UpdatedReplicas >= 1 &&
		sts.Status.CurrentRevision == sts.Status.UpdateRevision
}

// Endpoint returns the address clients dial: a DETERMINISTIC gateway SNI hostname when a gateway domain
// is configured (off-cluster CI reaches every daemon through the single shared SNI gateway), else the
// in-cluster Service DNS. No polling — the endpoint is computable from the key.
func (p *Provisioner) Endpoint(key string) string {
	if p.gatewayHost != "" {
		port := p.port
		if p.gatewayPort > 0 {
			port = p.gatewayPort
		}
		return router.EndpointHost(router.DaemonName(key)+"."+p.gatewayHost, port)
	}
	return router.Endpoint(key, p.namespace, p.port)
}

// WaitReady polls the daemon StatefulSet until it is serving (see daemonServing) or the wait budget
// elapses — which covers a daemon being rolled, not just one starting from zero.
func (p *Provisioner) WaitReady(ctx context.Context, key string) error {
	deadline := time.Now().Add(p.wait)
	for {
		var sts appsv1.StatefulSet
		err := p.c.Get(ctx, types.NamespacedName{Name: router.DaemonName(key), Namespace: p.namespace}, &sts)
		if err == nil && daemonServing(&sts) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for Ready replica")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

// StartInflight registers a routed build under id (see touchStatus).
func (p *Provisioner) StartInflight(ctx context.Context, key, id string) {
	p.touchStatus(ctx, key, "start", func(st *bkov1.BuildProjectStatus, now metav1.Time) {
		st.SetInflight(bkov1.StartInflight(p.live(st, now.Time), id, now))
		// A routed build (not a prewarm touch or a /complete release) feeds the cadence ring that
		// drives the adaptive idle window. Forks are excluded from adaptivity, so their one-shot
		// CRs skip the ring.
		if !router.IsForkKey(key) {
			st.RecentBuildTimes = bkov1.RecordBuildTime(st.RecentBuildTimes, now)
		}
	})
}

// EndInflight releases the build registered under id; an empty id releases the oldest entry.
func (p *Provisioner) EndInflight(ctx context.Context, key, id string) {
	p.touchStatus(ctx, key, "end", func(st *bkov1.BuildProjectStatus, now metav1.Time) {
		entries, found := bkov1.EndInflightBefore(p.live(st, now.Time), id, now.Time.Add(-p.maxBuild))
		if !found {
			// Nothing to release: a duplicate /complete, or an entry the safety net already expired.
			// Logged at debug rather than dropped silently, because a burst of these means clients
			// are releasing builds the server no longer tracks.
			p.log.V(1).Info("release for an unknown inflight build", "key", key, "buildID", id)
			return
		}
		st.SetInflight(entries)
	})
}

// live is the project's inflight set with the entries past --max-build-seconds already shed, and any
// pre-entries count adopted first. Every status write goes through it, so the routing API and the
// reconciler agree on what "still running" means.
func (p *Provisioner) live(st *bkov1.BuildProjectStatus, now time.Time) []bkov1.InflightBuild {
	bkov1.AdoptLegacyInflight(st)
	if p.maxBuild <= 0 {
		return st.Inflight
	}
	entries, _ := bkov1.ExpireInflight(st.Inflight, now, p.maxBuild)
	return entries
}

// Touch stamps LastBuildTime without registering a build (the /prewarm path).
func (p *Provisioner) Touch(ctx context.Context, key string) {
	p.touchStatus(ctx, key, "touch", func(*bkov1.BuildProjectStatus, metav1.Time) {})
}

// retriableStatusErr and statusBackoff are the shared retry envelope for the status writes on the
// /route path. ~6.4s of retries (vs DefaultBackoff's ~40ms): the informer cache can lag etcd by a beat
// right after the project is created, so a too-short budget drops the write exactly when it matters —
// and for an ephemeral fork that write is what keeps it from being reaped before its build registers.
func retriableStatusErr(err error) bool {
	return apierrors.IsConflict(err) || apierrors.IsNotFound(err)
}

func statusBackoff() wait.Backoff {
	return wait.Backoff{Steps: 8, Duration: 100 * time.Millisecond, Factor: 1.6, Jitter: 0.1}
}

// touchStatus applies mutate to the project's status and stamps LastBuildTime now. It re-Gets and
// retries on conflict AND not-found: a Status().Update that lost a 409 race with the reconciler would
// leave the inflight set wrong (the project could scale down mid-build, or never scale down), and right
// after /route|/prewarm creates the project the informer cache can still miss it, so a plain Get returns
// NotFound — retrying lets the cache catch up instead of dropping the touch (which would leave a
// warm-tier project stuck Idle). A terminal failure (all retries exhausted) is logged.
func (p *Provisioner) touchStatus(ctx context.Context, key, op string, mutate func(*bkov1.BuildProjectStatus, metav1.Time)) {
	err := retry.OnError(statusBackoff(), retriableStatusErr, func() error {
		var bp bkov1.BuildProject
		if err := p.c.Get(ctx, types.NamespacedName{Name: key, Namespace: p.namespace}, &bp); err != nil {
			return err
		}
		now := metav1.Now()
		mutate(&bp.Status, now)
		bp.Status.LastBuildTime = &now
		return p.c.Status().Update(ctx, &bp)
	})
	if err != nil {
		p.log.Error(err, "inflight status update failed; the count may be skewed until the max-build-seconds safety net", "key", key, "op", op)
	}
}

// S3CacheDecision resolves the project's s3CachePolicy for one routed build. The retained PVC covers
// warm restarts, so the cold cache's export side is throttled to exportInterval under the default
// cadence policy — the grant is CAS'd through the status (conflict-retried re-check), so concurrent
// routes across buildd replicas elect a single exporter per window. Fail-open to the historical
// (import+export) behaviour on a transient read error: a spurious export is cheap, a lost import
// never is.
func (p *Provisioner) S3CacheDecision(ctx context.Context, key string, exportInterval time.Duration, grantExport bool) (bool, bool) {
	// StartInflight ALWAYS writes just before this runs on the /route path, so the first CAS attempt
	// conflicting is the common case here, not the edge — hence the same envelope as touchStatus.
	retriable, backoff := retriableStatusErr, statusBackoff()

	var bp bkov1.BuildProject
	if err := retry.OnError(backoff, apierrors.IsNotFound, func() error {
		return p.c.Get(ctx, types.NamespacedName{Name: key, Namespace: p.namespace}, &bp)
	}); err != nil {
		p.log.V(1).Info("S3CacheDecision: project read failed; defaulting to import+export", "key", key, "err", err)
		return true, true
	}
	bp.ApplyDefaults()
	switch bp.Spec.S3CachePolicy {
	case bkov1.S3CacheNever:
		return false, false
	case bkov1.S3CacheAlways:
		return true, true
	}
	// cadence: import always; export only when the window elapsed AND this call may grant.
	if !grantExport || exportInterval <= 0 {
		return true, exportInterval <= 0 && grantExport
	}
	granted := false
	err := retry.OnError(backoff, retriable, func() error {
		var cur bkov1.BuildProject
		if err := p.c.Get(ctx, types.NamespacedName{Name: key, Namespace: p.namespace}, &cur); err != nil {
			return err
		}
		// Re-check the policy on the fresh read: a project flipped to never/always
		// mid-flight must not consume (or bypass) the cadence window.
		cur.ApplyDefaults()
		if cur.Spec.S3CachePolicy != bkov1.S3CacheCadence {
			granted = cur.Spec.S3CachePolicy == bkov1.S3CacheAlways
			return nil
		}
		if cur.Status.LastCacheExportGrant != nil && time.Since(cur.Status.LastCacheExportGrant.Time) < exportInterval {
			granted = false
			return nil // another route won this window
		}
		now := metav1.Now()
		cur.Status.LastCacheExportGrant = &now
		if err := p.c.Status().Update(ctx, &cur); err != nil {
			return err
		}
		granted = true
		return nil
	})
	if err != nil {
		p.log.V(1).Info("S3CacheDecision: grant stamp failed; skipping export this build", "key", key, "err", err)
		return true, false
	}
	return true, granted
}
