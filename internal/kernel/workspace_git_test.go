package kernel

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/workspace"
)

func TestGitWorktreeInfoMapping(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worktrees", "repo-ledger-fix")
	cases := []struct {
		name      string
		st        ManagedWorktreeStatus
		wantState workspace.State
		wantCS    workspace.ChangeState
		noChange  bool
	}{
		{"clean merged", ManagedWorktreeStatus{Clean: true, Branch: "hq/ledger-fix", MergedIntoMain: true, PushedToOrigin: true}, workspace.StateClean, workspace.ChangeLanded, false},
		{"dirty pushed", ManagedWorktreeStatus{Clean: false, Branch: "hq/ledger-fix", PushedToOrigin: true}, workspace.StateDirty, workspace.ChangeInReview, false},
		{"clean local", ManagedWorktreeStatus{Clean: true, Branch: "hq/ledger-fix"}, workspace.StateClean, workspace.ChangeLocal, false},
		{"detached head", ManagedWorktreeStatus{Clean: true}, workspace.StateClean, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.st.Name = "repo-ledger-fix"
			tc.st.Path = path
			tc.st.UpdatedAt = "2026-11-19T08:00:00Z"
			info := gitWorktreeInfo(tc.st)
			root := workspace.CanonicalPath(path)
			if info.Provider != workspace.CoreGitProviderID || info.WorkspaceID != root || info.Root != root || info.WorkDir != root || info.Name != "repo-ledger-fix" || info.Flavor != "git" {
				t.Fatalf("unexpected identity fields: %+v", info)
			}
			if info.State != tc.wantState {
				t.Fatalf("State = %q, want %q", info.State, tc.wantState)
			}
			if tc.noChange {
				if len(info.Changes) != 0 {
					t.Fatalf("expected no change for a detached head, got %+v", info.Changes)
				}
				return
			}
			if len(info.Changes) != 1 || info.Changes[0].ID != "branch:hq/ledger-fix" || info.Changes[0].State != tc.wantCS || info.Changes[0].UpdatedAt != tc.st.UpdatedAt {
				t.Fatalf("unexpected branch change: %+v", info.Changes)
			}
		})
	}
}

func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v (%s)", args, dir, err, string(out))
	}
	return strings.TrimSpace(string(out))
}

// testGitWorktreeAdder mirrors worktree.Add's naming (<vault>/worktrees/<repo>-<task>, branch hq/<task>) for
// local repositories; the kernel cannot import internal/worktree in its own tests.
func testGitWorktreeAdder(vaultDir, repoSpec, taskName, branch string) (string, error) {
	repoBase := filepath.Base(filepath.Clean(repoSpec))
	target := filepath.Join(vaultDir, "worktrees", repoBase+"-"+taskName)
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		return target, nil
	}
	if branch == "" {
		branch = "hq/" + taskName
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return "", err
	}
	if out, err := sanitizedGitCmd("-C", repoSpec, "worktree", "add", "-b", branch, target).CombinedOutput(); err != nil {
		return "", &workspace.ProviderError{Code: workspace.CodeUnavailable, Message: string(out)}
	}
	return target, nil
}

