package kernel

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

// RFC-0025 Phase B: ledger-mutating lifecycle operations on provider workspaces (spawn binding, CLI verbs,
// status refresh, and landed notifications). Git worktrees keep their existing storage and code paths
// (RFC-0025 §16.1-§16.2); everything here applies to provider (non-git) workspaces recorded in the ledger.
//
// Lock discipline: provider subprocesses never run inside UpdateTasks / UpdateLedger callbacks. Callers snapshot,
// call the provider, then re-lock and apply (compare-and-set on status.refreshed_at). Task and ledger updates run
// sequentially (never nested), and outbox writes (RankOutbox) always happen after the ledger lock is released.

const (
	// workspaceProviderCallTimeout bounds one provider call made by the lifecycle layer (the external adapter
	// enforces its own, shorter, per-manifest timeout).
	workspaceProviderCallTimeout = workspace.MaxProviderTimeout + 5*time.Second
	// DefaultOrphanWorkspaceMinAge keeps freshly created orphan workspaces out of PruneOrphanWorkspaces.
	DefaultOrphanWorkspaceMinAge = time.Hour
)

// matchWorkspaceRootPattern is workspace.MatchRootPattern. Tests substitute it to route paths to fake built-in
// providers (built-ins declare no root_patterns).
var matchWorkspaceRootPattern = workspace.MatchRootPattern

func workspaceProviderContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), workspaceProviderCallTimeout)
}

func workspaceProviderContextFrom(parent context.Context) (context.Context, context.CancelFunc) {
	if parent == nil {
		return workspaceProviderContext()
	}
	return context.WithTimeout(parent, workspaceProviderCallTimeout)
}

// WorkspaceSafeErrorSummary converts an arbitrary provider/transport error into a short code-only summary
// free of provider-controlled free text or raw stderr, suitable for status.error and LLM-visible surfaces.
func WorkspaceSafeErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	if code := workspace.ErrorCode(err); code != "" {
		return "provider error: " + code
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timed out") {
		return "provider call timed out"
	}
	return "provider call failed"
}

func wsInvalid(format string, args ...interface{}) error {
	return &workspace.ProviderError{Code: workspace.CodeInvalid, Message: fmt.Sprintf(format, args...)}
}

func wsConflict(format string, args ...interface{}) error {
	return &workspace.ProviderError{Code: workspace.CodeConflict, Message: fmt.Sprintf(format, args...)}
}

func wsNotFound(format string, args ...interface{}) error {
	return &workspace.ProviderError{Code: workspace.CodeNotFound, Message: fmt.Sprintf(format, args...)}
}

// WorkspaceStatusRefreshSeconds returns the daemon status refresh cadence declared for p (0 = on demand only).
// Pack providers declare it in their manifest; built-in providers may implement StatusRefreshSeconds() int.
func WorkspaceStatusRefreshSeconds(p workspace.Provider) int {
	switch v := p.(type) {
	case *workspace.ExternalProvider:
		return v.Spec().StatusRefreshSeconds
	case interface{ StatusRefreshSeconds() int }:
		return v.StatusRefreshSeconds()
	}
	return 0
}

// WorkspaceProviderSkill returns the provider guide skill an agent should load before running VCS commands in a
// workspace of p ("" when the provider declares none).
func WorkspaceProviderSkill(p workspace.Provider) string {
	switch v := p.(type) {
	case *workspace.ExternalProvider:
		return strings.TrimSpace(v.Spec().Skill)
	case interface{ Skill() string }:
		return strings.TrimSpace(v.Skill())
	}
	return ""
}

// WorkspaceCreateIdempotencyKey returns the idempotency key core sends with `create` for name (RFC-0025 §6.3).
func WorkspaceCreateIdempotencyKey(providerFQID, name string) string {
	return "create:" + strings.TrimSpace(providerFQID) + ":" + strings.TrimSpace(name)
}

// WorkspacePromptPreamble renders the machine-parsable Home-placement prompt header (RFC-0025 §9.2).
func WorkspacePromptPreamble(providerFQID, workspaceID, role, workDir, skill string) string {
	if role == "" {
		role = workspace.RolePrimary
	}
	lines := []string{
		fmt.Sprintf("[hq-workspace provider=%q id=%q role=%q]", providerFQID, workspaceID, role),
		fmt.Sprintf("Work inside the isolated workspace at %s.", workDir),
	}
	if s := strings.TrimSpace(skill); s != "" {
		lines = append(lines, fmt.Sprintf("Provider guide: load the `%s` skill before running VCS commands.", s))
	}
	return strings.Join(lines, "\n")
}

// ParseWorkspaceSpec splits a `--workspace=<provider-fqid>[:<existing-workspace-id>]` value.
func ParseWorkspaceSpec(spec string) (providerFQID, workspaceID string, err error) {
	s := strings.TrimSpace(spec)
	if s == "" {
		return "", "", wsInvalid("workspace spec is empty (expected <provider-fqid>[:<workspace-id>])")
	}
	providerFQID, workspaceID, hasID := strings.Cut(s, ":")
	providerFQID = strings.TrimSpace(providerFQID)
	workspaceID = strings.TrimSpace(workspaceID)
	pack, id, ok := strings.Cut(providerFQID, "/")
	if !ok || pack == "" || id == "" || strings.Contains(id, "/") {
		return "", "", wsInvalid("workspace provider %q must look like <pack>/<id> (for example acme-monorepo/client)", providerFQID)
	}
	if hasID && !workspace.ValidWorkspaceID(workspaceID) {
		return "", "", wsInvalid("workspace id %q must match [A-Za-z0-9][A-Za-z0-9._-]{0,127}", workspaceID)
	}
	return providerFQID, workspaceID, nil
}

