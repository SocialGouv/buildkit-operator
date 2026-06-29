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
	Pool        string        // ZFS parent dataset for project caches, e.g. "tank/bko"
	Image       string        // Incus image providing a vanilla buildkitd, e.g. "images:debian/12/buildkit"
	VMImage     string        // image for UNTRUSTED fork instances (VM isolation, P3); empty = Image
	MountPath   string        // buildkitd data dir the cache dataset is mounted at
	Port        int32         // buildkitd mTLS port
	Wait        time.Duration // cold-start (start instance) wait budget
	IdleTimeout time.Duration // scale-to-zero after this much idle (no inflight, no recent build)
}

// projectState is the in-memory record for one project's daemon. The local backend is a single process,
// so in-memory is the source of truth for inflight/last-build (no CRD); instances themselves are
// reconstructable from `incus` on restart. ip is cached during readiness checks so Endpoint (which has
// no ctx) can return the dialable address the handler just observed.
type projectState struct {
	spec      bkov1.BuildProjectSpec
	vm        bool
	inflight  int32
	lastBuild time.Time
	ip        string
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

// Ensure provisions the daemon for spec, deriving a distinct ephemeral fork project when untrusted so a
// fork PR gets its OWN cache dataset and can never poison the canonical one (the anti-poisoning property
// holds at the storage layer in the MVP; VM isolation of the fork is P3). It is idempotent: an existing
// stopped instance is started; a missing one is launched on its retained dataset.
func (p *Provisioner) Ensure(ctx context.Context, spec bkov1.BuildProjectSpec, untrusted bool) error {
	vm := false
	if untrusted {
		spec = bkov1.DeriveChild(spec, "", bkov1.ForkChild, router.ForkKey(spec.Key))
		vm = true // request VM isolation; honoured when a VMImage is configured (P3 seeds it CoW)
	}
	key := spec.Key
	name := router.DaemonName(key)

	p.mu.Lock()
	st := p.projects[key]
	if st == nil {
		st = &projectState{spec: spec, vm: vm}
		p.projects[key] = st
	}
	p.mu.Unlock()

	if err := p.ops.EnsureDataset(ctx, p.dataset(key)); err != nil {
		return fmt.Errorf("ensure dataset: %w", err)
	}
	exists, err := p.ops.InstanceExists(ctx, name)
	if err != nil {
		return fmt.Errorf("instance exists: %w", err)
	}
	if !exists {
		ispec := InstanceSpec{
			Name:      name,
			Image:     p.imageFor(vm),
			VM:        vm,
			Dataset:   p.dataset(key),
			MountPath: p.cfg.MountPath,
			Config:    map[string]string{labelKey: key},
		}
		if err := p.ops.Launch(ctx, ispec); err != nil {
			return fmt.Errorf("launch instance: %w", err)
		}
		return nil
	}
	// Exists but may be stopped (scaled to zero): start it back up on its warm dataset.
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

// labelKey marks our instances so reconcile/GC can list only what we manage.
const labelKey = "user.buildkit-operator.key"

// Ready reports whether the daemon is running, caching its IP for Endpoint.
func (p *Provisioner) Ready(ctx context.Context, key string) bool {
	name := router.DaemonName(key)
	running, err := p.ops.Running(ctx, name)
	if err != nil || !running {
		return false
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

// Endpoint returns the mTLS address clients dial: tcp://<instance-ip>:<port>. The IP is the one cached by
// the preceding Ready/WaitReady (Endpoint has no ctx); empty until the daemon has been observed ready.
func (p *Provisioner) Endpoint(key string) string {
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

// Reconcile runs one scale-to-zero pass: any project idle past IdleTimeout (no inflight build, last
// build older than the timeout) has its instance stopped, keeping the retained dataset. It is the
// single-host analogue of the controller's desiredReplicas=0 path; Run drives it on a ticker.
func (p *Provisioner) Reconcile(ctx context.Context) {
	now := time.Now()
	type cand struct {
		key, name string
	}
	var idle []cand
	p.mu.Lock()
	for key, st := range p.projects {
		if st.inflight == 0 && !st.lastBuild.IsZero() && now.Sub(st.lastBuild) > p.cfg.IdleTimeout {
			idle = append(idle, cand{key: key, name: router.DaemonName(key)})
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
