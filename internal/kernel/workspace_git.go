package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/workspace"
)

func init() {
	workspace.RegisterBuiltin(gitWorktreeProvider{})
}

// GitWorktreeAdder creates (or idempotently returns) a managed git worktree for taskName from repoSpec (a local
// repository path, remote URL, or owner/repo slug) under <vault>/worktrees/. It has the signature of
// internal/worktree.Add.
type GitWorktreeAdder func(vaultDir, repoSpec, taskName, branch string) (string, error)

var (
	gitWorktreeAdderMu sync.RWMutex
	gitWorktreeAdder   GitWorktreeAdder
)

// RegisterGitWorktreeAdder wires the git worktree creation primitive (internal/worktree.Add) into the built-in
// core/git-worktree provider and returns the previously registered adder. The kernel cannot import
// internal/worktree directly (it depends on internal/vault, which depends on the kernel), so cmd/hq registers it
// at startup. Until an adder is registered the provider reports Capabilities().Create == false and Create
// returns CodeUnsupported.
func RegisterGitWorktreeAdder(fn GitWorktreeAdder) GitWorktreeAdder {
	gitWorktreeAdderMu.Lock()
	defer gitWorktreeAdderMu.Unlock()
	prev := gitWorktreeAdder
	gitWorktreeAdder = fn
	return prev
}

func currentGitWorktreeAdder() GitWorktreeAdder {
	gitWorktreeAdderMu.RLock()
	defer gitWorktreeAdderMu.RUnlock()
	return gitWorktreeAdder
}

// gitWorktreeProvider is the built-in core/git-worktree provider (RFC-0025 §4.2). It wraps the existing
// worktree helpers without changing them and keeps IsManagedWorktreeDir as its own invariant: it only ever
// reports, inspects, or removes direct children of <vault>/worktrees/. Workspace IDs are canonical absolute
// worktree paths (bare directory names are accepted as input), so LedgerKey(provider, id) equals the canonical
// core/git-worktree ledger key for that worktree.
type gitWorktreeProvider struct {
	vaultDir string
}

var _ workspace.Provider = gitWorktreeProvider{}

func (p gitWorktreeProvider) ID() string { return workspace.CoreGitProviderID }

func (p gitWorktreeProvider) Description() string {
	return "Built-in git worktrees under <vault>/worktrees/ (hq worktree, hq spawn --repo)"
}

// ForVault binds the provider to vaultDir (workspace.Resolve calls it).
func (p gitWorktreeProvider) ForVault(vaultDir string) workspace.Provider {
	return gitWorktreeProvider{vaultDir: strings.TrimSpace(vaultDir)}
}

func (p gitWorktreeProvider) Capabilities() workspace.Capabilities {
	return workspace.Capabilities{
		Create:        currentGitWorktreeAdder() != nil,
		Remove:        true,
		Changes:       true,
		SharedHistory: true,
	}
}

func (p gitWorktreeProvider) ready(ctx context.Context) error {
	if p.vaultDir == "" {
		return &workspace.ProviderError{Code: workspace.CodeUnavailable, Message: "core/git-worktree is not bound to a Vault (obtain it via workspace.Resolve)"}
	}
	if ctx != nil {
		return ctx.Err()
	}
	return nil
}

