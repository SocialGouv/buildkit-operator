package controller

import (
	"context"
	"testing"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/router"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := bkov1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := volumesnapshotv1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// Reconcile must turn a BuildProject into a StatefulSet-of-1 (gen2 PVC template) +
// Service, both owned by the BuildProject, and publish the mTLS endpoint in status.
func TestReconcile_CreatesDaemon(t *testing.T) {
	s := testScheme(t)
	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	ns := "buildkit-operator"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierHot, StorageClass: "ebs-gp3"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{
		Client: c, Scheme: s,
		Cfg: builder.Config{Namespace: ns, BuildkitImage: "img", CompanionImage: "comp", DaemonCertsSecret: "certs", BuildkitdConfigMap: "cfg", Port: 1234, HealthPort: 8080},
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}, &sts); err != nil {
		t.Fatalf("statefulset not created: %v", err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		got := int32(-1)
		if sts.Spec.Replicas != nil {
			got = *sts.Spec.Replicas
		}
		t.Errorf("replicas = %d, want 1 (hot tier)", got)
	}
	if n := len(sts.Spec.VolumeClaimTemplates); n != 1 {
		t.Fatalf("volumeClaimTemplates = %d, want 1 (the cache PVC)", n)
	}
	if sc := sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName; sc == nil || *sc != "ebs-gp3" {
		t.Errorf("cache PVC storageClass = %v, want ebs-gp3 (from spec)", sc)
	}
	if len(sts.OwnerReferences) == 0 || sts.OwnerReferences[0].Name != key {
		t.Errorf("statefulset not owned by BuildProject")
	}

	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}, &svc); err != nil {
		t.Fatalf("service not created: %v", err)
	}

	var got bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: key, Namespace: ns}, &got); err != nil {
		t.Fatal(err)
	}
	if want := router.Endpoint(key, ns, 1234); got.Status.Endpoint != want {
		t.Errorf("status.endpoint = %q, want %q", got.Status.Endpoint, want)
	}
	// No Ready replica in the fake => not yet Warm.
	if got.Status.Phase == "Warm" {
		t.Errorf("phase = Warm without a ready replica")
	}
}

// A second reconcile must be idempotent (no error, still one STS).
func TestReconcile_Idempotent(t *testing.T) {
	s := testScheme(t)
	key := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	ns := "buildkit-operator"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64"},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}
	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
}

// A change to the rendered daemon spec (here the buildkit image) must roll the EXISTING daemon: the
// reconciler converges the pod template, not just replicas. (Hash-gated, so unchanged reconciles
// don't churn — the idempotent test above covers that side.)
func TestReconcile_RollsTemplateOnChange(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "rolltest"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierHot},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, BuildkitImage: "buildkit:v1", Port: 1234, HealthPort: 8080}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}

	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	r.Cfg.BuildkitImage = "buildkit:v2" // a buildkit-image bump, e.g. a chart upgrade
	if _, err := r.Reconcile(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}, &sts); err != nil {
		t.Fatal(err)
	}
	if got := sts.Spec.Template.Spec.Containers[0].Image; got != "buildkit:v2" {
		t.Errorf("daemon image = %q, want buildkit:v2 (template must converge on the existing STS)", got)
	}
}

