package workspace

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zjshen14/agy-hq/internal/store"
)

func testLedgerEntry(provider, id, root string, own Ownership) LedgerEntry {
	return LedgerEntry{
		Provider:    provider,
		WorkspaceID: id,
		Name:        id,
		Root:        root,
		WorkDir:     root,
		Ownership:   own,
		Status:      LedgerStatus{State: StateClean},
	}
}

func TestLedgerKeyRoundTrip(t *testing.T) {
	key := LedgerKey("acme-monorepo/client", "ledger-fix")
	if key != "acme-monorepo/client:ledger-fix" {
		t.Fatalf("LedgerKey = %q", key)
	}
	p, id, ok := ParseLedgerKey(key)
	if !ok || p != "acme-monorepo/client" || id != "ledger-fix" {
		t.Fatalf("ParseLedgerKey(%q) = %q %q %v", key, p, id, ok)
	}
	p, id, ok = ParseLedgerKey(LedgerKey(CoreGitProviderID, "/srv/vault/worktrees/repo-task"))
	if !ok || p != CoreGitProviderID || id != "/srv/vault/worktrees/repo-task" {
		t.Fatalf("ParseLedgerKey(git key) = %q %q %v", p, id, ok)
	}
	for _, bad := range []string{"", "no-colon", ":id", "provider:"} {
		if _, _, ok := ParseLedgerKey(bad); ok {
			t.Fatalf("ParseLedgerKey(%q) should fail", bad)
		}
	}
}

func TestUpdateLedgerAtomicWritePermissionsAndNoopPaths(t *testing.T) {
	vaultDir := t.TempDir()
	l, err := LoadLedger(vaultDir)
	if err != nil || l.SchemaVersion != LedgerSchemaVersion || len(l.Entries) != 0 {
		t.Fatalf("LoadLedger(empty) = %+v, %v", l, err)
	}

	// changed=false never creates the file
	if err := UpdateLedger(vaultDir, func(*Ledger) (bool, error) { return false, nil }); err != nil {
		t.Fatalf("UpdateLedger noop: %v", err)
	}
	if _, err := os.Stat(ledgerPath(vaultDir)); !os.IsNotExist(err) {
		t.Fatalf("expected no ledger file after a no-op update, stat err=%v", err)
	}

	entry := testLedgerEntry("acme-monorepo/client", "ledger-fix", "/srv/ws/user/ledger-fix", OwnershipOwned)
	entry.WorkDir = "/srv/ws/user/ledger-fix/src"
	entry.CreatedByTask = "ledger-fix"
	entry.WriterTask = "ledger-fix"
	entry.LeaseEnforced = true
	entry.BoundTasks = []string{"ledger-fix"}
	entry.Status = LedgerStatus{State: StateDirty, Changes: []ChangeRef{{ID: "c-4821", State: ChangeInReview}}, RefreshedAt: "2026-11-19T08:40:00Z"}
	if err := UpdateLedger(vaultDir, func(l *Ledger) (bool, error) {
		l.Put(entry)
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateLedger put: %v", err)
	}
	st, err := os.Stat(ledgerPath(vaultDir))
	if err != nil {
		t.Fatalf("stat ledger: %v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("ledger mode = %v, want 0600", st.Mode().Perm())
	}
	leftovers, _ := filepath.Glob(filepath.Join(vaultDir, "memory", ".tmp-*"))
	if len(leftovers) != 0 {
		t.Fatalf("atomic write left temp files: %v", leftovers)
	}
	raw, _ := os.ReadFile(ledgerPath(vaultDir))
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("ledger is not valid JSON: %v", err)
	}
	for _, k := range []string{"schema_version", "entries"} {
		if _, ok := doc[k]; !ok {
			t.Fatalf("ledger JSON missing %q: %s", k, raw)
		}
	}
	for _, k := range []string{`"workspace_id"`, `"work_dir"`, `"ownership": "owned"`, `"created_by_task"`, `"writer_task"`, `"lease_enforced": true`, `"bound_tasks"`, `"refreshed_at"`} {
		if !strings.Contains(string(raw), k) {
			t.Fatalf("ledger JSON missing %s: %s", k, raw)
		}
	}
	got, ok := GetEntry(vaultDir, entry.Key())
	if !ok || got.WorkDir != entry.WorkDir || got.Status.State != StateDirty || len(got.Status.Changes) != 1 {
		t.Fatalf("GetEntry = %+v, %v", got, ok)
	}

	// fn errors and invariant violations leave the file untouched
	before, _ := os.ReadFile(ledgerPath(vaultDir))
	if err := UpdateLedger(vaultDir, func(l *Ledger) (bool, error) {
		delete(l.Entries, entry.Key())
		return true, fmt.Errorf("boom")
	}); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected fn error to propagate, got %v", err)
	}
	invalid := []func(*Ledger){
		func(l *Ledger) { l.Entries["wrong-key"] = entry },
		func(l *Ledger) {
			e := entry
			e.Ownership = "borrowed"
			l.Put(e)
		},
		func(l *Ledger) {
			e := entry
			e.Root, e.WorkDir = "/", ""
			l.Put(e)
		},
		func(l *Ledger) {
			e := entry
			e.WorkDir = "/srv/ws/user/elsewhere"
			l.Put(e)
		},
		func(l *Ledger) {
			e := entry
			e.Status.State = "exploded"
			l.Put(e)
		},
	}
	for i, mutate := range invalid {
		if err := UpdateLedger(vaultDir, func(l *Ledger) (bool, error) {
			mutate(l)
			return true, nil
		}); err == nil {
			t.Fatalf("invalid mutation %d unexpectedly persisted", i)
		}
	}
	after, _ := os.ReadFile(ledgerPath(vaultDir))
	if string(before) != string(after) {
		t.Fatal("ledger changed after failed updates")
	}
}

