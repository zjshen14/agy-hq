package authz

import (
	"context"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/taint"
)

func TestValidateProfileEnforcesNI1ThroughNI3(t *testing.T) {
	// NI-1: untrusted ingress + private read + auto egress MUST fail
	badNI1 := BuildScopedDelegate(taint.Record{
		ConvID: "c1",
		Source: "delegate",
		Grant: &taint.SessionGrant{
			GrantID:    "g1",
			Profile:    string(ProfileScopedDelegate),
			EgressMode: "auto_reply",
			Tasks:      []string{"billing-v2"},
		},
	})
	badNI1.Reads.InboxBoundBatch = true
	if err := ValidateProfile(badNI1); err == nil {
		t.Fatalf("expected NI-1 violation when combining InboxBoundBatch + EgressFilteredReply")
	}

	// NI-1 verb guard: scoped_delegate cannot hold `status` or `memory lookup`
	badVerb := BuildScopedDelegate(taint.Record{
		ConvID: "c2",
		Source: "delegate",
		Grant: &taint.SessionGrant{
			GrantID:        "g2",
			Profile:        string(ProfileScopedDelegate),
			AllowedHQVerbs: []string{"delegate read-request", "memory lookup"},
		},
	})
	if badVerb.Profile != ProfileFailClosed {
		t.Fatalf("expected BuildScopedDelegate with forbidden verb to clamp to ProfileFailClosed, got %s", badVerb.Profile)
	}

	// NI-2: untrusted ingress cannot hold WorkspaceEditsLeased
	badNI2 := BuildQuarantinedReader(taint.Record{ConvID: "c3", Source: "gmail"})
	badNI2.Writes.WorkspaceEditsLeased = true
	if err := ValidateProfile(badNI2); err == nil {
		t.Fatalf("expected NI-2 violation on workspace writes under untrusted ingress")
	}

	// NI-3: owner_auto cannot hold HumanPresenceApprove
	badNI3 := BuildUnattendedAuto()
	badNI3.Egress.HumanPresenceApprove = true
	if err := ValidateProfile(badNI3); err == nil {
		t.Fatalf("expected NI-3 violation on HumanPresenceApprove under owner_auto")
	}
}

