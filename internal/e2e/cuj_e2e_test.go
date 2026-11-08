package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
)

type hqEnv struct {
	t        *testing.T
	binPath  string
	rootDir  string
	vaultDir string
}

func setupHQEnv(t *testing.T) *hqEnv {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	rootDir := filepath.Clean(filepath.Join(wd, "..", ".."))
	tmpDir := t.TempDir()
	binPath := filepath.Join(tmpDir, "hq")
	vaultDir := filepath.Join(tmpDir, "local")

	buildCmd := exec.Command("go", "build", "-o", binPath, "./cmd/hq")
	buildCmd.Dir = rootDir
	if out, err := buildCmd.CombinedOutput(); err != nil {
		t.Fatalf("failed to build hq binary: %v (%s)", err, string(out))
	}

	return &hqEnv{
		t:        t,
		binPath:  binPath,
		rootDir:  rootDir,
		vaultDir: vaultDir,
	}
}

func (e *hqEnv) run(stdin string, args ...string) (string, error) {
	e.t.Helper()
	cmd := exec.Command(e.binPath, args...)
	cmd.Dir = e.rootDir
	cmd.Env = append(os.Environ(),
		"HQ_ROOT_DIR="+e.rootDir,
		"HQ_VAULT_DIR="+e.vaultDir,
	)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// CUJ-1: Vault Bootstrap, Idempotency & Health Check
func TestCUJ1_VaultBootstrapAndIdempotency(t *testing.T) {
	env := setupHQEnv(t)

	// 1. Run `hq init`
	out, err := env.run("", "init")
	if err != nil {
		t.Fatalf("hq init failed: %v (%s)", err, out)
	}

	// Verify required files and isolated git repo exist, including initial CONSOLE.md
	for _, rel := range []string{
		"USER.md",
		"MEMORY.md",
		"config.yaml",
		"triggers.yaml",
		".gitignore",
		"CONSOLE.md",
		".git",
		"inbox",
		"notes/briefs",
		"worktrees",
		"private/ledger",
		"private/taxes",
		"memory/proposals",
		"memory/journal",
	} {
		if _, err := os.Stat(filepath.Join(env.vaultDir, rel)); err != nil {
			t.Fatalf("expected %s to exist after hq init: %v", rel, err)
		}
	}

	// 2. Customize USER.md and re-run `hq init` to verify idempotency
	customUser := "# Custom User Profile\n- Language: Bilingual\n"
	userPath := filepath.Join(env.vaultDir, "USER.md")
	if err := os.WriteFile(userPath, []byte(customUser), 0o644); err != nil {
		t.Fatalf("write USER.md: %v", err)
	}
	if _, err := env.run("", "init"); err != nil {
		t.Fatalf("second hq init failed: %v", err)
	}
	gotUser, _ := os.ReadFile(userPath)
	if string(gotUser) != customUser {
		t.Fatalf("hq init overwrote customized USER.md: got %q", string(gotUser))
	}

	// 3. Run `hq doctor` and `hq status`
	docOut, err := env.run("", "doctor")
	if err != nil {
		t.Fatalf("hq doctor failed: %v (%s)", err, docOut)
	}
	if !strings.Contains(docOut, "[PASS] Private Vault (local/)") {
		t.Fatalf("expected Private Vault PASS in hq doctor, got:\n%s", docOut)
	}

	statOut, err := env.run("", "status")
	if err != nil || !strings.Contains(statOut, "Pending Actions:  0") {
		t.Fatalf("hq status unexpected: err=%v out=%s", err, statOut)
	}

	// 4. Verify zero-touch auto-bootstrap on a completely uninitialized Vault via SessionStart hook
	cleanEnv := setupHQEnv(t)
	cleanArtDir := filepath.Join(t.TempDir(), "brain-clean")
	_ = os.MkdirAll(cleanArtDir, 0o755)
	stdin := fmt.Sprintf(`{"conversationId":"conv-clean","artifactDirectoryPath":%q}`, cleanArtDir)
	ssOut, err := cleanEnv.run(stdin, "hook", "session-start")
	if err != nil {
		t.Fatalf("zero-touch SessionStart auto-bootstrap failed: %v (%s)", err, ssOut)
	}
	if _, err := os.Stat(filepath.Join(cleanEnv.vaultDir, "USER.md")); err != nil {
		t.Fatalf("expected SessionStart to auto-initialize local/USER.md on clean clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cleanArtDir, "hq_console.md")); err != nil {
		t.Fatalf("expected SessionStart to sync hq_console.md on clean clone: %v", err)
	}
}

// CUJ-2: Front Desk Lease, Turn-1 Initialization, Handover & Demotion
func TestCUJ2_FrontDeskLifecycleAndHandover(t *testing.T) {
	env := setupHQEnv(t)
	if _, err := env.run("", "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	artDirA := filepath.Join(t.TempDir(), "brain-conv-a")
	artDirB := filepath.Join(t.TempDir(), "brain-conv-b")
	artDirC := filepath.Join(t.TempDir(), "brain-conv-c")
	_ = os.MkdirAll(artDirA, 0o755)
	_ = os.MkdirAll(artDirB, 0o755)
	_ = os.MkdirAll(artDirC, 0o755)

	// 1. Conv-A starts when Front Desk is vacant -> claims lease, gets Turn-1 Init instructions, and writes hq_console.md + .metadata.json
	startA := fmt.Sprintf(`{"conversationId":"conv-a-1111","artifactDirectoryPath":%q}`, artDirA)
	outA, err := env.run(startA, "hook", "session-start")
	if err != nil {
		t.Fatalf("session-start Conv-A failed: %v (%s)", err, outA)
	}
	if !strings.Contains(outA, "New [HQ] Front Desk Initialization Required") || !strings.Contains(outA, "hq_console.md") {
		t.Fatalf("expected Turn-1 Front Desk Initialization instructions in Conv-A session-start, got:\n%s", outA)
	}
	if _, err := os.Stat(filepath.Join(artDirA, "hq_console.md")); err != nil {
		t.Fatalf("expected hq_console.md in Conv-A artifact dir: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artDirA, "hq_console.md.metadata.json")); err != nil {
		t.Fatalf("expected hq_console.md.metadata.json in Conv-A artifact dir: %v", err)
	}

	// 2. Conv-A finishes a turn (`hq hook stop`) -> must NOT be added to tasks.json
	stopA := fmt.Sprintf(`{"conversationId":"conv-a-1111","executionNum":1,"artifactDirectoryPath":%q,"lastUserInput":"Status check"}`, artDirA)
	if _, err := env.run(stopA, "hook", "stop"); err != nil {
		t.Fatalf("stop Conv-A: %v", err)
	}
	tasks, _ := kernel.ListTasks(env.vaultDir)
	if len(tasks) != 0 {
		t.Fatalf("expected active Front Desk Conv-A to be excluded from tasks.json, got %d tasks", len(tasks))
	}

	// 3. Conv-B starts while Conv-A holds Front Desk lease -> must NOT steal lease, gets Front Desk Notice
	startB := fmt.Sprintf(`{"conversationId":"conv-b-2222","artifactDirectoryPath":%q}`, artDirB)
	outB, err := env.run(startB, "hook", "session-start")
	if err != nil {
		t.Fatalf("session-start Conv-B: %v", err)
	}
	if !strings.Contains(outB, "Front Desk Notice") || !strings.Contains(outB, "conv-a-1111") {
		t.Fatalf("expected Front Desk Notice referencing conv-a-1111, got:\n%s", outB)
	}
	fd, ok := kernel.GetFrontDesk(env.vaultDir)
	if !ok || fd.ConversationID != "conv-a-1111" {
		t.Fatalf("expected Conv-A to still hold Front Desk lease, got %+v", fd)
	}

	// 4. Demote Conv-A into a normal Task Conversation via `hq adopt` -> vacates front_desk.json & removes hq_console.md from artDirA
	adoptOut, err := env.run("", "adopt",
		"--conv=conv-a-1111",
		"--task=headquarter-bootstrap",
		"--title=Bootstrap & Architecture Design",
		"--summary=Initial bootstrap complete",
	)
	if err != nil {
		t.Fatalf("hq adopt Conv-A failed: %v (%s)", err, adoptOut)
	}
	if _, ok := kernel.GetFrontDesk(env.vaultDir); ok {
		t.Fatalf("expected front_desk.json to be vacated after adopting Front Desk Conv-A as a task")
	}
	if _, err := os.Stat(filepath.Join(artDirA, "hq_console.md")); !os.IsNotExist(err) {
		t.Fatalf("expected hq_console.md to be removed from demoted Conv-A artifact dir")
	}
	// Even if Conv-A finishes its turn (`hq hook stop`) while Front Desk is vacant, hq_console.md must NOT reappear in artDirA
	if _, err := env.run(stopA, "hook", "stop"); err != nil {
		t.Fatalf("stop Conv-A after demotion: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artDirA, "hq_console.md")); !os.IsNotExist(err) {
		t.Fatalf("regression: hq hook stop recreated hq_console.md in demoted Conv-A artifact dir!")
	}

	// 5. Conv-C starts from scratch -> automatically claims Front Desk, sees Conv-A in Tracked Task Conversations, and gets hq_console.md in artDirC
	startC := fmt.Sprintf(`{"conversationId":"conv-c-3333","artifactDirectoryPath":%q}`, artDirC)
	outC, err := env.run(startC, "hook", "session-start")
	if err != nil {
		t.Fatalf("session-start Conv-C: %v", err)
	}
	if !strings.Contains(outC, "New [HQ] Front Desk Initialization Required") || !strings.Contains(outC, "[Task:headquarter] Bootstrap & Architecture Design") {
		t.Fatalf("expected Conv-C to claim Front Desk and inject Tracked Task Conversations, got:\n%s", outC)
	}
	if _, err := os.Stat(filepath.Join(artDirC, "hq_console.md")); err != nil {
		t.Fatalf("expected hq_console.md in new Front Desk Conv-C artifact dir: %v", err)
	}
}

// CUJ-3: Dedicated Task Conversation Lifecycle (Spawn, Adopt, Stop Sync, Complete)
func TestCUJ3_TaskConversationLifecycle(t *testing.T) {
	env := setupHQEnv(t)
	if _, err := env.run("", "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Claim Front Desk first so task conversations are distinct from Front Desk
	artDesk := filepath.Join(t.TempDir(), "brain-desk")
	_ = os.MkdirAll(artDesk, 0o755)
	_, _ = env.run(fmt.Sprintf(`{"conversationId":"desk-0000","artifactDirectoryPath":%q}`, artDesk), "hook", "session-start")

	// 1. Spawn a task via `hq spawn --dry-run`
	spawnOut, err := env.run("", "spawn",
		"--task=opencli-fix-pr279",
		"--prompt=Fix failing unit test in PR #279",
		"--dry-run",
	)
	if err != nil {
		t.Fatalf("hq spawn failed: %v (%s)", err, spawnOut)
	}
	if !strings.Contains(spawnOut, "[Task:opencli] opencli-fix-pr279") {
		t.Fatalf("expected [Task:opencli] title prefix in spawn output, got: %s", spawnOut)
	}

	// 2. Simulate first Stop hook of the newly spawned conversation -> links ConversationID without duplicating entry!
	stopSpawned := `{"conversationId":"spawned-conv-9999","executionNum":1,"modelName":"gemini-3.8-flash-high","lastUserInput":"Fix failing unit test in PR #279"}`
	if _, err := env.run(stopSpawned, "hook", "stop"); err != nil {
		t.Fatalf("stop spawned task: %v", err)
	}
	tasks, err := kernel.ListTasks(env.vaultDir)
	if err != nil || len(tasks) != 1 {
		t.Fatalf("expected 1 linked task (no duplicate), got %d: %+v", len(tasks), tasks)
	}
	if tasks[0].TaskSlug != "opencli-fix-pr279" || tasks[0].ConversationID != "spawned-conv-9999" {
		t.Fatalf("expected opencli-fix-pr279 to link to spawned-conv-9999, got %+v", tasks[0])
	}

	// 3. Subsequent Stop hook on the same task must preserve curated Title and TaskSlug
	stopTurn2 := `{"conversationId":"spawned-conv-9999","executionNum":2,"modelName":"gemini-3.8-flash-high","lastUserInput":"Follow up question on PR #279"}`
	if _, err := env.run(stopTurn2, "hook", "stop"); err != nil {
		t.Fatalf("stop turn 2: %v", err)
	}
	tasks, _ = kernel.ListTasks(env.vaultDir)
	if tasks[0].TaskSlug != "opencli-fix-pr279" || !strings.HasPrefix(tasks[0].Title, "[Task:opencli]") {
		t.Fatalf("regression: Stop hook overwrote curated TaskSlug or Title: %+v", tasks[0])
	}

	// 4. Complete the task via `hq complete`
	compOut, err := env.run("", "complete", "opencli-fix-pr279", "--summary=Merged PR #279 after green CI")
	if err != nil {
		t.Fatalf("hq complete failed: %v (%s)", err, compOut)
	}
	tasks, _ = kernel.ListTasks(env.vaultDir)
	if tasks[0].Status != "completed" || tasks[0].LastSummary != "Merged PR #279 after green CI" {
		t.Fatalf("expected task status completed, got %+v", tasks[0])
	}
}

// CUJ-4: Smart Model & Thinking-Depth Routing + 429 Quota Fallback
func TestCUJ4_SmartModelRoutingAndQuotaFallback(t *testing.T) {
	cases := []struct {
		prompt    string
		modelFlag string
		depthFlag string
		tierFlag  string
		wantModel string
		wantTier  string
		wantSub   string
	}{
		{
			prompt:    "Quick inbox triage and status summary",
			wantModel: "gemini-3.8-flash-low",
			wantTier:  "quick",
			wantSub:   "flash_lite",
		},
		{
			prompt:    "Add unit tests for the new CLI flag",
			wantModel: "gemini-3.8-flash-high",
			wantTier:  "standard",
			wantSub:   "flash",
		},
		{
			prompt:    "Investigate root cause of race condition in auth architecture",
			wantModel: "gemini-4-argon-high",
			wantTier:  "deep",
			wantSub:   "pro",
		},
		{
			prompt:    "Please use opus to review this PR",
			wantModel: "claude-opus-5-5-high",
			wantTier:  "standard",
			wantSub:   "pro",
		},
		{
			prompt:    "Implement feature",
			modelFlag: "sonnet",
			depthFlag: "medium",
			wantModel: "claude-sonnet-5-5-medium",
			wantTier:  "standard",
			wantSub:   "pro",
		},
	}

	for _, tc := range cases {
		gotModel, gotTier, gotSub := kernel.ResolveModel(tc.prompt, tc.modelFlag, tc.depthFlag, tc.tierFlag)
		if gotModel != tc.wantModel || gotTier != tc.wantTier || gotSub != tc.wantSub {
			t.Fatalf("ResolveModel(%q, %q, %q, %q) = (%q, %q, %q), want (%q, %q, %q)",
				tc.prompt, tc.modelFlag, tc.depthFlag, tc.tierFlag,
				gotModel, gotTier, gotSub, tc.wantModel, tc.wantTier, tc.wantSub)
		}
	}
}

// CUJ-5: Approval-Bound Outbound Execution & Tamper Rejection
func TestCUJ5_ApprovalBoundOutboundExecution(t *testing.T) {
	env := setupHQEnv(t)
	if _, err := env.run("", "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	// 1. PreToolUse must hard-deny irreversible outbound commands
	for _, forbidden := range []string{
		"gws gmail send --to user@example.com",
		"gh pr merge 279 --squash",
		"gh release create v1.0.0",
		"vercel --prod",
	} {
		payload := fmt.Sprintf(`{"toolCall":{"name":"run_command","args":{"CommandLine":%q}}}`, forbidden)
		out, err := env.run(payload, "hook", "pre-tool")
		if err != nil {
			t.Fatalf("pre-tool error for %q: %v", forbidden, err)
		}
		if !strings.Contains(out, `"decision":"deny"`) {
			t.Fatalf("expected hard deny for %q, got: %s", forbidden, out)
		}
	}

	// 1b. PreToolUse MUST allow `hq propose --cmd="gh pr merge ..."` when staging an outbound command
	proposeHookPayload := `{"toolCall":{"name":"run_command","args":{"CommandLine":"./bin/hq propose --type=pr_merge --summary=\"Merge PR 279\" --cmd=\"gh pr merge 279 --squash\""}}}`
	allowOut, err := env.run(proposeHookPayload, "hook", "pre-tool")
	if err != nil || !strings.Contains(allowOut, `"decision":"allow"`) {
		t.Fatalf("expected PreToolUse to allow hq propose with outbound --cmd, got err=%v out=%s", err, allowOut)
	}

	// 2. Stage a proposal via `hq propose`
	markerFile := filepath.Join(env.vaultDir, "notes", "cuj5-approved.txt")
	cmdStr := fmt.Sprintf("echo 'CUJ5_OK' > %s", markerFile)
	propOut, err := env.run("", "propose",
		"--type=external_action",
		"--summary=Write CUJ5 approval marker",
		"--cmd="+cmdStr,
	)
	if err != nil {
		t.Fatalf("hq propose failed: %v (%s)", err, propOut)
	}
	pending, err := approvals.List(env.vaultDir)
	if err != nil || len(pending) != 1 {
		t.Fatalf("expected 1 pending proposal in Vault, got %d (err=%v, out=%s)", len(pending), err, propOut)
	}
	prop := pending[0]

	// 3. Approving with wrong SHA-256 prefix must fail and NOT execute command
	if _, err := env.run("", "approve", prop.ID, "--sha256=deadbeef0000"); err == nil {
		t.Fatalf("expected hq approve to fail on mismatched sha256 prefix")
	}
	if _, err := os.Stat(markerFile); !os.IsNotExist(err) {
		t.Fatalf("command executed despite sha256 mismatch!")
	}

	// 4. Approving with matching SHA-256 prefix must execute command and log intervention
	appOut, err := env.run("", "approve", prop.ID, "--sha256="+prop.SHA256[:12])
	if err != nil {
		t.Fatalf("hq approve failed: %v (%s)", err, appOut)
	}
	gotMarker, err := os.ReadFile(markerFile)
	if err != nil || strings.TrimSpace(string(gotMarker)) != "CUJ5_OK" {
		t.Fatalf("expected marker file CUJ5_OK, got err=%v content=%q", err, string(gotMarker))
	}
	interventions, err := os.ReadFile(filepath.Join(env.vaultDir, "memory", "interventions.jsonl"))
	if err != nil || !strings.Contains(string(interventions), prop.ID) {
		t.Fatalf("expected proposal %s in interventions.jsonl", prop.ID)
	}
}

// CUJ-6: Parallel Multi-Repo Git Worktree Lifecycle (`add`, `list`, `remove`)
func TestCUJ6_ParallelWorktreeLifecycle(t *testing.T) {
	env := setupHQEnv(t)
	if _, err := env.run("", "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Create a dummy local git repository to branch a worktree from
	dummyRepo := filepath.Join(t.TempDir(), "sample-repo")
	_ = os.MkdirAll(dummyRepo, 0o755)
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"config", "user.email", "test@agy-hq.local"},
		{"config", "user.name", "Test"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dummyRepo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}
	_ = os.WriteFile(filepath.Join(dummyRepo, "README.md"), []byte("# Sample\n"), 0o644)
	for _, args := range [][]string{
		{"add", "README.md"},
		{"commit", "-m", "initial"},
	} {
		c := exec.Command("git", args...)
		c.Dir = dummyRepo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, string(out))
		}
	}

	// 1. Add worktree (passing task name that already starts with repo prefix to verify no double prefix)
	addOut, err := env.run("", "worktree", "add", dummyRepo, "sample-repo-fix-bug")
	if err != nil {
		t.Fatalf("worktree add failed: %v (%s)", err, addOut)
	}
	expectedWT := filepath.Join(env.vaultDir, "worktrees", "sample-repo-fix-bug")
	if _, err := os.Stat(expectedWT); err != nil {
		t.Fatalf("expected worktree at %s: %v", expectedWT, err)
	}

	// 2. List worktrees
	listOut, err := env.run("", "worktree", "list")
	if err != nil || !strings.Contains(listOut, "sample-repo-fix-bug") {
		t.Fatalf("worktree list unexpected: err=%v out=%s", err, listOut)
	}

	// 3. Remove worktree
	rmOut, err := env.run("", "worktree", "remove", "sample-repo-fix-bug")
	if err != nil {
		t.Fatalf("worktree remove failed: %v (%s)", err, rmOut)
	}
	if _, err := os.Stat(expectedWT); !os.IsNotExist(err) {
		t.Fatalf("expected worktree %s to be removed", expectedWT)
	}
}

// CUJ-7: Quarantined Inbox Drop & 0-Token Heartbeat Gatekeeper
func TestCUJ7_InboxRoutingAndHeartbeatGatekeeper(t *testing.T) {
	env := setupHQEnv(t)
	if _, err := env.run("", "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	// Pre-create today's Morning Brief so the first heartbeat is 100% idle (0 LLM tokens)
	today := time.Now().Format("2006-01-02")
	briefPath := filepath.Join(env.vaultDir, "notes", "briefs", today+".md")
	_ = os.WriteFile(briefPath, []byte("# Brief\n"), 0o644)

	// 1. Idle heartbeat -> 0 LLM tokens
	hbOut, err := env.run("", "heartbeat", "--dry-run")
	if err != nil || strings.TrimSpace(hbOut) != "HEARTBEAT_OK (0 LLM tokens)" {
		t.Fatalf("expected HEARTBEAT_OK (0 LLM tokens), got err=%v out=%q", err, hbOut)
	}

	// 2. Drop a CSV statement and a 10-Q PDF into local/inbox/
	_ = os.WriteFile(filepath.Join(env.vaultDir, "inbox", "bank-2026-11.csv"), []byte("date,amount\n"), 0o644)
	_ = os.WriteFile(filepath.Join(env.vaultDir, "inbox", "nvda-10-q.pdf"), []byte("%PDF-1.4\n"), 0o644)

	hbOut2, err := env.run("", "heartbeat", "--dry-run")
	if err != nil {
		t.Fatalf("heartbeat with inbox items failed: %v (%s)", err, hbOut2)
	}
	if !strings.Contains(hbOut2, "ledger-ingest") || !strings.Contains(hbOut2, "thesis-check") {
		t.Fatalf("expected ledger-ingest and thesis-check spawns, got: %s", hbOut2)
	}
	// Verify files were moved atomically to local/inbox/queued/
	for _, fn := range []string{"bank-2026-11.csv", "nvda-10-q.pdf"} {
		if _, err := os.Stat(filepath.Join(env.vaultDir, "inbox", "queued", fn)); err != nil {
			t.Fatalf("expected %s to be moved to inbox/queued/: %v", fn, err)
		}
	}
}

// CUJ-8: Zero-Token Per-Turn Journaling & Live Console Sync
func TestCUJ8_TurnJournalingAndLiveConsoleSync(t *testing.T) {
	env := setupHQEnv(t)
	if _, err := env.run("", "init"); err != nil {
		t.Fatalf("init: %v", err)
	}

	artDesk := filepath.Join(t.TempDir(), "brain-desk")
	_ = os.MkdirAll(artDesk, 0o755)
	_, _ = env.run(fmt.Sprintf(`{"conversationId":"desk-conv","artifactDirectoryPath":%q}`, artDesk), "hook", "session-start")

	// Create a synthetic transcript JSONL file with multi-line planner response and tool calls
	transcriptPath := filepath.Join(t.TempDir(), "transcript.jsonl")
	lines := []string{
		`{"step_index":0,"type":"USER_INPUT","content":"Update README and run tests"}`,
		`{"step_index":1,"type":"PLANNER_RESPONSE","content":"Updated README.\n\n### Summary\nAll tests passed!","tool_calls":[{"name":"edit_file","args":{"Path":"/repo/README.md"}},{"name":"run_command","args":{"CommandLine":"go test ./..."}}]}`,
	}
	_ = os.WriteFile(transcriptPath, []byte(strings.Join(lines, "\n")+"\n"), 0o644)

	stopPayload := bytes.NewBuffer(nil)
	_ = json.NewEncoder(stopPayload).Encode(map[string]interface{}{
		"conversationId":        "task-conv-8888",
		"executionNum":          3,
		"modelName":             "gemini-3.8-flash-high",
		"lastUserInput":         "Update README and run tests",
		"transcriptPath":        transcriptPath,
		"terminationReason":     "NO_TOOL_CALL",
		"artifactDirectoryPath": filepath.Join(t.TempDir(), "brain-task"),
	})

	if out, err := env.run(stopPayload.String(), "hook", "stop"); err != nil {
		t.Fatalf("hook stop failed: %v (%s)", err, out)
	}

	// 1. Verify immutable journal file exists in local/memory/journal/YYYY-MM-DD/task-conv-8888-3.json
	today := time.Now().Format("2006-01-02")
	journalFile := filepath.Join(env.vaultDir, "memory", "journal", today, "task-conv-8888-3.json")
	jBytes, err := os.ReadFile(journalFile)
	if err != nil {
		t.Fatalf("expected journal file %s: %v", journalFile, err)
	}
	if !strings.Contains(string(jBytes), "/repo/README.md") || !strings.Contains(string(jBytes), "All tests passed!") {
		t.Fatalf("unexpected journal content:\n%s", string(jBytes))
	}

	// 2. Verify Front Desk's hq_console.md was synced with single-line Task Board row and Recent Turn Activity
	consoleBytes, err := os.ReadFile(filepath.Join(artDesk, "hq_console.md"))
	if err != nil {
		t.Fatalf("expected Front Desk hq_console.md: %v", err)
	}
	consoleStr := string(consoleBytes)
	if !strings.Contains(consoleStr, "task-conv-8888") || !strings.Contains(consoleStr, "Updated README. ### Summary All tests passed!") {
		t.Fatalf("expected single-line sanitized summary in Front Desk hq_console.md, got:\n%s", consoleStr)
	}
}
