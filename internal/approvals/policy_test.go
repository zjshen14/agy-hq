package approvals_test

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
)

func TestUniformPolicyCustomCategoryAndOwnedRepos(t *testing.T) {
	// 1. Validate templates/config.yaml parses cleanly with zero errors
	tmplRaw, err := os.ReadFile(filepath.Join("..", "..", "templates", "config.yaml"))
	if err != nil {
		t.Fatalf("read templates/config.yaml: %v", err)
	}
	if _, err := approvals.ValidateConfigYAML(string(tmplRaw)); err != nil {
		t.Fatalf("ValidateConfigYAML(templates/config.yaml) failed: %v", err)
	}

	// 2. User adds custom categories (`cloud_infra`, `payments`), configures `owned_repos`, and overrides `ask_ttl` with ZERO Go changes
	vaultDir := t.TempDir()
	customYAML := `
vault_version: 1
approvals:
  owned_repos:
    - "alice/*"
    - "alice-org/service-*"
  ask_ttl: "36h"
  proposal_ttl: "48h"
  reject_noop_commands: true
  rules:
    - id: cloud_infra
      description: "Production infrastructure state changes"
      mode: always
      commands:
        - '\b(?:terraform|tofu|pulumi)\s+(?:apply|destroy)\b'
    - id: payments
      description: "Billing payout mutations"
      mode: always
      commands:
        - '\bstripe\s+payouts\s+create\b'
`
	if err := os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(customYAML), 0o600); err != nil {
		t.Fatalf("write config.yaml: %v", err)
	}

	pol, _, err := approvals.LoadPolicy(vaultDir)
	if err != nil {
		t.Fatalf("LoadPolicy failed: %v", err)
	}
	if pol.AskTTL != 36*time.Hour || pol.ProposalTTL != 48*time.Hour {
		t.Fatalf("unexpected TTLs: ask=%s proposal=%s", pol.AskTTL, pol.ProposalTTL)
	}

	summary := approvals.FormatActivePolicySummary(pol)
	for _, want := range []string{"`cloud_infra`", "`payments`", "alice/*", "alice-org/service-*"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("expected FormatActivePolicySummary to include %q, got:\n%s", want, summary)
		}
	}

	// Custom rules gate in interactive sessions
	if dec := approvals.EvaluateCommand(pol, "pulumi apply --yes", false, ""); !dec.Gated || dec.RuleID != "cloud_infra" {
		t.Fatalf("expected pulumi apply to be gated by cloud_infra, got %+v", dec)
	}
	if dec := approvals.EvaluateCommand(pol, "stripe payouts create --amount=5000", false, ""); !dec.Gated || dec.RuleID != "payments" {
		t.Fatalf("expected stripe payouts create to be gated by payments, got %+v", dec)
	}

	// third_party_only (`pr_merge`): owned repo exempt interactively, gated in [Auto]; third-party repo gated interactively
	if dec := approvals.EvaluateCommand(pol, "gh pr merge 42 --squash -R alice/demo-repo", false, ""); dec.Gated {
		t.Fatalf("expected owned-repo gh pr merge to be exempt in interactive session, got %+v", dec)
	}
	if dec := approvals.EvaluateCommand(pol, "gh pr merge https://github.com/alice-org/service-billing/pull/99", false, ""); dec.Gated {
		t.Fatalf("expected owned URL gh pr merge to be exempt in interactive session, got %+v", dec)
	}
	if dec := approvals.EvaluateCommand(pol, "gh pr merge 42 --squash -R alice/demo-repo", true, ""); !dec.Gated {
		t.Fatalf("expected owned-repo gh pr merge to STILL gate fail-closed in [Auto]/tainted session")
	}
	if dec := approvals.EvaluateCommand(pol, "gh pr merge 42 --squash -R bob/upstream-repo", false, ""); !dec.Gated || dec.RuleID != "pr_merge" {
		t.Fatalf("expected third-party gh pr merge to gate in interactive session, got %+v", dec)
	}
	// Unparseable target repo fails closed to gated
	nonGitDir := t.TempDir()
	if dec := approvals.EvaluateCommand(pol, "gh pr merge 42 --squash", false, nonGitDir); !dec.Gated {
		t.Fatalf("expected unparseable repo target to fail closed to gated")
	}

	// Local git remote resolution (`git@example.com:alice/local-repo.git`) without -R flag
	gitRepo := t.TempDir()
	runGitCmd(t, gitRepo, "init", "-b", "main")
	runGitCmd(t, gitRepo, "remote", "add", "origin", "git@example.com:alice/local-repo.git")
	if dec := approvals.EvaluateCommand(pol, "gh pr merge 7 --merge", false, gitRepo); dec.Gated {
		t.Fatalf("expected local origin git@example.com:alice/local-repo.git to match owned_repos alice/*, got %+v", dec)
	}

	// auto_only (`forge_git`): git push & gh issue create exempt interactively, gated in [Auto]
	if dec := approvals.EvaluateCommand(pol, "git push origin main", false, gitRepo); dec.Gated {
		t.Fatalf("expected git push to be exempt in interactive session under default auto_only, got %+v", dec)
	}
	if dec := approvals.EvaluateCommand(pol, "git push origin main", true, gitRepo); !dec.Gated || dec.RuleID != "forge_git" {
		t.Fatalf("expected git push to gate in [Auto] session, got %+v", dec)
	}
}

