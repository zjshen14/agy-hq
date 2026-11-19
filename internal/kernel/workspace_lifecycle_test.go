package kernel

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

func TestAcquireWorkspaceOwnershipBindingAndLease(t *testing.T) {
	vaultDir := newWorkspaceTestVault(t)
	fs := newFakeWS(t, vaultDir)
	registerLiveTask(t, vaultDir, "ledger-fix", "task")

	acq, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{
		Provider: fakeWSProviderID, Name: "ledger-fix", TaskSlug: "ledger-fix", Role: workspace.RolePrimary, LeaseEnforced: true,
	})
	if err != nil {
		t.Fatalf("AcquireWorkspace: %v", err)
	}
	e := acq.Entry
	if !acq.Created || acq.LedgerExisted || acq.ProviderExisted || e.Ownership != workspace.OwnershipOwned || e.CreatedByTask != "ledger-fix" {
		t.Fatalf("expected an owned, newly created entry: %+v", acq)
	}
	if e.WriterTask != "ledger-fix" || !e.LeaseEnforced || len(e.BoundTasks) != 1 || e.BoundTasks[0] != "ledger-fix" {
		t.Fatalf("expected writer lease and binding for ledger-fix: %+v", e)
	}
	if e.Status.State != workspace.StateClean || e.Status.RefreshedAt == "" || e.CreatedAt == "" {
		t.Fatalf("expected a fresh status baseline: %+v", e.Status)
	}
	if acq.Binding.Key != fakeKey("ledger-fix") || acq.Binding.Role != workspace.RolePrimary || acq.Binding.WorkDir != filepath.Join(fs.rootBase, "ledger-fix", "src") {
		t.Fatalf("unexpected binding: %+v", acq.Binding)
	}
	if acq.Skill != "fake-workspace-guide" {
		t.Fatalf("expected provider skill, got %q", acq.Skill)
	}
	if got := fs.callsWith("create:"); len(got) != 1 || got[0] != "create:ledger-fix:create:core/fake-test:ledger-fix" {
		t.Fatalf("expected one create with the slug name and idempotency key, got %v", got)
	}

	// A workspace the provider reports as pre-existing is ledgered as attached, never owned.
	fs.preexisting["legacy-ws"] = true
	acq2, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "legacy-ws"})
	if err != nil || acq2.Created || acq2.Entry.Ownership != workspace.OwnershipAttached || acq2.Entry.CreatedByTask != "" {
		t.Fatalf("expected attached entry for existed=true, got %+v err=%v", acq2, err)
	}

	// Attaching by id records attached and the task takes the lease.
	fs.set("shared", workspace.StateDirty, inReview("c-1", false))
	registerLiveTask(t, vaultDir, "writer-a", "task")
	acq3, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "shared", TaskSlug: "writer-a", LeaseEnforced: true})
	if err != nil || acq3.Entry.Ownership != workspace.OwnershipAttached || acq3.Entry.WriterTask != "writer-a" || acq3.Entry.Status.State != workspace.StateDirty {
		t.Fatalf("unexpected attach result: %+v err=%v", acq3, err)
	}

	// A second live task cannot silently take an enforced lease.
	registerLiveTask(t, vaultDir, "writer-b", "task")
	fs.resetCalls()
	_, err = AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "shared", TaskSlug: "writer-b", LeaseEnforced: true})
	if workspace.ErrorCode(err) != workspace.CodeConflict || !strings.Contains(err.Error(), "hq workspace handoff") {
		t.Fatalf("expected lease conflict pointing at handoff, got %v", err)
	}
	if calls := fs.callsWith("status:"); len(calls) != 0 {
		t.Fatalf("lease conflict must be refused before calling the provider, got %v", calls)
	}
	// A readonly binding does not take the lease, so it is allowed.
	if _, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "shared", TaskSlug: "writer-b", Role: workspace.RoleReadonly}); err != nil {
		t.Fatalf("readonly attach should not need the lease: %v", err)
	}
	if got := mustEntry(t, vaultDir, fakeKey("shared")); got.WriterTask != "writer-a" || len(got.BoundTasks) != 2 {
		t.Fatalf("readonly attach must keep the writer: %+v", got)
	}

	for name, opts := range map[string]WorkspaceAcquireOptions{
		"git provider":    {Provider: workspace.CoreGitProviderID, Name: "x"},
		"free text name":  {Provider: fakeWSProviderID, Name: "Fix The Bug"},
		"unknown":         {Provider: "acme-monorepo/missing", Name: "x"},
		"bad role":        {Provider: fakeWSProviderID, Name: "x", Role: "owner"},
		"missing attach":  {Provider: fakeWSProviderID, WorkspaceID: "nope"},
		"bad attach id":   {Provider: fakeWSProviderID, WorkspaceID: "../etc"},
		"create disabled": {Provider: fakeWSProviderID, Name: "blocked"},
	} {
		if name == "create disabled" {
			fs.noCreate = true
		}
		if _, err := AcquireWorkspace(vaultDir, opts); err == nil {
			t.Fatalf("%s: expected an error", name)
		}
		fs.noCreate = false
	}
}

