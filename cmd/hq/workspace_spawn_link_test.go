package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

// preToolWrite runs the PreToolUse hook for a write_file by conv and returns its decision and reason. It never
// calls t.Fatal, so it is safe from Language Server double handler goroutines.
func preToolWrite(vaultDir, conv, path string) (string, string, error) {
	payload, _ := json.Marshal(map[string]interface{}{
		"conversationId": conv,
		"toolCall":       map[string]interface{}{"name": "write_file", "args": map[string]interface{}{"Path": path, "Content": "package billing\n"}},
	})
	var out bytes.Buffer
	if err := hooks.HandlePreToolUse(bytes.NewReader(payload), &out, vaultDir); err != nil {
		return "", "", err
	}
	var resp struct {
		Decision string `json:"decision"`
		Reason   string `json:"reason"`
	}
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil {
		return "", "", fmt.Errorf("decode %s: %w", out.String(), err)
	}
	return resp.Decision, resp.Reason, nil
}

func preToolWriteDecision(t *testing.T, vaultDir, conv, path string) (string, string) {
	t.Helper()
	d, reason, err := preToolWrite(vaultDir, conv, path)
	if err != nil {
		t.Fatalf("pre-tool write_file: %v", err)
	}
	return d, reason
}

// TestWorkspaceSpawnLinksTaskBeforeFirstPrompt covers the spawn -> first-write race: the child receives its prompt
// inside SpawnTask, so a task linked only after SpawnTask returns is invisible to W1 during the child's first turn.
func TestWorkspaceSpawnLinksTaskBeforeFirstPrompt(t *testing.T) {
	env := newCLIWorkspaceEnv(t)
	prevLegacy, prevOpts := spawnTaskFn, spawnTaskWithOptionsFn
	t.Cleanup(func() { spawnTaskFn, spawnTaskWithOptionsFn = prevLegacy, prevOpts })
	req := workspaceSpawnRequest{Spec: cliFakeProviderID, Title: "[Task:ledger] t", Scope: "ledger", Tier: "standard", Model: "m", Prompt: "Fix rounding"}
	target := func(slug string) string { return filepath.Join(env.fake.rootBase, slug, "src", "billing", "round.go") }

	// Language Server path: the hook links the conversation before the prompt, so the child's first write passes W1.
	const childConv = "123e4567-e89b-42d3-a456-426614174010"
	spawnTaskFn = nil
	var hookCalls int
	var firstWrite, firstReason string
	spawnTaskWithOptionsFn = func(vaultDir, trigger, skill, title, prompt, modelID string, dryRun bool, opts kernel.SpawnTaskOptions) (string, error) {
		if opts.OnCascadeCreated == nil || trigger != "manual-spawn:ledger-fix" {
			return "", errors.New("hq spawn --workspace must pass OnCascadeCreated for its manual-spawn trigger")
		}
		opts.OnCascadeCreated(childConv) // StartCascade succeeded; the prompt has not been delivered yet
		hookCalls++
		firstWrite, firstReason = preToolWriteDecision(t, vaultDir, childConv, target("ledger-fix")) // first turn
		return childConv, nil
	}
	req.TaskSlug = "ledger-fix"
	var out, errOut bytes.Buffer
	if code := runWorkspaceSpawn(env.vaultDir, req, &out, &errOut); code != 0 {
		t.Fatalf("spawn: code=%d err=%s", code, errOut.String())
	}
	if hookCalls != 1 || firstWrite != "allow" {
		t.Fatalf("the child's first write must pass W1 once the hook linked the task: hooks=%d decision=%s reason=%s", hookCalls, firstWrite, firstReason)
	}
	if task, ok := kernel.FindTaskByConversationID(env.vaultDir, childConv); !ok || task.TaskSlug != "ledger-fix" || len(task.Workspaces) != 1 || task.Workspaces[0].Key != cliKey("ledger-fix") {
		t.Fatalf("expected ledger-fix linked to %s with its binding: %+v ok=%v", childConv, task, ok)
	}
	if d, _ := preToolWriteDecision(t, env.vaultDir, "conv-bystander", target("ledger-fix")); d != "deny" {
		t.Fatalf("a bystander must still be denied, got %s", d)
	}

	// agentapi-fallback shape: no hook call, the ID only comes back in the output and is linked afterwards.
	spawnTaskWithOptionsFn = func(string, string, string, string, string, string, bool, kernel.SpawnTaskOptions) (string, error) {
		return "Spawned conversation 123e4567-e89b-42d3-a456-426614174011", nil
	}
	req.TaskSlug = "fallback-task"
	if code := runWorkspaceSpawn(env.vaultDir, req, &out, &errOut); code != 0 {
		t.Fatalf("fallback spawn: %s", errOut.String())
	}
	if task, ok := kernel.FindTaskByConversationID(env.vaultDir, "123e4567-e89b-42d3-a456-426614174011"); !ok || task.TaskSlug != "fallback-task" {
		t.Fatalf("the fallback output must still link the task: %+v ok=%v", task, ok)
	}

	// The pre-fix ordering (link after return) is the race: an unlinked child is denied, with a pointer to hq tasks.
	var raceDecision, raceReason string
	spawnTaskFn = func(vaultDir, trigger, skill, title, prompt, modelID string, dryRun bool, force ...bool) (string, error) {
		raceDecision, raceReason = preToolWriteDecision(t, vaultDir, "123e4567-e89b-42d3-a456-426614174012", target("late-link"))
		return "Spawned conversation 123e4567-e89b-42d3-a456-426614174012", nil
	}
	req.TaskSlug = "late-link"
	if code := runWorkspaceSpawn(env.vaultDir, req, &out, &errOut); code != 0 {
		t.Fatalf("late-link spawn: %s", errOut.String())
	}
	if raceDecision != "deny" || !strings.Contains(raceReason, "does not resolve to a tracked Layer-1 task") || !strings.Contains(raceReason, "Run `hq tasks`") {
		t.Fatalf("an unlinked first write must be denied with the hq tasks hint, got %s: %s", raceDecision, raceReason)
	}
}

