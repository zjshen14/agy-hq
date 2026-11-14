package kernel

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestMain ensures unit tests never inherit live Antigravity LanguageServerService
// credentials or caller conversation IDs from the parent IDE shell.
func TestMain(m *testing.M) {
	_ = os.Unsetenv("ANTIGRAVITY_LS_ADDRESS")
	_ = os.Unsetenv("ANTIGRAVITY_CSRF_TOKEN")
	_ = os.Unsetenv("ANTIGRAVITY_CONVERSATION_ID")
	os.Exit(m.Run())
}

type mockLSUpdateCall struct {
	CascadeID        string
	CascadeIDs       []string
	MergeAnnotations bool
	Archived         *bool
	Pinned           *bool
	ArchivalTS       string
	RawJSON          string
}

type mockLSServer struct {
	mu           sync.Mutex
	csrfToken    string
	offline503   bool
	notFoundIDs  map[string]bool
	trajectories map[string]LSTrajectorySummary
	getCalls     int
	updateCalls  []mockLSUpdateCall
	srv          *httptest.Server
}

func newMockLSServer(t *testing.T, csrf string) *mockLSServer {
	t.Helper()
	m := &mockLSServer{
		csrfToken:    csrf,
		notFoundIDs:  make(map[string]bool),
		trajectories: make(map[string]LSTrajectorySummary),
	}
	m.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.Header.Get("Connect-Protocol-Version") != "1" {
			http.Error(w, "missing connect header", http.StatusBadRequest)
			return
		}
		if r.Header.Get("x-codeium-csrf-token") != m.csrfToken {
			http.Error(w, `{"code":"unauthenticated","message":"invalid csrf"}`, http.StatusUnauthorized)
			return
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, "read body", http.StatusBadRequest)
			return
		}

		m.mu.Lock()
		defer m.mu.Unlock()
		if m.offline503 {
			http.Error(w, `{"code":"unavailable","message":"ls restarting"}`, http.StatusServiceUnavailable)
			return
		}

		switch r.URL.Path {
		case "/exa.language_server_pb.LanguageServerService/GetAllCascadeTrajectories":
			m.getCalls++
			var req lsGetAllTrajectoriesRequest
			if err := json.Unmarshal(bodyBytes, &req); err != nil || !req.ExcludeSubtrajectories {
				http.Error(w, `{"code":"invalid_argument","message":"expected excludeSubtrajectories=true"}`, http.StatusBadRequest)
				return
			}
			cp := make(map[string]LSTrajectorySummary, len(m.trajectories))
			for k, v := range m.trajectories {
				cp[k] = v
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(lsGetAllTrajectoriesResponse{TrajectorySummaries: cp})

		case "/exa.language_server_pb.LanguageServerService/UpdateConversationAnnotations":
			var req lsUpdateAnnotationsRequest
			if err := json.Unmarshal(bodyBytes, &req); err != nil {
				http.Error(w, `{"code":"invalid_argument","message":"bad json"}`, http.StatusBadRequest)
				return
			}
			call := mockLSUpdateCall{
				CascadeID:        req.CascadeID,
				CascadeIDs:       append([]string(nil), req.CascadeIDs...),
				MergeAnnotations: req.MergeAnnotations,
				Archived:         req.Annotations.Archived,
				Pinned:           req.Annotations.Pinned,
				ArchivalTS:       req.Annotations.ArchivalStatusTimestamp,
				RawJSON:          string(bodyBytes),
			}
			m.updateCalls = append(m.updateCalls, call)

			targets := req.CascadeIDs
			if len(targets) == 0 && req.CascadeID != "" {
				targets = []string{req.CascadeID}
			}
			for _, id := range targets {
				nid := normalizeUUID(id)
				if m.notFoundIDs[nid] {
					http.Error(w, `{"code":"not_found","message":"cascade trajectory not found"}`, http.StatusNotFound)
					return
				}
			}
			for _, id := range targets {
				nid := normalizeUUID(id)
				sum, ok := m.trajectories[nid]
				if !ok {
					http.Error(w, `{"code":"not_found","message":"unknown cascade"}`, http.StatusNotFound)
					return
				}
				if !req.MergeAnnotations {
					sum.Annotations = LSConversationAnnotations{}
				}
				if req.Annotations.Archived != nil {
					sum.Annotations.Archived = *req.Annotations.Archived
				}
				if req.Annotations.Pinned != nil {
					sum.Annotations.Pinned = *req.Annotations.Pinned
				}
				if req.Annotations.ArchivalStatusTimestamp != "" {
					sum.Annotations.ArchivalStatusTimestamp = req.Annotations.ArchivalStatusTimestamp
				}
				m.trajectories[nid] = sum
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockLSServer) config(now time.Time) SidebarSyncConfig {
	return SidebarSyncConfig{
		BaseURL:        m.srv.URL,
		CSRFToken:      m.csrfToken,
		CallerOverride: true,
		CallerConvID:   "",
		Now:            func() time.Time { return now },
	}
}

func testUUID(n int) string {
	return fmt.Sprintf("00000000-0000-0000-0000-%012d", n)
}

func TestSidebarSyncWireContractAndBatchChunking(t *testing.T) {
	now := time.Date(2026, 11, 14, 14, 0, 0, 0, time.UTC)
	mockLS := newMockLSServer(t, "test-csrf-secret")
	vaultDir := t.TempDir()

	u1 := testUUID(101)
	mockLS.trajectories[u1] = LSTrajectorySummary{
		Status: "CASCADE_RUN_STATUS_IDLE",
		Annotations: LSConversationAnnotations{
			Title: "[Task:hq] Wire Contract Check",
		},
	}

	if err := RegisterTask(vaultDir, ActiveTask{
		ConversationID: u1,
		TaskSlug:       "wire-contract",
		Title:          "[Task:hq] Wire Contract Check",
		Status:         "completed",
		CreatedAt:      now.Add(-2 * time.Hour).Format(time.RFC3339),
		LastUpdatedAt:  now.Add(-1 * time.Hour).Format(time.RFC3339),
	}); err != nil {
		t.Fatalf("RegisterTask: %v", err)
	}

	// 1. ArchiveTaskWithOptions must send mergeAnnotations=true, archived=true, and valid RFC3339 timestamp
	res, err := ArchiveTaskWithOptions(vaultDir, "wire-contract", ArchiveTaskOptions{
		SidebarConfig: mockLS.config(now),
	})
	if err != nil {
		t.Fatalf("ArchiveTaskWithOptions: %v", err)
	}
	if !res.Synced || res.Queued {
		t.Fatalf("expected immediate sidebar sync, got %+v", res)
	}

	mockLS.mu.Lock()
	if len(mockLS.updateCalls) != 1 {
		t.Fatalf("expected 1 UpdateConversationAnnotations call, got %d", len(mockLS.updateCalls))
	}
	call1 := mockLS.updateCalls[0]
	sum1 := mockLS.trajectories[u1]
	mockLS.mu.Unlock()

	if !call1.MergeAnnotations || call1.Archived == nil || !*call1.Archived {
		t.Fatalf("unexpected archive patch call: %+v", call1)
	}
	if !strings.Contains(call1.RawJSON, `"archived":true`) || !strings.Contains(call1.RawJSON, `"mergeAnnotations":true`) {
		t.Fatalf("expected raw JSON wire payload to contain explicit archived:true and mergeAnnotations:true, got %s", call1.RawJSON)
	}
	if _, pErr := time.Parse(time.RFC3339Nano, call1.ArchivalTS); pErr != nil {
		t.Fatalf("expected RFC3339 archivalStatusTimestamp, got %q: %v", call1.ArchivalTS, pErr)
	}
	if !sum1.Annotations.Archived || sum1.Annotations.Title != "[Task:hq] Wire Contract Check" {
		t.Fatalf("expected archived=true with preserved title, got %+v", sum1.Annotations)
	}

	tasks, _ := ListTasks(vaultDir)
	if len(tasks) != 1 || !tasks[0].SidebarArchived || tasks[0].SidebarSyncedAt == "" {
		t.Fatalf("expected hot task SidebarArchived=true, got %+v", tasks)
	}

	// 2. ReopenTaskWithOptions MUST explicitly serialize `"archived":false` on the wire (proto3_optional presence)
	reopenRes, err := ReopenTaskWithOptions(vaultDir, "wire-contract", ReopenTaskOptions{
		NoWorktree:    true,
		SidebarConfig: mockLS.config(now.Add(5 * time.Minute)),
	})
	if err != nil {
		t.Fatalf("ReopenTaskWithOptions: %v", err)
	}
	if !reopenRes.Synced {
		t.Fatalf("expected reopen sidebar sync, got %+v", reopenRes)
	}

	mockLS.mu.Lock()
	if len(mockLS.updateCalls) != 2 {
		t.Fatalf("expected 2 UpdateConversationAnnotations calls, got %d", len(mockLS.updateCalls))
	}
	call2 := mockLS.updateCalls[1]
	sumAfterReopen := mockLS.trajectories[u1]
	mockLS.mu.Unlock()

	if !strings.Contains(call2.RawJSON, `"archived":false`) {
		t.Fatalf("CRITICAL proto3_optional regression: expected explicit \"archived\":false in JSON body, got %s", call2.RawJSON)
	}
	if sumAfterReopen.Annotations.Archived || sumAfterReopen.Annotations.Title != "[Task:hq] Wire Contract Check" {
		t.Fatalf("expected unarchived conversation with title preserved, got %+v", sumAfterReopen.Annotations)
	}

	// 3. Batch chunking (65 candidates with BatchSize=25 -> chunks of 25, 25, 15) + per-ID isolation on 404
	var coldBatch []ActiveTask
	missingInBatch := testUUID(205) // inside the third chunk (ListArchivedTasks orders newest-first: 265..201 -> 215..201)
	for i := 201; i <= 265; i++ {
		id := testUUID(i)
		mockLS.trajectories[id] = LSTrajectorySummary{
			Status: "CASCADE_RUN_STATUS_IDLE",
			Annotations: LSConversationAnnotations{
				Title: fmt.Sprintf("[Task:batch] Item %d", i),
			},
		}
		coldBatch = append(coldBatch, ActiveTask{
			ConversationID: id,
			TaskSlug:       fmt.Sprintf("batch-%d", i),
			Status:         "archived",
		})
	}
	mockLS.notFoundIDs[missingInBatch] = true
	if err := AppendTasksToColdArchive(vaultDir, coldBatch); err != nil {
		t.Fatalf("AppendTasksToColdArchive: %v", err)
	}

	mockLS.mu.Lock()
	mockLS.updateCalls = nil
	mockLS.mu.Unlock()

	bRep, err := SyncSidebar(vaultDir, SyncSidebarOptions{
		Now:          now.Add(10 * time.Minute),
		Backfill:     true,
		BatchSize:    25,
		ClientConfig: mockLS.config(now.Add(10 * time.Minute)),
	})
	if err != nil {
		t.Fatalf("SyncSidebar batch: %v", err)
	}
	if bRep.ArchivedCount != 64 || bRep.MissingOrSubtrajectoryCount != 1 {
		t.Fatalf("expected 64 archived and 1 isolated 404 missing, got %+v", bRep)
	}

	mockLS.mu.Lock()
	defer mockLS.mu.Unlock()
	// Chunks 1 & 2 succeed in 2 batch RPCs (25 + 25); chunk 3 (15) hits 404 and falls back to 15 unary RPCs = 18 total calls
	if len(mockLS.updateCalls) != 18 {
		t.Fatalf("expected 18 update calls (2x25 batch + 1x15 failed batch + 15 unary fallback), got %d", len(mockLS.updateCalls))
	}
	if len(mockLS.updateCalls[0].CascadeIDs) != 25 || len(mockLS.updateCalls[1].CascadeIDs) != 25 || len(mockLS.updateCalls[2].CascadeIDs) != 15 {
		t.Fatalf("unexpected chunk sizes: %d, %d, %d",
			len(mockLS.updateCalls[0].CascadeIDs), len(mockLS.updateCalls[1].CascadeIDs), len(mockLS.updateCalls[2].CascadeIDs))
	}
}

func TestSidebarArchiveSevenSafetyGates(t *testing.T) {
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)
	mockLS := newMockLSServer(t, "gate-csrf")
	vaultDir := t.TempDir()

	fdLeaseID := testUUID(1)
	fdTitleID := testUUID(2)
	callerID := testUUID(3)
	vaultPinnedID := testUUID(4)
	uiPinnedID := testUUID(5)
	uiStarredID := testUUID(6)
	hotActiveID := testUUID(7)
	busyRunningID := testUUID(8)
	busySubagentID := testUUID(9)
	busyWaitingID := testUUID(10)
	alreadyArchID := testUUID(11)
	eligibleColdID := testUUID(12)

	if err := SetFrontDesk(vaultDir, fdLeaseID, ""); err != nil {
		t.Fatalf("SetFrontDesk: %v", err)
	}

	mockLS.trajectories = map[string]LSTrajectorySummary{
		fdLeaseID:      {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "Custom Coordinator"}},
		fdTitleID:      {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[HQ] Front Desk"}},
		callerID:       {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] Current Caller"}},
		vaultPinnedID:  {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] Vault Pinned"}},
		uiPinnedID:     {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] UI Pinned", Pinned: true}},
		uiStarredID:    {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] UI Starred", Starred: true}},
		hotActiveID:    {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] Reopened Active"}},
		busyRunningID:  {Status: "CASCADE_RUN_STATUS_RUNNING", Annotations: LSConversationAnnotations{Title: "[Task:hq] Running"}},
		busySubagentID: {Status: "CASCADE_RUN_STATUS_IDLE", HasRunningSubagents: true, Annotations: LSConversationAnnotations{Title: "[Task:hq] Subagent Running"}},
		busyWaitingID:  {Status: "CASCADE_RUN_STATUS_IDLE", WaitingSteps: []json.RawMessage{json.RawMessage(`{"id":"w1"}`)}, Annotations: LSConversationAnnotations{Title: "[Task:hq] Waiting"}},
		alreadyArchID:  {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] Already Archived", Archived: true}},
		eligibleColdID: {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] Clean Historical"}},
	}

	// Gate S1: non-UUID task must never hit LS
	_ = RegisterTask(vaultDir, ActiveTask{ConversationID: "conv-legacy-demo", TaskSlug: "legacy-demo", Status: "completed", LastUpdatedAt: now.Format(time.RFC3339)})
	s1Res, err := ArchiveTaskWithOptions(vaultDir, "legacy-demo", ArchiveTaskOptions{SidebarConfig: mockLS.config(now)})
	if err != nil || s1Res.SkippedReason != "invalid_uuid" {
		t.Fatalf("Gate S1 failed: err=%v res=%+v", err, s1Res)
	}

	// Gate S2: Front Desk must NEVER be archived in sidebar even with Force=true!
	for _, tc := range []struct {
		id    string
		slug  string
		title string
	}{
		{fdLeaseID, "fd-lease", "Custom Coordinator"},
		{fdTitleID, "fd-title", "[HQ] Front Desk"},
	} {
		_ = RegisterTask(vaultDir, ActiveTask{ConversationID: tc.id, TaskSlug: tc.slug, Title: tc.title, Status: "completed", LastUpdatedAt: now.Format(time.RFC3339)})
		s2Res, s2Err := ArchiveTaskWithOptions(vaultDir, tc.slug, ArchiveTaskOptions{Force: true, SidebarConfig: mockLS.config(now)})
		if s2Err != nil || s2Res.SkippedReason != "front_desk" || mockLS.trajectories[tc.id].Annotations.Archived {
			t.Fatalf("Gate S2 failed for %s: err=%v res=%+v archived=%v", tc.slug, s2Err, s2Res, mockLS.trajectories[tc.id].Annotations.Archived)
		}
	}

	// Gate S3: Explicit self-archive defers to queue instead of archiving live caller mid-turn, then drains once caller changes
	_ = RegisterTask(vaultDir, ActiveTask{ConversationID: callerID, TaskSlug: "self-task", Status: "completed", LastUpdatedAt: now.Format(time.RFC3339)})
	selfCfg := mockLS.config(now)
	selfCfg.CallerConvID = callerID
	s3Res, err := ArchiveTaskWithOptions(vaultDir, "self-task", ArchiveTaskOptions{SidebarConfig: selfCfg})
	if err != nil || !s3Res.Queued || s3Res.SkippedReason != "caller_self_deferred" {
		t.Fatalf("Gate S3 explicit self deferral failed: err=%v res=%+v", err, s3Res)
	}
	if mockLS.trajectories[callerID].Annotations.Archived {
		t.Fatalf("Gate S3 must not archive caller while caller is active")
	}
	// Drain from same caller -> still skipped without burning retry exhaustion
	_, _ = DrainSidebarSyncQueue(vaultDir, DefaultSidebarBatchTimeout, selfCfg)
	if mockLS.trajectories[callerID].Annotations.Archived {
		t.Fatalf("Gate S3 must not drain caller_self while caller matches")
	}
	// Advance past 30s reschedule and drain from another session -> archives cleanly!
	otherCfg := mockLS.config(now.Add(1 * time.Minute))
	drainRep, err := SyncSidebar(vaultDir, SyncSidebarOptions{
		Now:          now.Add(1 * time.Minute),
		QueueOnly:    true,
		ClientConfig: otherCfg,
	})
	if err != nil || drainRep.ArchivedCount != 1 || !mockLS.trajectories[callerID].Annotations.Archived {
		t.Fatalf("expected deferred caller task to archive once caller changed: err=%v rep=%+v", err, drainRep)
	}
	// Reset callerID back to unarchived in mockLS so we can also test bulk Gate S3 skip below
	s := mockLS.trajectories[callerID]
	s.Annotations.Archived = false
	mockLS.trajectories[callerID] = s

	// Gate S4: Vault Pinned rejected without --force; UI Pinned skipped without --force and overridden with --force
	_ = RegisterTask(vaultDir, ActiveTask{ConversationID: vaultPinnedID, TaskSlug: "v-pinned", Pinned: true, Status: "completed", LastUpdatedAt: now.Format(time.RFC3339)})
	if _, pErr := ArchiveTaskWithOptions(vaultDir, "v-pinned", ArchiveTaskOptions{SidebarConfig: mockLS.config(now)}); pErr == nil {
		t.Fatalf("expected ArchiveTaskWithOptions without Force to reject Vault-pinned task")
	}
	_ = RegisterTask(vaultDir, ActiveTask{ConversationID: uiPinnedID, TaskSlug: "u-pinned", Status: "completed", LastUpdatedAt: now.Format(time.RFC3339)})
	uPinRes, err := ArchiveTaskWithOptions(vaultDir, "u-pinned", ArchiveTaskOptions{SidebarConfig: mockLS.config(now)})
	if err != nil || uPinRes.SkippedReason != "ui_pinned" || mockLS.trajectories[uiPinnedID].Annotations.Archived {
		t.Fatalf("expected UI-pinned task to skip sidebar without --force: err=%v res=%+v", err, uPinRes)
	}

	// Gate S5: Active hot task in Vault must block cold backfill for the same ConversationID
	_ = RegisterTask(vaultDir, ActiveTask{ConversationID: hotActiveID, TaskSlug: "hot-active", Status: "active", LastUpdatedAt: now.Format(time.RFC3339)})

	// Seed cold archive with all remaining candidates to test Backfill Gates S2..S7 in one sweep
	if err := AppendTasksToColdArchive(vaultDir, []ActiveTask{
		{ConversationID: fdLeaseID, TaskSlug: "cold-fd1", Status: "archived"},
		{ConversationID: fdTitleID, TaskSlug: "cold-fd2", Status: "archived"},
		{ConversationID: callerID, TaskSlug: "cold-caller", Status: "archived"},
		{ConversationID: vaultPinnedID, TaskSlug: "cold-vpinned", Pinned: true, Status: "archived"},
		{ConversationID: uiStarredID, TaskSlug: "cold-ustarred", Status: "archived"},
		{ConversationID: hotActiveID, TaskSlug: "cold-reopened", Status: "archived"},
		{ConversationID: busyRunningID, TaskSlug: "cold-running", Status: "archived"},
		{ConversationID: busySubagentID, TaskSlug: "cold-subagent", Status: "archived"},
		{ConversationID: busyWaitingID, TaskSlug: "cold-waiting", Status: "archived"},
		{ConversationID: alreadyArchID, TaskSlug: "cold-already", Status: "archived"},
		{ConversationID: eligibleColdID, TaskSlug: "cold-eligible", Status: "archived"},
	}); err != nil {
		t.Fatalf("AppendTasksToColdArchive: %v", err)
	}

	backfillCfg := mockLS.config(now)
	backfillCfg.CallerConvID = callerID
	bfRep, err := SyncSidebar(vaultDir, SyncSidebarOptions{
		Now:          now,
		Backfill:     true,
		ClientConfig: backfillCfg,
	})
	if err != nil {
		t.Fatalf("SyncSidebar backfill: %v", err)
	}
	if bfRep.ArchivedCount != 1 || len(bfRep.ArchivedIDs) != 1 || bfRep.ArchivedIDs[0] != eligibleColdID {
		t.Fatalf("expected ONLY eligibleColdID (%s) to be archived, got %+v", eligibleColdID, bfRep)
	}
	if bfRep.AlreadySyncedCount != 1 {
		t.Fatalf("expected Gate S7 AlreadySyncedCount=1, got %d", bfRep.AlreadySyncedCount)
	}

	reasons := make(map[string]string)
	for _, sk := range bfRep.ProtectedSkipped {
		reasons[sk.ConversationID] = sk.Gate + ":" + sk.Reason
	}
	expectedReasons := map[string]string{
		fdLeaseID:      "S2:front_desk",
		fdTitleID:      "S2:front_desk",
		callerID:       "S3:caller_self",
		vaultPinnedID:  "S4:vault_pinned",
		uiPinnedID:     "S4:ui_pinned",
		uiStarredID:    "S4:ui_starred",
		hotActiveID:    "S5:active_in_vault",
		busyRunningID:  "S6:trajectory_busy",
		busySubagentID: "S6:trajectory_busy",
		busyWaitingID:  "S6:trajectory_busy",
	}
	for id, want := range expectedReasons {
		if got := reasons[id]; got != want {
			t.Fatalf("expected skip reason %q for %s, got %q (all=%v)", want, id, got, reasons)
		}
		if mockLS.trajectories[id].Annotations.Archived {
			t.Fatalf("protected trajectory %s (%s) must NOT be archived!", id, want)
		}
	}

	// Explicit single-task ArchiveTaskWithOptions with Force=true CAN override UI pin (Gate S4), while Gate S6 queues busy trajectories until idle
	forcePinRes, err := ArchiveTaskWithOptions(vaultDir, "u-pinned", ArchiveTaskOptions{Force: true, SidebarConfig: mockLS.config(now)})
	if err != nil || !forcePinRes.Synced || !mockLS.trajectories[uiPinnedID].Annotations.Archived {
		t.Fatalf("expected explicit --force archive to override UI pin: err=%v res=%+v", err, forcePinRes)
	}

	_ = RegisterTask(vaultDir, ActiveTask{ConversationID: busyRunningID, TaskSlug: "busy-task", Status: "completed", LastUpdatedAt: now.Format(time.RFC3339)})
	busyRes, err := ArchiveTaskWithOptions(vaultDir, "busy-task", ArchiveTaskOptions{SidebarConfig: mockLS.config(now)})
	if err != nil || !busyRes.Queued || busyRes.SkippedReason != "trajectory_busy" {
		t.Fatalf("expected Gate S6 busy task to queue on explicit archive: err=%v res=%+v", err, busyRes)
	}
	// Transition trajectory to IDLE and drain
	mockLS.mu.Lock()
	idleCopy := mockLS.trajectories[busyRunningID]
	idleCopy.Status = "CASCADE_RUN_STATUS_IDLE"
	mockLS.trajectories[busyRunningID] = idleCopy
	mockLS.mu.Unlock()

	afterIdleRep, err := SyncSidebar(vaultDir, SyncSidebarOptions{
		Now:          now.Add(1 * time.Minute),
		QueueOnly:    true,
		ClientConfig: mockLS.config(now.Add(1 * time.Minute)),
	})
	if err != nil || afterIdleRep.ArchivedCount != 1 || !mockLS.trajectories[busyRunningID].Annotations.Archived {
		t.Fatalf("expected queued busy task to archive once IDLE: err=%v rep=%+v", err, afterIdleRep)
	}
}

func TestSidebarOfflineQueueSupersessionHeartbeatAndTerminalPruning(t *testing.T) {
	now := time.Date(2026, 11, 14, 16, 0, 0, 0, time.UTC)
	mockLS := newMockLSServer(t, "offline-csrf")
	vaultDir := t.TempDir()

	uSupersede := testUUID(301)
	uHeartbeat := testUUID(302)
	uNotFound := testUUID(303)

	mockLS.trajectories[uSupersede] = LSTrajectorySummary{
		Status:      "CASCADE_RUN_STATUS_IDLE",
		Annotations: LSConversationAnnotations{Title: "[Task:hq] Supersede Check", Archived: true},
	}
	mockLS.trajectories[uHeartbeat] = LSTrajectorySummary{
		Status:      "CASCADE_RUN_STATUS_IDLE",
		Annotations: LSConversationAnnotations{Title: "[Task:hq] Heartbeat Drain"},
	}
	mockLS.trajectories[uNotFound] = LSTrajectorySummary{
		Status:      "CASCADE_RUN_STATUS_IDLE",
		Annotations: LSConversationAnnotations{Title: "[Task:hq] Ephemeral Deleted"},
	}
	mockLS.notFoundIDs[uNotFound] = true

	for _, tc := range []struct {
		id   string
		slug string
	}{
		{uSupersede, "task-supersede"},
		{uHeartbeat, "task-heartbeat"},
		{uNotFound, "task-notfound"},
	} {
		_ = RegisterTask(vaultDir, ActiveTask{
			ConversationID: tc.id,
			TaskSlug:       tc.slug,
			Status:         "completed",
			LastUpdatedAt:  now.Format(time.RFC3339),
		})
	}

	offlineCfg := SidebarSyncConfig{
		BaseURL:        "",
		CSRFToken:      "",
		CallerOverride: true,
		Now:            func() time.Time { return now },
	}

	// 1. Archive while offline -> succeeds locally and enqueues DesiredArchived=true
	r1, err := ArchiveTaskWithOptions(vaultDir, "task-supersede", ArchiveTaskOptions{SidebarConfig: offlineCfg})
	if err != nil || !r1.Queued || r1.SkippedReason != "ls_offline" {
		t.Fatalf("expected offline archive to queue: err=%v res=%+v", err, r1)
	}
	q := LoadSidebarSyncQueue(vaultDir)
	if !q.Items[uSupersede].DesiredArchived {
		t.Fatalf("expected DesiredArchived=true in queue, got %+v", q.Items[uSupersede])
	}

	// 2. Reopen while still offline -> supersedes queue entry to DesiredArchived=false!
	r2, err := ReopenTaskWithOptions(vaultDir, "task-supersede", ReopenTaskOptions{
		NoWorktree:    true,
		SidebarConfig: offlineCfg,
	})
	if err != nil || !r2.Queued {
		t.Fatalf("expected offline reopen to queue supersession: err=%v res=%+v", err, r2)
	}
	q = LoadSidebarSyncQueue(vaultDir)
	if len(q.Items) != 1 || q.Items[uSupersede].DesiredArchived {
		t.Fatalf("expected queued item superseded to DesiredArchived=false, got %+v", q.Items)
	}

	// Drain when LS comes online -> sends archived=false (unarchiving uSupersede) and removes queue file!
	dRep, err := DrainSidebarSyncQueue(vaultDir, DefaultSidebarBatchTimeout, mockLS.config(now.Add(10*time.Second)))
	if err != nil || dRep.UnarchivedCount != 1 || mockLS.trajectories[uSupersede].Annotations.Archived {
		t.Fatalf("expected superseded queue drain to unarchive uSupersede: err=%v rep=%+v", err, dRep)
	}
	if HasPendingSidebarSyncQueue(vaultDir) {
		t.Fatalf("expected sidebar_sync_queue.json to be deleted when empty")
	}

	// 3. HTTP 503 transient failure + Terminal 404 pruning + RunHeartbeat opportunistic drain
	mockLS.mu.Lock()
	mockLS.offline503 = true
	mockLS.mu.Unlock()

	rHB, err := ArchiveTaskWithOptions(vaultDir, "task-heartbeat", ArchiveTaskOptions{SidebarConfig: mockLS.config(now)})
	if err != nil || !rHB.Queued {
		t.Fatalf("expected HTTP 503 to enqueue task-heartbeat: err=%v res=%+v", err, rHB)
	}
	rNF, err := ArchiveTaskWithOptions(vaultDir, "task-notfound", ArchiveTaskOptions{SidebarConfig: offlineCfg})
	if err != nil || !rNF.Queued {
		t.Fatalf("expected offline archive of task-notfound to enqueue: err=%v res=%+v", err, rNF)
	}
	if st := InspectSidebarSyncQueue(vaultDir); st.PendingCount != 2 {
		t.Fatalf("expected InspectSidebarSyncQueue PendingCount=2, got %+v", st)
	}

	// Restore mock LS (uNotFound still returns 404 on UpdateConversationAnnotations) and advance RetryAt so RunHeartbeat drains both
	mockLS.mu.Lock()
	mockLS.offline503 = false
	mockLS.mu.Unlock()
	// Make task-heartbeat immediately due in queue for RunHeartbeat (which uses wall-clock time.Now())
	_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
		ConversationID:  uHeartbeat,
		TaskSlug:        "task-heartbeat",
		DesiredArchived: true,
		NextRetryAt:     time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339),
	}, time.Now().Add(-1*time.Minute))
	_ = EnqueueSidebarSync(vaultDir, SidebarSyncQueueItem{
		ConversationID:  uNotFound,
		TaskSlug:        "task-notfound",
		DesiredArchived: true,
		NextRetryAt:     time.Now().Add(-1 * time.Minute).UTC().Format(time.RFC3339),
	}, time.Now().Add(-1*time.Minute))

	t.Setenv("ANTIGRAVITY_LS_ADDRESS", mockLS.srv.URL)
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", mockLS.csrfToken)
	if _, hbErr := RunHeartbeat(vaultDir, false); hbErr != nil {
		t.Fatalf("RunHeartbeat: %v", hbErr)
	}
	if !mockLS.trajectories[uHeartbeat].Annotations.Archived {
		t.Fatalf("expected RunHeartbeat to drain and archive uHeartbeat")
	}
	if HasPendingSidebarSyncQueue(vaultDir) {
		t.Fatalf("expected uNotFound (404) to be pruned terminally and queue file removed, still found: %+v", LoadSidebarSyncQueue(vaultDir))
	}
}

