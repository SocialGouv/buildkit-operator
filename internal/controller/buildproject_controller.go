// Package controller holds the buildd reconcilers. BuildProjectReconciler turns
// each BuildProject into a StatefulSet-of-1 vanilla buildkitd + Service, with a
// retained gen2 PVC. M1 pins replicas at 1; the idle/snapshot loops (M2/M3) layer
// on top without changing this core.
package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/metrics"
	"github.com/socialgouv/buildkit-operator/internal/router"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// BuildProjectReconciler reconciles BuildProject objects.
type BuildProjectReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	Cfg             builder.Config
	KeepSnapshots   int // durability snapshots retained per project (default 3)
	MaxBuildSeconds int // inflight builds older than this stop pinning a daemon warm (default 7200)
}

const (
	projectKeyLabel = "buildkit-operator.socialgouv.github.io/project-key"
	cloneOfLabel    = "buildkit-operator.socialgouv.github.io/clone-of"
)

// +kubebuilder:rbac:groups=buildkit-operator.socialgouv.github.io,resources=buildprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=buildkit-operator.socialgouv.github.io,resources=buildprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets,resourceNames=buildkit-daemon-certs,verbs=get
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures the Service + StatefulSet exist and reflects readiness in status.
func (r *BuildProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var bp bkov1.BuildProject
	if err := r.Get(ctx, req.NamespacedName, &bp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	bp.ApplyDefaults()

	desired := desiredReplicas(&bp, time.Now(), r.maxBuildAge())
	// Reap an idle ephemeral fork daemon: delete its cache PVC + the fork BuildProject (the owner-ref
	// cascade removes the STS/Service). Forks are one-shot — nothing warm to keep — so this stops their
	// retained PVCs and CRs from accumulating.
	if router.IsForkKey(bp.Spec.Key) && desired == 0 {
		// Birth-window guard: buildd creates the fork, then a beat later stamps LastBuildTime and
		// increments inflight (and an informer-cache read can briefly lag right after the Create). On the
		// very first reconcile a fresh fork therefore still shows inflight=0 / LastBuildTime=nil → desired
		// 0; reaping it then would kill the daemon before its build ever registers (the untrusted build
		// would hang). Hold off until the fork has had its chance, then reap only if still idle.
		if age := time.Since(bp.CreationTimestamp.Time); age < forkReapGrace {
			return ctrl.Result{RequeueAfter: forkReapGrace - age}, nil
		}
		return ctrl.Result{}, r.reapFork(ctx, &bp)
	}

	if err := r.ensureService(ctx, &bp); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure service: %w", err)
	}
	if err := r.ensureStatefulSet(ctx, &bp, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure statefulset: %w", err)
	}

	var live appsv1.StatefulSet
	_ = r.Get(ctx, types.NamespacedName{Name: router.DaemonName(bp.Spec.Key), Namespace: r.Cfg.Namespace}, &live)
	ready := live.Status.ReadyReplicas

	newPhase := phaseFrom(desired, ready)
	condMsg := fmt.Sprintf("replicas desired=%d ready=%d", desired, ready)
	// A daemon that wants to be up (desired=1) but isn't ready is normally just Scaling — but if its
	// pod is wedged (CrashLoopBackOff, image pull error, OOM, Failed) it would sit in Scaling forever
	// and never surface the failure. Promote that to Failed so `kubectl get bp` shows it; we keep
	// requeuing below so it self-heals once the pod recovers.
	if newPhase == "Scaling" {
		if reason := r.daemonFailure(ctx, bp.Spec.Key); reason != "" {
			newPhase = "Failed"
			condMsg = "daemon pod unhealthy: " + reason
		}
	}
	newEndpoint := router.Endpoint(bp.Spec.Key, r.Cfg.Namespace, r.Cfg.Port)
	readyCond := boolToCond(ready >= 1)
	prev := meta.FindStatusCondition(bp.Status.Conditions, "Ready")
	// Only write status when something actually changed — a status write re-triggers reconcile
	// (status is watched) and would busy-loop with the idle requeue. A full Status().Update writes
	// every (required) status field; a stale reconcile conflicts and is retried by the requeue, so it
	// never clobbers LastBuildTime — that field is written conflict-safely by touchLastBuild.
	changed := bp.Status.Replicas != ready || bp.Status.Phase != newPhase || bp.Status.Endpoint != newEndpoint ||
		prev == nil || prev.Status != readyCond || prev.Reason != newPhase
	bp.Status.Replicas = ready
	bp.Status.Endpoint = newEndpoint
	bp.Status.Phase = newPhase
	meta.SetStatusCondition(&bp.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  readyCond,
		Reason:  newPhase,
		Message: condMsg,
	})
	if changed {
		if err := r.Status().Update(ctx, &bp); err != nil {
			return ctrl.Result{}, client.IgnoreNotFound(err)
		}
	}
	snapAfter, serr := r.maybeSnapshot(ctx, &bp)
	if serr != nil {
		l.V(1).Info("snapshot deferred", "err", serr.Error())
	}
	if ferr := r.reconcileFanout(ctx, &bp); ferr != nil {
		l.V(1).Info("fanout deferred", "err", ferr.Error())
	}
	l.V(1).Info("reconciled", "key", bp.Spec.Key, "phase", bp.Status.Phase, "ready", ready, "desired", desired)

	// Requeue at the soonest of: idle re-check (while warm) and the next snapshot due.
	requeue := time.Duration(0)
	if desired == 1 && bp.Spec.Tier != bkov1.TierHot {
		requeue = idleRecheckInterval(&bp)
	}
	// While a daemon is coming up or wedged (Scaling/Failed), poll so readiness/recovery is observed
	// even on the hot tier — the controller doesn't watch Pods, so a CrashLoop wouldn't re-trigger us.
	if desired == 1 && ready < 1 {
		if requeue == 0 || scalingRecheckInterval < requeue {
			requeue = scalingRecheckInterval
		}
	}
	if snapAfter > 0 && (requeue == 0 || snapAfter < requeue) {
		requeue = snapAfter
	}
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{}, nil
}

