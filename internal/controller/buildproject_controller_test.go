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
	policyv1 "k8s.io/api/policy/v1"
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

// Rolling a daemon replaces its pod, which severs the mTLS stream of every build running on it — the
// client sees "failed to write client preface: EOF" mid-build. So a template change waits for the
// daemon to drain. It must NOT wait on a daemon that is down (the roll may be the very fix), and it
// must land as soon as the last build is released.
func TestReconcile_HoldsRollWhileBuildsAreInFlight(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "drain"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierHot},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, BuildkitImage: "buildkit:v1", Port: 1234, HealthPort: 8080}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}
	stsName := types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}
	daemonImage := func(t *testing.T) string {
		t.Helper()
		var sts appsv1.StatefulSet
		if err := c.Get(t.Context(), stsName, &sts); err != nil {
			t.Fatal(err)
		}
		return sts.Spec.Template.Spec.Containers[0].Image
	}
	setReady := func(t *testing.T, ready int32) {
		t.Helper()
		var sts appsv1.StatefulSet
		if err := c.Get(t.Context(), stsName, &sts); err != nil {
			t.Fatal(err)
		}
		sts.Status.ReadyReplicas = ready
		if err := c.Status().Update(t.Context(), &sts); err != nil {
			t.Fatal(err)
		}
	}
	routeBuild := func(t *testing.T, entries []bkov1.InflightBuild) {
		t.Helper()
		var cur bkov1.BuildProject
		if err := c.Get(t.Context(), req.NamespacedName, &cur); err != nil {
			t.Fatal(err)
		}
		cur.Status.SetInflight(entries)
		if err := c.Status().Update(t.Context(), &cur); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	setReady(t, 1)

	// A build is running on the ready daemon: the image bump must NOT land yet.
	routeBuild(t, []bkov1.InflightBuild{{ID: "b1", Since: metav1.Now()}})
	r.Cfg.BuildkitImage = "buildkit:v2"
	res, err := r.Reconcile(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := daemonImage(t); got != "buildkit:v1" {
		t.Errorf("daemon image = %q while a build is in flight, want the roll held at buildkit:v1", got)
	}
	if res.RequeueAfter == 0 {
		t.Error("a held roll must requeue — an entry can leave by expiring, which writes nothing")
	}

	// The build is released: the pending template lands on the next reconcile.
	routeBuild(t, nil)
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if got := daemonImage(t); got != "buildkit:v2" {
		t.Errorf("daemon image = %q after the daemon drained, want buildkit:v2", got)
	}
}

// A WEDGED daemon (CrashLoopBackOff, bad image…) must not keep a leftover entry from blocking the
// change that repairs it. Merely "not ready" is NOT the signal — a saturated buildkitd can miss its
// readiness probes mid-build, and rolling then would sever the very build the hold protects.
func TestReconcile_RollsWedgedDaemonDespiteInflight(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "drainbroken"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierHot},
	}
	wedged := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: router.DaemonName(key) + "-0", Namespace: ns,
			Labels: map[string]string{projectKeyLabel: key},
		},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name:  "buildkitd",
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}},
		}}},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp, wedged).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, BuildkitImage: "broken:v1", Port: 1234, HealthPort: 8080}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}
	stsName := types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	// The daemon is up as far as replica counting goes, and a build is registered on it.
	var sts appsv1.StatefulSet
	_ = c.Get(t.Context(), stsName, &sts)
	sts.Status.ReadyReplicas = 1
	if err := c.Status().Update(t.Context(), &sts); err != nil {
		t.Fatal(err)
	}
	var cur bkov1.BuildProject
	_ = c.Get(t.Context(), req.NamespacedName, &cur)
	cur.Status.SetInflight([]bkov1.InflightBuild{{ID: "b1", Since: metav1.Now()}})
	if err := c.Status().Update(t.Context(), &cur); err != nil {
		t.Fatal(err)
	}

	r.Cfg.BuildkitImage = "fixed:v2" // the roll IS the repair
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(t.Context(), stsName, &sts)
	if got := sts.Spec.Template.Spec.Containers[0].Image; got != "fixed:v2" {
		t.Errorf("daemon image = %q, want fixed:v2 — a wedged daemon must not block its own repair", got)
	}
}

