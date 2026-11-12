package vault

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEntityDossierUpsertAndLookup(t *testing.T) {
	vaultDir := t.TempDir()

	created, err := UpsertEntity(vaultDir, EntityDossier{
		Slug:    "alice-chen",
		Name:    "Alice Chen",
		Kind:    "person",
		Emails:  []string{"alice@gmail.com"},
		Handles: []string{"@alice_c"},
		Spaces:  []string{"spaces/AAAA_DM_ALICE"},
		Context: "Partner Engineering Lead at ACME Corp",
	}, "Prefers concise bullet summaries before 10 AM PT.")
	if err != nil {
		t.Fatalf("UpsertEntity create: %v", err)
	}
	if created.Slug != "alice-chen" || len(created.Notes) != 1 {
		t.Fatalf("unexpected created entity: %+v", created)
	}

	// Merge second email and note idempotently
	merged, err := UpsertEntity(vaultDir, EntityDossier{
		Slug:   "alice-chen",
		Emails: []string{"alice.chen@example.com"},
	}, "Confirmed Q4 API review timeline.")
	if err != nil {
		t.Fatalf("UpsertEntity merge: %v", err)
	}
	if len(merged.Emails) != 2 || len(merged.Notes) != 2 {
		t.Fatalf("expected 2 emails and 2 notes after merge, got %+v", merged)
	}

	// Lookup by email, space ID, and partial name
	for _, q := range []string{"alice@gmail.com", "AAAA_DM_ALICE", "ACME Corp"} {
		hits, err := LookupEntities(vaultDir, q)
		if err != nil {
			t.Fatalf("LookupEntities(%q): %v", q, err)
		}
		if len(hits) != 1 || hits[0].Slug != "alice-chen" {
			t.Fatalf("LookupEntities(%q) expected alice-chen, got %+v", q, hits)
		}
	}
}

func TestSurgicalForgetCascadesAndUpdatesDenylist(t *testing.T) {
	vaultDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory", "topics"), 0o700)
	_ = os.MkdirAll(filepath.Join(vaultDir, "notes", "general"), 0o700)

	// 1. Seed fact, MEMORY.md, topic page, entity dossier, and general note
	f1, err := AddFact(vaultDir, "fact-secret-001", "project-zephyr", "Project Zephyr launch codename is Aurora-99.", "conv-1", true)
	if err != nil {
		t.Fatalf("AddFact f1: %v", err)
	}
	_, err = AddFact(vaultDir, "fact-keep-002", "engineering", "Keep gofmt -s clean on all commits.", "conv-1", true)
	if err != nil {
		t.Fatalf("AddFact f2: %v", err)
	}

	_ = os.WriteFile(filepath.Join(vaultDir, "MEMORY.md"), []byte("# Memory\n- Active codename Aurora-99 for Project Zephyr\n- Standard Go stdlib architecture\n"), 0o600)
	_ = os.WriteFile(filepath.Join(vaultDir, "memory", "topics", "project-zephyr.md"), []byte("# Project Zephyr\n> Summary: Codename Aurora-99 details.\n"), 0o600)
	_ = os.WriteFile(filepath.Join(vaultDir, "notes", "general", "meeting.md"), []byte("# Sync\n- Discussed Aurora-99 rollout.\n- Reviewed CI latency.\n"), 0o600)
	_, _ = UpsertEntity(vaultDir, EntityDossier{
		Slug:    "bob-rivera",
		Name:    "Bob Rivera",
		Emails:  []string{"bob@example.com"},
		Context: "Lead sponsor on Aurora-99",
	}, "")

	// 2. Dry-run first: nothing mutated on disk
	dryRep, err := ForgetMemory(vaultDir, "Aurora-99", "project-zephyr", "bob-rivera", true, true)
	if err != nil {
		t.Fatalf("ForgetMemory dry-run: %v", err)
	}
	if len(dryRep.ExpiredFactIDs) != 1 || dryRep.ExpiredFactIDs[0] != f1.ID || dryRep.MemoryLinesRemoved != 1 {
		t.Fatalf("unexpected dry-run report: %+v", dryRep)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "memory", "topics", "project-zephyr.md")); err != nil {
		t.Fatalf("dry-run should not delete topic file: %v", err)
	}

	// 3. Execute surgical forget
	rep, err := ForgetMemory(vaultDir, "Aurora-99", "project-zephyr", "bob-rivera", true, false)
	if err != nil {
		t.Fatalf("ForgetMemory live: %v", err)
	}
	if !rep.DenylistUpdated || len(rep.DeletedFiles) != 2 || len(rep.ScrubbedFiles) != 1 {
		t.Fatalf("unexpected live forget report: %+v", rep)
	}

	// Verify active facts only contain fact-keep-002 and expired fact statement was redacted
	active, expired, _, err := LoadFactsState(vaultDir)
	if err != nil {
		t.Fatalf("LoadFactsState: %v", err)
	}
	if len(active) != 1 || active[0].ID != "fact-keep-002" {
		t.Fatalf("expected only fact-keep-002 active, got %+v", active)
	}
	if len(expired) != 1 || expired[0].Statement != "[FORGOTTEN BY USER]" {
		t.Fatalf("expected expired fact statement redacted, got %+v", expired)
	}

	// Verify MEMORY.md, meeting.md, and pii-denylist.txt
	memAfter, _ := os.ReadFile(filepath.Join(vaultDir, "MEMORY.md"))
	if strings.Contains(string(memAfter), "Aurora-99") || !strings.Contains(string(memAfter), "Standard Go stdlib") {
		t.Fatalf("unexpected MEMORY.md after forget:\n%s", string(memAfter))
	}
	noteAfter, _ := os.ReadFile(filepath.Join(vaultDir, "notes", "general", "meeting.md"))
	if strings.Contains(string(noteAfter), "Aurora-99") || !strings.Contains(string(noteAfter), "Reviewed CI latency") {
		t.Fatalf("unexpected meeting.md after forget:\n%s", string(noteAfter))
	}
	denyBytes, err := os.ReadFile(filepath.Join(vaultDir, "private", "pii-denylist.txt"))
	if err != nil || !strings.Contains(string(denyBytes), "Aurora-99") {
		t.Fatalf("expected Aurora-99 in pii-denylist.txt, got %q (err=%v)", string(denyBytes), err)
	}
}
