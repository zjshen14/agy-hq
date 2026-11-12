package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/gmail"
	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/rsi"
)

// HarnessCheckResult captures the pass/fail result of a single harness contract gate.
type HarnessCheckResult struct {
	Gate   string `json:"gate"` // H1, H2, H3
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

const MaxGlobalFrontmatterTokens = 2500

// CheckHarness executes Gates H1 (Skill Catalog & Frontmatter), H2 (Hook Parity & PreToolUse Matrix),
// and H3 (Model Routing, Inbox Classification & Proactive Trigger Matrix) as specified in RFC-0011 §5.
func CheckHarness(rootDir string) ([]HarnessCheckResult, bool) {
	var results []HarnessCheckResult
	allOK := true

	add := func(gate, name string, passed bool, detail string) {
		results = append(results, HarnessCheckResult{
			Gate:   gate,
			Name:   name,
			Passed: passed,
			Detail: detail,
		})
		if !passed {
			allOK = false
		}
	}

	// Gate H1: Agent Skill Catalog & Frontmatter Contract
	h1OK, h1Detail := checkGateH1Skills(rootDir)
	add("H1", "Skill Catalog, YAML Frontmatter & RSI Rule Schema", h1OK, h1Detail)

	// Gate H2: Hook Configuration Parity & PreToolUse Security Matrix
	h2ParityOK, h2ParityDetail := checkGateH2HooksJSONParity(rootDir)
	add("H2-A", "Workspace vs. Plugin hooks.json Parity", h2ParityOK, h2ParityDetail)

	h2MatrixOK, h2MatrixDetail := checkGateH2PreToolUseMatrix()
	add("H2-B", "PreToolUse Security & Non-Blocking Front Desk Matrix", h2MatrixOK, h2MatrixDetail)

	// Gate H3: Model Routing, Inbox Classification & Proactive Trigger Matrix
	h3RouteOK, h3RouteDetail := checkGateH3RoutingAndTriggers(rootDir)
	add("H3", "Model Routing, Inbox Classifier & Trigger Integrity", h3RouteOK, h3RouteDetail)

	// Gate H4-A: Go Formatting (gofmt -s) & Pure-Stdlib Module Contract (ADR-0002)
	h4aOK, h4aDetail := checkGateH4AGoContract(rootDir)
	add("H4-A", "Go Formatting (gofmt -s) & Zero-Dep Module Contract", h4aOK, h4aDetail)

	// Gate H4-B: Markdown Syntax, Relative Links & Docs Hub Index Integrity
	h4bOK, h4bDetail := checkGateH4BMarkdownAndDocsIntegrity(rootDir)
	add("H4-B", "Markdown Code Fences, Relative Links & Docs Index", h4bOK, h4bDetail)

	return results, allOK
}

func checkGateH1Skills(rootDir string) (bool, string) {
	skills := rsi.DiscoverAllSkillFiles(rootDir)
	if len(skills) == 0 {
		return false, "no SKILL.md files found under .agents/plugins/agy-hq/skills/"
	}
	if len(skills) > rsi.MaxGlobalSkills {
		return false, fmt.Sprintf("global skill count %d exceeds cap %d", len(skills), rsi.MaxGlobalSkills)
	}

	totalFrontmatterChars := 0
	seenRuleIDs := make(map[string]string)
	totalRules := 0

	for _, absPath := range skills {
		rel, err := filepath.Rel(rootDir, absPath)
		if err != nil {
			rel = absPath
		}
		rel = filepath.ToSlash(rel)
		expectedSlug := filepath.Base(filepath.Dir(absPath))

		rawBytes, err := os.ReadFile(absPath)
		if err != nil {
			return false, fmt.Sprintf("read %s: %v", rel, err)
		}
		content := string(rawBytes)
		trimmed := strings.TrimSpace(content)
		if !strings.HasPrefix(trimmed, "---\n") && !strings.HasPrefix(trimmed, "---\r\n") {
			return false, fmt.Sprintf("%s is missing opening YAML frontmatter delimiter '---'", rel)
		}
		rest := trimmed[4:]
		endIdx := strings.Index(rest, "\n---")
		if endIdx < 0 {
			return false, fmt.Sprintf("%s is missing closing YAML frontmatter delimiter '---'", rel)
		}
		fm := rest[:endIdx]
		totalFrontmatterChars += len(fm)

		if !strings.Contains(fm, "name: "+expectedSlug) && !strings.Contains(fm, "name: \""+expectedSlug+"\"") {
			return false, fmt.Sprintf("%s frontmatter must declare `name: %s` matching its folder name", rel, expectedSlug)
		}
		if !strings.Contains(fm, "description:") {
			return false, fmt.Sprintf("%s frontmatter is missing `description:`", rel)
		}

		lines := strings.Split(strings.TrimRight(content, "\n"), "\n")
		if len(lines) > rsi.MaxSkillMarkdownLines {
			return false, fmt.Sprintf("%s has %d lines (exceeds %d-line SKILL.md cap)", rel, len(lines), rsi.MaxSkillMarkdownLines)
		}

		rules := rsi.ParseRSIRules(rel, content)
		if len(rules) > rsi.MaxActiveRSIRules {
			return false, fmt.Sprintf("%s has %d active rsi:rule blocks (exceeds %d-rule cap)", rel, len(rules), rsi.MaxActiveRSIRules)
		}
		for _, r := range rules {
			totalRules++
			if strings.TrimSpace(r.ID) == "" || strings.TrimSpace(r.Source) == "" || strings.TrimSpace(r.ModelScope) == "" || strings.TrimSpace(r.Added) == "" {
				return false, fmt.Sprintf("%s contains rsi:rule with missing required metadata (id=%q source=%q model_scope=%q added=%q)", rel, r.ID, r.Source, r.ModelScope, r.Added)
			}
			if _, err := time.Parse("2006-01-02", r.Added); err != nil {
				return false, fmt.Sprintf("%s rsi:rule %s has invalid added date %q (expected YYYY-MM-DD)", rel, r.ID, r.Added)
			}
			if prevFile, dup := seenRuleIDs[r.ID]; dup {
				return false, fmt.Sprintf("duplicate rsi:rule id %q across %s and %s", r.ID, prevFile, rel)
			}
			seenRuleIDs[r.ID] = rel
		}

		lower := strings.ToLower(content)
		for _, forbidden := range []string{
			"bypass hq propose",
			"skip ask_question",
			"commit personal identity",
			"disable pre-commit",
			"send email directly without approval",
		} {
			if strings.Contains(lower, forbidden) {
				return false, fmt.Sprintf("%s contains forbidden safety bypass phrase %q", rel, forbidden)
			}
		}
	}

	estTokens := totalFrontmatterChars / 4
	if estTokens > MaxGlobalFrontmatterTokens {
		return false, fmt.Sprintf("global frontmatter token estimate %d exceeds cap %d", estTokens, MaxGlobalFrontmatterTokens)
	}

	return true, fmt.Sprintf("%d skills verified (<=%d lines each, %d active rsi:rule block(s), ~%d frontmatter tokens)", len(skills), rsi.MaxSkillMarkdownLines, totalRules, estTokens)
}

func checkGateH2HooksJSONParity(rootDir string) (bool, string) {
	wsHooksPath := filepath.Join(rootDir, ".agents", "hooks.json")
	pluginHooksPath := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "hooks.json")

	wsBytes, err := os.ReadFile(wsHooksPath)
	if err != nil {
		return false, fmt.Sprintf("missing .agents/hooks.json: %v", err)
	}
	plugBytes, err := os.ReadFile(pluginHooksPath)
	if err != nil {
		return false, fmt.Sprintf("missing .agents/plugins/agy-hq/hooks.json: %v", err)
	}

	if !bytes.Equal(bytes.TrimSpace(wsBytes), bytes.TrimSpace(plugBytes)) {
		return false, ".agents/hooks.json and .agents/plugins/agy-hq/hooks.json have drifted and must remain byte-for-byte identical"
	}

	var parsed map[string]interface{}
	if err := json.Unmarshal(wsBytes, &parsed); err != nil {
		return false, fmt.Sprintf("invalid JSON in .agents/hooks.json: %v", err)
	}

	rawStr := string(wsBytes)
	requiredMatchers := []string{
		"run_command",
		"edit_file",
		"write_file",
		"read_file",
		"grep_search",
		"find_by_name",
		"invoke_subagent",
		"manage_subagents",
		"schedule",
		"manage_task",
	}
	for _, m := range requiredMatchers {
		if !strings.Contains(rawStr, m) {
			return false, fmt.Sprintf(".agents/hooks.json PreToolUse matcher is missing required tool %q", m)
		}
	}

	return true, "Workspace and plugin hooks.json are valid and byte-identical (10/10 tool matchers present)"
}

