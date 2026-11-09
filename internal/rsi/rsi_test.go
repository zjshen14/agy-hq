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
	_ = os.MkdirAll(filepath.Join(vaultDir, "private", "gchat", "drafts"), 0o755)
	_ = os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte("# Roadmap Index\n- Active item\n"), 0o644)

	for _, sk := range []string{"gchat", "swe-pulse", "thesis-check"} {
		skDir := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", sk)
		_ = os.MkdirAll(skDir, 0o755)
		content := "---\nname: " + sk + "\ndescription: Test skill for " + sk + "\n---\n\n# " + sk + "\n\nCore operational steps.\n"
		_ = os.WriteFile(filepath.Join(skDir, "SKILL.md"), []byte(content), 0o644)
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
			"modelName":      "gemini-3.8-flash-high",
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
	if _, err := Hypothesize(rootDir, vaultDir, "", false); err == nil || !strings.Contains(err.Error(), "Quota Circuit Breaker") {
		t.Fatalf("expected 75%% Quota Circuit Breaker to block Hypothesize, got: %v", err)
	}
	_ = os.Remove(filepath.Join(vaultDir, "memory", "quota_state.json"))

	// 4. Run Stage 2: Hypothesize
	hyps, err := Hypothesize(rootDir, vaultDir, "", false)
	if err != nil {
		t.Fatalf("Hypothesize failed: %v", err)
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
	_, absGChatSkill := ResolveSkillFile(rootDir, "gchat")
	skillAfter, _ := os.ReadFile(absGChatSkill)
	if !strings.Contains(string(skillAfter), LearnedRulesHeading) || !strings.Contains(string(skillAfter), "<!-- rsi:rule id=") {
		t.Fatalf("expected SKILL.md to contain learned operational rule block, got:\n%s", string(skillAfter))
	}

	// 7. Test Model-Upgrade Ablation (`Ablate` when upgrading to gemini-5-ultra)
	ablated, err := Ablate(rootDir, vaultDir, "gemini-5-ultra")
	if err != nil || len(ablated) == 0 {
		t.Fatalf("expected Ablate to stage deprecation hypothesis for gemini-3.8-flash-high scoped rule when upgrading to gemini-5-ultra, got %d (%v)", len(ablated), err)
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
		TargetFiles:    []string{".agents/plugins/agy-hq/skills/gchat/SKILL.md"},
		ProposedDiff:   "--- a/.agents/plugins/agy-hq/skills/gchat/SKILL.md\n+++ b/.agents/plugins/agy-hq/skills/gchat/SKILL.md\n+rule\n",
	}
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, hTainted, taintedObs); ok || !strings.Contains(reason, "tainted") {
		t.Fatalf("expected tainted observation to be blocked from mutating Ring 2, got ok=%v reason=%s", ok, reason)
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
	relThesis, _ := ResolveSkillFile(rootDir, "thesis-check")
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
