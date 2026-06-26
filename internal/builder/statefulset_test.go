package builder

import (
	"testing"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/router"
	corev1 "k8s.io/api/core/v1"
)

// The sandboxed runtime is applied to UNTRUSTED fork daemons only — trusted builds keep the default
// runtime (runc) for speed; and when no sandbox runtime is configured, nobody gets one.
func TestStatefulSet_SandboxRuntimeForForksOnly(t *testing.T) {
	canonical := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	fork := router.ForkKey(canonical)

	get := func(key, sandbox string) *string {
		bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Key: key, Arch: "amd64"}}
		return StatefulSet(bp, Config{Namespace: "ns", Port: 1234, SandboxRuntimeClass: sandbox}).
			Spec.Template.Spec.RuntimeClassName
	}

	if rc := get(fork, "sysbox-runc"); rc == nil || *rc != "sysbox-runc" {
		t.Errorf("fork daemon RuntimeClassName = %v, want sysbox-runc", rc)
	}
	if rc := get(canonical, "sysbox-runc"); rc != nil {
		t.Errorf("trusted daemon RuntimeClassName = %v, want nil (default runtime)", rc)
	}
	if rc := get(fork, ""); rc != nil {
		t.Errorf("no sandbox configured: RuntimeClassName = %v, want nil", rc)
	}
}

// Fork daemon pods carry the untrusted=true marker (so the fork-egress NetworkPolicy can select them);
// trusted daemons do not. The marker is on the pod template only, never on the selector.
func TestStatefulSet_UntrustedPodLabel(t *testing.T) {
	canonical := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	fork := router.ForkKey(canonical)

	sts := func(key string) map[string]string {
		bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Key: key, Arch: "amd64"}}
		return StatefulSet(bp, Config{Namespace: "ns", Port: 1234}).Spec.Template.ObjectMeta.Labels
	}

	if got := sts(fork)[LabelUntrusted]; got != "true" {
		t.Errorf("fork pod %s = %q, want true", LabelUntrusted, got)
	}
	if _, ok := sts(canonical)[LabelUntrusted]; ok {
		t.Errorf("trusted pod must not carry %s", LabelUntrusted)
	}
	// The selector must stay clean of the marker (it would otherwise break addressing).
	bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Key: fork, Arch: "amd64"}}
	if _, ok := StatefulSet(bp, Config{Namespace: "ns", Port: 1234}).Spec.Selector.MatchLabels[LabelUntrusted]; ok {
		t.Errorf("selector must not carry %s", LabelUntrusted)
	}
}

func TestStatefulSet_S3CredsAreTrustedOnly(t *testing.T) {
	canonical := router.ProjectKey("github.com/org/repo", "", "", "amd64")
	fork := router.ForkKey(canonical)
	cfg := Config{Namespace: "ns", Port: 1234, S3CredsSecret: "buildkit-s3"}

	envFrom := func(key string) []corev1.EnvFromSource {
		bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Key: key, Arch: "amd64"}}
		return StatefulSet(bp, cfg).Spec.Template.Spec.Containers[0].EnvFrom
	}

	trusted := envFrom(canonical)
	if len(trusted) != 1 || trusted[0].SecretRef == nil || trusted[0].SecretRef.Name != "buildkit-s3" {
		t.Fatalf("trusted daemon EnvFrom = %#v, want the configured S3 Secret", trusted)
	}
	if got := envFrom(fork); len(got) != 0 {
		t.Fatalf("fork daemon EnvFrom = %#v, want no S3 credentials", got)
	}
}

// Regression (B3): in the privileged profile the daemon socket lives under /run/buildkit, so the
// shared `run` emptyDir must be mounted at that SAME path in both the daemon and the companion —
// otherwise the companion's buildctl probe can't reach the socket and /readyz never goes ready.
func TestStatefulSet_PrivilegedCompanionSharesSocketDir(t *testing.T) {
	bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Key: "p1", Arch: "amd64", SecurityProfile: bkov1.ProfilePrivileged}}
	sts := StatefulSet(bp, Config{Namespace: "ns", Port: 1234, HealthPort: 8080, Companion: true})
	containers := sts.Spec.Template.Spec.Containers
	if len(containers) != 2 {
		t.Fatalf("want daemon + companion, got %d containers", len(containers))
	}
	runPath := func(c corev1.Container) string {
		for _, m := range c.VolumeMounts {
			if m.Name == "run" {
				return m.MountPath
			}
		}
		return ""
	}
	daemonRun, companionRun := runPath(containers[0]), runPath(containers[1])
	if daemonRun != "/run/buildkit" || companionRun != "/run/buildkit" {
		t.Errorf("run mount: daemon=%q companion=%q, want both /run/buildkit", daemonRun, companionRun)
	}
	// The companion must probe the privileged socket that lives under the shared run dir.
	var addr string
	for _, a := range containers[1].Args {
		if len(a) > len("--buildkit-addr=") && a[:len("--buildkit-addr=")] == "--buildkit-addr=" {
			addr = a[len("--buildkit-addr="):]
		}
	}
	if addr != "unix:///run/buildkit/buildkitd.sock" {
		t.Errorf("companion --buildkit-addr = %q, want the privileged socket under /run/buildkit", addr)
	}
}