func normalizeWorkspaceRole(role string) (string, error) {
	switch r := strings.ToLower(strings.TrimSpace(role)); r {
	case "":
		return workspace.RolePrimary, nil
	case workspace.RolePrimary, workspace.RoleSecondary, workspace.RoleReadonly:
		return r, nil
	default:
		return "", wsInvalid("binding role %q must be primary, secondary, or readonly", role)
	}
}

// wsPathsOverlap reports whether cleaned paths a and b overlap (one equals or sits inside the other).
func wsPathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == "" || b == "" || a == "." || b == "." {
		return false
	}
	return workspace.PathWithin(a, b) || workspace.PathWithin(b, a)
}

// validateAcquiredWorkspaceInfo checks what a provider returned before it is ledgered: a well-formed ID, a clean
// absolute root that is neither "/", a top-level system directory, $HOME (or an ancestor of $HOME), nor overlapping
// the Vault, matches the pack provider's root_patterns when declared, and has a work directory inside the root.
func validateAcquiredWorkspaceInfo(vaultDir string, p workspace.Provider, info workspace.Info) error {
	if !workspace.ValidWorkspaceID(info.WorkspaceID) {
		return wsInvalid("provider returned invalid workspace_id %q", workspace.SanitizeDisplayText(info.WorkspaceID, 80))
	}
	root := info.Root
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) || workspace.HasControlOrFormatRune(root) {
		return wsInvalid("provider returned invalid workspace root %q", workspace.SanitizeDisplayText(root, 120))
	}
	canonRoot := workspace.CanonicalPath(root)
	if filepath.Dir(root) == string(filepath.Separator) || (canonRoot != "" && filepath.Dir(canonRoot) == string(filepath.Separator)) {
		return wsConflict("workspace root %s is a top-level directory; provider workspaces must live inside a dedicated subdirectory", root)
	}
	if info.WorkDir != "" && (!filepath.IsAbs(info.WorkDir) || filepath.Clean(info.WorkDir) != info.WorkDir || !workspace.PathWithin(info.WorkDir, root) || workspace.HasControlOrFormatRune(info.WorkDir)) {
		return wsInvalid("provider returned work_dir %q outside workspace root %q", workspace.SanitizeDisplayText(info.WorkDir, 120), root)
	}
	for _, v := range []string{filepath.Clean(vaultDir), workspace.CanonicalPath(vaultDir)} {
		if v != "" && v != "." && (wsPathsOverlap(root, v) || (canonRoot != "" && wsPathsOverlap(canonRoot, v))) {
			return wsConflict("workspace root %s overlaps the Vault; provider workspaces must live outside it", root)
		}
	}
	rawHome := strings.TrimSpace(hostpaths.Resolve(vaultDir).HomeDir)
	if rawHome == "" {
		if h, hErr := os.UserHomeDir(); hErr == nil {
			rawHome = strings.TrimSpace(h)
		}
	}
	var homeDirs []string
	if rawHome != "" {
		cleanHome := filepath.Clean(rawHome)
		homeDirs = append(homeDirs, cleanHome)
		if ch := workspace.CanonicalPath(rawHome); ch != "" && ch != cleanHome {
			homeDirs = append(homeDirs, ch)
		}
	}
	for _, h := range homeDirs {
		if h == "" || h == string(filepath.Separator) {
			continue
		}
		if workspace.PathWithin(h, root) || (canonRoot != "" && workspace.PathWithin(h, canonRoot)) {
			return wsConflict("workspace root %s equals or contains the user home directory", root)
		}
	}
	if ep, ok := p.(*workspace.ExternalProvider); ok && len(ep.Spec().RootPatterns) > 0 {
		matched := false
		for _, pat := range ep.Spec().RootPatterns {
			if ok1, err := filepath.Match(pat, root); err == nil && ok1 {
				matched = true
				break
			}
			if canonRoot != "" && canonRoot != root {
				if ok2, err := filepath.Match(pat, canonRoot); err == nil && ok2 {
					matched = true
					break
				}
			}
		}
		if !matched {
			return wsConflict("workspace root %s does not match provider %s root_patterns", root, ep.ID())
		}
	}
	return nil
}

func cloneChangeRefs(in []workspace.ChangeRef) []workspace.ChangeRef {
	if len(in) == 0 {
		return nil
	}
	return append([]workspace.ChangeRef(nil), in...)
}

func cloneLedgerEntry(e workspace.LedgerEntry) workspace.LedgerEntry {
	e.BoundTasks = append([]string(nil), e.BoundTasks...)
	e.NotifiedLanded = append([]string(nil), e.NotifiedLanded...)
	e.Status.Changes = cloneChangeRefs(e.Status.Changes)
	return e
}

func ledgerStatusFromInfo(info workspace.Info, nowStr string) workspace.LedgerStatus {
	return workspace.LedgerStatus{
		State:       info.State,
		Base:        info.Base,
		Changes:     cloneChangeRefs(info.Changes),
		RefreshedAt: nowStr,
	}
}

// needsOwnerWorkspaceChanges counts open changes waiting on the workspace owner.
func needsOwnerWorkspaceChanges(changes []workspace.ChangeRef) int {
	n := 0
	for _, c := range changes {
		if c.NeedsOwner && c.State.Open() {
			n++
		}
	}
	return n
}

