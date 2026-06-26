package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
)

// stubExec swaps both exec seams for a fake that re-execs this test binary into TestHelperProcess.
// The child branches on the subcommand (git / buildx create|inspect|rm) so a single stub can drive
// the multi-call ensureBuilder flow. env carries per-subcommand output/exit overrides; anything unset
// defaults to empty output and exit 0. The originals are restored via t.Cleanup.
func stubExec(t *testing.T, env map[string]string) {
	t.Helper()
	origCmd, origCtx := execCommand, execCommandContext
	t.Cleanup(func() { execCommand, execCommandContext = origCmd, origCtx })

	childEnv := append(os.Environ(), "GO_WANT_HELPER_PROCESS=1")
	for k, v := range env {
		childEnv = append(childEnv, k+"="+v)
	}
	build := func(name string, args []string) *exec.Cmd {
		cs := append([]string{"-test.run=TestHelperProcess", "--", name}, args...)
		c := exec.Command(os.Args[0], cs...)
		c.Env = childEnv
		return c
	}
	execCommand = func(name string, args ...string) *exec.Cmd { return build(name, args) }
	execCommandContext = func(_ context.Context, name string, args ...string) *exec.Cmd { return build(name, args) }
}

func helperEnvInt(key string) int { n, _ := strconv.Atoi(os.Getenv(key)); return n }

// TestHelperProcess is the child stubExec re-execs into. It only acts under GO_WANT_HELPER_PROCESS=1.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	name := ""
	if len(args) > 0 {
		name = args[0]
	}
	joined := strings.Join(args, " ")

	switch {
	case name == "git":
		fmt.Fprint(os.Stdout, os.Getenv("HP_GIT_OUT"))
		os.Exit(helperEnvInt("HP_GIT_EXIT"))
	case strings.Contains(joined, "buildx create"):
		fmt.Fprint(os.Stderr, os.Getenv("HP_CREATE_STDERR"))
		os.Exit(helperEnvInt("HP_CREATE_EXIT"))
	case strings.Contains(joined, "buildx inspect"):
		fmt.Fprint(os.Stdout, os.Getenv("HP_INSPECT_OUT"))
		os.Exit(helperEnvInt("HP_INSPECT_EXIT"))
	case strings.Contains(joined, "buildx rm"):
		os.Exit(0)
	default:
		fmt.Fprint(os.Stdout, os.Getenv("HP_OUT"))
		os.Exit(helperEnvInt("HP_EXIT"))
	}
}
