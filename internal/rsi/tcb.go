package rsi

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Ring0ProtectedPaths lists paths inside the Immutable Trusted Computing Base (Ring 0)
// that no RSI hypothesis or self-patch is ever permitted to modify.
var Ring0ProtectedPaths = []string{
	"internal/approvals/",
	"internal/rsi/tcb.go",
	"internal/rsi/verifier.go",
	".gitignore",
	".git/hooks/",
	"local/.gitignore",
	"templates/gitignore.tmpl",
}

// IsRing0Path returns true if relOrAbsPath touches any Ring 0 Immutable TCB file.
func IsRing0Path(p string) bool {
	slash := filepath.ToSlash(filepath.Clean(p))
	slash = strings.TrimPrefix(slash, "./")
	for _, prot := range Ring0ProtectedPaths {
		if slash == strings.TrimSuffix(prot, "/") ||
			strings.HasPrefix(slash, prot) ||
			strings.Contains(slash, "/"+prot) ||
			strings.HasSuffix(slash, "/"+strings.TrimSuffix(prot, "/")) {
			return true
		}
	}
	return false
}

// CheckTCBAndAntiGoodhart executes Gate 1 (Static Trusted Computing Base, Taint Isolation,
// Anti-Collapse Wholesale-Rewrite Check, and Go AST Anti-Goodharting Test Check).
func CheckTCBAndAntiGoodhart(rootDir, vaultDir string, h *RSIHypothesis, observations []FrictionEvent) (bool, string) {
	if h == nil {
		return false, "nil hypothesis"
	}

	// 1. Ring 0 Immutable TCB boundary check on TargetFiles and ProposedDiff headers
	if h.Ring == Ring0ImmutableTCB {
		return false, "Ring 0 (Immutable TCB) mutations are unconditionally prohibited"
	}
	for _, tf := range h.TargetFiles {
		if IsRing0Path(tf) {
			return false, fmt.Sprintf("Gate 1 violation: target file %q is inside Ring 0 Immutable TCB", tf)
		}
	}
	for _, line := range strings.Split(h.ProposedDiff, "\n") {
		if strings.HasPrefix(line, "--- a/") || strings.HasPrefix(line, "+++ b/") {
			p := strings.TrimSpace(line[6:])
			if IsRing0Path(p) {
				return false, fmt.Sprintf("Gate 1 violation: diff header %q touches Ring 0 Immutable TCB", p)
			}
		}
	}

	// 2. Quarantined Taint-to-Code Isolation: tainted sessions cannot source Ring 1 or Ring 2 patches
	obsByID := make(map[string]FrictionEvent, len(observations))
	for _, ev := range observations {
		obsByID[ev.ID] = ev
	}
	for _, oid := range h.ObservationIDs {
		if ev, ok := obsByID[oid]; ok && ev.Tainted && (h.Ring == Ring1KernelCode || h.Ring == Ring2SkillTrigger) {
			return false, fmt.Sprintf("Gate 1 violation: source observation %q originated in a quarantined (tainted) session and cannot mutate Ring %d", oid, h.Ring)
		}
	}

	// 3. Anti-Collapse Wholesale-Rewrite Check on SKILL.md, AGENTS.md, and MEMORY.md
	for _, tf := range h.TargetFiles {
		base := filepath.Base(tf)
		if base == "SKILL.md" || base == "AGENTS.md" || base == "MEMORY.md" {
			absPath := tf
			if !filepath.IsAbs(absPath) {
				absPath = filepath.Join(rootDir, filepath.FromSlash(tf))
			}
			if existingBytes, err := os.ReadFile(absPath); err == nil {
				existingLines := len(strings.Split(strings.TrimSpace(string(existingBytes)), "\n"))
				deletedLines := countDiffDeletions(h.ProposedDiff)
				if existingLines >= 10 && float64(deletedLines)/float64(existingLines) > 0.30 {
					return false, fmt.Sprintf("Gate 1 violation: wholesale rewrite blocked on %s (deletes %d of %d existing lines, >30%% cap)", tf, deletedLines, existingLines)
				}
			}
		}
	}

	// 4. Ring 1 Kernel/Hook Code Anti-Goodharting & Additive Schema Checks
	if h.Ring == Ring1KernelCode {
		if strings.TrimSpace(h.ReplayTestName) == "" || !strings.HasPrefix(h.ReplayTestName, "Test") {
			return false, "Gate 1 violation: Ring 1 code patch must declare a valid ReplayTestName (Test*)"
		}
		if ok, reason := checkDiffAntiGoodhartAST(rootDir, h.ProposedDiff); !ok {
			return false, reason
		}
		if ok, reason := checkAdditiveJSONStructTags(h.ProposedDiff); !ok {
			return false, reason
		}
	}

	return true, "Gate 1 PASS (Ring 0 untouched, taint-clean, AST & schema verified)"
}

