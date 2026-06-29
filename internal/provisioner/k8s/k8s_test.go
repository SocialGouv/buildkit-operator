package k8s

import (
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/builder"
	"github.com/socialgouv/buildkit-operator/internal/router"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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
	return s
}

func newProv(c client.Client, wait time.Duration, gatewayHost string, gatewayPort int32) *Provisioner {
	return New(c, builder.Config{Namespace: "buildkit-operator", Port: 1234}, wait, gatewayHost, gatewayPort, logr.Discard())
}

// TestEndpoint covers both endpoint shapes: gateway SNI host (off-cluster) vs in-cluster Service DNS.
func TestEndpoint(t *testing.T) {
	// In-cluster: no gateway host configured.
	in := New(nil, builder.Config{Namespace: "ns", Port: 1234}, 0, "", 0, logr.Discard())
	if got, want := in.Endpoint("p1"), router.Endpoint("p1", "ns", 1234); got != want {
		t.Errorf("in-cluster Endpoint = %q, want %q", got, want)
	}

	// Gateway: SNI hostname <daemon>.<gatewayHost>, with the gateway port overriding cfg.Port.
	gw := New(nil, builder.Config{Namespace: "ns", Port: 1234}, 0, "gw.example.com", 443, logr.Discard())
	got := gw.Endpoint("p1")
	want := router.EndpointHost(router.DaemonName("p1")+".gw.example.com", 443)
	if got != want {
		t.Errorf("gateway Endpoint = %q, want %q", got, want)
	}

	// Gateway host but no explicit gateway port => falls back to cfg.Port.
	gw2 := New(nil, builder.Config{Namespace: "ns", Port: 1234}, 0, "gw.example.com", 0, logr.Discard())
	if !strings.HasSuffix(gw2.Endpoint("p1"), ":1234") {
		t.Errorf("gateway w/o port should use cfg.Port: %q", gw2.Endpoint("p1"))
	}
}

// TestEnsure_StampsDefaultStorageClass: a project created with no StorageClass gets the operator-wide
// default stamped (empty default leaves it unset so the cluster's default StorageClass is used); an
// explicit StorageClass is always preserved.
func TestEnsure_StampsDefaultStorageClass(t *testing.T) {
	get := func(t *testing.T, defaultSC, specSC string) string {
		t.Helper()
		c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
		p := New(c, builder.Config{Namespace: "buildkit-operator", Port: 1234, DefaultStorageClass: defaultSC}, 0, "", 0, logr.Discard())
		spec := bkov1.BuildProjectSpec{Key: "psc", Arch: "amd64", StorageClass: specSC}
		if err := p.Ensure(t.Context(), spec, false); err != nil {
			t.Fatalf("Ensure: %v", err)
		}
		var bp bkov1.BuildProject
		if err := c.Get(t.Context(), types.NamespacedName{Name: "psc", Namespace: "buildkit-operator"}, &bp); err != nil {
			t.Fatalf("get: %v", err)
		}
		return bp.Spec.StorageClass
	}
	if got := get(t, "ebs-gp3", ""); got != "ebs-gp3" {
		t.Errorf("empty spec + default: StorageClass = %q, want ebs-gp3", got)
	}
	if got := get(t, "", ""); got != "" {
		t.Errorf("empty spec + empty default: StorageClass = %q, want empty (cluster default)", got)
	}
	if got := get(t, "ebs-gp3", "fast-ssd"); got != "fast-ssd" {
		t.Errorf("explicit spec StorageClass must be preserved, got %q", got)
	}
}

// TestEnsure_CreatesAndStamps: a missing project is created and its LastBuildTime stamped
// (warm-from-birth), so desiredReplicas holds a warm replica immediately.
func TestEnsure_CreatesAndStamps(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).Build()
	p := newProv(c, 0, "", 0)

	spec := bkov1.BuildProjectSpec{Key: "pnew", Repo: "github.com/o/r", Arch: "amd64"}
	if err := p.Ensure(t.Context(), spec, false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	var bp bkov1.BuildProject
	if err := c.Get(t.Context(), types.NamespacedName{Name: "pnew", Namespace: p.namespace}, &bp); err != nil {
		t.Fatalf("project not created: %v", err)
	}
	if bp.Status.LastBuildTime == nil {
		t.Error("LastBuildTime not stamped at creation")
	}
}

// TestEnsure_Idempotent: an existing project is a no-op (no error, no second create).
func TestEnsure_Idempotent(t *testing.T) {
	existing := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: "pexist", Namespace: "buildkit-operator"},
		Spec:       bkov1.BuildProjectSpec{Key: "pexist", Repo: "github.com/o/r", Arch: "amd64"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(existing).Build()
	p := newProv(c, 0, "", 0)

	if err := p.Ensure(t.Context(), existing.Spec, false); err != nil {
		t.Errorf("idempotent Ensure: %v", err)
	}
}

