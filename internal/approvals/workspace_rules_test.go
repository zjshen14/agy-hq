package approvals

import (
	"strings"
	"testing"
)

func TestWorkspaceCoreRulesGateDestructiveVerbs(t *testing.T) {
	pol := DefaultPolicyConfig()
	gated := map[string]string{
		"hq workspace remove acme-monorepo/client:ledger-fix --force":                    "workspace_remove",
		"./bin/hq workspace remove acme-monorepo/client:ledger-fix --force":              "workspace_remove",
		"hq workspace remove --force acme-monorepo/client:ledger-fix":                    "workspace_remove",
		"hq workspace remove acme-monorepo/client:ledger-fix -force":                     "workspace_remove",
		"hq workspace remove acme-monorepo/client:ledger-fix --force=true":               "workspace_remove",
		"cd /tmp && hq workspace remove acme-monorepo/client:ledger-fix \"--force\"":     "workspace_remove",
		"HQ_VAULT_DIR=/v hq workspace remove acme-monorepo/client:ledger-fix --force":    "workspace_remove",
		"hq workspace own acme-monorepo/client:ledger-fix":                               "workspace_own",
		"./bin/hq workspace own acme-monorepo/client:ledger-fix":                         "workspace_own",
		"echo start; hq workspace own acme-monorepo/client:ledger-fix && echo done":      "workspace_own",
		"hq workspace remove acme-monorepo/client:ledger-fix --force # cleanup old work": "workspace_remove",
		// P0-1 regressions: multi-dash, quote-splitting, backslash-escaping, and command substitution
		"hq workspace remove acme-monorepo/client:ledger-fix ---force":            "workspace_remove",
		"hq workspace remove acme-monorepo/client:ledger-fix ----force=1":         "workspace_remove",
		`hq workspace remove acme-monorepo/client:ledger-fix --""force`:           "workspace_remove",
		`hq workspace "remove" acme-monorepo/client:ledger-fix --force`:           "workspace_remove",
		`hq "workspace" remove acme-monorepo/client:ledger-fix --force`:           "workspace_remove",
		`hq workspace 'own' acme-monorepo/client:ledger-fix`:                      "workspace_own",
		`hq "workspace" own acme-monorepo/client:ledger-fix`:                      "workspace_own",
		`hq workspace \o\w\n acme-monorepo/client:ledger-fix`:                     "workspace_own",
		`echo $(hq workspace 'own' acme-monorepo/client:ledger-fix)`:              "workspace_own",
		`echo "$(hq workspace "remove" acme-monorepo/client:ledger-fix --force)"`: "workspace_remove",
	}
	for cmd, rule := range gated {
		for _, auto := range []bool{false, true} {
			d := EvaluateCommand(pol, cmd, auto, "")
			if !d.Gated || d.RuleID != rule || d.Mode != ModeAlways {
				t.Fatalf("EvaluateCommand(%q, auto=%v) = %+v, want gated by %s", cmd, auto, d, rule)
			}
		}
	}
	allowed := []string{
		"hq workspace remove acme-monorepo/client:ledger-fix",
		"hq workspace remove acme-monorepo/client:my-force-ws",
		"hq workspace list --json",
		"hq workspace status acme-monorepo/client:ledger-fix --refresh",
		"hq workspace handoff acme-monorepo/client:ledger-fix --to=other-task",
		"hq workspace unbind acme-monorepo/client:ledger-fix",
		"hq workspace owned-list",
		"hq worktree remove repo-task",
		// `hq propose` itself never matches: PreToolUse strips the --cmd value before evaluating the line.
		`hq propose --type=workspace_remove --summary="Remove hq-owned workspace acme-monorepo/client:ledger-fix (2 open change(s))" --cmd=""`,
	}
	for _, cmd := range allowed {
		if d := EvaluateCommand(pol, cmd, false, ""); d.Gated {
			t.Fatalf("EvaluateCommand(%q) unexpectedly gated: %+v", cmd, d)
		}
	}
}