func TestLoadLedgerRejectsSymlinkCorruptAndNewerSchema(t *testing.T) {
	vaultDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "elsewhere.json")
	if err := os.WriteFile(target, []byte(`{"schema_version":1,"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, ledgerPath(vaultDir)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLedger(vaultDir); err == nil {
		t.Fatal("expected symlinked ledger to be refused")
	}
	if err := UpdateLedger(vaultDir, func(l *Ledger) (bool, error) { return true, nil }); err == nil {
		t.Fatal("expected UpdateLedger to refuse a symlinked ledger")
	}
	_ = os.Remove(ledgerPath(vaultDir))

	if err := os.WriteFile(ledgerPath(vaultDir), []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLedger(vaultDir); err == nil {
		t.Fatal("expected corrupt ledger to error")
	}
	if err := os.WriteFile(ledgerPath(vaultDir), []byte(`{"schema_version":2,"entries":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadLedger(vaultDir); err == nil {
		t.Fatal("expected newer schema to error")
	}
}

func TestUpdateLedgerConcurrentWritersSerialize(t *testing.T) {
	vaultDir := t.TempDir()
	const workers, rounds = 12, 5
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				err := UpdateLedger(vaultDir, func(l *Ledger) (bool, error) {
					shared := testLedgerEntry("acme-monorepo/client", "shared", "/srv/ws/user/shared", OwnershipAttached)
					if e, ok := l.Entries[shared.Key()]; ok {
						shared = e
					}
					shared.BoundTasks = append(shared.BoundTasks, fmt.Sprintf("t-%d-%d", w, r))
					l.Put(shared)
					own := testLedgerEntry("acme-monorepo/client", fmt.Sprintf("ws-%d", w), fmt.Sprintf("/srv/ws/user/ws-%d", w), OwnershipOwned)
					l.Put(own)
					return true, nil
				})
				if err != nil {
					t.Errorf("UpdateLedger: %v", err)
				}
			}
		}(w)
	}
	wg.Wait()
	l, err := LoadLedger(vaultDir)
	if err != nil {
		t.Fatalf("LoadLedger: %v", err)
	}
	if len(l.Entries) != workers+1 {
		t.Fatalf("entries = %d, want %d", len(l.Entries), workers+1)
	}
	if n := len(l.Entries[LedgerKey("acme-monorepo/client", "shared")].BoundTasks); n != workers*rounds {
		t.Fatalf("lost updates: %d bound tasks, want %d", n, workers*rounds)
	}
}

