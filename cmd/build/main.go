// Command build is the buildkit-operator client CLI / CI entrypoint: a thin, drop-in-ish
// `docker build` that routes every build to the one hot vanilla buildkitd that
// serves its (project, arch).
//
// The whole point of buildkit-operator is cache sharing: concurrent builds of the same
// project must hit the same daemon so they share its layer cache and its
// `RUN --mount=type=cache` mounts. That only works if every build resolves to
// the *same* project key. So this CLI does NOT invent its own key scheme — it
// delegates to internal/router (NormalizeRepo / ProjectKey / DaemonName), the
// single source of truth that buildd also uses, and asks buildd to route:
//
//  1. RESOLVE inputs (repo from --repo or `git remote.origin.url`, arch from
//     runtime.GOARCH, context/file/tags/...).
//  2. ROUTE: POST router.RouteRequest to <buildd-url>/route and decode
//     router.RouteResponse{Key, Endpoint, Namespace} — the mTLS address of the
//     daemon's Service.
//  3. BUILD via the buildx `remote` driver: create an idempotent
//     `buildkit-operator-<key>` builder pointed at the endpoint (with the client mTLS
//     material), then `docker buildx build ...` streaming progress straight
//     through.
//
// It shells out to `docker buildx` (assumed on PATH) and `git` (best-effort for
// repo detection) and otherwise uses only the Go stdlib plus cobra.
//
// Environment variables (every flag has an env fallback so --help shows the
// effective value):
//
//	BUILDKIT_OPERATOR_BUILDD_URL     base URL of buildd          (default http://buildd.buildkit-operator.svc:8080)
//	BUILDKIT_OPERATOR_CLIENT_CERTS   dir with ca.pem/cert.pem/key.pem for the remote driver mTLS
//	                        (default $HOME/.buildkit-operator/certs)
//	BUILDKIT_OPERATOR_NAME           optional monorepo component (segments the cache identity)
//
// The S3 cold cache is a PROJECT policy, not a client concern: when buildd has a bucket configured
// it returns the per-project cache reference on /route and this CLI applies it automatically — no S3
// env or credentials on the client side (the daemons hold the AWS creds via buildd --s3-creds-secret).
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/socialgouv/buildkit-operator/internal/router"
)

// config holds the resolved CLI configuration. Cobra defaults are env-aware
// (via envOr) so `--help` shows the value that will actually be used.
type config struct {
	repo        string
	name        string
	target      string
	arch        string
	contextDir  string
	dockerfile  string
	tags        []string
	push        bool
	output      string
	progress    string
	builddURL   string
	clientCerts string
	dryRun      bool
	untrusted   bool
	token       string
	routeWait   time.Duration
}

