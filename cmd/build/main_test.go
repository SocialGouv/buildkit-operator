package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/socialgouv/buildkit-operator/internal/router"
)

func quietLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// --- pure helpers -----------------------------------------------------------

func TestBuildxBuildArgs(t *testing.T) {
	cfg := &config{
		dockerfile: "Dockerfile.prod",
		target:     "runtime",
		tags:       []string{"img:1", "img:latest"},
		push:       true,
		progress:   "plain",
		contextDir: "ctx",
	}
	got := cfg.buildxBuildArgs("bld", router.RouteResponse{})
	want := []string{
		"buildx", "build", "--builder", "bld",
		"--file", "Dockerfile.prod",
		"--target", "runtime",
		"--tag", "img:1", "--tag", "img:latest",
		"--push",
		"--progress", "plain",
		"ctx",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("buildxBuildArgs =\n %v\nwant\n %v", got, want)
	}
}

// --output is used when not pushing; --push and --output are mutually exclusive (run() enforces that).
func TestBuildxBuildArgs_Output(t *testing.T) {
	cfg := &config{output: "type=local,dest=out", contextDir: "."}
	got := cfg.buildxBuildArgs("bld", router.RouteResponse{})
	if !contains(got, "--output") || !contains(got, "type=local,dest=out") {
		t.Errorf("expected --output flags, got %v", got)
	}
	if contains(got, "--push") {
		t.Errorf("must not emit --push when only --output set: %v", got)
	}
}

// A cache reference on the route response appends --cache-from/--cache-to.
func TestBuildxBuildArgs_WithCache(t *testing.T) {
	cfg := &config{contextDir: "."}
	resp := router.RouteResponse{Cache: &router.CacheConfig{Type: "s3", Bucket: "b", Name: "n", Region: "gra"}}
	got := cfg.buildxBuildArgs("bld", resp)
	if !contains(got, "--cache-from") || !contains(got, "--cache-to") {
		t.Errorf("expected cache flags, got %v", got)
	}
}

func TestCacheArgs(t *testing.T) {
	tests := []struct {
		name string
		in   *router.CacheConfig
		want []string
	}{
		{"nil", nil, nil},
		{"wrong type", &router.CacheConfig{Type: "registry", Bucket: "b"}, nil},
		{"empty bucket", &router.CacheConfig{Type: "s3"}, nil},
		{
			"full",
			&router.CacheConfig{Type: "s3", Bucket: "b", Name: "n", Region: "gra", EndpointURL: "https://s3"},
			[]string{
				"--cache-from", "type=s3,bucket=b,name=n,region=gra,endpoint_url=https://s3,use_path_style=true",
				"--cache-to", "type=s3,bucket=b,name=n,region=gra,endpoint_url=https://s3,use_path_style=true,mode=max",
			},
		},
		{
			"minimal",
			&router.CacheConfig{Type: "s3", Bucket: "b", Name: "n"},
			[]string{"--cache-from", "type=s3,bucket=b,name=n", "--cache-to", "type=s3,bucket=b,name=n,mode=max"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := cacheArgs(tt.in); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("cacheArgs = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveRepo(t *testing.T) {
	// Explicit --repo wins.
	cfg := &config{repo: "explicit/repo"}
	if got := cfg.resolveRepo(quietLogger()); got != "explicit/repo" {
		t.Errorf("explicit: %q", got)
	}

	// No repo, no git: falls back to the context basename.
	stubExec(t, map[string]string{"HP_GIT_EXIT": "1"}) // git fails -> no remote
	cfg = &config{contextDir: "/tmp/some-project"}
	if got := cfg.resolveRepo(quietLogger()); got != "some-project" {
		t.Errorf("basename fallback: %q", got)
	}

	// No repo but git remote present.
	stubExec(t, map[string]string{"HP_GIT_OUT": "git@github.com:org/repo.git\n"})
	cfg = &config{contextDir: "."}
	if got := cfg.resolveRepo(quietLogger()); got != "git@github.com:org/repo.git" {
		t.Errorf("git remote: %q", got)
	}
}

func TestEnvHelpers(t *testing.T) {
	t.Setenv("BKO_X", "v")
	if envOr("BKO_X", "def") != "v" {
		t.Error("envOr set")
	}
	if envOr("BKO_UNSET_XYZ", "def") != "def" {
		t.Error("envOr unset")
	}
	for _, truthy := range []string{"1", "true", "YES"} {
		t.Setenv("BKO_B", truthy)
		if !envBool("BKO_B") {
			t.Errorf("envBool(%q) = false", truthy)
		}
	}
	t.Setenv("BKO_B", "nope")
	if envBool("BKO_B") {
		t.Error("envBool(nope) = true")
	}
	t.Setenv("BKO_D", "2s")
	if durationOr("BKO_D", time.Minute) != 2*time.Second {
		t.Error("durationOr valid")
	}
	t.Setenv("BKO_D", "bad")
	if durationOr("BKO_D", time.Minute) != time.Minute {
		t.Error("durationOr fallback")
	}
}

func TestDefaultArchAndCertsDir(t *testing.T) {
	if a := defaultArch(); a != "amd64" && a != "arm64" {
		t.Errorf("defaultArch = %q, want amd64|arm64", a)
	}
	if d := defaultCertsDir(); !strings.HasSuffix(d, filepath.Join(".buildkit-operator", "certs")) {
		t.Errorf("defaultCertsDir = %q", d)
	}
	// HOME unset: fall back to the relative path (UserHomeDir errors).
	t.Setenv("HOME", "")
	if d := defaultCertsDir(); d != filepath.Join(".buildkit-operator", "certs") {
		t.Errorf("defaultCertsDir (no HOME) = %q, want relative fallback", d)
	}
}

func TestIsExistingBuilder(t *testing.T) {
	for _, s := range []string{"existing instance", "ERROR: already exists", "Existing Instance foo"} {
		if !isExistingBuilder(s) {
			t.Errorf("isExistingBuilder(%q) = false", s)
		}
	}
	if isExistingBuilder("connection refused") {
		t.Error("isExistingBuilder(connection refused) = true")
	}
}

// --- HTTP: routeBuild / completeBuild --------------------------------------

func TestRouteBuild_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/route" {
			t.Errorf("path = %q, want /route", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer token: %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(router.RouteResponse{Key: "p1", Endpoint: "tcp://d:1234", Namespace: "ns"})
	}))
	defer srv.Close()

	cfg := &config{builddURL: srv.URL, token: "tok", routeWait: 5 * time.Second}
	resp, err := routeBuild(t.Context(), cfg, router.RouteRequest{Repo: "r", Arch: "amd64"})
	if err != nil {
		t.Fatalf("routeBuild: %v", err)
	}
	if resp.Key != "p1" || resp.Endpoint != "tcp://d:1234" {
		t.Errorf("resp = %+v", resp)
	}
}

func TestRouteBuild_HTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "no route", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	cfg := &config{builddURL: srv.URL, routeWait: 5 * time.Second}
	_, err := routeBuild(t.Context(), cfg, router.RouteRequest{Repo: "r", Arch: "amd64"})
	if err == nil || !strings.Contains(err.Error(), "no route") {
		t.Errorf("want error carrying the body, got %v", err)
	}
}