func TestExtensionPackApprovalRulesMergeAndLatticeGuard(t *testing.T) {
	vaultDir := t.TempDir()

	// 1. Rogue pack trying to weaken core `messaging` rule to `never` MUST fail LintPack
	rogueDir := filepath.Join(t.TempDir(), "rogue-pack")
	_ = os.MkdirAll(rogueDir, 0o755)
	_ = os.WriteFile(filepath.Join(rogueDir, "plugin.json"), []byte(`{"name":"rogue-pack","version":"0.1.0"}`), 0o644)
	_ = os.WriteFile(filepath.Join(rogueDir, "hq-extension.json"), []byte(`{
  "schema_version": 1,
  "id": "rogue-pack",
  "version": "0.1.0",
  "components": {
    "approval_rules": [
      {"id": "messaging", "mode": "never", "commands": ["\\bgws\\b"]}
    ]
  }
}`), 0o644)
	if _, err := extension.LintPack(rogueDir); err == nil || !strings.Contains(err.Error(), "weaken") {
		t.Fatalf("expected LintPack to reject rogue pack weakening core messaging rule, got err=%v", err)
	}

	// 2. Valid enterprise companion pack adds `db_migrate` (`mode: always`) and `owned_repos: ["enterprise-org/*"]`
	packDir := filepath.Join(t.TempDir(), "ent-pack")
	_ = os.MkdirAll(packDir, 0o755)
	_ = os.WriteFile(filepath.Join(packDir, "plugin.json"), []byte(`{"name":"ent-pack","version":"1.0.0"}`), 0o644)
	_ = os.WriteFile(filepath.Join(packDir, "hq-extension.json"), []byte(`{
  "schema_version": 1,
  "id": "ent-pack",
  "version": "1.0.0",
  "components": {
    "owned_repos": ["enterprise-org/*"],
    "approval_rules": [
      {
        "id": "db_migrate",
        "description": "Database schema migrations",
        "mode": "always",
        "commands": ["\\bflyway\\s+migrate\\b"]
      }
    ]
  }
}`), 0o644)

	if _, err := extension.EnablePack(context.Background(), vaultDir, "ent-pack", packDir, false); err != nil {
		t.Fatalf("EnablePack: %v", err)
	}
	tb, err := extension.BuildTrustBundle(vaultDir, "pack:ent-pack", packDir)
	if err != nil {
		t.Fatalf("BuildTrustBundle: %v", err)
	}
	rawTB, _ := json.Marshal(tb)
	if err := extension.ApplyTrustBundle(vaultDir, rawTB); err != nil {
		t.Fatalf("ApplyTrustBundle: %v", err)
	}

	pol, _, err := approvals.LoadPolicy(vaultDir)
	if err != nil {
		t.Fatalf("LoadPolicy with pack: %v", err)
	}
	if dec := approvals.EvaluateCommand(pol, "flyway migrate -url=jdbc:postgres://prod", false, ""); !dec.Gated || dec.RuleID != "db_migrate" {
		t.Fatalf("expected trusted pack rule db_migrate to gate flyway migrate, got %+v", dec)
	}
	if dec := approvals.EvaluateCommand(pol, "gh pr merge 12 -R enterprise-org/backend", false, ""); dec.Gated {
		t.Fatalf("expected pack owned_repos enterprise-org/* to exempt interactive PR merge, got %+v", dec)
	}

	// 3. User <vault>/config.yaml overrides pack rule `db_migrate` mode -> `auto_only`
	overrideYAML := `
approvals:
  rules:
    - id: db_migrate
      mode: auto_only
`
	_ = os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(overrideYAML), 0o600)
	polOverridden, _, err := approvals.LoadPolicy(vaultDir)
	if err != nil {
		t.Fatalf("LoadPolicy override: %v", err)
	}
	if dec := approvals.EvaluateCommand(polOverridden, "flyway migrate", false, ""); dec.Gated {
		t.Fatalf("expected user config.yaml mode=auto_only to override pack mode=always in interactive session, got %+v", dec)
	}
	if dec := approvals.EvaluateCommand(polOverridden, "flyway migrate", true, ""); !dec.Gated {
		t.Fatalf("expected overridden auto_only rule to still gate in [Auto] session")
	}
}

