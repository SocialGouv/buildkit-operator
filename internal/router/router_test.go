package router

import "testing"

// The single most important correctness property of buildkit-operator: all references to
// the SAME logical project must collapse to the SAME ProjectKey, so concurrent
// and later builds converge on one daemon and share its cache. A regression here
// silently fragments the cache.
func TestProjectKey_SameProjectConverges(t *testing.T) {
	want := ProjectKey("https://github.com/Org/Repo.git", "", "", "amd64")
	same := []string{
		"https://github.com/org/repo",
		"https://github.com/org/repo/",
		"https://github.com/org/repo.git",
		"git@github.com:Org/Repo.git",
		"http://github.com/org/repo",
		"ssh://github.com/org/repo.git",
		"  https://GitHub.com/ORG/repo  ",
		"https://github.com:443/org/repo", // explicit port must not fragment the cache
		"ssh://git@github.com:22/org/repo.git",
	}
	for _, r := range same {
		if got := ProjectKey(r, "", "", "amd64"); got != want {
			t.Errorf("ProjectKey(%q) = %s, want %s (must converge)", r, got, want)
		}
	}
}

func TestProjectKey_DistinctWhenItShould(t *testing.T) {
	base := ProjectKey("github.com/org/repo", "", "", "amd64")
	cases := map[string]string{
		"diff arch":   ProjectKey("github.com/org/repo", "", "", "arm64"),
		"diff target": ProjectKey("github.com/org/repo", "", "builder", "amd64"),
		"diff repo":   ProjectKey("github.com/org/other", "", "", "amd64"),
		"diff name":   ProjectKey("github.com/org/repo", "api", "", "amd64"),
	}
	for name, k := range cases {
		if k == base {
			t.Errorf("%s: key %s collides with base (must differ)", name, k)
		}
	}
}

// The optional name segments a monorepo into per-component daemons, but an EMPTY name must be
// transparent: a single-image repo keeps the exact key it had before name existed (migration-safe).
func TestProjectKey_NameSegmentsMonorepo(t *testing.T) {
	noName := ProjectKey("github.com/org/mono", "", "", "amd64")
	api := ProjectKey("github.com/org/mono", "api", "", "amd64")
	web := ProjectKey("github.com/org/mono", "web", "", "amd64")
	if api == web || api == noName || web == noName {
		t.Error("distinct monorepo components must get distinct daemons")
	}
	// empty name is transparent + normalized (trim/case).
	if ProjectKey("r", "", "", "amd64") != ProjectKey("r", "  ", "", "amd64") {
		t.Error("blank name must normalize to empty (no segment)")
	}
	if ProjectKey("r", "API", "", "amd64") != ProjectKey("r", "api", "", "amd64") {
		t.Error("name must be case-normalized")
	}
}

func TestProjectKey_TargetAndArchNormalized(t *testing.T) {
	if ProjectKey("r", "", "", "amd64") != ProjectKey("r", "", "default", "x86_64") {
		t.Error("empty target must equal 'default'; x86_64 must equal amd64")
	}
	if ProjectKey("r", "", "T", "ARM64") != ProjectKey("r", "", "t", "aarch64") {
		t.Error("target/arch must be case- and alias-normalized")
	}
}

func TestProjectKey_FormatAndDeterministic(t *testing.T) {
	k := ProjectKey("github.com/org/repo", "", "", "amd64")
	if len(k) != 17 || k[0] != 'p' {
		t.Errorf("key %q: want 'p'+16 hex (len 17)", k)
	}
	if k != ProjectKey("github.com/org/repo", "", "", "amd64") {
		t.Error("ProjectKey must be deterministic")
	}
}

func TestNormalizeRepo(t *testing.T) {
	cases := map[string]string{
		"https://github.com/Org/Repo.git": "github.com/org/repo",
		"git@github.com:Org/Repo.git":     "github.com/org/repo",
		"https://github.com/org/repo/":    "github.com/org/repo",
		"ssh://git@gitlab.com/g/p.git":    "gitlab.com/g/p",
		"https://github.com:443/org/repo": "github.com/org/repo", // strip explicit port
		"ssh://git@gitlab.com:22/g/p.git": "gitlab.com/g/p",      // strip explicit ssh port
		"git@github.com:org/repo.git":     "github.com/org/repo", // scp colon is NOT a port
	}
	for in, want := range cases {
		if got := NormalizeRepo(in); got != want {
			t.Errorf("NormalizeRepo(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestForkKey_DistinctFromCanonical(t *testing.T) {
	canonical := ProjectKey("github.com/org/repo", "", "", "amd64")
	fork := ForkKey(canonical)
	if fork == canonical {
		t.Fatal("fork key must differ from canonical (no shared cache)")
	}
	if name := DaemonName(fork); len(name) > 63 {
		t.Errorf("fork daemon name %q exceeds 63", name)
	}
	if ForkKey(canonical) != fork {
		t.Error("ForkKey must be deterministic")
	}
}

func TestDaemonNameAndEndpoint(t *testing.T) {
	k := ProjectKey("github.com/org/repo", "", "", "amd64")
	name := DaemonName(k)
	if name != "buildkitd-"+k {
		t.Errorf("DaemonName = %q", name)
	}
	if len(name) > 63 {
		t.Errorf("DaemonName %q exceeds DNS-1123 limit (63)", name)
	}
	if got, want := Endpoint(k, "buildkit-operator", 1234), "tcp://"+name+".buildkit-operator.svc:1234"; got != want {
		t.Errorf("Endpoint = %q, want %q", got, want)
	}
}