func (r *BuildProjectReconciler) ensureService(ctx context.Context, bp *bkov1.BuildProject) error {
	want := builder.Service(bp, r.Cfg)
	if err := ctrl.SetControllerReference(bp, want, r.Scheme); err != nil {
		return err
	}
	var existing corev1.Service
	err := r.Get(ctx, types.NamespacedName{Name: want.Name, Namespace: want.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, want)
	} else if err != nil {
		return err
	}
	if existing.Spec.Type == want.Spec.Type &&
		reflect.DeepEqual(existing.Spec.Ports, want.Spec.Ports) &&
		reflect.DeepEqual(existing.Spec.Selector, want.Spec.Selector) {
		return nil // already in the desired state — skip the no-op write (a needless API round-trip + Service watch event every reconcile)
	}
	existing.Spec.Type = want.Spec.Type // converge an old per-daemon LoadBalancer down to ClusterIP (the gateway is the external path now)
	existing.Spec.Ports = want.Spec.Ports
	existing.Spec.Selector = want.Spec.Selector
	return r.Update(ctx, &existing)
}

// templateHashAnnotation records the hash of the desired pod template on the StatefulSet, so the
// reconciler can detect a real spec change (image/env/args/profile) without diffing against
// server-defaulted fields (which would churn forever).
const templateHashAnnotation = "buildkit-operator.socialgouv.github.io/template-hash"

// ensureStatefulSet creates the STS if absent, else converges the two things we drive: the replica
// count, and the pod template when its desired hash changes (a buildkit-image bump, S3 creds, etc. —
// rolled onto the daemon; the retained PVC survives the restart). The immutable volumeClaimTemplates
// and selector are never touched (k8s forbids it).
func (r *BuildProjectReconciler) ensureStatefulSet(ctx context.Context, bp *bkov1.BuildProject, desired int32) error {
	want := builder.StatefulSet(bp, r.Cfg)
	want.Spec.Replicas = &desired
	hash := templateHash(want.Spec.Template)
	if want.Annotations == nil {
		want.Annotations = map[string]string{}
	}
	want.Annotations[templateHashAnnotation] = hash
	if err := ctrl.SetControllerReference(bp, want, r.Scheme); err != nil {
		return err
	}
	var existing appsv1.StatefulSet
	err := r.Get(ctx, types.NamespacedName{Name: want.Name, Namespace: want.Namespace}, &existing)
	if apierrors.IsNotFound(err) {
		return r.Create(ctx, want)
	} else if err != nil {
		return err
	}

	old := int32(1)
	if existing.Spec.Replicas != nil {
		old = *existing.Spec.Replicas
	}
	templateChanged := existing.Annotations[templateHashAnnotation] != hash
	if old == desired && !templateChanged {
		return nil
	}
	patch := client.MergeFrom(existing.DeepCopy())
	existing.Spec.Replicas = &desired
	if templateChanged {
		existing.Spec.Template = want.Spec.Template
		if existing.Annotations == nil {
			existing.Annotations = map[string]string{}
		}
		existing.Annotations[templateHashAnnotation] = hash
	}
	if err := r.Patch(ctx, &existing, patch); err != nil {
		return err
	}
	if old > desired {
		metrics.ScaleEvents.WithLabelValues("down").Inc()
	} else if desired > old {
		metrics.ScaleEvents.WithLabelValues("up").Inc()
	}
	return nil
}

