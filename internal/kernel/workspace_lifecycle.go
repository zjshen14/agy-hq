package kernel

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/zjshen14/agy-hq/internal/outbox"
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
	// maxNotifiedLanded bounds LedgerEntry.NotifiedLanded (oldest change IDs are dropped first).
	maxNotifiedLanded = 64
	// workspaceProviderCallTimeout bounds one provider call made by the lifecycle layer (the external adapter
	// enforces its own, shorter, per-manifest timeout).
	workspaceProviderCallTimeout = workspace.MaxProviderTimeout + 5*time.Second
	// workspaceUncachedMaxAge is the cache freshness bound used when a provider disables daemon refresh.
	workspaceUncachedMaxAge = time.Hour
	// WorkspaceLandedSourceSkill attributes landed-change outbox items.
	WorkspaceLandedSourceSkill = "workspace-lifecycle"
	// DefaultOrphanWorkspaceMinAge keeps freshly created orphan workspaces out of PruneOrphanWorkspaces.
	DefaultOrphanWorkspaceMinAge = time.Hour
)

// matchWorkspaceRootPattern is workspace.MatchRootPattern. Tests substitute it to route paths to fake built-in
// providers (built-ins declare no root_patterns).
var matchWorkspaceRootPattern = workspace.MatchRootPattern

func workspaceProviderContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), workspaceProviderCallTimeout)
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

// wsPathWithin reports whether path equals root or sits beneath it (whole path segments, cleaned inputs).
func wsPathWithin(path, root string) bool {
	path, root = filepath.Clean(path), filepath.Clean(root)
	if path == "" || root == "" || path == "." || root == "." {
		return false
	}
	if path == root {
		return true
	}
	if root == string(filepath.Separator) {
		return strings.HasPrefix(path, root)
	}
	return strings.HasPrefix(path, root+string(filepath.Separator))
}

func wsPathsOverlap(a, b string) bool {
	return wsPathWithin(a, b) || wsPathWithin(b, a)
}

func wsHasControlRune(s string) bool {
	for _, r := range s {
		if r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return true
		}
	}
	return false
}

// validateAcquiredWorkspaceInfo checks what a provider returned before it is ledgered: a well-formed ID, a clean
// absolute root that neither is "/" nor overlaps the Vault, and a work directory inside the root.
func validateAcquiredWorkspaceInfo(vaultDir string, info workspace.Info) error {
	if !workspace.ValidWorkspaceID(info.WorkspaceID) {
		return wsInvalid("provider returned invalid workspace_id %q", clipWorkspaceText(info.WorkspaceID, 80))
	}
	root := info.Root
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || root == string(filepath.Separator) || wsHasControlRune(root) {
		return wsInvalid("provider returned invalid workspace root %q", clipWorkspaceText(root, 120))
	}
	if info.WorkDir != "" && (!filepath.IsAbs(info.WorkDir) || filepath.Clean(info.WorkDir) != info.WorkDir || !wsPathWithin(info.WorkDir, root) || wsHasControlRune(info.WorkDir)) {
		return wsInvalid("provider returned work_dir %q outside workspace root %q", clipWorkspaceText(info.WorkDir, 120), root)
	}
	for _, v := range []string{filepath.Clean(vaultDir), workspace.CanonicalPath(vaultDir)} {
		if v != "" && v != "." && (wsPathsOverlap(root, v) || wsPathsOverlap(workspace.CanonicalPath(root), v)) {
			return wsConflict("workspace root %s overlaps the Vault; provider workspaces must live outside it", root)
		}
	}
	return nil
}

// clipWorkspaceText keeps provider-controlled or error text single-line, free of control characters, and short.
func clipWorkspaceText(s string, maxRunes int) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteRune(' ')
		case r == unicode.ReplacementChar || unicode.IsControl(r) || unicode.Is(unicode.Cf, r):
			continue
		default:
			b.WriteRune(r)
		}
	}
	return outbox.SanitizeAntiCue(strings.Join(strings.Fields(b.String()), " "), maxRunes)
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

