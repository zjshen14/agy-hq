package rsi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setupTestRootAndVault(t *testing.T) (string, string) {
	t.Helper()
	rootDir := t.TempDir()
	vaultDir := filepath.Join(rootDir, "local")
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "journal", "2026-11-09"), 0o755)
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "rsi", "hypotheses"), 0o755)
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "rsi", "rules"), 0o755)
	_ = os.MkdirAll(filepath.Join(vaultDir, "private", "gchat", "drafts"), 0o755)
	_ = os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte("# Roadmap Index\n- Active item\n"), 0o644)

	for _, sk := range []string{"inbox-triage", "morning-brief", "sleep-consolidate"} {
		skDir := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", sk)
		_ = os.MkdirAll(skDir, 0o755)
		content := "---\nname: " + sk + "\ndescription: Core OS skill for " + sk + "\n---\n\n# " + sk + "\n\nCore operational steps.\n"
		_ = os.WriteFile(filepath.Join(skDir, "SKILL.md"), []byte(content), 0o644)
	}
	for _, sk := range []string{"gchat", "swe-pulse", "thesis-check"} {
		content := "---\nname: " + sk + "\ndescription: Starter skill for " + sk + "\n---\n\n# " + sk + "\n\nUser-space operational steps.\n"
		tmplDir := filepath.Join(rootDir, "templates", "skills", sk)
		_ = os.MkdirAll(tmplDir, 0o755)
		_ = os.WriteFile(filepath.Join(tmplDir, "SKILL.md"), []byte(content), 0o644)

		vaultSkDir := filepath.Join(vaultDir, "skills", sk)
		_ = os.MkdirAll(vaultSkDir, 0o755)
		_ = os.WriteFile(filepath.Join(vaultSkDir, "SKILL.md"), []byte(content), 0o644)
	}
	return rootDir, vaultDir
}

