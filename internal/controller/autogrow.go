package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sync"
	"time"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/router"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// Bounded cache-volume auto-grow: the per-project observation loop that closes
// the last manual-tuning gap. buildkitd GC starts reclaiming layers past 85% of
// the volume, so a project whose working set outgrows its PVC silently thrashes
// its own cache. Instead of an operator watching `df` and patching by hand, the
// reconciler polls the companion's /usage (statfs of the cache filesystem) on
// warm daemons and, past the threshold, grows the PVC by a bounded step — the
// AutoGrowMaxGi cap is the cost quota: growth is automatic, unbounded growth is
// not. Shrink never happens (Cinder can't, and a big cache is the point).
//
// The filesystem resize applies when the kubelet remounts the volume; when the
// PVC reports FileSystemResizePending and the daemon has no build in flight,
// the reconciler bounces the pod so the new capacity lands without waiting for
// the next natural scale-to-zero cycle.

// UsageProber fetches the cache-filesystem usage from a daemon pod's companion
// (/usage on the health port). Injected on the reconciler so tests fake it.
type UsageProber func(ctx context.Context, podIP string) (bytesUsed, bytesTotal uint64, err error)

// HTTPUsageProber probes the companion's /usage endpoint on healthPort.
// Daemon ingress is not network-restricted (the egress-lockdown NetworkPolicy
// leaves ingress to mTLS on the buildkitd port), so a plain GET from buildd
// works; the companion serves read-only statfs numbers.
func HTTPUsageProber(healthPort int32) UsageProber {
	hc := &http.Client{Timeout: 3 * time.Second}
	return func(ctx context.Context, podIP string) (uint64, uint64, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://%s:%d/usage", podIP, healthPort), nil)
		if err != nil {
			return 0, 0, err
		}
		resp, err := hc.Do(req)
		if err != nil {
			return 0, 0, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return 0, 0, fmt.Errorf("companion /usage: HTTP %d", resp.StatusCode)
		}
		var u struct {
			BytesUsed  uint64 `json:"bytesUsed"`
			BytesTotal uint64 `json:"bytesTotal"`
		}
		// LimitReader: the endpoint is unauthenticated pod HTTP inside the builds
		// namespace — a compromised daemon must not be able to feed the elected
		// leader an unbounded body. The real payload is <100 bytes.
		if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&u); err != nil {
			return 0, 0, fmt.Errorf("companion /usage: %w", err)
		}
		return u.BytesUsed, u.BytesTotal, nil
	}
}

const (
	// autoGrowProbeInterval rate-limits per-project usage probes: volume
	// pressure moves on build timescales, not reconcile timescales.
	autoGrowProbeInterval = 10 * time.Minute
	// autoGrowBounceInterval rate-limits the resize-applying pod bounce. If the
	// filesystem resize never completes (broken CSI, node-side resize failure),
	// an ungated bounce would kill the daemon on every reconcile forever.
	autoGrowBounceInterval = 15 * time.Minute
)

// probeGate tracks the last action time per project key (in-memory: reconciles
// run on the single elected leader; a restart just acts once more, harmless).
type probeGate struct {
	mu   sync.Mutex
	last map[string]time.Time
}

// allow reports whether the action is due again for key and records it if so.
func (g *probeGate) allow(key string, now time.Time, interval time.Duration) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.last == nil {
		g.last = map[string]time.Time{}
	}
	if t, ok := g.last[key]; ok && now.Sub(t) < interval {
		return false
	}
	g.last[key] = now
	return true
}

// autoGrowTarget is the pure grow decision: the new size in Gi, or 0 for "no
// change". Grows by factor (ceil), clamped to maxGi; only past thresholdPct.
func autoGrowTarget(currentGi int32, usedPct float64, thresholdPct int, factor float64, maxGi int32) int32 {
	if thresholdPct <= 0 || maxGi <= 0 || currentGi <= 0 || currentGi >= maxGi {
		return 0
	}
	if usedPct < float64(thresholdPct) {
		return 0
	}
	if factor <= 1 {
		return 0
	}
	// Compare in float before the int32 cast so an outsized factor saturates at
	// maxGi instead of overflowing into a negative (silently-inert) value.
	nf := math.Ceil(float64(currentGi) * factor)
	next := maxGi
	if nf < float64(maxGi) {
		next = int32(nf)
	}
	if next <= currentGi {
		return 0
	}
	return next
}

