package local

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/go-logr/logr"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/provisioner"
	"github.com/socialgouv/buildkit-operator/internal/router"
)

// Config holds the single-host knobs for the local backend.
type Config struct {
	Pool             string        // ZFS parent dataset for project caches, e.g. "tank/bko"
	Image            string        // Incus image providing a vanilla buildkitd, e.g. "images:debian/12/buildkit"
	VMImage          string        // image for UNTRUSTED fork instances (VM isolation); empty disables forks
	MountPath        string        // buildkitd data dir the cache dataset is mounted at
	Port             int32         // buildkitd mTLS port
	Wait             time.Duration // cold-start (start instance) wait budget
	IdleTimeout      time.Duration // scale-to-zero after this much idle (no inflight, no recent build)
	SnapshotEvery    time.Duration // durability snapshot cadence per canonical project (0 = disabled)
	KeepSnapshots    int           // snapshots retained per project (older pruned)
	MaxBuildSeconds  int           // safety net: an inflight older than this stops pinning warm (0 = off)
	ForkEgressStrict bool          // bind the strict egress ACL to untrusted fork instances (default on)
	// EndpointDomain, when set, makes Endpoint return the DETERMINISTIC tcp://<daemon>.<domain>:<port>
	// (resolved by Incus DNS), matching a wildcard *.<domain> daemon cert — the single-host analogue of
	// the k8s Service DNS / SNI gateway. Empty = dial the instance's IP (cached during readiness checks).
	EndpointDomain string
	// CertsHostPath, when set, is the host directory holding the daemon mTLS material (ca.pem/cert.pem/
	// key.pem); it is bind-mounted read-only into each instance at /certs so buildkitd serves mTLS.
	CertsHostPath string
}

// projectState is the in-memory record for one project's daemon. The local backend is a single process,
// so in-memory is the source of truth for inflight/last-build (no CRD); instances themselves are
// reconstructable from `incus` on restart. ip is cached during readiness checks so Endpoint (which has
// no ctx) can return the dialable address the handler just observed.
type projectState struct {
	spec       bkov1.BuildProjectSpec
	vm         bool
	hot        bool // fan-out clone: stays up, never scaled to zero by the idle loop
	inflight   int32
	lastBuild  time.Time
	ip         string
	lastSnap   string    // newest durability snapshot (full name dataset@snap), the seed for forks/clones
	lastSnapAt time.Time // when lastSnap was taken (drives the cadence)
}

// Provisioner is the single-host (Incus + ZFS) implementation of provisioner.Provisioner.
type Provisioner struct {
	ops HostOps
	cfg Config
	log logr.Logger

	mu       sync.Mutex
	projects map[string]*projectState
}

var _ provisioner.Provisioner = (*Provisioner)(nil)

// addrResolver lets a runtime own the full dial address host:port for a key (e.g. the Docker runtime
// publishes buildkitd to a deterministic loopback port). When the HostOps implements it, Endpoint uses
// it and readiness needs only the instance running; otherwise the EndpointDomain / instance-IP path runs.
type addrResolver interface {
	Addr(key string) string
}

// New builds the local provisioner over the given HostOps (NewCLI in production, a fake in tests).
func New(ops HostOps, cfg Config, log logr.Logger) *Provisioner {
	return &Provisioner{ops: ops, cfg: cfg, log: log, projects: map[string]*projectState{}}
}

// dataset is the ZFS dataset backing a key's warm cache.
func (p *Provisioner) dataset(key string) string {
	return strings.TrimRight(p.cfg.Pool, "/") + "/" + key
}

// imageFor picks the instance image: the VM image for untrusted forks (VM isolation), else the default.
func (p *Provisioner) imageFor(vm bool) string {
	if vm && p.cfg.VMImage != "" {
		return p.cfg.VMImage
	}
	return p.cfg.Image
}

