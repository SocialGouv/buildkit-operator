package local

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/router"
)

// fakeHost is an in-memory HostOps for unit-testing the provisioner's state machine without an Incus host.
type fakeHost struct {
	mu        sync.Mutex
	datasets  map[string]bool
	instances map[string]*fakeInst
	launched  []InstanceSpec
	deleted   []string
	snaps     map[string][]string // dataset -> snapshot full names, oldest-first
	cloned    map[string]string   // new dataset -> source snapshot
	egress    map[string]bool     // instance name -> strict?
}

type fakeInst struct {
	running bool
	ip      string
	dataset string
	vm      bool
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		datasets:  map[string]bool{},
		instances: map[string]*fakeInst{},
		snaps:     map[string][]string{},
		cloned:    map[string]string{},
		egress:    map[string]bool{},
	}
}

func (f *fakeHost) EnsureDataset(_ context.Context, dataset string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.datasets[dataset] = true
	return nil
}

func (f *fakeHost) InstanceExists(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.instances[name]
	return ok, nil
}

func (f *fakeHost) Launch(_ context.Context, spec InstanceSpec) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launched = append(f.launched, spec)
	f.instances[spec.Name] = &fakeInst{running: true, ip: "10.0.0.1", dataset: spec.Dataset, vm: spec.VM}
	return nil
}

func (f *fakeHost) Start(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in := f.instances[name]; in != nil {
		in.running = true
		in.ip = "10.0.0.1"
	}
	return nil
}

func (f *fakeHost) Stop(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in := f.instances[name]; in != nil {
		in.running = false
		in.ip = ""
	}
	return nil
}

func (f *fakeHost) Running(_ context.Context, name string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in := f.instances[name]
	return in != nil && in.running, nil
}

func (f *fakeHost) IP(_ context.Context, name string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if in := f.instances[name]; in != nil {
		return in.ip, nil
	}
	return "", nil
}

func (f *fakeHost) Delete(_ context.Context, name, _ string, _ bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.instances, name)
	f.deleted = append(f.deleted, name)
	return nil
}

func (f *fakeHost) Snapshot(_ context.Context, dataset, snap string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snaps[dataset] = append(f.snaps[dataset], dataset+"@"+snap)
	return nil
}

func (f *fakeHost) ListSnapshots(_ context.Context, dataset string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.snaps[dataset]...), nil
}

func (f *fakeHost) DestroySnapshot(_ context.Context, snapshot string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for ds, list := range f.snaps {
		for i, s := range list {
			if s == snapshot {
				f.snaps[ds] = append(list[:i], list[i+1:]...)
				return nil
			}
		}
	}
	return nil
}

func (f *fakeHost) Clone(_ context.Context, snapshot, dataset string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cloned[dataset] = snapshot
	f.datasets[dataset] = true
	return nil
}

func (f *fakeHost) ApplyEgress(_ context.Context, name string, strict bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.egress[name] = strict
	return nil
}

func testProv(ops HostOps) *Provisioner {
	return New(ops, Config{
		Pool: "tank/bko", Image: "images:debian/12", MountPath: "/data",
		Port: 1234, Wait: 0, IdleTimeout: time.Minute,
	}, logr.Discard())
}

func canonSpec() bkov1.BuildProjectSpec {
	key := router.ProjectKey("github.com/o/r", "", "", "amd64")
	return bkov1.BuildProjectSpec{Key: key, Repo: "github.com/o/r", Arch: "amd64"}
}