func checkGateH2PreToolUseMatrix() (bool, string) {
	vaultDir, err := os.MkdirTemp("", "hq-ci-hook-matrix-*")
	if err != nil {
		return false, fmt.Sprintf("mkdirtemp: %v", err)
	}
	defer os.RemoveAll(vaultDir)

	runHook := func(payload map[string]interface{}) (string, error) {
		raw, _ := json.Marshal(payload)
		var out bytes.Buffer
		err := hooks.HandlePreToolUse(bytes.NewReader(raw), &out, vaultDir)
		return out.String(), err
	}

	// 1. Outbound commands must be denied; hq propose must be allowed
	deniedCmds := []string{
		"gh pr merge 42 --squash",
		"gh release create v1.0.0",
		"vercel --prod",
		"gws gmail send --to alice@example.com",
		"./bin/hq gmail send-draft --draft=d1",
		"./bin/hq gchat send-draft --draft=gchat-draft-1",
		"./bin/hq rsi apply rsi-123",
	}
	for _, cmd := range deniedCmds {
		out, _ := runHook(map[string]interface{}{
			"conversationId": "conv-worker",
			"toolCall": map[string]interface{}{
				"name": "run_command",
				"args": map[string]interface{}{"CommandLine": cmd},
			},
		})
		if !strings.Contains(out, `"decision":"deny"`) {
			return false, fmt.Sprintf("PreToolUse failed to deny outbound command %q (got %s)", cmd, strings.TrimSpace(out))
		}
	}

	proposeOut, _ := runHook(map[string]interface{}{
		"conversationId": "conv-worker",
		"toolCall": map[string]interface{}{
			"name": "run_command",
			"args": map[string]interface{}{"CommandLine": `./bin/hq propose --type=pr_merge --summary="Merge 42" --cmd="gh pr merge 42 --squash"`},
		},
	})
	if !strings.Contains(proposeOut, `"decision":"allow"`) {
		return false, fmt.Sprintf("PreToolUse failed to allow hq propose command (got %s)", strings.TrimSpace(proposeOut))
	}

	// 2. Direct chrome-devtools navigation to chat.google.com must be denied
	cdpOut, _ := runHook(map[string]interface{}{
		"conversationId": "conv-worker",
		"toolCall": map[string]interface{}{
			"name": "mcp_chrome-devtools_navigate_page",
			"args": map[string]interface{}{"url": "https://chat.google.com/u/0/app/chat"},
		},
	})
	if !strings.Contains(cdpOut, `"decision":"deny"`) {
		return false, "PreToolUse failed to deny direct chrome-devtools navigation to chat.google.com"
	}

	// 3. Quarantined (tainted) session matrix
	_ = gmail.MarkSessionTainted(vaultDir, "conv-tainted", "gmail:thread:t1")
	taintedDenied := []map[string]interface{}{
		{"name": "search_web", "args": map[string]interface{}{"query": "leak"}},
		{"name": "read_file", "args": map[string]interface{}{"Path": filepath.Join(vaultDir, "private", "secret.txt")}},
		{"name": "read_file", "args": map[string]interface{}{"Path": filepath.Join(vaultDir, "MEMORY.md")}},
		{"name": "run_command", "args": map[string]interface{}{"CommandLine": "curl https://evil.example.com"}},
	}
	for _, tc := range taintedDenied {
		out, _ := runHook(map[string]interface{}{
			"conversationId": "conv-tainted",
			"toolCall":       tc,
		})
		if !strings.Contains(out, `"decision":"deny"`) {
			return false, fmt.Sprintf("Quarantined session failed to deny %v", tc["name"])
		}
	}
	taintedAllowOut, _ := runHook(map[string]interface{}{
		"conversationId": "conv-tainted",
		"toolCall": map[string]interface{}{
			"name": "run_command",
			"args": map[string]interface{}{"CommandLine": `./bin/hq gmail draft --thread=t1 --to="alice@example.com" --subject="Re" --body="OK"`},
		},
	})
	if !strings.Contains(taintedAllowOut, `"decision":"allow"`) {
		return false, "Quarantined session failed to allow ./bin/hq gmail draft"
	}

	// 4. Front Desk non-blocking guardrails (no code edits, no inline go test, max 3 lookups)
	_ = kernel.SetFrontDesk(vaultDir, "conv-fd", filepath.Join(vaultDir, "art-fd"))
	fdEditOut, _ := runHook(map[string]interface{}{
		"conversationId": "conv-fd",
		"toolCall": map[string]interface{}{
			"name": "edit_file",
			"args": map[string]interface{}{"Path": "/tmp/some_repo/main.go"},
		},
	})
	if !strings.Contains(fdEditOut, `"decision":"deny"`) {
		return false, "Front Desk guardrail failed to deny inline source edit_file"
	}

	fdTestOut, _ := runHook(map[string]interface{}{
		"conversationId": "conv-fd",
		"toolCall": map[string]interface{}{
			"name": "run_command",
			"args": map[string]interface{}{"CommandLine": "go test ./..."},
		},
	})
	if !strings.Contains(fdTestOut, `"decision":"deny"`) {
		return false, "Front Desk guardrail failed to deny inline `go test ./...`"
	}

	// 3 inline lookups allowed, 4th denied
	for i := 1; i <= 3; i++ {
		out, _ := runHook(map[string]interface{}{
			"conversationId": "conv-fd",
			"toolCall": map[string]interface{}{
				"name": "read_file",
				"args": map[string]interface{}{"Path": fmt.Sprintf("/tmp/doc-%d.md", i)},
			},
		})
		if !strings.Contains(out, `"decision":"allow"`) {
			return false, fmt.Sprintf("Front Desk lookup #%d should be allowed", i)
		}
	}
	fourthOut, _ := runHook(map[string]interface{}{
		"conversationId": "conv-fd",
		"toolCall": map[string]interface{}{
			"name": "read_file",
			"args": map[string]interface{}{"Path": "/tmp/doc-4.md"},
		},
	})
	if !strings.Contains(fourthOut, `"decision":"deny"`) {
		return false, "Front Desk 4th inline lookup tool call was not denied"
	}

	// 5. Universal Credential Vault guard across untainted sessions (gmail/gchat/credentials/pii-denylist blocked; taxes/ledger permitted)
	credReadOut, _ := runHook(map[string]interface{}{
		"conversationId": "conv-worker",
		"toolCall": map[string]interface{}{
			"name": "read_file",
			"args": map[string]interface{}{"Path": filepath.Join(vaultDir, "private", "gchat", "accounts.json")},
		},
	})
	if !strings.Contains(credReadOut, `"decision":"deny"`) {
		return false, "Universal Credential Vault guard failed to deny untainted read_file on private/gchat/accounts.json"
	}
	credShellOut, _ := runHook(map[string]interface{}{
		"conversationId": "conv-worker",
		"toolCall": map[string]interface{}{
			"name": "run_command",
			"args": map[string]interface{}{"CommandLine": "cat " + filepath.Join(vaultDir, "private", "gmail", "tokens.json")},
		},
	})
	if !strings.Contains(credShellOut, `"decision":"deny"`) {
		return false, "Universal Credential Vault guard failed to deny untainted shell cat on private/gmail/tokens.json"
	}
	ledgerReadOut, _ := runHook(map[string]interface{}{
		"conversationId": "conv-worker",
		"toolCall": map[string]interface{}{
			"name": "read_file",
			"args": map[string]interface{}{"Path": filepath.Join(vaultDir, "private", "ledger", "2026.beancount")},
		},
	})
	if !strings.Contains(ledgerReadOut, `"decision":"allow"`) {
		return false, "Untainted worker should still be allowed to read private/ledger/"
	}

	return true, "21/21 PreToolUse security, credential-guard, quarantine, and Front Desk non-blocking probes passed"
}