// M2 elasticity: the scale decision must honor tier + idle window + in-flight.
func TestDesiredReplicas(t *testing.T) {
	now := time.Now()
	mk := func(tier string, idleSec int32, ago time.Duration, hasBuilt bool, inflight int32) *bkov1.BuildProject {
		bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Tier: tier, IdleTimeoutSec: idleSec}}
		bp.Status.InflightBuilds = inflight
		if hasBuilt {
			ts := metav1.NewTime(now.Add(-ago))
			bp.Status.LastBuildTime = &ts
		}
		return bp
	}
	const maxBuild = 2 * time.Hour
	cases := []struct {
		name string
		bp   *bkov1.BuildProject
		want int32
	}{
		{"hot always on", mk(bkov1.TierHot, 0, 0, false, 0), 1},
		{"warm recent build", mk(bkov1.TierWarm, 900, time.Minute, true, 0), 1},
		{"warm idle -> zero", mk(bkov1.TierWarm, 900, time.Hour, true, 0), 0},
		{"warm never built -> zero", mk(bkov1.TierWarm, 900, 0, false, 0), 0},
		// In-flight keeps it warm even past the idle window — until the inflight counter goes stale
		// (a client that missed its /complete can't pin a daemon forever).
		{"warm in-flight (fresh) -> one", mk(bkov1.TierWarm, 900, time.Hour, true, 2), 1},
		{"warm in-flight (stale) -> zero", mk(bkov1.TierWarm, 900, 3*time.Hour, true, 2), 0},
	}
	for _, c := range cases {
		if got := desiredReplicas(c.bp, now, maxBuild); got != c.want {
			t.Errorf("%s: desiredReplicas = %d, want %d", c.name, got, c.want)
		}
	}
}

// B4: lowering Fanout (or disabling it) must delete the orphan clone BuildProjects, not leak them.
func TestReconcile_FanoutScalesDown(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "fandown"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierHot, Fanout: 1},
	}
	bp.Status.LastSnapshot = "snap-fandown-1"
	// Pre-existing clone #2 from a previous higher Fanout — must be pruned now that Fanout=1.
	c2key := router.CloneKey(key, 2)
	orphan := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: c2key, Namespace: ns, Labels: map[string]string{cloneOfLabel: key}},
		Spec:       bkov1.BuildProjectSpec{Key: c2key, Arch: "amd64", Tier: bkov1.TierHot},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp, orphan).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var gone bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: c2key, Namespace: ns}, &gone); err == nil {
		t.Errorf("orphan clone %s should have been pruned (Fanout=1)", c2key)
	}
	var kept bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.CloneKey(key, 1), Namespace: ns}, &kept); err != nil {
		t.Errorf("clone #1 should exist (Fanout=1): %v", err)
	}
}

// Q6/B2: an idle ephemeral fork daemon must be reaped — its cache PVC and the fork BuildProject
// deleted — so forks don't accumulate retained PVCs/CRs.
func TestReconcile_ReapsIdleFork(t *testing.T) {
	s := testScheme(t)
	ns := "buildkit-operator"
	canonical := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	fork := router.ForkKey(canonical)
	old := metav1.NewTime(time.Now().Add(-time.Hour)) // idle past the fork window, inflight released
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: fork, Namespace: ns, CreationTimestamp: old}, // past the birth-window grace
		Spec:       bkov1.BuildProjectSpec{Key: fork, Arch: "amd64", Tier: bkov1.TierWarm, IdleTimeoutSec: bkov1.ForkIdleTimeoutSec},
	}
	bp.Status.LastBuildTime = &old
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: router.CachePVCName(fork), Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp, pvc).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: fork, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var goneBP bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: fork, Namespace: ns}, &goneBP); err == nil {
		t.Errorf("idle fork BuildProject should have been reaped")
	}
	var gonePVC corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.CachePVCName(fork), Namespace: ns}, &gonePVC); err == nil {
		t.Errorf("idle fork cache PVC should have been deleted")
	}
}