func countDiffDeletions(diff string) int {
	deleted := 0
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			deleted++
		}
	}
	return deleted
}

var (
	reDeletedTestFunc = regexp.MustCompile(`^-\s*func\s+(Test\w+)\s*\(\s*\w+\s+\*testing\.T\s*\)`)
	reJSONTag         = regexp.MustCompile("`json:\"([^\"]+)\"`")
)

// checkDiffAntiGoodhartAST inspects unified diffs and any full Go test source snippets using go/parser & go/ast
// to reject test deletion, t.Skip/t.SkipNow/t.Skipf injection, or os.Exit(0) short-circuiting.
func checkDiffAntiGoodhartAST(rootDir, diff string) (bool, string) {
	addedTestFuncs := make(map[string]bool)
	deletedTestFuncs := make(map[string]bool)
	var addedGoLines []string

	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			if m := reDeletedTestFunc.FindStringSubmatch(line); len(m) == 2 {
				deletedTestFuncs[m[1]] = true
			}
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			addedLine := line[1:]
			addedGoLines = append(addedGoLines, addedLine)
			if m := reDeletedTestFunc.FindStringSubmatch("-" + addedLine); len(m) == 2 {
				addedTestFuncs[m[1]] = true
			}
		}
	}

	for fn := range deletedTestFuncs {
		if !addedTestFuncs[fn] {
			return false, fmt.Sprintf("Gate 1 Anti-Goodharting violation: Ring 1 diff deletes existing test function %s", fn)
		}
	}

	// Parse added Go statements inside a synthetic test wrapper using go/parser & go/ast
	// to detect t.Skip, t.SkipNow, t.Skipf, or os.Exit(0) calls.
	syntheticSrc := "package p\nfunc _check() {\n" + strings.Join(addedGoLines, "\n") + "\n}\n"
	fset := token.NewFileSet()
	if fileAST, err := parser.ParseFile(fset, "patch_check.go", syntheticSrc, 0); err == nil {
		var violation string
		ast.Inspect(fileAST, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			pkgIdent, _ := sel.X.(*ast.Ident)
			method := sel.Sel.Name
			if method == "Skip" || method == "SkipNow" || method == "Skipf" {
				violation = fmt.Sprintf("Gate 1 Anti-Goodharting AST violation: test bypass call `.%s(...)` is forbidden in RSI patches", method)
				return false
			}
			if pkgIdent != nil && pkgIdent.Name == "os" && method == "Exit" {
				violation = "Gate 1 Anti-Goodharting AST violation: `os.Exit(...)` is forbidden in RSI test patches"
				return false
			}
			return true
		})
		if violation != "" {
			return false, violation
		}
	} else {
		// Fallback lexical check if added lines are partial expressions/declarations across functions
		joined := strings.Join(addedGoLines, "\n")
		for _, forbidden := range []string{".Skip(", ".SkipNow(", ".Skipf(", "os.Exit(0)"} {
			if strings.Contains(joined, forbidden) {
				return false, fmt.Sprintf("Gate 1 Anti-Goodharting violation: forbidden test bypass %q detected in diff", forbidden)
			}
		}
	}

	return true, ""
}

func checkAdditiveJSONStructTags(diff string) (bool, string) {
	removedTags := make(map[string]bool)
	addedTags := make(map[string]bool)
	for _, line := range strings.Split(diff, "\n") {
		if strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") {
			for _, m := range reJSONTag.FindAllStringSubmatch(line, -1) {
				key := strings.Split(m[1], ",")[0]
				if key != "" && key != "-" {
					removedTags[key] = true
				}
			}
		}
		if strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") {
			for _, m := range reJSONTag.FindAllStringSubmatch(line, -1) {
				key := strings.Split(m[1], ",")[0]
				if key != "" && key != "-" {
					addedTags[key] = true
				}
			}
		}
	}
	for k := range removedTags {
		if !addedTags[k] {
			return false, fmt.Sprintf("Gate 1 Schema Invariant violation: diff removes or renames existing JSON struct tag `json:%q` (Vault schemas must be strictly additive)", k)
		}
	}
	return true, ""
}
