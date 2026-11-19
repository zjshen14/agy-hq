package hooks

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

func TestPreToolUseProtectsWorkspaceLedger(t *testing.T) {
	vaultDir := t.TempDir()
	for _, tool := range []string{"write_file", "edit_file"} {
		for _, protected := range []string{
			filepath.Join(vaultDir, "memory", "workspaces.json"),
			filepath.Join(vaultDir, "memory", ".workspaces.lock"),
		} {
			var out bytes.Buffer
			raw, _ := json.Marshal(map[string]interface{}{
				"conversationId": "conv-task-worker",
				"toolCall": map[string]interface{}{
					"name": tool,
					"args": map[string]interface{}{"Path": protected, "Content": "{}", "Target": "a", "Replacement": "b"},
				},
			})
			if err := HandlePreToolUse(bytes.NewReader(raw), &out, vaultDir); err != nil {
				t.Fatalf("HandlePreToolUse(%s %s): %v", tool, protected, err)
			}
			if !strings.Contains(out.String(), `"decision":"deny"`) || !strings.Contains(out.String(), "workspace ledgers") {
				t.Fatalf("expected %s to %s to be denied, got: %s", tool, protected, out.String())
			}
		}
	}
}

func TestStopHookRebuildPreservesTaskWorkspaceBindings(t *testing.T) {
	vaultDir := t.TempDir()
	const deskConv = "00000000-0000-0000-0000-0000000000fd"
	const taskConv = "00000000-0000-0000-0000-0000000000a1"
	if err := kernel.SetFrontDesk(vaultDir, deskConv, filepath.Join(vaultDir, "art")); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}
	binding := workspace.Binding{
		Key:      workspace.LedgerKey("acme-monorepo/client", "ledger-fix"),
		Provider: "acme-monorepo/client",
		Root:     "/srv/ws/user/ledger-fix",
		WorkDir:  "/srv/ws/user/ledger-fix/src",
		Role:     workspace.RolePrimary,
	}
	if err := kernel.RegisterTask(vaultDir, kernel.ActiveTask{
		TaskSlug:       "ledger-fix",
		Title:          "[Task:acme] Ledger fix",
		Kind:           "task",
		ConversationID: taskConv,
		Status:         "active",
		TurnCount:      3,
		Workspaces:     []workspace.Binding{binding},
	}); err != nil {
		t.Fatalf("RegisterTask: %v", err)
	}

	var out bytes.Buffer
	payload := fmt.Sprintf(`{"conversationId":%q,"executionNum":4,"terminationReason":"NO_TOOL_CALL","lastUserInput":"keep going"}`, taskConv)
	if err := HandleStop(strings.NewReader(payload), &out, vaultDir); err != nil {
		t.Fatalf("HandleStop: %v", err)
	}
	task, ok := kernel.FindTaskByConversationID(vaultDir, taskConv)
	if !ok {
		t.Fatal("task disappeared after Stop")
	}
	if task.TurnCount != 4 {
		t.Fatalf("expected the Stop hook to re-register the task (TurnCount 4), got %d", task.TurnCount)
	}
	if len(task.Workspaces) != 1 || task.Workspaces[0] != binding {
		t.Fatalf("Stop hook rebuild dropped workspace bindings: %+v", task.Workspaces)
	}
}
