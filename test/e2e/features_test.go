//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	volumesnapshotv1 "github.com/kubernetes-csi/external-snapshotter/client/v8/apis/volumesnapshot/v1"
	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"github.com/socialgouv/buildkit-operator/internal/router"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const sentinel = "EXPENSIVE_RAN_SENTINEL"

// expensiveDockerfile is cacheable and produces a REAL layer (10 MB). The sentinel is ASSEMBLED at
// runtime from a shell var (`${S}`), so the literal RUN command — which buildkit prints on every build,
// cached or not — does NOT contain the full sentinel string; only the step's *execution* echoes it.
// That lets a test distinguish "ran" (sentinel in the exec output) from "CACHED" (sentinel absent).
const expensiveDockerfile = "FROM busybox\n" +
	"RUN S=RAN; echo \"EXPENSIVE_${S}_SENTINEL\"; dd if=/dev/zero of=/blob bs=1M count=10 2>/dev/null\n" +
	"RUN echo done > /ok\n"

// 1. Routing + warm local cache: a project's second build reuses its hot PVC cache (the expensive
// layer comes back CACHED — the sentinel does not re-execute).
func TestRouting_WarmCache(t *testing.T) {
	repo := uniqueRepo("warm")
	key := router.ProjectKey(repo, "", "", "amd64")
	t.Cleanup(func() { deleteBuildProject(key) })
	ctxDir := writeContext(t, expensiveDockerfile, nil)

	out1 := runBuild(t, buildOpts{repo: repo, contextDir: ctxDir})
	if !strings.Contains(out1, "routed "+repo) {
		t.Errorf("build 1: missing routed line\n%s", tail(out1, 10))
	}
	if !strings.Contains(out1, sentinel) {
		t.Errorf("build 1 (cold): expensive layer should have executed (sentinel absent)\nBUILD1:\n%s", tail(out1, 25))
	}
	time.Sleep(8 * time.Second) // let the daemon commit its cache before the warm rebuild
	out2 := runBuild(t, buildOpts{repo: repo, contextDir: ctxDir})
	if strings.Contains(out2, sentinel) {
		t.Errorf("build 2 (warm): expensive layer should be CACHED, but the sentinel re-executed\nBUILD2:\n%s", tail(out2, 25))
	}
	if countOccurrences(out2, "CACHED") == 0 {
		t.Error("build 2 (warm): expected CACHED steps")
	}
}

// 2. S3 cold cache: after deleting the BuildProject (fresh daemon, empty PVC), the rebuild rehydrates
// the expensive layer FROM S3 — the sentinel does not re-execute and the daemon imports the S3 manifest.
func TestS3ColdCache(t *testing.T) {
	repo := uniqueRepo("s3cold")
	key := router.ProjectKey(repo, "", "", "amd64")
	t.Cleanup(func() { deleteBuildProject(key) })
	ctxDir := writeContext(t, expensiveDockerfile, nil)

	out1 := runBuild(t, buildOpts{repo: repo, contextDir: ctxDir})
	if !strings.Contains(out1, "exporting cache to Amazon S3") {
		t.Skip("S3 cold cache not configured on this buildd (no export) — skipping")
	}
	time.Sleep(10 * time.Second) // let the S3 export fully propagate before the fresh-daemon rebuild
	deleteBuildProject(key)
	waitFor(t, "fresh daemon (BuildProject gone)", 90*time.Second, func() bool {
		_, err := getBuildProject(t, key)
		return err != nil
	})
	out2 := runBuild(t, buildOpts{repo: repo, contextDir: ctxDir})
	if !strings.Contains(out2, "importing cache manifest from s3") {
		t.Error("S3 cold: rebuild on a fresh daemon did not import the S3 cache manifest")
	}
	if strings.Contains(out2, sentinel) {
		t.Error("S3 cold: expensive layer should have come CACHED from S3, but the sentinel re-executed")
	}
}