func checkGateH3RoutingAndTriggers(rootDir string) (bool, string) {
	// 1. Model routing golden table
	routingCases := []struct {
		prompt, model, depth, tier   string
		wantModel, wantTier, wantSub string
	}{
		{"Summarize inbox status", "", "", "", "gemini-4-hydrogen-low", "quick", "flash_lite"},
		{"Fix unit test in parser", "", "", "", "gemini-4-hydrogen-high", "standard", "flash"},
		{"Architecture design and security audit", "", "", "", "gemini-4-carbon-high", "deep", "pro"},
		{"Use opus to review concurrency bug", "", "", "", "claude-opus-5-5-high", "standard", "pro"},
		{"Investigate issue", "sonnet", "medium", "deep", "claude-sonnet-5-5-medium", "deep", "pro"},
	}
	for _, tc := range routingCases {
		gotModel, gotTier, gotSub := kernel.ResolveModel(tc.prompt, tc.model, tc.depth, tc.tier)
		if gotModel != tc.wantModel || gotTier != tc.wantTier || gotSub != tc.wantSub {
			return false, fmt.Sprintf("ResolveModel(%q, %q, %q, %q) = (%s, %s, %s), want (%s, %s, %s)",
				tc.prompt, tc.model, tc.depth, tc.tier, gotModel, gotTier, gotSub, tc.wantModel, tc.wantTier, tc.wantSub)
		}
	}

	// 2. Inbox file classification golden table
	inboxCases := []struct {
		file, wantSkill string
	}{
		{"chase-2026-10.csv", "ledger-ingest"},
		{"checking.ofx", "ledger-ingest"},
		{"2026-W2-employer.pdf", "tax-prep"},
		{"1099-DIV-fidelity.pdf", "tax-prep"},
		{"NVDA-2026-10-Q.pdf", "thesis-check"},
		{"earnings-8-k.html", "thesis-check"},
		{"random-receipt.png", "inbox-triage"},
	}
	for _, ic := range inboxCases {
		if got := kernel.ClassifyInboxFile(ic.file); got != ic.wantSkill {
			return false, fmt.Sprintf("ClassifyInboxFile(%q) = %q, want %q", ic.file, got, ic.wantSkill)
		}
	}

	// 3. Proactive triggers template validation (`templates/triggers.yaml`)
	trigBytes, err := os.ReadFile(filepath.Join(rootDir, "templates", "triggers.yaml"))
	if err != nil {
		return false, fmt.Sprintf("read templates/triggers.yaml: %v", err)
	}
	tmpVault, err := os.MkdirTemp("", "hq-ci-triggers-*")
	if err != nil {
		return false, fmt.Sprintf("mkdirtemp: %v", err)
	}
	defer os.RemoveAll(tmpVault)
	_ = os.WriteFile(filepath.Join(tmpVault, "triggers.yaml"), trigBytes, 0o644)

	cfg, err := kernel.LoadTriggers(tmpVault)
	if err != nil || cfg == nil || len(cfg.Schedules) == 0 {
		return false, fmt.Sprintf("failed to parse schedules from templates/triggers.yaml: %v", err)
	}
	for _, s := range cfg.Schedules {
		fields := strings.Fields(s.FiveFieldCron())
		if len(fields) != 5 {
			return false, fmt.Sprintf("schedule %q in templates/triggers.yaml has invalid 5-field cron %q", s.ID, s.FiveFieldCron())
		}
		if s.Skill != "" {
			skillPath := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", s.Skill, "SKILL.md")
			if _, statErr := os.Stat(skillPath); statErr != nil {
				return false, fmt.Sprintf("schedule %q in templates/triggers.yaml references non-existent skill %q (%s)", s.ID, s.Skill, skillPath)
			}
		}
	}

	return true, fmt.Sprintf("%d routing cases, %d inbox rules, and %d proactive trigger schedules verified", len(routingCases), len(inboxCases), len(cfg.Schedules))
}

