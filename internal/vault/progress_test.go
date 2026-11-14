package vault

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestProgressSaveLoadListAndArchive(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 14, 15, 30, 0, 0, time.UTC)

	saved, err := SaveProgress(vaultDir, ProgressCheckpoint{
		TaskSlug:  "headquarter-event-inbox",
		Goal:      "Ship crash-safe Event Inbox",
		Completed: "- Built internal/events with flock\n- Verified unit tests",
		NextSteps: "- Wire ProcessInbox and HandleStop",
		Pointers:  "- branch: hq/always-on-agents-gap",
	}, now)
	if err != nil {
		t.Fatalf("SaveProgress: %v", err)
	}

	loaded, raw, ok, err := LoadProgress(vaultDir, "headquarter-event-inbox")
	if err != nil || !ok {
		t.Fatalf("LoadProgress: ok=%v err=%v", ok, err)
	}
	if loaded.Goal != saved.Goal || !strings.Contains(raw, "## Next Steps") {
		t.Fatalf("unexpected loaded checkpoint: %+v\n%s", loaded, raw)
	}

	list, err := ListProgress(vaultDir)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListProgress: len=%d err=%v", len(list), err)
	}

	archPath, err := ArchiveProgress(vaultDir, "headquarter-event-inbox", now)
	if err != nil {
		t.Fatalf("ArchiveProgress: %v", err)
	}
	if _, err := os.Stat(archPath); err != nil {
		t.Fatalf("archived progress missing at %s: %v", archPath, err)
	}
	if _, _, stillThere, _ := LoadProgress(vaultDir, "headquarter-event-inbox"); stillThere {
		t.Fatalf("expected active progress file removed after archive")
	}
}
