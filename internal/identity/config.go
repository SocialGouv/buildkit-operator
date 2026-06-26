package identity

import (
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/yaml"
)

// Env var names for the file-less configuration path (when no ConfigMap is mounted).
const (
	EnvProviders = "BUILDKIT_OPERATOR_OIDC_PROVIDERS"      // JSON array of Provider
	EnvAllowlist = "BUILDKIT_OPERATOR_OIDC_REPO_ALLOWLIST" // comma/space-separated repo globs
	EnvDisable   = "BUILDKIT_OPERATOR_OIDC_DISABLE"        // "true" → OIDC off (admin break-glass)
)

// LoadConfig assembles the OIDC Config. When path is non-empty it is the base (a mounted ConfigMap file,
// YAML or JSON); env vars then OVERLAY it (so an operator can flip Disable or extend the allowlist via
// the Deployment env without re-templating the ConfigMap). Both are admin-controlled surfaces — the
// secure default is verification ON; only an operator who owns the Deployment/ConfigMap can relax it.
func LoadConfig(path string) (Config, error) {
	var cfg Config
	if path != "" {
		b, err := os.ReadFile(path)
		if err != nil {
			return Config{}, fmt.Errorf("read oidc config %q: %w", path, err)
		}
		if err := yaml.Unmarshal(b, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse oidc config %q: %w", path, err)
		}
	}
	if v := strings.TrimSpace(os.Getenv(EnvProviders)); v != "" {
		var ps []Provider
		if err := yaml.Unmarshal([]byte(v), &ps); err != nil {
			return Config{}, fmt.Errorf("parse %s: %w", EnvProviders, err)
		}
		cfg.Providers = ps
	}
	if v := strings.TrimSpace(os.Getenv(EnvAllowlist)); v != "" {
		cfg.RepoAllowlist = strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' })
	}
	if v := strings.TrimSpace(os.Getenv(EnvDisable)); v != "" {
		cfg.Disable = v == "true" || v == "1" || v == "yes"
	}
	return cfg, nil
}
