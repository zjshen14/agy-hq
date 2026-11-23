package vault

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMemoryEngine_TopicDiscoveryAndFactLifecycle(t *testing.T) {
	vaultDir := t.TempDir()
	topicsDir := filepath.Join(vaultDir, "memory", "topics")
	if err := os.MkdirAll(topicsDir, 0o755); err != nil {
		t.Fatalf("mkdir topics: %v", err)
	}

	// 1. Seed two topic pages and verify LoadTopicIndex extracts Title and Summary
	t1 := "# Architecture Overview\n> **Summary:** Core two-layer Vault and microkernel architecture.\n\nBody text.\n"
	t2 := "# Investing Playbook\n> **Updated:** 2026-11-08\n> **Summary:** SEC 10-K/10-Q semantic diff and thesis kill criteria.\n"
	_ = os.WriteFile(filepath.Join(topicsDir, "01-arch.md"), []byte(t1), 0o644)
	_ = os.WriteFile(filepath.Join(topicsDir, "02-investing.md"), []byte(t2), 0o644)

	pages, err := LoadTopicIndex(vaultDir)
	if err != nil || len(pages) != 2 {
		t.Fatalf("expected 2 topic pages, got %d (%v)", len(pages), err)
	}
	if pages[0].Title != "Architecture Overview" || pages[0].Summary != "Core two-layer Vault and microkernel architecture." {
		t.Fatalf("unexpected page[0]: %+v", pages[0])
	}
	if pages[1].Title != "Investing Playbook" || pages[1].Summary != "SEC 10-K/10-Q semantic diff and thesis kill criteria." {
		t.Fatalf("unexpected page[1]: %+v", pages[1])
	}

	// 2. Test AddFactWithOptions, PruneFact, and LoadFactsState ID deduplication
	f1, _, err := AddFactWithOptions(vaultDir, AddFactOptions{ID: "fact-100", Topic: "arch", Statement: "Old unverified assumption", SourceConv: "conv-1"})
	if err != nil || f1.ID != "fact-100" {
		t.Fatalf("AddFactWithOptions f1 failed: %v", err)
	}
	f2, _, err := AddFactWithOptions(vaultDir, AddFactOptions{Topic: "arch", Statement: "Verified 4-layer memory hierarchy", SourceConv: "conv-2", Verified: true})
	if err != nil || f2.ID == "" {
		t.Fatalf("AddFactWithOptions f2 failed: %v", err)
	}

	active, _, _, err := LoadFactsState(vaultDir)
	if err != nil || len(active) != 2 {
		t.Fatalf("expected 2 active facts, got %d (%v)", len(active), err)
	}

	// Append a manual tombstone line with same ID to simulate LLM or CLI supersession before compaction
	pruned, err := PruneFact(vaultDir, "fact-100", f2.ID)
	if err != nil || pruned.ValidTo == nil || pruned.SupersededBy != f2.ID {
		t.Fatalf("PruneFact failed: %+v (%v)", pruned, err)
	}

	activeAfter, expiredAfter, rawLines, err := LoadFactsState(vaultDir)
	if err != nil {
		t.Fatalf("LoadFactsState: %v", err)
	}
	if len(activeAfter) != 1 || activeAfter[0].ID != f2.ID {
		t.Fatalf("expected only %s active after prune, got %+v", f2.ID, activeAfter)
	}
	if len(expiredAfter) != 1 || expiredAfter[0].ID != "fact-100" {
		t.Fatalf("expected fact-100 in expiredAfter, got %+v", expiredAfter)
	}
	if rawLines != 2 {
		t.Fatalf("expected 2 canonical lines after PruneFact compaction, got %d", rawLines)
	}

	// 3. Test AuditMemory stale reference detection & dual line/byte budget
	_ = os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte("# Memory\n- Working on legacy-closed-task\n"), 0o600)
	rep, err := AuditMemory(vaultDir, []string{"legacy-closed-task"})
	if err != nil {
		t.Fatalf("AuditMemory: %v", err)
	}
	if rep.OverBudget {
		t.Fatalf("expected OverBudget=false for 2-line MEMORY.md")
	}
	if len(rep.OrphanTopicSlugs) != 0 {
		t.Fatalf("expected 0 OrphanTopicSlugs under RFC-0024 orthogonal MEMORY.md, got %+v", rep.OrphanTopicSlugs)
	}
	if len(rep.StaleReferences) != 1 {
		t.Fatalf("expected 1 stale reference for legacy-closed-task, got %+v", rep.StaleReferences)
	}
}