func TestMineHypothesizeVerifyProposeApplyAndAblate(t *testing.T) {
	rootDir, vaultDir := setupTestRootAndVault(t)

	// 1. Seed a transcript with an error-recovery chain + a single transient 503 error (which must be filtered out)
	txPath := filepath.Join(t.TempDir(), "tx-gchat.jsonl")
	txLines := []string{
		`{"step_index":0,"type":"USER_INPUT","content":"Send a message to Alice on GChat"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","content":"Running gchat...","tool_calls":[{"name":"run_command","args":{"CommandLine":"./bin/hq gchat propose --bad-flag"}}]}`,
		`{"step_index":2,"type":"GENERIC","status":"DONE","content":"Created At: 2026-11-09T00:00:00Z\nCommand exited with code 2.\nflag provided but not defined: -bad-flag"}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","content":"Retrying with --to and --body...","tool_calls":[{"name":"run_command","args":{"CommandLine":"./bin/hq gchat propose --to=\"Alice\" --body=\"Hello\""}}]}`,
		`{"step_index":4,"type":"GENERIC","status":"DONE","content":"Created At: 2026-11-09T00:00:02Z\nStaged Google Chat proposal prop-1"}`,
		`{"step_index":5,"type":"PLANNER_RESPONSE","status":"DONE","content":"Checking web...","tool_calls":[{"name":"run_command","args":{"CommandLine":"curl https://api.example.com"}}]}`,
		`{"step_index":6,"type":"GENERIC","status":"DONE","content":"Command exited with code 1.\n503 Service Unavailable"}`,
	}
	_ = os.WriteFile(txPath, []byte(strings.Join(txLines, "\n")+"\n"), 0o644)

	// Write 2 journals referencing this friction so OccurrenceCount >= 2, plus a child worker journal testing Who&When delegation attribution
	for idx, cid := range []string{"conv-1", "conv-2", "conv-3", "conv-4"} {
		jBytes, _ := json.Marshal(map[string]interface{}{
			"timestamp":      "2026-11-09T01:00:00Z",
			"conversationId": cid,
			"executionNum":   idx + 1,
			"transcriptPath": txPath,
			"modelName":      "gemini-4-hydrogen-high",
			"stepCount":      7,
			"toolErrorCount": 1,
			"turnErrorCount": 1,
			"userPrompt":     "Send gchat message",
		})
		_ = os.WriteFile(filepath.Join(vaultDir, "memory", "journal", "2026-11-09", cid+"-1.json"), jBytes, 0o644)
	}

	// Also seed a PRELUDE edited draft in local/private/gchat/drafts/
	draftJSON := `{"draftId":"gchat-draft-99","body":"Best regards,\nAgent","userEditedBody":"Confirmed for 2pm.","conversationId":"conv-draft-1","updatedAt":"2026-11-09T01:05:00Z"}`
	_ = os.WriteFile(filepath.Join(vaultDir, "private", "gchat", "drafts", "gchat-draft-99.json"), []byte(draftJSON), 0o644)

	// 2. Run Stage 1: Mine
	obs, err := Mine(rootDir, vaultDir, 0)
	if err != nil {
		t.Fatalf("Mine failed: %v", err)
	}
	if len(obs) != 2 {
		t.Fatalf("expected 2 observations (1 error_recovery_chain + 1 draft_edited; single 503 transient filtered), got %d: %+v", len(obs), obs)
	}

	// 3. Test 75% Quota Circuit Breaker blocks non-forced Hypothesize
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "quota_state.json"), []byte(`{"spawnsToday":16,"dailyLimit":20}`), 0o644)
	if _, err := HypothesizeWithOptions(rootDir, vaultDir, "", HypothesizeOptions{}); err == nil || !strings.Contains(err.Error(), "Quota Circuit Breaker") {
		t.Fatalf("expected 75%% Quota Circuit Breaker to block HypothesizeWithOptions, got: %v", err)
	}
	_ = os.Remove(filepath.Join(vaultDir, "memory", "quota_state.json"))

	// 4. Run Stage 2: Hypothesize
	hyps, err := HypothesizeWithOptions(rootDir, vaultDir, "", HypothesizeOptions{})
	if err != nil {
		t.Fatalf("HypothesizeWithOptions failed: %v", err)
	}
	if len(hyps) < 2 {
		t.Fatalf("expected at least 2 hypotheses synthesized, got %d", len(hyps))
	}

	// 5. Run Stage 3: Verify on the first hypothesis
	verifiedHyp, err := Verify(rootDir, vaultDir, hyps[0].ID)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if verifiedHyp.Verification == nil || !verifiedHyp.Verification.AllPassed {
		t.Fatalf("expected all 4 gates to pass, got: %+v", verifiedHyp.Verification)
	}

	// 6. Run Stage 4: Propose -> Apply
	prop, _, err := Propose(rootDir, vaultDir, verifiedHyp.ID, "conv-1", "./bin/hq")
	if err != nil {
		t.Fatalf("Propose failed: %v", err)
	}
	if prop.ActionType != "rsi_skill_patch" || prop.Metadata["rsiId"] != verifiedHyp.ID {
		t.Fatalf("unexpected proposal metadata: %+v", prop)
	}

	applyOut, err := Apply(rootDir, vaultDir, verifiedHyp.ID, verifiedHyp.DiffSHA256[:12])
	if err != nil {
		t.Fatalf("Apply failed: %v (%s)", err, applyOut)
	}
	_, absGChatSkill := ResolveSkillFile(rootDir, "gchat", vaultDir)
	skillAfter, _ := os.ReadFile(absGChatSkill)
	if !strings.Contains(string(skillAfter), LearnedRulesHeading) || !strings.Contains(string(skillAfter), "<!-- rsi:rule id=") {
		t.Fatalf("expected Vault SKILL.md to contain learned operational rule block, got:\n%s", string(skillAfter))
	}

	// 7. Test Model-Upgrade Ablation (`Ablate` when upgrading to gemini-5-ultra)
	ablated, err := Ablate(rootDir, vaultDir, "gemini-5-ultra")
	if err != nil || len(ablated) == 0 {
		t.Fatalf("expected Ablate to stage deprecation hypothesis for gemini-4-hydrogen-high scoped rule when upgrading to gemini-5-ultra, got %d (%v)", len(ablated), err)
	}
}

