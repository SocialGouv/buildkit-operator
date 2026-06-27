//go:build e2e

// Package e2e is an end-to-end test suite that runs the REAL build path (scripts/build.sh → buildd
// /route → a remote buildkitd over mTLS) against a live cluster, and asserts both the build output and
// the resulting cluster state. It is gated behind the `e2e` build tag and skips unless BKO_E2E_BUILDD_URL
// is set, so `go test ./...` never triggers it. See test/e2e/README.md for the required env + tooling.
package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	appsv1 "k8s.io/api/apps/v1"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// cfg is the suite configuration, loaded once from the environment.
type cfg struct {
	buildURL    string // buildd /route API (e.g. https://buildd.bko.fabrique.social.gouv.fr)
	gatewayHost string // off-cluster SNI host (e.g. bko.fabrique.social.gouv.fr)
	context     string // kube context (default ovh-prod)
	operatorNS  string // control-plane namespace (default buildkit-operator)
	buildsNS    string // daemons namespace (default buildkit-builds)
	certsDir    string // dir with ca.pem/cert.pem/key.pem (default ../../deploy/cert/.certs/client)
	authSecret  string // bearer-token secret in operatorNS (default buildkit-operator-auth, key "token")
	pushImage   string // optional registry ref for the provenance/SBOM/cosign tests (skipped if empty)
	token       string // resolved from authSecret
	root        string // repo root (holds scripts/build.sh)
	kubeconfig  string
}

var c cfg
var k8s client.Client

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func TestMain(m *testing.M) {
	c.buildURL = os.Getenv("BKO_E2E_BUILDD_URL")
	if c.buildURL == "" {
		fmt.Println("BKO_E2E_BUILDD_URL unset — skipping the e2e suite")
		os.Exit(0)
	}
	c.gatewayHost = os.Getenv("BKO_E2E_GATEWAY_HOST")
	c.context = env("BKO_E2E_CONTEXT", "ovh-prod")
	c.operatorNS = env("BKO_E2E_OPERATOR_NS", "buildkit-operator")
	c.buildsNS = env("BKO_E2E_BUILDS_NS", "buildkit-builds")
	c.authSecret = env("BKO_E2E_AUTH_SECRET", "buildkit-operator-auth")
	c.pushImage = os.Getenv("BKO_E2E_PUSH_IMAGE")
	c.kubeconfig = os.Getenv("KUBECONFIG")

	root, err := repoRoot()
	must(err)
	c.root = root
	c.certsDir = env("BKO_E2E_CERTS_DIR", filepath.Join(root, "deploy/cert/.certs/client"))

	k8s, err = newClient()
	must(err)
	// Resolve the bearer token from the cluster (so callers don't have to handle the secret).
	var sec corev1.Secret
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: c.authSecret, Namespace: c.operatorNS}, &sec); err == nil {
		c.token = string(sec.Data["token"])
	}
	os.Exit(m.Run())
}

func must(err error) {
	if err != nil {
		fmt.Println("e2e setup failed:", err)
		os.Exit(1)
	}
}

func repoRoot() (string, error) {
	d, err := os.Getwd() // .../test/e2e
	if err != nil {
		return "", err
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(d, "go.mod")); err == nil {
			return d, nil
		}
		d = filepath.Dir(d)
	}
	return "", fmt.Errorf("go.mod not found above the test dir")
}

func newClient() (client.Client, error) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	_ = bkov1.AddToScheme(scheme)
	_ = volumesnapshotv1.AddToScheme(scheme)
	loader := clientcmd.NewDefaultClientConfigLoadingRules()
	if c.kubeconfig != "" {
		loader.ExplicitPath = c.kubeconfig
	}
	rc, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		loader, &clientcmd.ConfigOverrides{CurrentContext: c.context}).ClientConfig()
	if err != nil {
		return nil, err
	}
	return client.New(rc, client.Options{Scheme: scheme})
}

// buildOpts drives a single scripts/build.sh invocation.
type buildOpts struct {
	repo       string
	contextDir string
	untrusted  bool
	push       bool
	tags       string
	provenance string
	sbom       bool
	buildArgs  map[string]string // --build-arg KEY=VALUE (mapped to the BUILD_ARGS env, one per line)
	extra      map[string]string // extra env (e.g. BUILDKIT_OPERATOR_WAIT_WARM)
	timeout    time.Duration
}