// resolveDir maps a workspace ID (absolute worktree path or bare directory name under <vault>/worktrees/) to a
// managed worktree directory. The directory may not exist (Status then reports StateMissing).
func (p gitWorktreeProvider) resolveDir(workspaceID string) (string, error) {
	id := strings.TrimSpace(workspaceID)
	if id == "" {
		return "", &workspace.ProviderError{Code: workspace.CodeInvalid, Message: "workspace_id is required"}
	}
	var dir string
	if filepath.IsAbs(id) {
		dir = filepath.Clean(id)
	} else {
		if strings.ContainsAny(id, `/\`) || id == "." || id == ".." {
			return "", &workspace.ProviderError{Code: workspace.CodeInvalid, Message: fmt.Sprintf("workspace_id %q must be an absolute path or a worktree directory name", id)}
		}
		dir = filepath.Join(p.vaultDir, "worktrees", id)
	}
	if !IsManagedWorktreeDir(p.vaultDir, dir) {
		return "", &workspace.ProviderError{Code: workspace.CodeInvalid, Message: fmt.Sprintf("%q is not a managed worktree directly under <vault>/worktrees/", id)}
	}
	return dir, nil
}

// Probe reports the managed worktree that contains path, without running git.
func (p gitWorktreeProvider) Probe(ctx context.Context, path string) (workspace.Info, bool, error) {
	if err := p.ready(ctx); err != nil {
		return workspace.Info{}, false, err
	}
	clean := strings.TrimSpace(path)
	if clean == "" || !filepath.IsAbs(clean) {
		return workspace.Info{}, false, &workspace.ProviderError{Code: workspace.CodeInvalid, Message: "probe path must be absolute"}
	}
	wtRoot := workspace.CanonicalPath(filepath.Join(p.vaultDir, "worktrees"))
	rel, err := filepath.Rel(wtRoot, workspace.CanonicalPath(clean))
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return workspace.Info{}, false, nil
	}
	name := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	dir := filepath.Join(wtRoot, name)
	if !IsManagedWorktreeDir(p.vaultDir, dir) {
		return workspace.Info{}, false, nil
	}
	if st, sErr := os.Stat(dir); sErr != nil || !st.IsDir() {
		return workspace.Info{}, false, nil
	}
	info := newGitWorktreeInfo(dir, name)
	info.State = workspace.StateUnknown
	return info, true, nil
}

// List pages through the worktrees reported by InspectWorktrees. The cursor is a decimal offset. Unlike pack
// providers' cheap `list`, git entries include state and the branch change because InspectWorktrees computes
// them anyway.
func (p gitWorktreeProvider) List(ctx context.Context, req workspace.ListRequest) ([]workspace.Info, string, error) {
	if err := p.ready(ctx); err != nil {
		return nil, "", err
	}
	offset := 0
	if c := strings.TrimSpace(req.Cursor); c != "" {
		n, err := strconv.Atoi(c)
		if err != nil || n < 0 {
			return nil, "", &workspace.ProviderError{Code: workspace.CodeInvalid, Message: fmt.Sprintf("invalid list cursor %q", c)}
		}
		offset = n
	}
	limit := req.Limit
	if limit <= 0 || limit > workspace.MaxListLimit {
		limit = workspace.MaxListLimit
	}
	statuses, err := InspectWorktrees(p.vaultDir, time.Now())
	if err != nil {
		return nil, "", err
	}
	if offset >= len(statuses) {
		return nil, "", nil
	}
	end := offset + limit
	next := ""
	if end < len(statuses) {
		next = strconv.Itoa(end)
	} else {
		end = len(statuses)
	}
	out := make([]workspace.Info, 0, end-offset)
	for _, st := range statuses[offset:end] {
		out = append(out, gitWorktreeInfo(st))
	}
	return out, next, nil
}

// Create adds a worktree named after req.Name via the registered adder (internal/worktree.Add). req.Base names
// the source repository (local path, remote URL, or owner/repo slug); req.ShareFrom instead names an existing
// managed worktree whose repository is reused (git worktrees always share history). Existed reports whether
// the worktree directory was already present.
func (p gitWorktreeProvider) Create(ctx context.Context, req workspace.CreateRequest) (workspace.CreateResult, error) {
	if err := p.ready(ctx); err != nil {
		return workspace.CreateResult{}, err
	}
	adder := currentGitWorktreeAdder()
	if adder == nil {
		return workspace.CreateResult{}, &workspace.ProviderError{Code: workspace.CodeUnsupported, Message: "core/git-worktree creation is not wired into this binary"}
	}
	if err := workspace.ValidateCreateName(req.Name); err != nil {
		return workspace.CreateResult{}, err
	}
	repoSpec := strings.TrimSpace(req.Base)
	if share := strings.TrimSpace(req.ShareFrom); share != "" {
		srcDir, err := p.resolveDir(share)
		if err != nil {
			return workspace.CreateResult{}, err
		}
		if st, sErr := os.Stat(srcDir); sErr != nil || !st.IsDir() {
			return workspace.CreateResult{}, &workspace.ProviderError{Code: workspace.CodeNotFound, Message: fmt.Sprintf("share_from worktree %q does not exist", share)}
		}
		repoSpec = resolveRepoRootForWorktree(p.vaultDir, srcDir)
	}
	if repoSpec == "" {
		return workspace.CreateResult{}, &workspace.ProviderError{Code: workspace.CodeInvalid, Message: "core/git-worktree create needs base (repository path, URL, or owner/repo slug) or share_from"}
	}
	if strings.ContainsAny(repoSpec, "\x00\r\n") {
		return workspace.CreateResult{}, &workspace.ProviderError{Code: workspace.CodeInvalid, Message: "repository spec contains control characters"}
	}
	before := managedWorktreeNames(p.vaultDir)
	wtDir, err := adder(p.vaultDir, repoSpec, req.Name, "")
	if err != nil {
		return workspace.CreateResult{}, fmt.Errorf("core/git-worktree create %s: %w", req.Name, err)
	}
	if !IsManagedWorktreeDir(p.vaultDir, wtDir) {
		return workspace.CreateResult{}, fmt.Errorf("core/git-worktree create %s: worktree %q is outside <vault>/worktrees/", req.Name, wtDir)
	}
	return workspace.CreateResult{
		Info:    gitWorktreeInfo(inspectManagedWorktree(p.vaultDir, wtDir)),
		Existed: before[filepath.Base(filepath.Clean(wtDir))],
	}, nil
}

// Status inspects one managed worktree (StateMissing when its directory is gone).
func (p gitWorktreeProvider) Status(ctx context.Context, workspaceID string) (workspace.Info, error) {
	if err := p.ready(ctx); err != nil {
		return workspace.Info{}, err
	}
	dir, err := p.resolveDir(workspaceID)
	if err != nil {
		return workspace.Info{}, err
	}
	if st, sErr := os.Stat(dir); sErr != nil || !st.IsDir() {
		info := newGitWorktreeInfo(dir, filepath.Base(dir))
		info.State = workspace.StateMissing
		return info, nil
	}
	return gitWorktreeInfo(inspectManagedWorktree(p.vaultDir, dir)), nil
}

// Remove deletes one managed worktree through removeManagedWorktree after re-checking the request's
// preconditions: RequireClean refuses dirty trees (CodeDirty) and RequireNoOpenChanges refuses branches not yet
// merged into the default branch (CodeHasOpenChanges). Without RequireClean the removal is forced.
func (p gitWorktreeProvider) Remove(ctx context.Context, req workspace.RemoveRequest) error {
	if err := p.ready(ctx); err != nil {
		return err
	}
	dir, err := p.resolveDir(req.WorkspaceID)
	if err != nil {
		return err
	}
	if exp := strings.TrimSpace(req.ExpectedRoot); exp != "" && workspace.CanonicalPath(exp) != workspace.CanonicalPath(dir) {
		return &workspace.ProviderError{Code: workspace.CodeConflict, Message: fmt.Sprintf("expected_root %q does not match worktree %q", exp, dir)}
	}
	if st, sErr := os.Stat(dir); sErr != nil || !st.IsDir() {
		return &workspace.ProviderError{Code: workspace.CodeNotFound, Message: fmt.Sprintf("worktree %q does not exist", dir)}
	}
	if req.RequireClean && !isWorktreeCleanOrAbsent(dir) {
		return &workspace.ProviderError{Code: workspace.CodeDirty, Message: fmt.Sprintf("worktree %q has uncommitted changes", dir)}
	}
	if req.RequireNoOpenChanges && !isWorktreeMergedIntoDefaultBranch(p.vaultDir, dir) {
		return &workspace.ProviderError{Code: workspace.CodeHasOpenChanges, Message: fmt.Sprintf("worktree %q holds commits not merged into the default branch", dir)}
	}
	if _, err := removeManagedWorktree(p.vaultDir, dir, "", !req.RequireClean, true); err != nil {
		return err
	}
	if st, sErr := os.Stat(dir); sErr == nil && st.IsDir() {
		return fmt.Errorf("core/git-worktree remove: %q still exists", dir)
	}
	return nil
}

// inspectManagedWorktree computes, for one directory, the same fields InspectWorktrees reports per worktree
// (without the owner-task lookup), reusing the same git helpers.
func inspectManagedWorktree(vaultDir, wtPath string) ManagedWorktreeStatus {
	st := ManagedWorktreeStatus{
		Name:           filepath.Base(wtPath),
		Path:           wtPath,
		RepoRoot:       resolveRepoRootForWorktree(vaultDir, wtPath),
		Branch:         worktreeBranchName(wtPath),
		Clean:          isWorktreeCleanOrAbsent(wtPath),
		MergedIntoMain: isWorktreeMergedIntoDefaultBranch(vaultDir, wtPath),
		PushedToOrigin: isWorktreePushedToOrigin(vaultDir, wtPath),
	}
	if info, err := os.Stat(wtPath); err == nil {
		st.UpdatedAt = info.ModTime().UTC().Format(time.RFC3339)
	}
	return st
}

func newGitWorktreeInfo(dir, name string) workspace.Info {
	root := workspace.CanonicalPath(dir)
	return workspace.Info{
		Provider:    workspace.CoreGitProviderID,
		WorkspaceID: root,
		Name:        name,
		Root:        root,
		WorkDir:     root,
		Flavor:      "git",
		Kind:        "primary",
		State:       workspace.StateUnknown,
	}
}

// gitWorktreeInfo maps ManagedWorktreeStatus onto the normalized model: Clean -> clean|dirty, and the branch
// becomes one ChangeRef{ID: "branch:<branch>"} that is landed when merged into the default branch, in_review
// when pushed to origin, and local otherwise.
func gitWorktreeInfo(st ManagedWorktreeStatus) workspace.Info {
	info := newGitWorktreeInfo(st.Path, st.Name)
	if st.Clean {
		info.State = workspace.StateClean
	} else {
		info.State = workspace.StateDirty
	}
	if branch := strings.TrimSpace(st.Branch); branch != "" {
		state := workspace.ChangeLocal
		switch {
		case st.MergedIntoMain:
			state = workspace.ChangeLanded
		case st.PushedToOrigin:
			state = workspace.ChangeInReview
		}
		info.Changes = []workspace.ChangeRef{{ID: "branch:" + branch, State: state, UpdatedAt: st.UpdatedAt}}
	}
	return info
}

func managedWorktreeNames(vaultDir string) map[string]bool {
	names := make(map[string]bool)
	entries, err := os.ReadDir(filepath.Join(vaultDir, "worktrees"))
	if err != nil {
		return names
	}
	for _, e := range entries {
		if e.IsDir() {
			names[e.Name()] = true
		}
	}
	return names
}