// openWorkspaceChanges counts changes that still hold unlanded work.
func openWorkspaceChanges(changes []workspace.ChangeRef) int {
	n := 0
	for _, c := range changes {
		if c.State.Open() {
			n++
		}
	}
	return n
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

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
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
// $ANTIGRAVITY_CONVERSATION_ID) to its live Layer-1 owning task.
func resolveWorkspaceTask(vaultDir, target string) (ActiveTask, error) {
	t, ok := ResolveOwningTask(vaultDir, target)
	if !ok {
		if strings.TrimSpace(target) == "" {
			return ActiveTask{}, wsNotFound("no owning task for this conversation (pass --task=<slug-or-conv>)")
		}
		return ActiveTask{}, wsNotFound("task %q not found (or it is the Front Desk)", target)
	}
	if !isLiveTaskStatus(t) {
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
			return WorkspaceAcquireResult{}, wsConflict("provider returned workspace %q for requested %q", clipWorkspaceText(st.WorkspaceID, 80), attachID)
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
	if err := validateAcquiredWorkspaceInfo(vaultDir, info); err != nil {
		return WorkspaceAcquireResult{}, err
	}
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
			result.PrevBound = containsString(e.BoundTasks, slug)
			result.PrevOrphaned = e.OrphanedOpenChanges
			if filepath.Clean(e.Root) != info.Root {
				return false, wsConflict("ledger entry %s has root %s but the provider reported %s", key, e.Root, info.Root)
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
				Provider:    fqid,
				WorkspaceID: info.WorkspaceID,
				Name:        info.Name,
				Root:        info.Root,
				WorkDir:     info.WorkDir,
				Ownership:   workspace.OwnershipAttached,
				CreatedAt:   nowStr,
				Status:      ledgerStatusFromInfo(info, nowStr),
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
		return "", wsInvalid("share_from %q must be a ledger key (<provider-fqid>:<workspace-id>) or a workspace id", clipWorkspaceText(raw, 80))
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
	if raw == "" || !filepath.IsAbs(raw) || wsHasControlRune(raw) {
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
	t, err := resolveWorkspaceTask(vaultDir, toTarget)
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
	t, ok := ResolveOwningTask(vaultDir, taskTarget)
	if !ok {
		if strings.TrimSpace(taskTarget) == "" {
			return "", wsNotFound("no owning task for this conversation (pass --task=<slug-or-conv>)")
		}
		return "", wsNotFound("task %q not found (or it is the Front Desk)", taskTarget)
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
		summary += " (" + clipWorkspaceText(strings.ReplaceAll(strings.ReplaceAll(r, `"`, ""), "'", ""), 80) + ")"
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
		case openWorkspaceChanges(info.Changes) > 0:
			res.NeedsApproval = true
			res.Reason = fmt.Sprintf("%d open change(s)", openWorkspaceChanges(info.Changes))
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

// ---------------------------------------------------------------------------------------------------------------
// Status refresh (compare-and-set) and landed notifications

// landedNotice is one (ledger key, change id) transition to landed, emitted once as a brief_rollup outbox item.
type landedNotice struct {
	Key       string
	ChangeID  string
	Task      string
	Owned     bool
	OpenCount int
}

// applyWorkspaceInfo writes a fresh provider status into e's cache and returns the change IDs that transitioned
// to landed since the previous successful refresh and were not notified before (recorded in notified_landed).
// A provider reporting a different root than the ledger is treated as an error: the cache keeps its last state.
func applyWorkspaceInfo(e *workspace.LedgerEntry, info workspace.Info, nowStr string) []string {
	if filepath.Clean(info.Root) != filepath.Clean(e.Root) {
		e.Status.Error = clipWorkspaceText(fmt.Sprintf("provider reported root %s but the ledger has %s", info.Root, e.Root), 200)
		e.Status.RefreshedAt = nowStr
		return nil
	}
	prev := e.Status
	e.Status = ledgerStatusFromInfo(info, nowStr)
	if e.OrphanedOpenChanges && openWorkspaceChanges(e.Status.Changes) == 0 {
		e.OrphanedOpenChanges = false // every change kept it alive has landed or been abandoned
	}
	if prev.RefreshedAt == "" {
		return nil // first observation is the baseline, not a transition
	}
	prevState := make(map[string]workspace.ChangeState, len(prev.Changes))
	for _, c := range prev.Changes {
		prevState[c.ID] = c.State
	}
	var landed []string
	for _, c := range e.Status.Changes {
		if c.State != workspace.ChangeLanded || prevState[c.ID] == workspace.ChangeLanded || containsString(e.NotifiedLanded, c.ID) {
			continue
		}
		e.NotifiedLanded = append(e.NotifiedLanded, c.ID)
		landed = append(landed, c.ID)
	}
	if n := len(e.NotifiedLanded); n > maxNotifiedLanded {
		e.NotifiedLanded = append([]string(nil), e.NotifiedLanded[n-maxNotifiedLanded:]...)
	}
	return landed
}

func buildLandedNotices(e workspace.LedgerEntry, ids []string) []landedNotice {
	if len(ids) == 0 {
		return nil
	}
	task := e.WriterTask
	if task == "" && len(e.BoundTasks) > 0 {
		task = e.BoundTasks[0]
	}
	if task == "" {
		task = e.CreatedByTask
	}
	out := make([]landedNotice, 0, len(ids))
	for _, id := range ids {
		out = append(out, landedNotice{
			Key:       e.Key(),
			ChangeID:  id,
			Task:      task,
			Owned:     e.Ownership == workspace.OwnershipOwned,
			OpenCount: openWorkspaceChanges(e.Status.Changes),
		})
	}
	return out
}

// emitLandedNotices adds one brief_rollup outbox item per notice. Only opaque IDs, enums, and counts are included
// (never provider-authored titles), because brief items reach LLM context. Delivery is at-most-once: the
// notified_landed mark was committed under the ledger lock before this runs.
func emitLandedNotices(vaultDir string, notices []landedNotice, now time.Time) int {
	sent := 0
	for _, n := range notices {
		scope := n.Task
		if scope == "" {
			scope = "workspaces"
		}
		summary := fmt.Sprintf("Change %s in workspace %s landed.", n.ChangeID, n.Key)
		switch {
		case n.OpenCount == 0 && n.Owned:
			summary += fmt.Sprintf(" No open changes remain; the workspace is eligible for cleanup (archive its task, or `hq workspace remove %s`).", n.Key)
		case n.OpenCount == 0:
			summary += " No open changes remain (attached workspace: hq only unbinds it)."
		default:
			summary += fmt.Sprintf(" %d change(s) are still open.", n.OpenCount)
		}
		if _, _, err := outbox.Add(vaultDir, outbox.OutboxItem{
			SourceSkill:   WorkspaceLandedSourceSkill,
			Scope:         scope,
			Tier:          outbox.TierBriefRollup,
			Title:         fmt.Sprintf("Workspace change landed: %s %s", n.Key, n.ChangeID),
			Summary:       summary,
			SuggestedSlug: n.Task,
		}, now); err == nil {
			sent++
		}
	}
	return sent
}

// fetchWorkspaceStatus resolves the entry's provider and asks it for a fresh status (outside every lock).
func fetchWorkspaceStatus(vaultDir string, e workspace.LedgerEntry) (workspace.Info, error) {
	p, err := workspace.Resolve(vaultDir, e.Provider)
	if err != nil {
		return workspace.Info{}, err
	}
	ctx, cancel := workspaceProviderContext()
	defer cancel()
	info, err := p.Status(ctx, e.WorkspaceID)
	if err != nil {
		return workspace.Info{}, err
	}
	info.Provider = e.Provider
	return info, nil
}

// applyWorkspaceStatusCAS applies a provider status result (or error) to the cache of entry key only when
// status.refreshed_at still equals expectRefreshedAt (a concurrent refresh wins otherwise). A not_found error
// records the workspace as missing. Landed transitions are emitted after the ledger lock is released. It returns
// the entry as stored after the attempt and whether this result was applied.
func applyWorkspaceStatusCAS(vaultDir, key, expectRefreshedAt string, info workspace.Info, callErr error, now time.Time) (workspace.LedgerEntry, bool, error) {
	e, applied, _, err := applyWorkspaceStatusCASCounted(vaultDir, key, expectRefreshedAt, info, callErr, now)
	return e, applied, err
}

// applyWorkspaceStatusCASCounted is applyWorkspaceStatusCAS that also reports how many landed notices it emitted.
func applyWorkspaceStatusCASCounted(vaultDir, key, expectRefreshedAt string, info workspace.Info, callErr error, now time.Time) (workspace.LedgerEntry, bool, int, error) {
	nowStr := now.UTC().Format(time.RFC3339)
	var out workspace.LedgerEntry
	var notices []landedNotice
	applied := false
	err := workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		out, notices, applied = workspace.LedgerEntry{}, nil, false
		e, ok := l.Entries[key]
		if !ok {
			return false, nil
		}
		out = cloneLedgerEntry(e)
		if e.Status.RefreshedAt != expectRefreshedAt {
			return false, nil
		}
		switch {
		case callErr != nil && workspace.ErrorCode(callErr) == workspace.CodeNotFound:
			e.Status = workspace.LedgerStatus{State: workspace.StateMissing, RefreshedAt: nowStr}
		case callErr != nil:
			e.Status.Error = clipWorkspaceText(callErr.Error(), 200)
			if e.Status.Error == "" {
				e.Status.Error = "status refresh failed"
			}
			e.Status.RefreshedAt = nowStr
		default:
			notices = buildLandedNotices(e, applyWorkspaceInfo(&e, info, nowStr))
		}
		l.Put(e)
		out = cloneLedgerEntry(e)
		applied = true
		return true, nil
	})
	if err != nil {
		return out, false, 0, err
	}
	sent := emitLandedNotices(vaultDir, notices, now)
	return out, applied, sent, nil
}

// RefreshWorkspaceStatus asks the provider for a fresh status of the ledger entry key and stores it in the
// status cache (compare-and-set on refreshed_at). Provider errors are recorded in status.error and returned.
func RefreshWorkspaceStatus(vaultDir, key string, now time.Time) (workspace.LedgerEntry, error) {
	if now.IsZero() {
		now = time.Now()
	}
	e, ok := workspace.GetEntry(vaultDir, key)
	if !ok {
		return workspace.LedgerEntry{}, wsNotFound("workspace %s is not in the ledger", key)
	}
	info, callErr := fetchWorkspaceStatus(vaultDir, e)
	updated, _, err := applyWorkspaceStatusCAS(vaultDir, key, e.Status.RefreshedAt, info, callErr, now)
	if err != nil {
		return updated, err
	}
	if callErr != nil && workspace.ErrorCode(callErr) != workspace.CodeNotFound {
		return updated, callErr
	}
	return updated, nil
}

// WorkspaceRefreshResult is one entry's outcome from RefreshWorkspaces.
type WorkspaceRefreshResult struct {
	Key   string
	Entry workspace.LedgerEntry
	Err   error
}

// RefreshWorkspaces refreshes the given ledger keys with at most concurrency provider calls in flight.
func RefreshWorkspaces(vaultDir string, keys []string, concurrency int, now time.Time) []WorkspaceRefreshResult {
	if concurrency <= 0 {
		concurrency = 4
	}
	results := make([]WorkspaceRefreshResult, len(keys))
	sem := make(chan struct{}, concurrency)
	done := make(chan struct{})
	for i, key := range keys {
		go func(i int, key string) {
			sem <- struct{}{}
			defer func() {
				<-sem
				done <- struct{}{}
			}()
			e, err := RefreshWorkspaceStatus(vaultDir, key, now)
			results[i] = WorkspaceRefreshResult{Key: key, Entry: e, Err: err}
		}(i, key)
	}
	for range keys {
		<-done
	}
	return results
}

// ---------------------------------------------------------------------------------------------------------------
// Cache freshness and eligibility (cache-only; used by compaction)

// WorkspaceCacheUsable reports whether e's cached status is trustworthy for automated decisions: refreshed,
// not errored, and fresher than 2 x refreshSeconds (or 1h when daemon refresh is disabled). Fail-closed.
func WorkspaceCacheUsable(e workspace.LedgerEntry, refreshSeconds int, now time.Time) (bool, string) {
	if strings.TrimSpace(e.Status.Error) != "" {
		return false, "status errored"
	}
	ts, err := time.Parse(time.RFC3339, strings.TrimSpace(e.Status.RefreshedAt))
	if err != nil {
		return false, "status never refreshed"
	}
	maxAge := workspaceUncachedMaxAge
	if refreshSeconds > 0 {
		maxAge = 2 * time.Duration(refreshSeconds) * time.Second
	}
	if now.Sub(ts) > maxAge {
		return false, "status cache stale"
	}
	return true, ""
}

// WorkspaceDormantEligible reports whether e lets a dormant task auto-archive: a usable cache, clean state, and
// every change landed or abandoned (RFC-0025 §8).
func WorkspaceDormantEligible(e workspace.LedgerEntry, refreshSeconds int, now time.Time) (bool, string) {
	if ok, why := WorkspaceCacheUsable(e, refreshSeconds, now); !ok {
		return false, why
	}
	if e.Status.State != workspace.StateClean {
		return false, "workspace " + string(e.Status.State)
	}
	if n := openWorkspaceChanges(e.Status.Changes); n > 0 {
		return false, fmt.Sprintf("%d open change(s)", n)
	}
	return true, ""
}

// ---------------------------------------------------------------------------------------------------------------
// Inventory and summaries (cache-only render helpers)

// WorkspaceChangeView is a display-safe change summary (opaque ID and enums only; never provider titles).
type WorkspaceChangeView struct {
	ID         string                `json:"id"`
	State      workspace.ChangeState `json:"state"`
	NeedsOwner bool                  `json:"needs_owner,omitempty"`
}

// WorkspaceInventoryRow is one row of `hq workspace list` / `status`.
type WorkspaceInventoryRow struct {
	Source              string                `json:"source"` // ledger | git | provider-list
	Provider            string                `json:"provider"`
	Key                 string                `json:"key"`
	WorkspaceID         string                `json:"workspace_id"`
	Name                string                `json:"name,omitempty"`
	Root                string                `json:"root"`
	WorkDir             string                `json:"work_dir,omitempty"`
	Ownership           string                `json:"ownership"` // owned | attached | git | unledgered
	WriterTask          string                `json:"writer_task,omitempty"`
	LeaseEnforced       bool                  `json:"lease_enforced,omitempty"`
	BoundTasks          []string              `json:"bound_tasks,omitempty"`
	State               workspace.State       `json:"state"`
	OpenChanges         int                   `json:"open_changes"`
	NeedsOwner          int                   `json:"needs_owner"`
	Changes             []WorkspaceChangeView `json:"changes,omitempty"`
	RefreshedAt         string                `json:"refreshed_at,omitempty"`
	Error               string                `json:"error,omitempty"`
	OrphanedOpenChanges bool                  `json:"orphaned_open_changes,omitempty"`
}

func changeViews(changes []workspace.ChangeRef) []WorkspaceChangeView {
	var out []WorkspaceChangeView
	for _, c := range changes {
		out = append(out, WorkspaceChangeView{ID: c.ID, State: c.State, NeedsOwner: c.NeedsOwner})
	}
	return out
}

// WorkspaceRowFromEntry renders a ledger entry as an inventory row.
func WorkspaceRowFromEntry(e workspace.LedgerEntry) WorkspaceInventoryRow {
	return WorkspaceInventoryRow{
		Source:              "ledger",
		Provider:            e.Provider,
		Key:                 e.Key(),
		WorkspaceID:         e.WorkspaceID,
		Name:                e.Name,
		Root:                e.Root,
		WorkDir:             e.WorkDir,
		Ownership:           string(e.Ownership),
		WriterTask:          e.WriterTask,
		LeaseEnforced:       e.LeaseEnforced,
		BoundTasks:          append([]string(nil), e.BoundTasks...),
		State:               e.Status.State,
		OpenChanges:         openWorkspaceChanges(e.Status.Changes),
		NeedsOwner:          needsOwnerWorkspaceChanges(e.Status.Changes),
		Changes:             changeViews(e.Status.Changes),
		RefreshedAt:         e.Status.RefreshedAt,
		Error:               e.Status.Error,
		OrphanedOpenChanges: e.OrphanedOpenChanges,
	}
}

// WorkspaceInventoryOptions configures WorkspaceInventory.
type WorkspaceInventoryOptions struct {
	Provider   string // only this provider FQID ("" = all)
	All        bool   // also page through provider `list` for workspaces missing from the ledger
	IncludeGit bool   // include git worktrees mapped from InspectWorktrees
	Now        time.Time
}

const maxInventoryListPages = 10

// WorkspaceInventory merges ledger entries, git worktrees (InspectWorktrees), and optionally unledgered provider
// workspaces paged from `list`. Provider failures during --all paging are returned as warnings.
func WorkspaceInventory(vaultDir string, opts WorkspaceInventoryOptions) ([]WorkspaceInventoryRow, []string, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	filter := strings.TrimSpace(opts.Provider)
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil {
		return nil, nil, err
	}
	var rows []WorkspaceInventoryRow
	seen := make(map[string]bool)
	keys := make([]string, 0, len(l.Entries))
	for k := range l.Entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		e := l.Entries[k]
		if filter != "" && e.Provider != filter {
			continue
		}
		rows = append(rows, WorkspaceRowFromEntry(e))
		seen[k] = true
	}
	if opts.IncludeGit && (filter == "" || filter == workspace.CoreGitProviderID) {
		statuses, gErr := InspectWorktrees(vaultDir, now)
		if gErr == nil {
			for _, st := range statuses {
				info := gitWorktreeInfo(st)
				row := WorkspaceInventoryRow{
					Source:      "git",
					Provider:    workspace.CoreGitProviderID,
					Key:         workspace.LedgerKey(workspace.CoreGitProviderID, info.WorkspaceID),
					WorkspaceID: info.WorkspaceID,
					Name:        info.Name,
					Root:        info.Root,
					WorkDir:     info.WorkDir,
					Ownership:   "git",
					WriterTask:  st.OwnerTaskSlug,
					State:       info.State,
					OpenChanges: openWorkspaceChanges(info.Changes),
					Changes:     changeViews(info.Changes),
				}
				if st.OwnerTaskSlug != "" {
					row.BoundTasks = []string{st.OwnerTaskSlug}
				}
				rows = append(rows, row)
			}
		}
	}
	var warnings []string
	if opts.All {
		for _, ps := range workspace.ListProviders(vaultDir) {
			if ps.FQID == workspace.CoreGitProviderID || (filter != "" && ps.FQID != filter) {
				continue
			}
			if !ps.Trusted {
				warnings = append(warnings, fmt.Sprintf("%s disabled: %s", ps.FQID, ps.Reason))
				continue
			}
			p, rErr := workspace.Resolve(vaultDir, ps.FQID)
			if rErr != nil {
				warnings = append(warnings, fmt.Sprintf("%s: %v", ps.FQID, rErr))
				continue
			}
			cursor := ""
			for page := 0; page < maxInventoryListPages; page++ {
				ctx, cancel := workspaceProviderContext()
				infos, next, lErr := p.List(ctx, workspace.ListRequest{Cursor: cursor})
				cancel()
				if lErr != nil {
					warnings = append(warnings, fmt.Sprintf("%s list: %s", ps.FQID, clipWorkspaceText(lErr.Error(), 200)))
					break
				}
				for _, info := range infos {
					key := workspace.LedgerKey(ps.FQID, info.WorkspaceID)
					if seen[key] {
						continue
					}
					seen[key] = true
					rows = append(rows, WorkspaceInventoryRow{
						Source:      "provider-list",
						Provider:    ps.FQID,
						Key:         key,
						WorkspaceID: info.WorkspaceID,
						Name:        info.Name,
						Root:        info.Root,
						WorkDir:     info.WorkDir,
						Ownership:   "unledgered",
						State:       info.State,
					})
				}
				if next == "" {
					break
				}
				cursor = next
				if page == maxInventoryListPages-1 {
					warnings = append(warnings, fmt.Sprintf("%s list truncated after %d pages", ps.FQID, maxInventoryListPages))
				}
			}
		}
	}
	return rows, warnings, nil
}

// WorkspaceSummary aggregates the ledger cache for `hq status`.
type WorkspaceSummary struct {
	Total       int            `json:"total"`
	Git         int            `json:"git"`
	ByProvider  map[string]int `json:"by_provider"`
	OpenChanges int            `json:"open_changes"`
	NeedsOwner  int            `json:"needs_owner"`
	Orphaned    int            `json:"orphaned"`
}

// SummarizeWorkspaces aggregates the provider ledger (cache only, no provider calls) plus gitCount git worktrees.
// It returns false when the ledger has no entries, so callers keep their pre-RFC-0025 output.
func SummarizeWorkspaces(vaultDir string, gitCount int) (WorkspaceSummary, bool) {
	l, err := workspace.LoadLedger(vaultDir)
	if err != nil || len(l.Entries) == 0 {
		return WorkspaceSummary{}, false
	}
	s := WorkspaceSummary{Git: gitCount, ByProvider: make(map[string]int)}
	for _, e := range l.Entries {
		s.ByProvider[e.Provider]++
		s.OpenChanges += openWorkspaceChanges(e.Status.Changes)
		s.NeedsOwner += needsOwnerWorkspaceChanges(e.Status.Changes)
		if e.OrphanedOpenChanges {
			s.Orphaned++
		}
	}
	s.Total = gitCount + len(l.Entries)
	return s, true
}

// StatusLine renders `Workspaces: N (git: a, <provider>: b) · open changes: c · needs you: d`.
func (s WorkspaceSummary) StatusLine() string {
	parts := []string{fmt.Sprintf("git: %d", s.Git)}
	providers := make([]string, 0, len(s.ByProvider))
	for p := range s.ByProvider {
		providers = append(providers, p)
	}
	sort.Strings(providers)
	for _, p := range providers {
		parts = append(parts, fmt.Sprintf("%s: %d", p, s.ByProvider[p]))
	}
	return fmt.Sprintf("Workspaces: %d (%s) · open changes: %d · needs you: %d", s.Total, strings.Join(parts, ", "), s.OpenChanges, s.NeedsOwner)
}