// templateHash is a stable fingerprint of the DESIRED pod template (recomputed identically each
// reconcile), so it changes only when buildkit-operator's rendered spec changes — not on apiserver defaulting.
func templateHash(t corev1.PodTemplateSpec) string {
	b, _ := json.Marshal(t)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}

// desiredReplicas is the M2 elasticity decision: the hot tier stays at 1; otherwise
// forkReapGrace is how long a freshly-created ephemeral fork is shielded from reaping, covering the
// window between buildd's Create and its LastBuildTime stamp / inflight increment (plus informer-cache
// lag). Once a build is in flight the fork lives on inflight; this only protects the birth window.
const forkReapGrace = 90 * time.Second

// 1 while a build is in flight or the project was active within IdleTimeoutSec, else 0
// (scale-to-zero — the PVC is retained via volumeClaimTemplates, so the next build just
// reattaches the warm cache; no restore). Idle timeout default seeded from bench B.
func desiredReplicas(bp *bkov1.BuildProject, now time.Time, maxBuild time.Duration) int32 {
	if bp.Spec.Tier == bkov1.TierHot {
		return 1
	}
	if bp.Status.InflightBuilds > 0 && !inflightStale(bp, now, maxBuild) {
		return 1
	}
	if bp.Status.LastBuildTime != nil && bp.Spec.IdleTimeoutSec > 0 {
		if now.Sub(bp.Status.LastBuildTime.Time) < time.Duration(bp.Spec.IdleTimeoutSec)*time.Second {
			return 1
		}
	}
	return 0
}

// inflightStale reports whether the inflight counter is too old to trust — a client almost certainly
// missed its /complete call. It bounds the blast radius of a leaked counter to maxBuild, so a crashed
// build can't pin a hot daemon forever (buildd increments inflight on /route, decrements on /complete).
func inflightStale(bp *bkov1.BuildProject, now time.Time, maxBuild time.Duration) bool {
	return bp.Status.LastBuildTime == nil || now.Sub(bp.Status.LastBuildTime.Time) > maxBuild
}

// maxBuildAge is the staleness window for the inflight safety net (default 2h).
func (r *BuildProjectReconciler) maxBuildAge() time.Duration {
	if r.MaxBuildSeconds <= 0 {
		return 2 * time.Hour
	}
	return time.Duration(r.MaxBuildSeconds) * time.Second
}

// reapFork tears down an idle ephemeral fork daemon: its retained cache PVC (no warm value to keep)
// then the fork BuildProject itself (the owner-ref cascade removes the STS + Service).
func (r *BuildProjectReconciler) reapFork(ctx context.Context, bp *bkov1.BuildProject) error {
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: router.CachePVCName(bp.Spec.Key), Namespace: r.Cfg.Namespace}, &pvc)
	if err == nil {
		if derr := r.Delete(ctx, &pvc); derr != nil && !apierrors.IsNotFound(derr) {
			return derr
		}
	} else if !apierrors.IsNotFound(err) {
		return err
	}
	return client.IgnoreNotFound(r.Delete(ctx, bp))
}

// idleRecheckInterval requeues a warm project often enough to scale it down promptly
// once it crosses IdleTimeoutSec (events alone won't fire when nothing changes).
func idleRecheckInterval(bp *bkov1.BuildProject) time.Duration {
	iv := time.Duration(bp.Spec.IdleTimeoutSec) * time.Second / 6
	if iv < 30*time.Second {
		iv = 30 * time.Second
	}
	return iv
}

