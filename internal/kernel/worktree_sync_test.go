package kernel

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func initTestGitRepo(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@agy-hq.local"},
		{"config", "user.name", "Test"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Base\n"), 0o644); err != nil {
		t.Fatalf("write README: %v", err)
	}
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "initial commit"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
}

func addTestWorktree(t *testing.T, repoDir, wtDir, branch string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(wtDir), 0o755); err != nil {
		t.Fatalf("mkdir parent %s: %v", wtDir, err)
	}
	cmd := exec.Command("git", "-C", repoDir, "worktree", "add", "-b", branch, wtDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add %s: %v (%s)", wtDir, err, string(out))
	}
}

func commitFileInGitDir(t *testing.T, dir, relPath, content, message string) {
	t.Helper()
	full := filepath.Join(dir, relPath)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", full, err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", full, err)
	}
	for _, args := range [][]string{
		{"add", relPath},
		{"commit", "-m", message},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v in %s: %v (%s)", args, dir, err, string(out))
		}
	}
}

func TestIsManagedWorktreeDirRing0Guard(t *testing.T) {
	vaultDir := t.TempDir()
	wtRoot := filepath.Join(vaultDir, "worktrees")
	validWT := filepath.Join(wtRoot, "repo-feature")
	_ = os.MkdirAll(validWT, 0o755)

	if !IsManagedWorktreeDir(vaultDir, validWT) {
		t.Fatalf("expected %s to be recognized as managed worktree dir", validWT)
	}
	for _, rejected := range []string{
		"",
		vaultDir,
		wtRoot,
		filepath.Dir(vaultDir),
		filepath.Join(vaultDir, "notes", "doc"),
		filepath.Join(validWT, "nested"),
	} {
		if IsManagedWorktreeDir(vaultDir, rejected) {
			t.Fatalf("expected %q to be rejected by IsManagedWorktreeDir", rejected)
		}
	}
}

func TestAttachWorktreeMultiRepoNestedAndArchiveCleanup(t *testing.T) {
	vaultDir := t.TempDir()
	repoA := filepath.Join(t.TempDir(), "repoA")
	repoB := filepath.Join(t.TempDir(), "repoB")
	initTestGitRepo(t, repoA)
	initTestGitRepo(t, repoB)

	wtA := filepath.Join(vaultDir, "worktrees", "repoA-oss-launch")
	wtB := filepath.Join(vaultDir, "worktrees", "repoB-companion-blog")
	addTestWorktree(t, repoA, wtA, "hq/oss-launch")
	addTestWorktree(t, repoB, wtB, "hq/companion-blog")

	fdConv := "00000000-0000-0000-0000-000000000100"
	taskConv := "00000000-0000-0000-0000-000000000101"
	nestedSubConv := "00000000-0000-0000-0000-000000000102"
	if err := SetFrontDesk(vaultDir, fdConv, ""); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "oss-launch",
		Title:          "[Task:oss] Open Source Launch",
		Kind:           "task",
		ConversationID: taskConv,
		WorktreeDir:    wtA,
		RepoRoot:       repoA,
		Status:         "completed",
		CompletedAt:    time.Now().Add(-2 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RegisterTask: %v", err)
	}
	if err := RecordNestedConversation(vaultDir, nestedSubConv, taskConv); err != nil {
		t.Fatalf("RecordNestedConversation: %v", err)
	}

	// 1. Front Desk conversation must never capture worktrees
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", fdConv)
	if _, ok := AttachWorktreeToTask(vaultDir, "", wtB, repoB); ok {
		t.Fatalf("expected Front Desk conversation to be ignored by AttachWorktreeToTask")
	}

	// 2. Nested child subagent under oss-launch attaches wtB to oss-launch and reactivates completed task
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", nestedSubConv)
	slug, ok := AttachWorktreeToTask(vaultDir, "", wtB, repoB)
	if !ok || slug != "oss-launch" {
		t.Fatalf("expected nested subagent to attach wtB to oss-launch, got slug=%q ok=%v", slug, ok)
	}

	// 3. Subsequent Stop-hook style RegisterTask update must preserve both primary wtA and secondary wtB
	if err := RegisterTask(vaultDir, ActiveTask{
		ConversationID: taskConv,
		TurnCount:      5,
		LastSummary:    "Updated companion blog post",
	}); err != nil {
		t.Fatalf("RegisterTask update: %v", err)
	}
	got, found := FindTaskByConversationID(vaultDir, taskConv)
	if !found {
		t.Fatalf("expected task %s to exist", taskConv)
	}
	if got.Status != "active" {
		t.Fatalf("expected completed task to reactivate to active when new worktree attached, got %q", got.Status)
	}
	allWTs := got.AllWorktreeDirs()
	if len(allWTs) != 2 || !sameCleanPath(allWTs[0], wtA) || !sameCleanPath(allWTs[1], wtB) {
		t.Fatalf("expected [wtA, wtB], got %v", allWTs)
	}

	// 4. Dirty secondary worktree wtB must block ArchiveTask atomically WITHOUT deleting clean primary wtA
	dirtyPath := filepath.Join(wtB, "uncommitted.md")
	if err := os.WriteFile(dirtyPath, []byte("draft"), 0o644); err != nil {
		t.Fatalf("write dirty file: %v", err)
	}
	if err := ArchiveTask(vaultDir, "oss-launch", false); err == nil {
		t.Fatalf("expected ArchiveTask to refuse when secondary worktree has uncommitted changes")
	}
	if _, err := os.Stat(wtA); err != nil {
		t.Fatalf("atomic check failed: primary worktree wtA was deleted when secondary wtB failed validation: %v", err)
	}

	// 5. Once wtB is clean, ArchiveTask removes BOTH cross-repository worktrees cleanly
	_ = os.Remove(dirtyPath)
	if err := ArchiveTask(vaultDir, "oss-launch", false); err != nil {
		t.Fatalf("ArchiveTask clean failed: %v", err)
	}
	for _, wt := range []string{wtA, wtB} {
		if _, err := os.Stat(wt); !os.IsNotExist(err) {
			t.Fatalf("expected worktree %s to be removed on archive, stat err=%v", wt, err)
		}
	}
}

