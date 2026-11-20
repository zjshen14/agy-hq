package ci

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTestFile(t *testing.T, root, rel, content string, perm os.FileMode) {
	t.Helper()
	abs := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), perm); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func seedSyntheticGoRepo(t *testing.T, modPath string) string {
	t.Helper()
	root := t.TempDir()
	writeTestFile(t, root, "go.mod", fmt.Sprintf("module %s\n\ngo 1.24\n", modPath), 0o644)
	return root
}

func TestGateH5A_NoProdTestingImports(t *testing.T) {
	root := seedSyntheticGoRepo(t, "example.com/h5a")
	writeTestFile(t, root, "cmd/app/main.go", "package main\nfunc main() {}\n", 0o644)
	writeTestFile(t, root, "cmd/app/main_test.go", "package main\nimport \"testing\"\nfunc TestOK(t *testing.T) {}\n", 0o644)

	ws := newSlopWorkspace(root)
	if ok, detail := checkGateH5ANoProdTestingImport(ws); !ok {
		t.Fatalf("expected H5-A pass with testing only in _test.go, got: %s", detail)
	}

	writeTestFile(t, root, "internal/leak/leak.go", "package leak\nimport _ \"testing\"\nfunc Helper() {}\n", 0o644)
	wsBad := newSlopWorkspace(root)
	ok, detail := checkGateH5ANoProdTestingImport(wsBad)
	if ok || !strings.Contains(detail, "internal/leak/leak.go:2") {
		t.Fatalf("expected H5-A failure on internal/leak/leak.go:2, got ok=%v detail=%q", ok, detail)
	}
}

func TestGateH5B_DeadUnexportedSymbols(t *testing.T) {
	root := seedSyntheticGoRepo(t, "example.com/h5b")

	// Live package exercising init, _, main, generics, promoted methods, method values, and interface satisfaction.
	writeTestFile(t, root, "internal/live/live.go", `package live

var _ = initMarker()

func init() {}

func initMarker() int { return 1 }

type inner struct{}

func (inner) promoted() {}

type outer struct{ inner }

type runner interface {
	promoted()
}

type box[T any] struct{ v T }

func (b box[T]) get() T { return b.v }

func Run() int {
	var o outer
	var r runner = o
	r.promoted()
	fn := o.promoted
	fn()
	b := box[int]{v: 7}
	return b.get()
}
`, 0o644)

	// Build-tag excluded file with an unexported symbol must be ignored by build.Default.MatchFile on non-matching OS.
	writeTestFile(t, root, "internal/live/ignored_custom.go", `//go:build h5_nonexistent_tag

package live

func excludedByBuildTag() {}
`, 0o644)

	// Test-only package must be skipped cleanly without failing.
	writeTestFile(t, root, "internal/e2e/e2e_test.go", "package e2e\nimport \"testing\"\nfunc TestDummy(t *testing.T) {}\n", 0o644)

	writeTestFile(t, root, "cmd/app/main.go", "package main\nimport \"example.com/h5b/internal/live\"\nfunc main() { _ = live.Run() }\n", 0o644)

	wsClean := newSlopWorkspace(root)
	if ok, detail := checkGateH5BDeadUnexportedSymbols(wsClean); !ok {
		t.Fatalf("expected H5-B pass on clean repo, got: %s", detail)
	}

	// Add dead unexported func, method, type, var, const, unused interface, and prod helper only called in _test.go.
	writeTestFile(t, root, "internal/live/slop.go", `package live

const deadConst = 42

var deadVar = "unused"

type deadType struct{}

func (deadType) deadMethod() {}

type deadIface interface {
	deadIfaceMethod()
}

type deadImpl struct{}

func (deadImpl) deadIfaceMethod() {}

func deadChainA() { deadChainB() }
func deadChainB() {}

func testOnlyProdHelper() {}
`, 0o644)
	writeTestFile(t, root, "internal/live/slop_test.go", "package live\nimport \"testing\"\nfunc TestX(t *testing.T) { testOnlyProdHelper() }\n", 0o644)

	wsBad := newSlopWorkspace(root)
	ok, detail := checkGateH5BDeadUnexportedSymbols(wsBad)
	if ok {
		t.Fatalf("expected H5-B failure on dead symbols, got pass: %s", detail)
	}
	for _, want := range []string{"deadConst", "deadVar", "deadType", "deadMethod", "deadIface", "deadImpl", "deadIfaceMethod", "deadChainA", "deadChainB", "testOnlyProdHelper"} {
		if !strings.Contains(detail, want) {
			t.Errorf("expected H5-B detail to report %q, got: %s", want, detail)
		}
	}
}