// runBuild runs the real client (scripts/build.sh) and returns its combined output, failing the test on
// a non-zero exit. For use from a spawned goroutine (where t.Fatalf is illegal), use runBuildE.
func runBuild(t *testing.T, o buildOpts) string {
	t.Helper()
	out, err := runBuildE(t, o)
	if err != nil {
		t.Fatalf("build.sh failed (%v):\n%s", err, tail(out, 40))
	}
	return out
}

// runBuildE runs scripts/build.sh and returns (output, error) without touching t's failure state, so it
// is safe to call from a goroutine.
func runBuildE(t *testing.T, o buildOpts) (string, error) {
	t.Helper()
	if o.timeout == 0 {
		o.timeout = 6 * time.Minute
	}
	if o.tags == "" {
		o.tags = "bko-e2e:local"
	}
	ctx, cancel := context.WithTimeout(context.Background(), o.timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", filepath.Join(c.root, "scripts/build.sh"))
	cmd.Dir = c.root
	ca, _ := os.ReadFile(filepath.Join(c.certsDir, "ca.pem"))
	cert, _ := os.ReadFile(filepath.Join(c.certsDir, "cert.pem"))
	key, _ := os.ReadFile(filepath.Join(c.certsDir, "key.pem"))
	e := map[string]string{
		"BUILDKIT_OPERATOR_BUILDD_URL": c.buildURL,
		"BUILDKIT_OPERATOR_TOKEN":      c.token,
		"BUILDKIT_OPERATOR_CA":         string(ca),
		"BUILDKIT_OPERATOR_CERT":       string(cert),
		"BUILDKIT_OPERATOR_KEY":        string(key),
		"REPO":                         o.repo,
		"ARCH":                         "amd64",
		"BUILD_CONTEXT":                o.contextDir,
		"TAGS":                         o.tags,
		"PUSH":                         boolStr(o.push),
		"UNTRUSTED":                    boolStr(o.untrusted),
		"PROVENANCE":                   o.provenance,
		"SBOM":                         boolStr(o.sbom),
		"PATH":                         os.Getenv("PATH"),
		"HOME":                         os.Getenv("HOME"),
		"KUBECONFIG":                   c.kubeconfig,
	}
	if c.gatewayHost != "" {
		e["BUILDKIT_OPERATOR_GATEWAY_HOST"] = c.gatewayHost
	}
	if len(o.buildArgs) > 0 {
		var b strings.Builder
		for k, v := range o.buildArgs {
			b.WriteString(k + "=" + v + "\n")
		}
		e["BUILD_ARGS"] = b.String()
	}
	for k, v := range o.extra {
		e[k] = v
	}
	cmd.Env = flatten(e)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func flatten(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	return out
}

func tail(s string, n int) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

// writeContext makes a temp build context with the given Dockerfile and optional extra files.
func writeContext(t *testing.T, dockerfile string, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Dockerfile"), []byte(dockerfile), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// --- cluster helpers ---------------------------------------------------------

func getBuildProject(t *testing.T, key string) (*bkov1.BuildProject, error) {
	var bp bkov1.BuildProject
	err := k8s.Get(context.Background(), types.NamespacedName{Name: key, Namespace: c.buildsNS}, &bp)
	return &bp, err
}

func getSTS(t *testing.T, name string) (*appsv1.StatefulSet, error) {
	var sts appsv1.StatefulSet
	err := k8s.Get(context.Background(), types.NamespacedName{Name: name, Namespace: c.buildsNS}, &sts)
	return &sts, err
}

func deleteBuildProject(key string) {
	bp := &bkov1.BuildProject{}
	bp.Name, bp.Namespace = key, c.buildsNS
	_ = k8s.Delete(context.Background(), bp)
}

// waitFor polls fn until it returns true or the deadline elapses.
func waitFor(t *testing.T, what string, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(3 * time.Second)
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func countOccurrences(s, sub string) int { return strings.Count(s, sub) }

// uniqueRepo returns a per-run repo identity so concurrent runs / reruns don't collide on a daemon.
func uniqueRepo(name string) string {
	return fmt.Sprintf("bko-e2e/%s-%d", name, time.Now().UnixNano())
}

// leaseHasHolder reports whether a coordination Lease with a non-empty holder exists in operatorNS.
func leaseHasHolder(t *testing.T) bool {
	var leases coordinationv1.LeaseList
	if err := k8s.List(context.Background(), &leases, client.InNamespace(c.operatorNS)); err != nil {
		return false
	}
	for i := range leases.Items {
		if h := leases.Items[i].Spec.HolderIdentity; h != nil && *h != "" {
			return true
		}
	}
	return false
}
