package controller

import (
	"context"
	"testing"
	"time"

	buildcatv1 "github.com/devthejo/buildcat/api/v1alpha1"
	"github.com/devthejo/buildcat/internal/builder"
	"github.com/devthejo/buildcat/internal/router"
	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
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
	if err := buildcatv1.AddToScheme(s); err != nil {
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
	key := router.ProjectKey("github.com/org/repo", "", "amd64")
	ns := "buildcat"
	bp := &buildcatv1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       buildcatv1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: buildcatv1.TierHot},
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
		t.Fatalf("volumeClaimTemplates = %d, want 1 (the gen2 cache PVC)", n)
	}
	if sc := sts.Spec.VolumeClaimTemplates[0].Spec.StorageClassName; sc == nil || *sc != "csi-cinder-high-speed-gen2" {
		t.Errorf("cache PVC storageClass = %v, want gen2 default", sc)
	}
	if len(sts.OwnerReferences) == 0 || sts.OwnerReferences[0].Name != key {
		t.Errorf("statefulset not owned by BuildProject")
	}

	var svc corev1.Service
	if err := c.Get(context.Background(), types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}, &svc); err != nil {
		t.Fatalf("service not created: %v", err)
	}

	var got buildcatv1.BuildProject
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
	key := router.ProjectKey("github.com/org/repo", "", "amd64")
	ns := "buildcat"
	bp := &buildcatv1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       buildcatv1.BuildProjectSpec{Key: key, Arch: "amd64"},
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

// M2 elasticity: the scale decision must honor tier + idle window + in-flight.
func TestDesiredReplicas(t *testing.T) {
	now := time.Now()
	mk := func(tier string, idleSec int32, ago time.Duration, hasBuilt bool, inflight int32) *buildcatv1.BuildProject {
		bp := &buildcatv1.BuildProject{Spec: buildcatv1.BuildProjectSpec{Tier: tier, IdleTimeoutSec: idleSec}}
		bp.Status.InflightBuilds = inflight
		if hasBuilt {
			ts := metav1.NewTime(now.Add(-ago))
			bp.Status.LastBuildTime = &ts
		}
		return bp
	}
	cases := []struct {
		name string
		bp   *buildcatv1.BuildProject
		want int32
	}{
		{"hot always on", mk(buildcatv1.TierHot, 0, 0, false, 0), 1},
		{"warm recent build", mk(buildcatv1.TierWarm, 900, time.Minute, true, 0), 1},
		{"warm idle -> zero", mk(buildcatv1.TierWarm, 900, time.Hour, true, 0), 0},
		{"warm never built -> zero", mk(buildcatv1.TierWarm, 900, 0, false, 0), 0},
		{"warm in-flight -> one", mk(buildcatv1.TierWarm, 900, time.Hour, true, 2), 1},
	}
	for _, c := range cases {
		if got := desiredReplicas(c.bp, now); got != c.want {
			t.Errorf("%s: desiredReplicas = %d, want %d", c.name, got, c.want)
		}
	}
}

// An idle warm project must be scaled to zero by the reconciler (PVC retained).
func TestReconcile_ScalesIdleToZero(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildcat", "idle"
	old := metav1.NewTime(time.Now().Add(-2 * time.Hour))
	bp := &buildcatv1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       buildcatv1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: buildcatv1.TierWarm, IdleTimeoutSec: 900},
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

// M3 durability: when the cadence is due and the cache PVC exists, the reconciler must
// create a VolumeSnapshot of that PVC (in-use; no scale-to-zero required on OVH).
func TestReconcile_SnapshotsOnCadence(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildcat", "snaptest"
	bp := &buildcatv1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       buildcatv1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: buildcatv1.TierHot, SnapshotEverySec: 60},
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
	ns, key := "buildcat", "fan"
	bp := &buildcatv1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       buildcatv1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: buildcatv1.TierHot, Fanout: 2},
	}
	bp.Status.LastSnapshot = "snap-fan-1"
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	for i := 1; i <= 2; i++ {
		ckey := router.CloneKey(key, i)
		var clone buildcatv1.BuildProject
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
