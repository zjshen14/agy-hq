package offer_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/offer"
	"github.com/zjshen14/agy-hq/internal/taint"
)

func setupGmailMockThread(t *testing.T, vaultDir, mockDir, threadID, headMsgID string) {
	t.Helper()
	privGmail := filepath.Join(vaultDir, "private", "gmail")
	if err := os.MkdirAll(privGmail, 0o700); err != nil {
		t.Fatalf("mkdir private/gmail: %v", err)
	}
	reg := map[string]interface{}{
		"defaultAccount": "work",
		"accounts": map[string]interface{}{
			"work": map[string]interface{}{
				"alias":   "work",
				"email":   "user@acme.com",
				"enabled": true,
			},
		},
	}
	regBytes, _ := json.MarshalIndent(reg, "", "  ")
	_ = os.WriteFile(filepath.Join(privGmail, "accounts.json"), regBytes, 0o600)

	threadsDir := filepath.Join(mockDir, "threads")
	_ = os.MkdirAll(threadsDir, 0o700)
	threadDoc := map[string]interface{}{
		"id": threadID,
		"messages": []map[string]interface{}{
			{
				"id":           headMsgID,
				"internalDate": "1731600000000",
				"snippet":      "Please confirm Q4 architecture review",
				"payload": map[string]interface{}{
					"headers": []map[string]string{
						{"name": "From", "value": "Alice Smith <alice@example.com>"},
						{"name": "To", "value": "user@acme.com"},
						{"name": "Subject", "value": "Q4 Architecture Review"},
						{"name": "Authentication-Results", "value": "mx.google.com; dkim=pass header.d=example.com; spf=pass"},
					},
				},
			},
		},
	}
	tBytes, _ := json.MarshalIndent(threadDoc, "", "  ")
	if err := os.WriteFile(filepath.Join(threadsDir, threadID+".json"), tBytes, 0o600); err != nil {
		t.Fatalf("write mock thread: %v", err)
	}
}

func TestOfferStageParticipantLockSupersedeAndPrecondition(t *testing.T) {
	vaultDir := t.TempDir()
	mockDir := t.TempDir()
	t.Setenv("HQ_GMAIL_MOCK_DIR", mockDir)

	setupGmailMockThread(t, vaultDir, mockDir, "th-500", "msg-501")

	convID := "conv-tainted-offer-1"
	_, err := taint.Mark(context.Background(), vaultDir, taint.Record{
		ConvID:    convID,
		Source:    "watch_read:gmail:work:th-500",
		Connector: "gmail",
		Account:   "work",
		ItemKeys:  []string{"th-500", "msg-501"},
	})
	if err != nil {
		t.Fatalf("taint.Mark: %v", err)
	}

	// 1. Display-name smuggling `"alice@example.com" <x@evil.com>` MUST fail RFC 5322 participant_lock!
	_, spoofErr := offer.StageOffer(context.Background(), vaultDir, offer.StageRequest{
		Connector: "gmail",
		Account:   "work",
		Action:    "reply",
		ThreadKey: "th-500",
		Params: map[string]string{
			"to":      `"alice@example.com" <x@evil.com>`,
			"subject": "Re: Q4 Architecture Review",
		},
		Body:     "Looks good.",
		FlagConv: convID,
	})
	if spoofErr == nil || !strings.Contains(spoofErr.Error(), "participant_lock violation") {
		t.Fatalf("expected display-name smuggling recipient to be rejected, got: %v", spoofErr)
	}

	// 2. Foreign thread `th-999` under tainted session MUST fail scope lock!
	_, foreignErr := offer.StageOffer(context.Background(), vaultDir, offer.StageRequest{
		Connector: "gmail",
		Account:   "work",
		Action:    "reply",
		ThreadKey: "th-999",
		Params:    map[string]string{"to": "alice@example.com"},
		Body:      "Looks good.",
		FlagConv:  convID,
	})
	if foreignErr == nil || !strings.Contains(foreignErr.Error(), "scope violation") {
		t.Fatalf("expected foreign thread offer to fail scope lock, got: %v", foreignErr)
	}

	// 3. Valid reply to authenticated participant stages HashVersion: 2 proposal + SupersedePrior
	prop1, err := offer.StageOffer(context.Background(), vaultDir, offer.StageRequest{
		Connector: "gmail",
		Account:   "work",
		Action:    "reply",
		ThreadKey: "th-500",
		Params: map[string]string{
			"to":      "Alice Smith <alice@example.com>",
			"subject": "Re: Q4 Architecture Review",
		},
		Body:     "Draft v1: Thursday 2pm works.",
		FlagConv: convID,
	})
	if err != nil {
		t.Fatalf("valid StageOffer failed: %v", err)
	}
	if prop1.HashVersion != 2 || prop1.PayloadSHA256 == "" || prop1.Metadata["expectedHeadItemId"] != "msg-501" {
		t.Fatalf("unexpected v2 proposal metadata: %+v", prop1)
	}

	// 4. Staging revised draft v2 on same thread supersedes prop1 automatically
	prop2, err := offer.StageOffer(context.Background(), vaultDir, offer.StageRequest{
		Connector: "gmail",
		Account:   "work",
		Action:    "reply",
		ThreadKey: "th-500",
		Params: map[string]string{
			"to":      "alice@example.com",
			"subject": "Re: Q4 Architecture Review",
		},
		Body:     "Draft v2: Friday 10am works.",
		FlagConv: convID,
	})
	if err != nil {
		t.Fatalf("second StageOffer failed: %v", err)
	}
	pending, _ := approvals.List(vaultDir)
	if len(pending) != 1 || pending[0].ID != prop2.ID {
		t.Fatalf("expected prop1 to be superseded leaving only prop2, got %+v", pending)
	}

	// 5. Simulate new message `msg-502` arriving on thread BEFORE human approves -> precondition MUST block!
	setupGmailMockThread(t, vaultDir, mockDir, "th-500", "msg-502")
	_, appErr := approvals.ApproveProposal(vaultDir, prop2.ID, prop2.SHA256[:12])
	if appErr == nil || !strings.Contains(appErr.Error(), "stale offer") {
		t.Fatalf("expected stale thread head precondition to block approval, got: %v", appErr)
	}
}
