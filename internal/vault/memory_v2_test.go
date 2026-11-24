package vault

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemoryV2_EvalFixturesAndAuditIntegrity(t *testing.T) {
	suite, err := RunEvalFixtures()
	if err != nil {
		t.Fatalf("RunEvalFixtures: %v", err)
	}
	if !suite.AllPassed || suite.PassedCount != 4 {
		t.Fatalf("expected all 4 fixtures to pass, got %+v", suite)
	}

	vaultDir := t.TempDir()
	topicsDir := filepath.Join(vaultDir, "memory", "topics")
	_ = os.MkdirAll(topicsDir, 0o700)

	// Duplicate Playbook-Owner + broken link + invalid header
	_ = os.WriteFile(filepath.Join(topicsDir, "alpha-a.md"), []byte("# Alpha A\n> **Summary:** Alpha A summary.\n> **Playbook-Owner:** repo:alpha\n\n## Active Playbook\n- Trap A\n\nSee [missing](./does-not-exist.md).\n"), 0o600)
	_ = os.WriteFile(filepath.Join(topicsDir, "alpha-b.md"), []byte("# Alpha B\n> **Summary:** Alpha B summary.\n> **Playbook-Owner:** repo:alpha\n\n## Active Playbook\n- Trap B\n"), 0o600)
	_ = os.WriteFile(filepath.Join(topicsDir, "bad-header.md"), []byte("No heading here at all.\n"), 0o600)

	// MEMORY.md exceeding byte budget with only 15 long lines
	var bigMem strings.Builder
	bigMem.WriteString("# Active Priorities\n")
	for i := 0; i < 16; i++ {
		bigMem.WriteString("- " + strings.Repeat("x", 185) + "\n")
	}
	_ = os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte(bigMem.String()), 0o600)

	// Dated fact without --expires triggers warning and audit alert
	_, warn, err := AddFactWithOptions(vaultDir, AddFactOptions{
		ID:        "f-dated",
		Topic:     "schedule",
		Statement: "Conference on 2026-12-01 in Seattle",
		Verified:  true,
	})
	if err != nil {
		t.Fatalf("AddFactWithOptions: %v", err)
	}
	if warn == "" {
		t.Fatalf("expected calendar-date-without-expires warning")
	}

	rep, err := AuditMemory(vaultDir, nil)
	if err != nil {
		t.Fatalf("AuditMemory: %v", err)
	}
	if !rep.OverBudget || rep.MemoryBytes <= MaxMemoryIndexBytes {
		t.Fatalf("expected byte-budget OverBudget=true, got %+v", rep)
	}
	if len(rep.DuplicatePlaybookOwners) != 1 {
		t.Fatalf("expected 1 DuplicatePlaybookOwners alert, got %+v", rep.DuplicatePlaybookOwners)
	}
	if len(rep.BrokenTopicLinks) != 1 {
		t.Fatalf("expected 1 BrokenTopicLinks alert, got %+v", rep.BrokenTopicLinks)
	}
	if len(rep.InvalidTopicHeaders) != 1 {
		t.Fatalf("expected 1 InvalidTopicHeaders alert, got %+v", rep.InvalidTopicHeaders)
	}
	if len(rep.MissingTTLEventFacts) != 1 {
		t.Fatalf("expected 1 MissingTTLEventFacts alert, got %+v", rep.MissingTTLEventFacts)
	}
}

func TestMemoryV2_SearchBM25AndCJKAndSecurity(t *testing.T) {
	vaultDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "topics"), 0o700)
	_ = os.MkdirAll(filepath.Join(vaultDir, "private"), 0o700)
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "journal", "2026-11-18"), 0o700)

	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "topics", "memory-arch.md"),
		[]byte("# Memory Architecture\n> **Summary:** 记忆系统与分层索引设计。\n> **Scopes:** repo:agy-hq\n\n## 偏好路由\n长期偏好写入 USER.md 或带作用域的 facts.jsonl。\n"), 0o600)
	_ = os.WriteFile(filepath.Join(vaultDir, "private", "secret.md"),
		[]byte("# Secret\nNEVER_LEAK_KEY_42 偏好路由\n"), 0o600)

	hits, err := SearchMemory(vaultDir, SearchOptions{Query: "偏好路由", Scope: "repo:agy-hq", Limit: 5})
	if err != nil {
		t.Fatalf("SearchMemory CJK: %v", err)
	}
	if len(hits) == 0 || hits[0].Layer != "topics" || !strings.Contains(hits[0].Provenance, "#L") {
		t.Fatalf("expected CJK topic hit with line provenance, got %+v", hits)
	}
	leakHits, _ := SearchMemory(vaultDir, SearchOptions{Query: "NEVER_LEAK_KEY_42", Limit: 5})
	if len(leakHits) != 0 {
		t.Fatalf("expected 0 hits from private/, got %+v", leakHits)
	}
}