func phaseFrom(desired, ready int32) string {
	switch {
	case desired == 0:
		return "Idle"
	case ready >= 1:
		return "Warm"
	default:
		return "Scaling"
	}
}

// scalingRecheckInterval is how often a not-yet-ready daemon is re-reconciled so readiness (or a stuck
// pod transitioning into Failed, then recovering) is observed even without a Pod watch.
const scalingRecheckInterval = 30 * time.Second

// daemonFailure returns a short reason when the project's daemon pod is wedged (so the phase can go
// Failed instead of an indefinite Scaling), or "" when it's merely still starting. It inspects the
// pod the StatefulSet manages: terminal pod phase, or a container blocked in a back-off / pull / OOM
// waiting reason. Best-effort — a list error just yields "" (treated as "still scaling").
func (r *BuildProjectReconciler) daemonFailure(ctx context.Context, key string) string {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(r.Cfg.Namespace), client.MatchingLabels{projectKeyLabel: key}); err != nil {
		return ""
	}
	// blockedReasons are container waiting states that won't clear on their own (vs the transient
	// ContainerCreating / PodInitializing that a healthy cold start passes through).
	blockedReasons := map[string]bool{
		"CrashLoopBackOff": true, "ImagePullBackOff": true, "ErrImagePull": true,
		"CreateContainerError": true, "CreateContainerConfigError": true, "InvalidImageName": true,
	}
	for i := range pods.Items {
		p := &pods.Items[i]
		if p.Status.Phase == corev1.PodFailed {
			return "pod " + p.Name + " phase=Failed"
		}
		for _, cs := range p.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && blockedReasons[w.Reason] {
				return cs.Name + ": " + w.Reason
			}
			if t := cs.LastTerminationState.Terminated; t != nil && t.Reason == "OOMKilled" {
				return cs.Name + ": OOMKilled"
			}
		}
	}
	return ""
}

func boolToCond(b bool) metav1.ConditionStatus {
	if b {
		return metav1.ConditionTrue
	}
	return metav1.ConditionFalse
}

// maybeSnapshot takes a durability VolumeSnapshot of the project's cache when the cadence
// (SnapshotEverySec) is due. OVH supports in-use snapshots (bench D-bis), so this needs no
// scale-to-zero. Returns the duration until the next snapshot is due (0 = disabled).
func (r *BuildProjectReconciler) maybeSnapshot(ctx context.Context, bp *bkov1.BuildProject) (time.Duration, error) {
	if bp.Spec.SnapshotEverySec <= 0 || r.Cfg.SnapshotClass == "" {
		return 0, nil
	}
	cadence := time.Duration(bp.Spec.SnapshotEverySec) * time.Second
	pvcName := router.CachePVCName(bp.Spec.Key)
	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: r.Cfg.Namespace}, &pvc); err != nil {
		return cadence, client.IgnoreNotFound(err) // no cache PVC yet (never built) — retry later
	}
	var snaps volumesnapshotv1.VolumeSnapshotList
	if err := r.List(ctx, &snaps, client.InNamespace(r.Cfg.Namespace), client.MatchingLabels{projectKeyLabel: bp.Spec.Key}); err != nil {
		return cadence, err
	}
	if latest := newestSnapshot(snaps.Items); latest != nil {
		if age := time.Since(latest.CreationTimestamp.Time); age < cadence {
			return cadence - age, nil
		}
	}
	// UnixNano (not Unix) so two reconciles within the same second can't collide on the snapshot name.
	name := fmt.Sprintf("snap-%s-%d", bp.Spec.Key, time.Now().UnixNano())
	snap := &volumesnapshotv1.VolumeSnapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: r.Cfg.Namespace, Labels: map[string]string{projectKeyLabel: bp.Spec.Key}},
		Spec: volumesnapshotv1.VolumeSnapshotSpec{
			Source:                  volumesnapshotv1.VolumeSnapshotSource{PersistentVolumeClaimName: ptr(pvcName)},
			VolumeSnapshotClassName: ptr(r.Cfg.SnapshotClass),
		},
	}
	if err := ctrl.SetControllerReference(bp, snap, r.Scheme); err != nil {
		return cadence, err
	}
	if err := r.Create(ctx, snap); err != nil {
		return cadence, err
	}
	metrics.SnapshotsTotal.Inc()
	orig := bp.DeepCopy()
	bp.Status.LastSnapshot = name
	// LastSnapshot gates fanout (reconcileFanout returns early while it's empty), so a dropped patch
	// silently stalls fan-out until the next snapshot is due. Log it instead of swallowing.
	if err := r.Status().Patch(ctx, bp, client.MergeFrom(orig)); err != nil {
		log.FromContext(ctx).V(1).Info("persist LastSnapshot failed", "err", err.Error(), "snapshot", name)
	}
	r.pruneSnapshots(ctx, append(snaps.Items, *snap))
	return cadence, nil
}

