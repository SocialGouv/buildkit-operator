// Package k8s is the Kubernetes backend of the buildd provisioner: it materialises the one hot vanilla
// buildkitd per project as a BuildProject CRD (reconciled into a StatefulSet-of-1 + Service + retained
// PVC by internal/controller) and addresses it via Service DNS or the shared SNI gateway.
//
// The logic here is moved verbatim from cmd/buildd (ensureBuildProject/ready/waitReady/endpointFor/
// addInflight + the fork derivation) behind the provisioner.Provisioner contract — no behaviour change.
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
	log                 logr.Logger
}

// compile-time check that the k8s backend satisfies the contract.
var _ provisioner.Provisioner = (*Provisioner)(nil)

// New builds the Kubernetes provisioner from the shared builder.Config plus the routing-API knobs.
func New(c client.Client, cfg builder.Config, wait time.Duration, gatewayHost string, gatewayPort int32, log logr.Logger) *Provisioner {
	return &Provisioner{
		c:                   c,
		namespace:           cfg.Namespace,
		port:                cfg.Port,
		wait:                wait,
		gatewayHost:         gatewayHost,
		gatewayPort:         gatewayPort,
		defaultStorageClass: cfg.DefaultStorageClass,
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
	// is the cold-start flake: AddInflight's Get returned NotFound right after Create, so the touch was
	// dropped and the warm-tier project never scaled up.
	now := metav1.Now()
	created.Status.LastBuildTime = &now
	if err := p.c.Status().Update(ctx, created); err != nil {
		p.log.Error(err, "stamp LastBuildTime at create failed; relying on the AddInflight touch", "key", spec.Key)
	}
	return nil
}

// Ready reports whether the project's daemon already has a ready replica (warm fast path).
func (p *Provisioner) Ready(ctx context.Context, key string) bool {
	var sts appsv1.StatefulSet
	if err := p.c.Get(ctx, types.NamespacedName{Name: router.DaemonName(key), Namespace: p.namespace}, &sts); err != nil {
		return false
	}
	return sts.Status.ReadyReplicas >= 1
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

// WaitReady polls the daemon StatefulSet until it has a ready replica or the wait budget elapses.
func (p *Provisioner) WaitReady(ctx context.Context, key string) error {
	deadline := time.Now().Add(p.wait)
	for {
		var sts appsv1.StatefulSet
		err := p.c.Get(ctx, types.NamespacedName{Name: router.DaemonName(key), Namespace: p.namespace}, &sts)
		if err == nil && sts.Status.ReadyReplicas >= 1 {
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

// AddInflight adjusts Status.InflightBuilds by delta (floored at 0) and stamps LastBuildTime now. It
// re-Gets and retries on conflict AND not-found: a Status().Update that lost a 409 race with the
// reconciler would leave the count wrong (the project could scale down mid-build, or never scale down),
// and right after /route|/prewarm creates the project the informer cache can still miss it, so a plain
// Get returns NotFound — retrying lets the cache catch up instead of dropping the touch (which would
// leave a warm-tier project stuck Idle). A terminal failure (all retries exhausted) is logged.
func (p *Provisioner) AddInflight(ctx context.Context, key string, delta int32) {
	retriable := func(err error) bool { return apierrors.IsConflict(err) || apierrors.IsNotFound(err) }
	// ~6.4s of retries (vs DefaultBackoff's ~40ms): the informer cache can lag etcd by a beat right after
	// the project is created, so a too-short backoff drops the touch — and for an ephemeral fork that
	// touch is what keeps it from being reaped before its build registers.
	backoff := wait.Backoff{Steps: 8, Duration: 100 * time.Millisecond, Factor: 1.6, Jitter: 0.1}
	err := retry.OnError(backoff, retriable, func() error {
		var bp bkov1.BuildProject
		if err := p.c.Get(ctx, types.NamespacedName{Name: key, Namespace: p.namespace}, &bp); err != nil {
			return err
		}
		n := bp.Status.InflightBuilds + delta
		if n < 0 {
			n = 0
		}
		bp.Status.InflightBuilds = n
		now := metav1.Now()
		bp.Status.LastBuildTime = &now
		return p.c.Status().Update(ctx, &bp)
	})
	if err != nil {
		p.log.Error(err, "AddInflight failed; inflight count may be skewed until the max-build-seconds safety net", "key", key, "delta", delta)
	}
}