func TestUpdateLedgerLockRankOrdering(t *testing.T) {
	if !(store.RankTasks < store.RankWorkspaces && store.RankWorkspaces < store.RankMemory) {
		t.Fatalf("P2-14: expected RankTasks (%d) < RankWorkspaces (%d) < RankMemory (%d)", store.RankTasks, store.RankWorkspaces, store.RankMemory)
	}
	vaultDir := t.TempDir()
	// A task update (RankTasks) may nest a ledger update...
	err := store.With(context.Background(), store.RankTasks, filepath.Join(vaultDir, "memory", ".tasks.lock"), func(context.Context) error {
		return UpdateLedger(vaultDir, func(l *Ledger) (bool, error) {
			l.Put(testLedgerEntry("acme-monorepo/client", "nested", "/srv/ws/user/nested", OwnershipOwned))
			return true, nil
		})
	})
	if err != nil {
		t.Fatalf("expected RankTasks -> RankWorkspaces nesting to succeed, got %v", err)
	}
	// ...but never the reverse direction (a higher-ranked store must not nest the ledger).
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected RankSubagentQueue -> RankWorkspaces nesting to panic in tests")
		}
	}()
	_ = store.With(context.Background(), store.RankSubagentQueue, filepath.Join(vaultDir, "memory", ".queue.lock"), func(context.Context) error {
		return UpdateLedger(vaultDir, func(*Ledger) (bool, error) { return false, nil })
	})
}

