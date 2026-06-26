// Package identity verifies a CI OIDC token and turns it into a SERVER-VERIFIED project identity, so
// buildd never has to trust the client's self-declared repo / untrusted fields. The callers are CI
// systems (GitHub Actions, GitLab CI) that natively mint short-lived OIDC JWTs whose claims (the
// repository / project path) are signed by the forge — verifying the signature against the issuer's
// JWKS binds the build to a real repo and kills cross-repo cache poisoning.
//
// It is provider-extensible: adding a forge (e.g. Forgejo) is adding a Provider with its issuer + claim
// mapping, no other change. Verification is fail-closed: any error (unknown issuer, bad signature,
// wrong audience, expired) is a rejection.
package identity

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/socialgouv/buildkit-operator/internal/router"
)

// Identity is the verified result handed back to buildd. Repo is host-qualified and normalized exactly
// like a RouteRequest.Repo (router.NormalizeRepo), so it drops straight into router.ProjectKey and the
// resulting cache key is identical to the pre-OIDC one — no cache migration.
type Identity struct {
	Issuer    string
	Repo      string
	Untrusted bool
}

// Provider configures one trusted OIDC issuer. Type selects a built-in claim mapping; Issuer/Audience
// are always required. RepoClaim/Host override the Type defaults when a forge deviates.
type Provider struct {
	// Type selects built-in claim handling: "github" | "gitlab" (Forgejo: follow-up). Optional when
	// RepoClaim+Host are set explicitly.
	Type string `json:"type,omitempty"`
	// Issuer is the OIDC issuer URL (e.g. https://token.actions.githubusercontent.com). Discovery runs
	// against <issuer>/.well-known/openid-configuration.
	Issuer string `json:"issuer"`
	// Audience is the expected `aud` claim — the value the CI job requests when minting the token. The
	// token is rejected unless its audience contains this.
	Audience string `json:"audience"`
	// RepoClaim is the JWT claim holding the repository path (GitHub: "repository", GitLab:
	// "project_path"). Defaulted from Type when empty.
	RepoClaim string `json:"repoClaim,omitempty"`
	// Host is prefixed to the (host-less) repo claim to host-qualify it before normalization (GitHub:
	// "github.com"; GitLab: the issuer host). Defaulted from Type / Issuer when empty.
	Host string `json:"host,omitempty"`
}

// Config is the whole OIDC policy — loaded from an env JSON blob or a mounted ConfigMap file. Disable is
// the admin break-glass (only an operator who controls the buildd Deployment/ConfigMap can set it).
type Config struct {
	Providers     []Provider `json:"providers,omitempty"`
	RepoAllowlist []string   `json:"repoAllowlist,omitempty"`
	Disable       bool       `json:"disable,omitempty"`
}

// claimMapper turns a provider's raw claims into (repoPath, untrusted). repoPath is the host-less path
// (it gets host-qualified + normalized by the verifier).
type claimMapper struct {
	repoClaim string
	host      string
	untrusted func(map[string]any) bool
}

// builtin returns the default claim mapping for a known provider Type, host-qualifying via the issuer
// when the forge is self-hosted (GitLab/Forgejo).
func builtin(p Provider) (claimMapper, error) {
	host := p.Host
	if host == "" {
		host = hostFromIssuer(p.Issuer)
	}
	switch strings.ToLower(p.Type) {
	case "github":
		if p.Host == "" {
			host = "github.com"
		}
		return claimMapper{
			repoClaim: orDefault(p.RepoClaim, "repository"),
			host:      host,
			// A pull_request run carries ref refs/pull/<n>/merge — treat it as untrusted (it builds code
			// from a PR branch). Canonical pushes (refs/heads/...) stay trusted.
			untrusted: func(c map[string]any) bool { return strings.HasPrefix(asString(c["ref"]), "refs/pull/") },
		}, nil
	case "gitlab":
		return claimMapper{
			repoClaim: orDefault(p.RepoClaim, "project_path"),
			host:      host,
			// ref_protected is "true" only on protected branches/tags; an unprotected ref (fork/MR
			// feature branch) is untrusted.
			untrusted: func(c map[string]any) bool { return asString(c["ref_protected"]) == "false" },
		}, nil
	case "forgejo", "gitea":
		// Forgejo/Gitea Actions mirror the GitHub Actions OIDC token (repository claim, refs/pull/* on
		// PRs) but the forge is self-hosted, so host comes from the issuer (not github.com). Override
		// repoClaim if your instance differs.
		return claimMapper{
			repoClaim: orDefault(p.RepoClaim, "repository"),
			host:      host,
			untrusted: func(c map[string]any) bool { return strings.HasPrefix(asString(c["ref"]), "refs/pull/") },
		}, nil
	default:
		if p.RepoClaim == "" || host == "" {
			return claimMapper{}, fmt.Errorf("oidc provider %q: unknown type %q and no repoClaim/host override", p.Issuer, p.Type)
		}
		return claimMapper{repoClaim: p.RepoClaim, host: host, untrusted: func(map[string]any) bool { return false }}, nil
	}
}