// TestEnsure_UntrustedDerivesFork: an untrusted build provisions the ephemeral fork project (distinct
// key, seeded read-only from the canonical snapshot), never the canonical one — the anti-poisoning path.
func TestEnsure_UntrustedDerivesFork(t *testing.T) {
	canonical := router.ProjectKey("github.com/o/r", "", "", "amd64")
	canon := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: canonical, Namespace: "buildkit-operator"},
		Spec:       bkov1.BuildProjectSpec{Key: canonical, Repo: "github.com/o/r", Arch: "amd64"},
	}
	canon.Status.LastSnapshot = "snap-1"
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(canon).Build()
	p := newProv(c, 0, "", 0)

	spec := bkov1.BuildProjectSpec{Key: canonical, Repo: "github.com/o/r", Arch: "amd64"}
	if err := p.Ensure(t.Context(), spec, true); err != nil {
		t.Fatalf("Ensure untrusted: %v", err)
	}
	forkKey := router.ForkKey(canonical)
	var fork bkov1.BuildProject
	if err := c.Get(t.Context(), types.NamespacedName{Name: forkKey, Namespace: p.namespace}, &fork); err != nil {
		t.Fatalf("fork project not created: %v", err)
	}
	if fork.Spec.RestoreFromSnapshot != "snap-1" {
		t.Errorf("fork seed = %q, want canonical snapshot snap-1", fork.Spec.RestoreFromSnapshot)
	}
}

// TestWaitReady_Success: a StatefulSet already reporting a ready replica returns nil immediately.
func TestWaitReady_Success(t *testing.T) {
	key := "pwarm"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: router.DaemonName(key), Namespace: "buildkit-operator"},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sts).Build()
	p := newProv(c, time.Second, "", 0)
	if err := p.WaitReady(t.Context(), key); err != nil {
		t.Errorf("WaitReady: %v", err)
	}
}

// TestWaitReady_Timeout: with no daemon and a zero wait budget, WaitReady gives up with an error.
func TestWaitReady_Timeout(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	p := newProv(c, 0, "", 0) // deadline already passed on first miss
	if err := p.WaitReady(t.Context(), "pmissing"); err == nil {
		t.Error("WaitReady: want timeout error, got nil")
	}
}

// TestReady covers the warm fast-path probe: true with a ready replica, false when the STS is missing.
func TestReady(t *testing.T) {
	key := "pwarm"
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: router.DaemonName(key), Namespace: "buildkit-operator"},
		Status:     appsv1.StatefulSetStatus{ReadyReplicas: 1},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(sts).Build()
	p := newProv(c, 0, "", 0)
	if !p.Ready(t.Context(), key) {
		t.Error("Ready = false with a ready replica, want true")
	}
	if p.Ready(t.Context(), "pmissing") {
		t.Error("Ready = true with no StatefulSet, want false")
	}
}

// TestAddInflight_UpdatesStatus: AddInflight increments the inflight counter, stamps LastBuildTime,
// and floors a negative result at zero (a /complete that races ahead of /route can't go negative).
func TestAddInflight_UpdatesStatus(t *testing.T) {
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: "pcount", Namespace: "buildkit-operator"},
		Spec:       bkov1.BuildProjectSpec{Key: "pcount", Repo: "github.com/o/r", Arch: "amd64"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
	p := newProv(c, 0, "", 0)

	p.AddInflight(t.Context(), "pcount", +2)
	var got bkov1.BuildProject
	_ = c.Get(t.Context(), types.NamespacedName{Name: "pcount", Namespace: "buildkit-operator"}, &got)
	if got.Status.InflightBuilds != 2 {
		t.Errorf("InflightBuilds = %d, want 2", got.Status.InflightBuilds)
	}
	if got.Status.LastBuildTime == nil {
		t.Error("LastBuildTime not stamped")
	}

	p.AddInflight(t.Context(), "pcount", -5) // floors at 0
	_ = c.Get(t.Context(), types.NamespacedName{Name: "pcount", Namespace: "buildkit-operator"}, &got)
	if got.Status.InflightBuilds != 0 {
		t.Errorf("InflightBuilds = %d after over-decrement, want 0 (floored)", got.Status.InflightBuilds)
	}
}

// Only positive deltas (real routed builds) feed the adaptive-idle cadence ring — a prewarm touch
// (delta 0) or a /complete release (delta -1) must not inflate the observed build frequency.
func TestAddInflight_RecordsBuildCadence(t *testing.T) {
	bp := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: "pcadence", Namespace: "buildkit-operator"},
		Spec:       bkov1.BuildProjectSpec{Key: "pcadence", Repo: "github.com/o/r", Arch: "amd64"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
	p := newProv(c, 0, "", 0)

	p.AddInflight(t.Context(), "pcadence", +1)
	p.AddInflight(t.Context(), "pcadence", 0)  // prewarm touch
	p.AddInflight(t.Context(), "pcadence", -1) // complete
	p.AddInflight(t.Context(), "pcadence", +1)

	var got bkov1.BuildProject
	_ = c.Get(t.Context(), types.NamespacedName{Name: "pcadence", Namespace: "buildkit-operator"}, &got)
	if n := len(got.Status.RecentBuildTimes); n != 2 {
		t.Errorf("RecentBuildTimes = %d entries, want 2 (only +1 deltas recorded)", n)
	}
}