// Ensure launches a new instance on its dataset when none exists.
func TestEnsure_LaunchesWhenAbsent(t *testing.T) {
	f := newFakeHost()
	p := testProv(f)
	spec := canonSpec()

	if err := p.Ensure(context.Background(), spec, false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(f.launched) != 1 {
		t.Fatalf("launched %d instances, want 1", len(f.launched))
	}
	got := f.launched[0]
	if got.Name != router.DaemonName(spec.Key) {
		t.Errorf("instance name = %q, want %q", got.Name, router.DaemonName(spec.Key))
	}
	if got.Dataset != "tank/bko/"+spec.Key {
		t.Errorf("dataset = %q, want tank/bko/%s", got.Dataset, spec.Key)
	}
	if got.MountPath != "/data" || got.VM {
		t.Errorf("unexpected spec %+v", got)
	}
	if !f.datasets["tank/bko/"+spec.Key] {
		t.Error("dataset not ensured")
	}
}

// Ensure restarts an existing, stopped instance (scale-up from zero) instead of launching a new one.
func TestEnsure_StartsStoppedInstance(t *testing.T) {
	f := newFakeHost()
	p := testProv(f)
	spec := canonSpec()
	name := router.DaemonName(spec.Key)
	// Pre-existing but stopped (scaled to zero earlier).
	f.instances[name] = &fakeInst{running: false, dataset: "tank/bko/" + spec.Key}

	if err := p.Ensure(context.Background(), spec, false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(f.launched) != 0 {
		t.Errorf("launched %d, want 0 (should start, not relaunch)", len(f.launched))
	}
	if running, _ := f.Running(context.Background(), name); !running {
		t.Error("instance not started")
	}
}

// Ensure is idempotent for an already-running instance: no relaunch, no error.
func TestEnsure_IdempotentWhenRunning(t *testing.T) {
	f := newFakeHost()
	p := testProv(f)
	spec := canonSpec()
	name := router.DaemonName(spec.Key)
	f.instances[name] = &fakeInst{running: true, ip: "10.0.0.1"}

	if err := p.Ensure(context.Background(), spec, false); err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if len(f.launched) != 0 {
		t.Errorf("launched %d, want 0", len(f.launched))
	}
}

// Untrusted builds get a distinct fork key, their OWN dataset, and a VM-isolation request.
func TestEnsure_UntrustedForksToOwnDataset(t *testing.T) {
	f := newFakeHost()
	p := New(f, Config{Pool: "tank/bko", Image: "img", VMImage: "vmimg", MountPath: "/data", Port: 1234}, logr.Discard())
	spec := canonSpec()

	if err := p.Ensure(context.Background(), spec, true); err != nil {
		t.Fatalf("Ensure untrusted: %v", err)
	}
	forkKey := router.ForkKey(spec.Key)
	if len(f.launched) != 1 {
		t.Fatalf("launched %d, want 1", len(f.launched))
	}
	got := f.launched[0]
	if got.Name != router.DaemonName(forkKey) {
		t.Errorf("fork instance = %q, want %q", got.Name, router.DaemonName(forkKey))
	}
	if got.Dataset != "tank/bko/"+forkKey {
		t.Errorf("fork dataset = %q, want its own (anti-poisoning)", got.Dataset)
	}
	if !got.VM || got.Image != "vmimg" {
		t.Errorf("fork should use VM isolation + VM image, got %+v", got)
	}
}

// Ready is false until the instance runs, then true and caches the IP that Endpoint returns.
func TestReadyAndEndpoint(t *testing.T) {
	f := newFakeHost()
	p := testProv(f)
	spec := canonSpec()
	key := spec.Key

	if p.Ready(context.Background(), key) {
		t.Error("Ready = true before any instance, want false")
	}
	if err := p.Ensure(context.Background(), spec, false); err != nil {
		t.Fatal(err)
	}
	if !p.Ready(context.Background(), key) {
		t.Fatal("Ready = false after launch, want true")
	}
	want := router.EndpointHost("10.0.0.1", 1234)
	if got := p.Endpoint(key); got != want {
		t.Errorf("Endpoint = %q, want %q", got, want)
	}
}

// With EndpointDomain set, Endpoint is the deterministic DNS address (no IP query), Ready needs only the
// instance running, and the certs dir is bind-mounted into the instance.
func TestEndpointDomainDeterministic(t *testing.T) {
	f := newFakeHost()
	p := New(f, Config{
		Pool: "tank/bko", Image: "img", MountPath: "/data", Port: 1234,
		EndpointDomain: "bko.local", CertsHostPath: "/etc/bko/certs",
	}, logr.Discard())
	spec := canonSpec()
	if err := p.Ensure(context.Background(), spec, false); err != nil {
		t.Fatal(err)
	}
	if !p.Ready(context.Background(), spec.Key) {
		t.Fatal("Ready = false for a running instance with a DNS endpoint")
	}
	want := router.EndpointHost(router.DaemonName(spec.Key)+".bko.local", 1234)
	if got := p.Endpoint(spec.Key); got != want {
		t.Errorf("Endpoint = %q, want deterministic %q", got, want)
	}
	if f.launched[0].CertsHostPath != "/etc/bko/certs" {
		t.Errorf("certs host path not propagated to the instance: %q", f.launched[0].CertsHostPath)
	}
}

// WaitReady returns nil once running, and errors when the wait budget is exhausted.
func TestWaitReady(t *testing.T) {
	f := newFakeHost()
	p := testProv(f)
	spec := canonSpec()
	if err := p.Ensure(context.Background(), spec, false); err != nil {
		t.Fatal(err)
	}
	if err := p.WaitReady(context.Background(), spec.Key); err != nil {
		t.Errorf("WaitReady (running): %v", err)
	}

	// A key whose instance never runs times out (Wait=0 => deadline passes on the first miss).
	if err := p.WaitReady(context.Background(), "pmissing"); err == nil {
		t.Error("WaitReady: want timeout, got nil")
	}
}

// AddInflight increments, stamps last-build, and floors a negative result at zero.
func TestAddInflight(t *testing.T) {
	f := newFakeHost()
	p := testProv(f)
	key := canonSpec().Key

	p.AddInflight(context.Background(), key, +2)
	p.mu.Lock()
	st := p.projects[key]
	p.mu.Unlock()
	if st == nil || st.inflight != 2 {
		t.Fatalf("inflight = %v, want 2", st)
	}
	if st.lastBuild.IsZero() {
		t.Error("lastBuild not stamped")
	}
	p.AddInflight(context.Background(), key, -5)
	p.mu.Lock()
	got := p.projects[key].inflight
	p.mu.Unlock()
	if got != 0 {
		t.Errorf("inflight = %d after over-decrement, want 0 (floored)", got)
	}
}

// Reconcile stops idle instances (no inflight, last build older than the timeout) and keeps busy ones.
func TestReconcile_ScalesToZeroWhenIdle(t *testing.T) {
	f := newFakeHost()
	p := testProv(f)

	idle := canonSpec()
	busy := bkov1.BuildProjectSpec{Key: router.ProjectKey("github.com/o/busy", "", "", "amd64")}
	if err := p.Ensure(context.Background(), idle, false); err != nil {
		t.Fatal(err)
	}
	if err := p.Ensure(context.Background(), busy, false); err != nil {
		t.Fatal(err)
	}

	// idle: last build well past the timeout, no inflight. busy: inflight build right now.
	p.mu.Lock()
	p.projects[idle.Key].lastBuild = time.Now().Add(-2 * time.Minute)
	p.projects[busy.Key].lastBuild = time.Now()
	p.projects[busy.Key].inflight = 1
	p.mu.Unlock()

	p.Reconcile(context.Background())

	if running, _ := f.Running(context.Background(), router.DaemonName(idle.Key)); running {
		t.Error("idle instance still running after Reconcile, want stopped")
	}
	if running, _ := f.Running(context.Background(), router.DaemonName(busy.Key)); !running {
		t.Error("busy instance stopped, want kept running")
	}
}

// A project never touched (lastBuild zero) is not scaled to zero by accident.
func TestReconcile_KeepsUntouchedProject(t *testing.T) {
	f := newFakeHost()
	p := testProv(f)
	spec := canonSpec()
	if err := p.Ensure(context.Background(), spec, false); err != nil {
		t.Fatal(err)
	}
	p.Reconcile(context.Background())
	if running, _ := f.Running(context.Background(), router.DaemonName(spec.Key)); !running {
		t.Error("untouched instance scaled to zero, want kept (zero lastBuild must not trigger)")
	}
}

// Untrusted builds are refused without a VM image: we never run untrusted code in a container.
func TestEnsure_UntrustedRequiresVMImage(t *testing.T) {
	f := newFakeHost()
	p := New(f, Config{Pool: "tank/bko", Image: "img", MountPath: "/data", Port: 1234}, logr.Discard()) // no VMImage
	if err := p.Ensure(context.Background(), canonSpec(), true); err == nil {
		t.Fatal("Ensure untrusted without VMImage: want error, got nil")
	}
	if len(f.launched) != 0 {
		t.Errorf("launched %d instances for a refused untrusted build, want 0", len(f.launched))
	}
}

// A fork's cache is a CoW clone of the canonical project's latest snapshot (warm fork seed), and gets
// the strict egress profile.
func TestEnsure_ForkSeedsCoWAndStrictEgress(t *testing.T) {
	f := newFakeHost()
	p := New(f, Config{Pool: "tank/bko", Image: "img", VMImage: "vmimg", MountPath: "/data", Port: 1234, ForkEgressStrict: true}, logr.Discard())
	canon := canonSpec()
	// Canonical already has a durability snapshot to seed from.
	canonSnap := p.dataset(canon.Key) + "@bko-1"
	f.snaps[p.dataset(canon.Key)] = []string{canonSnap}

	if err := p.Ensure(context.Background(), canon, true); err != nil {
		t.Fatalf("Ensure untrusted: %v", err)
	}
	forkKey := router.ForkKey(canon.Key)
	if got := f.cloned[p.dataset(forkKey)]; got != canonSnap {
		t.Errorf("fork dataset cloned from %q, want canonical snapshot %q", got, canonSnap)
	}
	if !f.egress[router.DaemonName(forkKey)] {
		t.Error("fork instance not given the strict egress profile")
	}
}

// Reconcile snapshots a canonical project on cadence and prunes to KeepSnapshots (oldest first).
func TestReconcile_SnapshotCadenceAndRetention(t *testing.T) {
	f := newFakeHost()
	p := New(f, Config{Pool: "tank/bko", Image: "img", MountPath: "/data", Port: 1234,
		IdleTimeout: time.Hour, SnapshotEvery: time.Millisecond, KeepSnapshots: 2}, logr.Discard())
	spec := canonSpec()
	if err := p.Ensure(context.Background(), spec, false); err != nil {
		t.Fatal(err)
	}
	// Two pre-existing snapshots + a build so the cadence fires.
	ds := p.dataset(spec.Key)
	f.snaps[ds] = []string{ds + "@old0", ds + "@old1"}
	p.AddInflight(context.Background(), spec.Key, 0) // stamps lastBuild

	p.Reconcile(context.Background())

	got := f.snaps[ds]
	if len(got) != 2 {
		t.Fatalf("snapshots = %v, want 2 retained", got)
	}
	if got[0] == ds+"@old0" {
		t.Errorf("oldest snapshot not pruned: %v", got)
	}
	p.mu.Lock()
	last := p.projects[spec.Key].lastSnap
	p.mu.Unlock()
	if last == "" {
		t.Error("lastSnap not recorded after snapshot")
	}
}

// Forks are never snapshotted by the cadence (children inherit, never snapshot themselves).
func TestReconcile_SkipsForkSnapshots(t *testing.T) {
	f := newFakeHost()
	p := New(f, Config{Pool: "tank/bko", Image: "img", VMImage: "vmimg", MountPath: "/data", Port: 1234,
		IdleTimeout: time.Hour, SnapshotEvery: time.Millisecond, KeepSnapshots: 3}, logr.Discard())
	if err := p.Ensure(context.Background(), canonSpec(), true); err != nil {
		t.Fatal(err)
	}
	forkKey := router.ForkKey(canonSpec().Key)
	p.AddInflight(context.Background(), forkKey, 0)

	p.Reconcile(context.Background())

	if snaps := f.snaps[p.dataset(forkKey)]; len(snaps) != 0 {
		t.Errorf("fork was snapshotted (%v), want none", snaps)
	}
}

// Fanout spawns n hot CoW clones, seeded from the project's snapshot, that the idle loop never stops.
func TestFanout_SpawnsHotCoWClones(t *testing.T) {
	f := newFakeHost()
	p := New(f, Config{Pool: "tank/bko", Image: "img", MountPath: "/data", Port: 1234, IdleTimeout: time.Minute}, logr.Discard())
	key := canonSpec().Key
	snap := p.dataset(key) + "@bko-1"
	f.snaps[p.dataset(key)] = []string{snap}

	if err := p.Fanout(context.Background(), key, 2); err != nil {
		t.Fatalf("Fanout: %v", err)
	}
	for i := 1; i <= 2; i++ {
		ck := router.CloneKey(key, i)
		if running, _ := f.Running(context.Background(), router.DaemonName(ck)); !running {
			t.Errorf("clone %d not running", i)
		}
		if f.cloned[p.dataset(ck)] != snap {
			t.Errorf("clone %d not seeded from snapshot", i)
		}
		// Mark the clone long-idle: a hot clone must survive Reconcile anyway.
		p.mu.Lock()
		p.projects[ck].lastBuild = time.Now().Add(-time.Hour)
		p.mu.Unlock()
	}

	p.Reconcile(context.Background())

	for i := 1; i <= 2; i++ {
		ck := router.CloneKey(key, i)
		if running, _ := f.Running(context.Background(), router.DaemonName(ck)); !running {
			t.Errorf("hot clone %d was scaled to zero, want kept", i)
		}
	}
}
