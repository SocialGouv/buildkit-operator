package local

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
)

// TestHelperProcess is the canned subprocess the exec seam delegates to: it prints HELPER_STDOUT and
// exits with HELPER_EXIT, so tests can model incus/zfs output without those binaries.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	fmt.Fprint(os.Stdout, os.Getenv("HELPER_STDOUT"))
	if code := os.Getenv("HELPER_EXIT"); code == "1" {
		os.Exit(1)
	}
	os.Exit(0)
}

// stubExec swaps the exec seam so cli.run executes TestHelperProcess instead of the real binary. The
// supplied assert fn sees the (name, args) actually requested, so argument construction is verified.
func stubExec(t *testing.T, stdout string, exit bool, assert func(name string, args []string)) {
	t.Helper()
	orig := execCommandContext
	t.Cleanup(func() { execCommandContext = orig })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if assert != nil {
			assert(name, args)
		}
		cs := append([]string{"-test.run=TestHelperProcess", "--"}, append([]string{name}, args...)...)
		cmd := exec.CommandContext(ctx, os.Args[0], cs...)
		env := append(os.Environ(), "GO_WANT_HELPER_PROCESS=1", "HELPER_STDOUT="+stdout)
		if exit {
			env = append(env, "HELPER_EXIT=1")
		}
		cmd.Env = env
		return cmd
	}
}

func TestCLI_IPParsesIPv4Column(t *testing.T) {
	stubExec(t, "10.0.0.5 (eth0)", false, func(name string, args []string) {
		if name != "incus" || args[0] != "list" {
			t.Errorf("unexpected call: %s %v", name, args)
		}
	})
	ip, err := NewCLI().IP(context.Background(), "buildkitd-p1")
	if err != nil {
		t.Fatal(err)
	}
	if ip != "10.0.0.5" {
		t.Errorf("IP = %q, want 10.0.0.5", ip)
	}
}

func TestCLI_RunningParsesState(t *testing.T) {
	stubExec(t, "RUNNING", false, nil)
	got, err := NewCLI().Running(context.Background(), "buildkitd-p1")
	if err != nil {
		t.Fatal(err)
	}
	if !got {
		t.Error("Running = false, want true for RUNNING")
	}
}

func TestCLI_InstanceExistsFromInfoExit(t *testing.T) {
	// `incus info` exits non-zero for an unknown instance => absent (no error).
	stubExec(t, "", true, nil)
	exists, err := NewCLI().InstanceExists(context.Background(), "missing")
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Error("InstanceExists = true on non-zero info exit, want false")
	}
}

func TestCLI_LaunchInitsAddsDevicesStarts(t *testing.T) {
	var calls []string
	stubExec(t, "", false, func(_ string, args []string) { calls = append(calls, strings.Join(args, " ")) })
	err := NewCLI().Launch(context.Background(), InstanceSpec{
		Name: "buildkitd-p1", Image: "vmimg", VM: true,
		Dataset: "tank/bko/p1", MountPath: "/data", CertsHostPath: "/etc/bko/certs",
		Config: map[string]string{"user.buildkit-operator.key": "p1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	all := strings.Join(calls, " | ")
	for _, want := range []string{
		"init vmimg buildkitd-p1",
		"--vm",
		"--config user.buildkit-operator.key=p1",
		"config device add buildkitd-p1 cache disk source=/tank/bko/p1 path=/data",
		"config device add buildkitd-p1 certs disk source=/etc/bko/certs path=/certs readonly=true",
		"start buildkitd-p1",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("launch calls %q missing %q", all, want)
		}
	}
}

func TestCLI_EnsureDatasetIdempotentWhenExists(t *testing.T) {
	// `zfs list` succeeds => dataset exists => no `zfs create` issued.
	var calls int
	stubExec(t, "tank/bko/p1", false, func(_ string, args []string) {
		calls++
		if args[0] == "create" {
			t.Errorf("unexpected zfs create for an existing dataset")
		}
	})
	if err := NewCLI().EnsureDataset(context.Background(), "tank/bko/p1"); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (list only)", calls)
	}
}