func addUniqueSorted(list []string, v string) []string {
	v = strings.TrimSpace(v)
	if v == "" {
		return list
	}
	for _, x := range list {
		if x == v {
			return list
		}
	}
	out := append(append([]string(nil), list...), v)
	sort.Strings(out)
	return out
}

func removeString(list []string, v string) ([]string, bool) {
	var out []string
	removed := false
	for _, x := range list {
		if x == v {
			removed = true
			continue
		}
		out = append(out, x)
	}
	return out, removed
}

// releaseLedgerBinder drops slug from the entry's bound tasks and releases its writer lease. It reports whether
// the entry changed.
func releaseLedgerBinder(e *workspace.LedgerEntry, slug string) bool {
	changed := false
	if out, removed := removeString(e.BoundTasks, slug); removed {
		e.BoundTasks = out
		changed = true
	}
	if slug != "" && e.WriterTask == slug {
		e.WriterTask = ""
		e.LeaseEnforced = false
		changed = true
	}
	return changed
}

// workspaceTaskIndex is a read-only snapshot of live (non-archived) hot tasks used for lease, liveness, and
// binding decisions. It is built before any ledger lock is taken.
type workspaceTaskIndex struct {
	live     map[string]ActiveTask
	bindings map[string][]string // ledger key -> sorted slugs of live tasks holding a binding with that key
}

func isLiveTaskStatus(t ActiveTask) bool {
	return !strings.EqualFold(strings.TrimSpace(t.Status), "archived")
}

func buildWorkspaceTaskIndex(tasks []ActiveTask) workspaceTaskIndex {
	ix := workspaceTaskIndex{live: make(map[string]ActiveTask), bindings: make(map[string][]string)}
	for _, t := range tasks {
		slug := strings.TrimSpace(t.TaskSlug)
		if slug == "" || !isLiveTaskStatus(t) {
			continue
		}
		ix.live[slug] = t
		for _, b := range t.Workspaces {
			if b.Key != "" {
				ix.bindings[b.Key] = addUniqueSorted(ix.bindings[b.Key], slug)
			}
		}
	}
	return ix
}

func loadWorkspaceTaskIndex(vaultDir string) workspaceTaskIndex {
	tasks, _ := ListTasks(vaultDir)
	return buildWorkspaceTaskIndex(tasks)
}

func (ix workspaceTaskIndex) isLive(slug string) bool {
	_, ok := ix.live[strings.TrimSpace(slug)]
	return ok
}

// liveBinders returns the live tasks bound to e (ledger bound_tasks or a task binding with e's key).
func (ix workspaceTaskIndex) liveBinders(e workspace.LedgerEntry) []string {
	var out []string
	for _, s := range e.BoundTasks {
		if ix.isLive(s) {
			out = addUniqueSorted(out, s)
		}
	}
	for _, s := range ix.bindings[e.Key()] {
		out = addUniqueSorted(out, s)
	}
	return out
}

func taskHasWorkspaceBinding(t ActiveTask, key string) (workspace.Binding, bool) {
	for _, b := range t.Workspaces {
		if b.Key == key {
			return b, true
		}
	}
	return workspace.Binding{}, false
}

// taskHasPrimaryWorkspace reports whether t already has a primary binding (a git WorktreeDir or a provider binding).
func taskHasPrimaryWorkspace(t ActiveTask) bool {
	if strings.TrimSpace(t.WorktreeDir) != "" {
		return true
	}
	for _, b := range t.Workspaces {
		if b.Role == workspace.RolePrimary {
			return true
		}
	}
	return false
}

// DefaultWorkspaceBindingRole returns primary when t has no primary workspace yet and secondary otherwise.
func DefaultWorkspaceBindingRole(t ActiveTask) string {
	if taskHasPrimaryWorkspace(t) {
		return workspace.RoleSecondary
	}
	return workspace.RolePrimary
}

func bindingForEntry(e workspace.LedgerEntry, role string) workspace.Binding {
	return workspace.Binding{Key: e.Key(), Provider: e.Provider, Root: e.Root, WorkDir: e.WorkDir, Role: role}
}

// AddTaskWorkspaceBinding merges b into the provider bindings of the task with slug (the ledger is not touched).
func AddTaskWorkspaceBinding(vaultDir, slug string, b workspace.Binding) error {
	slug = strings.TrimSpace(slug)
	if slug == "" || b.Key == "" {
		return wsInvalid("task slug and binding key are required")
	}
	return UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		for i := range tasks {
			if tasks[i].TaskSlug != slug {
				continue
			}
			if existing, ok := taskHasWorkspaceBinding(tasks[i], b.Key); ok && existing == b {
				return tasks, false, nil
			}
			tasks[i].Workspaces = workspace.MergeBindings(tasks[i].Workspaces, []workspace.Binding{b})
			return tasks, true, nil
		}
		return tasks, false, wsNotFound("task %q not found in local/memory/tasks.json", slug)
	})
}

