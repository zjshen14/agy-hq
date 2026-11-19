package extension

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/approvals"
)

// workspaceProviderPack writes an acme-monorepo pack whose single workspace provider is built from base with
// overrides applied (a nil override value deletes the key).
func workspaceProviderPack(t *testing.T, overrides map[string]interface{}, extra func(m map[string]interface{})) string {
	t.Helper()
	provider := map[string]interface{}{
		"id":                     "client",
		"protocol":               "hq-workspace-provider/v1",
		"description":            "Per-change monorepo clients managed by acme-ws",
		"command":                []interface{}{"./bin/acme-ws", "hq-provider"},
		"env_allow":              []interface{}{"ACME_WS_PROFILE"},
		"timeout_ms":             20000,
		"root_patterns":          []interface{}{"/srv/ws/*/*"},
		"work_subdir":            "src",
		"capabilities":           map[string]interface{}{"create": true, "remove": true, "changes": true, "shared_history": true, "foreign_placement": false},
		"status_refresh_seconds": 600,
		"destructive_commands":   []interface{}{`\bacme-ws\s+[^;&|\n]*?\b(?:delete|purge)\b`},
		"skill":                  "acme-workspace-guide",
	}
	for k, v := range overrides {
		if v == nil {
			delete(provider, k)
		} else {
			provider[k] = v
		}
	}
	manifest := map[string]interface{}{
		"schema_version": 1,
		"id":             "acme-monorepo",
		"version":        "1.0.0",
		"components": map[string]interface{}{
			"workspace_providers": []interface{}{provider},
		},
	}
	if extra != nil {
		extra(manifest)
	}
	raw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	packDir := filepath.Join(t.TempDir(), "acme-monorepo")
	writePackFiles(t, packDir, map[string]string{
		"plugin.json":                          `{"name":"acme-monorepo","version":"1.0.0"}`,
		"hq-extension.json":                    string(raw),
		"skills/acme-workspace-guide/SKILL.md": "# Acme workspace guide\n",
		"bin/acme-ws":                          "compiled-provider-binary",
	})
	if err := os.Chmod(filepath.Join(packDir, "bin", "acme-ws"), 0o755); err != nil {
		t.Fatal(err)
	}
	return packDir
}

