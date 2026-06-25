package builder

import (
	"testing"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/router"
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