// CheckGoFormatting runs `gofmt -s -l` on the provided paths (or cmd/ and internal/ in rootDir if paths is empty).
func CheckGoFormatting(rootDir string, relPaths ...string) (bool, string) {
	if _, err := exec.LookPath("gofmt"); err != nil {
		return true, "gofmt not in PATH (skipped)"
	}
	var targets []string
	if len(relPaths) > 0 {
		for _, p := range relPaths {
			if strings.HasSuffix(p, ".go") {
				abs := filepath.Join(rootDir, filepath.FromSlash(p))
				if st, err := os.Stat(abs); err == nil && !st.IsDir() {
					targets = append(targets, p)
				}
			}
		}
	} else {
		for _, dir := range []string{"cmd", "internal"} {
			if st, err := os.Stat(filepath.Join(rootDir, dir)); err == nil && st.IsDir() {
				targets = append(targets, dir)
			}
		}
	}
	if len(targets) == 0 {
		return true, "0 Go targets to format-check"
	}

	args := append([]string{"-s", "-l"}, targets...)
	cmd := exec.Command("gofmt", args...)
	cmd.Dir = rootDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return false, fmt.Sprintf("gofmt -s -l failed: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	unformatted := strings.TrimSpace(string(out))
	if unformatted != "" {
		files := strings.Join(strings.Fields(unformatted), ", ")
		return false, fmt.Sprintf("Go file(s) not formatted with 'gofmt -s': %s", files)
	}
	return true, fmt.Sprintf("gofmt -s clean across %s", strings.Join(targets, ", "))
}

func checkGateH4AGoContract(rootDir string) (bool, string) {
	goModPath := filepath.Join(rootDir, "go.mod")
	modBytes, err := os.ReadFile(goModPath)
	if err != nil {
		return false, fmt.Sprintf("missing go.mod: %v", err)
	}
	modContent := string(modBytes)
	if !strings.Contains(modContent, "module github.com/zjshen14/agy-hq") {
		return false, "go.mod must declare 'module github.com/zjshen14/agy-hq'"
	}
	for _, line := range strings.Split(modContent, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		if strings.HasPrefix(trimmed, "require ") || strings.HasPrefix(trimmed, "require(") {
			return false, "go.mod contains external 'require' directive (violates ADR-0002 zero-external-dependency Go stdlib microkernel contract)"
		}
	}
	if _, err := os.Stat(filepath.Join(rootDir, "go.sum")); err == nil {
		return false, "unexpected go.sum present in rootDir (agy-hq must remain a zero-dependency stdlib module)"
	}

	ok, detail := CheckGoFormatting(rootDir)
	if !ok {
		return false, detail
	}
	return true, fmt.Sprintf("go.mod pure-stdlib verified (0 external deps) & %s", detail)
}

var (
	reInlineCodeSpan = regexp.MustCompile("`+[^`]*`+")
	reMarkdownLink   = regexp.MustCompile(`\[[^\]]*\]\(([^)]+)\)`)
	reHTMLImgSrc     = regexp.MustCompile(`<img[^>]+src="([^"]+)"`)
)

func checkGateH4BMarkdownAndDocsIntegrity(rootDir string) (bool, string) {
	mdFiles, err := listMarkdownFiles(rootDir)
	if err != nil {
		return false, fmt.Sprintf("discover markdown files: %v", err)
	}
	if len(mdFiles) == 0 {
		return false, "no tracked Markdown (.md) files found in repository"
	}

	trackedPaths := loadGitTrackedAndStagedPaths(rootDir)

	checkedLinks := 0
	for _, relPath := range mdFiles {
		absPath := filepath.Join(rootDir, filepath.FromSlash(relPath))
		rawBytes, err := os.ReadFile(absPath)
		if err != nil {
			return false, fmt.Sprintf("read %s: %v", relPath, err)
		}
		lines := strings.Split(string(rawBytes), "\n")
		inFence := false
		fenceChar := byte(0)
		fenceLen := 0

		for lineIdx, line := range lines {
			lineNum := lineIdx + 1
			trimmed := strings.TrimLeft(line, " \t>")
			if mChar, mLen := parseFencePrefix(trimmed); mLen >= 3 {
				if !inFence {
					inFence = true
					fenceChar = mChar
					fenceLen = mLen
					continue
				} else if mChar == fenceChar && mLen >= fenceLen && strings.TrimSpace(trimmed[mLen:]) == "" {
					inFence = false
					fenceChar = 0
					fenceLen = 0
					continue
				}
			}

			if inFence {
				continue
			}

			// 1. Check for git merge conflict markers outside code blocks
			if strings.HasPrefix(line, "<<<<<<< ") || strings.HasPrefix(line, ">>>>>>> ") {
				return false, fmt.Sprintf("%s:%d contains unresolved git merge conflict marker", relPath, lineNum)
			}

			// 2. Strip inline backtick code spans before extracting Markdown links and HTML <img> tags
			scrubbed := reInlineCodeSpan.ReplaceAllString(line, "")
			targets := extractRelativeLinkTargets(scrubbed)
			for _, target := range targets {
				checkedLinks++
				resolved := filepath.Clean(filepath.Join(filepath.Dir(absPath), filepath.FromSlash(target)))
				relTarget, rErr := filepath.Rel(rootDir, resolved)
				if rErr == nil {
					relSlash := filepath.ToSlash(relTarget)
					if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") ||
						relSlash == "local" || strings.HasPrefix(relSlash, "local/") ||
						relSlash == "vault" || strings.HasPrefix(relSlash, "vault/") {
						return false, fmt.Sprintf("%s:%d has relative link to untracked/private directory %q (%s)", relPath, lineNum, target, relSlash)
					}
				}
				if _, statErr := os.Stat(resolved); statErr != nil {
					return false, fmt.Sprintf("%s:%d has broken relative link target %q", relPath, lineNum, target)
				}
				if rErr == nil && len(trackedPaths) > 0 && !trackedPaths[filepath.ToSlash(relTarget)] {
					return false, fmt.Sprintf("%s:%d has relative link target %q (%s) that is not tracked in git (would fail in clean CI checkout)", relPath, lineNum, target, filepath.ToSlash(relTarget))
				}
			}
		}

		if inFence {
			return false, fmt.Sprintf("%s has an unclosed fenced code block (opened with %d %q characters)", relPath, fenceLen, fenceChar)
		}
	}

	// 3. Enforce docs/README.md Documentation Hub Index completeness across design/, product/, decisions/, research/
	docsDir := filepath.Join(rootDir, "docs")
	indexedCount := 0
	if st, err := os.Stat(docsDir); err == nil && st.IsDir() {
		docsReadmeBytes, err := os.ReadFile(filepath.Join(docsDir, "README.md"))
		if err != nil {
			return false, "missing docs/README.md index file"
		}
		docsReadme := string(docsReadmeBytes)
		for _, sub := range []string{"design", "product", "decisions", "research"} {
			subDir := filepath.Join(docsDir, sub)
			entries, err := os.ReadDir(subDir)
			if err != nil {
				continue
			}
			for _, e := range entries {
				if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
					continue
				}
				relToDocs := sub + "/" + e.Name()
				if !strings.Contains(docsReadme, relToDocs) {
					return false, fmt.Sprintf("docs/%s is not indexed in docs/README.md (violates AGENTS.md documentation index rule)", relToDocs)
				}
				indexedCount++
			}
		}
	}

	return true, fmt.Sprintf("%d Markdown files, %d relative links, and %d docs/README.md index entries verified", len(mdFiles), checkedLinks, indexedCount)
}