func TestRouteBuild_EmptyEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(router.RouteResponse{Key: "p1"}) // no endpoint
	}))
	defer srv.Close()

	cfg := &config{builddURL: srv.URL, routeWait: 5 * time.Second}
	if _, err := routeBuild(t.Context(), cfg, router.RouteRequest{Repo: "r", Arch: "amd64"}); err == nil {
		t.Error("want error on empty endpoint")
	}
}

func TestCompleteBuild_PostsKey(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		got <- body["key"]
	}))
	defer srv.Close()

	cfg := &config{builddURL: srv.URL, token: "tok"}
	completeBuild(cfg, "p1", quietLogger()) // best-effort, never panics
	select {
	case k := <-got:
		if k != "p1" {
			t.Errorf("posted key = %q, want p1", k)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("completeBuild did not POST /complete")
	}
}

// completeBuild swallows transport errors (unreachable buildd) — it must not panic.
func TestCompleteBuild_UnreachableIsSilent(t *testing.T) {
	cfg := &config{builddURL: "http://127.0.0.1:1"}
	completeBuild(cfg, "p1", quietLogger())
}

// --- exec-backed: git / buildx ---------------------------------------------

func TestGitRemoteURL(t *testing.T) {
	stubExec(t, map[string]string{"HP_GIT_OUT": "  https://github.com/org/repo.git\n"})
	if got := gitRemoteURL("."); got != "https://github.com/org/repo.git" {
		t.Errorf("gitRemoteURL = %q", got)
	}
	stubExec(t, map[string]string{"HP_GIT_EXIT": "1"})
	if got := gitRemoteURL("."); got != "" {
		t.Errorf("gitRemoteURL on failure = %q, want empty", got)
	}
}

func TestBuilderHasEndpoint(t *testing.T) {
	stubExec(t, map[string]string{"HP_INSPECT_OUT": "Endpoints:\n tcp://d:1234\n"})
	if !builderHasEndpoint(t.Context(), "bld", "tcp://d:1234") {
		t.Error("builderHasEndpoint = false, want true when inspect output contains endpoint")
	}
	stubExec(t, map[string]string{"HP_INSPECT_EXIT": "1"})
	if builderHasEndpoint(t.Context(), "bld", "tcp://d:1234") {
		t.Error("builderHasEndpoint = true, want false when inspect fails")
	}
}

