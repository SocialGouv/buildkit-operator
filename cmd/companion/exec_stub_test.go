package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

// stubExec swaps the execCommandContext seam for a fake that re-execs this test binary into
// TestHelperProcess, which emits the given combined output and exit code. It restores the original
// via t.Cleanup. This is the stdlib "TestHelperProcess" pattern: it exercises the real *exec.Cmd
// plumbing (CombinedOutput, Run) without needing buildctl on PATH.
func stubExec(t *testing.T, output string, exitCode int) {
	t.Helper()
	orig := execCommandContext
	t.Cleanup(func() { execCommandContext = orig })
	execCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cs := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
		cmd := exec.CommandContext(ctx, os.Args[0], cs...)
		cmd.Env = append(os.Environ(),
			"GO_WANT_HELPER_PROCESS=1",
			"HELPER_OUTPUT="+output,
			"HELPER_EXIT="+strconv.Itoa(exitCode),
		)
		return cmd
	}
}

// TestHelperProcess is not a real test: it is the child process stubExec re-execs into. It only acts
// when GO_WANT_HELPER_PROCESS=1, otherwise it returns immediately so the normal suite ignores it.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	fmt.Fprint(os.Stdout, os.Getenv("HELPER_OUTPUT"))
	code, _ := strconv.Atoi(os.Getenv("HELPER_EXIT"))
	os.Exit(code)
}