// removeTaskWorkspaceBindings drops bindings with key from the task with slug, or from every live task when slug
// is empty. It reports the slugs whose bindings were removed.
func removeTaskWorkspaceBindings(vaultDir, slug, key string) ([]string, error) {
	var touched []string
	err := UpdateTasks(vaultDir, func(tasks []ActiveTask) ([]ActiveTask, bool, error) {
		touched = nil
		changed := false
		for i := range tasks {
			if slug != "" && tasks[i].TaskSlug != slug {
				continue
			}
			if slug == "" && !isLiveTaskStatus(tasks[i]) {
				continue
			}
			var kept []workspace.Binding
			removed := false
			for _, b := range tasks[i].Workspaces {
				if b.Key == key {
					removed = true
					continue
				}
				kept = append(kept, b)
			}
			if removed {
				tasks[i].Workspaces = kept
				touched = append(touched, tasks[i].TaskSlug)
				changed = true
			}
		}
		return tasks, changed, nil
	})
	return touched, err
}

// resolveWorkspaceTask resolves a --task / --to target (slug or conversation ID; empty falls back to
// $ANTIGRAVITY_CONVERSATION_ID) to its owning task, requiring a live task when requireLive is true.
func resolveWorkspaceTask(vaultDir, target string, requireLive bool) (ActiveTask, error) {
	t, ok := ResolveOwningTask(vaultDir, target)
	if !ok {
		if strings.TrimSpace(target) == "" {
			return ActiveTask{}, wsNotFound("no owning task for this conversation (pass --task=<slug-or-conv>)")
		}
		return ActiveTask{}, wsNotFound("task %q not found (or it is the Front Desk)", target)
	}
	if requireLive && !isLiveTaskStatus(t) {
		return ActiveTask{}, wsConflict("task %q is archived (reopen it first)", t.TaskSlug)
	}
	return t, nil
}

// ---------------------------------------------------------------------------------------------------------------
// Acquire (create or attach) and rollback

// WorkspaceAcquireOptions configures AcquireWorkspace.
type WorkspaceAcquireOptions struct {
	Provider      string // provider FQID (core/git-worktree is refused: git worktrees keep their own storage)
	WorkspaceID   string // attach this existing workspace (provider `status`) instead of creating one
	Name          string // create name derived from the task slug (workspace.ValidateCreateName)
	ShareFrom     string // create a shared-history sibling of this ledger entry (key or bare workspace id, same provider)
	TaskSlug      string // owning Layer-1 task: added to bound_tasks and, unless Role is readonly, made writer_task
	Role          string // binding role for TaskSlug (default primary)
	LeaseEnforced bool   // enforce the W1 single-writer lease for TaskSlug
	BindTask      bool   // also merge the binding into the existing task's ActiveTask.Workspaces
	Now           time.Time
}

// WorkspaceAcquireResult reports what AcquireWorkspace did, including what RollbackAcquiredWorkspace must undo.
type WorkspaceAcquireResult struct {
	Entry           workspace.LedgerEntry
	Binding         workspace.Binding // zero when no TaskSlug was given
	Skill           string            // provider guide skill ("" when none)
	Created         bool              // this call created the workspace (provider existed=false, no prior ledger entry)
	ProviderExisted bool              // provider reported the workspace already existed (or it was attached by id)
	LedgerExisted   bool              // the ledger already had an entry for the workspace
	PrevWriterTask  string            // writer_task before this call
	PrevLease       bool              // lease_enforced before this call
	PrevBound       bool              // TaskSlug was already in bound_tasks before this call
	PrevOrphaned    bool              // orphaned_open_changes before this call
}

