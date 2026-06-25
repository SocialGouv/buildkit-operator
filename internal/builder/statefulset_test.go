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