func TestAcquireWorkspaceShareFromSibling(t *testing.T) {
	vaultDir := newWorkspaceTestVault(t)
	fs := newFakeWS(t, vaultDir)
	src := acquireForTask(t, vaultDir, "main-line")
	registerLiveTask(t, vaultDir, "side-quest", "task")

	// Ledger key form: a new owned sibling for the new task, with the source id forwarded to the provider.
	sib, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{
		Provider: fakeWSProviderID, Name: "side-quest", ShareFrom: src.Entry.Key(), TaskSlug: "side-quest", LeaseEnforced: true,
	})
	if err != nil {
		t.Fatalf("share_from create: %v", err)
	}
	if got := fs.callsWith("share:"); len(got) != 1 || got[0] != "share:side-quest<-main-line" {
		t.Fatalf("expected share_from=main-line forwarded to the provider, got %v", got)
	}
	if !sib.Created || sib.Entry.Ownership != workspace.OwnershipOwned || sib.Entry.WriterTask != "side-quest" || !sib.Entry.LeaseEnforced {
		t.Fatalf("expected an owned, leased sibling: %+v", sib)
	}
	if e := mustEntry(t, vaultDir, src.Entry.Key()); e.WriterTask != "main-line" {
		t.Fatalf("the source lease must be untouched: %+v", e)
	}
	// Bare workspace id form, and an attached source, are both accepted at the core level.
	fs.set("borrowed", workspace.StateClean)
	if _, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "borrowed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "sib-two", ShareFrom: "borrowed"}); err != nil {
		t.Fatalf("bare id share_from: %v", err)
	}

	fs.resetCalls()
	refusals := map[string]struct {
		opts WorkspaceAcquireOptions
		code string
	}{
		"unknown source":   {WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "x1", ShareFrom: "never-ledgered"}, workspace.CodeNotFound},
		"other provider":   {WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "x2", ShareFrom: "acme-monorepo/client:main-line"}, workspace.CodeInvalid},
		"malformed source": {WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "x3", ShareFrom: "../etc"}, workspace.CodeInvalid},
		"with attach id":   {WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "borrowed", ShareFrom: "main-line"}, workspace.CodeInvalid},
	}
	for name, tc := range refusals {
		if _, err := AcquireWorkspace(vaultDir, tc.opts); workspace.ErrorCode(err) != tc.code {
			t.Fatalf("%s: got %v, want %s", name, err, tc.code)
		}
	}
	fs.noShared = true
	if _, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "x4", ShareFrom: "main-line"}); workspace.ErrorCode(err) != workspace.CodeUnsupported || !strings.Contains(err.Error(), "shared_history") {
		t.Fatalf("expected an unsupported shared_history refusal, got %v", err)
	}
	if got := fs.callsWith("create:"); len(got) != 0 {
		t.Fatalf("refused share_from requests must never reach the provider: %v", got)
	}
}

func TestAcquireWorkspaceRefusesRootsOverlappingTheVault(t *testing.T) {
	vaultDir := newWorkspaceTestVault(t)
	fs := newFakeWS(t, vaultDir)
	fs.rootBase = filepath.Join(vaultDir, "notes")
	if _, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "inside"}); workspace.ErrorCode(err) != workspace.CodeConflict {
		t.Fatalf("expected a conflict for a workspace root inside the Vault, got %v", err)
	}
	if l, _ := workspace.LoadLedger(vaultDir); len(l.Entries) != 0 {
		t.Fatalf("nothing may be ledgered: %+v", l.Entries)
	}
}

