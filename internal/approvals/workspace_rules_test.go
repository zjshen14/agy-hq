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
