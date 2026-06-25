// Package router computes the stable cache identity that routes a build to the
// right hot daemon. It is intentionally PURE (no Kubernetes imports) so the CLI
// and the control plane share exactly the same normalization — the single most
// important correctness property of buildkit-operator: all builds that must share a cache
// MUST resolve to the same Key (a too-fine key fragments the cache and kills sharing).
package router

import (
	"crypto/sha256"
	"encoding/hex"
	"strconv"
	"strings"
)

// RouteRequest is the wire request from the CLI to buildd's /route endpoint.
type RouteRequest struct {
	Repo string `json:"repo"`
	// Name is an optional component within the repo (a monorepo image/path). Empty for a
	// single-image repo — and an empty Name keeps the exact same key as before this field existed.
	Name   string `json:"name,omitempty"`
	Target string `json:"target"`
	Arch   string `json:"arch"`
	// Untrusted marks a fork-PR build: routed to an ephemeral daemon seeded read-only from the
	// canonical snapshot, with no write-back to the canonical cache (anti cache-poisoning, M4).
	Untrusted bool `json:"untrusted,omitempty"`
}

// RouteResponse is the wire response: the resolved key, the mTLS endpoint to build against, and
// (optionally) the project's cold-cache reference for the client to apply.
type RouteResponse struct {
	Key       string `json:"key"`
	Endpoint  string `json:"endpoint"`
	Namespace string `json:"namespace"`
	// Cache, when non-nil, is the project's cold cache the client should add to the build. It carries
	// NO credentials: the daemon holds them (AWS env from a Secret), so the cache config is centralized
	// in buildd instead of duplicated across every CI caller's secrets.
	Cache *CacheConfig `json:"cache,omitempty"`
}

// CacheConfig is a buildx remote-cache reference buildd hands to the client (currently S3 only).
type CacheConfig struct {
	Type        string `json:"type"`                  // "s3"
	Bucket      string `json:"bucket"`                // shared bucket
	Region      string `json:"region,omitempty"`      // S3 region
	EndpointURL string `json:"endpointUrl,omitempty"` // S3 endpoint (OVH Object Storage / MinIO)
	Name        string `json:"name"`                  // per-project cache prefix = the project key
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

// NormalizeName canonicalizes the optional monorepo component name (lowercased, trimmed).
func NormalizeName(name string) string {
	return strings.TrimSpace(strings.ToLower(name))
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
// sha256(normRepo [\x00 n:name] \x00 normTarget \x00 normArch). Coarse on purpose (no context,
// no branch) so concurrent + later builds of the same project converge on one daemon. The optional
// name segments a monorepo into per-component daemons; it is OMITTED when empty, so single-image
// repos keep the exact key they had before name existed (migration-safe).
func ProjectKey(repo, name, target, arch string) string {
	seed := NormalizeRepo(repo)
	if n := NormalizeName(name); n != "" {
		seed += "\x00n:" + n
	}
	seed += "\x00" + NormalizeTarget(target) + "\x00" + NormalizeArch(arch)
	h := sha256.Sum256([]byte(seed))
	return "p" + hex.EncodeToString(h[:])[:16]
}

// DaemonName is the k8s object name (StatefulSet/Service) for a project key.
// "buildkitd-p<16hex>" => 27 chars, DNS-1123 safe, well under 63.
func DaemonName(key string) string {
	return "buildkitd-" + key
}

// ForkKey is the ephemeral key for untrusted (fork-PR) builds of a project. Distinct from
// the canonical key so a fork daemon never shares the canonical cache (anti cache-poisoning).
func ForkKey(canonicalKey string) string {
	return "fork" + canonicalKey
}

// IsForkKey reports whether a key belongs to an untrusted fork daemon (built by ForkKey). Used to
// apply tighter isolation (e.g. a sandboxed runtime) to untrusted builds only.
func IsForkKey(key string) bool {
	return strings.HasPrefix(key, "fork")
}

// CloneKey is the key of the i-th fan-out clone of a project (M5). Distinct from the canonical
// and fork keys, so each clone is an independent sibling daemon seeded from the snapshot.
func CloneKey(canonicalKey string, i int) string {
	return "c" + strconv.Itoa(i) + canonicalKey
}

// CachePVCName is the retained cache PVC of a project — the StatefulSet's "cache"
// volumeClaimTemplate at ordinal 0 (cache-<daemon>-0). It persists across scale-to-zero.
func CachePVCName(key string) string {
	return "cache-" + DaemonName(key) + "-0"
}

// ServiceFQDN is the in-cluster DNS name of a project's daemon Service.
func ServiceFQDN(key, namespace string) string {
	return DaemonName(key) + "." + namespace + ".svc"
}

// Endpoint is the mTLS address clients dial for a project's daemon.
func Endpoint(key, namespace string, port int32) string {
	return "tcp://" + ServiceFQDN(key, namespace) + ":" + strconv.Itoa(int(port))
}

// EndpointHost formats a daemon endpoint for an arbitrary host — e.g. the <daemon>.<gateway-domain>
// SNI hostname when daemons are reached through the shared gateway (off-cluster CI).
func EndpointHost(host string, port int32) string {
	return "tcp://" + host + ":" + strconv.Itoa(int(port))
}