func TestMemoryV2_DreamPlanPrecheckAndCursorAndJournalArchive(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 18, 23, 30, 0, 0, time.UTC)

	// 1. Clean empty vault -> HasDelta=false & PrecheckLine() == "" (gatekeeper exit_zero_nonempty)
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o700)
	_ = os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte("# Active Priorities\n- All clean\n"), 0o600)
	planIdle, err := BuildDreamPlan(vaultDir, DreamPlanOptions{Now: now})
	if err != nil {
		t.Fatalf("BuildDreamPlan idle: %v", err)
	}
	if planIdle.HasDelta || planIdle.PrecheckLine() != "" {
		t.Fatalf("expected idle dream-plan HasDelta=false and empty PrecheckLine, got %+v (%q)", planIdle, planIdle.PrecheckLine())
	}

	// 2. Seed an old journal (20 days old) + a recent untainted journal + a recent tainted journal + rejected intervention
	oldDay := now.AddDate(0, 0, -20).Format("2006-01-02")
	today := now.Format("2006-01-02")
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "journal", oldDay), 0o700)
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "journal", today), 0o700)

	oldRec, _ := json.Marshal(JournalRecord{
		Timestamp:      now.AddDate(0, 0, -20).Format(time.RFC3339),
		ConversationID: "conv-long-fd",
		ExecutionNum:   1,
		StepCount:      142,
		TranscriptPath: "/tmp/old-fd-transcript.jsonl",
		UserPrompt:     "Old front desk turn",
	})
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "journal", oldDay, "conv-long-fd-1.json"), oldRec, 0o600)

	newRec, _ := json.Marshal(JournalRecord{
		Timestamp:      now.Add(-2 * time.Hour).Format(time.RFC3339),
		ConversationID: "conv-active-1",
		ExecutionNum:   1,
		StepCount:      12,
		TurnErrorCount: 1,
		FilesModified:  []string{"internal/vault/memory.go"},
		UserPrompt:     "Always prefer concise diffs",
	})
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "journal", today, "conv-active-1-1.json"), newRec, 0o600)

	taintedRec, _ := json.Marshal(JournalRecord{
		Timestamp:      now.Add(-1 * time.Hour).Format(time.RFC3339),
		ConversationID: "conv-tainted-1",
		ExecutionNum:   1,
		TaintedSession: true,
		UserPrompt:     "Untrusted inbound email",
	})
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "journal", today, "conv-tainted-1-1.json"), taintedRec, 0o600)

	rejLine := fmt.Sprintf(`{"timestamp":%q,"decision":"rejected","proposal":{"actionType":"gchat_send","summary":"Do not send evening status pings"}}`+"\n", now.Add(-3*time.Hour).Format(time.RFC3339))
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "interventions.jsonl"), []byte(rejLine), 0o600)

	planActive, err := BuildDreamPlan(vaultDir, DreamPlanOptions{Now: now})
	if err != nil {
		t.Fatalf("BuildDreamPlan active: %v", err)
	}
	if !planActive.HasDelta || planActive.PrecheckLine() == "" {
		t.Fatalf("expected active dream-plan delta, got %+v", planActive)
	}
	if len(planActive.SalientConversations) != 1 || planActive.SalientConversations[0].ConversationID != "conv-active-1" {
		t.Fatalf("expected only untainted recent conv-active-1, got %+v", planActive.SalientConversations)
	}
	if planActive.SkippedTaintedTurns != 1 {
		t.Fatalf("expected SkippedTaintedTurns=1, got %d", planActive.SkippedTaintedTurns)
	}
	if len(planActive.NegativeConstraints) != 1 {
		t.Fatalf("expected 1 negative constraint from rejected intervention, got %+v", planActive.NegativeConstraints)
	}

	// 3. Archive >14d journals and verify archived stepCount & transcriptPath fallback
	archivedCnt, err := ArchiveOldJournals(vaultDir, DefaultHotArchiveDays, now)
	if err != nil || archivedCnt != 1 {
		t.Fatalf("ArchiveOldJournals: count=%d err=%v", archivedCnt, err)
	}
	if maxStep := FindArchivedMaxStepCount(vaultDir, "conv-long-fd"); maxStep != 142 {
		t.Fatalf("expected FindArchivedMaxStepCount=142, got %d", maxStep)
	}
	if txPath := FindArchivedTranscriptPath(vaultDir, "conv-long-fd"); txPath != "/tmp/old-fd-transcript.jsonl" {
		t.Fatalf("expected archived transcript path, got %q", txPath)
	}

	// 4. Advance cursor strictly on commit -> subsequent dream-plan is idle
	if err := AdvanceDreamCursor(vaultDir, "conv-dream-run", now); err != nil {
		t.Fatalf("AdvanceDreamCursor: %v", err)
	}
	planAfterCommit, err := BuildDreamPlan(vaultDir, DreamPlanOptions{Now: now.Add(time.Minute)})
	if err != nil {
		t.Fatalf("BuildDreamPlan after commit: %v", err)
	}
	if planAfterCommit.HasDelta || planAfterCommit.PrecheckLine() != "" {
		t.Fatalf("expected idle dream-plan after cursor advance & journal archive, got %+v", planAfterCommit)
	}
}