// AcquireWorkspace creates (Name) or attaches (WorkspaceID) a provider workspace and records it in the ownership
// ledger. A workspace created by this call (existed=false) is ledgered as owned; a pre-existing one as attached.
// An existing ledger entry keeps its ownership and is reused. The provider runs outside every lock; a live
// writer lease held by another task is refused (hand it off with `hq workspace handoff`). ShareFrom asks the
// provider for a shared-history sibling (RFC-0025 §6.3) of a ledgered workspace of the same provider; the provider
// must declare shared_history (pack providers additionally require the source to be ledger-owned).
func AcquireWorkspace(vaultDir string, opts WorkspaceAcquireOptions) (WorkspaceAcquireResult, error) {
	fqid := strings.TrimSpace(opts.Provider)
	if fqid == workspace.CoreGitProviderID {
		return WorkspaceAcquireResult{}, wsInvalid("%s worktrees are managed with --repo and `hq worktree`, not the provider workspace ledger", fqid)
	}
	role, err := normalizeWorkspaceRole(opts.Role)
	if err != nil {
		return WorkspaceAcquireResult{}, err
	}
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	nowStr := now.UTC().Format(time.RFC3339)
	slug := strings.TrimSpace(opts.TaskSlug)
	attachID := strings.TrimSpace(opts.WorkspaceID)
	takesLease := slug != "" && role != workspace.RoleReadonly
	shareFrom := ""
	if raw := strings.TrimSpace(opts.ShareFrom); raw != "" {
		if attachID != "" {
			return WorkspaceAcquireResult{}, wsInvalid("share_from creates a new sibling workspace; it cannot be combined with attaching %q", attachID)
		}
		if shareFrom, err = resolveShareFromSource(vaultDir, fqid, raw); err != nil {
			return WorkspaceAcquireResult{}, err
		}
	}

	ix := loadWorkspaceTaskIndex(vaultDir)
	if attachID != "" && takesLease {
		// Cheap pre-check before spawning the provider; re-checked authoritatively under the ledger lock.
		if e, ok := workspace.GetEntry(vaultDir, workspace.LedgerKey(fqid, attachID)); ok {
			if err := checkWorkspaceLease(e, slug, ix); err != nil {
				return WorkspaceAcquireResult{}, err
			}
		}
	}

	p, err := workspace.Resolve(vaultDir, fqid)
	if err != nil {
		return WorkspaceAcquireResult{}, err
	}
	ctx, cancel := workspaceProviderContext()
	defer cancel()
	var info workspace.Info
	providerExisted := false
	if attachID == "" {
		name := strings.TrimSpace(opts.Name)
		if err := workspace.ValidateCreateName(name); err != nil {
			return WorkspaceAcquireResult{}, err
		}
		if !p.Capabilities().Create {
			return WorkspaceAcquireResult{}, &workspace.ProviderError{Code: workspace.CodeUnsupported, Message: fqid + " does not support create (attach an existing workspace with <provider>:<workspace-id>)"}
		}
		if shareFrom != "" && !p.Capabilities().SharedHistory {
			return WorkspaceAcquireResult{}, &workspace.ProviderError{Code: workspace.CodeUnsupported, Message: fqid + " does not support shared-history siblings (capabilities.shared_history)"}
		}
		res, cErr := p.Create(ctx, workspace.CreateRequest{Name: name, ShareFrom: shareFrom, IdempotencyKey: WorkspaceCreateIdempotencyKey(fqid, name)})
		if cErr != nil {
			return WorkspaceAcquireResult{}, cErr
		}
		info, providerExisted = res.Info, res.Existed
	} else {
		if !workspace.ValidWorkspaceID(attachID) {
			return WorkspaceAcquireResult{}, wsInvalid("workspace id %q is malformed", attachID)
		}
		st, sErr := p.Status(ctx, attachID)
		if sErr != nil {
			return WorkspaceAcquireResult{}, sErr
		}
		if st.WorkspaceID != attachID {
			return WorkspaceAcquireResult{}, wsConflict("provider returned workspace %q for requested %q", workspace.SanitizeDisplayText(st.WorkspaceID, 80), attachID)
		}
		if st.State == workspace.StateMissing {
			return WorkspaceAcquireResult{}, wsNotFound("workspace %s does not exist", workspace.LedgerKey(fqid, attachID))
		}
		info, providerExisted = st, true
	}
	info.Provider = fqid
	if info.WorkDir == "" {
		info.WorkDir = info.Root
	}
	if err := validateAcquiredWorkspaceInfo(vaultDir, p, info); err != nil {
		return WorkspaceAcquireResult{}, err
	}
	canonRoot := workspace.CanonicalPath(info.Root)
	key := workspace.LedgerKey(fqid, info.WorkspaceID)

	var result WorkspaceAcquireResult
	var notices []landedNotice
	err = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		result = WorkspaceAcquireResult{ProviderExisted: providerExisted}
		notices = nil
		e, exists := l.Entries[key]
		if exists {
			result.LedgerExisted = true
			result.PrevWriterTask = e.WriterTask
			result.PrevLease = e.LeaseEnforced
			result.PrevBound = slices.Contains(e.BoundTasks, slug)
			result.PrevOrphaned = e.OrphanedOpenChanges
			if filepath.Clean(e.Root) != info.Root {
				return false, wsConflict("ledger entry %s has root %s but the provider reported %s", key, e.Root, info.Root)
			}
			if e.CanonicalRoot == "" && canonRoot != "" {
				e.CanonicalRoot = canonRoot
			}
			if takesLease {
				if err := checkWorkspaceLease(e, slug, ix); err != nil {
					return false, err
				}
			}
			ids := applyWorkspaceInfo(&e, info, nowStr)
			notices = buildLandedNotices(e, ids)
		} else {
			for otherKey, other := range l.Entries {
				if wsPathsOverlap(filepath.Clean(other.Root), info.Root) {
					return false, wsConflict("workspace root %s overlaps ledger entry %s (%s)", info.Root, otherKey, other.Root)
				}
			}
			e = workspace.LedgerEntry{
				Provider:      fqid,
				WorkspaceID:   info.WorkspaceID,
				Name:          info.Name,
				Root:          info.Root,
				CanonicalRoot: canonRoot,
				WorkDir:       info.WorkDir,
				Ownership:     workspace.OwnershipAttached,
				CreatedAt:     nowStr,
				Status:        ledgerStatusFromInfo(info, nowStr),
			}
			if attachID == "" && !providerExisted {
				e.Ownership = workspace.OwnershipOwned
				e.CreatedByTask = slug
			}
		}
		if e.WorkDir == "" {
			e.WorkDir = info.WorkDir
		}
		if slug != "" {
			e.BoundTasks = addUniqueSorted(e.BoundTasks, slug)
			e.OrphanedOpenChanges = false
			if takesLease {
				e.WriterTask = slug
				e.LeaseEnforced = opts.LeaseEnforced
			}
		}
		l.Put(e)
		result.Entry = cloneLedgerEntry(e)
		result.Created = !result.LedgerExisted && attachID == "" && !providerExisted
		return true, nil
	})
	if err != nil {
		return WorkspaceAcquireResult{}, err
	}
	emitLandedNotices(vaultDir, notices, now)
	result.Skill = WorkspaceProviderSkill(p)
	if slug != "" {
		result.Binding = bindingForEntry(result.Entry, role)
		if opts.BindTask {
			if err := AddTaskWorkspaceBinding(vaultDir, slug, result.Binding); err != nil {
				return result, err
			}
		}
	}
	return result, nil
}

