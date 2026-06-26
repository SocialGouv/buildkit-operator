package controller

import (
	"context"
	"strings"
	"time"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/router"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// cachePVCPrefix is the name prefix of every retained per-project cache PVC (cache-buildkitd-<key>-0,
// the StatefulSet's "cache" volumeClaimTemplate at ordinal 0). Kept in sync with router.CachePVCName.
var cachePVCPrefix = "cache-" + router.DaemonName("")

// PVCSweeper is a leader-only periodic GC for ORPHANED cache PVCs. A project's cache PVC is created by
// the StatefulSet volumeClaimTemplate and RETAINED across scale-to-zero (so the warm cache survives) —
// which also means it is NOT garbage-collected when its BuildProject disappears: a fork deleted
// externally (kubectl delete bp), a canonical project removed, or buildd crashing mid-reap all leave the
// PVC behind, accumulating storage cost. Fork StatefulSets now auto-delete their PVC (retention policy),
// but this sweep is the catch-all backstop that also reclaims historical and canonical orphans: it
// deletes any cache PVC whose BuildProject no longer exists.
type PVCSweeper struct {
	Client    client.Client
	Namespace string
	Interval  time.Duration // sweep cadence (default 15m)
	Grace     time.Duration // skip PVCs younger than this, so a project still being created isn't raced (default 10m)
}

// NeedLeaderElection runs the sweeper only on the leader — it issues deletes, which must not be
// duplicated across HA replicas.
func (s *PVCSweeper) NeedLeaderElection() bool { return true }

func (s *PVCSweeper) interval() time.Duration {
	if s.Interval > 0 {
		return s.Interval
	}
	return 15 * time.Minute
}

func (s *PVCSweeper) grace() time.Duration {
	if s.Grace > 0 {
		return s.Grace
	}
	return 10 * time.Minute
}

// Start implements manager.Runnable: sweep once at startup (clear any backlog), then on the ticker,
// until the manager context is cancelled.
func (s *PVCSweeper) Start(ctx context.Context) error {
	t := time.NewTicker(s.interval())
	defer t.Stop()
	s.sweep(ctx)
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

func (s *PVCSweeper) sweep(ctx context.Context) {
	l := log.FromContext(ctx).WithName("pvc-sweeper")

	var bps bkov1.BuildProjectList
	if err := s.Client.List(ctx, &bps, client.InNamespace(s.Namespace)); err != nil {
		l.V(1).Info("list buildprojects failed — skipping sweep", "err", err.Error())
		return
	}
	// The set of cache PVCs that SHOULD exist: one per live BuildProject (keyed off Spec.Key, the
	// authoritative routing identity — not metadata.Name).
	live := make(map[string]struct{}, len(bps.Items))
	for i := range bps.Items {
		live[router.CachePVCName(bps.Items[i].Spec.Key)] = struct{}{}
	}

	var pvcs corev1.PersistentVolumeClaimList
	if err := s.Client.List(ctx, &pvcs, client.InNamespace(s.Namespace)); err != nil {
		l.V(1).Info("list pvcs failed — skipping sweep", "err", err.Error())
		return
	}
	grace := s.grace()
	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		if !strings.HasPrefix(pvc.Name, cachePVCPrefix) {
			continue // not a per-project cache PVC
		}
		if _, ok := live[pvc.Name]; ok {
			continue // its BuildProject still exists — keep the (possibly warm) cache
		}
		if pvc.DeletionTimestamp != nil {
			continue // already terminating
		}
		if time.Since(pvc.CreationTimestamp.Time) < grace {
			continue // too young — don't race a project still being provisioned
		}
		if err := s.Client.Delete(ctx, pvc); err != nil && !apierrors.IsNotFound(err) {
			l.Info("orphan cache PVC delete failed", "pvc", pvc.Name, "err", err.Error())
			continue
		}
		l.Info("reaped orphan cache PVC (no BuildProject)", "pvc", pvc.Name)
	}
}