func TestRollbackAcquiredWorkspace(t *testing.T) {
	vaultDir := newWorkspaceTestVault(t)
	fs := newFakeWS(t, vaultDir)

	// Created by this call: removed with both preconditions and dropped from the ledger.
	acq, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "spawned", TaskSlug: "spawned", LeaseEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := RollbackAcquiredWorkspace(vaultDir, acq, "spawned"); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if got := fs.callsWith("remove:"); len(got) != 1 || got[0] != "remove:spawned:clean=true:open=true" {
		t.Fatalf("expected one guarded remove, got %v", got)
	}
	if _, ok := workspace.GetEntry(vaultDir, fakeKey("spawned")); ok || fs.exists("spawned") {
		t.Fatal("created workspace must be removed and dropped from the ledger")
	}

	// Pre-existing workspace attached by this call: only the ledger entry is dropped.
	fs.set("existing", workspace.StateClean)
	fs.resetCalls()
	acq, err = AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "existing", TaskSlug: "t1", LeaseEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := RollbackAcquiredWorkspace(vaultDir, acq, "t1"); err != nil {
		t.Fatal(err)
	}
	if len(fs.callsWith("remove:")) != 0 || !fs.exists("existing") {
		t.Fatal("an attached workspace must never be removed by a rollback")
	}
	if _, ok := workspace.GetEntry(vaultDir, fakeKey("existing")); ok {
		t.Fatal("the attached entry created by the failed spawn must be dropped")
	}

	// Existing ledger entry: the previous writer and binders are restored.
	acq, err = AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "existing", TaskSlug: "first", LeaseEnforced: false})
	if err != nil {
		t.Fatal(err)
	}
	acq2, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "existing", TaskSlug: "second", LeaseEnforced: true})
	if err != nil || !acq2.LedgerExisted || acq2.PrevWriterTask != "first" {
		t.Fatalf("expected reuse of the ledger entry: %+v err=%v", acq2, err)
	}
	if err := RollbackAcquiredWorkspace(vaultDir, acq2, "second"); err != nil {
		t.Fatal(err)
	}
	if e := mustEntry(t, vaultDir, fakeKey("existing")); e.WriterTask != "first" || e.LeaseEnforced || len(e.BoundTasks) != 1 || e.BoundTasks[0] != "first" {
		t.Fatalf("expected previous writer restored, got %+v", e)
	}
	_ = acq

	// A provider refusal keeps the created workspace as an unbound owned orphan for prune.
	acq, err = AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "sticky", TaskSlug: "sticky", LeaseEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	fs.removeErr["sticky"] = &workspace.ProviderError{Code: workspace.CodeUnavailable, Message: "backend offline"}
	if err := RollbackAcquiredWorkspace(vaultDir, acq, "sticky"); err == nil {
		t.Fatal("expected the rollback to report the provider failure")
	}
	if e := mustEntry(t, vaultDir, fakeKey("sticky")); e.Ownership != workspace.OwnershipOwned || len(e.BoundTasks) != 0 || e.WriterTask != "" {
		t.Fatalf("expected an unbound owned orphan, got %+v", e)
	}
}