func TestCompactTasksSubagentCleanupDormantUnmergedProtectionAndOrphanPrune(t *testing.T) {
	vaultDir := t.TempDir()
	repoA := filepath.Join(t.TempDir(), "repoA")
	repoB := filepath.Join(t.TempDir(), "repoB")
	initTestGitRepo(t, repoA)
	initTestGitRepo(t, repoB)

	now := time.Now().UTC().Truncate(time.Second)
	oldTime := now.Add(-3 * time.Hour)

	// 1. Merged orphan in repoB (reproduces blogs-open-source-agy-hq)
	wtMergedOrphan := filepath.Join(vaultDir, "worktrees", "repoB-merged-orphan")
	addTestWorktree(t, repoB, wtMergedOrphan, "hq/merged-orphan")
	commitFileInGitDir(t, wtMergedOrphan, "post.md", "# Shipped\n", "feat: blog post")
	mergeCmd := exec.Command("git", "-C", repoB, "merge", "--ff-only", "hq/merged-orphan")
	if out, err := mergeCmd.CombinedOutput(); err != nil {
		t.Fatalf("git merge into repoB main: %v (%s)", err, string(out))
	}
	_ = os.Chtimes(wtMergedOrphan, oldTime, oldTime)

	// 2. Unmerged RFC orphan in repoA (reproduces headquarter-marketplace-distribution)
	wtUnmergedOrphan := filepath.Join(vaultDir, "worktrees", "repoA-unmerged-rfc")
	addTestWorktree(t, repoA, wtUnmergedOrphan, "hq/unmerged-rfc")
	commitFileInGitDir(t, wtUnmergedOrphan, "docs/rfc.md", "# Phase 1 RFC\n", "docs: phase 1 rfc")
	_ = os.Chtimes(wtUnmergedOrphan, oldTime, oldTime)

	// 3. Dirty orphan in repoA
	wtDirtyOrphan := filepath.Join(vaultDir, "worktrees", "repoA-dirty-orphan")
	addTestWorktree(t, repoA, wtDirtyOrphan, "hq/dirty-orphan")
	_ = os.WriteFile(filepath.Join(wtDirtyOrphan, "scratch.txt"), []byte("wip"), 0o644)
	_ = os.Chtimes(wtDirtyOrphan, oldTime, oldTime)

	// 4. Dormant (>72h) idle task with UNMERGED commits (weekend Phase 1 RFC waiting for review)
	wtDormantRFC := filepath.Join(vaultDir, "worktrees", "repoA-dormant-rfc")
	addTestWorktree(t, repoA, wtDormantRFC, "hq/dormant-rfc")
	commitFileInGitDir(t, wtDormantRFC, "docs/weekend-rfc.md", "# Weekend RFC\n", "docs: weekend rfc")
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:      "dormant-rfc",
		Title:         "[Task:repoA] Weekend RFC",
		Kind:          "task",
		WorktreeDir:   wtDormantRFC,
		RepoRoot:      repoA,
		Status:        "idle",
		CreatedAt:     now.Add(-72 * time.Hour).Format(time.RFC3339),
		LastUpdatedAt: now.Add(-72 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RegisterTask dormant-rfc: %v", err)
	}

	// 5. Completed Layer-1 subagent (>30h old) with clean managed worktree + non-managed main repo root guard check
	wtSubagent := filepath.Join(vaultDir, "worktrees", "repoA-subagent-fix")
	addTestWorktree(t, repoA, wtSubagent, "hq/subagent-fix")
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:      "subagent-fix",
		Title:         "[Subagent:repoA] Bug Fix",
		Kind:          "subagent",
		WorktreeDir:   wtSubagent,
		Status:        "completed",
		CreatedAt:     now.Add(-30 * time.Hour).Format(time.RFC3339),
		LastUpdatedAt: now.Add(-30 * time.Hour).Format(time.RFC3339),
		CompletedAt:   now.Add(-30 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RegisterTask subagent-fix: %v", err)
	}

	// Dry-run preview must identify repoB-merged-orphan without deleting anything
	dryRep, err := CompactTasks(vaultDir, CompactTasksOptions{
		Now:       now,
		DryRun:    true,
		NoSidebar: true,
	})
	if err != nil {
		t.Fatalf("CompactTasks dry-run: %v", err)
	}
	if len(dryRep.PrunedWorktrees) != 1 || dryRep.PrunedWorktrees[0] != "repoB-merged-orphan" {
		t.Fatalf("expected dry-run PrunedWorktrees=[repoB-merged-orphan], got %+v", dryRep.PrunedWorktrees)
	}
	if _, err := os.Stat(wtMergedOrphan); err != nil {
		t.Fatalf("dry-run must not delete wtMergedOrphan: %v", err)
	}

	// Live CompactTasks prunes merged orphan + completed subagent worktree while protecting unmerged & dirty worktrees
	liveRep, err := CompactTasks(vaultDir, CompactTasksOptions{
		Now:       now,
		DryRun:    false,
		NoSidebar: true,
	})
	if err != nil {
		t.Fatalf("CompactTasks live: %v", err)
	}
	if len(liveRep.PrunedWorktrees) != 1 || liveRep.PrunedWorktrees[0] != "repoB-merged-orphan" {
		t.Fatalf("expected live PrunedWorktrees=[repoB-merged-orphan], got %+v", liveRep.PrunedWorktrees)
	}
	if _, err := os.Stat(wtMergedOrphan); !os.IsNotExist(err) {
		t.Fatalf("expected wtMergedOrphan to be deleted, stat err=%v", err)
	}
	if _, err := os.Stat(wtSubagent); !os.IsNotExist(err) {
		t.Fatalf("expected completed subagent worktree wtSubagent to be deleted on cold rotation, stat err=%v", err)
	}

	// Protected worktrees must all remain intact on disk
	for _, protected := range []string{wtUnmergedOrphan, wtDirtyOrphan, wtDormantRFC} {
		if _, err := os.Stat(protected); err != nil {
			t.Fatalf("expected protected worktree %s to remain intact, stat err=%v", protected, err)
		}
	}
	hotTasks, _ := ListTasks(vaultDir)
	if len(hotTasks) != 1 || hotTasks[0].TaskSlug != "dormant-rfc" || hotTasks[0].Status == "archived" {
		t.Fatalf("expected dormant-rfc with unmerged commits to stay unarchived in hot tasks, got %+v", hotTasks)
	}
}