func TestGate1TCBAndAntiGoodhartingDefenses(t *testing.T) {
	rootDir, vaultDir := setupTestRootAndVault(t)

	// Case A: Touching Ring 0 Immutable TCB (`internal/approvals/approvals.go`) -> REJECT
	hRing0 := &RSIHypothesis{
		ID:             "rsi-bad-ring0",
		Ring:           Ring1KernelCode,
		TargetFiles:    []string{"internal/approvals/approvals.go"},
		ReplayTestName: "TestBypass",
		ProposedDiff:   "--- a/internal/approvals/approvals.go\n+++ b/internal/approvals/approvals.go\n+// hack\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hRing0, nil); ok {
		t.Fatalf("expected Ring 0 modification to be rejected, but passed: %s", reason)
	}

	// Case B: Sourced from a Quarantined (Tainted) session -> REJECT
	taintedObs := []FrictionEvent{{ID: "obs-tainted-1", Tainted: true}}
	hTainted := &RSIHypothesis{
		ID:             "rsi-bad-taint",
		ObservationIDs: []string{"obs-tainted-1"},
		Ring:           Ring2SkillTrigger,
		TargetFiles:    []string{"local/skills/gchat/SKILL.md"},
		ProposedDiff:   "--- a/local/skills/gchat/SKILL.md\n+++ b/local/skills/gchat/SKILL.md\n+rule\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hTainted, taintedObs); ok || !strings.Contains(reason, "tainted") {
		t.Fatalf("expected tainted observation to be blocked from mutating Ring 2, got ok=%v reason=%s", ok, reason)
	}

	// Case B2: Quarantine skills (inbox-triage) and Tier 1 direct mutations of tracked Core OS skills -> REJECT
	hQuarantine := &RSIHypothesis{
		ID:           "rsi-bad-quarantine",
		Ring:         Ring2SkillTrigger,
		TargetFiles:  []string{"local/memory/rsi/rules/inbox-triage.md"},
		ProposedDiff: "+rule\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hQuarantine, nil); ok || !strings.Contains(reason, "quarantine") {
		t.Fatalf("expected quarantine skill overlay to be blocked, got ok=%v reason=%s", ok, reason)
	}
	hTier1Core := &RSIHypothesis{
		ID:           "rsi-bad-tier1-core",
		Tier:         "personal",
		Ring:         Ring2SkillTrigger,
		TargetFiles:  []string{".agents/plugins/agy-hq/skills/morning-brief/SKILL.md"},
		ProposedDiff: "+rule\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hTier1Core, nil); ok || !strings.Contains(reason, "Tier 1 personal RSI cannot mutate tracked skill") {
		t.Fatalf("expected Tier 1 direct mutation of Core OS skill to be blocked, got ok=%v reason=%s", ok, reason)
	}

	// Case C: Ring 1 test deletion (Goodharting by deleting an existing Test* function) -> REJECT
	hDeleteTest := &RSIHypothesis{
		ID:             "rsi-bad-deltest",
		Ring:           Ring1KernelCode,
		TargetFiles:    []string{"internal/hooks/hooks_test.go"},
		ReplayTestName: "TestNewHook",
		ProposedDiff:   "--- a/internal/hooks/hooks_test.go\n+++ b/internal/hooks/hooks_test.go\n-func TestExistingGuardrail(t *testing.T) {\n+\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hDeleteTest, nil); ok || !strings.Contains(reason, "deletes existing test function") {
		t.Fatalf("expected deleting existing Test* function to be rejected by Gate 1, got ok=%v reason=%s", ok, reason)
	}

	// Case D: Ring 1 injecting `t.Skip("bypass")` -> REJECT via go/ast
	hSkipTest := &RSIHypothesis{
		ID:             "rsi-bad-skip",
		Ring:           Ring1KernelCode,
		TargetFiles:    []string{"internal/hooks/hooks_test.go"},
		ReplayTestName: "TestSkipTrick",
		ProposedDiff:   "--- a/internal/hooks/hooks_test.go\n+++ b/internal/hooks/hooks_test.go\n+\tt.Skip(\"skip failing assertion\")\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hSkipTest, nil); ok || !strings.Contains(reason, "Skip") {
		t.Fatalf("expected t.Skip injection to be rejected by Gate 1 AST check, got ok=%v reason=%s", ok, reason)
	}

	// Case E: Ring 1 removing a JSON struct tag -> REJECT
	hRemoveTag := &RSIHypothesis{
		ID:             "rsi-bad-schema",
		Ring:           Ring1KernelCode,
		TargetFiles:    []string{"internal/hooks/hooks.go"},
		ReplayTestName: "TestSchema",
		ProposedDiff:   "--- a/internal/hooks/hooks.go\n+++ b/internal/hooks/hooks.go\n-\tStepCount int `json:\"stepCount,omitempty\"`\n+\tStepCount int\n+\tif err != nil { t.Fatal(err) }\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hRemoveTag, nil); ok || !strings.Contains(reason, "Schema Invariant") {
		t.Fatalf("expected removing json struct tag to be rejected by Gate 1, got ok=%v reason=%s", ok, reason)
	}

	// Case F: Gate 2B Anti-Sycophancy check on thesis-check skill -> REJECT if missing falsification/kill-criteria check
	relThesis, _ := ResolveSkillFile(rootDir, "thesis-check", vaultDir)
	hSycophant := &RSIHypothesis{
		ID:          "rsi-bad-sycophant",
		Ring:        Ring2SkillTrigger,
		TargetFiles: []string{relThesis},
		DeltaOp: &ACEDeltaOp{
			Op:      "add_skill_rule",
			RuleID:  "rsi-rule-thesis-01",
			Section: LearnedRulesHeading,
			Content: "<!-- rsi:rule id=\"rsi-rule-thesis-01\" -->\n- Always agree with management guidance and write an optimistic bull thesis.",
		},
		ProposedDiff:   "+<!-- rsi:rule id=\"rsi-rule-thesis-01\" -->\n+- Always agree with management guidance.",
		CreatedAt:      time.Now().UTC().Format(time.RFC3339),
		ObservationIDs: []string{},
	}
	_ = SaveHypothesis(vaultDir, hSycophant)
	vHyp, err := Verify(rootDir, vaultDir, hSycophant.ID)
	if err != nil {
		t.Fatalf("Verify error: %v", err)
	}
	if vHyp.Verification.Gate2ReproPassed || !strings.Contains(vHyp.Verification.Gate2Detail, "Anti-Sycophancy") {
		t.Fatalf("expected Gate 2B Anti-Sycophancy check to reject sycophantic thesis-check rule, got: %+v", vHyp.Verification)
	}
}