func TestHandoffUnbindOwnAndRemoveWorkspace(t *testing.T) {
	vaultDir := newWorkspaceTestVault(t)
	fs := newFakeWS(t, vaultDir)
	acquireForTask(t, vaultDir, "owner")
	registerLiveTask(t, vaultDir, "helper", "task")
	key := fakeKey("owner")

	slug, err := HandoffWorkspace(vaultDir, key, "conv-helper")
	if err != nil || slug != "helper" {
		t.Fatalf("handoff: slug=%q err=%v", slug, err)
	}
	if e := mustEntry(t, vaultDir, key); e.WriterTask != "helper" || !e.LeaseEnforced || len(e.BoundTasks) != 2 {
		t.Fatalf("expected the lease to move to helper: %+v", e)
	}
	if b, ok := taskHasWorkspaceBinding(mustTask(t, vaultDir, "helper"), key); !ok || b.Role != workspace.RolePrimary {
		t.Fatalf("expected helper to be bound (primary: it had none), got %+v ok=%v", b, ok)
	}
	if _, err := HandoffWorkspace(vaultDir, key, "no-such-task"); err == nil {
		t.Fatal("handoff to an unknown task must fail")
	}

	// Removal refuses workspaces still bound to live tasks.
	if _, err := RemoveWorkspace(vaultDir, key, false, time.Now()); workspace.ErrorCode(err) != workspace.CodeConflict {
		t.Fatalf("expected a live-binding conflict, got %v", err)
	}
	for _, s := range []string{"helper", "owner"} {
		if _, err := UnbindWorkspace(vaultDir, key, s); err != nil {
			t.Fatalf("unbind %s: %v", s, err)
		}
	}
	if e := mustEntry(t, vaultDir, key); len(e.BoundTasks) != 0 || e.WriterTask != "" || e.LeaseEnforced {
		t.Fatalf("expected no binders and no lease after unbinding: %+v", e)
	}
	if _, err := UnbindWorkspace(vaultDir, key, "owner"); workspace.ErrorCode(err) != workspace.CodeNotFound {
		t.Fatalf("unbinding twice must report not bound, got %v", err)
	}

	// Open changes need approval; the forced form removes regardless.
	fs.set("owner", workspace.StateClean, inReview("c-7", true))
	res, err := RemoveWorkspace(vaultDir, key, false, time.Now())
	if err != nil || !res.NeedsApproval || res.Removed || !strings.Contains(res.Reason, "open change") {
		t.Fatalf("expected NeedsApproval for open changes: %+v err=%v", res, err)
	}
	if len(fs.callsWith("remove:")) != 0 {
		t.Fatal("no provider remove may run before approval")
	}
	proposal := WorkspaceRemoveProposal("./bin/hq", key, res.Reason)
	if !strings.Contains(proposal, "./bin/hq propose --type=workspace_remove") || !strings.Contains(proposal, "--cmd='hq workspace remove "+key+" --force'") {
		t.Fatalf("unexpected proposal command: %s", proposal)
	}
	res, err = RemoveWorkspace(vaultDir, key, true, time.Now())
	if err != nil || !res.Removed {
		t.Fatalf("forced remove: %+v err=%v", res, err)
	}
	if got := fs.callsWith("remove:"); len(got) != 1 || got[0] != "remove:owner:clean=false:open=false" {
		t.Fatalf("expected an unconditional provider remove, got %v", got)
	}
	if _, ok := workspace.GetEntry(vaultDir, key); ok {
		t.Fatal("removed workspace must leave the ledger")
	}

	// Attached workspaces are never removed, even with force, until promoted with own.
	fs.set("borrowed", workspace.StateClean)
	if _, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "borrowed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveWorkspace(vaultDir, fakeKey("borrowed"), true, time.Now()); workspace.ErrorCode(err) != workspace.CodeConflict {
		t.Fatalf("attached remove must be refused, got %v", err)
	}
	if e, changed, err := OwnWorkspace(vaultDir, fakeKey("borrowed")); err != nil || !changed || e.Ownership != workspace.OwnershipOwned {
		t.Fatalf("own: %+v changed=%v err=%v", e, changed, err)
	}
	if _, changed, _ := OwnWorkspace(vaultDir, fakeKey("borrowed")); changed {
		t.Fatal("owning an owned workspace is a no-op")
	}
	res, err = RemoveWorkspace(vaultDir, fakeKey("borrowed"), false, time.Now())
	if err != nil || !res.Removed {
		t.Fatalf("clean owned workspace should be removable without approval: %+v err=%v", res, err)
	}

	// A workspace that is already gone is forgotten.
	fs.set("ghost", workspace.StateClean)
	if _, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, Name: "ghost-x"}); err != nil {
		t.Fatal(err)
	}
	fs.mu.Lock()
	delete(fs.workspaces, "ghost-x")
	fs.mu.Unlock()
	res, err = RemoveWorkspace(vaultDir, fakeKey("ghost-x"), false, time.Now())
	if err != nil || !res.Forgotten {
		t.Fatalf("expected a missing workspace to be forgotten: %+v err=%v", res, err)
	}
}