func TestGitWorktreeProviderLifecycle(t *testing.T) {
	vaultDir := t.TempDir()
	repo := filepath.Join(t.TempDir(), "repo")
	initTestGitRepo(t, repo)
	prev := RegisterGitWorktreeAdder(testGitWorktreeAdder)
	t.Cleanup(func() { RegisterGitWorktreeAdder(prev) })
	ctx := context.Background()

	p, err := workspace.Resolve(vaultDir, workspace.CoreGitProviderID)
	if err != nil {
		t.Fatalf("Resolve(core/git-worktree): %v", err)
	}
	if caps := p.Capabilities(); !caps.Create || !caps.Remove || !caps.Changes || !caps.SharedHistory {
		t.Fatalf("unexpected capabilities: %+v", caps)
	}

	if _, err := p.Create(ctx, workspace.CreateRequest{Name: "ledger-fix"}); workspace.ErrorCode(err) != workspace.CodeInvalid {
		t.Fatalf("Create without base = %v, want CodeInvalid", err)
	}
	if _, err := p.Create(ctx, workspace.CreateRequest{Name: "Bad Name", Base: repo}); workspace.ErrorCode(err) != workspace.CodeInvalid {
		t.Fatalf("Create(bad name) = %v, want CodeInvalid", err)
	}
	res, err := p.Create(ctx, workspace.CreateRequest{Name: "ledger-fix", Base: repo})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	wtDir := filepath.Join(vaultDir, "worktrees", "repo-ledger-fix")
	root := workspace.CanonicalPath(wtDir)
	if res.Existed || res.Info.WorkspaceID != root || res.Info.State != workspace.StateClean {
		t.Fatalf("unexpected create result: %+v", res)
	}
	if len(res.Info.Changes) != 1 || res.Info.Changes[0].ID != "branch:hq/ledger-fix" || res.Info.Changes[0].State != workspace.ChangeLanded {
		t.Fatalf("fresh branch at the default branch tip should read as landed: %+v", res.Info.Changes)
	}
	if again, err := p.Create(ctx, workspace.CreateRequest{Name: "ledger-fix", Base: repo}); err != nil || !again.Existed {
		t.Fatalf("idempotent Create = %+v, %v", again, err)
	}
	sibling, err := p.Create(ctx, workspace.CreateRequest{Name: "followup", ShareFrom: "repo-ledger-fix"})
	if err != nil || !strings.HasSuffix(sibling.Info.Name, "-followup") {
		t.Fatalf("Create(share_from) = %+v, %v", sibling, err)
	}

	// The canonical ledger key matches LedgerKey(provider, workspace ID).
	if gotKey := workspace.LedgerKey(workspace.CoreGitProviderID, workspace.CanonicalPath(wtDir)); gotKey != workspace.LedgerKey(workspace.CoreGitProviderID, res.Info.WorkspaceID) {
		t.Fatalf("ledger key %q does not match provider workspace ID %q", gotKey, res.Info.WorkspaceID)
	}

	commitFileInGitDir(t, wtDir, "feature.txt", "work\n", "feature work")
	st, err := p.Status(ctx, root)
	if err != nil || st.State != workspace.StateClean || st.Changes[0].State != workspace.ChangeLocal {
		t.Fatalf("Status after local commit = %+v, %v", st, err)
	}
	runGitT(t, repo, "update-ref", "refs/remotes/origin/hq/ledger-fix", runGitT(t, wtDir, "rev-parse", "HEAD"))
	st, err = p.Status(ctx, "repo-ledger-fix")
	if err != nil || st.Changes[0].State != workspace.ChangeInReview {
		t.Fatalf("Status after push = %+v, %v", st, err)
	}
	if err := os.WriteFile(filepath.Join(wtDir, "scratch.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	st, err = p.Status(ctx, root)
	if err != nil || st.State != workspace.StateDirty {
		t.Fatalf("Status with untracked file = %+v, %v", st, err)
	}

	info, owned, err := p.Probe(ctx, filepath.Join(wtDir, "sub", "new.go"))
	if err != nil || !owned || info.WorkspaceID != root {
		t.Fatalf("Probe(inside) = %+v, %v, %v", info, owned, err)
	}
	if _, owned, err := p.Probe(ctx, filepath.Join(vaultDir, "notes", "x.md")); err != nil || owned {
		t.Fatalf("Probe(outside) = %v, %v", owned, err)
	}

	list, next, err := p.List(ctx, workspace.ListRequest{Limit: 1})
	if err != nil || len(list) != 1 || next != "1" {
		t.Fatalf("List page 1 = %+v, %q, %v", list, next, err)
	}
	list2, next2, err := p.List(ctx, workspace.ListRequest{Cursor: next, Limit: 1})
	if err != nil || len(list2) != 1 || next2 != "" || list2[0].WorkspaceID == list[0].WorkspaceID {
		t.Fatalf("List page 2 = %+v, %q, %v", list2, next2, err)
	}
	if _, _, err := p.List(ctx, workspace.ListRequest{Cursor: "abc"}); workspace.ErrorCode(err) != workspace.CodeInvalid {
		t.Fatalf("List(bad cursor) = %v, want CodeInvalid", err)
	}

	outside := filepath.Join(t.TempDir(), "not-managed")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{outside, repo, "../escape"} {
		if err := p.Remove(ctx, workspace.RemoveRequest{WorkspaceID: id}); workspace.ErrorCode(err) != workspace.CodeInvalid {
			t.Fatalf("Remove(%q) = %v, want CodeInvalid", id, err)
		}
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("unmanaged directory was touched: %v", err)
	}
	if err := p.Remove(ctx, workspace.RemoveRequest{WorkspaceID: root, ExpectedRoot: outside}); workspace.ErrorCode(err) != workspace.CodeConflict {
		t.Fatalf("Remove(expected_root mismatch) = %v, want CodeConflict", err)
	}
	if err := p.Remove(ctx, workspace.RemoveRequest{WorkspaceID: root, RequireClean: true}); workspace.ErrorCode(err) != workspace.CodeDirty {
		t.Fatalf("Remove(dirty) = %v, want CodeDirty", err)
	}
	if err := os.Remove(filepath.Join(wtDir, "scratch.txt")); err != nil {
		t.Fatal(err)
	}
	if err := p.Remove(ctx, workspace.RemoveRequest{WorkspaceID: root, RequireClean: true, RequireNoOpenChanges: true}); workspace.ErrorCode(err) != workspace.CodeHasOpenChanges {
		t.Fatalf("Remove(unmerged) = %v, want CodeHasOpenChanges", err)
	}
	if err := p.Remove(ctx, workspace.RemoveRequest{WorkspaceID: root, ExpectedRoot: wtDir, RequireClean: true}); err != nil {
		t.Fatalf("Remove(clean) = %v", err)
	}
	if _, err := os.Stat(wtDir); !os.IsNotExist(err) {
		t.Fatalf("worktree still present after Remove: %v", err)
	}
	if st, err := p.Status(ctx, root); err != nil || st.State != workspace.StateMissing {
		t.Fatalf("Status after Remove = %+v, %v", st, err)
	}
	if err := p.Remove(ctx, workspace.RemoveRequest{WorkspaceID: root}); workspace.ErrorCode(err) != workspace.CodeNotFound {
		t.Fatalf("Remove(missing) = %v, want CodeNotFound", err)
	}
	if _, err := os.Stat(repo); err != nil {
		t.Fatalf("primary repository must never be removed: %v", err)
	}

	RegisterGitWorktreeAdder(nil)
	if p.Capabilities().Create {
		t.Fatal("Create capability should track adder registration")
	}
	if _, err := p.Create(ctx, workspace.CreateRequest{Name: "later", Base: repo}); workspace.ErrorCode(err) != workspace.CodeUnsupported {
		t.Fatalf("Create without adder = %v, want CodeUnsupported", err)
	}
	if _, err := (gitWorktreeProvider{}).Status(ctx, root); workspace.ErrorCode(err) != workspace.CodeUnavailable {
		t.Fatalf("unbound provider Status = %v, want CodeUnavailable", err)
	}
}

func TestRegisterTaskPreservesWorkspaceBindingsAcrossStopStyleRebuilds(t *testing.T) {
	vaultDir := t.TempDir()
	bindA := workspace.Binding{Key: "acme-monorepo/client:ledger-fix", Provider: "acme-monorepo/client", Root: "/srv/ws/user/ledger-fix", WorkDir: "/srv/ws/user/ledger-fix/src", Role: workspace.RolePrimary}
	bindB := workspace.Binding{Key: "acme-monorepo/client:shared", Provider: "acme-monorepo/client", Root: "/srv/ws/user/shared", Role: workspace.RoleReadonly}
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "ledger-fix",
		Title:          "[Task:acme] Ledger fix",
		Kind:           "task",
		ConversationID: "11111111-2222-3333-4444-555555555555",
		Status:         "active",
		Workspaces:     []workspace.Binding{bindA, bindB},
	}); err != nil {
		t.Fatalf("RegisterTask: %v", err)
	}

	// Stop-hook style rebuild: every known field copied by hand, Workspaces omitted.
	cur, ok := FindTaskByConversationID(vaultDir, "11111111-2222-3333-4444-555555555555")
	if !ok {
		t.Fatal("task not registered")
	}
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       cur.TaskSlug,
		Title:          cur.Title,
		Kind:           cur.EffectiveKind(),
		Scope:          cur.EffectiveScope(),
		Tier:           cur.Tier,
		Model:          cur.Model,
		WorktreeDir:    cur.WorktreeDir,
		WorktreeDirs:   cur.WorktreeDirs,
		RepoRoot:       cur.RepoRoot,
		ConversationID: cur.ConversationID,
		Status:         "active",
		TurnCount:      cur.TurnCount + 1,
		CreatedAt:      cur.CreatedAt,
		LastSummary:    "turn summary",
	}); err != nil {
		t.Fatalf("RegisterTask (rebuild): %v", err)
	}
	after, _ := FindTaskByConversationID(vaultDir, cur.ConversationID)
	if len(after.Workspaces) != 2 || after.Workspaces[0] != bindA || after.Workspaces[1] != bindB || after.TurnCount != cur.TurnCount+1 {
		t.Fatalf("Stop-style rebuild dropped workspace bindings: %+v", after)
	}

	// Incoming bindings win per key and new keys are appended.
	bindBRole := bindB
	bindBRole.Role = workspace.RoleSecondary
	bindC := workspace.Binding{Key: "acme-monorepo/client:extra", Provider: "acme-monorepo/client", Root: "/srv/ws/user/extra", Role: workspace.RoleSecondary}
	if err := RegisterTask(vaultDir, ActiveTask{TaskSlug: "ledger-fix", Workspaces: []workspace.Binding{bindBRole, bindC}}); err != nil {
		t.Fatalf("RegisterTask (merge): %v", err)
	}
	merged, _ := FindTaskByConversationID(vaultDir, cur.ConversationID)
	if len(merged.Workspaces) != 3 || merged.Workspaces[1].Role != workspace.RoleSecondary || merged.Workspaces[2] != bindC {
		t.Fatalf("unexpected merged bindings: %+v", merged.Workspaces)
	}

	// The pending-spawn link path (first Stop of a conversation with no slug) also unions bindings.
	if err := RegisterTask(vaultDir, ActiveTask{TaskSlug: "pending-task", Title: "[Task:acme] Pending", Kind: "task", Status: "active", Workspaces: []workspace.Binding{bindA}}); err != nil {
		t.Fatalf("RegisterTask (pending): %v", err)
	}
	if err := RegisterTask(vaultDir, ActiveTask{ConversationID: "99999999-8888-7777-6666-555555555555", Model: "m", Workspaces: []workspace.Binding{bindC}}); err != nil {
		t.Fatalf("RegisterTask (link): %v", err)
	}
	linked, ok := FindTaskByConversationID(vaultDir, "99999999-8888-7777-6666-555555555555")
	if !ok || linked.TaskSlug != "pending-task" || len(linked.Workspaces) != 2 || linked.Workspaces[0] != bindA || linked.Workspaces[1] != bindC {
		t.Fatalf("pending-spawn link lost or mis-merged bindings: %+v", linked)
	}
}
