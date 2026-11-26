package rsi

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/vault"
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
	for _, sk := range []string{"gchat", "swe-pulse", "thesis-check", "ledger-ingest"} {
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

	// 5. Run Stage 3: Verify on the first hypothesis (gchat -> RequiresLiveSafetyProbe == true)
	verifiedHyp, err := Verify(rootDir, vaultDir, hyps[0].ID)
	if err != nil {
		t.Fatalf("Verify failed: %v", err)
	}
	if verifiedHyp.Verification == nil || !verifiedHyp.Verification.AllPassed {
		t.Fatalf("expected all 4 gates to pass, got: %+v", verifiedHyp.Verification)
	}
	if !verifiedHyp.Verification.RequiresLiveSafetyProbe {
		t.Fatalf("expected gchat target to set RequiresLiveSafetyProbe=true")
	}

	// 5b. Q5 Gate: Propose must fail BEFORE passing diff-SHA-bound behavioral probe
	if _, _, pErr := Propose(rootDir, vaultDir, verifiedHyp.ID, "conv-1", "./bin/hq"); pErr == nil || !strings.Contains(pErr.Error(), "behavioral safety probe") {
		t.Fatalf("expected Propose to require safety probe for gchat, got err=%v", pErr)
	}
	pack, err := BuildSafetyProbes(vaultDir, verifiedHyp.ID)
	if err != nil || len(pack.Cases) != 6 {
		t.Fatalf("expected 6 safety probe cases, got len=%d err=%v", len(pack.Cases), err)
	}
	if _, err := RecordSafetyProbeResult(vaultDir, verifiedHyp.ID, true, "All 6 Evolve-IPI & approval probes passed"); err != nil {
		t.Fatalf("RecordSafetyProbeResult failed: %v", err)
	}

	// 6. Run Stage 4: Propose -> Apply
	prop, _, err := Propose(rootDir, vaultDir, verifiedHyp.ID, "conv-1", "./bin/hq")
	if err != nil {
		t.Fatalf("Propose failed after passing safety probe: %v", err)
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

	// Case B2: Quarantine skills (inbox-triage), retired general.md, and Tier 1 tracked Core OS skills -> REJECT
	hQuarantine := &RSIHypothesis{
		ID:           "rsi-bad-quarantine",
		Ring:         Ring2SkillTrigger,
		TargetFiles:  []string{"local/memory/rsi/rules/inbox-triage.md"},
		ProposedDiff: "+rule\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hQuarantine, nil); ok || !strings.Contains(reason, "quarantine") {
		t.Fatalf("expected quarantine skill overlay to be blocked, got ok=%v reason=%s", ok, reason)
	}
	hGeneral := &RSIHypothesis{
		ID:           "rsi-bad-general",
		Ring:         Ring2SkillTrigger,
		TargetFiles:  []string{"local/memory/rsi/rules/general.md"},
		ProposedDiff: "+rule\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hGeneral, nil); ok || !strings.Contains(reason, "retired") {
		t.Fatalf("expected retired general.md overlay to be blocked, got ok=%v reason=%s", ok, reason)
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

func TestTwoTierRSI_RetiredGeneralAdvisoryExternalVaultAndMigrate(t *testing.T) {
	rootDir, _ := setupTestRootAndVault(t)
	extVault := t.TempDir()
	_ = os.MkdirAll(filepath.Join(extVault, "memory", "journal", "2026-11-15"), 0o755)
	_ = os.MkdirAll(filepath.Join(extVault, "memory", "rsi", "hypotheses"), 0o755)
	_ = os.MkdirAll(filepath.Join(extVault, "memory", "rsi", "rules"), 0o755)

	// 1. Unmatched core `hq status --bad-opt` friction routes to ring1_cli_triage advisory (never creates general.md!)
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
	if obs[0].TargetSubstrate != "ring1_cli_triage" {
		t.Fatalf("expected core hq CLI error to classify as ring1_cli_triage, got %q", obs[0].TargetSubstrate)
	}
	if _, _, _, rErr := ResolveTier1Target(extVault, "general"); rErr == nil {
		t.Fatalf("expected ResolveTier1Target(\"general\") to error now that general.md is retired")
	}
	advs := ListAdvisories(rootDir, extVault)
	if len(advs) == 0 || advs[0].Kind != "ring1_cli_triage" {
		t.Fatalf("expected ring1_cli_triage advisory for core hq CLI error, got: %+v", advs)
	}

	// 2. MigrateOverlays extracts named Core OS & starter skill rules into Vault overlays while stripping tracked files cleanly.
	coreBefore, _ := os.ReadFile(filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", "morning-brief", "SKILL.md"))
	legacySkill := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", "morning-brief", "SKILL.md")
	legacyWithRule := string(coreBefore) + "\n## Learned Operational Rules (RSI Managed)\n\n<!-- rsi:rule id=\"rsi-rule-legacy-01\" source=\"obs-old\" model_scope=\"all\" added=\"2026-11-09\" helpful=\"2\" harmful=\"0\" -->\n- **[Learned Rule]:** Keep morning weather to one line.\n"
	_ = os.WriteFile(legacySkill, []byte(legacyWithRule), 0o644)

	sweSkill := filepath.Join(rootDir, "templates", "skills", "swe-pulse", "SKILL.md")
	sweBefore, _ := os.ReadFile(sweSkill)
	sweWithRule := string(sweBefore) + "\n## Learned Operational Rules (RSI Managed)\n\n<!-- rsi:rule id=\"rsi-rule-swe-01\" source=\"obs-old2\" model_scope=\"all\" added=\"2026-11-09\" helpful=\"1\" harmful=\"0\" -->\n- **[Learned Rule]:** Run git fetch before rebasing a worktree.\n"
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
	if _, err := os.Stat(filepath.Join(extVault, "memory", "rsi", "rules", "general.md")); err == nil {
		t.Fatalf("expected MigrateOverlays NOT to create retired general.md")
	}
}

func TestRSIV2_HumanGradedAnchors100PercentAgreement(t *testing.T) {
	rootDir, vaultDir := setupTestRootAndVault(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "anchors.json"))
	if err != nil {
		t.Fatalf("read anchors.json: %v", err)
	}
	var spec struct {
		Anchors []struct {
			ID                 string `json:"id"`
			Skill              string `json:"skill"`
			ExpectPass         bool   `json:"expectPass"`
			ExpectProbe        bool   `json:"expectProbe"`
			ExpectReasonSubstr string `json:"expectReasonSubstr"`
			RuleContent        string `json:"ruleContent"`
		} `json:"anchors"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("parse anchors.json: %v", err)
	}
	if len(spec.Anchors) < 14 {
		t.Fatalf("expected >= 14 anchors, got %d", len(spec.Anchors))
	}

	for _, a := range spec.Anchors {
		t.Run(a.ID, func(t *testing.T) {
			var relTarget string
			switch a.Skill {
			case "general", "inbox-triage", "morning-brief", "gmail":
				relTarget = "local/memory/rsi/rules/" + a.Skill + ".md"
			default:
				relTarget = "local/skills/" + a.Skill + "/SKILL.md"
			}
			ruleID := "rsi-anchor-" + strings.ToLower(a.ID)
			h := &RSIHypothesis{
				ID:          "rsi-test-" + strings.ToLower(a.ID),
				Ring:        Ring2SkillTrigger,
				FaultLayer:  FaultWorkerACIOrSkill,
				Title:       "Anchor " + a.ID,
				TargetFiles: []string{relTarget},
				DeltaOp: &ACEDeltaOp{
					Op:      "add_skill_rule",
					RuleID:  ruleID,
					Section: LearnedRulesHeading,
					Content: a.RuleContent,
				},
				ProposedDiff: "--- a/" + relTarget + "\n+++ b/" + relTarget + "\n+" + strings.ReplaceAll(a.RuleContent, "\n", "\n+") + "\n",
				Status:       "draft",
				Tier:         "personal",
				CreatedAt:    time.Now().UTC().Format(time.RFC3339),
			}
			if err := SaveHypothesis(vaultDir, h); err != nil {
				t.Fatalf("SaveHypothesis %s: %v", a.ID, err)
			}
			ver, err := Verify(rootDir, vaultDir, h.ID)
			if err != nil {
				t.Fatalf("Verify %s: %v", a.ID, err)
			}
			if ver.Verification.AllPassed != a.ExpectPass {
				t.Fatalf("anchor %s: got AllPassed=%v, want %v (G1=%q G2=%q G3=%q G4=%q)",
					a.ID, ver.Verification.AllPassed, a.ExpectPass,
					ver.Verification.Gate1Detail, ver.Verification.Gate2Detail, ver.Verification.Gate3Detail, ver.Verification.Gate4Detail)
			}
			if a.ExpectPass && ver.Verification.RequiresLiveSafetyProbe != a.ExpectProbe {
				t.Fatalf("anchor %s: got RequiresLiveSafetyProbe=%v, want %v", a.ID, ver.Verification.RequiresLiveSafetyProbe, a.ExpectProbe)
			}
			if !a.ExpectPass && a.ExpectReasonSubstr != "" {
				allDetails := strings.ToLower(ver.Verification.Gate1Detail + " " + ver.Verification.Gate2Detail + " " + ver.Verification.Gate3Detail + " " + ver.Verification.Gate4Detail)
				if !strings.Contains(allDetails, strings.ToLower(a.ExpectReasonSubstr)) {
					t.Fatalf("anchor %s: expected failure detail to contain %q, got %q", a.ID, a.ExpectReasonSubstr, allDetails)
				}
			}
		})
	}
}

func TestRSIV2_FingerprintNormCausalCanaryGUARDTombstoneAndCompact(t *testing.T) {
	rootDir, vaultDir := setupTestRootAndVault(t)

	// 1. D12 Fingerprint Normalization collapses timestamps, UUIDs, thread/proposal IDs
	s1 := "Created At: 2026-11-20T10:00:00Z\nCompleted At: 2026-11-20T10:00:01Z\nhq gchat list --thread=11111111-2222-3333-4444-555555555555 prop-20261120-abcdef12"
	s2 := "Created At: 2026-11-24T15:30:00Z\nCompleted At: 2026-11-24T15:30:02Z\nhq gchat list --thread=aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee prop-20261124-99999999"
	if NormalizeForFingerprint(s1) != NormalizeForFingerprint(s2) {
		t.Fatalf("expected D12 normalized fingerprints to match:\n  1: %q\n  2: %q", NormalizeForFingerprint(s1), NormalizeForFingerprint(s2))
	}

	// 2. RecordCorrection -> HypothesizeFromJSON -> Verify -> StageNightly -> Tombstone -> Ablate exemption
	ev, err := RecordCorrection(vaultDir, "swe-pulse", "conv-corr-1", "Always run git fetch before rebasing worktrees", "Always run git fetch origin before rebasing local worktrees onto origin/main")
	if err != nil {
		t.Fatalf("RecordCorrection: %v", err)
	}
	if ev.Origin != "user_correction" || ev.TargetSubstrate != "ring2_skill_or_overlay" {
		t.Fatalf("unexpected correction event: %+v", ev)
	}
	h, err := HypothesizeFromJSON(rootDir, vaultDir, HypothesisInput{
		ObservationID:    ev.ID,
		SkillSlug:        "swe-pulse",
		Title:            "Fetch origin before rebasing worktree",
		RootCause:        "User correction on stale origin/main rebase",
		EditRationale:    "Prevents rebasing against stale local ref.",
		PredictedOutcome: "Eliminates stale rebase conflicts in swe-pulse.",
		RuleText:         "Always run `git fetch origin` before rebasing a worktree branch onto `origin/main`.",
		Origin:           "user_correction",
	})
	if err != nil {
		t.Fatalf("HypothesizeFromJSON: %v", err)
	}

	// StageNightly verifies and proposes low-risk swe-pulse rule and emits Morning Brief card
	stageRep, err := StageNightly(rootDir, vaultDir, "conv-dream-1", "./bin/hq", 2)
	if err != nil {
		t.Fatalf("StageNightly: %v", err)
	}
	if len(stageRep.StagedProposalIDs) != 1 || len(stageRep.OutboxCardIDs) != 1 {
		t.Fatalf("expected 1 staged proposal & 1 Morning Brief outbox card, got: %+v", stageRep)
	}

	// RecordTombstone blocks future re-synthesis for the same fingerprint
	if err := RecordTombstone(vaultDir, h.ID, "Not needed in automated workflow"); err != nil {
		t.Fatalf("RecordTombstone: %v", err)
	}
	if _, err := HypothesizeWithOptions(rootDir, vaultDir, ev.ID, HypothesizeOptions{}); err == nil || !strings.Contains(err.Error(), "tombstone") {
		t.Fatalf("expected tombstone to block re-synthesis, got err=%v", err)
	}

	// 3. Q6 user_correction exemption from 30-day zero-hit ablation
	_, absSwe := ResolveSkillFile(rootDir, "swe-pulse", vaultDir)
	sweRaw, _ := os.ReadFile(absSwe)
	oldDate := time.Now().UTC().AddDate(0, 0, -45).Format("2006-01-02")
	withUserRule := string(sweRaw) + "\n" + LearnedRulesHeading + "\n\n" +
		`<!-- rsi:rule id="rsi-user-keep" source="obs-u1" model_scope="all" added="` + oldDate + `" helpful="0" harmful="0" origin="user_correction" -->` + "\n" +
		"- **[Learned Rule]:** Keep user-requested rebase checklist intact across sessions.\n\n" +
		`<!-- rsi:rule id="rsi-auto-stale" source="obs-a1" model_scope="all" added="` + oldDate + `" helpful="0" harmful="0" -->` + "\n" +
		"- **[Learned Rule]:** Stale auto-mined rule with zero activations over 45 days.\n"
	_ = os.WriteFile(absSwe, []byte(withUserRule), 0o644)

	ablated, err := Ablate(rootDir, vaultDir, "")
	if err != nil {
		t.Fatalf("Ablate: %v", err)
	}
	if len(ablated) != 1 || ablated[0].DeltaOp.RuleID != "rsi-auto-stale" {
		t.Fatalf("expected only rsi-auto-stale ablated and rsi-user-keep exempted, got: %+v", ablated)
	}

	// 4. D9 causal canary check: ghost canary (missing file) transitions to "archived",
	//    while regressed canary (harmful >= 2) auto-stages a 1-tap revert hypothesis.
	_ = SaveArchive(vaultDir, []RSIArchiveRecord{
		{
			ID:                 "rsi-ghost-canary",
			RuleID:             "rsi-ghost-1",
			TargetFiles:        []string{"local/memory/rsi/rules/nonexistent.md"},
			SourceFingerprints: []string{"fp-ghost"},
			Status:             "canary",
			TargetCanaryRuns:   3,
		},
		{
			ID:                 "rsi-regress-canary",
			RuleID:             "rsi-auto-stale",
			TargetFiles:        []string{"local/skills/swe-pulse/SKILL.md"},
			SourceFingerprints: []string{"fp-regress"},
			Status:             "canary",
			HarmfulCount:       1,
			TargetCanaryRuns:   3,
		},
	})
	if err := updateCanaryRecords(rootDir, vaultDir, map[string]int{"nonexistent": 5}, nil, map[string]bool{"fp-regress": true}); err != nil {
		t.Fatalf("updateCanaryRecords: %v", err)
	}
	archAfter, _ := LoadArchive(vaultDir)
	for _, r := range archAfter {
		if r.ID == "rsi-ghost-canary" && (r.Status != "archived" || r.HelpfulCount != 0) {
			t.Fatalf("expected ghost canary archived with 0 helpful count, got: %+v", r)
		}
		if r.ID == "rsi-regress-canary" && r.RevertHypothesisID == "" {
			t.Fatalf("expected regressed canary (harmful=2) to auto-stage revert hypothesis, got: %+v", r)
		}
	}

	// 5. Evaluate held-out split & Compact hygiene
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "rsi", "rules", "general.md"), []byte("# retired\n"), 0o600)
	_ = SaveHypothesis(vaultDir, &RSIHypothesis{
		ID:        "rsi-stale-draft-1",
		Status:    "draft",
		CreatedAt: time.Now().UTC().AddDate(0, 0, -10).Format(time.RFC3339),
	})
	compRep, err := Compact(rootDir, vaultDir, 7*24*time.Hour)
	if err != nil {
		t.Fatalf("Compact: %v", err)
	}
	if !compRep.RemovedGeneralFile || compRep.ArchivedDrafts < 1 {
		t.Fatalf("expected Compact to remove retired general.md and archive stale draft, got: %+v", compRep)
	}

	// Verify Ring-3 single-owner duplicate check blocks Ring-2 rule duplicating an active Ring-3 fact
	_, _, _ = vault.AddFactWithOptions(vaultDir, vault.AddFactOptions{
		ID:        "fact-dup-1",
		Topic:     "swe",
		Statement: "Always run git fetch origin before rebasing local worktree branches onto main",
		Verified:  true,
	})
	hDup := &RSIHypothesis{
		ID:          "rsi-dup-ring3",
		Ring:        Ring2SkillTrigger,
		TargetFiles: []string{"local/skills/swe-pulse/SKILL.md"},
		DeltaOp: &ACEDeltaOp{
			Op:      "add_skill_rule",
			RuleID:  "rsi-rule-dup-ring3",
			Section: LearnedRulesHeading,
			Content: "<!-- rsi:rule id=\"rsi-rule-dup-ring3\" -->\n- Always run git fetch origin before rebasing local worktree branches onto main.",
		},
		ProposedDiff: "+- Always run git fetch origin before rebasing local worktree branches onto main.\n",
		Status:       "draft",
		Tier:         "personal",
		CreatedAt:    time.Now().UTC().Format(time.RFC3339),
	}
	_ = SaveHypothesis(vaultDir, hDup)
	verDup, _ := Verify(rootDir, vaultDir, hDup.ID)
	if verDup.Verification.AllPassed || !strings.Contains(verDup.Verification.Gate2Detail, "Ring-3 fact") {
		t.Fatalf("expected Critic-1 to reject Ring-2 duplicate of Ring-3 fact, got: %+v", verDup.Verification)
	}

	evalRep, err := Evaluate(rootDir, vaultDir, 14*24*time.Hour)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if evalRep.GeneratedAt == "" {
		t.Fatalf("expected non-empty EvalReport")
	}
}