func TestGateH5C_NoUnlockedVaultWrites(t *testing.T) {
	root := seedSyntheticGoRepo(t, "example.com/h5c")

	// internal/store is allowed to define WriteAtomic / raw writes.
	writeTestFile(t, root, "internal/store/store.go", `package store
import "os"
func WriteAtomic(p string, b []byte, m os.FileMode) error { return os.WriteFile(p, b, m) }
`, 0o644)

	// Non-ranked file + store.WriteAtomic on ranked file are both clean.
	writeTestFile(t, root, "internal/safe/safe.go", `package safe
import (
	"os"
	"path/filepath"
	"example.com/h5c/internal/store"
)
func SaveSafe(v string) error {
	_ = os.WriteFile(filepath.Join(v, "memory", "budget.json"), []byte("{}"), 0o600)
	_ = os.WriteFile(filepath.Join(v, "memory", "tasks_archive.jsonl"), []byte(""), 0o600)
	return store.WriteAtomic(filepath.Join(v, "memory", "tasks.json"), []byte("[]"), 0o644)
}
`, 0o644)

	wsClean := newSlopWorkspace(root)
	if ok, detail := checkGateH5CNoUnlockedVaultWrites(wsClean); !ok {
		t.Fatalf("expected H5-C pass, got: %s", detail)
	}

	// Raw os.WriteFile via direct literal, local var, helper func, and package const must all be caught.
	writeTestFile(t, root, "internal/bad/bad.go", `package bad
import (
	"os"
	"path/filepath"
)
const wsFile = "workspaces.json"
func goalsPath(v string) string { return filepath.Join(v, "memory", "goals.json") }
func BadWrites(v string) {
	_ = os.WriteFile(filepath.Join(v, "memory", "tasks.json"), []byte("[]"), 0o644)
	ep := filepath.Join(v, "memory", "events.json")
	_ = os.WriteFile(ep, []byte("[]"), 0o600)
	_ = os.WriteFile(goalsPath(v), []byte("[]"), 0o600)
	_ = os.WriteFile(filepath.Join(v, "memory", wsFile), []byte("{}"), 0o600)
}
`, 0o644)

	wsBad := newSlopWorkspace(root)
	ok, detail := checkGateH5CNoUnlockedVaultWrites(wsBad)
	if ok {
		t.Fatalf("expected H5-C failure on raw os.WriteFile writes, got pass: %s", detail)
	}
	for _, want := range []string{"tasks.json", "events.json", "goals.json", "workspaces.json"} {
		if !strings.Contains(detail, want) {
			t.Errorf("expected H5-C detail to include %q, got: %s", want, detail)
		}
	}
}

func TestGateH5D_SidecarParity(t *testing.T) {
	root := t.TempDir()
	seedSidecar := func(base string, launchPerm os.FileMode, mainBody string) {
		writeTestFile(t, root, base+"/main.mjs", mainBody, 0o755)
		writeTestFile(t, root, base+"/launch.sh", "#!/bin/sh\n", launchPerm)
		writeTestFile(t, root, base+"/sidecar.json", "{\"entrypoint\":\"launch.sh\"}\n", 0o644)
	}
	seedSidecar(".agents/plugins/agy-hq/sidecars/agy-hq-console", 0o755, "console.log('ok');\n")
	seedSidecar("templates/sidecars/agy-hq-console", 0o755, "console.log('ok');\n")

	if ok, detail := checkGateH5DSidecarParity(root); !ok {
		t.Fatalf("expected H5-D pass on synced sidecar dirs, got: %s", detail)
	}

	// Executable mode drift on launch.sh
	if err := os.Chmod(filepath.Join(root, "templates/sidecars/agy-hq-console/launch.sh"), 0o644); err != nil {
		t.Fatal(err)
	}
	if ok, detail := checkGateH5DSidecarParity(root); ok || !strings.Contains(detail, "executable mode drift") {
		t.Fatalf("expected H5-D mode drift failure, got ok=%v detail=%q", ok, detail)
	}
	_ = os.Chmod(filepath.Join(root, "templates/sidecars/agy-hq-console/launch.sh"), 0o755)

	// Byte drift (whitespace)
	writeTestFile(t, root, "templates/sidecars/agy-hq-console/main.mjs", "console.log('ok');\n\n", 0o755)
	if ok, detail := checkGateH5DSidecarParity(root); ok || !strings.Contains(detail, "byte drift") {
		t.Fatalf("expected H5-D byte drift failure, got ok=%v detail=%q", ok, detail)
	}
	writeTestFile(t, root, "templates/sidecars/agy-hq-console/main.mjs", "console.log('ok');\n", 0o755)

	// Extra file in templates/sidecars/agy-hq-console
	writeTestFile(t, root, "templates/sidecars/agy-hq-console/extra.txt", "drift\n", 0o644)
	if ok, detail := checkGateH5DSidecarParity(root); ok || !strings.Contains(detail, "extra.txt") {
		t.Fatalf("expected H5-D extra file failure, got ok=%v detail=%q", ok, detail)
	}
}