func TestMemoryV2_EntityOverflowAndCascadingForget(t *testing.T) {
	vaultDir := t.TempDir()

	// 1. Entity overflow (> 10 notes rolls older bullets into entities/archived/<slug>-history.md)
	var notes []string
	for i := 1; i <= 12; i++ {
		notes = append(notes, fmt.Sprintf("[2026-11-%02d] Note #%d for partner", i, i))
	}
	ent, err := UpsertEntity(vaultDir, EntityDossier{
		Slug:  "partner-acme",
		Name:  "Partner Acme",
		Notes: notes,
	}, "")
	if err != nil {
		t.Fatalf("UpsertEntity: %v", err)
	}
	if len(ent.Notes) != MaxEntityBullets {
		t.Fatalf("expected active notes capped at %d, got %d", MaxEntityBullets, len(ent.Notes))
	}
	archBytes, err := os.ReadFile(filepath.Join(vaultDir, "memory", "entities", "archived", "partner-acme-history.md"))
	if err != nil || !strings.Contains(string(archBytes), "Note #1 for partner") {
		t.Fatalf("expected overflow notes in archived history, err=%v content=%s", err, string(archBytes))
	}

	// 2. Cascading forget scrubs facts, MEMORY.md, hot journal, archived journal, progress, obs, and rsi
	token := "FORGET_ME_SECRET_777"
	_, _, _ = AddFactWithOptions(vaultDir, AddFactOptions{ID: "f-secret", Topic: "misc", Statement: "Contains " + token, SourceConv: "conv-src-1", Verified: true})
	_ = os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte("# Active Priorities\n- Track "+token+"\n"), 0o600)
	_, _ = SaveProgress(vaultDir, ProgressCheckpoint{TaskSlug: "p-1", Goal: "Check " + token}, time.Now().UTC())
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "journal", "2026-11-18"), 0o700)
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "journal", "2026-11-18", "c1-1.json"),
		[]byte(`{"conversationId":"conv-j-1","userPrompt":"Mentions `+token+`"}`), 0o600)
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "journal", "archive"), 0o700)
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "journal", "archive", "2026-10.jsonl"),
		[]byte(`{"conversationId":"conv-arch-1","responseSummary":"Archived `+token+`"}`+"\n"), 0o600)
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "obs"), 0o700)
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "obs", "index.json"),
		[]byte(`{"conversations":{"conv-obs-1":{"conversationId":"conv-obs-1","promptPreview":"Obs `+token+`"}}}`), 0o600)
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "rsi"), 0o700)
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "rsi", "observations.jsonl"),
		[]byte(`{"id":"obs-1","errorExcerpt":"RSI `+token+`"}`+"\n"), 0o600)

	rep, err := ForgetMemory(vaultDir, token, "", "", false, false)
	if err != nil {
		t.Fatalf("ForgetMemory: %v", err)
	}
	if len(rep.ExpiredFactIDs) != 1 || rep.MemoryLinesRemoved != 1 {
		t.Fatalf("unexpected fact/memory scrub counts: %+v", rep)
	}
	if len(rep.ScrubbedFiles) < 5 {
		t.Fatalf("expected >=5 scrubbed files (progress, hot journal, archived journal, obs, rsi), got %+v", rep.ScrubbedFiles)
	}
	if len(rep.SourceConvIDs) < 3 {
		t.Fatalf("expected >=3 sourceConvIds reported, got %+v", rep.SourceConvIDs)
	}
	// Verify token no longer exists in any of those files
	for _, rel := range rep.ScrubbedFiles {
		b, _ := os.ReadFile(filepath.Join(vaultDir, filepath.FromSlash(rel)))
		if strings.Contains(string(b), token) {
			t.Fatalf("token %q still present in %s after ForgetMemory", token, rel)
		}
	}
}