// The drain guarantees every build at least minBuildGrace before a forced roll can sever it — a
// 45-minute build must survive — but it is still bounded: once even the YOUNGEST build has had its
// hour, the pending template goes through.
func TestReconcile_RollGuaranteesBuildGrace(t *testing.T) {
	s := testScheme(t)
	ns := "buildkit-operator"
	// rollWith drives one project whose single in-flight build started `buildAge` ago, and reports the
	// image the daemon ends up on.
	rollWith := func(t *testing.T, key string, buildAge time.Duration) string {
		t.Helper()
		bp := &bkov1.BuildProject{
			ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
			Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierHot},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
		r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, BuildkitImage: "buildkit:v1", Port: 1234, HealthPort: 8080}}
		req := ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}
		stsName := types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}

		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		var sts appsv1.StatefulSet
		_ = c.Get(t.Context(), stsName, &sts)
		sts.Status.ReadyReplicas = 1
		if err := c.Status().Update(t.Context(), &sts); err != nil {
			t.Fatal(err)
		}
		var cur bkov1.BuildProject
		_ = c.Get(t.Context(), req.NamespacedName, &cur)
		cur.Status.SetInflight([]bkov1.InflightBuild{{ID: "b1", Since: metav1.NewTime(time.Now().Add(-buildAge))}})
		if err := c.Status().Update(t.Context(), &cur); err != nil {
			t.Fatal(err)
		}
		r.Cfg.BuildkitImage = "buildkit:v2"
		if _, err := r.Reconcile(t.Context(), req); err != nil {
			t.Fatal(err)
		}
		_ = c.Get(t.Context(), stsName, &sts)
		return sts.Spec.Template.Spec.Containers[0].Image
	}

	if got := rollWith(t, "grace45", 45*time.Minute); got != "buildkit:v1" {
		t.Errorf("a 45-minute build was severed (image = %q), want the roll held — every build gets %v", got, minBuildGrace)
	}
	if got := rollWith(t, "grace90", 90*time.Minute); got != "buildkit:v2" {
		t.Errorf("image = %q with a build past its grace period, want the roll to go through", got)
	}
}

// The absolute cap: a project building back-to-back keeps its youngest build young forever, so the
// hold would never end on the grace rule alone. Past --max-build-seconds the roll goes anyway.
func TestReconcile_RollsThroughAfterAbsoluteCap(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "drainbound"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierHot},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, BuildkitImage: "buildkit:v1", Port: 1234, HealthPort: 8080}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}
	stsName := types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	var sts appsv1.StatefulSet
	_ = c.Get(t.Context(), stsName, &sts)
	sts.Status.ReadyReplicas = 1
	if err := c.Status().Update(t.Context(), &sts); err != nil {
		t.Fatal(err)
	}
	freshBuild := func(t *testing.T) {
		t.Helper()
		var cur bkov1.BuildProject
		_ = c.Get(t.Context(), req.NamespacedName, &cur)
		cur.Status.SetInflight([]bkov1.InflightBuild{{ID: "b", Since: metav1.Now()}})
		if err := c.Status().Update(t.Context(), &cur); err != nil {
			t.Fatal(err)
		}
	}
	freshBuild(t)

	r.Cfg.BuildkitImage = "buildkit:v2"
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(t.Context(), stsName, &sts)
	if sts.Annotations[rollHeldSinceAnnotation] == "" {
		t.Fatal("the hold must be stamped on the StatefulSet, else its clock restarts every reconcile")
	}
	if got := sts.Spec.Template.Spec.Containers[0].Image; got != "buildkit:v1" {
		t.Fatalf("daemon image = %q, want the roll held at buildkit:v1", got)
	}

	// Backdate the hold past the cap; a brand-new build keeps arriving, as on a busy project.
	sts.Annotations[rollHeldSinceAnnotation] = time.Now().Add(-3 * time.Hour).UTC().Format(time.RFC3339)
	if err := c.Update(t.Context(), &sts); err != nil {
		t.Fatal(err)
	}
	freshBuild(t)
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	_ = c.Get(t.Context(), stsName, &sts)
	if got := sts.Spec.Template.Spec.Containers[0].Image; got != "buildkit:v2" {
		t.Errorf("daemon image = %q past the absolute cap, want buildkit:v2", got)
	}
	if sts.Annotations[rollHeldSinceAnnotation] != "" {
		t.Errorf("the hold stamp must be cleared once the roll lands, got %q", sts.Annotations[rollHeldSinceAnnotation])
	}
}