// 3. RUN --mount=type=cache persistence: the cache dir survives across builds on the daemon's PVC.
func TestCacheMountPersistence(t *testing.T) {
	repo := uniqueRepo("cachemount")
	key := router.ProjectKey(repo, "", "", "amd64")
	t.Cleanup(func() { deleteBuildProject(key) })
	df := "FROM busybox\nCOPY bust /bust\n" +
		"RUN --mount=type=cache,target=/ccache sh -c 'echo \"CCACHE_BEFORE=$(ls /ccache 2>/dev/null | wc -l)\"; " +
		"for i in $(seq 1 100); do : > /ccache/f$i; done'\n"
	dir := writeContext(t, df, map[string]string{"bust": "A"})

	out1 := runBuild(t, buildOpts{repo: repo, contextDir: dir})
	if !strings.Contains(out1, "CCACHE_BEFORE=0") {
		t.Errorf("build 1: expected an empty cache mount (BEFORE=0)\n%s", tail(out1, 8))
	}
	// Change the COPYed file so the cache-mount RUN re-executes (its parent layer changes).
	if err := os.WriteFile(dir+"/bust", []byte("B"), 0o644); err != nil {
		t.Fatal(err)
	}
	out2 := runBuild(t, buildOpts{repo: repo, contextDir: dir})
	if !strings.Contains(out2, "CCACHE_BEFORE=100") {
		t.Errorf("build 2: cache mount did not persist (want BEFORE=100)\n%s", tail(out2, 8))
	}
}

// 4. Untrusted-fork isolation: an untrusted build runs on an EPHEMERAL fork daemon in a Kata microVM
// (runtimeClassName=kata-clh) with NO S3 credentials, and the build executes (guest kernel via uname).
func TestUntrustedKataIsolation(t *testing.T) {
	repo := uniqueRepo("untrusted")
	canonical := router.ProjectKey(repo, "", "", "amd64")
	fork := router.ForkKey(canonical)
	t.Cleanup(func() { deleteBuildProject(fork); deleteBuildProject(canonical) })
	ctxDir := writeContext(t, "FROM busybox\nRUN uname -a > /k && cat /k\n", nil)

	type res struct {
		out string
		err error
	}
	done := make(chan res, 1)
	go func() {
		out, err := runBuildE(t, buildOpts{repo: repo, untrusted: true, contextDir: ctxDir, timeout: 7 * time.Minute})
		done <- res{out, err}
	}()

	// While the build cold-starts the fork, assert the fork daemon's Kata runtime + no S3 creds.
	var sts *appsv1.StatefulSet
	waitFor(t, "fork daemon StatefulSet", 4*time.Minute, func() bool {
		s, err := getSTS(t, router.DaemonName(fork))
		if err != nil {
			return false
		}
		sts = s
		return true
	})
	if rc := sts.Spec.Template.Spec.RuntimeClassName; rc == nil || *rc != "kata-clh" {
		t.Errorf("fork daemon runtimeClassName = %v, want kata-clh", rc)
	}
	for _, ef := range sts.Spec.Template.Spec.Containers[0].EnvFrom {
		if ef.SecretRef != nil {
			t.Errorf("fork daemon must carry NO S3 credentials, got envFrom %s", ef.SecretRef.Name)
		}
	}
	r := <-done
	if r.err != nil {
		t.Fatalf("untrusted build failed (%v):\n%s", r.err, tail(r.out, 20))
	}
	if !strings.Contains(r.out, "untrusted=true") {
		t.Error("build did not route as untrusted")
	}
	if !strings.Contains(r.out, "Linux") { // uname output proves the build ran inside the (micro)VM
		t.Errorf("untrusted build did not execute\n%s", tail(r.out, 12))
	}
}

