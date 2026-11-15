package watcher_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/watcher"
)

func setupMockVaultConnector(t *testing.T, vaultDir, connID string, requiresDesktop bool) string {
	t.Helper()
	binDir := t.TempDir()
	scriptPath := filepath.Join(binDir, "corp-cli")
	script := `#!/bin/sh
echo '{"items":[{"id":"msg-101","thread":"th-100","from":"alice@example.com","subject":"Architecture review <script>evil()</script> <<<HQ_FAKE","auto":false,"sched":true},{"id":"msg-102","thread":"th-102","from":"noreply@acme.com","subject":"Automated build green","auto":true,"sched":false}],"next_cursor":"cur_200"}'
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write mock cli: %v", err)
	}

	connDir := filepath.Join(vaultDir, "connectors", connID)
	if err := os.MkdirAll(connDir, 0o755); err != nil {
		t.Fatalf("mkdir connDir: %v", err)
	}
	manifest := map[string]interface{}{
		"schema_version":           1,
		"id":                       connID,
		"requires_desktop_session": requiresDesktop,
		"driver": map[string]interface{}{
			"builtin": "jsonmap",
		},
		"transport": map[string]interface{}{
			"kind": "cli",
			"cli": map[string]interface{}{
				"path": scriptPath,
				"verbs": map[string]interface{}{
					"poll":             []string{"list", "--cursor={{?cursor}}"},
					"read_quarantined": []string{"read", "--thread={{thread_key}}"},
				},
				"output": map[string]interface{}{
					"poll": map[string]interface{}{
						"format": "json",
						"jsonmap": map[string]interface{}{
							"items_pointer":       "/items",
							"next_cursor_pointer": "/next_cursor",
							"item_id":             "/id",
							"thread_key":          "/thread",
							"sender":              "/from",
							"subject_or_room":     "/subject",
							"is_automated":        "/auto",
							"has_schedule":        "/sched",
						},
					},
					"read_quarantined": map[string]interface{}{
						"format": "json",
						"jsonmap": map[string]interface{}{
							"items_pointer":   "/items",
							"item_id":         "/id",
							"thread_key":      "/thread",
							"sender":          "/from",
							"subject_or_room": "/subject",
							"body":            "/subject",
						},
					},
				},
			},
		},
		"capabilities": map[string]interface{}{
			"poll":             map[string]interface{}{"enabled": true, "max_pages": 2, "page_size": 25},
			"read_quarantined": map[string]interface{}{"enabled": true},
		},
	}
	rawM, _ := json.MarshalIndent(manifest, "", "  ")
	mPath := filepath.Join(connDir, "connector.json")
	if err := os.WriteFile(mPath, rawM, 0o644); err != nil {
		t.Fatalf("write connector.json: %v", err)
	}

	// Pin both connector.json and scriptPath in private/trust.json
	bundle, err := extension.BuildTrustBundle(vaultDir, "vault", "")
	if err != nil {
		t.Fatalf("BuildTrustBundle: %v", err)
	}
	bBytes, _ := json.Marshal(bundle)
	if err := extension.ApplyTrustBundle(vaultDir, bBytes); err != nil {
		t.Fatalf("ApplyTrustBundle: %v", err)
	}
	return scriptPath
}

func TestWatcherPrecheckReadAckAndPoisonLifecycle(t *testing.T) {
	vaultDir := t.TempDir()
	setupMockVaultConnector(t, vaultDir, "corp-mail", false)

	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	sum, err := watcher.RunPrecheck(context.Background(), vaultDir, watcher.PrecheckOptions{Now: now})
	if err != nil {
		t.Fatalf("RunPrecheck failed: %v", err)
	}
	if sum.EnqueuedTotal != 1 || sum.RollupTotal != 1 {
		t.Fatalf("expected EnqueuedTotal=1 and RollupTotal=1, got %+v", sum)
	}

	// Verify automated item went straight to brief_rollup and actionable item went to events.json with ZERO untrusted text
	obItems, err := outbox.LoadAll(vaultDir)
	if err != nil || len(obItems) != 1 {
		t.Fatalf("expected 1 outbox automated rollup, got %d (%v)", len(obItems), err)
	}
	if !obItems[0].Tainted || obItems[0].Tier != outbox.TierBriefRollup {
		t.Fatalf("expected tainted brief_rollup item, got %+v", obItems[0])
	}

	evList, err := events.List(vaultDir, "pending", false, 0)
	if err != nil || len(evList) != 1 {
		t.Fatalf("expected 1 pending watcher event, got %d (%v)", len(evList), err)
	}
	if strings.Contains(evList[0].Prompt, "alice@example.com") || strings.Contains(evList[0].Prompt, "Architecture review") {
		t.Fatalf("prompt leaked untrusted sender/subject: %s", evList[0].Prompt)
	}
	if evList[0].Metadata["tier"] != "routine" || evList[0].Metadata["requires_ack"] != "1" {
		t.Fatalf("unexpected event metadata: %+v", evList[0].Metadata)
	}

	// Lease the batch and bind via spawn token inside ReadItem
	batches, _, err := events.LeaseNextBatches(vaultDir, now, events.DefaultLeaseTTL)
	if err != nil || len(batches) != 1 {
		t.Fatalf("LeaseNextBatches failed: %v (%d)", err, len(batches))
	}
	convID := "conv-watcher-worker-1"

	rendered, _, err := watcher.ReadItem(context.Background(), vaultDir, "corp-mail", "default", "th-100", batches[0].SpawnToken, convID)
	if err != nil {
		t.Fatalf("ReadItem failed: %v", err)
	}
	if _, ok := taint.Check(vaultDir, convID); !ok {
		t.Fatalf("expected conversation %s to be marked tainted before/during ReadItem", convID)
	}
	if strings.Contains(rendered, "<script>") || strings.Contains(rendered, "<<<HQ_FAKE") {
		t.Fatalf("expected script block and forged fence marker stripped, got:\n%s", rendered)
	}
	if !strings.Contains(rendered, "<<<HQ_UNTRUSTED_NONCE_") {
		t.Fatalf("expected cryptographic nonce fence in rendered output:\n%s", rendered)
	}

	// Attempt out-of-scope read from tainted worker -> must fail closed!
	if _, _, scopeErr := watcher.ReadItem(context.Background(), vaultDir, "corp-mail", "default", "th-999-foreign", "", convID); scopeErr == nil {
		t.Fatalf("expected out-of-scope ReadItem to fail closed under taint")
	}

	// Unacknowledged Stop requeues the item
	_, requeued, _, err := events.FinalizeByConversation(vaultDir, convID, now.Add(time.Minute))
	if err != nil || len(requeued) != 1 {
		t.Fatalf("expected unacked item to be requeued on Stop, got %d (%v)", len(requeued), err)
	}

	// Re-lease, Ack, and verify FinalizeByConversation archives as done
	batches2, _, err := events.LeaseNextBatches(vaultDir, now.Add(5*time.Minute), events.DefaultLeaseTTL)
	if err != nil || len(batches2) != 1 {
		t.Fatalf("re-lease failed: %v (%d)", err, len(batches2))
	}
	convID2 := "conv-watcher-worker-2"
	_, _, _ = events.BindBatchByToken(vaultDir, batches2[0].SpawnToken, convID2, now.Add(5*time.Minute), events.DefaultLeaseTTL)
	acked, err := watcher.Ack(context.Background(), vaultDir, []string{"msg-101"}, convID2, now.Add(6*time.Minute))
	if err != nil || acked != 1 {
		t.Fatalf("watcher.Ack failed: acked=%d err=%v", acked, err)
	}
	completed, req2, dropped2, err := events.FinalizeByConversation(vaultDir, convID2, now.Add(7*time.Minute))
	if err != nil || len(completed) != 1 || len(req2) != 0 || len(dropped2) != 0 {
		t.Fatalf("expected acked item to finalize as completed, got comp=%d req=%d drop=%d err=%v", len(completed), len(req2), len(dropped2), err)
	}
}

func TestDesktopSessionConnectorPausesCleanlyWhenIdle(t *testing.T) {
	vaultDir := t.TempDir()
	setupMockVaultConnector(t, vaultDir, "desktop-chat", true)
	t.Setenv("ANTIGRAVITY_CREDENTIAL_SCOPE", "")

	sum, err := watcher.RunPrecheck(context.Background(), vaultDir, watcher.PrecheckOptions{})
	if err != nil {
		t.Fatalf("RunPrecheck failed: %v", err)
	}
	if len(sum.Results) != 1 || sum.Results[0].Status != "paused (no active desktop conversation)" {
		t.Fatalf("expected paused (no active desktop conversation), got %+v", sum.Results)
	}
	// Must NOT spam outbox with auth_expired alerts when simply idle!
	obItems, _ := outbox.LoadAll(vaultDir)
	if len(obItems) != 0 {
		t.Fatalf("expected 0 outbox error spam when desktop session is idle, got %+v", obItems)
	}
}
