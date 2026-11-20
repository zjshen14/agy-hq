package kernel

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/workspace"
)

const (
	// DefaultOrphanWorktreeMinAge prevents background compaction from racing with a brand-new
	// unlinked git worktree checked out moments earlier.
	DefaultOrphanWorktreeMinAge = 1 * time.Hour
)

func sameCleanPath(a, b string) bool {
	ca := workspace.CanonicalPath(a)
	cb := workspace.CanonicalPath(b)
	return ca != "" && ca == cb
}

// IsManagedWorktreeDir returns true iff dir is a direct child directory under <vault>/worktrees/.
// This Ring-0 guard guarantees task archival and orphan pruning can never remove a primary repository root
// or any directory outside <vault>/worktrees/.
func IsManagedWorktreeDir(vaultDir, dir string) bool {
	if strings.TrimSpace(vaultDir) == "" || strings.TrimSpace(dir) == "" {
		return false
	}
	wtRoot := workspace.CanonicalPath(filepath.Join(vaultDir, "worktrees"))
	cleanDir := workspace.CanonicalPath(dir)
	if wtRoot == "" || cleanDir == "" || cleanDir == wtRoot {
		return false
	}
	return filepath.Dir(cleanDir) == wtRoot
}

// AllWorktreeDirs returns all deduplicated worktree directories owned by t (primary WorktreeDir first,
// followed by any secondary WorktreeDirs).
func (t ActiveTask) AllWorktreeDirs() []string {
	var out []string
	add := func(raw string) {
		s := strings.TrimSpace(raw)
		if s == "" {
			return
		}
		clean := filepath.Clean(s)
		for _, existing := range out {
			if sameCleanPath(existing, clean) {
				return
			}
		}
		out = append(out, clean)
	}
	add(t.WorktreeDir)
	for _, d := range t.WorktreeDirs {
		add(d)
	}
	return out
}

// ExistingWorktreeDirs returns the subset of AllWorktreeDirs that currently exist as directories on disk.
func (t ActiveTask) ExistingWorktreeDirs() []string {
	var out []string
	for _, d := range t.AllWorktreeDirs() {
		if info, err := os.Stat(d); err == nil && info.IsDir() {
			out = append(out, d)
		}
	}
	return out
}

// AddWorktreeDir binds wtDir to t (setting WorktreeDir if empty, otherwise appending to WorktreeDirs).
// Returns true if t was modified.
func (t *ActiveTask) AddWorktreeDir(wtDir, repoRoot string) bool {
	wtDir = strings.TrimSpace(wtDir)
	if wtDir == "" {
		return false
	}
	clean := filepath.Clean(wtDir)
	changed := false
	if strings.TrimSpace(t.WorktreeDir) == "" {
		t.WorktreeDir = clean
		if strings.TrimSpace(t.RepoRoot) == "" && strings.TrimSpace(repoRoot) != "" {
			t.RepoRoot = strings.TrimSpace(repoRoot)
		}
		var filtered []string
		for _, d := range t.WorktreeDirs {
			if strings.TrimSpace(d) != "" && !sameCleanPath(d, clean) {
				filtered = append(filtered, filepath.Clean(strings.TrimSpace(d)))
			}
		}
		t.WorktreeDirs = filtered
		return true
	}
	if sameCleanPath(t.WorktreeDir, clean) {
		if strings.TrimSpace(t.RepoRoot) == "" && strings.TrimSpace(repoRoot) != "" {
			t.RepoRoot = strings.TrimSpace(repoRoot)
			return true
		}
		return false
	}
	for _, existing := range t.WorktreeDirs {
		if sameCleanPath(existing, clean) {
			return changed
		}
	}
	t.WorktreeDirs = append(t.WorktreeDirs, clean)
	return true
}

