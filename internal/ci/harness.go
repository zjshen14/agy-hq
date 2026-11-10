package ci

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

	return true, "18/18 PreToolUse security, quarantine, and Front Desk non-blocking probes passed"
}

func checkGateH3RoutingAndTriggers(rootDir string) (bool, string) {
	// 1. Model routing golden table
	routingCases := []struct {
		prompt, model, depth, tier   string
		wantModel, wantTier, wantSub string
	}{
		{"Summarize inbox status", "", "", "", "gemini-3.8-flash-low", "quick", "flash_lite"},
		{"Fix unit test in parser", "", "", "", "gemini-3.8-flash-high", "standard", "flash"},
		{"Architecture design and security audit", "", "", "", "gemini-4-argon-high", "deep", "pro"},
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