func main() {
	if err := newRootCmd().Execute(); err != nil {
		slog.Error("build failed", "err", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	cfg := &config{}

	cmd := &cobra.Command{
		Use:   "build [flags] [CONTEXT]",
		Short: "buildkit-operator client: route to the project's hot buildkitd and build via buildx remote",
		Long: "build resolves the project key for (repo, target, arch), asks buildd to route it to " +
			"the one hot vanilla buildkitd serving that project, then runs `docker buildx build` " +
			"against that daemon so concurrent builds share its layer and cache-mount caches.",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			// A positional CONTEXT overrides --context, matching `docker build`.
			if len(args) == 1 {
				cfg.contextDir = args[0]
			}
			return run(cmd.Context(), cfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.repo, "repo", os.Getenv("BUILDKIT_OPERATOR_REPO"),
		"source repository identity (empty = derive from git remote.origin.url, then context basename)")
	f.StringVar(&cfg.name, "name", os.Getenv("BUILDKIT_OPERATOR_NAME"),
		"optional monorepo component (image/path) — segments the cache identity so each image gets its own daemon")
	f.StringVar(&cfg.target, "target", os.Getenv("BUILDKIT_OPERATOR_TARGET"),
		"Dockerfile target stage (also part of the cache identity)")
	f.StringVar(&cfg.arch, "arch", envOr("BUILDKIT_OPERATOR_ARCH", defaultArch()),
		"build architecture: amd64 | arm64 (default mapped from runtime.GOARCH)")
	f.StringVarP(&cfg.contextDir, "context", "c", ".",
		"build context directory")
	f.StringVarP(&cfg.dockerfile, "file", "f", "",
		"path to the Dockerfile (default: Dockerfile in the context)")
	f.StringArrayVarP(&cfg.tags, "tag", "t", nil,
		"image name and optional tag (repeatable)")
	f.BoolVar(&cfg.push, "push", false,
		"push the result to the registry (shorthand for --output type=registry)")
	f.StringVarP(&cfg.output, "output", "o", "",
		"buildx output destination (e.g. type=local,dest=out); mutually exclusive with --push")
	f.StringVar(&cfg.progress, "progress", "auto",
		"progress output mode: auto | plain | tty | rawjson")
	f.StringVar(&cfg.builddURL, "buildd-url", envOr("BUILDKIT_OPERATOR_BUILDD_URL", "http://buildd.buildkit-operator.svc:8080"),
		"base URL of the buildd control plane; routing POSTs to <url>/route")
	f.StringVar(&cfg.clientCerts, "client-certs", envOr("BUILDKIT_OPERATOR_CLIENT_CERTS", defaultCertsDir()),
		"directory holding ca.pem, cert.pem and key.pem for the remote driver mTLS")
	f.BoolVar(&cfg.dryRun, "dry-run", false,
		"resolve + route, then print the buildx argv without creating the builder or building")
	f.BoolVar(&cfg.untrusted, "untrusted", envBool("BUILDKIT_OPERATOR_UNTRUSTED"),
		"untrusted (fork-PR) build: route to an ephemeral daemon seeded read-only from the canonical "+
			"snapshot, with NO write-back to the shared cache (anti cache-poisoning)")
	f.StringVar(&cfg.token, "token", os.Getenv("BUILDKIT_OPERATOR_TOKEN"),
		"bearer token for the buildd /route API (when buildd requires auth)")
	f.DurationVar(&cfg.routeWait, "route-wait", durationOr("BUILDKIT_OPERATOR_ROUTE_WAIT", 5*time.Minute),
		"max time to wait for buildd to route the build (must cover a cold-start daemon attach)")

	return cmd
}

// run executes the resolve -> route -> build pipeline.
func run(ctx context.Context, cfg *config) error {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	if cfg.push && cfg.output != "" {
		return fmt.Errorf("--push and --output are mutually exclusive")
	}

	// 1. RESOLVE.
	repo := router.NormalizeRepo(cfg.resolveRepo(logger))
	if repo == "" {
		return fmt.Errorf("could not resolve a repository: pass --repo (or run inside a git repo / non-empty context)")
	}
	arch := router.NormalizeArch(cfg.arch)
	if arch != "amd64" && arch != "arm64" {
		return fmt.Errorf("unsupported --arch %q: want amd64 or arm64", cfg.arch)
	}

	// The router is the single source of truth for the key; computing it locally
	// too lets us log the value we expect and surface routing drift early.
	localKey := router.ProjectKey(repo, cfg.name, cfg.target, arch)
	logger.Info("resolved build",
		"repo", repo,
		"name", cfg.name,
		"target", cfg.target,
		"arch", arch,
		"context", cfg.contextDir,
		"expected_key", localKey,
		"expected_daemon", router.DaemonName(localKey),
	)

	// 2. ROUTE.
	resp, err := routeBuild(ctx, cfg, router.RouteRequest{
		Repo:      repo,
		Name:      cfg.name,
		Target:    cfg.target,
		Arch:      arch,
		Untrusted: cfg.untrusted,
	})
	if err != nil {
		return err
	}
	// Release the inflight build buildd counted on /route so the daemon can scale to zero promptly
	// (best-effort; buildd's safety net bounds a missed release). Skipped on dry-run (no route side effect).
	if !cfg.dryRun {
		defer completeBuild(cfg, resp.Key, logger)
	}
	if resp.Key != localKey {
		// Not fatal — buildd owns the authoritative key — but worth a loud log:
		// a mismatch means client and control plane disagree on normalization.
		logger.Warn("routed key differs from locally computed key",
			"routed_key", resp.Key, "local_key", localKey)
	}
	logger.Info("routed to daemon",
		"key", resp.Key, "endpoint", resp.Endpoint, "namespace", resp.Namespace)

	// 3. BUILD.
	builder := "buildkit-operator-" + resp.Key
	buildArgs := cfg.buildxBuildArgs(builder, resp)

	if cfg.dryRun {
		printDryRun(cfg, repo, arch, resp, builder, buildArgs)
		return nil
	}

	if err := ensureBuilder(ctx, builder, cfg.clientCerts, resp.Endpoint, logger); err != nil {
		return err
	}

	logger.Info("starting buildx build", "builder", builder, "argv", append([]string{"docker"}, buildArgs...))
	if err := runStreaming(ctx, "docker", buildArgs); err != nil {
		return fmt.Errorf("buildx build failed: %w", err)
	}
	logger.Info("build succeeded", "key", resp.Key)
	return nil
}

// resolveRepo returns the raw (un-normalized) repo identity from, in order:
// the --repo flag, the git remote.origin.url of the context, or the context
// directory's basename. Normalization is the caller's job (router.NormalizeRepo).
func (cfg *config) resolveRepo(logger *slog.Logger) string {
	if cfg.repo != "" {
		return cfg.repo
	}
	if url := gitRemoteURL(cfg.contextDir); url != "" {
		logger.Debug("derived repo from git remote", "url", url)
		return url
	}
	abs, err := filepath.Abs(cfg.contextDir)
	if err != nil {
		abs = cfg.contextDir
	}
	base := filepath.Base(abs)
	if base == "." || base == string(filepath.Separator) || base == "" {
		return ""
	}
	logger.Debug("derived repo from context basename", "repo", base)
	return base
}

// gitRemoteURL best-effort reads remote.origin.url for the given context dir.
// Any failure (no git, not a repo, no remote) yields "" and is non-fatal.
func gitRemoteURL(dir string) string {
	cmd := exec.Command("git", "-C", dir, "config", "--get", "remote.origin.url")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// routeBuild POSTs the RouteRequest to <builddURL>/route and decodes the RouteResponse. A non-200
// prints the response body and returns an error. The timeout is cfg.routeWait, which must cover a
// cold-start daemon attach (buildd holds the request open until the daemon is Ready) — a too-short
// client timeout would abort an otherwise-successful cold start.
func routeBuild(ctx context.Context, cfg *config, req router.RouteRequest) (router.RouteResponse, error) {
	var zero router.RouteResponse

	body, err := json.Marshal(req)
	if err != nil {
		return zero, fmt.Errorf("encode route request: %w", err)
	}

	url := strings.TrimRight(cfg.builddURL, "/") + "/route"
	ctx, cancel := context.WithTimeout(ctx, cfg.routeWait)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return zero, fmt.Errorf("build route request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+cfg.token)
	}

	res, err := http.DefaultClient.Do(httpReq)
	if err != nil {
		return zero, fmt.Errorf("route request to %s failed: %w", url, err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(res.Body)
		return zero, fmt.Errorf("route %s returned %s: %s", url, res.Status, strings.TrimSpace(buf.String()))
	}

	var resp router.RouteResponse
	if err := json.NewDecoder(res.Body).Decode(&resp); err != nil {
		return zero, fmt.Errorf("decode route response: %w", err)
	}
	if resp.Endpoint == "" {
		return zero, fmt.Errorf("route response from %s has empty endpoint", url)
	}
	return resp, nil
}

// completeBuild best-effort releases the inflight build buildd counted on /route, so the daemon can
// scale to zero once idle. Failures are logged and ignored — buildd's --max-build-seconds safety net
// bounds a missed release.
func completeBuild(cfg *config, key string, logger *slog.Logger) {
	body, _ := json.Marshal(map[string]string{"key": key})
	url := strings.TrimRight(cfg.builddURL, "/") + "/complete"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if cfg.token != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		logger.Debug("release inflight build failed (non-fatal)", "err", err)
		return
	}
	_ = res.Body.Close()
}

// ensureBuilder idempotently creates the per-project buildx remote builder
// pointed at the daemon endpoint, wiring in the client mTLS material. buildx
// errors when the named instance already exists; that is the steady state for a
// hot project, so we treat "existing instance" as success.
func ensureBuilder(ctx context.Context, builder, certsDir, endpoint string, logger *slog.Logger) error {
	driverOpt := fmt.Sprintf("cacert=%s,cert=%s,key=%s",
		filepath.Join(certsDir, "ca.pem"),
		filepath.Join(certsDir, "cert.pem"),
		filepath.Join(certsDir, "key.pem"),
	)
	args := []string{
		"buildx", "create",
		"--name", builder,
		"--driver", "remote",
		"--driver-opt", driverOpt,
		endpoint,
	}

	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stderr = &stderr
	if err := cmd.Run(); err == nil {
		logger.Info("created buildx builder", "builder", builder, "endpoint", endpoint)
		return nil
	} else if !isExistingBuilder(stderr.String()) {
		return fmt.Errorf("buildx create failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// The builder already exists. Keep it only if its endpoint still matches — otherwise a changed
	// endpoint (gateway host added, namespace change) would silently route builds to a stale address.
	if builderHasEndpoint(ctx, builder, endpoint) {
		logger.Info("buildx builder already exists (idempotent)", "builder", builder)
		return nil
	}
	logger.Info("buildx builder endpoint changed, recreating", "builder", builder, "endpoint", endpoint)
	_ = exec.CommandContext(ctx, "docker", "buildx", "rm", builder).Run()
	cmd = exec.CommandContext(ctx, "docker", args...)
	stderr.Reset()
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("buildx recreate failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	logger.Info("recreated buildx builder", "builder", builder, "endpoint", endpoint)
	return nil
}

// builderHasEndpoint reports whether the named buildx builder is configured for endpoint (parsed from
// `docker buildx inspect`). A failure to inspect is treated as "not matching" so we recreate safely.
func builderHasEndpoint(ctx context.Context, builder, endpoint string) bool {
	out, err := exec.CommandContext(ctx, "docker", "buildx", "inspect", builder).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), endpoint)
}

// isExistingBuilder reports whether a buildx create error is the benign
// "already exists" case we treat as idempotent success.
func isExistingBuilder(stderr string) bool {
	s := strings.ToLower(stderr)
	return strings.Contains(s, "existing instance") || strings.Contains(s, "already exists")
}

// buildxBuildArgs assembles the exact `docker buildx build ...` argv (without
// the leading "docker"). Order is deterministic so --dry-run output is stable.
func (cfg *config) buildxBuildArgs(builder string, resp router.RouteResponse) []string {
	args := []string{"buildx", "build", "--builder", builder}

	if cfg.dockerfile != "" {
		args = append(args, "--file", cfg.dockerfile)
	}
	if cfg.target != "" {
		args = append(args, "--target", cfg.target)
	}
	for _, t := range cfg.tags {
		args = append(args, "--tag", t)
	}
	if cfg.push {
		args = append(args, "--push")
	} else if cfg.output != "" {
		args = append(args, "--output", cfg.output)
	}
	if cfg.progress != "" {
		args = append(args, "--progress", cfg.progress)
	}
	if resp.Cache != nil {
		args = append(args, cacheArgs(resp.Cache)...)
	}

	return append(args, cfg.contextDir)
}

// cacheArgs renders the buildx remote-cache flags from the reference buildd returned on /route.
// It carries NO credentials: the daemon resolves them from its own AWS env (buildd --s3-creds-secret),
// so the cold cache is a project policy, not something every CI caller has to configure.
func cacheArgs(c *router.CacheConfig) []string {
	if c == nil || c.Type != "s3" || c.Bucket == "" {
		return nil
	}
	parts := []string{"type=s3", "bucket=" + c.Bucket, "name=" + c.Name}
	if c.Region != "" {
		parts = append(parts, "region="+c.Region)
	}
	if c.EndpointURL != "" {
		parts = append(parts, "endpoint_url="+c.EndpointURL, "use_path_style=true")
	}
	common := strings.Join(parts, ",")
	return []string{"--cache-from", common, "--cache-to", common + ",mode=max"}
}

// printDryRun reports the resolved identity, the routing result, and the exact
// buildx argv to stdout, without touching docker.
func printDryRun(cfg *config, repo, arch string, resp router.RouteResponse, builder string, buildArgs []string) {
	out := os.Stdout
	fmt.Fprintln(out, "dry-run: resolved build (no builder created, nothing executed)")
	fmt.Fprintf(out, "  repo:      %s\n", repo)
	fmt.Fprintf(out, "  target:    %s\n", cfg.target)
	fmt.Fprintf(out, "  arch:      %s\n", arch)
	fmt.Fprintf(out, "  key:       %s\n", resp.Key)
	fmt.Fprintf(out, "  endpoint:  %s\n", resp.Endpoint)
	fmt.Fprintf(out, "  namespace: %s\n", resp.Namespace)
	fmt.Fprintf(out, "  builder:   %s\n", builder)
	fmt.Fprintf(out, "  argv:      docker %s\n", strings.Join(buildArgs, " "))
}

// runStreaming runs a command attached to the process's stdio so buildx
// progress streams straight through to the user's terminal.
func runStreaming(ctx context.Context, name string, args []string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// defaultArch maps the host GOARCH to a buildkit-operator arch via the shared router normalizer (the same
// one buildd uses), defaulting to amd64 for anything outside buildkit-operator's supported set.
func defaultArch() string {
	if a := router.NormalizeArch(runtime.GOARCH); a == "amd64" || a == "arm64" {
		return a
	}
	return "amd64"
}

// defaultCertsDir returns $HOME/.buildkit-operator/certs, or a relative fallback when
// HOME is unset (rare; keeps the default printable in --help).
func defaultCertsDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".buildkit-operator", "certs")
	}
	return filepath.Join(".buildkit-operator", "certs")
}

// envOr returns the env var value or a fallback when unset/empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// envBool reports whether an env var is set to a truthy value (1/true/yes, case-insensitive).
func envBool(key string) bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(key))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// durationOr parses an env var as a Go duration, falling back to def on unset/unparseable input.
func durationOr(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
