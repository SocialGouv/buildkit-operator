package identity

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadConfig_FileAndEnvOverlay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oidc.yaml")
	const body = `
providers:
  - type: github
    issuer: https://token.actions.githubusercontent.com
    audience: buildkit-operator
repoAllowlist:
  - github.com/socialgouv/*
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("file is the base", func(t *testing.T) {
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if len(cfg.Providers) != 1 || cfg.Providers[0].Type != "github" {
			t.Fatalf("providers = %+v", cfg.Providers)
		}
		if len(cfg.RepoAllowlist) != 1 {
			t.Fatalf("allowlist = %v", cfg.RepoAllowlist)
		}
		if cfg.Disable {
			t.Error("disable should default false")
		}
	})

	t.Run("env overlays the file", func(t *testing.T) {
		t.Setenv(EnvDisable, "true")
		t.Setenv(EnvAllowlist, "github.com/a/*, github.com/b/*")
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if !cfg.Disable {
			t.Error("env BUILDKIT_OPERATOR_OIDC_DISABLE=true must win")
		}
		if len(cfg.RepoAllowlist) != 2 {
			t.Errorf("env allowlist = %v, want 2 entries", cfg.RepoAllowlist)
		}
	})
}

func TestLoadConfig_EnvOnlyProviders(t *testing.T) {
	t.Setenv(EnvProviders, `[{"type":"gitlab","issuer":"https://gitlab.example.com","audience":"bko"}]`)
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Providers) != 1 || cfg.Providers[0].Type != "gitlab" {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
}

func TestLoadConfig_BadFile(t *testing.T) {
	if _, err := LoadConfig(filepath.Join(t.TempDir(), "does-not-exist.yaml")); err == nil {
		t.Fatal("expected error for missing config file")
	}
}
