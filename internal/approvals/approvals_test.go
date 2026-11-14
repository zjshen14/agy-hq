package approvals

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
)

func TestCreateApproveAndReject(t *testing.T) {
	vaultDir := t.TempDir()
	prop, err := Create(vaultDir, "test_echo", "Echo test message", "echo approved-ok", "", "conv-1")
	if err != nil {
		t.Fatalf("Create failed: %v", err)
	}
	if prop.ExpiresAt == "" {
		t.Fatalf("expected default 24h ExpiresAt on new proposal")
	}

	list, err := List(vaultDir)
	if err != nil || len(list) != 1 {
		t.Fatalf("expected 1 pending proposal, got %d (%v)", len(list), err)
	}

	// Wrong SHA prefix must fail
	if _, err := Approve(vaultDir, prop.ID, "000000000000"); err == nil {
		t.Fatalf("expected error on mismatched sha256 prefix")
	}

	// Empty or short SHA prefix must fail
	if _, err := Approve(vaultDir, prop.ID, ""); err == nil {
		t.Fatalf("expected error on empty sha256 prefix")
	}
	if _, err := Approve(vaultDir, prop.ID, prop.SHA256[:4]); err == nil {
		t.Fatalf("expected error on short (<8 char) sha256 prefix")
	}

	// Path traversal proposal ID must fail on Approve and Reject
	if _, err := Approve(vaultDir, "../../outside", prop.SHA256[:12]); err == nil {
		t.Fatalf("expected error on path traversal proposal ID in Approve")
	}
	if err := Reject(vaultDir, "../../outside", "nope"); err == nil {
		t.Fatalf("expected error on path traversal proposal ID in Reject")
	}

	// Valid SHA prefix must execute command and remove from pending
	out, err := Approve(vaultDir, prop.ID, prop.SHA256[:12])
	if err != nil {
		t.Fatalf("Approve failed: %v", err)
	}
	if !strings.Contains(out, "approved-ok") {
		t.Fatalf("unexpected command output: %q", out)
	}

	listAfter, _ := List(vaultDir)
	if len(listAfter) != 0 {
		t.Fatalf("expected 0 pending proposals after approval, got %d", len(listAfter))
	}
}

func TestProposalTTLExpirationAndContextualPIIScan(t *testing.T) {
	vaultDir := t.TempDir()
	privDir := filepath.Join(vaultDir, "private")
	_ = os.MkdirAll(privDir, 0o700)
	_ = os.WriteFile(filepath.Join(privDir, "pii-denylist.txt"), []byte("ProjectNightfall\n"), 0o600)

	pastExp := time.Now().Add(-2 * time.Hour).UTC().Format(time.RFC3339)
	prop, err := CreateWithMetadata(vaultDir, "gmail_send", "Send contract draft", "echo never-run", "", "conv-ttl", map[string]string{
		"expiresAt":    pastExp,
		"draftPayload": "Includes ProjectNightfall, SSN 123-45-6789, and 742 Evergreen Avenue.",
	})
	if err != nil {
		t.Fatalf("CreateWithMetadata: %v", err)
	}
	if prop.RiskTier != "pii_warning" || len(prop.RiskFlags) < 3 {
		t.Fatalf("expected pii_warning and >=3 RiskFlags, got tier=%q flags=%v", prop.RiskTier, prop.RiskFlags)
	}

	// Attempting to approve an expired proposal must fail and record "expired"
	if _, err := ApproveProposal(vaultDir, prop.ID, prop.SHA256[:12]); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expected expired error on ApproveProposal, got: %v", err)
	}

	// Verify interventions.jsonl recorded Decision="expired"
	interBytes, err := os.ReadFile(filepath.Join(vaultDir, "memory", "interventions.jsonl"))
	if err != nil || !strings.Contains(string(interBytes), `"decision":"expired"`) {
		t.Fatalf("expected expired record in interventions.jsonl, got: %s (%v)", string(interBytes), err)
	}
}

func TestStructuredArgvScrubbedEnvAndDeferredResumption(t *testing.T) {
	vaultDir := t.TempDir()
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "secret-csrf-must-not-leak")

	// 1. Structured CommandArgv executes without sh -c (so "$(...)" or "; echo pwned" is treated literally)
	// and strips ANTIGRAVITY_CSRF_TOKEN from child environment.
	propApprove, err := CreateWithOptions(vaultDir, CreateOptions{
		ActionType:       "external_action",
		Summary:          "Literal argv execution",
		CommandArgv:      []string{"/usr/bin/env"},
		ConversationID:   "00000000-0000-0000-0000-000000000001",
		ResumeOnDecide:   true,
		ContinuationNote: "Register 48h WaitingOn SLA",
		OriginSkill:      "inbox-triage",
	})
	if err != nil {
		t.Fatalf("CreateWithOptions: %v", err)
	}
	out, err := ApproveProposal(vaultDir, propApprove.ID, propApprove.SHA256[:12])
	if err != nil {
		t.Fatalf("ApproveProposal argv: %v", err)
	}
	if strings.Contains(out, "ANTIGRAVITY_CSRF_TOKEN=") {
		t.Fatalf("ApproveProposal leaked ANTIGRAVITY_CSRF_TOKEN into child env:\n%s", out)
	}

	// Verify ResumeOnDecide enqueued an approval_decided event targeting conv-0001
	pendingEvents, err := events.List(vaultDir, "pending", false, 0)
	if err != nil || len(pendingEvents) != 1 {
		t.Fatalf("expected 1 resumption event on ResumeOnDecide approve, got %d (%v)", len(pendingEvents), err)
	}
	if pendingEvents[0].SessionID != "00000000-0000-0000-0000-000000000001" || !strings.Contains(pendingEvents[0].Prompt, "Register 48h WaitingOn SLA") {
		t.Fatalf("unexpected approve resumption event: %+v", pendingEvents[0])
	}

	// 2. Rejecting with custom user steering reason automatically enqueues a revision resumption event
	propReject, err := CreateWithOptions(vaultDir, CreateOptions{
		ActionType:     "gmail_send",
		Summary:        "Reply proposing Tuesday 10am",
		CommandArgv:    []string{"echo", "send"},
		ConversationID: "00000000-0000-0000-0000-000000000002",
		OriginSkill:    "inbox-triage",
	})
	if err != nil {
		t.Fatalf("CreateWithOptions reject: %v", err)
	}
	if err := RejectProposal(vaultDir, propReject.ID, "Move proposed meeting to Thursday 2pm PT"); err != nil {
		t.Fatalf("RejectProposal: %v", err)
	}
	pendingEvents2, _ := events.List(vaultDir, "pending", false, 0)
	if len(pendingEvents2) != 2 {
		t.Fatalf("expected 2nd resumption event after steering rejection, got %d", len(pendingEvents2))
	}
	if pendingEvents2[1].SessionID != "00000000-0000-0000-0000-000000000002" || !strings.Contains(pendingEvents2[1].Prompt, "Thursday 2pm PT") {
		t.Fatalf("unexpected rejection resumption event: %+v", pendingEvents2[1])
	}
}