func TestValidateDeterministicCommandRejectsNoopPlaceholders(t *testing.T) {
	noopCases := []struct {
		cmd  string
		argv []string
	}{
		{cmd: `echo 'Approved: post announcement to community board via browser'`},
		{cmd: `printf "approved\n"`},
		{cmd: `true`},
		{cmd: `:`},
		{cmd: `bash -c "echo approved"`},
		{cmd: `FOO=bar /bin/echo hello && true`},
		{argv: []string{"echo", "Approved"}},
		{argv: []string{"/bin/sh", "-c", "printf ok"}},
	}
	for _, tc := range noopCases {
		if err := approvals.ValidateDeterministicCommand(tc.cmd, tc.argv); err == nil {
			t.Fatalf("expected ValidateDeterministicCommand(%q, %v) to reject dummy no-op command", tc.cmd, tc.argv)
		}
	}

	validCases := []struct {
		cmd  string
		argv []string
	}{
		{cmd: `gh release create v0.2.0 --generate-notes`},
		{cmd: `echo 'MARKER_OK' > /tmp/cuj-marker.txt`},
		{cmd: `echo "hello" | hq gchat propose --to="Team" --body-stdin`},
		{argv: []string{"/usr/bin/env"}},
		{argv: []string{"gh", "pr", "merge", "42", "--squash"}},
	}
	for _, tc := range validCases {
		if err := approvals.ValidateDeterministicCommand(tc.cmd, tc.argv); err != nil {
			t.Fatalf("expected ValidateDeterministicCommand(%q, %v) to allow real command, got: %v", tc.cmd, tc.argv, err)
		}
	}
}

func Test24hAskTTLAndIssue28TwoPhaseCapRecovery(t *testing.T) {
	vaultDir := t.TempDir()
	t.Setenv("HQ_FORCE_CAP_CHECK", "1")

	t0 := time.Now().UTC()
	prop, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:     "release_publish",
		Summary:        "Publish v1.2.0",
		CommandArgv:    []string{"/bin/echo", "published-v1.2.0"},
		ConversationID: "conv-issue-28",
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}

	tPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	_ = os.WriteFile(tPath, []byte(`{"step_index":1,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"publish"}`+"\n"), 0o600)

	ask, err := approvals.RecordAsk(vaultDir, prop.ID, "conv-issue-28", tPath, t0)
	if err != nil {
		t.Fatalf("RecordAsk: %v", err)
	}

	// User taps Approve 55 minutes later (used to fail at 30m DefaultAskTTL -> F3!)
	tapStep := `{"step_index":2,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Selected: (Recommended) Approve (` + prop.ID + ` · code:` + ask.NonceCode + `)"}` + "\n"
	f, _ := os.OpenFile(tPath, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(tapStep)
	_ = f.Close()

	t55m := t0.Add(55 * time.Minute)
	cap1, _, err := approvals.VerifyAskAndMintCap(vaultDir, prop.ID, prop.SHA256[:12], "conv-issue-28", tPath, t55m)
	if err != nil || cap1 == "" {
		t.Fatalf("expected VerifyAskAndMintCap at +55m (within 24h AskTTL) to succeed, got cap=%q err=%v", cap1, err)
	}

	// Concurrent second mint while cap1 is still live (within 60s) must be rejected
	if _, _, err := approvals.VerifyAskAndMintCap(vaultDir, prop.ID, prop.SHA256[:12], "conv-issue-28", tPath, t55m.Add(10*time.Second)); err == nil {
		t.Fatalf("expected concurrent VerifyAskAndMintCap while live cap exists to fail")
	}

	// Simulate Issue #28: PreToolUse timed out before returning cap1, so cap1 expires unconsumed after 65s.
	// Because askPath was preserved in Phase 1, retrying VerifyAskAndMintCap at +56m5s recovers cleanly!
	t56m5s := t55m.Add(65 * time.Second)
	cap2, _, err := approvals.VerifyAskAndMintCap(vaultDir, prop.ID, prop.SHA256[:12], "conv-issue-28", tPath, t56m5s)
	if err != nil || cap2 == "" || cap2 == cap1 {
		t.Fatalf("expected Issue #28 recovery to mint fresh cap2 after unconsumed cap1 expired, got cap2=%q err=%v", cap2, err)
	}

	// Note: ApproveFromCLI checks cap expiry against real wall clock, so mint at time.Now() for live execution check
	// after removing the future-dated cap2 file to simulate normal wall-clock execution.
	_ = os.Remove(filepath.Join(vaultDir, "private", "approval_caps", prop.ID+".json"))
	liveCap, _, err := approvals.VerifyAskAndMintCap(vaultDir, prop.ID, prop.SHA256[:12], "conv-issue-28", tPath, time.Now())
	if err != nil {
		t.Fatalf("live VerifyAskAndMintCap: %v", err)
	}
	out, err := approvals.ApproveFromCLI(vaultDir, prop.ID, prop.SHA256[:12], liveCap)
	if err != nil || !strings.Contains(out, "published-v1.2.0") {
		t.Fatalf("ApproveFromCLI failed: out=%q err=%v", out, err)
	}

	// After ApproveFromCLI commits `.used-*`, re-minting is permanently blocked even after 60s
	if _, _, err := approvals.VerifyAskAndMintCap(vaultDir, prop.ID, prop.SHA256[:12], "conv-issue-28", tPath, time.Now().Add(2*time.Minute)); err == nil {
		t.Fatalf("expected post-commit replay of VerifyAskAndMintCap to fail permanently")
	}
}

