package vault

import (
	"testing"
)

func TestLookupEntitiesRedactedClosesNotesOracleAndEmptyQuery(t *testing.T) {
	vaultDir := t.TempDir()
	_, err := UpsertEntity(vaultDir, EntityDossier{
		Slug:    "alice-chen",
		Name:    "Alice Chen",
		Kind:    "person",
		Emails:  []string{"alice@example.com"},
		Handles: []string{"@achen"},
		Spaces:  []string{"spaces/TESTSECRET99"},
		Context: "Confidential promotion calibration panel",
		Notes:   []string{"Salary band discussion pending"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}

	// Empty query must fail closed in quarantined mode
	if _, err := LookupEntitiesRedacted(vaultDir, ""); err == nil {
		t.Fatalf("expected empty query to fail closed in LookupEntitiesRedacted")
	}
	// Queries targeting private Context, Notes, or Spaces must return 0 hits (no boolean oracle!)
	for _, secretQ := range []string{"calibration", "salary", "TESTSECRET99"} {
		hits, err := LookupEntitiesRedacted(vaultDir, secretQ)
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", secretQ, err)
		}
		if len(hits) != 0 {
			t.Fatalf("notes/context/spaces oracle leaked hit for %q: %+v", secretQ, hits)
		}
	}
	// Public routing fields (Name / Email / Handle) match AND strip private fields
	hits, err := LookupEntitiesRedacted(vaultDir, "alice")
	if err != nil {
		t.Fatalf("unexpected error for alice: %v", err)
	}
	if len(hits) != 1 || hits[0].Name != "Alice Chen" {
		t.Fatalf("expected 1 match for alice, got %+v", hits)
	}
	if hits[0].Context != "" || len(hits[0].Notes) != 0 || len(hits[0].Spaces) != 0 || hits[0].Path != "" {
		t.Fatalf("expected private fields stripped from redacted dossier, got %+v", hits[0])
	}
}