// M2 elasticity: the scale decision must honor tier + idle window + in-flight.
func TestDesiredReplicas(t *testing.T) {
	now := time.Now()
	// inflightAge registers one build that started that long ago (0 = no inflight build).
	mk := func(tier string, idleSec int32, ago time.Duration, hasBuilt bool, inflightAge time.Duration) *bkov1.BuildProject {
		bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Tier: tier, IdleTimeoutSec: idleSec}}
		if inflightAge > 0 {
			bp.Status.SetInflight([]bkov1.InflightBuild{{ID: "b", Since: metav1.NewTime(now.Add(-inflightAge))}})
		}
		if hasBuilt {
			ts := metav1.NewTime(now.Add(-ago))
			bp.Status.LastBuildTime = &ts
		}
		return bp
	}
	// cadence attaches n build timestamps within the adaptive window.
	cadence := func(bp *bkov1.BuildProject, n int) *bkov1.BuildProject {
		for i := 0; i < n; i++ {
			bp.Status.RecentBuildTimes = append(bp.Status.RecentBuildTimes, metav1.NewTime(now.Add(-time.Duration(i+1)*time.Hour)))
		}
		return bp
	}
	const maxBuild = 2 * time.Hour
	cases := []struct {
		name        string
		bp          *bkov1.BuildProject
		adaptiveMax time.Duration
		want        int32
	}{
		{"hot always on", mk(bkov1.TierHot, 0, 0, false, 0), 0, 1},
		{"warm recent build", mk(bkov1.TierWarm, 900, time.Minute, true, 0), 0, 1},
		{"warm idle -> zero", mk(bkov1.TierWarm, 900, time.Hour, true, 0), 0, 0},
		{"warm never built -> zero", mk(bkov1.TierWarm, 900, 0, false, 0), 0, 0},
		// In-flight keeps it warm past the idle window — until THAT ENTRY is older than
		// max-build-seconds. The entry's own age decides, not the project's last activity: a project
		// that keeps routing builds must still shed the one whose /complete never came.
		{"warm in-flight (fresh) -> one", mk(bkov1.TierWarm, 900, time.Hour, true, time.Minute), 0, 1},
		{"warm in-flight (expired) -> zero", mk(bkov1.TierWarm, 900, 3*time.Hour, true, 3*time.Hour), 0, 0},
		{"warm in-flight expired but project still active -> zero", mk(bkov1.TierWarm, 900, time.Hour, true, 3*time.Hour), 0, 0},
		// Adaptive keep-warm: 6 builds in the window stretch the 900s idle to 5400s, so a build
		// 1h ago still holds the daemon; without adaptivity (max=0) the same project scales to zero.
		{"adaptive frequent -> one", cadence(mk(bkov1.TierWarm, 900, time.Hour, true, 0), 6), 6 * time.Hour, 1},
		{"adaptive frequent but capped -> zero", cadence(mk(bkov1.TierWarm, 900, 2*time.Hour, true, 0), 6), time.Hour, 0},
		{"adaptive single build keeps base idle -> zero", cadence(mk(bkov1.TierWarm, 900, time.Hour, true, 0), 1), 6 * time.Hour, 0},
	}
	for _, c := range cases {
		live, _ := bkov1.ExpireInflight(c.bp.Status.Inflight, now, maxBuild)
		if got := desiredReplicas(c.bp, live, now, c.adaptiveMax); got != c.want {
			t.Errorf("%s: desiredReplicas = %d, want %d", c.name, got, c.want)
		}
	}
}