// Daemon pods carry the configured nodeSelector/tolerations (to pin them to a dedicated build
// nodepool); with nothing configured they carry none. Parsing uses the JSON the chart renders.
func TestStatefulSet_DaemonScheduling(t *testing.T) {
	var cfg Config
	js := `{"nodeSelector":{"nodepool":"buildkit"},"tolerations":[{"key":"workload","operator":"Equal","value":"buildkit","effect":"NoSchedule"}]}`
	if err := cfg.SchedulingFromJSON(js); err != nil {
		t.Fatalf("SchedulingFromJSON: %v", err)
	}
	cfg.Namespace, cfg.Port = "ns", 1234
	bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Key: "p1", Arch: "amd64"}}
	spec := StatefulSet(bp, cfg).Spec.Template.Spec
	if spec.NodeSelector["nodepool"] != "buildkit" {
		t.Errorf("nodeSelector = %v, want nodepool=buildkit", spec.NodeSelector)
	}
	if len(spec.Tolerations) != 1 || spec.Tolerations[0].Key != "workload" || spec.Tolerations[0].Effect != corev1.TaintEffectNoSchedule {
		t.Errorf("tolerations = %v, want one workload:NoSchedule", spec.Tolerations)
	}

	// Empty/unset -> no-op, daemons schedule anywhere.
	if err := (&Config{}).SchedulingFromJSON(""); err != nil {
		t.Fatalf(`SchedulingFromJSON(""): %v`, err)
	}
	none := StatefulSet(bp, Config{Namespace: "ns", Port: 1234}).Spec.Template.Spec
	if none.NodeSelector != nil || none.Tolerations != nil || none.Affinity != nil {
		t.Errorf("unconfigured daemon must carry no scheduling")
	}
}

// A fork under a sandbox runtime runs the NON-rootless image PRIVILEGED (the microVM is the boundary);
// the rootless image's newuidmap can't run in the guest. Trusted/canonical daemons keep rootless.
func TestStatefulSet_SandboxedForkPrivilegedNonRootless(t *testing.T) {
	fork := router.ForkKey(router.ProjectKey("github.com/org/repo", "", "", "amd64"))
	cfg := Config{Namespace: "ns", Port: 1234, BuildkitImage: "moby/buildkit:v0.31.1-rootless", SandboxRuntimeClass: "kata-clh"}
	ctr := func(key string) corev1.Container {
		bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Key: key, Arch: "amd64", SecurityProfile: bkov1.ProfileRootless}}
		return StatefulSet(bp, cfg).Spec.Template.Spec.Containers[0]
	}
	f := ctr(fork)
	if f.Image != "moby/buildkit:v0.31.1" {
		t.Errorf("sandboxed fork image = %q, want derived non-rootless moby/buildkit:v0.31.1", f.Image)
	}
	if f.SecurityContext == nil || f.SecurityContext.Privileged == nil || !*f.SecurityContext.Privileged {
		t.Errorf("sandboxed fork must be privileged, got %+v", f.SecurityContext)
	}
	// Canonical (trusted) stays rootless even when a sandbox runtime is configured.
	c := ctr("p1")
	if c.Image != "moby/buildkit:v0.31.1-rootless" {
		t.Errorf("canonical image = %q, want rootless", c.Image)
	}
	if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
		t.Errorf("canonical must NOT be privileged")
	}
	// Explicit override wins over the derive.
	cfg.SandboxBuildkitImage = "ghcr.io/acme/bk:pinned"
	if got := ctr(fork).Image; got != "ghcr.io/acme/bk:pinned" {
		t.Errorf("SandboxBuildkitImage override = %q, want ghcr.io/acme/bk:pinned", got)
	}
	// The companion inode-GC backstop is skipped for ephemeral sandboxed forks, kept for canonical.
	cfg.Companion = true
	count := func(key string) int {
		bp := &bkov1.BuildProject{Spec: bkov1.BuildProjectSpec{Key: key, Arch: "amd64"}}
		return len(StatefulSet(bp, cfg).Spec.Template.Spec.Containers)
	}
	if count(fork) != 1 {
		t.Errorf("sandboxed fork containers = %d, want 1 (no companion)", count(fork))
	}
	if count("p1") != 2 {
		t.Errorf("canonical containers = %d, want 2 (buildkitd + companion)", count("p1"))
	}
}
