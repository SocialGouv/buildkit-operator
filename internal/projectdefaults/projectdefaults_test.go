package projectdefaults

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	bkov1 "github.com/socialgouv/buildkit-operator/api/v1alpha1"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "rules.yaml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	// Empty path = feature off.
	if cfg, err := Load(""); err != nil || cfg != nil {
		t.Fatalf("Load(\"\") = %v, %v; want nil, nil", cfg, err)
	}

	cfg, err := Load(writeConfig(t, `
rules:
  - repo: github.com/socialgouv/iterion
    name: "iterion-sandbox-*"
    cacheVolumeGi: 120
  - repo: "github.com/socialgouv/*"
    tier: hot
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules) != 2 {
		t.Fatalf("rules = %d, want 2", len(cfg.Rules))
	}

	for name, content := range map[string]string{
		"bad tier":    `rules: [{tier: scalding}]`,
		"bad pattern": `rules: [{repo: "github.com/[socialgouv/x"}]`,
		"negative":    `rules: [{idleTimeoutSec: -5}]`,
	} {
		if _, err := Load(writeConfig(t, content)); err == nil {
			t.Errorf("%s: Load accepted an invalid config", name)
		}
	}

	if _, err := Load(filepath.Join(t.TempDir(), "absent.yaml")); err == nil || !strings.Contains(err.Error(), "read") {
		t.Errorf("missing file: err = %v, want read error", err)
	}
}

func TestApply(t *testing.T) {
	cfg := &Config{Rules: []Rule{
		{Repo: "github.com/socialgouv/iterion", Name: "iterion-sandbox-*", CacheVolumeGi: 120, IdleTimeoutSec: 3600},
		{Repo: "github.com/socialgouv/iterion", Tier: bkov1.TierHot},
		{Repo: "github.com/other/*", Arch: "arm64", Tier: bkov1.TierHot},
	}}

	mk := func(repo, name, arch string) *bkov1.BuildProjectSpec {
		return &bkov1.BuildProjectSpec{Key: "p", Repo: repo, Name: name, Arch: arch}
	}

	// First match wins: the sandbox rule seeds cache + idle and stops there (no tier from rule 2).
	spec := mk("github.com/socialgouv/iterion", "iterion-sandbox-base", "amd64")
	cfg.Apply(spec)
	if spec.CacheVolumeGi != 120 || spec.IdleTimeoutSec != 3600 || spec.Tier != "" {
		t.Errorf("sandbox spec = cache %d idle %d tier %q; want 120/3600/\"\"", spec.CacheVolumeGi, spec.IdleTimeoutSec, spec.Tier)
	}

	// Other names of the same repo fall through to the tier rule.
	spec = mk("github.com/socialgouv/iterion", "iterion", "amd64")
	cfg.Apply(spec)
	if spec.Tier != bkov1.TierHot || spec.CacheVolumeGi != 0 {
		t.Errorf("app spec = tier %q cache %d; want hot/0", spec.Tier, spec.CacheVolumeGi)
	}

	// Arch-restricted rule.
	spec = mk("github.com/other/repo", "", "amd64")
	cfg.Apply(spec)
	if spec.Tier != "" {
		t.Errorf("amd64 spec matched an arm64-only rule (tier %q)", spec.Tier)
	}
	spec = mk("github.com/other/repo", "", "arm64")
	cfg.Apply(spec)
	if spec.Tier != bkov1.TierHot {
		t.Errorf("arm64 spec = tier %q, want hot", spec.Tier)
	}

	// No match / nil config: spec untouched.
	spec = mk("github.com/unrelated/repo", "", "amd64")
	cfg.Apply(spec)
	if spec.Tier != "" || spec.CacheVolumeGi != 0 {
		t.Error("unmatched spec must stay zero-valued")
	}
	(*Config)(nil).Apply(spec) // must not panic
}