// Birth-window guard: a freshly-created fork (no LastBuildTime / inflight yet → desired 0) must NOT be
// reaped — buildd stamps it a beat after Create, and reaping immediately would kill the daemon before
// its untrusted build ever registers (the cold-start flake that made untrusted builds hang).
func TestReconcile_DoesNotReapNewbornFork(t *testing.T) {
	s := testScheme(t)
	ns := "buildkit-operator"
	fork := router.ForkKey(router.ProjectKey("github.com/org/repo", "", "", "amd64"))
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: fork, Namespace: ns, CreationTimestamp: metav1.Now()}, // just born
		Spec:       bkov1.BuildProjectSpec{Key: fork, Arch: "amd64", Tier: bkov1.TierWarm, IdleTimeoutSec: bkov1.ForkIdleTimeoutSec},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}

	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: fork, Namespace: ns}})
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > forkReapGrace {
		t.Errorf("RequeueAfter = %v, want a positive value within the grace window", res.RequeueAfter)
	}
	var still bkov1.BuildProject
	if err := c.Get(context.Background(), types.NamespacedName{Name: fork, Namespace: ns}, &still); err != nil {
		t.Errorf("newborn fork should NOT have been reaped: %v", err)
	}
}

// An idle warm project must be scaled to zero by the reconciler (PVC retained).
func TestReconcile_ScalesIdleToZero(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "idle"
	old := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierWarm, IdleTimeoutSec: 900},
	}
	bp.Status.LastBuildTime = &old
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}, &sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 0 {
		t.Errorf("idle warm project: replicas = %v, want 0 (scale-to-zero)", sts.Spec.Replicas)
	}
}

// Regression: a warm project created WITHOUT IdleTimeoutSec (relying on defaulting) and built
// recently must stay warm. The fake client skips apiserver defaulting, so applyDefaults must supply
// the CRD default (900); otherwise IdleTimeoutSec stays 0 and desiredReplicas scales it to zero
// right after every build.
func TestReconcile_DefaultsIdleTimeout(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "nodefault"
	recent := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierWarm}, // IdleTimeoutSec unset (0)
	}
	bp.Status.LastBuildTime = &recent
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	var sts appsv1.StatefulSet
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}, &sts); err != nil {
		t.Fatal(err)
	}
	if sts.Spec.Replicas == nil || *sts.Spec.Replicas != 1 {
		t.Errorf("recently-built warm project with defaulted idle timeout: replicas = %v, want 1 (stayed warm)", sts.Spec.Replicas)
	}
}

// M3 durability: when the cadence is due and the cache PVC exists, the reconciler must
// create a VolumeSnapshot of that PVC (in-use; no scale-to-zero required on OVH).
func TestReconcile_SnapshotsOnCadence(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "snaptest"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierHot, SnapshotEverySec: 60},
	}
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: router.CachePVCName(key), Namespace: ns}}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp, pvc).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080, SnapshotClass: "csi-cinder-snapclass-v1"}}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	var snaps volumesnapshotv1.VolumeSnapshotList
	if err := c.List(context.Background(), &snaps, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(snaps.Items) != 1 {
		t.Fatalf("snapshots = %d, want 1", len(snaps.Items))
	}
	if src := snaps.Items[0].Spec.Source.PersistentVolumeClaimName; src == nil || *src != router.CachePVCName(key) {
		t.Errorf("snapshot source = %v, want %s", src, router.CachePVCName(key))
	}
}

// M5 fan-out: Fanout=N + a snapshot to clone from => N sibling clone BuildProjects, each seeded
// (CoW) from the latest snapshot and not fanning out themselves.
func TestReconcile_FanoutCreatesClones(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "fan"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierHot, Fanout: 2},
	}
	bp.Status.LastSnapshot = "snap-fan-1"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for i := 1; i <= 2; i++ {
		ckey := router.CloneKey(key, i)
		var clone bkov1.BuildProject
		if err := c.Get(context.Background(), types.NamespacedName{Name: ckey, Namespace: ns}, &clone); err != nil {
			t.Fatalf("clone %d (%s) not created: %v", i, ckey, err)
		}
		if clone.Spec.RestoreFromSnapshot != "snap-fan-1" {
			t.Errorf("clone %d restore = %q, want snap-fan-1", i, clone.Spec.RestoreFromSnapshot)
		}
		if clone.Spec.Fanout != 0 {
			t.Errorf("clone %d must not fan out itself", i)
		}
	}
}