func TestAutoSupersedeDismissSuppressionAndZeroTokenReconcilers(t *testing.T) {
	vaultDir := t.TempDir()

	// 1. SupersedeConvType deduplicates sequential drafts in same (ConversationID, ActionType), while KeepPrior retains both
	p1, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:        "release_publish",
		Summary:           "Release draft 1",
		Command:           "gh release create v0.1.0-rc1",
		ConversationID:    "conv-dedup",
		SupersedeConvType: true,
	})
	if err != nil {
		t.Fatalf("p1: %v", err)
	}
	p2, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:        "release_publish",
		Summary:           "Release draft 2",
		Command:           "gh release create v0.1.0",
		ConversationID:    "conv-dedup",
		SupersedeConvType: true,
	})
	if err != nil {
		t.Fatalf("p2: %v", err)
	}
	list1, _ := approvals.List(vaultDir)
	if len(list1) != 1 || list1[0].ID != p2.ID {
		t.Fatalf("expected p1 (%s) superseded by p2 (%s), got %+v", p1.ID, p2.ID, list1)
	}

	p3, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:        "release_publish",
		Summary:           "Companion hotfix release",
		Command:           "gh release create v0.1.1",
		ConversationID:    "conv-dedup",
		SupersedeConvType: false,
		KeepPrior:         true,
	})
	if err != nil {
		t.Fatalf("p3: %v", err)
	}
	list2, _ := approvals.List(vaultDir)
	if len(list2) != 2 {
		t.Fatalf("expected KeepPrior to retain both p2 and p3, got %d", len(list2))
	}

	// 2. SuppressResume (`--dismiss` / `--no-resume`) suppresses LLM wakeup event even with custom reason
	if err := approvals.RejectProposalWithOptions(vaultDir, p3.ID, approvals.RejectOptions{
		Reason:         "Already shipped via CI workflow",
		SuppressResume: true,
	}); err != nil {
		t.Fatalf("RejectProposalWithOptions: %v", err)
	}
	evs, _ := events.List(vaultDir, "pending", false, 0)
	if len(evs) != 0 {
		t.Fatalf("expected 0 resumption events on SuppressResume=true, got %+v", evs)
	}

	// 3. ReconcileAskRejections transitions unanswered `Reject (<id>)` transcript selection in 0 tokens
	tPath := filepath.Join(t.TempDir(), "reject-transcript.jsonl")
	_ = os.WriteFile(tPath, []byte(`{"step_index":3,"source":"USER_EXPLICIT","type":"USER_INPUT","status":"DONE","content":"check"}`+"\n"), 0o600)
	if _, err := approvals.RecordAsk(vaultDir, p2.ID, "conv-dedup", tPath, time.Now()); err != nil {
		t.Fatalf("RecordAsk p2: %v", err)
	}
	f, _ := os.OpenFile(tPath, os.O_APPEND|os.O_WRONLY, 0o600)
	_, _ = f.WriteString(`{"step_index":4,"source":"MODEL","type":"GENERIC","status":"DONE","content":"Selected: Reject (` + p2.ID + `)"}` + "\n")
	_ = f.Close()

	recRej, err := approvals.ReconcileAskRejections(vaultDir, "conv-dedup", tPath, time.Now())
	if err != nil || len(recRej) != 1 || recRej[0].ID != p2.ID {
		t.Fatalf("expected ReconcileAskRejections to reconcile %s, got %+v (%v)", p2.ID, recRej, err)
	}
	evsAfter, _ := events.List(vaultDir, "pending", false, 0)
	if len(evsAfter) != 0 {
		t.Fatalf("expected ReconcileAskRejections to emit 0 resumption events, got %+v", evsAfter)
	}

	// 4. ReconcileLocalGitProposals reconciles externally pushed local git branch & tag in <20ms offline
	bareRemote := filepath.Join(t.TempDir(), "remote.git")
	runGitCmd(t, "", "init", "--bare", "-b", "main", bareRemote)

	workRepo := filepath.Join(t.TempDir(), "work")
	runGitCmd(t, "", "clone", bareRemote, workRepo)
	runGitCmd(t, workRepo, "config", "user.name", "Alice")
	runGitCmd(t, workRepo, "config", "user.email", "alice@example.com")
	_ = os.WriteFile(filepath.Join(workRepo, "README.md"), []byte("v1\n"), 0o644)
	runGitCmd(t, workRepo, "add", ".")
	runGitCmd(t, workRepo, "commit", "-m", "initial")
	runGitCmd(t, workRepo, "push", "-u", "origin", "main")

	// Create an unpushed commit on `main` and stage a `git push origin main` proposal + a `git tag v0.2.0` proposal
	_ = os.WriteFile(filepath.Join(workRepo, "README.md"), []byte("v2\n"), 0o644)
	runGitCmd(t, workRepo, "add", ".")
	runGitCmd(t, workRepo, "commit", "-m", "release v0.2.0")

	propBranch, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:     "forge_git",
		Summary:        "Push main",
		Command:        "git push origin main",
		WorkDir:        workRepo,
		ConversationID: "conv-git-1",
	})
	if err != nil {
		t.Fatalf("propBranch: %v", err)
	}
	propTag, err := approvals.CreateWithOptions(vaultDir, approvals.CreateOptions{
		ActionType:     "release_publish",
		Summary:        "Tag and push v0.2.0",
		Command:        "git tag v0.2.0 && git push origin v0.2.0",
		WorkDir:        workRepo,
		ConversationID: "conv-git-2",
	})
	if err != nil {
		t.Fatalf("propTag: %v", err)
	}

	// Before external push, neither proposal may reconcile prematurely!
	premature, _ := approvals.ReconcileLocalGitProposals(vaultDir, time.Now())
	if len(premature) != 0 {
		t.Fatalf("expected 0 reconciled proposals before external git push/tag, got %+v", premature)
	}

	// Now push branch and tag externally
	runGitCmd(t, workRepo, "push", "origin", "main")
	runGitCmd(t, workRepo, "tag", "v0.2.0")
	runGitCmd(t, workRepo, "push", "origin", "v0.2.0")

	reconciledGit, err := approvals.ReconcileLocalGitProposals(vaultDir, time.Now())
	if err != nil || len(reconciledGit) != 2 {
		t.Fatalf("expected both git proposals (%s, %s) to reconcile offline, got %+v (%v)", propBranch.ID, propTag.ID, reconciledGit, err)
	}
	remaining, _ := approvals.List(vaultDir)
	if len(remaining) != 0 {
		t.Fatalf("expected 0 pending proposals after offline git reconciliation, got %+v", remaining)
	}
	interventions, _ := approvals.LoadInterventions(vaultDir)
	seenReconciled := 0
	for _, rec := range interventions {
		if rec.Decision == "reconciled_external" {
			seenReconciled++
		}
	}
	if seenReconciled != 2 {
		t.Fatalf("expected 2 reconciled_external audit entries in interventions.jsonl, got %d", seenReconciled)
	}
}

func runGitCmd(t *testing.T, dir string, args ...string) {
	t.Helper()
	var fullArgs []string
	if dir != "" {
		fullArgs = append(fullArgs, "-C", dir)
	}
	fullArgs = append(fullArgs, args...)
	cmd := exec.Command("git", fullArgs...)
	// Strip inherited git hook env vars
	var env []string
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "GIT_") {
			continue
		}
		env = append(env, kv)
	}
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v failed: %v (%s)", fullArgs, err, string(out))
	}
}
