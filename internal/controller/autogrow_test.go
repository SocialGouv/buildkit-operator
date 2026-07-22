package controller

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/router"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// The pure grow decision: threshold-gated, factor-stepped, quota-capped.
func TestAutoGrowTarget(t *testing.T) {
	cases := []struct {
		name      string
		currentGi int32
		usedPct   float64
		threshold int
		factor    float64
		maxGi     int32
		want      int32
	}{
		{"below threshold -> no change", 60, 79.9, 80, 1.5, 240, 0},
		{"past threshold -> grows by factor", 60, 85, 80, 1.5, 240, 90},
		{"capped at maxGi", 200, 90, 80, 1.5, 240, 240},
		{"already at cap -> no change", 240, 95, 80, 1.5, 240, 0},
		{"disabled threshold", 60, 95, 0, 1.5, 240, 0},
		{"disabled quota", 60, 95, 80, 1.5, 0, 0},
		{"degenerate factor", 60, 95, 80, 1.0, 240, 0},
	}
	for _, c := range cases {
		if got := autoGrowTarget(c.currentGi, c.usedPct, c.threshold, c.factor, c.maxGi); got != c.want {
			t.Errorf("%s: autoGrowTarget = %d, want %d", c.name, got, c.want)
		}
	}
}

// The real prober against a companion-shaped /usage endpoint, including the
// two failure modes the reconcile path must swallow (non-200, bad JSON).
func TestHTTPUsageProber(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/usage":
			fmt.Fprintln(w, `{"bytesUsed":55,"bytesTotal":64,"inodeRatio":0.1}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	host, portStr, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	probe := HTTPUsageProber(int32(port))
	used, total, err := probe(t.Context(), host)
	if err != nil || used != 55 || total != 64 {
		t.Fatalf("probe = (%d, %d, %v), want (55, 64, nil)", used, total, err)
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer bad.Close()
	host, portStr, _ = net.SplitHostPort(bad.Listener.Addr().String())
	port, _ = strconv.Atoi(portStr)
	if _, _, err := HTTPUsageProber(int32(port))(t.Context(), host); err == nil {
		t.Error("non-200 must error")
	}

	junk := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintln(w, "not json")
	}))
	defer junk.Close()
	host, portStr, _ = net.SplitHostPort(junk.Listener.Addr().String())
	port, _ = strconv.Atoi(portStr)
	if _, _, err := HTTPUsageProber(int32(port))(t.Context(), host); err == nil {
		t.Error("bad JSON must error")
	}
}

func autoGrowFixture(t *testing.T, key string, gi int32, podIP string) (*BuildProjectReconciler, *bkov1.BuildProject) {
	t.Helper()
	ns := "buildkit-operator"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Repo: "github.com/o/r", Arch: "amd64", CacheVolumeGi: gi},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: router.CachePVCName(key), Namespace: ns},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("60Gi")},
			},
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: router.DaemonName(key) + "-0", Namespace: ns},
		Status:     corev1.PodStatus{PodIP: podIP},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp, pvc, pod).Build()
	r := &BuildProjectReconciler{
		Client: c, Cfg: builder.Config{Namespace: ns},
		AutoGrowThresholdPct: 80, AutoGrowFactor: 1.5, AutoGrowMaxGi: 240,
	}
	return r, bp
}

// Past the threshold, the PVC request AND the spec are grown; the probe is
// rate-limited so the next reconcile doesn't re-probe immediately.
func TestMaybeAutoGrow_GrowsPastThreshold(t *testing.T) {
	r, bp := autoGrowFixture(t, "pgrow", 60, "10.0.0.9")
	probes := 0
	r.ProbeUsage = func(_ context.Context, ip string) (uint64, uint64, error) {
		probes++
		if ip != "10.0.0.9" {
			t.Errorf("probe hit %q, want the pod IP", ip)
		}
		return 55, 64, nil // ~86%
	}

	if err := r.maybeAutoGrow(t.Context(), bp, 1); err != nil {
		t.Fatal(err)
	}
	var pvc corev1.PersistentVolumeClaim
	_ = r.Get(t.Context(), types.NamespacedName{Name: router.CachePVCName("pgrow"), Namespace: r.Cfg.Namespace}, &pvc)
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "90Gi" {
		t.Errorf("PVC request = %s, want 90Gi", got.String())
	}
	var got bkov1.BuildProject
	_ = r.Get(t.Context(), types.NamespacedName{Name: "pgrow", Namespace: r.Cfg.Namespace}, &got)
	if got.Spec.CacheVolumeGi != 90 {
		t.Errorf("spec.CacheVolumeGi = %d, want 90", got.Spec.CacheVolumeGi)
	}

	// Second pass inside the probe interval: rate-limited, no second probe.
	if err := r.maybeAutoGrow(t.Context(), bp, 1); err != nil {
		t.Fatal(err)
	}
	if probes != 1 {
		t.Errorf("probes = %d, want 1 (rate-limited)", probes)
	}
}

// Below the threshold nothing moves; forks and disabled configs are inert.
func TestMaybeAutoGrow_Inert(t *testing.T) {
	r, bp := autoGrowFixture(t, "pquiet", 60, "10.0.0.9")
	r.ProbeUsage = func(context.Context, string) (uint64, uint64, error) { return 10, 64, nil }
	if err := r.maybeAutoGrow(t.Context(), bp, 1); err != nil {
		t.Fatal(err)
	}
	var got bkov1.BuildProject
	_ = r.Get(t.Context(), types.NamespacedName{Name: "pquiet", Namespace: r.Cfg.Namespace}, &got)
	if got.Spec.CacheVolumeGi != 60 {
		t.Errorf("below threshold: spec grew to %d", got.Spec.CacheVolumeGi)
	}

	fork, forkBP := autoGrowFixture(t, "forkpx", 60, "10.0.0.9")
	fork.ProbeUsage = func(context.Context, string) (uint64, uint64, error) {
		t.Error("fork must never be probed")
		return 0, 0, nil
	}
	if err := fork.maybeAutoGrow(t.Context(), forkBP, 1); err != nil {
		t.Fatal(err)
	}

	off, offBP := autoGrowFixture(t, "poff", 60, "10.0.0.9")
	off.AutoGrowThresholdPct = 0
	off.ProbeUsage = func(context.Context, string) (uint64, uint64, error) {
		t.Error("disabled auto-grow must never probe")
		return 0, 0, nil
	}
	if err := off.maybeAutoGrow(t.Context(), offBP, 1); err != nil {
		t.Fatal(err)
	}
}

// A granted-but-pending filesystem resize bounces the pod once the daemon is
// idle (no inflight), and only then.
func TestMaybeAutoGrow_BouncesIdleOnResizePending(t *testing.T) {
	r, bp := autoGrowFixture(t, "presize", 90, "10.0.0.9")
	r.ProbeUsage = func(context.Context, string) (uint64, uint64, error) { return 0, 0, nil }
	var pvc corev1.PersistentVolumeClaim
	_ = r.Get(t.Context(), types.NamespacedName{Name: router.CachePVCName("presize"), Namespace: r.Cfg.Namespace}, &pvc)
	pvc.Status.Conditions = []corev1.PersistentVolumeClaimCondition{{
		Type: corev1.PersistentVolumeClaimFileSystemResizePending, Status: corev1.ConditionTrue,
		LastTransitionTime: metav1.NewTime(time.Now()),
	}}
	if err := r.Status().Update(t.Context(), &pvc); err != nil {
		t.Fatal(err)
	}

	// In-flight build: no bounce.
	bp.Status.InflightBuilds = 1
	if err := r.maybeAutoGrow(t.Context(), bp, 1); err != nil {
		t.Fatal(err)
	}
	var pod corev1.Pod
	if err := r.Get(t.Context(), types.NamespacedName{Name: router.DaemonName("presize") + "-0", Namespace: r.Cfg.Namespace}, &pod); err != nil {
		t.Fatalf("pod must survive while a build is in flight: %v", err)
	}

	// Idle: bounce.
	bp.Status.InflightBuilds = 0
	if err := r.maybeAutoGrow(t.Context(), bp, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), types.NamespacedName{Name: router.DaemonName("presize") + "-0", Namespace: r.Cfg.Namespace}, &pod); err == nil {
		t.Error("pod must be deleted to apply the pending resize")
	}

	// Recreate the pod (as the StatefulSet would): the bounce gate must refuse
	// a second kill inside its interval — a never-completing resize (broken
	// CSI) must not murder the daemon on every reconcile.
	pod = corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: router.DaemonName("presize") + "-0", Namespace: r.Cfg.Namespace}}
	if err := r.Create(t.Context(), &pod); err != nil {
		t.Fatal(err)
	}
	if err := r.maybeAutoGrow(t.Context(), bp, 1); err != nil {
		t.Fatal(err)
	}
	if err := r.Get(t.Context(), types.NamespacedName{Name: router.DaemonName("presize") + "-0", Namespace: r.Cfg.Namespace}, &pod); err != nil {
		t.Errorf("second bounce inside the gate interval must be refused: %v", err)
	}
}

// While a controller-side expansion is still unfulfilled (request > capacity),
// the probe path must not compound the growth: statfs still measures the OLD
// filesystem and re-growing from the already-raised spec jumps straight to the
// quota.
func TestMaybeAutoGrow_NoCompoundWhileExpansionInFlight(t *testing.T) {
	r, bp := autoGrowFixture(t, "pinflight", 90, "10.0.0.9")
	r.ProbeUsage = func(context.Context, string) (uint64, uint64, error) {
		t.Error("must not probe while an expansion is in flight")
		return 0, 0, nil
	}
	var pvc corev1.PersistentVolumeClaim
	_ = r.Get(t.Context(), types.NamespacedName{Name: router.CachePVCName("pinflight"), Namespace: r.Cfg.Namespace}, &pvc)
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("90Gi")
	if err := r.Update(t.Context(), &pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("60Gi")}
	if err := r.Status().Update(t.Context(), &pvc); err != nil {
		t.Fatal(err)
	}

	if err := r.maybeAutoGrow(t.Context(), bp, 1); err != nil {
		t.Fatal(err)
	}
	var got bkov1.BuildProject
	_ = r.Get(t.Context(), types.NamespacedName{Name: "pinflight", Namespace: r.Cfg.Namespace}, &got)
	if got.Spec.CacheVolumeGi != 90 {
		t.Errorf("spec compounded to %d while expansion in flight", got.Spec.CacheVolumeGi)
	}
}

// A hand-grown PVC (pre-feature practice) is the sizing truth: the decision
// bases on it (no shrink attempt), and the spec adopts it.
func TestMaybeAutoGrow_AdoptsHandGrownPVC(t *testing.T) {
	r, bp := autoGrowFixture(t, "padopt", 60, "10.0.0.9")
	r.ProbeUsage = func(context.Context, string) (uint64, uint64, error) { return 10, 300, nil } // 3.3%
	var pvc corev1.PersistentVolumeClaim
	_ = r.Get(t.Context(), types.NamespacedName{Name: router.CachePVCName("padopt"), Namespace: r.Cfg.Namespace}, &pvc)
	pvc.Spec.Resources.Requests[corev1.ResourceStorage] = resource.MustParse("300Gi")
	if err := r.Update(t.Context(), &pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Capacity = corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("300Gi")}
	if err := r.Status().Update(t.Context(), &pvc); err != nil {
		t.Fatal(err)
	}

	if err := r.maybeAutoGrow(t.Context(), bp, 1); err != nil {
		t.Fatal(err)
	}
	_ = r.Get(t.Context(), types.NamespacedName{Name: router.CachePVCName("padopt"), Namespace: r.Cfg.Namespace}, &pvc)
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.String() != "300Gi" {
		t.Errorf("PVC request = %s — a hand-grown PVC must never be shrunk", got.String())
	}
	var got bkov1.BuildProject
	_ = r.Get(t.Context(), types.NamespacedName{Name: "padopt", Namespace: r.Cfg.Namespace}, &got)
	if got.Spec.CacheVolumeGi != 300 {
		t.Errorf("spec.CacheVolumeGi = %d, want 300 (adopted from the live PVC)", got.Spec.CacheVolumeGi)
	}
}
