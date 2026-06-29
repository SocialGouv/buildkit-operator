package local

import (
	"context"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// dockerHost is a HostOps backed by Docker + host directories — a dev / local runtime that needs no
// Incus or ZFS (vs the production cli backend). The per-project "dataset" is a host directory
// bind-mounted into a buildkitd container; snapshots/clones are directory copies (instant CoW via
// `cp --reflink=auto` on btrfs/xfs, a full copy elsewhere). It has NO VM isolation, so untrusted forks
// are not supported here — use the Incus runtime for those.
type dockerHost struct {
	docker string // docker binary
	port   int32  // buildkitd port inside the container
	certs  string // host certs dir; empty = serve plaintext tcp (dev only)
}

// NewDocker returns the Docker-backed HostOps. certsHostPath empty = buildkitd serves plaintext (dev).
func NewDocker(port int32, certsHostPath string) HostOps {
	return &dockerHost{docker: "docker", port: port, certs: certsHostPath}
}

func (d *dockerHost) run(ctx context.Context, args ...string) (string, error) {
	return (&cli{}).run(ctx, d.docker, args...) // reuse the exec seam + error wrapping
}

func (d *dockerHost) EnsureDataset(_ context.Context, dataset string) error {
	return os.MkdirAll(dataset, 0o755)
}

func (d *dockerHost) InstanceExists(ctx context.Context, name string) (bool, error) {
	if _, err := d.run(ctx, "inspect", "--type", "container", name); err != nil {
		return false, nil
	}
	return true, nil
}

// portFor maps a project key to a deterministic loopback host port. The container's bridge IP is not
// relied upon (it may be unassigned depending on the host's Docker network config); instead buildkitd is
// published to 127.0.0.1:<portFor(key)>, which works regardless of bridge networking.
func portFor(key string) int { return 20000 + int(crc32.ChecksumIEEE([]byte(key))%20000) }

// Addr returns the deterministic dial address host:port for a key (loopback + published port). The
// provisioner uses this (via the addrResolver seam) instead of the container IP.
func (d *dockerHost) Addr(key string) string { return fmt.Sprintf("127.0.0.1:%d", portFor(key)) }

func (d *dockerHost) Launch(ctx context.Context, spec InstanceSpec) error {
	args := []string{
		"run", "-d", "--name", spec.Name,
		"--label", labelKey + "=" + spec.Config[labelKey],
		"--privileged", "--restart=no",
		"-p", fmt.Sprintf("127.0.0.1:%d:%d", portFor(spec.Config[labelKey]), d.port),
		"-v", spec.Dataset + ":" + spec.MountPath,
	}
	bkArgs := []string{"--addr", fmt.Sprintf("tcp://0.0.0.0:%d", d.port), "--root", spec.MountPath}
	if d.certs != "" {
		args = append(args, "-v", d.certs+":/certs:ro")
		bkArgs = append(bkArgs, "--tlscacert", "/certs/ca.pem", "--tlscert", "/certs/cert.pem", "--tlskey", "/certs/key.pem")
	}
	args = append(args, spec.Image)
	args = append(args, bkArgs...)
	_, err := d.run(ctx, args...)
	return err
}

func (d *dockerHost) Start(ctx context.Context, name string) error {
	_, err := d.run(ctx, "start", name)
	return err
}

func (d *dockerHost) Stop(ctx context.Context, name string) error {
	_, err := d.run(ctx, "stop", name)
	return err
}

func (d *dockerHost) Running(ctx context.Context, name string) (bool, error) {
	out, err := d.run(ctx, "inspect", "-f", "{{.State.Running}}", name)
	if err != nil {
		return false, nil
	}
	return strings.TrimSpace(out) == "true", nil
}

func (d *dockerHost) IP(ctx context.Context, name string) (string, error) {
	// The container's bridge IP is reachable from the host on Linux — the address clients dial.
	out, err := d.run(ctx, "inspect", "-f", "{{.NetworkSettings.IPAddress}}", name)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (d *dockerHost) Delete(ctx context.Context, name, dataset string, purgeDataset bool) error {
	if _, err := d.run(ctx, "rm", "-f", name); err != nil {
		return err
	}
	if purgeDataset && dataset != "" {
		return os.RemoveAll(dataset)
	}
	return nil
}

// snapDir is the on-disk location of a directory "snapshot": a sibling <dataset>@<snap>.
func (d *dockerHost) Snapshot(ctx context.Context, dataset, snap string) error {
	dst := dataset + "@" + snap
	// --reflink=auto: instant CoW on btrfs/xfs, a full copy on ext4 — correct either way. (Durable
	// snapshots here need buildd to read the daemon's data dir; run buildd as root, or use the Incus+ZFS
	// runtime whose snapshots are atomic at the kernel layer. On failure, don't leave a partial behind.)
	if _, err := (&cli{}).run(ctx, "cp", "--reflink=auto", "-a", dataset, dst); err != nil {
		_ = os.RemoveAll(dst)
		return err
	}
	return nil
}

func (d *dockerHost) ListSnapshots(_ context.Context, dataset string) ([]string, error) {
	matches, err := filepath.Glob(dataset + "@*")
	if err != nil {
		return nil, err
	}
	sort.Strings(matches) // bko-<unixsecs> names sort chronologically
	return matches, nil
}

func (d *dockerHost) DestroySnapshot(_ context.Context, snapshot string) error {
	return os.RemoveAll(snapshot)
}

func (d *dockerHost) Clone(ctx context.Context, snapshot, dataset string) error {
	_, err := (&cli{}).run(ctx, "cp", "--reflink=auto", "-a", snapshot, dataset)
	return err
}

// ApplyEgress is a no-op for the Docker dev runtime (no per-instance egress ACLs).
func (d *dockerHost) ApplyEgress(_ context.Context, _ string, _ bool) error { return nil }