func newestSnapshot(items []volumesnapshotv1.VolumeSnapshot) *volumesnapshotv1.VolumeSnapshot {
	var newest *volumesnapshotv1.VolumeSnapshot
	for i := range items {
		if newest == nil || items[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = &items[i]
		}
	}
	return newest
}

// pruneSnapshots keeps the newest KeepSnapshots (default 3) per project and deletes the rest.
func (r *BuildProjectReconciler) pruneSnapshots(ctx context.Context, items []volumesnapshotv1.VolumeSnapshot) {
	keep := r.KeepSnapshots
	if keep <= 0 {
		keep = 3
	}
	if len(items) <= keep {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].CreationTimestamp.After(items[j].CreationTimestamp.Time) })
	for i := keep; i < len(items); i++ {
		_ = r.Delete(ctx, &items[i])
	}
}

// reconcileFanout (M5, conditional) materializes Fanout warm clone daemons for a saturated
// project. Each clone is a sibling BuildProject seeded (CoW) from the latest snapshot — the bench
// proved Cinder clones are CoW (fast boot). Clones run hot, are owned by the canonical (cascade
// GC), and don't fan out themselves; layers converge via shared S3, cache mounts stay per-daemon.
// Vertical scaling (Resources / CacheVolumeGi) remains the first resort.
func (r *BuildProjectReconciler) reconcileFanout(ctx context.Context, bp *bkov1.BuildProject) error {
	// Prune clones beyond the desired Fanout FIRST, so lowering or disabling Fanout removes orphan hot
	// clones instead of leaking them. This runs even when Fanout==0 (full teardown of fan-out).
	want := map[string]bool{}
	for i := int32(1); i <= bp.Spec.Fanout; i++ {
		want[router.CloneKey(bp.Spec.Key, int(i))] = true
	}
	var clones bkov1.BuildProjectList
	if err := r.List(ctx, &clones, client.InNamespace(r.Cfg.Namespace), client.MatchingLabels{cloneOfLabel: bp.Spec.Key}); err != nil {
		return err
	}
	for i := range clones.Items {
		if !want[clones.Items[i].Name] {
			if err := r.Delete(ctx, &clones.Items[i]); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
	}

	if bp.Spec.Fanout <= 0 || bp.Status.LastSnapshot == "" {
		return nil // disabled, or no snapshot to clone from yet
	}
	for i := int32(1); i <= bp.Spec.Fanout; i++ {
		ckey := router.CloneKey(bp.Spec.Key, int(i))
		var clone bkov1.BuildProject
		if err := r.Get(ctx, types.NamespacedName{Name: ckey, Namespace: r.Cfg.Namespace}, &clone); err == nil {
			continue
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		clone = bkov1.BuildProject{
			ObjectMeta: metav1.ObjectMeta{
				Name: ckey, Namespace: r.Cfg.Namespace,
				Labels: map[string]string{projectKeyLabel: bp.Spec.Key, cloneOfLabel: bp.Spec.Key},
			},
			Spec: bkov1.DeriveChild(bp.Spec, bp.Status.LastSnapshot, bkov1.CloneChild, ckey),
		}
		if err := ctrl.SetControllerReference(bp, &clone, r.Scheme); err != nil {
			return err
		}
		if err := r.Create(ctx, &clone); err != nil {
			return err
		}
	}
	return nil
}

func ptr[T any](v T) *T { return &v }

// SetupWithManager wires the reconciler and the objects it owns.
func (r *BuildProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&bkov1.BuildProject{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&volumesnapshotv1.VolumeSnapshot{}).
		Complete(r)
}
