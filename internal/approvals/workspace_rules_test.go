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
		// Verifier P0-1 bypass repros: ANSI-C $'...', locale $"...", \\\n continuation, trailing slash, argv[0] spoof
		`hq workspace $'own' acme-monorepo/client:ledger-fix`:                                        "workspace_own",
		`hq workspace $'remove' acme-monorepo/client:ledger-fix --force`:                             "workspace_remove",
		`hq workspace remove acme-monorepo/client:ledger-fix $'-\x2dforce'`:                          "workspace_remove",
		`hq $'workspace' own acme-monorepo/client:ledger-fix`:                                        "workspace_own",
		`hq $"workspace" own acme-monorepo/client:ledger-fix`:                                        "workspace_own",
		"hq work\\\nspace own acme-monorepo/client:ledger-fix":                                       "workspace_own",
		"go run ./cmd/hq/ workspace own acme-monorepo/client:ledger-fix":                             "workspace_own",
		"/tmp/hq.test workspace own acme-monorepo/client:ledger-fix":                                 "workspace_own",
		`bash -c 'exec -a hq.test "$(command -v hq)" workspace own acme-monorepo/client:ledger-fix'`: "workspace_own",
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
	// Unconditional enforcement: even WITHOUT HQ_FORCE_CAP_CHECK, in a temp Vault, inside go test,
	// VerifyAndConsumeWorkspaceExecApproval must refuse missing tokens (zero argv[0]/TMPDIR bypass).
	t.Setenv("HQ_FORCE_CAP_CHECK", "")
	t.Setenv("HQ_APPROVAL_EXEC_ID", "")
	t.Setenv("HQ_APPROVAL_EXEC_TOKEN", "")

	key := "acme-monorepo/client:victim"
	if err := VerifyAndConsumeWorkspaceExecApproval(vaultDir, "workspace_remove", key); err == nil {
		t.Fatal("expected direct workspace_remove without exec token to fail unconditionally")
	}
	if err := VerifyAndConsumeWorkspaceExecApproval(vaultDir, "workspace_own", key); err == nil {
		t.Fatal("expected direct workspace_own without exec token to fail unconditionally")
	}

	// Benign proposal mentioning "workspace own <key>" inside a quoted argument must NOT mint or match a cap.
	benign := Proposal{
		ID:         "prop-1700000099-abcdef123456",
		ActionType: "ops",
		Command:    `git commit -m "document hq workspace own ` + key + `"`,
	}
	if shouldMintWorkspaceExecCap(benign) {
		t.Fatal("benign non-workspace proposal must never mint a workspace exec cap")
	}
	if workspaceExecCommandMatches(WorkspaceExecCapRecord{
		ActionType: "workspace_own",
		Command:    `git commit -m "hq workspace own ` + key + `"`,
	}, "workspace_own", key) {
		t.Fatal("quoted string argument must never satisfy workspaceExecCommandMatches")
	}
	if workspaceExecCommandMatches(WorkspaceExecCapRecord{
		ActionType: "workspace_remove",
		Command:    "hq workspace own " + key,
	}, "workspace_own", key) {
		t.Fatal("mismatched ActionType must never satisfy workspaceExecCommandMatches")
	}

	// Mint a valid exec cap for workspace_own on key.
	prop := Proposal{
		ID:         "prop-1700000000-abcdef123456",
		ActionType: "workspace_own",
		Command:    "hq workspace own " + key,
	}
	execID, rawTok, capPath, err := mintWorkspaceExecCap(vaultDir, prop, 60e9)
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

	// P1-5: workspace_auto_guard gates [Auto] workspace mutations in PreToolUse regardless of env vars.
	pol := DefaultPolicyConfig()
	for _, cmd := range []string{
		"env -u ANTIGRAVITY_CONVERSATION_ID hq workspace create acme/client foo",
		"env -u ANTIGRAVITY_CONVERSATION_ID hq workspace prune",
		"hq spawn --workspace=acme/client --task=foo --prompt=bar",
		"hq subagent --workspace=acme/client --role=ops --prompt=bar",
		"hq archive some-task",
	} {
		if d := EvaluateCommand(pol, cmd, true, ""); !d.Gated || d.RuleID != "workspace_auto_guard" {
			t.Fatalf("P1-5: expected [Auto] EvaluateCommand(%q) gated by workspace_auto_guard, got %+v", cmd, d)
		}
		if d := EvaluateCommand(pol, cmd, false, ""); d.Gated {
			t.Fatalf("P1-5: interactive EvaluateCommand(%q) must not be gated by workspace_auto_guard, got %+v", cmd, d)
		}
	}
}