func TestFindEntryForPathLongestSegmentAwarePrefix(t *testing.T) {
	vaultDir := t.TempDir()
	base := CanonicalPath(t.TempDir())
	realRoot := filepath.Join(base, "real", "ws-a")
	if err := os.MkdirAll(filepath.Join(realRoot, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link-to-a")
	if err := os.Symlink(realRoot, link); err != nil {
		t.Fatal(err)
	}
	if err := UpdateLedger(vaultDir, func(l *Ledger) (bool, error) {
		l.Put(testLedgerEntry("acme-monorepo/client", "outer", "/srv/ws/user", OwnershipAttached))
		l.Put(testLedgerEntry("acme-monorepo/client", "inner", "/srv/ws/user/ledger-fix", OwnershipOwned))
		l.Put(testLedgerEntry("acme-monorepo/client", "linked", realRoot, OwnershipOwned))
		return true, nil
	}); err != nil {
		t.Fatalf("UpdateLedger: %v", err)
	}
	cases := []struct {
		path   string
		wantID string
	}{
		{"/srv/ws/user/ledger-fix/src/billing/round.go", "inner"},
		{"/srv/ws/user/ledger-fix", "inner"},
		{"/srv/ws/user/ledger-fix2/main.go", "outer"},
		{"/srv/ws/user", "outer"},
		{filepath.Join(link, "src", "new_file.go"), "linked"},
		{"/srv/ws/username/x", ""},
		{"/srv/other", ""},
	}
	for _, tc := range cases {
		e, ok := FindEntryForPath(vaultDir, tc.path)
		if tc.wantID == "" {
			if ok {
				t.Fatalf("FindEntryForPath(%q) = %s, want no match", tc.path, e.WorkspaceID)
			}
			continue
		}
		if !ok || e.WorkspaceID != tc.wantID {
			t.Fatalf("FindEntryForPath(%q) = %q (%v), want %q", tc.path, e.WorkspaceID, ok, tc.wantID)
		}
	}
}

func TestProbeCandidatesDedupBoundsAndDrain(t *testing.T) {
	vaultDir := t.TempDir()
	if err := RecordProbeCandidate(vaultDir, "relative/path", "t1"); err == nil {
		t.Fatal("expected relative probe candidate to be rejected")
	}
	if err := RecordProbeCandidate(vaultDir, "/srv/ws/user/alpha", "task-a"); err != nil {
		t.Fatalf("RecordProbeCandidate: %v", err)
	}
	if err := RecordProbeCandidate(vaultDir, "/srv/ws/user/beta", "task-b"); err != nil {
		t.Fatalf("RecordProbeCandidate: %v", err)
	}
	// duplicate path + same task: no-op; duplicate path + new task: moves to the end with the new slug
	if err := RecordProbeCandidate(vaultDir, "/srv/ws/user/alpha/", "task-a"); err != nil {
		t.Fatalf("RecordProbeCandidate dup: %v", err)
	}
	l, _ := LoadLedger(vaultDir)
	if len(l.ProbeCandidates) != 2 || l.ProbeCandidates[0].Path != "/srv/ws/user/alpha" {
		t.Fatalf("unexpected candidates after duplicate: %+v", l.ProbeCandidates)
	}
	if err := RecordProbeCandidate(vaultDir, "/srv/ws/user/alpha", "task-c"); err != nil {
		t.Fatalf("RecordProbeCandidate retag: %v", err)
	}
	l, _ = LoadLedger(vaultDir)
	if len(l.ProbeCandidates) != 2 || l.ProbeCandidates[1].Path != "/srv/ws/user/alpha" || l.ProbeCandidates[1].TaskSlug != "task-c" || l.ProbeCandidates[0].RecordedAt == "" {
		t.Fatalf("unexpected candidates after retag: %+v", l.ProbeCandidates)
	}

	// already ledgered paths are never queued
	if err := UpdateLedger(vaultDir, func(l *Ledger) (bool, error) {
		l.Put(testLedgerEntry("acme-monorepo/client", "gamma", "/srv/ws/user/gamma", OwnershipAttached))
		return true, nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := RecordProbeCandidate(vaultDir, "/srv/ws/user/gamma/src", "task-g"); err != nil {
		t.Fatal(err)
	}
	l, _ = LoadLedger(vaultDir)
	for _, c := range l.ProbeCandidates {
		if strings.HasPrefix(c.Path, "/srv/ws/user/gamma") {
			t.Fatalf("ledgered path was queued: %+v", c)
		}
	}

	// bounded: oldest dropped
	for i := 0; i < MaxProbeCandidates+10; i++ {
		if err := RecordProbeCandidate(vaultDir, fmt.Sprintf("/srv/ws/bulk/ws-%03d", i), "bulk"); err != nil {
			t.Fatalf("RecordProbeCandidate bulk %d: %v", i, err)
		}
	}
	l, _ = LoadLedger(vaultDir)
	if len(l.ProbeCandidates) != MaxProbeCandidates {
		t.Fatalf("candidates = %d, want cap %d", len(l.ProbeCandidates), MaxProbeCandidates)
	}
	if l.ProbeCandidates[0].Path != "/srv/ws/bulk/ws-010" || l.ProbeCandidates[MaxProbeCandidates-1].Path != fmt.Sprintf("/srv/ws/bulk/ws-%03d", MaxProbeCandidates+9) {
		t.Fatalf("unexpected bounded window: first=%s last=%s", l.ProbeCandidates[0].Path, l.ProbeCandidates[MaxProbeCandidates-1].Path)
	}

	drained, err := DrainProbeCandidates(vaultDir, 3)
	if err != nil || len(drained) != 3 || drained[0].Path != "/srv/ws/bulk/ws-010" || drained[2].Path != "/srv/ws/bulk/ws-012" {
		t.Fatalf("DrainProbeCandidates(3) = %+v, %v", drained, err)
	}
	rest, err := DrainProbeCandidates(vaultDir, 0)
	if err != nil || len(rest) != MaxProbeCandidates-3 {
		t.Fatalf("DrainProbeCandidates(all) = %d, %v", len(rest), err)
	}
	none, err := DrainProbeCandidates(vaultDir, 5)
	if err != nil || len(none) != 0 {
		t.Fatalf("DrainProbeCandidates(empty) = %+v, %v", none, err)
	}
	l, _ = LoadLedger(vaultDir)
	if len(l.ProbeCandidates) != 0 || len(l.Entries) != 1 {
		t.Fatalf("unexpected ledger after drain: %+v", l)
	}
}

func TestCanonicalPathResolvesExistingAncestor(t *testing.T) {
	base := CanonicalPath(t.TempDir())
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if got := CanonicalPath(filepath.Join(link, "not", "yet", "created.txt")); got != filepath.Join(real, "not", "yet", "created.txt") {
		t.Fatalf("CanonicalPath through symlink = %q", got)
	}
	if got := CanonicalPath(""); got != "" {
		t.Fatalf("CanonicalPath(\"\") = %q", got)
	}
	if !pathWithin("/a/b/c", "/a/b") || pathWithin("/a/bc", "/a/b") || !pathWithin("/a/b", "/a/b") || !pathWithin("/x", "/") {
		t.Fatal("pathWithin segment semantics mismatch")
	}
}
