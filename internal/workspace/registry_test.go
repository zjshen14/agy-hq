package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeBuiltin struct {
	vaultDir string
}

func (f fakeBuiltin) ID() string                 { return "core/test-fake" }
func (f fakeBuiltin) Capabilities() Capabilities { return Capabilities{Create: true, Changes: true} }
func (f fakeBuiltin) Description() string {
	return "Fake built-in provider for tests\nwith a second line"
}
func (f fakeBuiltin) ForVault(vaultDir string) Provider {
	return fakeBuiltin{vaultDir: vaultDir}
}
func (f fakeBuiltin) Probe(context.Context, string) (Info, bool, error) { return Info{}, false, nil }
func (f fakeBuiltin) List(context.Context, ListRequest) ([]Info, string, error) {
	return nil, "", nil
}
func (f fakeBuiltin) Create(context.Context, CreateRequest) (CreateResult, error) {
	return CreateResult{}, nil
}
func (f fakeBuiltin) Status(context.Context, string) (Info, error) { return Info{}, nil }
func (f fakeBuiltin) Remove(context.Context, RemoveRequest) error  { return nil }

type badIDProvider struct{ fakeBuiltin }

func (badIDProvider) ID() string { return "acme-monorepo/sneaky" }

func TestRegisterBuiltinResolveAndList(t *testing.T) {
	RegisterBuiltin(fakeBuiltin{})
	vaultDir := t.TempDir()
	p, err := Resolve(vaultDir, "core/test-fake")
	if err != nil {
		t.Fatalf("Resolve builtin: %v", err)
	}
	if fb, ok := p.(fakeBuiltin); !ok || fb.vaultDir != vaultDir {
		t.Fatalf("expected Resolve to bind the builtin to the Vault, got %#v", p)
	}
	var st *ProviderStatus
	for _, s := range ListProviders(vaultDir) {
		if s.FQID == "core/test-fake" {
			s := s
			st = &s
		}
	}
	if st == nil || !st.Builtin || !st.Trusted || !st.Capabilities.Create || strings.Contains(st.Description, "\n") || st.Description == "" {
		t.Fatalf("unexpected builtin status: %+v", st)
	}

	for _, bad := range []Provider{nil, badIDProvider{}} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("expected RegisterBuiltin(%T) to panic", bad)
				}
			}()
			RegisterBuiltin(bad)
		}()
	}

	cases := map[string]string{
		"":                      CodeInvalid,
		"no-slash":              CodeInvalid,
		"a/b/c":                 CodeInvalid,
		"acme-monorepo/missing": CodeNotFound,
	}
	for fqid, want := range cases {
		if _, err := Resolve(vaultDir, fqid); ErrorCode(err) != want {
			t.Fatalf("Resolve(%q) = %v, want %s", fqid, err, want)
		}
	}
	if _, err := Resolve("", "acme-monorepo/client"); ErrorCode(err) != CodeInvalid {
		t.Fatalf("Resolve without a Vault = %v, want CodeInvalid", err)
	}
}