// Ensure provisions the daemon for spec, deriving a distinct ephemeral fork project when untrusted: a
// fork PR gets its OWN cache (CoW-seeded read-only from the canonical snapshot, so it can never poison
// the canonical cache) and runs under VM isolation (refused unless a VM image is configured). It is
// idempotent: an existing stopped instance is started; a missing one is launched on its (seeded) dataset.
func (p *Provisioner) Ensure(ctx context.Context, spec bkov1.BuildProjectSpec, untrusted bool) error {
	vm := false
	seedFrom := "" // canonical key to CoW-seed the fork's cache from (its latest snapshot)
	if untrusted {
		// Untrusted code must run under VM isolation — refuse rather than launch a fork in a container.
		if p.cfg.VMImage == "" {
			return errors.New("untrusted build requires VM isolation: set --incus-vm-image")
		}
		seedFrom = spec.Key
		spec = bkov1.DeriveChild(spec, "", bkov1.ForkChild, router.ForkKey(spec.Key))
		vm = true
	}
	return p.ensureInstance(ctx, spec, vm, untrusted && p.cfg.ForkEgressStrict, seedFrom)
}

// ensureInstance idempotently provisions one project's instance: start it if it exists (scale-up from
// zero), else seed its dataset (CoW clone from seedFrom's latest snapshot when set, else a fresh
// dataset) and launch it, then bind the egress profile.
func (p *Provisioner) ensureInstance(ctx context.Context, spec bkov1.BuildProjectSpec, vm, strictEgress bool, seedFrom string) error {
	key := spec.Key
	name := router.DaemonName(key)

	p.mu.Lock()
	st := p.projects[key]
	if st == nil {
		st = &projectState{spec: spec, vm: vm, hot: spec.Tier == bkov1.TierHot}
		p.projects[key] = st
	}
	p.mu.Unlock()

	exists, err := p.ops.InstanceExists(ctx, name)
	if err != nil {
		return fmt.Errorf("instance exists: %w", err)
	}
	if exists {
		running, err := p.ops.Running(ctx, name)
		if err != nil {
			return fmt.Errorf("instance running: %w", err)
		}
		if !running {
			if err := p.ops.Start(ctx, name); err != nil {
				return fmt.Errorf("start instance: %w", err)
			}
		}
		return nil
	}

	if err := p.seedDataset(ctx, key, seedFrom); err != nil {
		return err
	}
	instConfig := map[string]string{labelKey: key}
	if !vm {
		// A trusted canonical daemon runs as a system CONTAINER: buildkitd's OCI worker needs nesting
		// (runc/overlayfs in the container) and runs privileged — acceptable on a single trusted host.
		// Untrusted forks instead get vm=true (the VM is the boundary), never a privileged container.
		instConfig["security.nesting"] = "true"
		instConfig["security.privileged"] = "true"
	}
	ispec := InstanceSpec{
		Name:          name,
		Image:         p.imageFor(vm),
		VM:            vm,
		Dataset:       p.dataset(key),
		MountPath:     p.cfg.MountPath,
		CertsHostPath: p.cfg.CertsHostPath,
		Config:        instConfig,
	}
	if err := p.ops.Launch(ctx, ispec); err != nil {
		return fmt.Errorf("launch instance: %w", err)
	}
	// Egress hardening is best-effort: a missing ACL (e.g. the dev runtime, or ACLs not yet provisioned)
	// must not fail the build. It is logged loudly so a strict/untrusted daemon without its ACL is visible.
	if err := p.ops.ApplyEgress(ctx, name, strictEgress); err != nil {
		p.log.Error(err, "apply egress (continuing)", "key", key, "strict", strictEgress)
	}
	return nil
}

// seedDataset prepares a key's cache dataset. When seedFrom is set and that project has a snapshot, the
// dataset is an instant CoW clone of it (warm fork/clone); otherwise a fresh empty dataset.
func (p *Provisioner) seedDataset(ctx context.Context, key, seedFrom string) error {
	if seedFrom != "" {
		if snap := p.latestSnapshot(ctx, seedFrom); snap != "" {
			if err := p.ops.Clone(ctx, snap, p.dataset(key)); err != nil {
				return fmt.Errorf("clone seed: %w", err)
			}
			return nil
		}
	}
	if err := p.ops.EnsureDataset(ctx, p.dataset(key)); err != nil {
		return fmt.Errorf("ensure dataset: %w", err)
	}
	return nil
}

