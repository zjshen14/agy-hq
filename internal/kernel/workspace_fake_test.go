package kernel

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/workspace"
)

// fakeWSProviderID is a test-only built-in provider registered in this package's test binary. Its state is kept
// per Vault so tests stay isolated; workspace roots live under a per-test temp directory outside the Vault.
const fakeWSProviderID = "core/fake-test"

type fakeWSState struct {
	mu          sync.Mutex
	rootBase    string
	workspaces  map[string]workspace.Info
	preexisting map[string]bool
	statusErr   map[string]error
	removeErr   map[string]error
	noCreate    bool
	noChanges   bool
	noShared    bool
	callDelay   time.Duration
	refreshSecs int
	skill       string
	calls       []string
}

var fakeWSStates = struct {
	sync.Mutex
	byVault map[string]*fakeWSState
}{byVault: make(map[string]*fakeWSState)}

func init() {
	workspace.RegisterBuiltin(fakeWSProvider{})
}

// newFakeWS installs fake provider state for vaultDir (removed when the test ends).
func newFakeWS(t *testing.T, vaultDir string) *fakeWSState {
	t.Helper()
	base := workspace.CanonicalPath(t.TempDir())
	st := &fakeWSState{
		rootBase:    base,
		workspaces:  make(map[string]workspace.Info),
		preexisting: make(map[string]bool),
		statusErr:   make(map[string]error),
		removeErr:   make(map[string]error),
		refreshSecs: 600,
		skill:       "fake-workspace-guide",
	}
	key := filepath.Clean(vaultDir)
	fakeWSStates.Lock()
	fakeWSStates.byVault[key] = st
	fakeWSStates.Unlock()
	t.Cleanup(func() {
		fakeWSStates.Lock()
		delete(fakeWSStates.byVault, key)
		fakeWSStates.Unlock()
	})
	return st
}

func (s *fakeWSState) info(id string, state workspace.State, changes ...workspace.ChangeRef) workspace.Info {
	root := filepath.Join(s.rootBase, id)
	return workspace.Info{
		Provider:    fakeWSProviderID,
		WorkspaceID: id,
		Name:        id,
		Root:        root,
		WorkDir:     filepath.Join(root, "src"),
		Flavor:      "fake",
		Kind:        "primary",
		State:       state,
		Changes:     changes,
	}
}

// set records (or replaces) workspace id with the given state and changes.
func (s *fakeWSState) set(id string, state workspace.State, changes ...workspace.ChangeRef) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.workspaces[id] = s.info(id, state, changes...)
}

func (s *fakeWSState) exists(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.workspaces[id]
	return ok
}

func (s *fakeWSState) record(call string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, call)
}

func (s *fakeWSState) callsWith(prefix string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, c := range s.calls {
		if strings.HasPrefix(c, prefix) {
			out = append(out, c)
		}
	}
	return out
}

func (s *fakeWSState) resetCalls() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = nil
}

type fakeWSProvider struct{ vaultDir string }

func (fakeWSProvider) ID() string { return fakeWSProviderID }

func (p fakeWSProvider) ForVault(vaultDir string) workspace.Provider {
	return fakeWSProvider{vaultDir: vaultDir}
}

func (p fakeWSProvider) state() *fakeWSState {
	fakeWSStates.Lock()
	defer fakeWSStates.Unlock()
	return fakeWSStates.byVault[filepath.Clean(p.vaultDir)]
}

func (p fakeWSProvider) Capabilities() workspace.Capabilities {
	st := p.state()
	return workspace.Capabilities{
		Create:        st == nil || !st.noCreate,
		Remove:        true,
		Changes:       st == nil || !st.noChanges,
		SharedHistory: st == nil || !st.noShared,
	}
}

func (p fakeWSProvider) StatusRefreshSeconds() int {
	if st := p.state(); st != nil {
		return st.refreshSecs
	}
	return 0
}

func (p fakeWSProvider) Skill() string {
	if st := p.state(); st != nil {
		return st.skill
	}
	return ""
}

func (p fakeWSProvider) Probe(ctx context.Context, path string) (workspace.Info, bool, error) {
	st := p.state()
	if st == nil {
		return workspace.Info{}, false, &workspace.ProviderError{Code: workspace.CodeUnavailable, Message: "no fake state"}
	}
	if d := st.callDelay; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return workspace.Info{}, false, ctx.Err()
		}
	}
	st.record("probe:" + path)
	rel, err := filepath.Rel(st.rootBase, path)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return workspace.Info{}, false, nil
	}
	id := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	st.mu.Lock()
	defer st.mu.Unlock()
	if info, ok := st.workspaces[id]; ok {
		info.State = workspace.StateUnknown
		info.Changes = nil
		return info, true, nil
	}
	return workspace.Info{}, false, nil
}

func (p fakeWSProvider) List(context.Context, workspace.ListRequest) ([]workspace.Info, string, error) {
	st := p.state()
	if st == nil {
		return nil, "", nil
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	ids := make([]string, 0, len(st.workspaces))
	for id := range st.workspaces {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]workspace.Info, 0, len(ids))
	for _, id := range ids {
		info := st.workspaces[id]
		info.Changes = nil
		out = append(out, info)
	}
	return out, "", nil
}