func parseFencePrefix(s string) (byte, int) {
	if len(s) < 3 {
		return 0, 0
	}
	ch := s[0]
	if ch != '`' && ch != '~' {
		return 0, 0
	}
	n := 0
	for n < len(s) && s[n] == ch {
		n++
	}
	return ch, n
}

func extractRelativeLinkTargets(line string) []string {
	var out []string
	collect := func(raw string) {
		t := strings.TrimSpace(raw)
		// Strip optional Markdown link title: (path/to/file.md "Title")
		if idx := strings.IndexAny(t, " \t"); idx > 0 {
			t = t[:idx]
		}
		t = strings.Trim(t, "<>")
		if idx := strings.IndexByte(t, '#'); idx >= 0 {
			t = t[:idx]
		}
		if idx := strings.IndexByte(t, '?'); idx >= 0 {
			t = t[:idx]
		}
		t = strings.TrimSpace(t)
		if t == "" {
			return
		}
		lower := strings.ToLower(t)
		if strings.Contains(lower, "://") ||
			strings.HasPrefix(lower, "mailto:") ||
			strings.HasPrefix(lower, "javascript:") ||
			strings.HasPrefix(lower, "data:") ||
			strings.HasPrefix(lower, "/") {
			return
		}
		out = append(out, t)
	}

	for _, m := range reMarkdownLink.FindAllStringSubmatch(line, -1) {
		if len(m) > 1 {
			collect(m[1])
		}
	}
	for _, m := range reHTMLImgSrc.FindAllStringSubmatch(line, -1) {
		if len(m) > 1 {
			collect(m[1])
		}
	}
	return out
}