func TestSidebarBackfillDryRunZeroWriteGuaranteeAndCompactIntegration(t *testing.T) {
	now := time.Date(2026, 11, 14, 17, 0, 0, 0, time.UTC)
	mockLS := newMockLSServer(t, "dryrun-csrf")
	vaultDir := t.TempDir()

	uHotDone := testUUID(401)
	uCold1 := testUUID(402)
	uCold2 := testUUID(403)
	uFD := testUUID(404)

	_ = SetFrontDesk(vaultDir, uFD, "")
	mockLS.trajectories = map[string]LSTrajectorySummary{
		uHotDone: {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] Completed 30h Ago"}},
		uCold1:   {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] Historical 1"}},
		uCold2:   {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] Historical 2"}},
		uFD:      {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[HQ] Front Desk"}},
	}

	_ = RegisterTask(vaultDir, ActiveTask{
		ConversationID: uHotDone,
		TaskSlug:       "done-30h",
		Kind:           "task",
		Status:         "completed",
		CreatedAt:      now.Add(-32 * time.Hour).Format(time.RFC3339),
		LastUpdatedAt:  now.Add(-30 * time.Hour).Format(time.RFC3339),
	})
	_ = AppendTasksToColdArchive(vaultDir, []ActiveTask{
		{ConversationID: uCold1, TaskSlug: "hist-1", Status: "archived"},
		{ConversationID: uCold2, TaskSlug: "hist-2", Status: "archived"},
		{ConversationID: uFD, TaskSlug: "hist-fd", Status: "archived"},
	})

	tasksBefore, _ := os.ReadFile(tasksFilePath(vaultDir))
	coldBefore, _ := os.ReadFile(tasksArchiveFilePath(vaultDir))

	// 1. Dry-run backfill: 1 read RPC, 0 write RPCs, 0 disk mutations
	dryRep, err := SyncSidebar(vaultDir, SyncSidebarOptions{
		Now:          now,
		DryRun:       true,
		Backfill:     true,
		ClientConfig: mockLS.config(now),
	})
	if err != nil {
		t.Fatalf("SyncSidebar dry-run: %v", err)
	}
	if len(dryRep.WouldArchive) != 2 || len(dryRep.ProtectedSkipped) != 1 {
		t.Fatalf("expected 2 WouldArchive (uCold1, uCold2; hot completed within grace is NOT touched before compact) and 1 ProtectedSkipped (uFD), got %+v", dryRep)
	}

	mockLS.mu.Lock()
	if mockLS.getCalls != 1 || len(mockLS.updateCalls) != 0 {
		t.Fatalf("dry-run RPC violation: getCalls=%d updateCalls=%d", mockLS.getCalls, len(mockLS.updateCalls))
	}
	mockLS.mu.Unlock()

	tasksAfterDry, _ := os.ReadFile(tasksFilePath(vaultDir))
	coldAfterDry, _ := os.ReadFile(tasksArchiveFilePath(vaultDir))
	if string(tasksBefore) != string(tasksAfterDry) || string(coldBefore) != string(coldAfterDry) || HasPendingSidebarSyncQueue(vaultDir) {
		t.Fatalf("dry-run mutated Vault files on disk!")
	}

	// 2. CompactTasks auto-archives uHotDone (>24h completed) and syncs it to sidebar automatically!
	cRep, err := CompactTasks(vaultDir, CompactTasksOptions{
		Now:           now,
		SidebarConfig: mockLS.config(now),
	})
	if err != nil {
		t.Fatalf("CompactTasks: %v", err)
	}
	if len(cRep.AutoArchivedSlugs) != 1 || cRep.SidebarSyncedCount != 1 || !mockLS.trajectories[uHotDone].Annotations.Archived {
		t.Fatalf("expected CompactTasks to auto-archive and sync uHotDone, got %+v", cRep)
	}
}

func writeConnectFrame(w http.ResponseWriter, flags byte, payload interface{}) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	var hdr [5]byte
	hdr[0] = flags
	binary.BigEndian.PutUint32(hdr[1:5], uint32(len(raw)))
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if len(raw) > 0 {
		if _, err := w.Write(raw); err != nil {
			return err
		}
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func TestJetboxUnloadedHistoricalBackfillAndSubtrajectoryFilter(t *testing.T) {
	now := time.Date(2026, 11, 14, 18, 0, 0, 0, time.UTC)
	csrf := "jetbox-csrf"
	vaultDir := t.TempDir()

	uLoaded := testUUID(501)
	uUnloaded1 := testUUID(502)
	uUnloaded2 := testUUID(503)
	uUnloadedPinned := testUUID(504)
	uSubagent := testUUID(505)

	var mu sync.Mutex
	inMemoryLoaded := map[string]LSTrajectorySummary{
		uLoaded: {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[HQ] Front Desk", Pinned: true}},
	}
	jetboxPersisted := map[string]LSTrajectorySummary{
		uLoaded:         {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[HQ] Front Desk", Pinned: true}},
		uUnloaded1:      {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] 6-Day Old Unloaded 1"}},
		uUnloaded2:      {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] 6-Day Old Unloaded 2"}},
		uUnloadedPinned: {Status: "CASCADE_RUN_STATUS_IDLE", Annotations: LSConversationAnnotations{Title: "[Task:hq] Unloaded Pinned", Pinned: true}},
		uSubagent: {
			Status:             "CASCADE_RUN_STATUS_IDLE",
			TrajectoryMetadata: LSTrajectoryMetadata{ParentConversationID: uLoaded, NestingDepth: 1},
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-codeium-csrf-token") != csrf {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/exa.language_server_pb.LanguageServerService/GetAllCascadeTrajectories":
			mu.Lock()
			defer mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(lsGetAllTrajectoriesResponse{TrajectorySummaries: inMemoryLoaded})

		case "/exa.language_server_pb.LanguageServerService/JetboxSubscribeToSummaries":
			if r.Header.Get("Content-Type") != "application/connect+json" {
				http.Error(w, "expected application/connect+json", http.StatusBadRequest)
				return
			}
			mu.Lock()
			cp := make(map[string]LSTrajectorySummary, len(jetboxPersisted))
			for k, v := range jetboxPersisted {
				cp[k] = v
			}
			mu.Unlock()

			w.Header().Set("Content-Type", "application/connect+json")
			_ = writeConnectFrame(w, 0x00, lsJetboxSummariesFrame{Updates: cp})
			// Simulate open reactive stream waiting for client cancellation after snapshot (<100 items)
			<-r.Context().Done()

		case "/exa.language_server_pb.LanguageServerService/UpdateConversationAnnotations":
			body, _ := io.ReadAll(r.Body)
			var req lsUpdateAnnotationsRequest
			if err := json.Unmarshal(body, &req); err != nil || !req.MergeAnnotations {
				http.Error(w, "bad update", http.StatusBadRequest)
				return
			}
			targets := req.CascadeIDs
			if len(targets) == 0 && req.CascadeID != "" {
				targets = []string{req.CascadeID}
			}
			mu.Lock()
			for _, id := range targets {
				nid := normalizeUUID(id)
				sum, ok := jetboxPersisted[nid]
				if !ok {
					mu.Unlock()
					http.Error(w, `{"code":"not_found"}`, http.StatusNotFound)
					return
				}
				if req.Annotations.Archived != nil {
					sum.Annotations.Archived = *req.Annotations.Archived
				}
				jetboxPersisted[nid] = sum
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{}`))

		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfg := SidebarSyncConfig{
		BaseURL:        srv.URL,
		CSRFToken:      csrf,
		CallerOverride: true,
		Now:            func() time.Time { return now },
	}

	_ = SetFrontDesk(vaultDir, uLoaded, "")
	if err := AppendTasksToColdArchive(vaultDir, []ActiveTask{
		{ConversationID: uUnloaded1, TaskSlug: "old-1", Status: "archived"},
		{ConversationID: uUnloaded2, TaskSlug: "old-2", Status: "archived"},
		{ConversationID: uUnloadedPinned, TaskSlug: "old-pinned", Status: "archived"},
		{ConversationID: uSubagent, TaskSlug: "old-subagent", Kind: "subagent", Status: "completed"},
	}); err != nil {
		t.Fatalf("AppendTasksToColdArchive: %v", err)
	}

	// 1. Backfill must discover unloaded conversations via JetboxSubscribeToSummaries, archive uUnloaded1 & uUnloaded2,
	//    skip uUnloadedPinned (Gate S4), and ignore uSubagent (filtered out as nested subtrajectory)!
	rep, err := SyncSidebar(vaultDir, SyncSidebarOptions{
		Now:          now,
		Backfill:     true,
		ClientConfig: cfg,
	})
	if err != nil {
		t.Fatalf("SyncSidebar with Jetbox unloaded trajectories: %v", err)
	}
	if rep.ArchivedCount != 2 || rep.MissingOrSubtrajectoryCount != 1 || len(rep.ProtectedSkipped) != 1 {
		t.Fatalf("unexpected Jetbox backfill report: %+v", rep)
	}
	if rep.ProtectedSkipped[0].Reason != "ui_pinned" {
		t.Fatalf("expected Gate S4 ui_pinned on unloaded pinned thread, got %+v", rep.ProtectedSkipped)
	}
	mu.Lock()
	if !jetboxPersisted[uUnloaded1].Annotations.Archived || !jetboxPersisted[uUnloaded2].Annotations.Archived || jetboxPersisted[uUnloadedPinned].Annotations.Archived {
		t.Fatalf("unexpected Jetbox archived state after backfill: %+v", jetboxPersisted)
	}
	mu.Unlock()

	// 2. ReopenTaskWithOptions on an unloaded cold conversation must also resolve via Jetbox and unarchive cleanly!
	reopenRes, err := ReopenTaskWithOptions(vaultDir, "old-1", ReopenTaskOptions{
		NoWorktree:    true,
		SidebarConfig: cfg,
	})
	if err != nil || !reopenRes.Synced {
		t.Fatalf("expected ReopenTaskWithOptions on unloaded task to sync via Jetbox: err=%v res=%+v", err, reopenRes)
	}
	mu.Lock()
	defer mu.Unlock()
	if jetboxPersisted[uUnloaded1].Annotations.Archived {
		t.Fatalf("expected uUnloaded1 to be unarchived after cold reopen")
	}
}