func mergeTaskWorktrees(existing, incoming ActiveTask) (string, []string) {
	var candidates []string
	primary := strings.TrimSpace(incoming.WorktreeDir)
	if primary != "" {
		primary = filepath.Clean(primary)
		if strings.TrimSpace(existing.WorktreeDir) != "" && !sameCleanPath(existing.WorktreeDir, primary) {
			candidates = append(candidates, existing.WorktreeDir)
		}
	} else if strings.TrimSpace(existing.WorktreeDir) != "" {
		primary = filepath.Clean(strings.TrimSpace(existing.WorktreeDir))
	}
	candidates = append(candidates, existing.WorktreeDirs...)
	candidates = append(candidates, incoming.WorktreeDirs...)

	var extras []string
	for _, raw := range candidates {
		s := strings.TrimSpace(raw)
		if s == "" {
			continue
		}
		clean := filepath.Clean(s)
		if primary != "" && sameCleanPath(primary, clean) {
			continue
		}
		dup := false
		for _, e := range extras {
			if sameCleanPath(e, clean) {
				dup = true
				break
			}
		}
		if !dup {
			extras = append(extras, clean)
		}
	}
	return primary, extras
}

// AttachWorktreeOptions configures how AttachWorktreeToTaskWithOptions updates the owning task.
type AttachWorktreeOptions struct {
	ReactivateCompleted bool
}

// AttachWorktreeToTask binds a managed worktree under <vault>/worktrees/ to the owning task
// identified by targetTaskOrConvID (falling back to $ANTIGRAVITY_CONVERSATION_ID and walking
// nested subagent parent links up to the owning task). Front Desk conversations are never bound.
func AttachWorktreeToTask(vaultDir, targetTaskOrConvID, wtDir, repoRoot string) (string, bool) {
	return AttachWorktreeToTaskWithOptions(vaultDir, targetTaskOrConvID, wtDir, repoRoot, AttachWorktreeOptions{
		ReactivateCompleted: strings.TrimSpace(targetTaskOrConvID) == "",
	})
}

// AttachWorktreeToTaskWithOptions binds a managed worktree to its owning task with explicit options.
func AttachWorktreeToTaskWithOptions(vaultDir, targetTaskOrConvID, wtDir, repoRoot string, opts AttachWorktreeOptions) (string, bool) {
	if !IsManagedWorktreeDir(vaultDir, wtDir) {
		return "", false
	}
	cur := strings.TrimSpace(targetTaskOrConvID)
	if cur == "" {
		cur = strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	}
	if cur == "" || IsFrontDeskConversation(vaultDir, cur) {
		return "", false
	}

	nestedMap, _ := loadNestedConversations(vaultDir)
	deskConv := ""
	if fd, ok := GetFrontDesk(vaultDir); ok {
		deskConv = strings.TrimSpace(fd.ConversationID)
	}

	matchedSlug := ""
	matched := false
	_ = UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		if len(tasks) == 0 {
			return tasks, false, nil
		}
		matchedIdx := findOwningTaskIndex(tasks, nestedMap, deskConv, cur)
		if matchedIdx < 0 {
			return tasks, false, nil
		}

		t := &tasks[matchedIdx]
		changed := t.AddWorktreeDir(wtDir, repoRoot)
		nowStr := time.Now().Format(time.RFC3339)
		if opts.ReactivateCompleted && t.EffectiveKind() == "task" && t.Status == "completed" {
			t.Status = "active"
			t.CompletedAt = ""
			t.ArchivedAt = ""
			t.SidebarArchived = false
			changed = true
		}
		if changed {
			t.LastUpdatedAt = nowStr
		}
		matchedSlug = t.TaskSlug
		matched = true
		return tasks, changed, nil
	})
	if !matched {
		return "", false
	}
	return matchedSlug, true
}