func TestWorkspaceCoreRulesCannotBeWeakenedByPacks(t *testing.T) {
	for _, id := range []string{"workspace_remove", "workspace_own"} {
		if !CoreNonDisableablePackRules[id] {
			t.Fatalf("%s must be a core non-disableable rule", id)
		}
	}
	packs := []ExtensionPackRules{{
		PackID: "acme-monorepo",
		Rules: []Rule{
			{ID: "workspace_remove", Mode: ModeNever},
			{ID: "workspace_own", Mode: ModeAutoOnly},
		},
	}}
	pol, warnings, err := buildPolicyFromSources(packs, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(warnings) < 2 || !strings.Contains(strings.Join(warnings, "\n"), "attempted to weaken core rule") {
		t.Fatalf("expected weakening warnings, got %v", warnings)
	}
	for _, cmd := range []string{"hq workspace remove a/b:c --force", "hq workspace own a/b:c"} {
		if d := EvaluateCommand(pol, cmd, false, ""); !d.Gated || d.Mode != ModeAlways {
			t.Fatalf("pack weakening must be ignored for %q: %+v", cmd, d)
		}
	}
	// Packs may still tighten core rules by adding patterns.
	tight := []ExtensionPackRules{{PackID: "acme-monorepo", Rules: []Rule{{ID: "workspace_remove", Mode: ModeAlways, Commands: []string{`\bacme-ws\s+purge\b`}}}}}
	pol, _, err = buildPolicyFromSources(tight, "")
	if err != nil {
		t.Fatal(err)
	}
	if d := EvaluateCommand(pol, "acme-ws purge ledger-fix", false, ""); !d.Gated || d.RuleID != "workspace_remove" {
		t.Fatalf("expected the tightened pattern to gate: %+v", d)
	}
	if d := EvaluateCommand(pol, "hq workspace remove a/b:c --force", false, ""); !d.Gated {
		t.Fatalf("built-in pattern must survive a pack extension: %+v", d)
	}
	if !strings.Contains(FormatActivePolicySummary(DefaultPolicyConfig()), "`workspace_remove`") {
		t.Fatal("the SessionStart policy summary must list workspace_remove as always gated")
	}
}

func TestWorkspaceExecApprovalCapEnforcement(t *testing.T) {
	vaultDir := t.TempDir()
	t.Setenv("HQ_FORCE_CAP_CHECK", "1")
	t.Setenv("HQ_APPROVAL_EXEC_ID", "")
	t.Setenv("HQ_APPROVAL_EXEC_TOKEN", "")

	key := "acme-monorepo/client:victim"
	if err := VerifyAndConsumeWorkspaceExecApproval(vaultDir, "workspace_remove", key); err == nil {
		t.Fatal("expected direct workspace_remove without exec token to fail under HQ_FORCE_CAP_CHECK=1")
	}
	if err := VerifyAndConsumeWorkspaceExecApproval(vaultDir, "workspace_own", key); err == nil {
		t.Fatal("expected direct workspace_own without exec token to fail under HQ_FORCE_CAP_CHECK=1")
	}

	// Mint a valid exec cap for workspace_own on key.
	prop := Proposal{
		ID:         "prop-1700000000-abcdef123456",
		ActionType: "workspace_own",
		Command:    "hq workspace own " + key,
	}
	execID, rawTok, capPath, err := mintWorkspaceExecCap(vaultDir, prop, 60)
	if err != nil {
		t.Fatalf("mintWorkspaceExecCap: %v", err)
	}
	_ = capPath

	// Wrong key or wrong action must consume/reject cleanly.
	t.Setenv("HQ_APPROVAL_EXEC_ID", execID)
	t.Setenv("HQ_APPROVAL_EXEC_TOKEN", rawTok)
	if err := VerifyAndConsumeWorkspaceExecApproval(vaultDir, "workspace_own", "acme-monorepo/client:other"); err == nil {
		t.Fatal("expected wrong-key exec token to be rejected")
	}
	// Replay after failed/claimed attempt must also fail (one-shot).
	if err := VerifyAndConsumeWorkspaceExecApproval(vaultDir, "workspace_own", key); err == nil {
		t.Fatal("expected replayed exec token to be rejected")
	}

	// Fresh mint for matching key+action succeeds once and rejects replay.
	execID2, rawTok2, _, err := mintWorkspaceExecCap(vaultDir, prop, 60e9)
	if err != nil {
		t.Fatalf("mintWorkspaceExecCap 2: %v", err)
	}
	t.Setenv("HQ_APPROVAL_EXEC_ID", execID2)
	t.Setenv("HQ_APPROVAL_EXEC_TOKEN", rawTok2)
	if err := VerifyAndConsumeWorkspaceExecApproval(vaultDir, "workspace_own", key); err != nil {
		t.Fatalf("expected valid exec token to succeed: %v", err)
	}
	if err := VerifyAndConsumeWorkspaceExecApproval(vaultDir, "workspace_own", key); err == nil {
		t.Fatal("expected second use of one-shot exec token to be rejected")
	}

	// Verify workspace_remove --force matches both flag orderings.
	rmProp := Proposal{
		ID:         "prop-1700000001-abcdef123456",
		ActionType: "workspace_remove",
		Command:    "hq workspace remove " + key + " --force",
	}
	execID3, rawTok3, _, err := mintWorkspaceExecCap(vaultDir, rmProp, 60e9)
	if err != nil {
		t.Fatalf("mintWorkspaceExecCap rm: %v", err)
	}
	t.Setenv("HQ_APPROVAL_EXEC_ID", execID3)
	t.Setenv("HQ_APPROVAL_EXEC_TOKEN", rawTok3)
	if err := VerifyAndConsumeWorkspaceExecApproval(vaultDir, "workspace_remove", key); err != nil {
		t.Fatalf("expected valid workspace_remove exec token to succeed: %v", err)
	}
}