// checkWorkspaceLease refuses to take over an enforced writer lease held by another live task.
func checkWorkspaceLease(e workspace.LedgerEntry, slug string, ix workspaceTaskIndex) error {
	if e.LeaseEnforced && e.WriterTask != "" && e.WriterTask != slug && ix.isLive(e.WriterTask) {
		return wsConflict("workspace %s is leased to live task %q; transfer it first with `hq workspace handoff %s --to=%s`", e.Key(), e.WriterTask, e.Key(), slug)
	}
	return nil
}

// resolveShareFromSource maps a --share-from value (ledger key "<fqid>:<id>" or bare workspace id) to the source
// workspace ID. The source must be a ledger entry of the same provider: core never forwards share_from values that
// are not in the ledger (RFC-0025 §6.3).
func resolveShareFromSource(vaultDir, fqid, raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if provider, wsID, ok := workspace.ParseLedgerKey(id); ok && strings.Contains(provider, "/") {
		if provider != fqid {
			return "", wsInvalid("share_from %s belongs to provider %s, not %s", id, provider, fqid)
		}
		id = wsID
	}
	if !workspace.ValidWorkspaceID(id) {
		return "", wsInvalid("share_from %q must be a ledger key (<provider-fqid>:<workspace-id>) or a workspace id", workspace.SanitizeDisplayText(raw, 80))
	}
	key := workspace.LedgerKey(fqid, id)
	e, ok := workspace.GetEntry(vaultDir, key)
	if !ok {
		return "", wsNotFound("share_from source %s is not in the workspace ledger (create or adopt it first)", key)
	}
	if e.Ownership != workspace.OwnershipOwned && e.Ownership != workspace.OwnershipAttached {
		return "", wsInvalid("share_from source %s has no recorded ownership", key)
	}
	return id, nil
}

// RollbackAcquiredWorkspace undoes AcquireWorkspace after a failed spawn. It releases taskSlug's binding and
// lease (restoring the previous writer when the entry pre-existed), drops a ledger entry this call created, and,
// when this call created the workspace itself, removes it with both safety preconditions first. A workspace the
// provider refuses to remove stays ledgered as an unbound owned orphan for PruneOrphanWorkspaces.
func RollbackAcquiredWorkspace(vaultDir string, acq WorkspaceAcquireResult, taskSlug string) error {
	key := acq.Entry.Key()
	if acq.Entry.Provider == "" || acq.Entry.WorkspaceID == "" {
		return nil
	}
	slug := strings.TrimSpace(taskSlug)
	if acq.Created {
		var rmErr error
		p, err := workspace.Resolve(vaultDir, acq.Entry.Provider)
		if err != nil {
			rmErr = err
		} else {
			ctx, cancel := workspaceProviderContext()
			rmErr = p.Remove(ctx, workspace.RemoveRequest{
				WorkspaceID:          acq.Entry.WorkspaceID,
				ExpectedRoot:         acq.Entry.Root,
				RequireClean:         true,
				RequireNoOpenChanges: true,
			})
			cancel()
		}
		if rmErr != nil && workspace.ErrorCode(rmErr) != workspace.CodeNotFound {
			_ = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
				e, ok := l.Entries[key]
				if !ok {
					return false, nil
				}
				changed := releaseLedgerBinder(&e, slug)
				l.Put(e)
				return changed, nil
			})
			return fmt.Errorf("remove workspace %s created for the failed spawn: %w", key, rmErr)
		}
		return dropLedgerEntry(vaultDir, key)
	}
	if !acq.LedgerExisted {
		return dropLedgerEntry(vaultDir, key)
	}
	return workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		e, ok := l.Entries[key]
		if !ok {
			return false, nil
		}
		changed := false
		if !acq.PrevBound && slug != "" {
			if out, removed := removeString(e.BoundTasks, slug); removed {
				e.BoundTasks = out
				changed = true
			}
		}
		if slug != "" && e.WriterTask == slug && acq.PrevWriterTask != slug {
			e.WriterTask = acq.PrevWriterTask
			e.LeaseEnforced = acq.PrevLease
			changed = true
		}
		if acq.PrevOrphaned && !e.OrphanedOpenChanges {
			e.OrphanedOpenChanges = true
			changed = true
		}
		l.Put(e)
		return changed, nil
	})
}

func dropLedgerEntry(vaultDir, key string) error {
	return workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		if _, ok := l.Entries[key]; !ok {
			return false, nil
		}
		delete(l.Entries, key)
		return true, nil
	})
}

// ---------------------------------------------------------------------------------------------------------------
// Adopt, handoff, unbind, own, remove

// WorkspaceAdoptOptions configures AdoptWorkspace.
type WorkspaceAdoptOptions struct {
	Path     string // absolute path inside the workspace
	TaskSlug string // bind to this live task ("" = ledger only)
	Role     string // binding role (default primary when the task has no primary workspace, else secondary)
	Now      time.Time
}

