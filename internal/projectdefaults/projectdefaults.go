// Package projectdefaults applies platform-declared per-project defaults to
// auto-created BuildProjects. buildd's Ensure is create-only, so the moment a
// project is first routed is the one chance to seed a non-default tier / idle /
// cache size without hand-editing CRs keyed by hashed names. Rules live in a
// mounted config file (Helm: .Values.projectDefaults) — an admin-only surface,
// deliberately NOT settable by the routing caller (a repo must not be able to
// self-assign a hot daemon).
package projectdefaults

import (
	"fmt"
	"os"
	"path"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
	"sigs.k8s.io/yaml"
)

// Rule matches a project by its normalized identity and carries the spec
// fields to seed at creation. First matching rule wins; zero-valued fields of
// the winning rule leave the CRD defaults in charge.
type Rule struct {
	// Repo is a shell-style pattern (path.Match: `*` does not cross `/`)
	// against the NORMALIZED repo, e.g. "github.com/socialgouv/iterion" or
	// "github.com/socialgouv/*". Empty matches any repo.
	Repo string `json:"repo,omitempty"`
	// Name is a shell-style pattern against the normalized monorepo component
	// name, e.g. "iterion-sandbox-*". Empty matches any name (including none).
	Name string `json:"name,omitempty"`
	// Arch restricts the rule to one architecture (exact). Empty matches any.
	Arch string `json:"arch,omitempty"`

	// Tier seeds spec.tier (hot|warm). Empty keeps the CRD default.
	Tier string `json:"tier,omitempty"`
	// IdleTimeoutSec seeds spec.idleTimeoutSec. 0 keeps the CRD default.
	IdleTimeoutSec int32 `json:"idleTimeoutSec,omitempty"`
	// CacheVolumeGi seeds spec.cacheVolumeGi. 0 keeps the CRD default.
	CacheVolumeGi int32 `json:"cacheVolumeGi,omitempty"`
}

// Config is the mounted rules file ({rules: [...]}).
type Config struct {
	Rules []Rule `json:"rules,omitempty"`
}

// Load reads and validates a rules file (YAML or JSON). An empty path returns
// nil (feature off). Invalid patterns or enum values fail loudly at boot —
// a silently-dropped rule would read as "defaults mysteriously not applied".
func Load(configPath string) (*Config, error) {
	if configPath == "" {
		return nil, nil
	}
	b, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("read project-defaults config %q: %w", configPath, err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse project-defaults config %q: %w", configPath, err)
	}
	for i, r := range cfg.Rules {
		if _, err := path.Match(r.Repo, "probe"); r.Repo != "" && err != nil {
			return nil, fmt.Errorf("project-defaults rule %d: bad repo pattern %q: %w", i, r.Repo, err)
		}
		if _, err := path.Match(r.Name, "probe"); r.Name != "" && err != nil {
			return nil, fmt.Errorf("project-defaults rule %d: bad name pattern %q: %w", i, r.Name, err)
		}
		if r.Tier != "" && r.Tier != bkov1.TierHot && r.Tier != bkov1.TierWarm {
			return nil, fmt.Errorf("project-defaults rule %d: bad tier %q (hot|warm)", i, r.Tier)
		}
		if r.IdleTimeoutSec < 0 || r.CacheVolumeGi < 0 {
			return nil, fmt.Errorf("project-defaults rule %d: negative idleTimeoutSec/cacheVolumeGi", i)
		}
	}
	return &cfg, nil
}

// Apply seeds spec fields from the first rule matching the spec's normalized
// identity. Nil-config safe (no-op). Children derived from the canonical spec
// (forks, clones) get their own policy via DeriveChild afterwards.
func (c *Config) Apply(spec *bkov1.BuildProjectSpec) {
	if c == nil {
		return
	}
	for _, r := range c.Rules {
		if !match(r.Repo, spec.Repo) || !match(r.Name, spec.Name) {
			continue
		}
		if r.Arch != "" && r.Arch != spec.Arch {
			continue
		}
		if r.Tier != "" {
			spec.Tier = r.Tier
		}
		if r.IdleTimeoutSec > 0 {
			spec.IdleTimeoutSec = r.IdleTimeoutSec
		}
		if r.CacheVolumeGi > 0 {
			spec.CacheVolumeGi = r.CacheVolumeGi
		}
		return
	}
}

// match reports whether the shell-style pattern accepts value ("" = any).
// Patterns are validated at Load time, so a Match error cannot occur here.
func match(pattern, value string) bool {
	if pattern == "" {
		return true
	}
	ok, _ := path.Match(pattern, value)
	return ok
}