// providerState lazily initializes the go-oidc verifier on first use, so buildd starts WITHOUT needing
// the issuer reachable (discovery + JWKS happen on the first token from that issuer, then cache). A
// failed init is not cached — the next token retries.
type providerState struct {
	cfg    Provider
	mapper claimMapper
	mu     sync.Mutex
	v      *oidc.IDTokenVerifier
}

func (p *providerState) verifier(ctx context.Context) (*oidc.IDTokenVerifier, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.v != nil {
		return p.v, nil
	}
	prov, err := oidc.NewProvider(ctx, p.cfg.Issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc discovery for %q: %w", p.cfg.Issuer, err)
	}
	p.v = prov.Verifier(&oidc.Config{ClientID: p.cfg.Audience})
	return p.v, nil
}

// Verifier verifies tokens against the configured providers and applies the repo allowlist.
type Verifier struct {
	byIssuer  map[string]*providerState
	allowlist []string
}

// NewVerifier builds a Verifier from config. Returns nil (no error) when OIDC is disabled or has no
// providers — buildd treats a nil verifier as "OIDC off" (falls back to bearer/admin auth).
func NewVerifier(cfg Config) (*Verifier, error) {
	if cfg.Disable || len(cfg.Providers) == 0 {
		return nil, nil
	}
	v := &Verifier{byIssuer: make(map[string]*providerState, len(cfg.Providers)), allowlist: normalizeAllowlist(cfg.RepoAllowlist)}
	for _, p := range cfg.Providers {
		if p.Issuer == "" || p.Audience == "" {
			return nil, fmt.Errorf("oidc provider needs issuer and audience (got issuer=%q audience=%q)", p.Issuer, p.Audience)
		}
		m, err := builtin(p)
		if err != nil {
			return nil, err
		}
		iss := strings.TrimRight(p.Issuer, "/")
		if _, dup := v.byIssuer[iss]; dup {
			return nil, fmt.Errorf("duplicate oidc issuer %q", iss)
		}
		v.byIssuer[iss] = &providerState{cfg: p, mapper: m}
	}
	return v, nil
}

// ErrUntrustedIssuer is returned when a token's issuer is not configured (or the token is unparsable).
var ErrUntrustedIssuer = errors.New("token issuer is not a configured OIDC provider")

// Verify validates the raw JWT (signature against the issuer JWKS, audience, expiry) and returns the
// verified identity. Any error is a hard rejection (fail-closed).
func (v *Verifier) Verify(ctx context.Context, rawToken string) (Identity, error) {
	rawToken = strings.TrimSpace(rawToken)
	if rawToken == "" {
		return Identity{}, errors.New("empty token")
	}
	iss, err := unverifiedIssuer(rawToken)
	if err != nil {
		return Identity{}, err
	}
	ps, ok := v.byIssuer[strings.TrimRight(iss, "/")]
	if !ok {
		return Identity{}, ErrUntrustedIssuer
	}
	ver, err := ps.verifier(ctx)
	if err != nil {
		return Identity{}, err
	}
	tok, err := ver.Verify(ctx, rawToken)
	if err != nil {
		return Identity{}, fmt.Errorf("verify token from %q: %w", iss, err)
	}
	var claims map[string]any
	if err := tok.Claims(&claims); err != nil {
		return Identity{}, fmt.Errorf("decode claims: %w", err)
	}
	repoPath := asString(claims[ps.mapper.repoClaim])
	if repoPath == "" {
		return Identity{}, fmt.Errorf("token from %q has no %q claim", iss, ps.mapper.repoClaim)
	}
	repo := router.NormalizeRepo(ps.mapper.host + "/" + repoPath)
	return Identity{Issuer: iss, Repo: repo, Untrusted: ps.mapper.untrusted(claims)}, nil
}

// AllowRepo reports whether a verified repo may use the service. An empty allowlist allows every
// verified repo (OIDC alone is the gate); a non-empty allowlist is a hard org/repo restriction.
func (v *Verifier) AllowRepo(repo string) bool {
	if len(v.allowlist) == 0 {
		return true
	}
	repo = router.NormalizeRepo(repo)
	for _, e := range v.allowlist {
		if e == "*" {
			return true
		}
		if pfx, ok := strings.CutSuffix(e, "/*"); ok {
			if repo == pfx || strings.HasPrefix(repo, pfx+"/") {
				return true
			}
			continue
		}
		if repo == e {
			return true
		}
	}
	return false
}

func normalizeAllowlist(in []string) []string {
	out := make([]string, 0, len(in))
	for _, e := range in {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		if e == "*" {
			out = append(out, "*")
			continue
		}
		if pfx, ok := strings.CutSuffix(e, "/*"); ok {
			out = append(out, router.NormalizeRepo(pfx)+"/*")
			continue
		}
		out = append(out, router.NormalizeRepo(e))
	}
	return out
}

func hostFromIssuer(issuer string) string {
	u, err := url.Parse(issuer)
	if err != nil || u.Host == "" {
		return strings.TrimRight(issuer, "/")
	}
	return u.Host
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// asString coerces a JSON claim value (string, or occasionally bool/number) to a string for comparison.
func asString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case fmt.Stringer:
		return t.String()
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", t)
	}
}
