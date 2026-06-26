package main

import "os/exec"

// execCommandContext is a seam over os/exec so tests can stub the buildctl
// subprocess calls (worker probe, cache prune) without a real binary on PATH.
// Production code wires it to the stdlib; tests swap it and restore via defer.
var execCommandContext = exec.CommandContext