// maybeAutoGrow runs the bounded auto-grow cycle for a ready canonical daemon:
// finish a pending filesystem resize when the daemon is idle, then (rate-
// limited) probe usage and grow the PVC + spec when past the threshold.
// Best-effort by design — any failure is logged and retried on a later
// reconcile; the daemon keeps building either way.
func (r *BuildProjectReconciler) maybeAutoGrow(ctx context.Context, bp *bkov1.BuildProject, ready int32) error {
	if r.AutoGrowThresholdPct <= 0 || r.ProbeUsage == nil || router.IsForkKey(bp.Spec.Key) {
		return nil
	}
	l := log.FromContext(ctx)

	var pvc corev1.PersistentVolumeClaim
	if err := r.Get(ctx, types.NamespacedName{Name: router.CachePVCName(bp.Spec.Key), Namespace: r.Cfg.Namespace}, &pvc); err != nil {
		return client.IgnoreNotFound(err)
	}

	// A granted-but-unapplied expansion needs a remount: bounce the pod once
	// the daemon is idle so the capacity lands now instead of at the next
	// natural restart. (StatefulSet volumeClaimTemplates are immutable, so the
	// live PVC is the only object that carries the real size.) Gated on its own
	// interval — if the node-side resize keeps failing, an ungated bounce would
	// kill the daemon on every reconcile — and re-checked against a FRESH
	// (uncached) read of inflight, so a build routed a beat ago (informer lag)
	// isn't killed mid-connect.
	if resizePending(&pvc) && bp.Status.InflightCount() == 0 && ready >= 1 {
		if !r.bounceGate.allow(bp.Spec.Key, time.Now(), autoGrowBounceInterval) {
			return nil
		}
		if fresh, err := r.freshInflight(ctx, bp); err != nil || fresh > 0 {
			return err
		}
		var pod corev1.Pod
		podName := router.DaemonName(bp.Spec.Key) + "-0"
		if err := r.Get(ctx, types.NamespacedName{Name: podName, Namespace: r.Cfg.Namespace}, &pod); err == nil {
			l.Info("auto-grow: bouncing idle daemon to apply pending filesystem resize", "key", bp.Spec.Key, "pvc", pvc.Name)
			if err := r.Delete(ctx, &pod); err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		}
		return nil
	}

	// Sizing truth is the PVC, not the spec: a hand-grown PVC (the pre-feature
	// practice) must be adopted, never "shrunk back"; and a request the storage
	// backend hasn't fulfilled yet (capacity < request: controller-side
	// expansion still in flight, e.g. a Cinder quota stall) must not compound
	// into further growth — the statfs still measures the OLD filesystem.
	reqGi := giOf(pvc.Spec.Resources.Requests[corev1.ResourceStorage])
	capGi := giOf(pvc.Status.Capacity[corev1.ResourceStorage])
	if capGi > 0 && reqGi > capGi {
		return nil // expansion in flight — wait for it to land before re-deciding
	}
	currentGi := bp.Spec.CacheVolumeGi
	if reqGi > currentGi {
		currentGi = reqGi
	}

	if ready < 1 || !r.autoGrowGate.allow(bp.Spec.Key, time.Now(), autoGrowProbeInterval) {
		return nil
	}
	var pod corev1.Pod
	if err := r.Get(ctx, types.NamespacedName{Name: router.DaemonName(bp.Spec.Key) + "-0", Namespace: r.Cfg.Namespace}, &pod); err != nil {
		return client.IgnoreNotFound(err)
	}
	if pod.Status.PodIP == "" {
		return nil
	}
	used, total, err := r.ProbeUsage(ctx, pod.Status.PodIP)
	if err != nil || total == 0 {
		l.V(1).Info("auto-grow: usage probe failed", "key", bp.Spec.Key, "err", err)
		return nil
	}
	usedPct := float64(used) / float64(total) * 100

	next := autoGrowTarget(currentGi, usedPct, r.AutoGrowThresholdPct, r.autoGrowFactor(), int32(r.AutoGrowMaxGi))
	if next == 0 {
		// Even without growth, adopt a hand-grown PVC into the spec so a future
		// daemon re-creation provisions what the project actually runs on.
		if reqGi > bp.Spec.CacheVolumeGi {
			return r.recordSpecGi(ctx, bp, reqGi)
		}
		return nil
	}
	if next <= reqGi {
		return nil // never write the PVC below (or equal to) its current request
	}

	l.Info("auto-grow: cache volume past threshold, growing",
		"key", bp.Spec.Key, "usedPct", fmt.Sprintf("%.1f", usedPct),
		"fromGi", currentGi, "toGi", next, "maxGi", r.AutoGrowMaxGi)

	// PVC first (the object that matters), then the spec (so a future daemon
	// re-creation provisions the grown size). Both conflict-retried.
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var cur corev1.PersistentVolumeClaim
		if err := r.Get(ctx, types.NamespacedName{Name: pvc.Name, Namespace: pvc.Namespace}, &cur); err != nil {
			return err
		}
		if cur.Spec.Resources.Requests == nil {
			cur.Spec.Resources.Requests = corev1.ResourceList{}
		}
		if giOf(cur.Spec.Resources.Requests[corev1.ResourceStorage]) >= next {
			return nil // raced with another grow — never shrink
		}
		cur.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse(fmt.Sprintf("%dGi", next))
		return r.Update(ctx, &cur)
	}); err != nil {
		return fmt.Errorf("grow pvc %s: %w", pvc.Name, err)
	}
	return r.recordSpecGi(ctx, bp, next)
}