func TestResolveAllEightProfilesAndExpirationClamp(t *testing.T) {
	vaultDir := t.TempDir()
	ctx := context.Background()
	now := time.Now().UTC()

	// 1. Untainted profiles
	if got := ResolveAt(vaultDir, "conv-top", ResolveHints{}, now); got.Profile != ProfileInteractiveTask || got.HostPreset != HostPresetAuto {
		t.Fatalf("unexpected interactive profile: %+v", got)
	}
	if got := ResolveAt(vaultDir, "conv-fd", ResolveHints{IsFrontDesk: true}, now); got.Profile != ProfileFrontDesk {
		t.Fatalf("unexpected front desk profile: %+v", got)
	}
	if got := ResolveAt(vaultDir, "conv-auto", ResolveHints{IsAutoWorker: true}, now); got.Profile != ProfileUnattendedAuto || got.HostPreset != HostPresetDefaultSandbox {
		t.Fatalf("unexpected auto profile: %+v", got)
	}
	if got := ResolveAt(vaultDir, "conv-sub", ResolveHints{IsSubagent: true}, now); got.Profile != ProfileSubagent {
		t.Fatalf("unexpected subagent profile: %+v", got)
	}

	// 2. Quarantined Reader (legacy v1)
	if _, err := taint.Mark(ctx, vaultDir, taint.Record{
		ConvID:    "conv-qr",
		Source:    "spawn_batch:connector:gmail",
		Connector: "gmail",
		ItemKeys:  []string{"gmail:thread:t1"},
	}); err != nil {
		t.Fatal(err)
	}
	qr := ResolveAt(vaultDir, "conv-qr", ResolveHints{}, now)
	if qr.Profile != ProfileQuarantinedReader || !qr.Tainted {
		t.Fatalf("expected quarantined_reader, got %+v", qr)
	}
	if err := qr.EvalHQArgv([]string{"hq", "status"}); err == nil {
		t.Fatalf("expected `hq status` to be denied in quarantined_reader")
	}
	if err := qr.EvalHQArgv([]string{"hq", "watch", "read", "--connector=gmail"}); err != nil {
		t.Fatalf("expected `hq watch read` allowed in quarantined_reader, got %v", err)
	}

	// 3. Scoped Delegate via BindSessionFromEvents
	g := taint.SessionGrant{
		GrantID:    "grant-sd-1",
		Profile:    string(ProfileScopedDelegate),
		CallerID:   "alice@example.com",
		PolicyID:   "workstream-status",
		RequestID:  "req-1",
		ExpiresAt:  now.Add(15 * time.Minute).Format(time.RFC3339),
		Tasks:      []string{"billing-v2"},
		EgressMode: "auto_reply",
	}
	if err := taint.StagePendingGrant(ctx, vaultDir, g); err != nil {
		t.Fatal(err)
	}
	if _, bound, err := BindSessionFromEvents(ctx, vaultDir, "conv-sd", []events.Event{{
		ID:           "evt-1",
		Source:       "delegate",
		TaintOnSpawn: true,
		Metadata:     map[string]string{"authz_grant_id": "grant-sd-1"},
	}}); err != nil || !bound {
		t.Fatalf("BindSessionFromEvents = (%v, %v)", bound, err)
	}
	sd := ResolveAt(vaultDir, "conv-sd", ResolveHints{}, now)
	if sd.Profile != ProfileScopedDelegate || !sd.Egress.EgressFilteredReply {
		t.Fatalf("expected scoped_delegate, got %+v", sd)
	}
	if err := sd.EvalHQArgv([]string{"hq", "delegate", "read-task", "billing-v2"}); err != nil {
		t.Fatalf("expected delegate read-task allowed, got %v", err)
	}
	if err := sd.EvalHQArgv([]string{"hq", "watch", "read"}); err == nil {
		t.Fatalf("NI-1: watch read MUST be denied in scoped_delegate")
	}
	if err := sd.EvalHQArgv([]string{"hq", "memory", "lookup", "alice"}); err == nil {
		t.Fatalf("NI-1: memory lookup MUST be denied in scoped_delegate")
	}

	// 4. Expired grant clamps to ProfileFailClosed (0 verbs!)
	expired := ResolveAt(vaultDir, "conv-sd", ResolveHints{}, now.Add(30*time.Minute))
	if expired.Profile != ProfileFailClosed {
		t.Fatalf("expected expired grant to clamp to ProfileFailClosed, got %s", expired.Profile)
	}
	if err := expired.EvalHQArgv([]string{"hq", "delegate", "read-request", "req-1"}); err == nil {
		t.Fatalf("expected fail_closed to deny all commands")
	}

	// 5. Missing pending grant on bind clamps to ProfileFailClosed (never upgrades to quarantined_reader)
	if _, bound, err := BindSessionFromEvents(ctx, vaultDir, "conv-missing", []events.Event{{
		ID:           "evt-2",
		Source:       "delegate",
		TaintOnSpawn: true,
		Metadata:     map[string]string{"authz_grant_id": "nonexistent-grant"},
	}}); err != nil || !bound {
		t.Fatalf("BindSessionFromEvents missing grant = (%v, %v)", bound, err)
	}
	if got := ResolveAt(vaultDir, "conv-missing", ResolveHints{}, now); got.Profile != ProfileFailClosed {
		t.Fatalf("expected missing pending grant to clamp to ProfileFailClosed, got %+v", got)
	}
}

func TestAttenuateMonotonicMeet(t *testing.T) {
	top := BuildInteractiveTask()
	qr := BuildQuarantinedReader(taint.Record{ConvID: "c1", Source: "gmail:thread:t1"})
	met := Attenuate(top, qr)
	if !met.Tainted || met.Profile != ProfileQuarantinedReader || met.Reads.HostFS || met.Writes.WorkspaceEditsLeased || met.Egress.HumanPresenceApprove {
		t.Fatalf("expected monotonic downward meet to strip interactive privileges, got %+v", met)
	}
}