// The leaked inflight entries (a cancelled CI job kills the runner before its /complete) expire
// PER BUILD once past max-build-seconds — even while the project keeps routing new builds, which is
// exactly when the leak accumulates. A build that started recently survives the sweep.
func TestReconcile_ExpiresLeakedInflightWhileProjectIsActive(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "pstale"
	justNow := metav1.Now()
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Repo: "github.com/o/r", Arch: "amd64"},
		// LastBuildTime is fresh: the project is actively building, so a window keyed on project
		// activity would never fire. The leaked entries are old on their own clock.
		Status: bkov1.BuildProjectStatus{LastBuildTime: &justNow},
	}
	leaked := metav1.NewTime(time.Now().Add(-3 * time.Hour))
	bp.Status.SetInflight([]bkov1.InflightBuild{
		{ID: "leak1", Since: leaked},
		{ID: "leak2", Since: leaked},
		{ID: "running", Since: justNow},
	})
	c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns}}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
		t.Fatal(err)
	}
	var got bkov1.BuildProject
	_ = c.Get(t.Context(), types.NamespacedName{Name: key, Namespace: ns}, &got)
	if got.Status.InflightCount() != 1 || got.Status.Inflight[0].ID != "running" {
		t.Errorf("inflight = %v, want only the still-running build", got.Status.Inflight)
	}
	if got.Status.InflightBuilds != 1 {
		t.Errorf("InflightBuilds projection = %d, want 1 (must track the entries)", got.Status.InflightBuilds)
	}
}

// A status written before inflight became a set carries only the counter. It is ADOPTED into dated
// entries rather than zeroed: an upgrade landing mid-build must not read as "nothing is running" and
// scale the daemon out from under it. The adopted entries then expire on the normal clock, so a
// counter whose last build is older than the window clears on the same reconcile.
func TestReconcile_AdoptsLegacyCounter(t *testing.T) {
	s := testScheme(t)
	ns := "buildkit-operator"
	reconcileWith := func(t *testing.T, key string, last metav1.Time) bkov1.BuildProject {
		t.Helper()
		bp := &bkov1.BuildProject{
			ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
			Spec:       bkov1.BuildProjectSpec{Key: key, Repo: "github.com/o/r", Arch: "amd64", Tier: bkov1.TierWarm},
			Status:     bkov1.BuildProjectStatus{InflightBuilds: 3, LastBuildTime: &last},
		}
		c := fake.NewClientBuilder().WithScheme(s).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
		r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns}}
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}); err != nil {
			t.Fatal(err)
		}
		var got bkov1.BuildProject
		_ = c.Get(t.Context(), types.NamespacedName{Name: key, Namespace: ns}, &got)
		return got
	}

	// Mid-build upgrade: the count becomes entries and the daemon stays up.
	live := reconcileWith(t, "plive", metav1.Now())
	if live.Status.InflightCount() != 3 || live.Status.InflightBuilds != 3 {
		t.Errorf("fresh legacy counter: entries = %v, projection = %d, want 3 adopted", live.Status.Inflight, live.Status.InflightBuilds)
	}
	var sts appsv1.StatefulSet
	// The adopted entries must actually hold the daemon at 1 replica.
	if live.Status.Phase == "Idle" {
		t.Errorf("phase = Idle with adopted inflight entries, want the daemon held up (sts=%v)", sts.Spec.Replicas)
	}

	// A count whose last build predates the safety-net window is adopted and expired in one pass.
	old := reconcileWith(t, "pstaleold", metav1.NewTime(time.Now().Add(-3*time.Hour)))
	if old.Status.InflightCount() != 0 || old.Status.InflightBuilds != 0 {
		t.Errorf("stale legacy counter: entries = %v, projection = %d, want both empty", old.Status.Inflight, old.Status.InflightBuilds)
	}
}

