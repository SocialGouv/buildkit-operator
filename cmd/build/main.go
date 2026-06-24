// Command build is the buildcat client CLI / CI entrypoint: a thin, drop-in-ish
// `docker build` that routes every build to the one hot vanilla buildkitd that
// serves its (project, arch).
//
// The whole point of buildcat is cache sharing: concurrent builds of the same
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
//     `buildcat-<key>` builder pointed at the endpoint (with the client mTLS
//     material), then `docker buildx build ...` streaming progress straight
//     through.
//
// It shells out to `docker buildx` (assumed on PATH) and `git` (best-effort for
// repo detection) and otherwise uses only the Go stdlib plus cobra.
//
// Environment variables (every flag has an env fallback so --help shows the
// effective value):
//
//	BUILDCAT_BUILDD_URL     base URL of buildd          (default http://buildd.buildcat.svc:8080)
//	BUILDCAT_CLIENT_CERTS   dir with ca.pem/cert.pem/key.pem for the remote driver mTLS
//	                        (default $HOME/.buildcat/certs)
//	BUILDCAT_S3_BUCKET      S3/MinIO bucket for the cold cache  (required with --cache-s3)
//	BUILDCAT_S3_REGION      S3 region                           (with --cache-s3)
//	BUILDCAT_S3_ENDPOINT    S3 endpoint URL (MinIO/OVH)         (optional, with --cache-s3)
//	BUILDCAT_S3_PREFIX      key prefix inside the bucket        (optional; default the project key)
//
// AWS credentials for the S3 cache are read by buildx/buildkit from the usual
// AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY / AWS_SESSION_TOKEN environment.
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

	"github.com/socialgouv/buildcat/internal/router"
)