func TestMemoryV2_AdversarialTwoRepoSharedKeywordIsolation(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)
	older := now.Add(-4 * time.Hour)

	// Older verified repo-alpha fact about its web package
	_, _, err := AddFactWithOptions(vaultDir, AddFactOptions{
		ID:        "f-alpha-web",
		Topic:     "web-package",
		Statement: "repo-alpha web package lives in apps/alpha-web and deploys via vite",
		Scopes:    []string{"repo:repo-alpha"},
		Verified:  true,
		Now:       older,
	})
	if err != nil {
		t.Fatalf("add alpha fact: %v", err)
	}
	// Flood repo-beta with newer verified facts sharing the same "web package" keywords
	for i := 1; i <= 6; i++ {
		_, _, err := AddFactWithOptions(vaultDir, AddFactOptions{
			ID:        fmt.Sprintf("f-beta-web-%d", i),
			Topic:     "web-package",
			Statement: fmt.Sprintf("repo-beta web package folder services/beta-web note %d", i),
			Scopes:    []string{"repo:repo-beta"},
			Verified:  true,
			Now:       now.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			t.Fatalf("add beta fact %d: %v", i, err)
		}
	}
	// Newer non-conflicting domain fact sharing prompt keywords must still rank BELOW verified repo-alpha fact
	_, _, err = AddFactWithOptions(vaultDir, AddFactOptions{
		ID:        "f-domain-web",
		Topic:     "web-package",
		Statement: "General web package folder layout guidelines for frontend apps",
		Scopes:    []string{"domain:frontend"},
		Verified:  true,
		Now:       now.Add(10 * time.Minute),
	})
	if err != nil {
		t.Fatalf("add domain fact: %v", err)
	}

	active, _, _, err := LoadFactsStateAt(vaultDir, now.Add(15*time.Minute))
	if err != nil {
		t.Fatalf("LoadFactsStateAt: %v", err)
	}
	ctx := SessionScopeContext{
		Role:       "task",
		RepoSlug:   "repo-alpha",
		Scopes:     []string{"repo:repo-alpha"},
		PromptText: "Where is the web package folder in this repo?",
	}
	sel := SelectFactsForSession(active, ctx, now.Add(15*time.Minute), 10, 2400)
	if len(sel) == 0 {
		t.Fatalf("expected non-empty selection for repo-alpha")
	}
	if sel[0].ID != "f-alpha-web" {
		t.Fatalf("expected verified repo:repo-alpha fact first (ahead of newer domain lexMatched fact), got %s (%+v)", sel[0].ID, sel)
	}
	for _, f := range sel {
		if strings.HasPrefix(f.ID, "f-beta-") {
			t.Fatalf("cross-repo leak: repo-beta fact %s leaked into repo-alpha session", f.ID)
		}
	}

	// Topic pages: conflicting repo-beta topic must be excluded from both FilterTopicsForScope and MatchContextHints
	topicsDir := filepath.Join(vaultDir, "memory", "topics")
	_ = os.MkdirAll(topicsDir, 0o700)
	_ = os.WriteFile(filepath.Join(topicsDir, "web-package-alpha.md"), []byte(
		"# Alpha Web Package\n> **Summary:** Web package folder structure for repo-alpha.\n> **Scopes:** repo:repo-alpha\n> **Playbook-Owner:** repo:repo-alpha\n\n## Active Playbook\n- Alpha web package lives in apps/alpha-web.\n",
	), 0o600)
	_ = os.WriteFile(filepath.Join(topicsDir, "web-package-beta.md"), []byte(
		"# Beta Web Package\n> **Summary:** Web package folder structure for repo-beta.\n> **Scopes:** repo:repo-beta\n> **Playbook-Owner:** repo:repo-beta\n\n## Active Playbook\n- Beta web package lives in services/beta-web.\n",
	), 0o600)

	topics, err := LoadTopicIndex(vaultDir)
	if err != nil {
		t.Fatalf("LoadTopicIndex: %v", err)
	}
	for _, tp := range FilterTopicsForScope(topics, ctx, 5) {
		if tp.Slug == "web-package-beta" {
			t.Fatalf("FilterTopicsForScope leaked conflicting repo-beta topic into repo-alpha")
		}
	}
	for _, tp := range MatchContextHints(topics, ctx, 5) {
		if tp.Slug == "web-package-beta" {
			t.Fatalf("MatchContextHints leaked conflicting repo-beta topic into repo-alpha")
		}
	}
}