func TestListProvidersReportsTrustedPackProvider(t *testing.T) {
	fx := setupAcmePack(t, "", true)
	var st *ProviderStatus
	for _, s := range ListProviders(fx.vaultDir) {
		if s.FQID == "acme-monorepo/client" {
			s := s
			st = &s
		}
	}
	if st == nil {
		t.Fatal("pack provider missing from ListProviders")
	}
	if !st.Trusted || st.Reason != "" || st.Builtin || st.PackID != "acme-monorepo" {
		t.Fatalf("unexpected trust verdict: %+v", st)
	}
	if !st.Capabilities.Create || !st.Capabilities.SharedHistory || st.StatusRefreshSeconds != 600 || st.Skill != "acme-workspace-guide" {
		t.Fatalf("unexpected manifest projection: %+v", st)
	}
	if len(st.RootPatterns) != 1 || st.RootPatterns[0] != "/srv/ws/*/*" || !strings.Contains(st.Description, "acme-ws") {
		t.Fatalf("unexpected root patterns/description: %+v", st)
	}

	// Tampering with the pinned skill guide disables only this provider.
	skill := filepath.Join(fx.packDir, "skills", "acme-workspace-guide", "SKILL.md")
	if err := os.WriteFile(skill, []byte("# Tampered guide with different length\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, s := range ListProviders(fx.vaultDir) {
		if s.FQID == "acme-monorepo/client" && (s.Trusted || !strings.Contains(s.Reason, "skill guide")) {
			t.Fatalf("expected tampered skill to disable the provider, got %+v", s)
		}
	}
	if _, err := Resolve(fx.vaultDir, "acme-monorepo/client"); ErrorCode(err) != CodeUnavailable {
		t.Fatalf("Resolve with tampered skill = %v, want CodeUnavailable", err)
	}
}

func TestUntrustedPackProvidersAreInvisible(t *testing.T) {
	fx := setupAcmePack(t, "", false)
	for _, s := range ListProviders(fx.vaultDir) {
		if s.FQID == "acme-monorepo/client" {
			t.Fatalf("untrusted pack provider must not be listed: %+v", s)
		}
	}
	if _, err := Resolve(fx.vaultDir, "acme-monorepo/client"); ErrorCode(err) != CodeNotFound {
		t.Fatalf("Resolve(untrusted) = %v, want CodeNotFound", err)
	}
	if _, _, ok := MatchRootPattern(fx.vaultDir, "/srv/ws/user/ledger-fix/main.go"); ok {
		t.Fatal("untrusted pack root_patterns must not match")
	}
}

func TestMatchRootPatternDeepestAncestorWithoutSubprocess(t *testing.T) {
	fx := setupAcmePack(t, "", true)
	fqid, root, ok := MatchRootPattern(fx.vaultDir, "/srv/ws/user/ledger-fix/src/billing/round.go")
	if !ok || fqid != "acme-monorepo/client" || root != "/srv/ws/user/ledger-fix" {
		t.Fatalf("MatchRootPattern = %q %q %v", fqid, root, ok)
	}
	if _, root, ok := MatchRootPattern(fx.vaultDir, "/srv/ws/user/ledger-fix"); !ok || root != "/srv/ws/user/ledger-fix" {
		t.Fatalf("MatchRootPattern(root itself) = %q %v", root, ok)
	}
	for _, p := range []string{"/srv/ws/user", "/opt/other/file.go", "relative/path", ""} {
		if _, _, ok := MatchRootPattern(fx.vaultDir, p); ok {
			t.Fatalf("MatchRootPattern(%q) unexpectedly matched", p)
		}
	}
	// Matching is a pure manifest/filesystem check: the provider binary is never executed.
	if lines := fx.logLines(t); len(lines) != 0 {
		t.Fatalf("MatchRootPattern spawned the provider: %v", lines)
	}
}

func TestMatchRootPatternIgnoresVaultAndResolvesSymlinks(t *testing.T) {
	var base, outside string
	fx := setupAcmePackWith(t, func(fx acmeFixture) string {
		base = filepath.Dir(fx.vaultDir)
		outside = CanonicalPath(t.TempDir())
		patterns := fmt.Sprintf(`"root_patterns": [%q, %q]`, base+"/*", outside+"/ws/*")
		return strings.Replace(acmePackManifest, `"root_patterns": ["/srv/ws/*/*"]`, patterns, 1)
	}, true)

	for _, p := range []string{
		filepath.Join(fx.vaultDir, "notes", "todo.md"),
		filepath.Join(fx.vaultDir, "worktrees", "repo-task", "main.go"),
		fx.vaultDir,
	} {
		if fqid, root, ok := MatchRootPattern(fx.vaultDir, p); ok {
			t.Fatalf("path inside the Vault matched %s at %s: %s", fqid, root, p)
		}
	}
	if _, root, ok := MatchRootPattern(fx.vaultDir, filepath.Join(base, "elsewhere", "file.go")); !ok || root != filepath.Join(base, "elsewhere") {
		t.Fatalf("expected sibling of the Vault to match, got %q %v", root, ok)
	}

	real := filepath.Join(outside, "ws", "proj")
	if err := os.MkdirAll(filepath.Join(real, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(CanonicalPath(t.TempDir()), "proj-link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if fqid, root, ok := MatchRootPattern(fx.vaultDir, filepath.Join(link, "src", "new.go")); !ok || fqid != "acme-monorepo/client" || root != real {
		t.Fatalf("expected symlinked path to resolve to %s, got %q %q %v", real, fqid, root, ok)
	}
}

func TestMatchRootPatternNeverClaimsARootContainingTheVault(t *testing.T) {
	var parent string
	fx := setupAcmePackWith(t, func(fx acmeFixture) string {
		parent = filepath.Dir(filepath.Dir(fx.vaultDir))
		return strings.Replace(acmePackManifest, `"root_patterns": ["/srv/ws/*/*"]`, fmt.Sprintf(`"root_patterns": [%q]`, parent+"/*"), 1)
	}, true)
	vaultContainer := filepath.Dir(fx.vaultDir)
	if fqid, root, ok := MatchRootPattern(fx.vaultDir, filepath.Join(vaultContainer, "notes.md")); ok {
		t.Fatalf("root %s containing the Vault was claimed by %s", root, fqid)
	}
}