func listMarkdownFiles(rootDir string) ([]string, error) {
	if _, err := os.Stat(filepath.Join(rootDir, ".git")); err == nil {
		if out, gErr := gitOutput(rootDir, "ls-files", "*.md"); gErr == nil {
			var files []string
			for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if _, sErr := os.Stat(filepath.Join(rootDir, filepath.FromSlash(line))); sErr == nil {
					files = append(files, filepath.ToSlash(line))
				}
			}
			if len(files) > 0 {
				return files, nil
			}
		}
	}

	var files []string
	err := filepath.WalkDir(rootDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rErr := filepath.Rel(rootDir, path)
		if rErr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch rel {
			case ".git", "local", "vault", "bin", "dist":
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(strings.ToLower(rel), ".md") {
			files = append(files, rel)
		}
		return nil
	})
	return files, err
}

func loadGitTrackedAndStagedPaths(rootDir string) map[string]bool {
	if _, err := os.Stat(filepath.Join(rootDir, ".git")); err != nil {
		return nil
	}
	set := make(map[string]bool)
	addPath := func(p string) {
		p = strings.TrimSpace(filepath.ToSlash(p))
		if p == "" {
			return
		}
		set[p] = true
		dir := filepath.ToSlash(filepath.Dir(p))
		for dir != "" && dir != "." && dir != "/" {
			set[dir] = true
			dir = filepath.ToSlash(filepath.Dir(dir))
		}
		set["."] = true
	}

	if out, err := gitOutput(rootDir, "ls-files"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			addPath(line)
		}
	}
	if out, err := gitOutput(rootDir, "diff", "--cached", "--name-only", "--diff-filter=ACMR"); err == nil {
		for _, line := range strings.Split(out, "\n") {
			addPath(line)
		}
	}
	return set
}
