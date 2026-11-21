package taint

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestTaintV2GrantImmutabilityAndLedgerFailoverClamp(t *testing.T) {
	vaultDir := t.TempDir()
	ctx := context.Background()

	// 1. Stage + Consume pending grant
	g := SessionGrant{
		GrantID:        "grant-001",
		Profile:        "scoped_delegate",
		SourceFQID:     "transport:acme-work-pack/team-peer-bot",
		CallerID:       "alice@example.com",
		PolicyID:       "workstream-status",
		RequestID:      "req-123",
		Tasks:          []string{"billing-v2"},
		AllowedHQVerbs: []string{"delegate read-request", "delegate read-task", "delegate reply", "delegate escalate"},
		EgressMode:     "auto_reply",
	}
	if err := StagePendingGrant(ctx, vaultDir, g); err != nil {
		t.Fatalf("StagePendingGrant: %v", err)
	}
	got, ok, err := ConsumePendingGrant(ctx, vaultDir, "grant-001")
	if err != nil || !ok || got.PolicyID != "workstream-status" {
		t.Fatalf("ConsumePendingGrant = (%+v, %v, %v)", got, ok, err)
	}
	// Second consume returns false
	if _, ok2, err2 := ConsumePendingGrant(ctx, vaultDir, "grant-001"); err2 != nil || ok2 {
		t.Fatalf("expected consumed grant to be removed, got (%v, %v)", ok2, err2)
	}

	// 2. Bind v2 record with Grant
	if _, err := Mark(ctx, vaultDir, Record{
		ConvID: "conv-delegate-1",
		Source: "spawn_batch:delegate",
		Grant:  &g,
	}); err != nil {
		t.Fatalf("Mark v2: %v", err)
	}

	// 3. Write-once immutability: cannot overwrite Grant or merge inbox connector keys
	g2 := g
	g2.GrantID = "grant-evil"
	if _, err := Mark(ctx, vaultDir, Record{ConvID: "conv-delegate-1", Source: "spawn_batch:delegate", Grant: &g2}); err == nil {
		t.Fatalf("expected overwrite of immutable grant to fail")
	}
	if _, err := Mark(ctx, vaultDir, Record{ConvID: "conv-delegate-1", Source: "gmail:thread:t1", Connector: "gmail", ItemKeys: []string{"gmail:thread:t1"}}); err == nil {
		t.Fatalf("expected cross-profile inbox merge onto granted session to fail")
	}

	// 4. Reverse direction: cannot attach Grant onto existing quarantined_reader
	if _, err := Mark(ctx, vaultDir, Record{ConvID: "conv-qr-1", Source: "gmail:thread:t1", Connector: "gmail", ItemKeys: []string{"gmail:thread:t1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := Mark(ctx, vaultDir, Record{ConvID: "conv-qr-1", Source: "spawn_batch:delegate", Grant: &g}); err == nil {
		t.Fatalf("expected attaching grant onto existing quarantined_reader to fail")
	}

	// 5. P0-1 Ledger failover: deleting marker for v2 granted session MUST clamp to ProfileFailClosed (NOT quarantined_reader!)
	if err := os.Remove(markerPath(vaultDir, "conv-delegate-1")); err != nil {
		t.Fatal(err)
	}
	rec, tainted := Check(vaultDir, "conv-delegate-1")
	if !tainted || rec.Grant == nil || rec.Grant.Profile != ProfileFailClosed || !strings.HasPrefix(rec.Source, "fail_closed:") {
		t.Fatalf("expected v2 ledger failover to clamp to ProfileFailClosed, got %+v (tainted=%v)", rec, tainted)
	}

	// 6. Legacy v1 quarantined_reader still fails over cleanly with Grant == nil
	if err := os.Remove(markerPath(vaultDir, "conv-qr-1")); err != nil {
		t.Fatal(err)
	}
	recQR, taintedQR := Check(vaultDir, "conv-qr-1")
	if !taintedQR || recQR.Grant != nil || recQR.Connector != "gmail" {
		t.Fatalf("expected legacy v1 ledger failover to preserve quarantined_reader, got %+v", recQR)
	}
}