// recordSpecGi conflict-retries CacheVolumeGi onto the BuildProject spec.
func (r *BuildProjectReconciler) recordSpecGi(ctx context.Context, bp *bkov1.BuildProject, gi int32) error {
	if err := retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		var cur bkov1.BuildProject
		if err := r.Get(ctx, types.NamespacedName{Name: bp.Name, Namespace: bp.Namespace}, &cur); err != nil {
			return err
		}
		if cur.Spec.CacheVolumeGi >= gi {
			return nil
		}
		cur.Spec.CacheVolumeGi = gi
		return r.Update(ctx, &cur)
	}); err != nil {
		return fmt.Errorf("record grown size on %s: %w", bp.Name, err)
	}
	return nil
}

// freshInflight counts the LIVE unreleased builds bypassing the informer cache (Direct when
// wired, else the cached client) — a decision that can kill a running build (the auto-grow bounce,
// the scale-to-zero) must not act on a snapshot that predates a just-routed build. Expired entries
// are excluded, so a leak that the reconciler has not swept yet cannot block those decisions forever.
func (r *BuildProjectReconciler) freshInflight(ctx context.Context, bp *bkov1.BuildProject) (int, error) {
	live, err := r.freshInflightEntries(ctx, bp)
	return len(live), err
}

// freshInflightEntries is freshInflight's underlying read: the live entries straight from the API
// server. The routing API writes them from any buildd replica while the reconciler runs on the
// leader, so this lag is cross-process — a cached read can miss a build routed moments ago.
func (r *BuildProjectReconciler) freshInflightEntries(ctx context.Context, bp *bkov1.BuildProject) ([]bkov1.InflightBuild, error) {
	reader := r.Direct
	if reader == nil {
		reader = r.Client
	}
	var cur bkov1.BuildProject
	if err := reader.Get(ctx, types.NamespacedName{Name: bp.Name, Namespace: bp.Namespace}, &cur); err != nil {
		return nil, client.IgnoreNotFound(err)
	}
	bkov1.AdoptLegacyInflight(&cur.Status)
	live, _ := bkov1.ExpireInflight(cur.Status.Inflight, time.Now(), r.maxBuildAge())
	return live, nil
}

// giOf converts a storage quantity to whole Gi (floor; zero-quantity => 0).
func giOf(q resource.Quantity) int32 {
	return int32(q.Value() >> 30)
}

// resizePending reports whether the PVC has a granted expansion waiting for a
// filesystem resize (kubelet applies it at the next mount).
func resizePending(pvc *corev1.PersistentVolumeClaim) bool {
	for _, c := range pvc.Status.Conditions {
		if c.Type == corev1.PersistentVolumeClaimFileSystemResizePending && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// autoGrowFactor defaults the growth step (1.5×) when unset.
func (r *BuildProjectReconciler) autoGrowFactor() float64 {
	if r.AutoGrowFactor <= 1 {
		return 1.5
	}
	return r.AutoGrowFactor
}