// AdoptWorkspace attaches the workspace that owns Path: a ledger hit is reused, otherwise the provider whose
// root_patterns match Path is asked to `probe` it. The workspace is ledgered as attached (never removed by hq).
// With a task, it is bound and the task takes the writer lease (lease_enforced) unless the role is readonly.
func AdoptWorkspace(vaultDir string, opts WorkspaceAdoptOptions) (WorkspaceAcquireResult, error) {
	raw := strings.TrimSpace(opts.Path)
	if raw == "" || !filepath.IsAbs(raw) || workspace.HasControlOrFormatRune(raw) {
		return WorkspaceAcquireResult{}, wsInvalid("adopt path must be an absolute path")
	}
	path := filepath.Clean(raw)
	var fqid, id string
	if e, ok := workspace.FindEntryForPath(vaultDir, path); ok {
		fqid, id = e.Provider, e.WorkspaceID
	} else {
		matched, _, ok := matchWorkspaceRootPattern(vaultDir, path)
		if !ok {
			return WorkspaceAcquireResult{}, wsNotFound("no enabled, trusted workspace provider claims %s (root_patterns)", path)
		}
		p, err := workspace.Resolve(vaultDir, matched)
		if err != nil {
			return WorkspaceAcquireResult{}, err
		}
		ctx, cancel := workspaceProviderContext()
		info, owned, err := p.Probe(ctx, path)
		cancel()
		if err != nil {
			return WorkspaceAcquireResult{}, err
		}
		if !owned {
			return WorkspaceAcquireResult{}, wsNotFound("workspace provider %s does not own %s", matched, path)
		}
		fqid, id = matched, info.WorkspaceID
	}
	role := opts.Role
	if strings.TrimSpace(role) == "" {
		role = workspace.RolePrimary
		if opts.TaskSlug != "" {
			if t, ok := loadWorkspaceTaskIndex(vaultDir).live[strings.TrimSpace(opts.TaskSlug)]; ok {
				role = DefaultWorkspaceBindingRole(t)
			}
		}
	}
	normRole, err := normalizeWorkspaceRole(role)
	if err != nil {
		return WorkspaceAcquireResult{}, err
	}
	slug := strings.TrimSpace(opts.TaskSlug)
	return AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{
		Provider:      fqid,
		WorkspaceID:   id,
		TaskSlug:      slug,
		Role:          normRole,
		LeaseEnforced: slug != "" && normRole != workspace.RoleReadonly,
		BindTask:      slug != "",
		Now:           opts.Now,
	})
}

// HandoffWorkspace transfers the W1 writer lease of the ledger entry key to the task resolved from toTarget
// (slug or conversation ID, walking nested-subagent parents) and enforces it. The target is bound to the
// workspace when it is not already.
func HandoffWorkspace(vaultDir, key, toTarget string) (string, error) {
	if strings.TrimSpace(toTarget) == "" {
		return "", wsInvalid("handoff needs --to=<task-slug-or-conversation-id>")
	}
	t, err := resolveWorkspaceTask(vaultDir, toTarget, true)
	if err != nil {
		return "", err
	}
	slug := t.TaskSlug
	var entry workspace.LedgerEntry
	err = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		e, ok := l.Entries[key]
		if !ok {
			return false, wsNotFound("workspace %s is not in the ledger", key)
		}
		e.WriterTask = slug
		e.LeaseEnforced = true
		e.BoundTasks = addUniqueSorted(e.BoundTasks, slug)
		e.OrphanedOpenChanges = false
		l.Put(e)
		entry = cloneLedgerEntry(e)
		return true, nil
	})
	if err != nil {
		return "", err
	}
	if _, bound := taskHasWorkspaceBinding(t, key); !bound {
		if err := AddTaskWorkspaceBinding(vaultDir, slug, bindingForEntry(entry, DefaultWorkspaceBindingRole(t))); err != nil {
			return slug, err
		}
	}
	return slug, nil
}

// UnbindWorkspace removes the binding between the ledger entry key and the task resolved from taskTarget (empty
// falls back to $ANTIGRAVITY_CONVERSATION_ID), releasing its writer lease. The workspace itself is untouched.
func UnbindWorkspace(vaultDir, key, taskTarget string) (string, error) {
	t, err := resolveWorkspaceTask(vaultDir, taskTarget, false)
	if err != nil {
		return "", err
	}
	slug := t.TaskSlug
	touched, err := removeTaskWorkspaceBindings(vaultDir, slug, key)
	if err != nil {
		return slug, err
	}
	ledgerChanged := false
	err = workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		ledgerChanged = false
		e, ok := l.Entries[key]
		if !ok {
			return false, nil
		}
		ledgerChanged = releaseLedgerBinder(&e, slug)
		l.Put(e)
		return ledgerChanged, nil
	})
	if err != nil {
		return slug, err
	}
	if len(touched) == 0 && !ledgerChanged {
		return slug, wsNotFound("workspace %s is not bound to task %q", key, slug)
	}
	return slug, nil
}

// OwnWorkspace promotes an attached ledger entry to owned, making it removable by hq. It is approval-gated
// (`hq propose --type=workspace_own`) by the core workspace_own rule. It reports whether anything changed.
func OwnWorkspace(vaultDir, key string) (workspace.LedgerEntry, bool, error) {
	var entry workspace.LedgerEntry
	changed := false
	err := workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		changed = false
		e, ok := l.Entries[key]
		if !ok {
			return false, wsNotFound("workspace %s is not in the ledger", key)
		}
		if e.Ownership != workspace.OwnershipOwned {
			e.Ownership = workspace.OwnershipOwned
			changed = true
			l.Put(e)
		}
		entry = cloneLedgerEntry(e)
		return changed, nil
	})
	return entry, changed, err
}

// WorkspaceRemoveResult reports the outcome of RemoveWorkspace.
type WorkspaceRemoveResult struct {
	Key           string
	Removed       bool     // the provider deleted the workspace and its ledger entry was dropped
	Forgotten     bool     // the workspace was already gone; its ledger entry was dropped
	NeedsApproval bool     // dirty, unknown, or open changes: stage `hq propose --type=workspace_remove`
	Reason        string   // why approval is needed
	UnboundTasks  []string // live tasks whose bindings were dropped
	Entry         workspace.LedgerEntry
}