// 5. SLSA provenance + SBOM: a pushed build carries an attestation manifest with both predicate types.
// Needs a writable registry ref (BKO_E2E_PUSH_IMAGE) + `docker buildx imagetools` on the runner.
func TestProvenanceAndSBOM(t *testing.T) {
	if c.pushImage == "" {
		t.Skip("BKO_E2E_PUSH_IMAGE unset — skipping provenance/SBOM push test")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH — skipping")
	}
	repo := uniqueRepo("attest")
	key := router.ProjectKey(repo, "", "", "amd64")
	t.Cleanup(func() { deleteBuildProject(key) })
	ctxDir := writeContext(t, "FROM busybox\nRUN echo attest > /ok\n", nil)
	runBuild(t, buildOpts{repo: repo, contextDir: ctxDir, push: true, provenance: "mode=max", sbom: true, tags: c.pushImage})

	raw, err := exec.Command("docker", "buildx", "imagetools", "inspect", c.pushImage, "--raw").Output()
	if err != nil {
		t.Fatalf("imagetools inspect: %v", err)
	}
	var idx struct {
		Manifests []struct {
			Digest   string                        `json:"digest"`
			Platform struct{ Architecture string } `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(raw, &idx); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	var predicates string
	for _, m := range idx.Manifests {
		if m.Platform.Architecture != "unknown" {
			continue // the attestation manifest is platform unknown/unknown
		}
		ar, err := exec.Command("docker", "buildx", "imagetools", "inspect", c.pushImage+"@"+m.Digest, "--raw").Output()
		if err == nil {
			predicates += string(ar)
		}
	}
	if !strings.Contains(predicates, "slsa.dev/provenance") {
		t.Error("pushed image is missing an SLSA provenance attestation")
	}
	if !strings.Contains(predicates, "spdx.dev/Document") {
		t.Error("pushed image is missing an SPDX SBOM attestation")
	}
}

// 6. /prewarm returns a `ready` flag immediately (the non-blocking readiness signal a tunnelled client
// polls), and creates the project warm-from-birth.
func TestPrewarmReadiness(t *testing.T) {
	repo := uniqueRepo("prewarm")
	key := router.ProjectKey(repo, "", "", "amd64")
	t.Cleanup(func() { deleteBuildProject(key) })

	body, _ := json.Marshal(map[string]any{"repo": repo, "arch": "amd64"})
	req, _ := http.NewRequest(http.MethodPost, c.buildURL+"/prewarm", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if c.token != "" {
		req.Header.Set("authorization", "Bearer "+c.token)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("/prewarm: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var pr map[string]json.RawMessage
	if err := json.Unmarshal(raw, &pr); err != nil {
		t.Fatalf("decode /prewarm: %v (%s)", err, raw)
	}
	if _, ok := pr["ready"]; !ok {
		t.Error("/prewarm response is missing the `ready` field")
	}
	if _, ok := pr["endpoint"]; !ok {
		t.Error("/prewarm response is missing the endpoint")
	}
	// Warm-from-birth: the project must reach Warm (not get reaped / stuck Idle).
	waitFor(t, "project reaches Warm after prewarm", 4*time.Minute, func() bool {
		bp, err := getBuildProject(t, key)
		return err == nil && bp.Status.Phase == "Warm"
	})
}

// 7. Durable VolumeSnapshots: enabling snapshotEverySec on a project with a cache PVC produces an
// in-use Cinder snapshot that becomes ReadyToUse.
func TestSnapshots(t *testing.T) {
	repo := uniqueRepo("snap")
	key := router.ProjectKey(repo, "", "", "amd64")
	t.Cleanup(func() { deleteBuildProject(key) })
	ctxDir := writeContext(t, "FROM busybox\nRUN echo snap > /ok\n", nil)
	runBuild(t, buildOpts{repo: repo, contextDir: ctxDir}) // create the PVC

	bp, err := getBuildProject(t, key)
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	bp.Spec.SnapshotEverySec = 60
	if err := k8s.Update(context.Background(), bp); err != nil {
		t.Fatalf("enable snapshots: %v", err)
	}
	t.Cleanup(func() {
		var snaps volumesnapshotv1.VolumeSnapshotList
		_ = k8s.List(context.Background(), &snaps, client.InNamespace(c.buildsNS),
			client.MatchingLabels{"buildkit-operator.socialgouv.github.io/project-key": key})
		for i := range snaps.Items {
			_ = k8s.Delete(context.Background(), &snaps.Items[i])
		}
	})
	waitFor(t, "a VolumeSnapshot becomes ReadyToUse", 4*time.Minute, func() bool {
		var snaps volumesnapshotv1.VolumeSnapshotList
		if err := k8s.List(context.Background(), &snaps, client.InNamespace(c.buildsNS),
			client.MatchingLabels{"buildkit-operator.socialgouv.github.io/project-key": key}); err != nil {
			return false
		}
		for i := range snaps.Items {
			if r := snaps.Items[i].Status; r != nil && r.ReadyToUse != nil && *r.ReadyToUse {
				return true
			}
		}
		return false
	})
}

// 8. Prometheus observability: buildd exposes the operator's custom collectors.
func TestObservabilityMetrics(t *testing.T) {
	if _, err := exec.LookPath("kubectl"); err != nil {
		t.Skip("kubectl not on PATH — skipping metrics port-forward")
	}
	pf := exec.Command("kubectl", "--context", c.context, "-n", c.operatorNS,
		"port-forward", "deploy/buildkit-operator-buildd", "18099:8081")
	if c.kubeconfig != "" {
		pf.Env = append(os.Environ(), "KUBECONFIG="+c.kubeconfig)
	}
	if err := pf.Start(); err != nil {
		t.Fatalf("port-forward: %v", err)
	}
	defer func() { _ = pf.Process.Kill() }()
	time.Sleep(4 * time.Second)

	resp, err := (&http.Client{Timeout: 8 * time.Second}).Get("http://127.0.0.1:18099/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	for _, want := range []string{
		"buildkit_operator_routes_total",
		"buildkit_operator_route_duration_seconds",
		"buildkit_operator_coldstart_seconds",
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("/metrics missing collector %q", want)
		}
	}
}

// 9. HA control plane: buildd runs >=2 ready replicas with an elected leader Lease.
func TestHAControlPlane(t *testing.T) {
	var dep appsv1.Deployment
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: "buildkit-operator-buildd", Namespace: c.operatorNS}, &dep); err != nil {
		t.Fatalf("get buildd deployment: %v", err)
	}
	if dep.Status.ReadyReplicas < 2 {
		t.Errorf("buildd readyReplicas = %d, want >= 2 (HA)", dep.Status.ReadyReplicas)
	}
	if !leaseHasHolder(t) {
		t.Error("no leader-election Lease with a holder found")
	}
}

// 10. Scale-to-zero + PVC retention: an idle project drops to 0 replicas while its cache PVC stays
// Bound (so the next build reattaches rather than rebuilds). Observed across the live projects.
func TestScaleToZeroRetention(t *testing.T) {
	var bps bkov1.BuildProjectList
	if err := k8s.List(context.Background(), &bps, client.InNamespace(c.buildsNS)); err != nil {
		t.Fatalf("list projects: %v", err)
	}
	var checked bool
	for i := range bps.Items {
		bp := &bps.Items[i]
		if bp.Status.Phase != "Idle" || bp.Status.Replicas != 0 {
			continue
		}
		var pvc corev1.PersistentVolumeClaim
		if err := k8s.Get(context.Background(), types.NamespacedName{Name: router.CachePVCName(bp.Spec.Key), Namespace: c.buildsNS}, &pvc); err != nil {
			continue
		}
		checked = true
		if pvc.Status.Phase != corev1.ClaimBound {
			t.Errorf("idle project %s: cache PVC is %s, want Bound (retention)", bp.Spec.Key, pvc.Status.Phase)
		}
	}
	if !checked {
		t.Skip("no Idle/0-replica project with a retained PVC to observe right now")
	}
}
