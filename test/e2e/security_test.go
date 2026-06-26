//go:build e2e

package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

// prewarmStatus POSTs /prewarm with the given Authorization header value ("" = none) and returns the
// HTTP status. /prewarm is non-blocking (202 on success), so it's a fast probe of the auth gate.
func prewarmStatus(t *testing.T, authHeader string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"repo": uniqueRepo("sec"), "arch": "amd64"})
	req, _ := http.NewRequest(http.MethodPost, c.buildURL+"/prewarm", bytes.NewReader(body))
	req.Header.Set("content-type", "application/json")
	if authHeader != "" {
		req.Header.Set("authorization", authHeader)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("/prewarm: %v", err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// OIDC is enforced on the live buildd: an unauthenticated or garbage-token request is rejected, while
// the legacy bearer is still accepted (zero-downtime migration). The repo-binding / real-forge-token
// acceptance is exercised by the GitHub + GitLab builds (a real OIDC JWT can only be minted in CI).
func TestOIDCEnforcement(t *testing.T) {
	if c.token == "" {
		t.Skip("no legacy bearer token resolved from the cluster — skipping")
	}
	if code := prewarmStatus(t, ""); code != http.StatusUnauthorized {
		t.Errorf("no-token /prewarm = %d, want 401 (OIDC enforced)", code)
	}
	if code := prewarmStatus(t, "Bearer not-a-real-jwt"); code != http.StatusUnauthorized {
		t.Errorf("garbage-token /prewarm = %d, want 401", code)
	}
	if code := prewarmStatus(t, "Bearer "+c.token); code != http.StatusAccepted {
		t.Errorf("legacy-bearer /prewarm = %d, want 202 (migration fallback)", code)
	}
}

// The deployed security posture is intact: buildd verifies OIDC (--oidc-config + the policy ConfigMap),
// and the public gateway caps pre-auth connections (--max-conns).
func TestSecurityPosture(t *testing.T) {
	var buildd appsv1.Deployment
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: "buildkit-operator-buildd", Namespace: c.operatorNS}, &buildd); err != nil {
		t.Fatalf("get buildd: %v", err)
	}
	if !hasArg(buildd, "--oidc-config") {
		t.Error("buildd is not configured with --oidc-config (OIDC verification off)")
	}
	var oidcCM corev1.ConfigMap
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: "buildkit-operator-oidc", Namespace: c.operatorNS}, &oidcCM); err != nil {
		t.Errorf("OIDC policy ConfigMap missing: %v", err)
	}
	var gw appsv1.Deployment
	if err := k8s.Get(context.Background(), types.NamespacedName{Name: "buildkit-operator-gateway", Namespace: c.operatorNS}, &gw); err != nil {
		t.Fatalf("get gateway: %v", err)
	}
	if !hasArg(gw, "--max-conns") {
		t.Error("gateway is missing the --max-conns pre-auth connection cap")
	}
}

func hasArg(dep appsv1.Deployment, prefix string) bool {
	for _, c := range dep.Spec.Template.Spec.Containers {
		for _, a := range c.Args {
			if strings.HasPrefix(a, prefix) {
				return true
			}
		}
	}
	return false
}
