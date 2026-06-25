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
