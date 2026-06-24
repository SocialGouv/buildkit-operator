// Package controller holds the buildd reconcilers. BuildProjectReconciler turns
// each BuildProject into a StatefulSet-of-1 vanilla buildkitd + Service, with a
// retained gen2 PVC. M1 pins replicas at 1; the idle/snapshot loops (M2/M3) layer
// on top without changing this core.
package controller

import (
	"context"
	"fmt"
	"time"

	buildcatv1 "github.com/devthejo/buildcat/api/v1alpha1"
	"github.com/devthejo/buildcat/internal/builder"
	"github.com/devthejo/buildcat/internal/router"
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
	Scheme *runtime.Scheme
	Cfg    builder.Config
}

// +kubebuilder:rbac:groups=buildcat.dev,resources=buildprojects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=buildcat.dev,resources=buildprojects/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=buildcat.dev,resources=builds,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=buildcat.dev,resources=builds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims;configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

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

	newPhase := phaseFrom(desired, ready, bp.Status.InflightBuilds)
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
	l.V(1).Info("reconciled", "key", bp.Spec.Key, "phase", bp.Status.Phase, "ready", ready, "desired", desired)
	// While warm (and not hot), requeue so a quiet project scales to zero on time.
	if desired == 1 && bp.Spec.Tier != buildcatv1.TierHot {
		return ctrl.Result{RequeueAfter: idleRecheckInterval(&bp)}, nil
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
	if existing.Spec.Replicas == nil || *existing.Spec.Replicas != desired {
		patch := client.MergeFrom(existing.DeepCopy())
		existing.Spec.Replicas = &desired
		return r.Patch(ctx, &existing, patch)
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

func phaseFrom(desired, ready, inflight int32) string {
	switch {
	case desired == 0:
		return "Idle"
	case ready >= 1:
		return "Warm"
	default:
		return "Scaling"
	}
}

// applyDefaults guards against BuildProjects created without CRD defaulting.
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

// SetupWithManager wires the reconciler and the objects it owns.
func (r *BuildProjectReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&buildcatv1.BuildProject{}).
		Owns(&appsv1.StatefulSet{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
