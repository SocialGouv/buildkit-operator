// Package controller holds the buildd reconcilers. BuildProjectReconciler turns
// each BuildProject into a StatefulSet-of-1 vanilla buildkitd + Service, with a
// retained gen2 PVC. M1 pins replicas at 1; the idle/snapshot loops (M2/M3) layer
// on top without changing this core.
package controller

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	buildcatv1 "github.com/socialgouv/buildcat/api/v1alpha1"
	"github.com/socialgouv/buildcat/internal/builder"
	"github.com/socialgouv/buildcat/internal/metrics"
	"github.com/socialgouv/buildcat/internal/router"
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
	Scheme        *runtime.Scheme
	Cfg           builder.Config
	KeepSnapshots int // durability snapshots retained per project (default 3)
}

const projectKeyLabel = "buildcat.dev/project-key"

// +kubebuilder:rbac:groups=buildcat.dev,resources=buildprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=buildcat.dev,resources=buildprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=snapshot.storage.k8s.io,resources=volumesnapshots,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

// Reconcile ensures the Service + StatefulSet exist and reflects readiness in status.
func (r *BuildProjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	l := log.FromContext(ctx)

	var bp buildcatv1.BuildProject
	if err := r.Get(ctx, req.NamespacedName, &bp); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	applyDefaults(&bp)

	if err := r.ensureService(ctx, &bp); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure service: %w", err)
	}
	desired := desiredReplicas(&bp, time.Now())
	if err := r.ensureStatefulSet(ctx, &bp, desired); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensure statefulset: %w", err)
	}

	var live appsv1.StatefulSet
	_ = r.Get(ctx, types.NamespacedName{Name: router.DaemonName(bp.Spec.Key), Namespace: r.Cfg.Namespace}, &live)
	ready := live.Status.ReadyReplicas

	newPhase := phaseFrom(desired, ready)
	newEndpoint := router.Endpoint(bp.Spec.Key, r.Cfg.Namespace, r.Cfg.Port)
	readyCond := boolToCond(ready >= 1)
	prev := meta.FindStatusCondition(bp.Status.Conditions, "Ready")
	// Only write status when something actually changed — an unconditional Status().Update
	// re-triggers reconcile (status is watched) and would busy-loop with the idle requeue.
	changed := bp.Status.Replicas != ready || bp.Status.Phase != newPhase || bp.Status.Endpoint != newEndpoint ||
		prev == nil || prev.Status != readyCond || prev.Reason != newPhase
	bp.Status.Replicas = ready
	bp.Status.Endpoint = newEndpoint
	bp.Status.Phase = newPhase
	meta.SetStatusCondition(&bp.Status.Conditions, metav1.Condition{
		Type:    "Ready",
		Status:  readyCond,
		Reason:  newPhase,
		Message: fmt.Sprintf("replicas desired=%d ready=%d", desired, ready),
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
	if desired == 1 && bp.Spec.Tier != buildcatv1.TierHot {
		requeue = idleRecheckInterval(&bp)
	}
	if snapAfter > 0 && (requeue == 0 || snapAfter < requeue) {
		requeue = snapAfter
	}
	if requeue > 0 {
		return ctrl.Result{RequeueAfter: requeue}, nil
	}
	return ctrl.Result{}, nil
}

func (r *BuildProjectReconciler) ensureService(ctx context.Context, bp *buildcatv1.BuildProject) error {
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
	if reflect.DeepEqual(existing.Spec.Ports, want.Spec.Ports) && reflect.DeepEqual(existing.Spec.Selector, want.Spec.Selector) {
		return nil // already in the desired state — skip the no-op write (a needless API round-trip + Service watch event every reconcile)
	}
	existing.Spec.Ports = want.Spec.Ports
	existing.Spec.Selector = want.Spec.Selector
	return r.Update(ctx, &existing)
}

// ensureStatefulSet creates the STS if absent, else patches only the mutable bit
// we drive (replicas). Immutable fields (volumeClaimTemplates, selector) are never
// updated in place — that's a create-time contract.
func (r *BuildProjectReconciler) ensureStatefulSet(ctx context.Context, bp *buildcatv1.BuildProject, desired int32) error {
	want := builder.StatefulSet(bp, r.Cfg)
	want.Spec.Replicas = &desired
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
	old := int32(0)
	if existing.Spec.Replicas != nil {
		old = *existing.Spec.Replicas
	}
	if old != desired {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec.Replicas = &desired
		if err := r.Patch(ctx, &existing, patch); err != nil {
			return err
		}
		if desired > old {
			metrics.ScaleEvents.WithLabelValues("up").Inc()
		} else {
			metrics.ScaleEvents.WithLabelValues("down").Inc()
		}
	}
	return nil
}

// desiredReplicas is the M2 elasticity decision: the hot tier stays at 1; otherwise
// 1 while a build is in flight or the project was active within IdleTimeoutSec, else 0
// (scale-to-zero — the PVC is retained via volumeClaimTemplates, so the next build just
// reattaches the warm cache; no restore). Idle timeout default seeded from bench B.
func desiredReplicas(bp *buildcatv1.BuildProject, now time.Time) int32 {
	if bp.Spec.Tier == buildcatv1.TierHot {
		return 1
	}
	if bp.Status.InflightBuilds > 0 {
		return 1
	}
	if bp.Status.LastBuildTime != nil && bp.Spec.IdleTimeoutSec > 0 {
		if now.Sub(bp.Status.LastBuildTime.Time) < time.Duration(bp.Spec.IdleTimeoutSec)*time.Second {
			return 1
		}
	}
	return 0
}

// idleRecheckInterval requeues a warm project often enough to scale it down promptly
// once it crosses IdleTimeoutSec (events alone won't fire when nothing changes).
func idleRecheckInterval(bp *buildcatv1.BuildProject) time.Duration {
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

// applyDefaults guards against BuildProjects created without CRD/apiserver defaulting (the fake
// client in tests, or objects built in-process). It MUST mirror the +kubebuilder:default markers in
// api/v1alpha1/buildproject_types.go — keep the two in sync.
func applyDefaults(bp *buildcatv1.BuildProject) {
	if bp.Spec.StorageClass == "" {
		bp.Spec.StorageClass = "csi-cinder-high-speed-gen2"
	}
	if bp.Spec.CacheVolumeGi == 0 {
		bp.Spec.CacheVolumeGi = 60
	}
	if bp.Spec.Tier == "" {
		bp.Spec.Tier = buildcatv1.TierWarm
	}
	if bp.Spec.IdleTimeoutSec == 0 {
		// Mirrors +kubebuilder:default=900 (bench B). Without this, an undefaulted warm project
		// scales to zero immediately after every build (desiredReplicas skips the LastBuildTime
		// window when IdleTimeoutSec==0). 0 is unreachable via the API (omitempty => defaulted).
		bp.Spec.IdleTimeoutSec = 900
	}
	if bp.Spec.SecurityProfile == "" {
		bp.Spec.SecurityProfile = buildcatv1.ProfileRootless
	}
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
func (r *BuildProjectReconciler) maybeSnapshot(ctx context.Context, bp *buildcatv1.BuildProject) (time.Duration, error) {
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
	name := fmt.Sprintf("snap-%s-%d", bp.Spec.Key, time.Now().Unix())
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
	_ = r.Status().Patch(ctx, bp, client.MergeFrom(orig))
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
func (r *BuildProjectReconciler) reconcileFanout(ctx context.Context, bp *buildcatv1.BuildProject) error {
	if bp.Spec.Fanout <= 0 || bp.Status.LastSnapshot == "" {
		return nil // disabled, or no snapshot to clone from yet
	}
	for i := int32(1); i <= bp.Spec.Fanout; i++ {
		ckey := router.CloneKey(bp.Spec.Key, int(i))
		var clone buildcatv1.BuildProject
		if err := r.Get(ctx, types.NamespacedName{Name: ckey, Namespace: r.Cfg.Namespace}, &clone); err == nil {
			continue
		} else if !apierrors.IsNotFound(err) {
			return err
		}
		clone = buildcatv1.BuildProject{
			ObjectMeta: metav1.ObjectMeta{
				Name: ckey, Namespace: r.Cfg.Namespace,
				Labels: map[string]string{projectKeyLabel: bp.Spec.Key, "buildcat.dev/clone-of": bp.Spec.Key},
			},
			Spec: buildcatv1.DeriveChild(bp.Spec, bp.Status.LastSnapshot, buildcatv1.CloneChild, ckey),
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
		For(&buildcatv1.BuildProject{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Owns(&volumesnapshotv1.VolumeSnapshot{}).
		Complete(r)
}