// WorkspaceRemoveProposal returns the exact `hq propose` command that stages a forced removal of key. hqCmd is how
// the caller invokes hq (for example "./bin/hq"); the staged command itself always uses `hq`, which the approval
// executor resolves next to the approving binary.
func WorkspaceRemoveProposal(hqCmd, key, reason string) string {
	if strings.TrimSpace(hqCmd) == "" {
		hqCmd = "hq"
	}
	summary := "Remove hq-owned workspace " + key
	if r := strings.TrimSpace(reason); r != "" {
		summary += " (" + workspace.SanitizeDisplayText(strings.ReplaceAll(strings.ReplaceAll(r, `"`, ""), "'", ""), 80) + ")"
	}
	return fmt.Sprintf("%s propose --type=workspace_remove --summary=\"%s\" --cmd='hq workspace remove %s --force'", hqCmd, summary, key)
}

// RemoveWorkspace removes an owned ledger workspace. Attached workspaces are never removed (unbind them, or stage
// `hq workspace own` first). Without force, it requires a fresh status that is clean with no open changes, refuses
// workspaces bound to live tasks, and sends `remove` with both require_* preconditions; anything else reports
// NeedsApproval. With force (only reachable through `hq propose` + human approval, enforced by the core
// workspace_remove rule) it removes the owned workspace regardless of dirty state or open changes.
func RemoveWorkspace(vaultDir, key string, force bool, now time.Time) (WorkspaceRemoveResult, error) {
	if now.IsZero() {
		now = time.Now()
	}
	res := WorkspaceRemoveResult{Key: key}
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil {
		return res, err
	}
	e, ok := l.Entries[key]
	if !ok {
		return res, wsNotFound("workspace %s is not in the ledger", key)
	}
	res.Entry = cloneLedgerEntry(e)
	if e.Ownership != workspace.OwnershipOwned {
		return res, wsConflict("workspace %s is attached, so hq never removes it; unbind it (`hq workspace unbind %s`), or promote it first with an approved `hq workspace own %s`", key, key, key)
	}
	ix := loadWorkspaceTaskIndex(vaultDir)
	liveBinders := ix.liveBinders(e)
	p, err := workspace.Resolve(vaultDir, e.Provider)
	if err != nil {
		return res, err
	}
	if !p.Capabilities().Remove {
		return res, &workspace.ProviderError{Code: workspace.CodeUnsupported, Message: e.Provider + " does not support remove"}
	}
	req := workspace.RemoveRequest{WorkspaceID: e.WorkspaceID, ExpectedRoot: e.Root}
	if !force {
		if len(liveBinders) > 0 {
			return res, wsConflict("workspace %s is bound to live task(s) %s; unbind it or archive the task first", key, strings.Join(liveBinders, ", "))
		}
		info, stErr := fetchWorkspaceStatus(vaultDir, e)
		updated, _, _ := applyWorkspaceStatusCAS(vaultDir, key, e.Status.RefreshedAt, info, stErr, now)
		if updated.Provider != "" {
			res.Entry = updated
		}
		switch {
		case stErr != nil && workspace.ErrorCode(stErr) == workspace.CodeNotFound, stErr == nil && info.State == workspace.StateMissing:
			if err := forgetWorkspace(vaultDir, key, &res); err != nil {
				return res, err
			}
			return res, nil
		case stErr != nil:
			return res, fmt.Errorf("cannot verify workspace %s before removal: %w", key, stErr)
		case info.State != workspace.StateClean:
			res.NeedsApproval = true
			res.Reason = fmt.Sprintf("workspace is %s", info.State)
			return res, nil
		case !p.Capabilities().Changes:
			res.NeedsApproval = true
			res.Reason = "provider does not track open changes"
			return res, nil
		case workspace.CountOpenChanges(info.Changes) > 0:
			res.NeedsApproval = true
			res.Reason = fmt.Sprintf("%d open change(s)", workspace.CountOpenChanges(info.Changes))
			return res, nil
		}
		req.RequireClean = true
		req.RequireNoOpenChanges = true
	}
	ctx, cancel := workspaceProviderContext()
	rmErr := p.Remove(ctx, req)
	cancel()
	if rmErr != nil {
		switch workspace.ErrorCode(rmErr) {
		case workspace.CodeNotFound:
			if err := forgetWorkspace(vaultDir, key, &res); err != nil {
				return res, err
			}
			return res, nil
		case workspace.CodeDirty, workspace.CodeHasOpenChanges:
			if !force {
				res.NeedsApproval = true
				res.Reason = "provider refused: " + workspace.ErrorCode(rmErr)
				return res, nil
			}
		}
		return res, rmErr
	}
	if err := dropLedgerEntry(vaultDir, key); err != nil {
		return res, err
	}
	res.Removed = true
	res.UnboundTasks, _ = removeTaskWorkspaceBindings(vaultDir, "", key)
	return res, nil
}

func forgetWorkspace(vaultDir, key string, res *WorkspaceRemoveResult) error {
	if err := dropLedgerEntry(vaultDir, key); err != nil {
		return err
	}
	res.Forgotten = true
	res.UnboundTasks, _ = removeTaskWorkspaceBindings(vaultDir, "", key)
	return nil
}
