package kernel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeSpawnLS is a minimal Language Server double for SpawnTaskWithOptions: it records the order of StartCascade
// and SendUserCascadeMessage calls and lets the test observe Vault state at prompt-delivery time.
type fakeSpawnLS struct {
	mu        sync.Mutex
	cascadeID string
	events    []string
	onSend    func(cascadeID string) string // optional; its result is appended to events as "send:<result>"
}

func (f *fakeSpawnLS) record(ev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakeSpawnLS) snapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func (f *fakeSpawnLS) handler(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/GetCascadeModelConfigData"):
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"clientModelConfigs": []map[string]interface{}{
				{"modelId": "gemini-4-hydrogen-high", "modelOrAlias": map[string]interface{}{"model": "MODEL_PLACEHOLDER_TEST"}},
			},
		})
	case strings.HasSuffix(r.URL.Path, "/StartCascade"):
		f.record("start")
		_ = json.NewEncoder(w).Encode(map[string]string{"cascadeId": f.cascadeID})
	case strings.HasSuffix(r.URL.Path, "/SendUserCascadeMessage"):
		ev := "send"
		if f.onSend != nil {
			ev += ":" + f.onSend(f.cascadeID)
		}
		f.record(ev)
		_, _ = w.Write([]byte(`{}`))
	default:
		http.NotFound(w, r)
	}
}

func newSpawnOptionsTestVault(t *testing.T, ls *fakeSpawnLS) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(ls.handler))
	t.Cleanup(srv.Close)
	vaultDir := filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", srv.URL)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-spawn-options")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "")
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", t.TempDir())
	t.Setenv("HQ_CONFIG_DIR", t.TempDir())
	return vaultDir
}

// TestSpawnTaskWithOptionsHooksCascadeBeforeFirstPrompt pins the RFC-0025 spawn race fix: OnCascadeCreated runs
// after StartCascade and before SendUserCascadeMessage, so state linked from the hook (the pre-registered task's
// ConversationID) is visible when the child receives its first prompt.
func TestSpawnTaskWithOptionsHooksCascadeBeforeFirstPrompt(t *testing.T) {
	const childConv = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeee01"
	ls := &fakeSpawnLS{cascadeID: childConv}
	vaultDir := newSpawnOptionsTestVault(t, ls)
	ls.onSend = func(cascadeID string) string {
		if owner, ok := ResolveOwningTask(vaultDir, cascadeID); ok {
			return "owner=" + owner.TaskSlug
		}
		return "owner=?"
	}
	if err := RegisterTask(vaultDir, ActiveTask{TaskSlug: "ledger-fix", Title: "[Task:acme] ledger-fix", Kind: "task", Status: "active"}); err != nil {
		t.Fatalf("RegisterTask: %v", err)
	}

	var hookCalls []string
	out, err := SpawnTaskWithOptions(vaultDir, "manual-spawn:ledger-fix", "swe-pulse", "[Task:acme] ledger-fix", "Fix rounding", "gemini-4-hydrogen-high", false, SpawnTaskOptions{
		OnCascadeCreated: func(cascadeID string) {
			hookCalls = append(hookCalls, cascadeID)
			ls.record("hook")
			_ = RegisterTask(vaultDir, ActiveTask{TaskSlug: "ledger-fix", Kind: "task", ConversationID: cascadeID, Status: "active"})
		},
	})
	if err != nil || out != childConv {
		t.Fatalf("SpawnTaskWithOptions = %q, %v", out, err)
	}
	if len(hookCalls) != 1 || hookCalls[0] != childConv {
		t.Fatalf("OnCascadeCreated must run exactly once with the new conversation ID, got %v", hookCalls)
	}
	if got, want := strings.Join(ls.snapshot(), ","), "start,hook,send:owner=ledger-fix"; got != want {
		t.Fatalf("event order = %s, want %s (the hook must link the task before the first prompt)", got, want)
	}

	// SpawnTask keeps its behavior: same LS sequence, no hook.
	ls.mu.Lock()
	ls.events, ls.cascadeID = nil, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeee02"
	ls.mu.Unlock()
	if out, err := SpawnTask(vaultDir, "manual-spawn:other", "swe-pulse", "[Task:acme] other", "Other", "gemini-4-hydrogen-high", false); err != nil || out != "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeee02" {
		t.Fatalf("SpawnTask = %q, %v", out, err)
	}
	if got := strings.Join(ls.snapshot(), ","); got != "start,send:owner=?" {
		t.Fatalf("SpawnTask event order = %s", got)
	}

	// Dry runs never create a conversation, so the hook never runs.
	if _, err := SpawnTaskWithOptions(vaultDir, "manual-spawn:dry", "swe-pulse", "t", "p", "gemini-4-hydrogen-high", true, SpawnTaskOptions{
		OnCascadeCreated: func(string) { t.Fatal("OnCascadeCreated must not run on a dry run") },
	}); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}

