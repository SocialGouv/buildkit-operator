// Package router computes the stable cache identity that routes a build to the
// right hot daemon. It is intentionally PURE (no Kubernetes imports) so the CLI
// and the control plane share exactly the same normalization — the single most
// important correctness property of buildcat: all builds that must share a cache
// MUST resolve to the same Key (a too-fine key fragments the cache and kills sharing).
package router

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// RouteRequest is the wire request from the CLI to buildd's /route endpoint.
type RouteRequest struct {
	Repo   string `json:"repo"`
	Target string `json:"target"`
	Arch   string `json:"arch"`
}

// RouteResponse is the wire response: the resolved key and the mTLS endpoint to build against.
type RouteResponse struct {
	Key       string `json:"key"`
	Endpoint  string `json:"endpoint"`
	Namespace string `json:"namespace"`
}

// NormalizeRepo reduces a repository reference to a canonical host/path form so
// that https://, ssh://, git@host: and trailing .git/slashes all collapse to the
// same identity. Lowercased.
func NormalizeRepo(repo string) string {
	s := strings.TrimSpace(strings.ToLower(repo))
	// strip scheme:// first
	for _, sch := range []string{"https://", "http://", "git://", "ssh://"} {
		s = strings.TrimPrefix(s, sch)
	}
	// strip a leading user@ (scp-like git@host:path, or scheme'd ssh://git@host/path)
	if at := strings.IndexByte(s, '@'); at >= 0 {
		if slash := strings.IndexByte(s, '/'); slash < 0 || at < slash {
			s = s[at+1:]
		}
	}
	s = strings.ReplaceAll(s, ":", "/") // host:org/repo -> host/org/repo
	s = strings.TrimSuffix(s, "/")
	s = strings.TrimSuffix(s, ".git")
	s = strings.TrimSuffix(s, "/")
	// collapse duplicate slashes
	for strings.Contains(s, "//") {
		s = strings.ReplaceAll(s, "//", "/")
	}
	return s
}

// NormalizeTarget maps the empty Dockerfile target to a stable sentinel.
func NormalizeTarget(target string) string {
	t := strings.TrimSpace(strings.ToLower(target))
	if t == "" {
		return "default"
	}
	return t
}

// NormalizeArch canonicalizes the architecture token.
func NormalizeArch(arch string) string {
	a := strings.TrimSpace(strings.ToLower(arch))
	switch a {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	default:
		return a
	}
}

// ProjectKey is the stable cache identity: "p" + first 16 hex chars of
// sha256(normRepo \x00 normTarget \x00 normArch). Coarse on purpose (no context,
// no branch) so concurrent + later builds of the same project converge on one daemon.
func ProjectKey(repo, target, arch string) string {
	h := sha256.Sum256([]byte(NormalizeRepo(repo) + "\x00" + NormalizeTarget(target) + "\x00" + NormalizeArch(arch)))
	return "p" + hex.EncodeToString(h[:])[:16]
}

// DaemonName is the k8s object name (StatefulSet/Service) for a project key.
// "buildkitd-p<16hex>" => 27 chars, DNS-1123 safe, well under 63.
func DaemonName(key string) string {
	return "buildkitd-" + key
}

// ServiceFQDN is the in-cluster DNS name of a project's daemon Service.
func ServiceFQDN(key, namespace string) string {
	return DaemonName(key) + "." + namespace + ".svc"
}

// Endpoint is the mTLS address clients dial for a project's daemon.
func Endpoint(key, namespace string, port int32) string {
	return "tcp://" + ServiceFQDN(key, namespace) + ":" + itoa(port)
}

func itoa(p int32) string {
	// small, allocation-light int->string for ports
	if p == 0 {
		return "0"
	}
	var b [6]byte
	i := len(b)
	for p > 0 {
		i--
		b[i] = byte('0' + p%10)
		p /= 10
	}
	return string(b[i:])
}