func worktreeBranchName(wtDir string) string {
	out, err := sanitizedGitCmd("-C", wtDir, "symbolic-ref", "--quiet", "--short", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func worktreeHeadSHA(wtDir string) string {
	out, err := sanitizedGitCmd("-C", wtDir, "rev-parse", "--verify", "--quiet", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func candidateDefaultBranchRefs(repoRoot string) []string {
	var refs []string
	seen := make(map[string]bool)
	add := func(r string) {
		r = strings.TrimSpace(r)
		if r != "" && !seen[r] {
			seen[r] = true
			refs = append(refs, r)
		}
	}
	if out, err := sanitizedGitCmd("-C", repoRoot, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD").Output(); err == nil {
		add(strings.TrimSpace(string(out)))
	}
	add("refs/heads/main")
	add("refs/remotes/origin/main")
	add("refs/heads/master")
	add("refs/remotes/origin/master")

	var existing []string
	for _, r := range refs {
		if sanitizedGitCmd("-C", repoRoot, "rev-parse", "--verify", "--quiet", r).Run() == nil {
			existing = append(existing, r)
		}
	}
	if len(existing) == 0 {
		if out, err := sanitizedGitCmd("-C", repoRoot, "symbolic-ref", "--quiet", "HEAD").Output(); err == nil {
			r := strings.TrimSpace(string(out))
			if r != "" && sanitizedGitCmd("-C", repoRoot, "rev-parse", "--verify", "--quiet", r).Run() == nil {
				existing = append(existing, r)
			}
		}
	}
	return existing
}

func isWorktreeMergedIntoDefaultBranch(vaultDir, wtDir string) bool {
	if strings.TrimSpace(wtDir) == "" {
		return true
	}
	info, err := os.Stat(wtDir)
	if err != nil || !info.IsDir() {
		return true
	}
	headSHA := worktreeHeadSHA(wtDir)
	if headSHA == "" {
		return false
	}
	repoRoot := resolveRepoRootForWorktree(vaultDir, wtDir)
	if repoRoot == "" {
		return false
	}
	defaultRefs := candidateDefaultBranchRefs(repoRoot)
	if len(defaultRefs) == 0 {
		return false
	}
	for _, ref := range defaultRefs {
		if sanitizedGitCmd("-C", repoRoot, "merge-base", "--is-ancestor", headSHA, ref).Run() == nil {
			return true
		}
		if cherryOut, cErr := sanitizedGitCmd("-C", repoRoot, "cherry", ref, headSHA).Output(); cErr == nil {
			lines := strings.Split(strings.TrimSpace(string(cherryOut)), "\n")
			hasMinus := false
			hasPlus := false
			for _, line := range lines {
				line = strings.TrimSpace(line)
				if line == "" {
					continue
				}
				if strings.HasPrefix(line, "+") {
					hasPlus = true
					break
				}
				if strings.HasPrefix(line, "-") {
					hasMinus = true
				}
			}
			if hasMinus && !hasPlus {
				return true
			}
		}
	}
	return false
}

func isWorktreePushedToOrigin(vaultDir, wtDir string) bool {
	branch := worktreeBranchName(wtDir)
	headSHA := worktreeHeadSHA(wtDir)
	if branch == "" || headSHA == "" {
		return false
	}
	repoRoot := resolveRepoRootForWorktree(vaultDir, wtDir)
	if repoRoot == "" {
		return false
	}
	remoteRef := "refs/remotes/origin/" + branch
	if sanitizedGitCmd("-C", repoRoot, "rev-parse", "--verify", "--quiet", remoteRef).Run() != nil {
		return false
	}
	return sanitizedGitCmd("-C", repoRoot, "merge-base", "--is-ancestor", headSHA, remoteRef).Run() == nil
}

func areAllTaskWorktreesCleanOrAbsent(vaultDir string, t ActiveTask) bool {
	for _, wtDir := range t.AllWorktreeDirs() {
		if !IsManagedWorktreeDir(vaultDir, wtDir) {
			continue
		}
		if !isWorktreeCleanOrAbsent(wtDir) {
			return false
		}
	}
	return true
}

func areAllTaskWorktreesMergedAndClean(vaultDir string, t ActiveTask) bool {
	for _, wtDir := range t.AllWorktreeDirs() {
		if !IsManagedWorktreeDir(vaultDir, wtDir) {
			continue
		}
		if !isWorktreeCleanOrAbsent(wtDir) || !isWorktreeMergedIntoDefaultBranch(vaultDir, wtDir) {
			return false
		}
	}
	return true
}

func validateTaskWorktreesForArchive(vaultDir, target string, t ActiveTask, force bool) error {
	for _, wtDir := range t.AllWorktreeDirs() {
		if !IsManagedWorktreeDir(vaultDir, wtDir) {
			continue
		}
		info, err := os.Stat(wtDir)
		if err != nil || !info.IsDir() {
			continue
		}
		cmdStatus := sanitizedGitCmd("-C", wtDir, "status", "--porcelain")
		outStatus, err := cmdStatus.Output()
		if err != nil {
			return fmt.Errorf("git status check failed on %s: %w", wtDir, err)
		}
		if len(strings.TrimSpace(string(outStatus))) > 0 && !force {
			return fmt.Errorf("refusing to archive %q: uncommitted changes in worktree (pass --force to override)", target)
		}
	}
	return nil
}

func removeManagedWorktree(vaultDir, wtDir, fallbackRepoRoot string, force, strictErr bool) (string, error) {
	if !IsManagedWorktreeDir(vaultDir, wtDir) {
		return fallbackRepoRoot, nil
	}
	info, err := os.Stat(wtDir)
	if err != nil || !info.IsDir() {
		return fallbackRepoRoot, nil
	}
	repoRoot := resolveRepoRootForWorktree(vaultDir, wtDir)
	if repoRoot == "" {
		repoRoot = fallbackRepoRoot
	}
	var removeArgs []string
	if force {
		removeArgs = []string{"-C", repoRoot, "worktree", "remove", "--force", wtDir}
	} else {
		removeArgs = []string{"-C", repoRoot, "worktree", "remove", wtDir}
	}
	if out, rErr := sanitizedGitCmd(removeArgs...).CombinedOutput(); rErr != nil {
		if strictErr {
			return repoRoot, fmt.Errorf("git worktree remove failed: %w (%s)", rErr, string(out))
		}
		return repoRoot, nil
	}
	if _, sErr := os.Stat(wtDir); sErr == nil {
		_ = os.RemoveAll(wtDir)
	}
	_ = sanitizedGitCmd("-C", repoRoot, "worktree", "prune").Run()
	return repoRoot, nil
}

func removeTaskWorktrees(vaultDir string, t *ActiveTask, force, strictErr bool) error {
	for _, wtDir := range t.AllWorktreeDirs() {
		fallbackRepo := ""
		if sameCleanPath(wtDir, t.WorktreeDir) {
			fallbackRepo = t.RepoRoot
		}
		resolvedRepo, err := removeManagedWorktree(vaultDir, wtDir, fallbackRepo, force, strictErr)
		if sameCleanPath(wtDir, t.WorktreeDir) && t.RepoRoot == "" && resolvedRepo != "" {
			t.RepoRoot = resolvedRepo
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// ManagedWorktreeStatus describes the ownership, git cleanliness, and merge state of a directory in <vault>/worktrees/.
type ManagedWorktreeStatus struct {
	Name           string `json:"name"`
	Path           string `json:"path"`
	RepoRoot       string `json:"repoRoot,omitempty"`
	Branch         string `json:"branch,omitempty"`
	OwnerTaskSlug  string `json:"ownerTaskSlug,omitempty"`
	OwnerStatus    string `json:"ownerStatus,omitempty"`
	Clean          bool   `json:"clean"`
	MergedIntoMain bool   `json:"mergedIntoMain"`
	PushedToOrigin bool   `json:"pushedToOrigin"`
	UpdatedAt      string `json:"updatedAt,omitempty"`
	PruneAction    string `json:"pruneAction,omitempty"`
}

// InspectWorktrees inspects all directories under <vault>/worktrees/ and cross-references them against live hot tasks.
func InspectWorktrees(vaultDir string, now time.Time) ([]ManagedWorktreeStatus, error) {
	if now.IsZero() {
		now = time.Now()
	}
	wtRoot := filepath.Join(vaultDir, "worktrees")
	entries, err := os.ReadDir(wtRoot)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})

	tasks, _ := ListTasks(vaultDir)
	var liveTasks []ActiveTask
	for _, t := range tasks {
		st := strings.ToLower(strings.TrimSpace(t.Status))
		if st != "archived" {
			liveTasks = append(liveTasks, t)
		}
	}

	var statuses []ManagedWorktreeStatus
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		wtPath := filepath.Join(wtRoot, e.Name())
		if !IsManagedWorktreeDir(vaultDir, wtPath) {
			continue
		}
		var modTime time.Time
		if info, iErr := e.Info(); iErr == nil {
			modTime = info.ModTime()
		}
		ownerSlug := ""
		ownerStatus := ""
		for _, lt := range liveTasks {
			for _, d := range lt.AllWorktreeDirs() {
				if sameCleanPath(d, wtPath) {
					ownerSlug = lt.TaskSlug
					ownerStatus = lt.EffectiveStatus(now)
					break
				}
			}
			if ownerSlug != "" {
				break
			}
		}
		if ownerSlug == "" {
			for _, lt := range liveTasks {
				if lt.TaskSlug != "" && (e.Name() == lt.TaskSlug || strings.HasSuffix(e.Name(), "-"+lt.TaskSlug)) {
					ownerSlug = lt.TaskSlug
					ownerStatus = lt.EffectiveStatus(now)
					break
				}
			}
		}

		clean := isWorktreeCleanOrAbsent(wtPath)
		merged := isWorktreeMergedIntoDefaultBranch(vaultDir, wtPath)
		pushed := isWorktreePushedToOrigin(vaultDir, wtPath)
		repoRoot := resolveRepoRootForWorktree(vaultDir, wtPath)
		branch := worktreeBranchName(wtPath)
		updatedStr := ""
		if !modTime.IsZero() {
			updatedStr = modTime.UTC().Format(time.RFC3339)
		}

		statuses = append(statuses, ManagedWorktreeStatus{
			Name:           e.Name(),
			Path:           wtPath,
			RepoRoot:       repoRoot,
			Branch:         branch,
			OwnerTaskSlug:  ownerSlug,
			OwnerStatus:    ownerStatus,
			Clean:          clean,
			MergedIntoMain: merged,
			PushedToOrigin: pushed,
			UpdatedAt:      updatedStr,
		})
	}
	return statuses, nil
}

// PruneWorktreesOptions configures orphan git worktree garbage collection.
type PruneWorktreesOptions struct {
	Now    time.Time
	MinAge time.Duration
	DryRun bool
	Force  bool
}

// PruneWorktreesReport summarizes orphan git worktree inspection and pruning.
type PruneWorktreesReport struct {
	Scanned    int                     `json:"scanned"`
	Pruned     []string                `json:"pruned,omitempty"`
	WouldPrune []string                `json:"wouldPrune,omitempty"`
	Kept       []ManagedWorktreeStatus `json:"kept,omitempty"`
}

// PruneOrphanWorktrees safely prunes unreferenced worktrees under <vault>/worktrees/.
// Without Force, an orphan worktree is pruned ONLY when:
//  1. No non-archived hot task owns or matches it (keep:linked)
//  2. Working tree has zero uncommitted/untracked changes (keep:dirty)
//  3. Directory mtime is at least opts.MinAge old when MinAge > 0 (keep:recent)
//  4. HEAD is already merged (ancestor or patch-equivalent) into the parent repo's default branch (keep:unmerged)
func PruneOrphanWorktrees(vaultDir string, opts PruneWorktreesOptions) (*PruneWorktreesReport, error) {
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}
	statuses, err := InspectWorktrees(vaultDir, opts.Now)
	if err != nil {
		return nil, err
	}
	rep := &PruneWorktreesReport{
		Scanned: len(statuses),
	}
	for _, st := range statuses {
		if st.OwnerTaskSlug != "" {
			st.PruneAction = "keep:linked"
			rep.Kept = append(rep.Kept, st)
			continue
		}
		if !opts.Force {
			if !st.Clean {
				st.PruneAction = "keep:dirty"
				rep.Kept = append(rep.Kept, st)
				continue
			}
			if opts.MinAge > 0 && st.UpdatedAt != "" {
				if ts, tErr := time.Parse(time.RFC3339, st.UpdatedAt); tErr == nil && opts.Now.After(ts) && opts.Now.Sub(ts) < opts.MinAge {
					st.PruneAction = "keep:recent"
					rep.Kept = append(rep.Kept, st)
					continue
				}
			}
			if !st.MergedIntoMain {
				st.PruneAction = "keep:unmerged"
				rep.Kept = append(rep.Kept, st)
				continue
			}
		}

		if opts.DryRun {
			rep.WouldPrune = append(rep.WouldPrune, st.Name)
			continue
		}
		if _, rErr := removeManagedWorktree(vaultDir, st.Path, st.RepoRoot, opts.Force, true); rErr != nil {
			st.PruneAction = "keep:remove_error"
			rep.Kept = append(rep.Kept, st)
			continue
		}
		rep.Pruned = append(rep.Pruned, st.Name)
	}
	return rep, nil
}
