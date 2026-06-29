// Package local is the single-host backend of the buildd provisioner: it runs one vanilla buildkitd per
// project as an Incus instance backed by a retained ZFS dataset (the warm cache), reconciled by an
// in-process loop instead of a Kubernetes controller. It is the non-Kubernetes path described in
// docs/adr/0007 — for self-hosted / single-VM deployments where a cluster is overkill.
//
// host.go is the seam over the host's `incus` and `zfs` CLIs: an interface so the provisioner's logic is
// unit-tested with a fake, and a real implementation that shells out (the same exec-stub pattern as
// cmd/build). Keeping every host mutation behind this one interface is what makes the backend testable
// without an Incus host and keeps the "how we talk to the substrate" concern in one file.
package local

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

// InstanceSpec is the desired shape of one project's buildkitd instance.
type InstanceSpec struct {
	Name      string            // incus instance name, e.g. buildkitd-<key>
	Image     string            // image alias/remote, e.g. images:debian/12 (must provide buildkitd)
	VM        bool              // true = qemu VM (untrusted fork isolation, P3); false = system container
	Dataset   string            // ZFS dataset backing the warm cache, e.g. tank/bko/<key>
	MountPath string            // where Dataset is mounted inside the instance (buildkitd data dir)
	Config    map[string]string // extra incus config keys (e.g. user.* labels, security.* knobs)
}

// HostOps is everything the local provisioner does to the host. Every method is keyed by stable names
// derived from the project key, so the operations are idempotent and safe to retry.
type HostOps interface {
	// EnsureDataset creates the ZFS dataset (and parents) if absent. Retained across instance stops.
	EnsureDataset(ctx context.Context, dataset string) error
	// InstanceExists reports whether an instance of that name is defined (running or stopped).
	InstanceExists(ctx context.Context, name string) (bool, error)
	// Launch creates AND starts a new instance from spec (incus launch).
	Launch(ctx context.Context, spec InstanceSpec) error
	// Start starts an existing, stopped instance (scale-up from zero).
	Start(ctx context.Context, name string) error
	// Stop stops a running instance, keeping its dataset (scale-to-zero).
	Stop(ctx context.Context, name string) error
	// Running reports whether the instance is currently running.
	Running(ctx context.Context, name string) (bool, error)
	// IP returns the instance's primary IPv4 address (empty until it has one).
	IP(ctx context.Context, name string) (string, error)
	// Delete removes the instance (and, when purgeDataset, its dataset) — orphan GC / fork reap.
	Delete(ctx context.Context, name, dataset string, purgeDataset bool) error
}

// cli is the real HostOps: it shells out to `incus` and `zfs`. The exec seam mirrors cmd/build so tests
// elsewhere can stub the subprocess calls; here the unit tests use a fake HostOps instead.
type cli struct {
	incus string // path/name of the incus binary (default "incus")
	zfs   string // path/name of the zfs binary (default "zfs")
}

// NewCLI returns the production HostOps backed by the host's incus + zfs binaries.
func NewCLI() HostOps { return &cli{incus: "incus", zfs: "zfs"} }

var execCommandContext = exec.CommandContext // test seam

func (c *cli) run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := execCommandContext(ctx, name, args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return strings.TrimSpace(out.String()), nil
}

func (c *cli) EnsureDataset(ctx context.Context, dataset string) error {
	// `zfs create -p` is idempotent only via a prior existence check: creating an existing dataset errors.
	if _, err := c.run(ctx, c.zfs, "list", "-H", "-o", "name", dataset); err == nil {
		return nil
	}
	_, err := c.run(ctx, c.zfs, "create", "-p", dataset)
	return err
}

func (c *cli) InstanceExists(ctx context.Context, name string) (bool, error) {
	// `incus info <name>` exits non-zero when the instance is unknown; treat that as "absent".
	if _, err := c.run(ctx, c.incus, "info", name); err != nil {
		return false, nil
	}
	return true, nil
}

func (c *cli) Launch(ctx context.Context, spec InstanceSpec) error {
	args := []string{"launch", spec.Image, spec.Name}
	if spec.VM {
		args = append(args, "--vm")
	}
	for k, v := range spec.Config {
		args = append(args, "--config", k+"="+v)
	}
	// Attach the retained ZFS dataset as the warm-cache disk at the buildkitd data dir. `source=` a host
	// path binds the dataset's mountpoint; the instance keeps it across stops.
	if spec.Dataset != "" && spec.MountPath != "" {
		args = append(args, "--device", "cache,type=disk,source="+datasetMount(spec.Dataset)+",path="+spec.MountPath)
	}
	_, err := c.run(ctx, c.incus, args...)
	return err
}

func (c *cli) Start(ctx context.Context, name string) error {
	_, err := c.run(ctx, c.incus, "start", name)
	return err
}

func (c *cli) Stop(ctx context.Context, name string) error {
	_, err := c.run(ctx, c.incus, "stop", name)
	return err
}

func (c *cli) Running(ctx context.Context, name string) (bool, error) {
	out, err := c.run(ctx, c.incus, "list", name, "-c", "s", "-f", "csv")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(out), "RUNNING"), nil
}

func (c *cli) IP(ctx context.Context, name string) (string, error) {
	// `incus list <name> -c 4 -f csv` yields the IPv4 column, e.g. "10.0.0.5 (eth0)".
	out, err := c.run(ctx, c.incus, "list", name, "-c", "4", "-f", "csv")
	if err != nil {
		return "", err
	}
	field, _, _ := strings.Cut(strings.TrimSpace(out), " ")
	return field, nil
}

func (c *cli) Delete(ctx context.Context, name, dataset string, purgeDataset bool) error {
	if _, err := c.run(ctx, c.incus, "delete", name, "--force"); err != nil {
		return err
	}
	if purgeDataset && dataset != "" {
		if _, err := c.run(ctx, c.zfs, "destroy", "-r", dataset); err != nil {
			return err
		}
	}
	return nil
}

// datasetMount is the conventional mountpoint of a ZFS dataset (/<dataset>). Kept tiny + overridable in
// tests; production assumes the pool's default mountpoint scheme.
var datasetMount = func(dataset string) string { return "/" + dataset }