// Adaptive keep-warm: the effective idle is base × builds-in-window, capped, floored at base,
// and inert for hot/fork/disabled cases.
func TestEffectiveIdle(t *testing.T) {
	now := time.Now()
	mk := func(idleSec int32, key string, builds int) *bkov1.BuildProject {
		bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Key: key, IdleTimeoutSec: idleSec}}
		for i := 0; i < builds; i++ {
			bp.Status.RecentBuildTimes = append(bp.Status.RecentBuildTimes, metav1.NewTime(now.Add(-time.Duration(i+1)*time.Minute)))
		}
		return bp
	}
	const max = 6 * time.Hour
	cases := []struct {
		name string
		bp   *bkov1.BuildProject
		max  time.Duration
		want time.Duration
	}{
		{"disabled -> base", mk(900, "p1", 8), 0, 900 * time.Second},
		{"quiet -> base", mk(900, "p1", 0), max, 900 * time.Second},
		{"single build -> base", mk(900, "p1", 1), max, 900 * time.Second},
		{"scales with cadence", mk(900, "p1", 4), max, 3600 * time.Second},
		{"capped", mk(900, "p1", 30), max, max},
		{"fork excluded", mk(900, "forkp1", 8), max, 900 * time.Second},
		{"base above max -> base", mk(30000, "p1", 8), 4 * time.Hour, 30000 * time.Second},
	}
	for _, c := range cases {
		if got := effectiveIdle(c.bp, now, c.max); got != c.want {
			t.Errorf("%s: effectiveIdle = %v, want %v", c.name, got, c.want)
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

// Lowering Fanout deletes the surplus clones, and that cascade takes their StatefulSets — and any
// build running on them — with it. A clone still serving builds is left for a later reconcile.
func TestReconcileFanout_KeepsBusyClone(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "pfan"
	parent := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Fanout: 0},
		Status:     bkov1.BuildProjectStatus{LastSnapshot: "snap-1"},
	}
	mkClone := func(name string, inflight []bkov1.InflightBuild) *bkov1.BuildProject {
		c := &bkov1.BuildProject{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{cloneOfLabel: key}},
			Spec:       bkov1.BuildProjectSpec{Key: name, Arch: "amd64"},
		}
		c.Status.SetInflight(inflight)
		return c
	}
	busy := mkClone(key+"-c1", []bkov1.InflightBuild{{ID: "b", Since: metav1.Now()}})
	idle := mkClone(key+"-c2", nil)
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(parent, busy, idle).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234}}

	deferred, err := r.reconcileFanout(t.Context(), parent)
	if err != nil {
		t.Fatal(err)
	}
	if !deferred {
		t.Error("keeping a busy clone must report a deferred prune, else nothing comes back to it")
	}
	var got bkov1.BuildProject
	if err := c.Get(t.Context(), types.NamespacedName{Name: busy.Name, Namespace: ns}, &got); err != nil {
		t.Errorf("the clone serving a build was deleted: %v", err)
	}
	if err := c.Get(t.Context(), types.NamespacedName{Name: idle.Name, Namespace: ns}, &got); err == nil {
		t.Error("the idle surplus clone should have been pruned")
	}
}

// The daemon's PodDisruptionBudget follows its in-flight set: an eviction is refused while builds run
// and permitted once the daemon is idle, so a node drain waits for the builds and no longer.
func TestReconcile_PDBTracksInflight(t *testing.T) {
	s := testScheme(t)
	ns, key := "buildkit-operator", "ppdb"
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: key, Namespace: ns},
		Spec:       bkov1.BuildProjectSpec{Key: key, Arch: "amd64", Tier: bkov1.TierWarm},
	}
	c := fake.NewClientBuilder().WithScheme(s).WithObjects(bp).WithStatusSubresource(bp).Build()
	r := &BuildProjectReconciler{Client: c, Scheme: s, Cfg: builder.Config{Namespace: ns, Port: 1234, HealthPort: 8080}}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Name: key, Namespace: ns}}
	pdbName := types.NamespacedName{Name: router.DaemonName(key), Namespace: ns}
	budget := func(t *testing.T) *policyv1.PodDisruptionBudget {
		t.Helper()
		var pdb policyv1.PodDisruptionBudget
		if err := c.Get(t.Context(), pdbName, &pdb); err != nil {
			return nil
		}
		return &pdb
	}

	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if got := budget(t); got != nil {
		t.Errorf("idle daemon carries a budget (%v) — a drain must not wait on it, and a zero budget is not a no-op", got.Spec.MinAvailable)
	}

	var cur bkov1.BuildProject
	_ = c.Get(t.Context(), req.NamespacedName, &cur)
	cur.Status.SetInflight([]bkov1.InflightBuild{{ID: "b1", Since: metav1.Now()}})
	if err := c.Status().Update(t.Context(), &cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	pdb := budget(t)
	if pdb == nil || pdb.Spec.MinAvailable.IntValue() != 1 {
		t.Fatalf("daemon serving a build: budget = %v, want minAvailable 1 — eviction would sever it", pdb)
	}
	if len(pdb.OwnerReferences) == 0 {
		t.Error("the budget must be owned by the BuildProject, else it outlives it")
	}

	// Released: the budget goes away rather than lingering at zero.
	_ = c.Get(t.Context(), req.NamespacedName, &cur)
	cur.Status.SetInflight(nil)
	if err := c.Status().Update(t.Context(), &cur); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if got := budget(t); got != nil {
		t.Errorf("daemon back to idle still carries a budget (%v), want it removed", got.Spec.MinAvailable)
	}
}