func TestGateH5E_CUJDocParity(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, root, "internal/e2e/cuj_e2e_test.go", `package e2e
import "testing"
func TestCUJ1_First(t *testing.T) {}
func TestCUJ40_Layer1(t *testing.T) {}
func helperNotACUJ() {}
`, 0o644)
	writeTestFile(t, root, "docs/product/0003-critical-user-journeys.md", "| CUJ-1 | `TestCUJ1_First` |\n| CUJ-40 | `TestCUJ40_Layer1` |\n", 0o644)

	if ok, detail := checkGateH5ECUJDocParity(root); !ok {
		t.Fatalf("expected H5-E pass when all TestCUJ* are documented, got: %s", detail)
	}

	// Only prefix TestCUJ4 (not TestCUJ40_Layer1) must fail whole-token match.
	writeTestFile(t, root, "docs/product/0003-critical-user-journeys.md", "| CUJ-1 | `TestCUJ1_First` |\n| CUJ-4 | `TestCUJ4_Other` |\n", 0o644)
	if ok, detail := checkGateH5ECUJDocParity(root); ok || !strings.Contains(detail, "TestCUJ40_Layer1") {
		t.Fatalf("expected H5-E failure for missing TestCUJ40_Layer1, got ok=%v detail=%q", ok, detail)
	}
}

func TestGateH5F_MonolithLOCCeilings(t *testing.T) {
	root := t.TempDir()
	// Missing files are skipped cleanly in synthetic repos.
	writeTestFile(t, root, "cmd/hq/main.go", "package main\nfunc main() {}\n", 0o644)
	if ok, detail := checkGateH5FMonolithLOCCeilings(root); !ok {
		t.Fatalf("expected H5-F pass below ceiling, got: %s", detail)
	}

	// Exceeding internal/kernel/workspace_lifecycle.go ceiling must fail.
	bloated := "package kernel\n" + strings.Repeat("// line\n", 1800)
	writeTestFile(t, root, "internal/kernel/workspace_lifecycle.go", bloated, 0o644)
	if ok, detail := checkGateH5FMonolithLOCCeilings(root); ok || !strings.Contains(detail, "internal/kernel/workspace_lifecycle.go") {
		t.Fatalf("expected H5-F failure when workspace_lifecycle.go exceeds ceiling, got ok=%v detail=%q", ok, detail)
	}
}

func TestGateH5_MinimalSyntheticRepoDoesNotCrash(t *testing.T) {
	emptyRoot := t.TempDir()
	for _, r := range checkGateH5Slop(emptyRoot) {
		if !r.Passed {
			t.Errorf("expected Gate %s to skip cleanly on minimal synthetic repo without failing, got: %s", r.Gate, r.Detail)
		}
	}

	// Verify full CheckHarness on a minimal synthetic repo (without cmd/, internal/, sidecars/, docs/, or cuj_e2e_test.go)
	// fails only on H1/H2/H3/H4-B as expected and never panics or fails H5-A..H5-F unexpectedly.
	minRoot := seedSyntheticGoRepo(t, "example.com/min")
	writeTestFile(t, minRoot, ".agents/plugins/agy-hq/hooks.json", `{"hooks":{}}`, 0o644)
	results, _ := CheckHarness(minRoot)
	for _, r := range results {
		if strings.HasPrefix(r.Gate, "H5-") && !r.Passed {
			t.Errorf("expected %s to pass/skip on minimal synthetic repo without cmd/ or internal/, got: %s", r.Gate, r.Detail)
		}
	}
}