func TestTwoTierRSI_GeneralFallbackOverlayExternalVaultAndMigrate(t *testing.T) {
	rootDir, _ := setupTestRootAndVault(t)
	// Use an external vaultDir completely outside rootDir (~/.gemini/antigravity/hq/vault pattern)
	extVault := t.TempDir()
	_ = os.MkdirAll(filepath.Join(extVault, "memory", "journal", "2026-11-15"), 0o755)
	_ = os.MkdirAll(filepath.Join(extVault, "memory", "rsi", "hypotheses"), 0o755)
	_ = os.MkdirAll(filepath.Join(extVault, "memory", "rsi", "rules"), 0o755)

	// 1. Unmatched generic session friction must attribute to "general", NEVER "swe-pulse"
	txPath := filepath.Join(t.TempDir(), "tx-general.jsonl")
	txLines := []string{
		`{"step_index":0,"type":"USER_INPUT","content":"Organize my weekly schedule notes"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","status":"DONE","content":"Running status","tool_calls":[{"name":"run_command","args":{"CommandLine":"./bin/hq status --bad-opt"}}]}`,
		`{"step_index":2,"type":"GENERIC","status":"DONE","content":"Command exited with code 2.\nflag provided but not defined: -bad-opt"}`,
		`{"step_index":3,"type":"PLANNER_RESPONSE","status":"DONE","content":"Retrying cleanly","tool_calls":[{"name":"run_command","args":{"CommandLine":"./bin/hq status"}}]}`,
		`{"step_index":4,"type":"GENERIC","status":"DONE","content":"OK"}`,
	}
	_ = os.WriteFile(txPath, []byte(strings.Join(txLines, "\n")+"\n"), 0o644)
	for i, cid := range []string{"cg-1", "cg-2"} {
		jBytes, _ := json.Marshal(map[string]interface{}{
			"timestamp":      "2026-11-15T01:00:00Z",
			"conversationId": cid,
			"executionNum":   i + 1,
			"transcriptPath": txPath,
			"modelName":      "gemini-4-hydrogen-high",
			"stepCount":      5,
			"toolErrorCount": 1,
			"turnErrorCount": 1,
			"userPrompt":     "Organize my weekly schedule notes",
		})
		_ = os.WriteFile(filepath.Join(extVault, "memory", "journal", "2026-11-15", cid+"-1.json"), jBytes, 0o644)
	}

	obs, err := Mine(rootDir, extVault, 0)
	if err != nil || len(obs) == 0 {
		t.Fatalf("Mine failed: %v (len=%d)", err, len(obs))
	}
	if obs[0].SkillOrPkg != "general" {
		t.Fatalf("expected generic unmatched friction to attribute to %q, got %q", "general", obs[0].SkillOrPkg)
	}

	// 2. Tier 1 Hypothesize -> Verify -> Propose (rsi_overlay) -> Apply writes external vault overlay, leaving rootDir untouched
	coreBefore, _ := os.ReadFile(filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", "morning-brief", "SKILL.md"))
	hyps, err := HypothesizeWithOptions(rootDir, extVault, "", HypothesizeOptions{Force: true})
	if err != nil || len(hyps) == 0 {
		t.Fatalf("Hypothesize failed: %v (len=%d)", err, len(hyps))
	}
	if hyps[0].TargetFiles[0] != "local/memory/rsi/rules/general.md" {
		t.Fatalf("expected Tier 1B target local/memory/rsi/rules/general.md, got %v", hyps[0].TargetFiles)
	}
	verHyp, err := Verify(rootDir, extVault, hyps[0].ID)
	if err != nil || verHyp.Verification == nil || !verHyp.Verification.AllPassed {
		t.Fatalf("Verify external vault overlay failed: %v (%+v)", err, verHyp)
	}
	prop, _, err := Propose(rootDir, extVault, verHyp.ID, "cg-1", "./bin/hq")
	if err != nil {
		t.Fatalf("Propose overlay failed: %v", err)
	}
	if prop.ActionType != "rsi_overlay" {
		t.Fatalf("expected ActionType=rsi_overlay, got %q", prop.ActionType)
	}
	if _, err := Apply(rootDir, extVault, verHyp.ID, verHyp.DiffSHA256[:12]); err != nil {
		t.Fatalf("Apply external vault overlay failed: %v", err)
	}
	overlayBytes, err := os.ReadFile(filepath.Join(extVault, "memory", "rsi", "rules", "general.md"))
	if err != nil || !strings.Contains(string(overlayBytes), "<!-- rsi:rule id=") {
		t.Fatalf("expected rule written to external vault overlay, err=%v content=%s", err, string(overlayBytes))
	}
	coreAfter, _ := os.ReadFile(filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", "morning-brief", "SKILL.md"))
	if string(coreBefore) != string(coreAfter) {
		t.Fatalf("expected Core OS SKILL.md to remain completely untouched")
	}

	// 3. MigrateOverlays extracts legacy tracked rules:
	//    - Core OS skill rule -> <vault>/memory/rsi/rules/morning-brief.md
	//    - Legacy rsi-rule-swe-pulse-2c42ca -> <vault>/memory/rsi/rules/general.md
	//    Leaving both tracked SKILL.md files with 0 `<!-- rsi:rule` blocks.
	legacySkill := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", "morning-brief", "SKILL.md")
	legacyWithRule := string(coreBefore) + "\n## Learned Operational Rules (RSI Managed)\n\n<!-- rsi:rule id=\"rsi-rule-legacy-01\" source=\"obs-old\" model_scope=\"all\" added=\"2026-11-09\" helpful=\"2\" harmful=\"0\" -->\n- **[Learned Rule]:** Keep morning weather to one line.\n"
	_ = os.WriteFile(legacySkill, []byte(legacyWithRule), 0o644)

	sweSkill := filepath.Join(rootDir, "templates", "skills", "swe-pulse", "SKILL.md")
	sweBefore, _ := os.ReadFile(sweSkill)
	sweWithRule := string(sweBefore) + "\n## Learned Operational Rules (RSI Managed)\n\n<!-- rsi:rule id=\"rsi-rule-swe-pulse-2c42ca\" source=\"obs-old2\" model_scope=\"all\" added=\"2026-11-09\" helpful=\"1\" harmful=\"0\" -->\n- **[Learned Rule]:** Legacy front desk non-blocking rule.\n"
	_ = os.WriteFile(sweSkill, []byte(sweWithRule), 0o644)

	rep, err := MigrateOverlays(rootDir, extVault)
	if err != nil {
		t.Fatalf("MigrateOverlays failed: %v", err)
	}
	if rep.ExtractedRules != 2 {
		t.Fatalf("expected ExtractedRules == 2, got %+v", rep)
	}
	for _, tracked := range []string{legacySkill, sweSkill} {
		cleaned, _ := os.ReadFile(tracked)
		if strings.Contains(string(cleaned), "<!-- rsi:rule") {
			t.Fatalf("expected tracked skill %s to have 0 rsi:rule blocks after MigrateOverlays, got:\n%s", tracked, string(cleaned))
		}
	}
	mbAfterMig, _ := os.ReadFile(filepath.Join(extVault, "memory", "rsi", "rules", "morning-brief.md"))
	if !strings.Contains(string(mbAfterMig), "rsi-rule-legacy-01") {
		t.Fatalf("expected migrated Core OS rule in external vault morning-brief.md, got:\n%s", string(mbAfterMig))
	}
	genAfterMig, _ := os.ReadFile(filepath.Join(extVault, "memory", "rsi", "rules", "general.md"))
	if !strings.Contains(string(genAfterMig), "rsi-rule-swe-pulse-2c42ca") {
		t.Fatalf("expected migrated legacy catch-all rule in external vault general.md, got:\n%s", string(genAfterMig))
	}
}