// config holds the resolved CLI configuration. Cobra defaults are env-aware
// (via envOr) so `--help` shows the value that will actually be used.
type config struct {
	repo        string
	target      string
	arch        string
	contextDir  string
	dockerfile  string
	tags        []string
	push        bool
	output      string
	progress    string
	cacheS3     bool
	builddURL   string
	clientCerts string
	dryRun      bool
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
		Short: "buildcat client: route to the project's hot buildkitd and build via buildx remote",
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
	f.StringVar(&cfg.repo, "repo", os.Getenv("BUILDCAT_REPO"),
		"source repository identity (empty = derive from git remote.origin.url, then context basename)")
	f.StringVar(&cfg.target, "target", os.Getenv("BUILDCAT_TARGET"),
		"Dockerfile target stage (also part of the cache identity)")
	f.StringVar(&cfg.arch, "arch", envOr("BUILDCAT_ARCH", defaultArch()),
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
	f.BoolVar(&cfg.cacheS3, "cache-s3", false,
		"add S3 cold cache (--cache-from/--cache-to type=s3) configured from BUILDCAT_S3_* env")
	f.StringVar(&cfg.builddURL, "buildd-url", envOr("BUILDCAT_BUILDD_URL", "http://buildd.buildcat.svc:8080"),
		"base URL of the buildd control plane; routing POSTs to <url>/route")
	f.StringVar(&cfg.clientCerts, "client-certs", envOr("BUILDCAT_CLIENT_CERTS", defaultCertsDir()),
		"directory holding ca.pem, cert.pem and key.pem for the remote driver mTLS")
	f.BoolVar(&cfg.dryRun, "dry-run", false,
		"resolve + route, then print the buildx argv without creating the builder or building")

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
	arch := normalizeArch(cfg.arch)
	if arch == "" {
		return fmt.Errorf("unsupported --arch %q: want amd64 or arm64", cfg.arch)
	}

	// The router is the single source of truth for the key; computing it locally
	// too lets us log the value we expect and surface routing drift early.
	localKey := router.ProjectKey(repo, cfg.target, arch)
	logger.Info("resolved build",
		"repo", repo,
		"target", cfg.target,
		"arch", arch,
		"context", cfg.contextDir,
		"expected_key", localKey,
		"expected_daemon", router.DaemonName(localKey),
	)

	// 2. ROUTE.
	resp, err := routeBuild(ctx, cfg.builddURL, router.RouteRequest{
		Repo:   repo,
		Target: cfg.target,
		Arch:   arch,
	})
	if err != nil {
		return err
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
	builder := "buildcat-" + resp.Key
	buildArgs := cfg.buildxBuildArgs(builder, resp)

	if cfg.dryRun {
		printDryRun(cfg, repo, arch, resp, buildArgs)
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

// routeBuild POSTs the RouteRequest to <builddURL>/route and decodes the
// RouteResponse. A non-200 prints the response body and returns an error.
func routeBuild(ctx context.Context, builddURL string, req router.RouteRequest) (router.RouteResponse, error) {
	var zero router.RouteResponse

	body, err := json.Marshal(req)
	if err != nil {
		return zero, fmt.Errorf("encode route request: %w", err)
	}

	url := strings.TrimRight(builddURL, "/") + "/route"
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return zero, fmt.Errorf("build route request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")

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
	if err := cmd.Run(); err != nil {
		if isExistingBuilder(stderr.String()) {
			logger.Info("buildx builder already exists (idempotent)", "builder", builder)
			return nil
		}
		return fmt.Errorf("buildx create failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	logger.Info("created buildx builder", "builder", builder, "endpoint", endpoint)
	return nil
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
	if cfg.cacheS3 {
		args = append(args, s3CacheArgs(resp.Key)...)
	}

	return append(args, cfg.contextDir)
}

// s3CacheArgs builds the buildx S3 cold-cache flags from the BUILDCAT_S3_* env.
// Bucket is required; region/endpoint/prefix are added only when set. The
// authoritative project key is the default prefix so each project's cold cache
// is namespaced exactly like its daemon.
func s3CacheArgs(key string) []string {
	bucket := os.Getenv("BUILDCAT_S3_BUCKET")
	if bucket == "" {
		// Honour the flag intent loudly rather than silently dropping the cache:
		// a missing bucket is almost always a misconfiguration.
		slog.Warn("--cache-s3 set but BUILDCAT_S3_BUCKET is empty; skipping S3 cache")
		return nil
	}

	prefix := os.Getenv("BUILDCAT_S3_PREFIX")
	if prefix == "" {
		prefix = key
	}

	parts := []string{"region=" + os.Getenv("BUILDCAT_S3_REGION"), "bucket=" + bucket, "prefix=" + prefix + "/"}
	if ep := os.Getenv("BUILDCAT_S3_ENDPOINT"); ep != "" {
		parts = append(parts, "endpoint_url="+ep)
	}
	common := strings.Join(parts, ",")

	return []string{
		"--cache-from", "type=s3," + common,
		"--cache-to", "type=s3,mode=max," + common,
	}
}

// printDryRun reports the resolved identity, the routing result, and the exact
// buildx argv to stdout, without touching docker.
func printDryRun(cfg *config, repo, arch string, resp router.RouteResponse, buildArgs []string) {
	out := os.Stdout
	fmt.Fprintln(out, "dry-run: resolved build (no builder created, nothing executed)")
	fmt.Fprintf(out, "  repo:      %s\n", repo)
	fmt.Fprintf(out, "  target:    %s\n", cfg.target)
	fmt.Fprintf(out, "  arch:      %s\n", arch)
	fmt.Fprintf(out, "  key:       %s\n", resp.Key)
	fmt.Fprintf(out, "  endpoint:  %s\n", resp.Endpoint)
	fmt.Fprintf(out, "  namespace: %s\n", resp.Namespace)
	fmt.Fprintf(out, "  builder:   buildcat-%s\n", resp.Key)
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

// defaultArch maps the host GOARCH to a buildcat arch, defaulting to amd64.
func defaultArch() string {
	if a := normalizeArch(runtime.GOARCH); a != "" {
		return a
	}
	return "amd64"
}

// normalizeArch maps known arch spellings to buildcat's canonical set
// (amd64 | arm64); unknown values yield "".
func normalizeArch(a string) string {
	switch strings.ToLower(strings.TrimSpace(a)) {
	case "amd64", "x86_64":
		return "amd64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return ""
	}
}

// defaultCertsDir returns $HOME/.buildcat/certs, or a relative fallback when
// HOME is unset (rare; keeps the default printable in --help).
func defaultCertsDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return filepath.Join(home, ".buildcat", "certs")
	}
	return filepath.Join(".buildcat", "certs")
}

// envOr returns the env var value or a fallback when unset/empty.
func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