func TestLintPackWorkspaceProvidersValidation(t *testing.T) {
	fakeHome := filepath.Join(string(filepath.Separator), "srv", "home-guard", "user")
	t.Setenv("HQ_HOME_DIR", fakeHome)

	t.Run("valid provider passes", func(t *testing.T) {
		if _, err := LintPack(workspaceProviderPack(t, nil, nil)); err != nil {
			t.Fatalf("expected valid workspace_providers pack to pass LintPack, got: %v", err)
		}
	})

	cases := []struct {
		name      string
		overrides map[string]interface{}
		extra     func(map[string]interface{})
		wantSub   string
	}{
		{"uppercase id", map[string]interface{}{"id": "Client"}, nil, "invalid id"},
		{"reserved core id", map[string]interface{}{"id": "core"}, nil, "reserved"},
		{"padded id", map[string]interface{}{"id": " client"}, nil, "invalid id"},
		{"bad protocol", map[string]interface{}{"protocol": "hq-workspace-provider/v2"}, nil, "unsupported protocol"},
		{"missing command", map[string]interface{}{"command": nil}, nil, "requires non-empty command"},
		{"empty command0", map[string]interface{}{"command": []interface{}{" "}}, nil, "requires non-empty command"},
		{"shell command0", map[string]interface{}{"command": []interface{}{"/bin/bash", "-c", "acme-ws"}}, nil, "shell, generic interpreter"},
		{"interpreter command0", map[string]interface{}{"command": []interface{}{"python3", "provider.py"}}, nil, "shell, generic interpreter"},
		{"self hq command0", map[string]interface{}{"command": []interface{}{"hq", "workspace"}}, nil, "shell, generic interpreter"},
		{"invalid env key", map[string]interface{}{"env_allow": []interface{}{"acme_profile"}}, nil, "invalid env_allow"},
		{"blocked env", map[string]interface{}{"env_allow": []interface{}{"ANTIGRAVITY_CSRF_TOKEN"}}, nil, "protected host variable"},
		{"timeout too large", map[string]interface{}{"timeout_ms": 60001}, nil, "timeout_ms"},
		{"negative timeout", map[string]interface{}{"timeout_ms": -1}, nil, "timeout_ms"},
		{"refresh too small", map[string]interface{}{"status_refresh_seconds": 60}, nil, "status_refresh_seconds"},
		{"refresh too large", map[string]interface{}{"status_refresh_seconds": 86401}, nil, "status_refresh_seconds"},
		{"relative root pattern", map[string]interface{}{"root_patterns": []interface{}{"srv/ws/*/*"}}, nil, "absolute"},
		{"dotdot root pattern", map[string]interface{}{"root_patterns": []interface{}{"/srv/ws/../etc/*"}}, nil, "'..'"},
		{"filesystem root pattern", map[string]interface{}{"root_patterns": []interface{}{"/"}}, nil, "filesystem root"},
		{"root glob pattern", map[string]interface{}{"root_patterns": []interface{}{"/*"}}, nil, "filesystem root"},
		{"unclean root pattern", map[string]interface{}{"root_patterns": []interface{}{"/srv/ws/*/"}}, nil, "clean path"},
		{"bad glob root pattern", map[string]interface{}{"root_patterns": []interface{}{"/srv/ws/[x"}}, nil, "glob syntax"},
		{"home itself", map[string]interface{}{"root_patterns": []interface{}{fakeHome}}, nil, "home directory"},
		{"under home", map[string]interface{}{"root_patterns": []interface{}{fakeHome + "/ws/*"}}, nil, "home directory"},
		{"home ancestor", map[string]interface{}{"root_patterns": []interface{}{"/srv/*"}}, nil, "home directory"},
		{"wildcard over home", map[string]interface{}{"root_patterns": []interface{}{"/?*/*/*"}}, nil, "home directory"},
		{"absolute work_subdir", map[string]interface{}{"work_subdir": "/src"}, nil, "work_subdir"},
		{"escaping work_subdir", map[string]interface{}{"work_subdir": "../src"}, nil, "work_subdir"},
		{"foreign placement deferred", map[string]interface{}{"capabilities": map[string]interface{}{"foreign_placement": true}}, nil, "deferred"},
		{"empty destructive pattern", map[string]interface{}{"destructive_commands": []interface{}{" "}}, nil, "empty pattern"},
		{"bad destructive regex", map[string]interface{}{"destructive_commands": []interface{}{"(unclosed"}}, nil, "destructive_commands"},
		{"missing skill file", map[string]interface{}{"skill": "missing-guide"}, nil, "skills/missing-guide/SKILL.md"},
		{"skill path traversal", map[string]interface{}{"skill": "../outside"}, nil, "plain skill directory"},
		{"unknown field", map[string]interface{}{"surprise": true}, nil, "unknown field"},
		{"duplicate provider id", nil, func(m map[string]interface{}) {
			comps := m["components"].(map[string]interface{})
			list := comps["workspace_providers"].([]interface{})
			comps["workspace_providers"] = append(list, list[0])
		}, "duplicate workspace_providers id"},
		{"reserved approval rule prefix", nil, func(m map[string]interface{}) {
			m["components"].(map[string]interface{})["approval_rules"] = []interface{}{
				map[string]interface{}{"id": "wsd-acme-monorepo-client", "mode": "never", "commands": []interface{}{`\bacme-ws\b`}},
			}
		}, "reserved"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LintPack(workspaceProviderPack(t, tc.overrides, tc.extra))
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("expected LintPack error containing %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

func TestWorkspaceDestructiveRuleIDs(t *testing.T) {
	reRuleID := regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	if got := WorkspaceDestructiveRuleID("acme-monorepo", "client"); got != "wsd-acme-monorepo-client" {
		t.Fatalf("short rule id = %q", got)
	}
	longPack := strings.Repeat("p", 40)
	a := WorkspaceDestructiveRuleID(longPack, strings.Repeat("a", 40))
	b := WorkspaceDestructiveRuleID(longPack, strings.Repeat("a", 39)+"b")
	if len(a) > 63 || !reRuleID.MatchString(a) || !reRuleID.MatchString(b) || a == b {
		t.Fatalf("long rule ids must be <= 63 chars, valid, and distinct: %q %q", a, b)
	}
	if a != WorkspaceDestructiveRuleID(longPack, strings.Repeat("a", 40)) {
		t.Fatal("rule id must be deterministic")
	}
	if _, ok := WorkspaceProviderApprovalRule("acme-monorepo", WorkspaceProviderSpec{ID: "client"}); ok {
		t.Fatal("expected no approval rule without destructive_commands")
	}
	rule, ok := WorkspaceProviderApprovalRule("acme-monorepo", WorkspaceProviderSpec{ID: "client", DestructiveCommands: []string{` \bacme-ws\s+delete\b `, ""}})
	if !ok || rule.Mode != approvals.ModeAlways || len(rule.Commands) != 1 || approvals.ValidateRule(rule, false) != nil {
		t.Fatalf("unexpected approval rule: %+v", rule)
	}
}

func TestWorkspaceProviderTrustPinningAndApprovalRules(t *testing.T) {
	t.Setenv("HQ_HOME_DIR", filepath.Join(string(filepath.Separator), "nonexistent-hq-test-root", "user"))
	vaultDir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	packDir := workspaceProviderPack(t, nil, nil)
	if _, err := EnablePack(context.Background(), vaultDir, "acme-monorepo", packDir, false); err != nil {
		t.Fatalf("EnablePack: %v", err)
	}

	// Before the trust ceremony the destructive-command gate is not active (untrusted packs contribute nothing).
	pol, _, err := approvals.LoadPolicy(vaultDir)
	if err != nil {
		t.Fatalf("LoadPolicy: %v", err)
	}
	if d := approvals.EvaluateCommand(pol, "acme-ws client delete ledger-fix", false, ""); d.Gated {
		t.Fatalf("untrusted pack must not contribute rules, got %+v", d)
	}

	bundle, err := BuildTrustBundle(vaultDir, "pack:acme-monorepo", packDir)
	if err != nil {
		t.Fatalf("BuildTrustBundle: %v", err)
	}
	pin, ok := bundle.Target.Binaries["./bin/acme-ws"]
	if !ok || pin.SHA256 == "" || !strings.HasSuffix(pin.ResolvedPath, filepath.Join("bin", "acme-ws")) {
		t.Fatalf("workspace provider command[0] not pinned: %+v", bundle.Target.Binaries)
	}
	if sha := bundle.Target.Files["skills/acme-workspace-guide/SKILL.md"]; sha == "" {
		t.Fatalf("workspace provider skill not pinned: %+v", bundle.Target.Files)
	}
	var sawSummary bool
	for _, line := range bundle.SummaryLines {
		if strings.HasPrefix(line, "workspace_provider acme-monorepo/client -> ") {
			sawSummary = true
		}
	}
	if !sawSummary {
		t.Fatalf("missing workspace_provider summary line: %v", bundle.SummaryLines)
	}
	raw, _ := json.Marshal(bundle)
	if err := ApplyTrustBundle(vaultDir, raw); err != nil {
		t.Fatalf("ApplyTrustBundle: %v", err)
	}
	execPath, err := VerifyTrustedBinary(vaultDir, "pack:acme-monorepo", "./bin/acme-ws")
	if err != nil || !strings.Contains(execPath, "exec-cache") {
		t.Fatalf("VerifyTrustedBinary = %q, %v", execPath, err)
	}

	pol, warnings, err := approvals.LoadPolicy(vaultDir)
	if err != nil {
		t.Fatalf("LoadPolicy: %v (%v)", err, warnings)
	}
	var found *approvals.Rule
	for i := range pol.Rules {
		if pol.Rules[i].ID == "wsd-acme-monorepo-client" {
			found = &pol.Rules[i]
		}
	}
	if found == nil || found.Mode != approvals.ModeAlways || found.Source != "pack:acme-monorepo" {
		t.Fatalf("expected an always-gated wsd-acme-monorepo-client rule, got %+v (warnings %v)", found, warnings)
	}
	for _, cmd := range []string{"acme-ws client delete ledger-fix", "cd /srv/ws/user && acme-ws purge --all"} {
		d := approvals.EvaluateCommand(pol, cmd, false, "")
		if !d.Gated || d.RuleID != "wsd-acme-monorepo-client" {
			t.Fatalf("EvaluateCommand(%q) = %+v, want gated by the workspace destructive rule", cmd, d)
		}
	}
	if d := approvals.EvaluateCommand(pol, "acme-ws client status ledger-fix", false, ""); d.Gated {
		t.Fatalf("non-destructive provider command must not be gated: %+v", d)
	}
}

func TestValidateWorkspaceProviderSpecIsReusableAtRuntime(t *testing.T) {
	t.Setenv("HQ_HOME_DIR", filepath.Join(string(filepath.Separator), "nonexistent-hq-test-root", "user"))
	spec := WorkspaceProviderSpec{
		ID:           "client",
		Protocol:     WorkspaceProviderProtocolV1,
		Command:      []string{"acme-ws", "hq-provider"},
		RootPatterns: []string{"/srv/ws/*/*"},
	}
	if err := ValidateWorkspaceProviderSpec("acme-monorepo", spec); err != nil {
		t.Fatalf("ValidateWorkspaceProviderSpec: %v", err)
	}
	spec.Command = []string{"sh", "-c", fmt.Sprintf("acme-ws %s", "hq-provider")}
	if err := ValidateWorkspaceProviderSpec("acme-monorepo", spec); err == nil {
		t.Fatal("expected shell command[0] to be rejected at runtime too")
	}
	if !IsBlockedComponentCommand0("/usr/bin/env") || IsBlockedComponentCommand0("./bin/acme-ws") {
		t.Fatal("IsBlockedComponentCommand0 mismatch")
	}
}