// TestSpawnTaskWithOptionsAgentAPIFallbackSkipsHook keeps the agentapi fallback unchanged: it creates and prompts
// the conversation in one step, so the hook is not called and the ID comes back in the output.
func TestSpawnTaskWithOptionsAgentAPIFallbackSkipsHook(t *testing.T) {
	const childConv = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeee03"
	vaultDir := filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	script := "#!/bin/sh\necho \"" + childConv + "\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "agentapi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "")
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", t.TempDir())
	t.Setenv("HQ_CONFIG_DIR", t.TempDir())

	out, err := SpawnTaskWithOptions(vaultDir, "manual-spawn:fallback", "swe-pulse", "[Task:acme] fallback", "p", "gemini-4-hydrogen-high", false, SpawnTaskOptions{
		OnCascadeCreated: func(string) { t.Fatal("OnCascadeCreated must not run on the agentapi fallback") },
	})
	if err != nil || ExtractUUID(out) != childConv {
		t.Fatalf("agentapi fallback = %q, %v", out, err)
	}
}

func TestExtractUUIDAndAgentAPIStderrIsolation(t *testing.T) {
	const promptUUID = "11111111-2222-4333-8444-555555555555"
	const createdUUID = "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeee99"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "created conversation line wins over earlier prompt uuid",
			in:   "echoed prompt from " + promptUUID + "\nCreated conversation " + createdUUID + " (model=gemini-4-neon-medium)",
			want: createdUUID,
		},
		{
			name: "standalone uuid line wins over earlier uuid",
			in:   "debug: parent=" + promptUUID + "\n" + createdUUID + "\n",
			want: createdUUID,
		},
		{
			name: "last uuid fallback",
			in:   "parent " + promptUUID + " -> child " + createdUUID,
			want: createdUUID,
		},
		{
			name: "no uuid",
			in:   "plain output",
			want: "",
		},
	}
	for _, tc := range cases {
		if got := ExtractUUID(tc.in); got != tc.want {
			t.Errorf("%s: ExtractUUID(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	// Verify agentapi stderr noise containing an earlier UUID does not pollute stdout or fail-return output.
	vaultDir := filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		t.Fatal(err)
	}
	fakeBin := t.TempDir()
	script := "#!/bin/sh\necho \"warn: prompt mentions " + promptUUID + "\" >&2\necho \"" + createdUUID + "\"\n"
	if err := os.WriteFile(filepath.Join(fakeBin, "agentapi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("ANTIGRAVITY_LS_ADDRESS", "")
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")
	t.Setenv("ANTIGRAVITY_SIDECAR_WEB_PORT", "")
	t.Setenv("ANTIGRAVITY_APP_DATA_DIR", t.TempDir())
	t.Setenv("HQ_CONFIG_DIR", t.TempDir())

	out, err := SpawnTask(vaultDir, "manual-spawn:stderr-test", "swe-pulse", "[Task:acme] stderr", "parent "+promptUUID, "gemini-4-neon-medium", false)
	if err != nil {
		t.Fatalf("SpawnTask failed: %v", err)
	}
	if got := ExtractUUID(out); got != createdUUID {
		t.Fatalf("expected created UUID %s from stdout, got %q (raw=%q)", createdUUID, got, out)
	}
}