func TestEnsureBuilder_CreatesFresh(t *testing.T) {
	stubExec(t, nil) // create exits 0
	if err := ensureBuilder(t.Context(), "bld", "/certs", "tcp://d:1234", quietLogger()); err != nil {
		t.Errorf("ensureBuilder: %v", err)
	}
}

func TestEnsureBuilder_CreateFailsHard(t *testing.T) {
	stubExec(t, map[string]string{"HP_CREATE_EXIT": "1", "HP_CREATE_STDERR": "connection refused"})
	err := ensureBuilder(t.Context(), "bld", "/certs", "tcp://d:1234", quietLogger())
	if err == nil || !strings.Contains(err.Error(), "buildx create failed") {
		t.Errorf("want create error, got %v", err)
	}
}

// Builder already exists and its endpoint still matches => idempotent success, no recreate.
func TestEnsureBuilder_ExistingSameEndpoint(t *testing.T) {
	stubExec(t, map[string]string{
		"HP_CREATE_EXIT":   "1",
		"HP_CREATE_STDERR": "existing instance",
		"HP_INSPECT_OUT":   "tcp://d:1234",
	})
	if err := ensureBuilder(t.Context(), "bld", "/certs", "tcp://d:1234", quietLogger()); err != nil {
		t.Errorf("ensureBuilder idempotent: %v", err)
	}
}

// Builder exists but its endpoint changed => rm + recreate (the recreate create exits 0 here).
func TestEnsureBuilder_ExistingEndpointChanged(t *testing.T) {
	stubExec(t, map[string]string{
		"HP_CREATE_EXIT":   "1", // NOTE: both create attempts use this; see below
		"HP_CREATE_STDERR": "existing instance",
		"HP_INSPECT_OUT":   "tcp://OLD:1",
	})
	// With the recreate create also failing "existing instance", ensureBuilder returns the recreate
	// error. Assert it surfaced the recreate path rather than the idempotent one.
	err := ensureBuilder(t.Context(), "bld", "/certs", "tcp://NEW:2", quietLogger())
	if err == nil || !strings.Contains(err.Error(), "recreate failed") {
		t.Errorf("want recreate error, got %v", err)
	}
}

func TestRunStreaming(t *testing.T) {
	stubExec(t, nil)
	if err := runStreaming(t.Context(), "docker", []string{"buildx", "build", "."}); err != nil {
		t.Errorf("runStreaming success: %v", err)
	}
	stubExec(t, map[string]string{"HP_EXIT": "2"})
	if err := runStreaming(t.Context(), "docker", []string{"buildx", "build", "."}); err == nil {
		t.Error("runStreaming want error on non-zero exit")
	}
}

// --- run() pipeline (dry-run avoids docker) --------------------------------

func TestRun_DryRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(router.RouteResponse{Key: "p1", Endpoint: "tcp://d:1234", Namespace: "ns"})
	}))
	defer srv.Close()

	cfg := &config{
		repo: "github.com/org/repo", arch: "amd64", contextDir: ".",
		builddURL: srv.URL, dryRun: true, routeWait: 5 * time.Second,
	}
	if err := run(t.Context(), cfg); err != nil {
		t.Errorf("run dry-run: %v", err)
	}
}

func TestRun_ValidationErrors(t *testing.T) {
	t.Run("push and output conflict", func(t *testing.T) {
		cfg := &config{push: true, output: "type=local", repo: "r", arch: "amd64"}
		if err := run(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
			t.Errorf("want mutual-exclusion error, got %v", err)
		}
	})
	t.Run("bad arch", func(t *testing.T) {
		cfg := &config{repo: "github.com/o/r", arch: "riscv"}
		if err := run(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "unsupported --arch") {
			t.Errorf("want arch error, got %v", err)
		}
	})
	t.Run("unresolvable repo", func(t *testing.T) {
		stubExec(t, map[string]string{"HP_GIT_EXIT": "1"}) // no git remote
		cfg := &config{arch: "amd64", contextDir: "/"}     // abs basename "/" => empty repo
		if err := run(t.Context(), cfg); err == nil || !strings.Contains(err.Error(), "could not resolve a repository") {
			t.Errorf("want repo-resolution error, got %v", err)
		}
	})
}

// newRootCmd wires the flags; --dry-run drives the whole command without docker. Asserts the cobra
// surface executes end-to-end against a stub buildd.
func TestNewRootCmd_DryRunExecute(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(router.RouteResponse{Key: "p1", Endpoint: "tcp://d:1234", Namespace: "ns"})
	}))
	defer srv.Close()

	cmd := newRootCmd()
	cmd.SetArgs([]string{"--repo", "github.com/o/r", "--arch", "amd64", "--buildd-url", srv.URL, "--dry-run", "."})
	if err := cmd.Execute(); err != nil {
		t.Errorf("Execute --dry-run: %v", err)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