// TestWorkspaceSubagentSpawnUsesResolvedParent keeps the workspace owner and the spawned subagent's parent in
// agreement when the parent comes from the tool metadata rather than ANTIGRAVITY_CONVERSATION_ID.
func TestWorkspaceSubagentSpawnUsesResolvedParent(t *testing.T) {
	env := newCLIWorkspaceEnv(t)
	env.task("parent-task", "task")
	t.Setenv("ANTIGRAVITY_SOURCE_METADATA", `{"tool":{"conversationId":"conv-parent-task"}}`)
	prev := spawnSubagentFn
	t.Cleanup(func() { spawnSubagentFn = prev })
	var gotParent string
	spawnSubagentFn = func(vaultDir, parentConvID, role, prompt, modelID, workspaceDir string, dryRun bool, force ...bool) (string, error) {
		gotParent = parentConvID
		return "Spawned subagent 123e4567-e89b-42d3-a456-426614174013", nil
	}
	var out, errOut bytes.Buffer
	req := workspaceSubagentRequest{CmdLabel: "hq subagent", Spec: cliFakeProviderID, Role: "[standard] ops: Worker", TaskName: "meta-ws", Prompt: "Do it", Model: "m", WorkDir: env.rootDir}
	if code := runWorkspaceSubagentSpawn(env.vaultDir, req, &out, &errOut); code != 0 {
		t.Fatalf("subagent spawn: %s", errOut.String())
	}
	if gotParent != "conv-parent-task" {
		t.Fatalf("SpawnSubagent must receive the parent the owner was derived from, got %q", gotParent)
	}
	if e, ok := workspace.GetEntry(env.vaultDir, cliKey("meta-ws")); !ok || e.WriterTask != "parent-task" || !e.LeaseEnforced {
		t.Fatalf("expected parent-task to hold the lease: %+v ok=%v", e, ok)
	}
}