func TestRefreshWorkspaceStatusCASErrorsAndLandedNotices(t *testing.T) {
	vaultDir := newWorkspaceTestVault(t)
	fs := newFakeWS(t, vaultDir)
	fs.set("ledger-fix", workspace.StateDirty, inReview("c-1", false), inReview("c-2", true))
	acq, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "ledger-fix", TaskSlug: "ledger-fix", LeaseEnforced: true})
	if err != nil {
		t.Fatal(err)
	}
	key := acq.Entry.Key()
	base := acq.Entry.Status.RefreshedAt

	// Compare-and-set: a result computed against a stale refreshed_at is dropped.
	stale := fs.info("ledger-fix", workspace.StateClean)
	if _, applied, err := applyWorkspaceStatusCAS(vaultDir, key, "1999-01-01T00:00:00Z", stale, nil, time.Now()); err != nil || applied {
		t.Fatalf("stale CAS must not apply (applied=%v err=%v)", applied, err)
	}
	if e := mustEntry(t, vaultDir, key); e.Status.State != workspace.StateDirty || e.Status.RefreshedAt != base {
		t.Fatalf("cache must be unchanged after a lost CAS: %+v", e.Status)
	}

	// Provider errors are recorded without discarding the last good state.
	fs.statusErr["ledger-fix"] = errors.New("backend\noffline")
	now := time.Now().Add(time.Minute)
	if _, err := RefreshWorkspaceStatus(vaultDir, key, now); err == nil {
		t.Fatal("expected the provider error to be returned")
	}
	e := mustEntry(t, vaultDir, key)
	if e.Status.Error == "" || strings.Contains(e.Status.Error, "\n") || e.Status.State != workspace.StateDirty || len(e.Status.Changes) != 2 {
		t.Fatalf("expected an errored cache that keeps the last state: %+v", e.Status)
	}
	delete(fs.statusErr, "ledger-fix")

	// c-1 lands: exactly one brief_rollup outbox item, deduplicated across refreshes.
	fs.set("ledger-fix", workspace.StateClean, landed("c-1"), inReview("c-2", true))
	if _, err := RefreshWorkspaceStatus(vaultDir, key, now.Add(time.Minute)); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if _, err := RefreshWorkspaceStatus(vaultDir, key, now.Add(2*time.Minute)); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	items, err := outbox.List(vaultDir, outbox.TierBriefRollup, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || !strings.Contains(items[0].Title, key) || !strings.Contains(items[0].Title, "c-1") || items[0].SourceSkill != WorkspaceLandedSourceSkill || items[0].Scope != "ledger-fix" {
		t.Fatalf("expected one landed notice for c-1, got %+v", items)
	}
	if !strings.Contains(items[0].Summary, "1 change(s) are still open") {
		t.Fatalf("unexpected landed summary: %q", items[0].Summary)
	}
	if e := mustEntry(t, vaultDir, key); len(e.NotifiedLanded) != 1 || e.NotifiedLanded[0] != "c-1" || e.Status.Error != "" {
		t.Fatalf("expected notified_landed=[c-1] and a cleared error: %+v", e)
	}

	// A change first observed as already landed in the baseline is not a transition.
	fs.set("old-ws", workspace.StateClean, landed("c-9"))
	acq2, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: "old-ws"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := RefreshWorkspaceStatus(vaultDir, acq2.Entry.Key(), now.Add(3*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if items, _ := outbox.List(vaultDir, outbox.TierBriefRollup, "", time.Now()); len(items) != 1 {
		t.Fatalf("baseline-landed changes must not notify, got %d items", len(items))
	}

	// not_found marks the workspace missing.
	fs.mu.Lock()
	delete(fs.workspaces, "old-ws")
	fs.mu.Unlock()
	if _, err := RefreshWorkspaceStatus(vaultDir, acq2.Entry.Key(), now.Add(4*time.Minute)); err != nil {
		t.Fatalf("not_found refresh: %v", err)
	}
	if e := mustEntry(t, vaultDir, acq2.Entry.Key()); e.Status.State != workspace.StateMissing {
		t.Fatalf("expected missing state, got %+v", e.Status)
	}
}

func TestWorkspaceCacheEligibilityFailsClosed(t *testing.T) {
	now := time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-5 * time.Minute).Format(time.RFC3339)
	base := workspace.LedgerEntry{Provider: fakeWSProviderID, WorkspaceID: "w", Root: "/srv/ws/u/w", Ownership: workspace.OwnershipOwned}
	cases := []struct {
		name   string
		status workspace.LedgerStatus
		secs   int
		want   bool
	}{
		{"clean landed fresh", workspace.LedgerStatus{State: workspace.StateClean, Changes: []workspace.ChangeRef{landed("c")}, RefreshedAt: fresh}, 600, true},
		{"no changes", workspace.LedgerStatus{State: workspace.StateClean, RefreshedAt: fresh}, 600, true},
		{"open change", workspace.LedgerStatus{State: workspace.StateClean, Changes: []workspace.ChangeRef{inReview("c", false)}, RefreshedAt: fresh}, 600, false},
		{"dirty", workspace.LedgerStatus{State: workspace.StateDirty, RefreshedAt: fresh}, 600, false},
		{"errored", workspace.LedgerStatus{State: workspace.StateClean, RefreshedAt: fresh, Error: "boom"}, 600, false},
		{"never refreshed", workspace.LedgerStatus{State: workspace.StateClean}, 600, false},
		{"stale vs 2x cadence", workspace.LedgerStatus{State: workspace.StateClean, RefreshedAt: now.Add(-21 * time.Minute).Format(time.RFC3339)}, 600, false},
		{"within 2x cadence", workspace.LedgerStatus{State: workspace.StateClean, RefreshedAt: now.Add(-19 * time.Minute).Format(time.RFC3339)}, 600, true},
		{"refresh disabled, older than 1h", workspace.LedgerStatus{State: workspace.StateClean, RefreshedAt: now.Add(-61 * time.Minute).Format(time.RFC3339)}, 0, false},
		{"refresh disabled, within 1h", workspace.LedgerStatus{State: workspace.StateClean, RefreshedAt: now.Add(-30 * time.Minute).Format(time.RFC3339)}, 0, true},
	}
	for _, tc := range cases {
		e := base
		e.Status = tc.status
		if got, why := WorkspaceDormantEligible(e, tc.secs, now); got != tc.want {
			t.Errorf("%s: eligible=%v (%s), want %v", tc.name, got, why, tc.want)
		}
	}
}

func TestSummarizeWorkspacesStatusLine(t *testing.T) {
	vaultDir := newWorkspaceTestVault(t)
	if _, ok := SummarizeWorkspaces(vaultDir, 3); ok {
		t.Fatal("an empty ledger must not produce a status line")
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "memory", "workspaces.json")); !os.IsNotExist(err) {
		t.Fatal("summarizing must not create the ledger")
	}
	fs := newFakeWS(t, vaultDir)
	fs.set("a", workspace.StateDirty, inReview("c-1", true), landed("c-2"))
	fs.set("b", workspace.StateClean, inReview("c-3", false))
	for _, id := range []string{"a", "b"} {
		if _, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{Provider: fakeWSProviderID, WorkspaceID: id}); err != nil {
			t.Fatal(err)
		}
	}
	s, ok := SummarizeWorkspaces(vaultDir, 2)
	if !ok {
		t.Fatal("expected a summary")
	}
	want := "Workspaces: 4 (git: 2, core/fake-test: 2) · open changes: 2 · needs you: 1"
	if got := s.StatusLine(); got != want {
		t.Fatalf("StatusLine = %q, want %q", got, want)
	}
}