// S3CacheDecision: policy semantics + the cadence CAS (one export grant per window, stamped
// on the status; interval 0 restores export-every-build; always/never bypass the clock).
func TestS3CacheDecision(t *testing.T) {
	mk := func(policy string, lastGrant *metav1.Time) client.WithWatch {
		bp := &bkov1.BuildProject{
			ObjectMeta: metav1.ObjectMeta{Name: "pcache", Namespace: "buildkit-operator"},
			Spec:       bkov1.BuildProjectSpec{Key: "pcache", Repo: "github.com/o/r", Arch: "amd64", S3CachePolicy: policy},
			Status:     bkov1.BuildProjectStatus{LastCacheExportGrant: lastGrant},
		}
		return fake.NewClientBuilder().WithScheme(testScheme(t)).WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(bp).Build()
	}
	recent := metav1.NewTime(time.Now().Add(-time.Minute))
	stale := metav1.NewTime(time.Now().Add(-2 * time.Hour))

	cases := []struct {
		name        string
		c           client.WithWatch
		interval    time.Duration
		grant       bool
		wantImp     bool
		wantExp     bool
		wantStamped bool
	}{
		{"never", mk(bkov1.S3CacheNever, nil), time.Hour, true, false, false, false},
		{"always", mk(bkov1.S3CacheAlways, &recent), time.Hour, true, true, true, false},
		{"cadence first grant", mk("", nil), time.Hour, true, true, true, true},
		{"cadence window open", mk("", &stale), time.Hour, true, true, true, true},
		{"cadence window closed", mk("", &recent), time.Hour, true, true, false, false},
		{"cadence prewarm no grant", mk("", &stale), time.Hour, false, true, false, false},
		{"interval zero = historical", mk("", nil), 0, true, true, true, false},
	}
	for _, tc := range cases {
		p := newProv(tc.c, 0, "", 0)
		imp, exp := p.S3CacheDecision(t.Context(), "pcache", tc.interval, tc.grant)
		if imp != tc.wantImp || exp != tc.wantExp {
			t.Errorf("%s: decision = (%v,%v), want (%v,%v)", tc.name, imp, exp, tc.wantImp, tc.wantExp)
		}
		if tc.wantStamped {
			var got bkov1.BuildProject
			_ = tc.c.Get(t.Context(), types.NamespacedName{Name: "pcache", Namespace: "buildkit-operator"}, &got)
			// metav1.Time truncates to seconds — assert freshness, not ordering.
			if got.Status.LastCacheExportGrant == nil || time.Since(got.Status.LastCacheExportGrant.Time) > time.Minute {
				t.Errorf("%s: grant not stamped", tc.name)
			}
		}
	}

	// Missing CR (transient read failure surrogate): fail-open to import+export.
	empty := fake.NewClientBuilder().WithScheme(testScheme(t)).Build()
	if imp, exp := newProv(empty, 0, "", 0).S3CacheDecision(t.Context(), "absent", time.Hour, true); !imp || !exp {
		t.Errorf("missing CR: decision = (%v,%v), want fail-open (true,true)", imp, exp)
	}
}

// A fork derives from the LIVE canonical spec, not the request-reconstructed
// one: after auto-grow the canonical's CacheVolumeGi exceeds the request/rule
// value, and a fork PVC smaller than the snapshot it restores from is refused
// by the CSI provisioner (the untrusted build would 504 forever).
func TestEnsure_ForkInheritsLiveCanonicalSpec(t *testing.T) {
	spec := bkov1.BuildProjectSpec{Key: "pcanon", Repo: "github.com/o/r", Arch: "amd64"}
	canon := &bkov1.BuildProject{
		ObjectMeta: metav1.ObjectMeta{Name: "pcanon", Namespace: "buildkit-operator"},
		Spec: bkov1.BuildProjectSpec{
			Key: "pcanon", Repo: "github.com/o/r", Arch: "amd64",
			CacheVolumeGi: 240, // auto-grown past the request-derived default
		},
		Status: bkov1.BuildProjectStatus{LastSnapshot: "snap-1"},
	}
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).
		WithStatusSubresource(&bkov1.BuildProject{}).WithObjects(canon).Build()
	p := newProv(c, 0, "", 0)

	if err := p.Ensure(t.Context(), spec, true); err != nil {
		t.Fatal(err)
	}
	var fork bkov1.BuildProject
	if err := c.Get(t.Context(), types.NamespacedName{Name: router.ForkKey("pcanon"), Namespace: "buildkit-operator"}, &fork); err != nil {
		t.Fatalf("fork not created: %v", err)
	}
	if fork.Spec.CacheVolumeGi != 240 {
		t.Errorf("fork CacheVolumeGi = %d, want 240 (live canonical spec)", fork.Spec.CacheVolumeGi)
	}
	if fork.Spec.RestoreFromSnapshot != "snap-1" {
		t.Errorf("fork RestoreFromSnapshot = %q, want snap-1", fork.Spec.RestoreFromSnapshot)
	}
}