// TestWorkspaceSubagentLeaseInForceBeforeFirstPrompt runs the real kernel.SpawnSubagent against a Language Server
// double and checks, when the child's first prompt is delivered, that its first write already passes W1: the lease
// was recorded before the spawn and the Layer-1 subagent task was registered with its conversation ID before the
// prompt.
func TestWorkspaceSubagentLeaseInForceBeforeFirstPrompt(t *testing.T) {
	env := newCLIWorkspaceEnv(t)
	const deskConv = "123e4567-e89b-42d3-a456-426614174020"
	const childConv = "123e4567-e89b-42d3-a456-426614174021"
	if err := kernel.SetFrontDesk(env.vaultDir, deskConv, filepath.Join(env.rootDir, "desk-artifacts")); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}
	target := filepath.Join(env.fake.rootBase, "helper-ws", "src", "main.go")
	var mu sync.Mutex
	var atDelivery []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/GetCascadeModelConfigData"):
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"clientModelConfigs": []map[string]interface{}{
					{"modelId": "gemini-4-hydrogen-high", "modelOrAlias": map[string]interface{}{"model": "MODEL_PLACEHOLDER_TEST"}},
				},
			})
		case strings.HasSuffix(r.URL.Path, "/StartCascade"):
			_ = json.NewEncoder(w).Encode(map[string]string{"cascadeId": childConv})
		case strings.HasSuffix(r.URL.Path, "/SendUserCascadeMessage"):
			d, reason, err := preToolWrite(env.vaultDir, childConv, target)
			if err != nil {
				d = "error"
				reason = err.Error()
			}
			mu.Lock()
			atDelivery = append(atDelivery, d+": "+reason)
			mu.Unlock()
			_, _ = w.Write([]byte(`{}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", srv.URL)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-subagent-lease")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "")
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", t.TempDir())
	t.Setenv("HQ_CONFIG_DIR", t.TempDir())

	var out, errOut bytes.Buffer
	req := workspaceSubagentRequest{CmdLabel: "hq subagent", Spec: cliFakeProviderID, ParentConv: deskConv, Role: "[standard] ops: Helper", TaskName: "helper-ws", Prompt: "Do it", Model: "gemini-4-hydrogen-high", WorkDir: env.rootDir}
	if code := runWorkspaceSubagentSpawn(env.vaultDir, req, &out, &errOut); code != 0 {
		t.Fatalf("subagent spawn: code=%d err=%s", code, errOut.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(atDelivery) != 1 || !strings.HasPrefix(atDelivery[0], "allow") {
		t.Fatalf("the subagent's first write must pass W1 at prompt delivery, got %v", atDelivery)
	}
	if e, ok := workspace.GetEntry(env.vaultDir, cliKey("helper-ws")); !ok || e.WriterTask != "subagent-ops-helper" || !e.LeaseEnforced {
		t.Fatalf("expected the Layer-1 subagent task to hold the lease: %+v ok=%v", e, ok)
	}
}

func TestWorkspaceSpawnDoesNotRegisterSpuriousUUIDOnError(t *testing.T) {
	env := newCLIWorkspaceEnv(t)
	prevLegacy := spawnTaskFn
	t.Cleanup(func() { spawnTaskFn = prevLegacy })
	const spuriousUUID = "123e4567-e89b-42d3-a456-426614174099"
	spawnTaskFn = func(vaultDir, trigger, skill, title, prompt, modelID string, dryRun bool, force ...bool) (string, error) {
		return "error referencing prompt uuid " + spuriousUUID, fmt.Errorf("agentapi failed")
	}
	var out, errOut bytes.Buffer
	req := workspaceSpawnRequest{Spec: cliFakeProviderID, TaskSlug: "err-uuid-task", Title: "[Task:err] Err", Prompt: "Do it", Model: "m"}
	if code := runWorkspaceSpawn(env.vaultDir, req, &out, &errOut); code == 0 {
		t.Fatalf("expected non-zero exit code on SpawnTask error")
	}
	if _, ok := kernel.FindTaskByConversationID(env.vaultDir, spuriousUUID); ok {
		t.Fatalf("must not register spurious ConversationID %s when SpawnTask fails", spuriousUUID)
	}
}