// latestSnapshot returns the newest snapshot full-name for a project (in-memory hint first, else asks
// ZFS so it survives a buildd restart). Empty when the project has never been snapshotted.
func (p *Provisioner) latestSnapshot(ctx context.Context, key string) string {
	p.mu.Lock()
	if st := p.projects[key]; st != nil && st.lastSnap != "" {
		snap := st.lastSnap
		p.mu.Unlock()
		return snap
	}
	p.mu.Unlock()
	snaps, err := p.ops.ListSnapshots(ctx, p.dataset(key))
	if err != nil || len(snaps) == 0 {
		return ""
	}
	return snaps[len(snaps)-1]
}

// labelKey marks our instances so reconcile/GC can list only what we manage.
const labelKey = "user.buildkit-operator.key"

// Ready reports whether the daemon is running. With a deterministic DNS endpoint (EndpointDomain) that
// is sufficient; otherwise it also requires — and caches — the instance IP that Endpoint will return.
func (p *Provisioner) Ready(ctx context.Context, key string) bool {
	name := router.DaemonName(key)
	running, err := p.ops.Running(ctx, name)
	if err != nil || !running {
		return false
	}
	if _, ok := p.ops.(addrResolver); ok {
		return true // the runtime owns a deterministic dial address; running is sufficient
	}
	if p.cfg.EndpointDomain != "" {
		return true
	}
	if ip, err := p.ops.IP(ctx, name); err == nil && ip != "" {
		p.setIP(key, ip)
		return true
	}
	return false
}