func (p fakeWSProvider) Create(_ context.Context, req workspace.CreateRequest) (workspace.CreateResult, error) {
	st := p.state()
	if st == nil {
		return workspace.CreateResult{}, &workspace.ProviderError{Code: workspace.CodeUnavailable, Message: "no fake state"}
	}
	st.record("create:" + req.Name + ":" + req.IdempotencyKey)
	if req.ShareFrom != "" {
		st.record("share:" + req.Name + "<-" + req.ShareFrom)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if info, ok := st.workspaces[req.Name]; ok {
		return workspace.CreateResult{Info: info, Existed: true}, nil
	}
	info := st.info(req.Name, workspace.StateClean)
	st.workspaces[req.Name] = info
	return workspace.CreateResult{Info: info, Existed: st.preexisting[req.Name]}, nil
}

func (p fakeWSProvider) Status(ctx context.Context, id string) (workspace.Info, error) {
	st := p.state()
	if st == nil {
		return workspace.Info{}, &workspace.ProviderError{Code: workspace.CodeUnavailable, Message: "no fake state"}
	}
	if d := st.callDelay; d > 0 {
		select {
		case <-time.After(d):
		case <-ctx.Done():
			return workspace.Info{}, ctx.Err()
		}
	}
	st.record("status:" + id)
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.statusErr[id]; err != nil {
		return workspace.Info{}, err
	}
	info, ok := st.workspaces[id]
	if !ok {
		return workspace.Info{}, &workspace.ProviderError{Code: workspace.CodeNotFound, Message: "no such workspace"}
	}
	info.Changes = append([]workspace.ChangeRef(nil), info.Changes...)
	return info, nil
}

func (p fakeWSProvider) Remove(_ context.Context, req workspace.RemoveRequest) error {
	st := p.state()
	if st == nil {
		return &workspace.ProviderError{Code: workspace.CodeUnavailable, Message: "no fake state"}
	}
	st.record(fmt.Sprintf("remove:%s:clean=%v:open=%v", req.WorkspaceID, req.RequireClean, req.RequireNoOpenChanges))
	st.mu.Lock()
	defer st.mu.Unlock()
	if err := st.removeErr[req.WorkspaceID]; err != nil {
		return err
	}
	info, ok := st.workspaces[req.WorkspaceID]
	if !ok {
		return &workspace.ProviderError{Code: workspace.CodeNotFound, Message: "no such workspace"}
	}
	if req.ExpectedRoot != "" && req.ExpectedRoot != info.Root {
		return &workspace.ProviderError{Code: workspace.CodeConflict, Message: "expected_root mismatch"}
	}
	if req.RequireClean && info.State != workspace.StateClean {
		return &workspace.ProviderError{Code: workspace.CodeDirty, Message: "dirty"}
	}
	if req.RequireNoOpenChanges && workspace.CountOpenChanges(info.Changes) > 0 {
		return &workspace.ProviderError{Code: workspace.CodeHasOpenChanges, Message: "open changes"}
	}
	delete(st.workspaces, req.WorkspaceID)
	return nil
}

// ---------------------------------------------------------------------------------------------------------------
// shared helpers

func fakeKey(id string) string { return workspace.LedgerKey(fakeWSProviderID, id) }

func landed(id string) workspace.ChangeRef {
	return workspace.ChangeRef{ID: id, State: workspace.ChangeLanded}
}

func inReview(id string, needsOwner bool) workspace.ChangeRef {
	return workspace.ChangeRef{ID: id, State: workspace.ChangeInReview, NeedsOwner: needsOwner}
}

func newWorkspaceTestVault(t *testing.T) string {
	t.Helper()
	vaultDir := filepath.Join(workspace.CanonicalPath(t.TempDir()), "vault")
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTIGRAVITY_CONVERSATION_ID", "")
	return vaultDir
}

func registerLiveTask(t *testing.T, vaultDir, slug, kind string, bindings ...workspace.Binding) {
	t.Helper()
	if kind == "" {
		kind = "task"
	}
	now := time.Now().Format(time.RFC3339)
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       slug,
		Title:          "[Task:test] " + slug,
		Kind:           kind,
		ConversationID: "conv-" + slug,
		Status:         "active",
		CreatedAt:      now,
		LastUpdatedAt:  now,
		Workspaces:     bindings,
	}); err != nil {
		t.Fatalf("RegisterTask(%s): %v", slug, err)
	}
}

func mustTask(t *testing.T, vaultDir, slug string) ActiveTask {
	t.Helper()
	tasks, err := ListTasks(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tk := range tasks {
		if tk.TaskSlug == slug {
			return tk
		}
	}
	t.Fatalf("task %s not found", slug)
	return ActiveTask{}
}

func mustEntry(t *testing.T, vaultDir, key string) workspace.LedgerEntry {
	t.Helper()
	e, ok := workspace.GetEntry(vaultDir, key)
	if !ok {
		t.Fatalf("ledger entry %s missing", key)
	}
	return e
}

func setLedgerEntry(t *testing.T, vaultDir, key string, fn func(e *workspace.LedgerEntry)) {
	t.Helper()
	if err := workspace.UpdateLedger(vaultDir, func(l *workspace.Ledger) (bool, error) {
		e, ok := l.Entries[key]
		if !ok {
			return false, fmt.Errorf("entry %s missing", key)
		}
		fn(&e)
		l.Put(e)
		return true, nil
	}); err != nil {
		t.Fatalf("set ledger entry %s: %v", key, err)
	}
}

// acquireForTask creates an owned fake workspace for slug and registers slug as a live task bound to it, the way
// `hq spawn --workspace` does.
func acquireForTask(t *testing.T, vaultDir, slug string) WorkspaceAcquireResult {
	t.Helper()
	acq, err := AcquireWorkspace(vaultDir, WorkspaceAcquireOptions{
		Provider:      fakeWSProviderID,
		Name:          slug,
		TaskSlug:      slug,
		Role:          workspace.RolePrimary,
		LeaseEnforced: true,
	})
	if err != nil {
		t.Fatalf("AcquireWorkspace(%s): %v", slug, err)
	}
	registerLiveTask(t, vaultDir, slug, "task", acq.Binding)
	return acq
}