func TestWorkspacePromptPreambleAndSpecParsing(t *testing.T) {
	got := WorkspacePromptPreamble("acme-monorepo/client", "ledger-fix", "primary", "/srv/ws/user/ledger-fix/src", "acme-workspace-guide")
	want := "[hq-workspace provider=\"acme-monorepo/client\" id=\"ledger-fix\" role=\"primary\"]\n" +
		"Work inside the isolated workspace at /srv/ws/user/ledger-fix/src.\n" +
		"Provider guide: load the `acme-workspace-guide` skill before running VCS commands."
	if got != want {
		t.Fatalf("preamble mismatch:\n%s\nwant:\n%s", got, want)
	}
	if strings.Contains(WorkspacePromptPreamble("p/x", "w", "", "/w", ""), "Provider guide") {
		t.Fatal("no skill line without a provider skill")
	}
	if p, id, err := ParseWorkspaceSpec("acme-monorepo/client:ledger-fix"); err != nil || p != "acme-monorepo/client" || id != "ledger-fix" {
		t.Fatalf("ParseWorkspaceSpec = %q %q %v", p, id, err)
	}
	if p, id, err := ParseWorkspaceSpec("acme-monorepo/client"); err != nil || p != "acme-monorepo/client" || id != "" {
		t.Fatalf("ParseWorkspaceSpec without id = %q %q %v", p, id, err)
	}
	for _, bad := range []string{"", "client", "a/b/c", "acme/client:../x", "acme/client:"} {
		if _, _, err := ParseWorkspaceSpec(bad); err == nil {
			t.Fatalf("ParseWorkspaceSpec(%q) should fail", bad)
		}
	}
}