// WaitReady polls until the daemon is running with an IP, or the wait budget / ctx elapses.
func (p *Provisioner) WaitReady(ctx context.Context, key string) error {
	deadline := time.Now().Add(p.cfg.Wait)
	for {
		if p.Ready(ctx, key) {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("timed out waiting for instance to be ready")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
}

// Endpoint returns the mTLS address clients dial. With EndpointDomain it is the deterministic
// tcp://<daemon>.<domain>:<port> (resolved by Incus DNS, validated by a wildcard cert); otherwise
// tcp://<instance-ip>:<port> using the IP cached by the preceding Ready/WaitReady (Endpoint has no ctx).
func (p *Provisioner) Endpoint(key string) string {
	if r, ok := p.ops.(addrResolver); ok {
		return "tcp://" + r.Addr(key)
	}
	if p.cfg.EndpointDomain != "" {
		return router.EndpointHost(router.DaemonName(key)+"."+p.cfg.EndpointDomain, p.cfg.Port)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	ip := ""
	if st := p.projects[key]; st != nil {
		ip = st.ip
	}
	return router.EndpointHost(ip, p.cfg.Port)
}

// AddInflight adjusts the in-memory inflight counter (floored at 0) and stamps last-build, keeping the
// instance pinned warm against the scale-to-zero loop for the build's duration.
func (p *Provisioner) AddInflight(_ context.Context, key string, delta int32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	st := p.projects[key]
	if st == nil {
		st = &projectState{spec: bkov1.BuildProjectSpec{Key: key}}
		p.projects[key] = st
	}
	st.inflight += delta
	if st.inflight < 0 {
		st.inflight = 0
	}
	st.lastBuild = time.Now()
}

func (p *Provisioner) setIP(key, ip string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if st := p.projects[key]; st != nil {
		st.ip = ip
	}
}

// Reconcile runs one lifecycle pass: scale idle projects to zero, and take durability snapshots on
// cadence (with retention). It is the single-host analogue of the controller's desiredReplicas + M3
// snapshot loop; Run drives it on a ticker.
func (p *Provisioner) Reconcile(ctx context.Context) {
	now := time.Now()
	type cand struct{ key, name string }
	var idle, snap []cand
	p.mu.Lock()
	for key, st := range p.projects {
		// An inflight build pins the daemon warm — UNLESS it is older than the MaxBuildSeconds safety net
		// (a missed /complete must not pin a daemon forever; mirrors the k8s reconciler).
		pinned := st.inflight > 0
		if pinned && p.cfg.MaxBuildSeconds > 0 && now.Sub(st.lastBuild) > time.Duration(p.cfg.MaxBuildSeconds)*time.Second {
			pinned = false
		}
		// Scale-to-zero: warm (non-hot) projects idle past the timeout. Hot fan-out clones never idle out.
		if !st.hot && !pinned && !st.lastBuild.IsZero() && now.Sub(st.lastBuild) > p.cfg.IdleTimeout {
			idle = append(idle, cand{key: key, name: router.DaemonName(key)})
		}
		// Snapshot cadence: canonical projects only (forks/clones never snapshot — see DeriveChild), on
		// the configured interval. First snapshot fires once the project has seen a build.
		if p.cfg.SnapshotEvery > 0 && !router.IsForkKey(key) && !st.hot && !st.lastBuild.IsZero() &&
			now.Sub(st.lastSnapAt) > p.cfg.SnapshotEvery {
			snap = append(snap, cand{key: key})
		}
	}
	p.mu.Unlock()

	for _, c := range idle {
		running, err := p.ops.Running(ctx, c.name)
		if err != nil || !running {
			continue
		}
		if err := p.ops.Stop(ctx, c.name); err != nil {
			p.log.Error(err, "scale-to-zero stop failed", "key", c.key)
			continue
		}
		p.log.Info("scaled to zero (idle)", "key", c.key)
	}
	for _, c := range snap {
		p.snapshot(ctx, c.key, now)
	}
}

// snapshot takes one durability snapshot of a project's cache and prunes to KeepSnapshots (oldest first).
func (p *Provisioner) snapshot(ctx context.Context, key string, now time.Time) {
	name := fmt.Sprintf("bko-%d", now.Unix())
	dataset := p.dataset(key)
	if err := p.ops.Snapshot(ctx, dataset, name); err != nil {
		p.log.Error(err, "snapshot failed", "key", key)
		return
	}
	full := dataset + "@" + name
	p.mu.Lock()
	if st := p.projects[key]; st != nil {
		st.lastSnap = full
		st.lastSnapAt = now
	}
	p.mu.Unlock()

	if p.cfg.KeepSnapshots > 0 {
		snaps, err := p.ops.ListSnapshots(ctx, dataset)
		if err != nil {
			return
		}
		for i := 0; i < len(snaps)-p.cfg.KeepSnapshots; i++ {
			if err := p.ops.DestroySnapshot(ctx, snaps[i]); err != nil {
				p.log.Error(err, "prune snapshot failed", "snapshot", snaps[i])
			}
		}
	}
}

// Fanout ensures n hot CoW-clone daemons for a saturated project, each seeded from the project's latest
// snapshot (instant zfs clone) and kept hot (never scaled to zero). The clone primitive is the same one
// the fork path uses; an automatic saturation trigger is left to the caller (future work).
func (p *Provisioner) Fanout(ctx context.Context, key string, n int) error {
	for i := 1; i <= n; i++ {
		cloneKey := router.CloneKey(key, i)
		spec := bkov1.DeriveChild(bkov1.BuildProjectSpec{Key: key}, "", bkov1.CloneChild, cloneKey)
		if err := p.ensureInstance(ctx, spec, false, false, key); err != nil {
			return fmt.Errorf("fanout clone %d: %w", i, err)
		}
	}
	return nil
}

// Run drives the reconcile loop until ctx is done. Wired by buildd's local-backend setup as the
// lifecycle goroutine (the k8s backend uses a controller-runtime manager instead).
func (p *Provisioner) Run(ctx context.Context) error {
	interval := p.cfg.IdleTimeout / 4
	if interval < 5*time.Second {
		interval = 5 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			p.Reconcile(ctx)
		}
	}
}
