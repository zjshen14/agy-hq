package vault

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
)

const demoProposalID = "prop-demo-acme-q3-alert"

// SeedDemoData populates a synthetic, zero-PII demonstration dataset inside vaultDir (`local/`)
// so new developers evaluating `agy-hq` via `./bin/hq init --demo` or `./scripts/bootstrap.sh --demo`
// immediately see realistic Task Board activity, an inbox drop file, an investment thesis with kill
// criteria, a SHA-256 staged outbound proposal, and verified bi-temporal facts.
//
// SeedDemoData is strictly non-destructive: it never overwrites existing user files, populated
// tasks.json entries, or populated facts.jsonl records.
func SeedDemoData(vaultDir string) ([]string, error) {
	var seeded []string
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339)

	// 1. Sample Inbox Drop: Synthetic ACME Corp Form 10-Q HTML snippet
	inboxPath := filepath.Join(vaultDir, "inbox", "acme-corp-2026-10-Q.html")
	if _, err := os.Stat(inboxPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(inboxPath), 0o755); err != nil {
			return seeded, err
		}
		htmlContent := `<!DOCTYPE html>
<html>
<head><title>[Sample] ACME Corp (ACME) — Form 10-Q (Q3 FY2026)</title></head>
<body>
  <h1>UNITED STATES SECURITIES AND EXCHANGE COMMISSION — FORM 10-Q</h1>
  <h2>[Sample] ACME CORPORATION (Ticker: ACME)</h2>
  <section id="financial-highlights">
    <h3>Item 1. Condensed Consolidated Financial Highlights (Unaudited)</h3>
    <ul>
      <li>Q3 Revenue: $412.0M (+28% YoY vs. $321.9M in Q3 FY2025).</li>
      <li>GAAP Gross Margin: 64.2% (up 140 bps YoY; above 62.0% thesis floor).</li>
      <li>Free Cash Flow: $94.5M (22.9% FCF margin).</li>
      <li>Top Customer Concentration: Largest customer accounted for 18% of Q3 revenue.</li>
    </ul>
  </section>
  <section id="risk-factors">
    <h3>Item 1A. Risk Factors — Notable Q3 Update</h3>
    <p><strong>New Single-Source Packaging Substrate Risk:</strong> Lead times for advanced optical packaging substrates extended from 8 weeks to 14 weeks during Q3 FY2026. Qualification of a secondary supplier is targeted for Q1 FY2027.</p>
  </section>
</body>
</html>
`
		if err := os.WriteFile(inboxPath, []byte(htmlContent), 0o644); err != nil {
			return seeded, fmt.Errorf("write sample inbox filing: %w", err)
		}
		seeded = append(seeded, filepath.ToSlash(filepath.Join("inbox", "acme-corp-2026-10-Q.html")))
	}

	// 2. Sample Investment Thesis & Kill Criteria: local/notes/investing/tickers/ACME/thesis.md
	thesisRel := filepath.Join("notes", "investing", "tickers", "ACME", "thesis.md")
	thesisPath := filepath.Join(vaultDir, thesisRel)
	if _, err := os.Stat(thesisPath); os.IsNotExist(err) {
		if err := os.MkdirAll(filepath.Dir(thesisPath), 0o755); err != nil {
			return seeded, err
		}
		thesisContent := `# [Sample] ACME Corp (` + "`ACME`" + `) — Core Investment Thesis & Falsification Criteria

> Synthetic demonstration note seeded by ` + "`./bin/hq init --demo`" + `. Try running ` + "`/thesis-check`" + ` or ` + "`/inbox-triage`" + ` against ` + "`local/inbox/acme-corp-2026-10-Q.html`" + `!

## 1. Core Thesis Pillars
1. **Optical Interconnect Share Gains:** ACME's low-power optical engines are winning next-generation AI cluster deployments with >25% YoY revenue growth.
2. **Operating Leverage & Pricing Power:** Differentiated firmware lock-in sustains GAAP gross margins comfortably above 62%.

## 2. Explicit Falsification / Kill Criteria
- **Kill Criterion 1 (Margin Erosion):** GAAP gross margin falls below **62.0%** for two consecutive quarters.
- **Kill Criterion 2 (Customer Concentration):** Any single hyperscale customer exceeds **25%** of quarterly revenue.
- **Kill Criterion 3 (Supply Chain Bottleneck):** Single-source component lead times exceed **16 weeks** without a qualified second source.
`
		if err := os.WriteFile(thesisPath, []byte(thesisContent), 0o644); err != nil {
			return seeded, fmt.Errorf("write sample thesis: %w", err)
		}
		seeded = append(seeded, filepath.ToSlash(thesisRel))
	}

	// 3. Sample Layer-1 Task Board Entries: local/memory/tasks.json (only if absent or empty)
	tasksPath := filepath.Join(vaultDir, "memory", "tasks.json")
	existingTasks, _ := kernel.ListTasks(vaultDir)
	if len(existingTasks) == 0 {
		if err := os.MkdirAll(filepath.Dir(tasksPath), 0o755); err != nil {
			return seeded, err
		}
		demoTasks := []kernel.ActiveTask{
			{
				TaskSlug:       "sample-acme-q3-thesis-check",
				Title:          "[Task:ACME] [Sample] Q3 10-Q Thesis & Supply-Chain Audit",
				Kind:           "task",
				Scope:          "ACME",
				Tier:           "deep",
				Model:          "gemini-4-carbon-high",
				ConversationID: "demo-conv-task-acme-001",
				TurnCount:      3,
				Pinned:         true,
				CreatedAt:      now.Add(-45 * time.Minute).Format(time.RFC3339),
				LastUpdatedAt:  now.Add(-5 * time.Minute).Format(time.RFC3339),
				LastSummary:    "[Sample] Verified ACME Q3 revenue (+28% YoY) and 64.2% gross margin; flagged 14-week substrate lead-time risk.",
				Status:         "active",
			},
			{
				TaskSlug:       "subagent-agy-hq-inbox-router-demo",
				Title:          "[Subagent:inbox] [Sample] ACME 10-Q Filing Classifier [standard]",
				Kind:           "subagent",
				Scope:          "inbox",
				Tier:           "standard",
				Model:          "gemini-4-hydrogen-high",
				ConversationID: "demo-conv-sub-inbox-002",
				TurnCount:      1,
				CreatedAt:      now.Add(-30 * time.Minute).Format(time.RFC3339),
				LastUpdatedAt:  now.Add(-28 * time.Minute).Format(time.RFC3339),
				CompletedAt:    now.Add(-28 * time.Minute).Format(time.RFC3339),
				LastSummary:    "[Sample] Classified acme-corp-2026-10-Q.html as SEC Form 10-Q and staged digest approval.",
				Status:         "completed",
			},
			{
				TaskSlug:       "auto-morning-brief-demo",
				Title:          "[Auto] [Sample] Morning Brief",
				Kind:           "auto",
				Scope:          "brief",
				Tier:           "standard",
				Model:          "gemini-4-hydrogen-high",
				ConversationID: "demo-conv-auto-brief-003",
				TurnCount:      1,
				CreatedAt:      now.Add(-2 * time.Hour).Format(time.RFC3339),
				LastUpdatedAt:  now.Add(-118 * time.Minute).Format(time.RFC3339),
				CompletedAt:    now.Add(-118 * time.Minute).Format(time.RFC3339),
				LastSummary:    "[Sample] Delivered daily digest: 1 new SEC 10-Q in inbox/, 1 pending approval, all CI gates green.",
				Status:         "completed",
			},
		}
		b, err := json.MarshalIndent(demoTasks, "", "  ")
		if err != nil {
			return seeded, err
		}
		if err := os.WriteFile(tasksPath, b, 0o644); err != nil {
			return seeded, fmt.Errorf("write sample tasks.json: %w", err)
		}
		seeded = append(seeded, filepath.ToSlash(filepath.Join("memory", "tasks.json"))+" (3 [Sample] tasks)")
	}

	// 4. Sample SHA-256 Staged Outbound Proposal: local/memory/pending/prop-demo-acme-q3-alert.json
	pendingDir := filepath.Join(vaultDir, "memory", "pending")
	propPath := filepath.Join(pendingDir, demoProposalID+".json")
	if _, err := os.Stat(propPath); os.IsNotExist(err) {
		if err := os.MkdirAll(pendingDir, 0o755); err != nil {
			return seeded, err
		}
		actionType := "demo_echo"
		cmd := "echo '[Sample] Dispatched ACME Q3 10-Q thesis check summary'"
		prop := approvals.Proposal{
			ID:             demoProposalID,
			CreatedAt:      nowStr,
			ConversationID: "demo-conv-task-acme-001",
			ActionType:     actionType,
			Summary:        `[Sample] Publish ACME Q3 10-Q digest: "Gross margin 64.2% (+140 bps YoY); watching 14-week substrate lead time"`,
			Command:        cmd,
			WorkDir:        vaultDir,
			SHA256:         approvals.ComputeHash(actionType, cmd),
			Status:         "pending",
			Metadata: map[string]string{
				"sample": "true",
				"ticker": "ACME",
			},
		}
		b, err := json.MarshalIndent(prop, "", "  ")
		if err != nil {
			return seeded, err
		}
		if err := os.WriteFile(propPath, b, 0o644); err != nil {
			return seeded, fmt.Errorf("write sample proposal: %w", err)
		}
		seeded = append(seeded, filepath.ToSlash(filepath.Join("memory", "pending", demoProposalID+".json")))
	}

	// 5. Sample Bi-Temporal Atomic Facts: local/memory/facts.jsonl (only if empty)
	factsPath := filepath.Join(vaultDir, "memory", "facts.jsonl")
	existingFactsBytes, _ := os.ReadFile(factsPath)
	if len(strings.TrimSpace(string(existingFactsBytes))) == 0 {
		sampleFacts := []Fact{
			{
				ID:         "fact-demo-vault-isolation",
				Topic:      "architecture",
				Statement:  "[Sample] Layer 2 private Vault (local/) is 100% git-ignored from the Layer 1 root repository and versioned in its own local/.git.",
				SourceConv: "demo-conv-auto-brief-003",
				Verified:   true,
				ValidFrom:  nowStr,
			},
			{
				ID:         "fact-demo-acme-margin-floor",
				Topic:      "investing-acme",
				Statement:  "[Sample] ACME Corp thesis requires GAAP gross margin >= 62.0% and single-customer concentration <= 25%.",
				SourceConv: "demo-conv-task-acme-001",
				Verified:   true,
				ValidFrom:  nowStr,
			},
			{
				ID:         "fact-demo-approval-gate",
				Topic:      "security",
				Statement:  "[Sample] All outbound mutations require ./bin/hq propose and deterministic SHA-256 verification before execution.",
				SourceConv: "demo-conv-sub-inbox-002",
				Verified:   true,
				ValidFrom:  nowStr,
			},
		}
		var lines []string
		for _, f := range sampleFacts {
			b, err := json.Marshal(f)
			if err != nil {
				return seeded, err
			}
			lines = append(lines, string(b))
		}
		if err := os.WriteFile(factsPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			return seeded, fmt.Errorf("write sample facts.jsonl: %w", err)
		}
		seeded = append(seeded, filepath.ToSlash(filepath.Join("memory", "facts.jsonl"))+" (3 verified [Sample] facts)")
	}

	// Commit newly seeded demo artifacts inside the private Vault git repository if present
	if len(seeded) > 0 {
		if _, err := os.Stat(filepath.Join(vaultDir, ".git")); err == nil {
			_ = SanitizedGitCommand(vaultDir, "add", "-A").Run()
			_ = SanitizedGitCommand(vaultDir, "commit", "-m", "chore(vault): seed synthetic --demo sample dataset").Run()
		}
	}

	return seeded, nil
}
