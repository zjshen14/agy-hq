package kernel

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/events"
)

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	orig := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	defer func() {
		os.Stderr = orig
	}()

	fn()

	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

func TestResolveModelEnumNeverUsesStaleStaticWhenLiveConfigsPresent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/exa.language_server_pb.LanguageServerService/GetCascadeModelConfigData" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"clientModelConfigs": []map[string]interface{}{
				{
					"modelId": "gemini-4-hydrogen-low",
					"label":   "Gemini 4 Hydrogen (Low)",
					"modelOrAlias": map[string]interface{}{
						"model": "MODEL_PLACEHOLDER_M527",
					},
				},
				{
					"modelId": "gemini-4-carbon-high",
					"label":   "Gemini 4 Carbon (High)",
					"modelOrAlias": map[string]interface{}{
						"model": "MODEL_PLACEHOLDER_M420",
					},
				},
				{
					"modelId": "hydrogen-alias-high",
					"label":   "Gemini 4 Hydrogen (High)",
					"modelOrAlias": map[string]interface{}{
						"model": "MODEL_PLACEHOLDER_M525",
					},
				},
			},
			"defaultOverrideModelConfig": map[string]interface{}{
				"modelOrAlias": map[string]interface{}{
					"model": "MODEL_PLACEHOLDER_M526",
				},
			},
		})
	}))
	defer srv.Close()

	// 1. Retired model (gemini-3.8-flash-low) must NOT fall back to retired MODEL_PLACEHOLDER_M320;
	//    it must resolve to an active fallback in clientModelConfigs with a clear stderr warning.
	var gotEnum string
	var gotErr error
	stderrOut := captureStderr(t, func() {
		gotEnum, gotErr = ResolveModelEnum(srv.URL, "csrf-1", "gemini-3.8-flash-low")
	})
	if gotErr != nil {
		t.Fatalf("expected active fallback for retired gemini-3.8-flash-low, got err: %v", gotErr)
	}
	if gotEnum != "MODEL_PLACEHOLDER_M527" {
		t.Fatalf("expected active fallback MODEL_PLACEHOLDER_M527 (gemini-4-hydrogen-low), got %q", gotEnum)
	}
	if !strings.Contains(stderrOut, "gemini-3.8-flash-low") || !strings.Contains(stderrOut, "gemini-4-hydrogen-low") {
		t.Fatalf("expected clear stderr fallback warning mentioning requested and fallback models, got: %q", stderrOut)
	}

	// 2. Label-matched active model resolves directly from live clientModelConfigs without fallback warning.
	var labelEnum string
	var labelErr error
	stderrLabel := captureStderr(t, func() {
		labelEnum, labelErr = ResolveModelEnum(srv.URL, "csrf-1", "gemini-4-hydrogen-high")
	})
	if labelErr != nil || labelEnum != "MODEL_PLACEHOLDER_M525" {
		t.Fatalf("expected label-matched live enum MODEL_PLACEHOLDER_M525, got %q (%v)", labelEnum, labelErr)
	}
	if stderrLabel != "" {
		t.Fatalf("expected no fallback warning when label matches live config, got: %q", stderrLabel)
	}

	// 3. Even when a model exists in staticModelEnumMap (e.g. gemini-4-carbon-low -> M422),
	//    if live clientModelConfigs is non-empty and omits it, staticModelEnumMap must NOT be used.
	var omittedEnum string
	var omittedErr error
	stderrOmitted := captureStderr(t, func() {
		omittedEnum, omittedErr = ResolveModelEnum(srv.URL, "csrf-1", "gemini-4-carbon-low")
	})
	if omittedErr != nil {
		t.Fatalf("expected live fallback when model omitted from live list, got err: %v", omittedErr)
	}
	if omittedEnum == "MODEL_PLACEHOLDER_M422" {
		t.Fatalf("must NEVER use staticModelEnumMap enum MODEL_PLACEHOLDER_M422 when live clientModelConfigs is non-empty")
	}
	if omittedEnum != "MODEL_PLACEHOLDER_M420" {
		t.Fatalf("expected live fallback MODEL_PLACEHOLDER_M420, got %q", omittedEnum)
	}
	if !strings.Contains(stderrOmitted, "gemini-4-carbon-low") {
		t.Fatalf("expected stderr warning when model omitted from live configs, got: %q", stderrOmitted)
	}

	// 4. Vault customized routing (`config.yaml`) takes priority over built-in defaults during live fallback.
	customVault := t.TempDir()
	customCfg := "routing:\n  tiers:\n    quick: acme-custom-quick\n"
	if err := os.WriteFile(customVault+"/config.yaml", []byte(customCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	customSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"clientModelConfigs": []map[string]interface{}{
				{
					"modelId": "gemini-4-hydrogen-low",
					"modelOrAlias": map[string]interface{}{
						"model": "MODEL_PLACEHOLDER_M527",
					},
				},
				{
					"modelId": "acme-custom-quick",
					"modelOrAlias": map[string]interface{}{
						"model": "MODEL_ACME_CUSTOM_QUICK",
					},
				},
			},
		})
	}))
	defer customSrv.Close()

	var customEnum, customResID string
	var customFellBack bool
	var customErr error
	_ = captureStderr(t, func() {
		customEnum, customResID, customFellBack, customErr = resolveModelEnumLive(customVault, customSrv.URL, "csrf-1", "gemini-3.8-flash-low")
	})
	if customErr != nil || !customFellBack || customEnum != "MODEL_ACME_CUSTOM_QUICK" || customResID != "acme-custom-quick" {
		t.Fatalf("expected customized vault model acme-custom-quick to win live fallback ahead of built-in default, got enum=%q id=%q fb=%v err=%v", customEnum, customResID, customFellBack, customErr)
	}
}

func TestStaticModelEnumMapRetiredModelsRemoved(t *testing.T) {
	// Retired 3.8 Flash models and empty strings must not resolve offline via staticModelEnumMap.
	for _, retired := range []string{
		"gemini-3.8-flash-low",
		"gemini-3.8-flash-medium",
		"gemini-3.8-flash-high",
		"",
	} {
		if val, err := ResolveModelEnum("", "", retired); err == nil {
			t.Fatalf("expected error for retired/empty model %q offline, got %q", retired, val)
		}
	}

	// Active public Antigravity models resolve offline when LS is unavailable.
	activeCases := map[string]string{
		"gemini-4-neon-low":        "MODEL_PLACEHOLDER_M534",
		"gemini-4-neon-medium":     "MODEL_PLACEHOLDER_M533",
		"gemini-4-neon-high":       "MODEL_PLACEHOLDER_M532",
		"gemini-4-neon-xhigh":      "MODEL_PLACEHOLDER_M535",
		"gemini-4-hydrogen-low":    "MODEL_PLACEHOLDER_M527",
		"gemini-4-hydrogen-medium": "MODEL_PLACEHOLDER_M526",
		"gemini-4-hydrogen-high":   "MODEL_PLACEHOLDER_M525",
		"gemini-4-hydrogen-xhigh":  "MODEL_PLACEHOLDER_M528",
		"gemini-4-carbon-medium":   "MODEL_PLACEHOLDER_M421",
		"gemini-4-carbon-high":     "MODEL_PLACEHOLDER_M420",
		"claude-opus-5-5-high":     "MODEL_PLACEHOLDER_M402",
	}
	for id, wantEnum := range activeCases {
		got, err := ResolveModelEnum("", "", id)
		if err != nil || got != wantEnum {
			t.Fatalf("ResolveModelEnum(%q) = (%q, %v), want (%q, nil)", id, got, err, wantEnum)
		}
	}

	// Empty clientModelConfigs from LS falls back to staticModelEnumMap for active IDs and errors on retired IDs.
	emptySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"clientModelConfigs": []interface{}{}})
	}))
	defer emptySrv.Close()

	if got, err := ResolveModelEnum(emptySrv.URL, "csrf", "gemini-4-hydrogen-low"); err != nil || got != "MODEL_PLACEHOLDER_M527" {
		t.Fatalf("expected static fallback M527 on empty live list, got %q (%v)", got, err)
	}
	if got, err := ResolveModelEnum(emptySrv.URL, "csrf", "gemini-3.8-flash-low"); err == nil {
		t.Fatalf("expected error for retired model on empty live list, got %q", got)
	}
}

func TestResolveModelEnumNeonLiveAndFallback(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/exa.language_server_pb.LanguageServerService/GetCascadeModelConfigData" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"clientModelConfigs": []map[string]interface{}{
				{
					"modelId": "gemini-4-neon-medium",
					"label":   "Gemini 4 Neon (Medium)",
					"modelOrAlias": map[string]interface{}{
						"model": "MODEL_PLACEHOLDER_M533",
					},
				},
				{
					"modelId": "gemini-4-neon-high",
					"label":   "Gemini 4 Neon (High)",
					"modelOrAlias": map[string]interface{}{
						"model": "MODEL_PLACEHOLDER_M532",
					},
				},
			},
		})
	}))
	defer srv.Close()

	var gotEnum string
	var gotErr error
	stderrExact := captureStderr(t, func() {
		gotEnum, gotErr = ResolveModelEnum(srv.URL, "csrf-neon", "gemini-4-neon-medium")
	})
	if gotErr != nil || gotEnum != "MODEL_PLACEHOLDER_M533" || stderrExact != "" {
		t.Fatalf("expected direct live match MODEL_PLACEHOLDER_M533 with no warning, got enum=%q err=%v stderr=%q", gotEnum, gotErr, stderrExact)
	}

	// Omitted gemini-4-neon-xhigh falls back to active neon high rather than stale static M535.
	var fbEnum string
	var fbErr error
	stderrFB := captureStderr(t, func() {
		fbEnum, fbErr = ResolveModelEnum(srv.URL, "csrf-neon", "gemini-4-neon-xhigh")
	})
	if fbErr != nil || fbEnum != "MODEL_PLACEHOLDER_M532" {
		t.Fatalf("expected live fallback MODEL_PLACEHOLDER_M532 for omitted gemini-4-neon-xhigh, got %q (%v)", fbEnum, fbErr)
	}
	if !strings.Contains(stderrFB, "gemini-4-neon-xhigh") || !strings.Contains(stderrFB, "gemini-4-neon-high") {
		t.Fatalf("expected fallback warning mentioning requested and fallback neon models, got %q", stderrFB)
	}
}

func TestDispatchPendingEventsFastFailsOnStep1ExecutorError(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 18, 18, 0, 0, 0, time.UTC)
	failedConv := "aaaaaaaa-1111-4111-8111-111111111111"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/exa.language_server_pb.LanguageServerService/GetCascadeModelConfigData":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"clientModelConfigs": []map[string]interface{}{
					{
						"modelId": "gemini-4-hydrogen-low",
						"modelOrAlias": map[string]interface{}{
							"model": "MODEL_PLACEHOLDER_M527",
						},
					},
				},
			})
		case "/exa.language_server_pb.LanguageServerService/StartCascade":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"cascadeId": failedConv})
		case "/exa.language_server_pb.LanguageServerService/SendUserCascadeMessage":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		case "/exa.language_server_pb.LanguageServerService/GetCascadeTrajectory":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":        "CASCADE_RUN_STATUS_IDLE",
				"numTotalSteps": 2,
				"trajectory": map[string]interface{}{
					"steps": []map[string]interface{}{
						{"type": "CORTEX_STEP_TYPE_USER_INPUT", "status": "CORTEX_STEP_STATUS_DONE"},
						{
							"type":   "CORTEX_STEP_TYPE_ERROR_MESSAGE",
							"status": "CORTEX_STEP_STATUS_ERROR",
							"errorMessage": map[string]interface{}{
								"error": map[string]interface{}{
									"shortError": "failed to construct executor: unknown model key MODEL_PLACEHOLDER_M320: model not found",
								},
							},
						},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("ANTIGRAVITY_LS_ADDRESS", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-step1")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")

	ev, _, err := events.Enqueue(vaultDir, events.Event{
		Source:   "gatekeeper",
		ScopeKey: "fast-fail-scope",
		Skill:    "inbox-triage",
		Title:    "[Auto] Fast Fail Test",
		Prompt:   "Check inbox",
		Model:    "MODEL_PLACEHOLDER_M320",
	}, now)
	if err != nil {
		t.Fatalf("events.Enqueue: %v", err)
	}

	acts, err := DispatchPendingEvents(vaultDir, now, false)
	if err != nil {
		t.Fatalf("DispatchPendingEvents: %v", err)
	}
	if len(acts) != 1 || !strings.Contains(acts[0], "deferred:") || !strings.Contains(acts[0], "failed to construct executor") {
		t.Fatalf("expected immediate fast-fail deferral with executor error, got: %v", acts)
	}

	// Verify event is NOT wedged under a 15m lease: status must be pending with cleared lease fields.
	inbox, err := events.List(vaultDir, "", false, 0)
	if err != nil || len(inbox) != 1 || inbox[0].ID != ev.ID {
		t.Fatalf("events.List: %+v (%v)", inbox, err)
	}
	if inbox[0].Status != events.StatusPending || inbox[0].LeaseID != "" || inbox[0].LeaseConvID != "" || inbox[0].LeaseExpiresAt != "" {
		t.Fatalf("expected cleared lease after fast-fail, got: %+v", inbox[0])
	}
	if inbox[0].AttemptCount != 1 {
		t.Fatalf("expected AttemptCount=1, got %d", inbox[0].AttemptCount)
	}

	// Verify AutoSpawnsToday was not consumed by the fast-failed attempt.
	if q := LoadQuota(vaultDir); q.EffectiveAutoSpawns() != 0 {
		t.Fatalf("expected AutoSpawnsToday=0 after fast-fail spawn, got %d", q.EffectiveAutoSpawns())
	}

	// Verify dead 2-step conversation was queued for sidebar archive and unbound task was cleaned up.
	qItems := LoadSidebarSyncQueue(vaultDir)
	foundArchive := false
	for _, it := range qItems.Items {
		if it.ConversationID == failedConv && it.DesiredArchived && it.LastError == "spawn_step1_error" {
			foundArchive = true
			break
		}
	}
	if !foundArchive {
		t.Fatalf("expected dead failed conversation %s queued for sidebar archive, got %+v", failedConv, qItems.Items)
	}
	if tasks, _ := ListTasks(vaultDir); len(tasks) != 0 {
		t.Fatalf("expected unbound task removed after fast-fail spawn, got %+v", tasks)
	}
}

func TestReconcileAutoTasksReleasesLeaseOnStep1ErrorAndIsSafeForRunning(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 18, 18, 10, 0, 0, time.UTC)
	failedConv := "bbbbbbbb-2222-4222-8222-222222222222"
	healthyConv := "cccccccc-3333-4333-8333-333333333333"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/exa.language_server_pb.LanguageServerService/GetCascadeTrajectory" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			CascadeID string `json:"cascadeId"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.CascadeID {
		case failedConv:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":        "CASCADE_RUN_STATUS_IDLE",
				"numTotalSteps": 2,
				"trajectory": map[string]interface{}{
					"steps": []map[string]interface{}{
						{"type": "CORTEX_STEP_TYPE_USER_INPUT", "status": "CORTEX_STEP_STATUS_DONE"},
						{
							"type":   "CORTEX_STEP_TYPE_ERROR_MESSAGE",
							"status": "CORTEX_STEP_STATUS_ERROR",
							"errorMessage": map[string]interface{}{
								"message": "failed to construct executor: unknown model key MODEL_PLACEHOLDER_M320: model not found",
							},
						},
					},
				},
			})
		case healthyConv:
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":        "CASCADE_RUN_STATUS_RUNNING",
				"numTotalSteps": 2,
				"trajectory": map[string]interface{}{
					"steps": []map[string]interface{}{
						{"type": "CORTEX_STEP_TYPE_USER_INPUT", "status": "CORTEX_STEP_STATUS_DONE"},
						{"type": "CORTEX_STEP_TYPE_PLANNER_RESPONSE", "status": "CORTEX_STEP_STATUS_RUNNING"},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("ANTIGRAVITY_LS_ADDRESS", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-reconcile")

	evFail, _, err := events.Enqueue(vaultDir, events.Event{
		Source:   "gatekeeper",
		ScopeKey: "wedged-scope",
		Skill:    "inbox-triage",
		Title:    "[Auto] Wedged Scope",
		Prompt:   "Check mail",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	evHealthy, _, err := events.Enqueue(vaultDir, events.Event{
		Source:   "gatekeeper",
		ScopeKey: "healthy-scope",
		Skill:    "morning-brief",
		Title:    "[Auto] Healthy Scope",
		Prompt:   "Run brief",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = events.LeaseNextBatches(vaultDir, now, events.DefaultLeaseTTL)
	if err != nil {
		t.Fatal(err)
	}
	_ = events.BindBatchConversation(vaultDir, []string{evFail.ID}, failedConv, now, events.DefaultLeaseTTL)
	_ = events.BindBatchConversation(vaultDir, []string{evHealthy.ID}, healthyConv, now, events.DefaultLeaseTTL)

	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "auto-wedged-scope",
		Title:          "[Auto] Wedged Scope",
		Kind:           "auto",
		Scope:          "wedged-scope",
		ConversationID: failedConv,
		EventIDs:       []string{evFail.ID},
		CreatedAt:      now.Format(time.RFC3339),
		LastUpdatedAt:  now.Format(time.RFC3339),
		Status:         "active",
	})
	_ = RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "auto-healthy-scope",
		Title:          "[Auto] Healthy Scope",
		Kind:           "auto",
		Scope:          "healthy-scope",
		ConversationID: healthyConv,
		EventIDs:       []string{evHealthy.ID},
		CreatedAt:      now.Format(time.RFC3339),
		LastUpdatedAt:  now.Format(time.RFC3339),
		Status:         "active",
	})

	reconciled, err := ReconcileAutoTasks(vaultDir, now.Add(5*time.Second), false)
	if err != nil {
		t.Fatalf("ReconcileAutoTasks: %v", err)
	}
	if len(reconciled) != 1 || reconciled[0] != "auto-wedged-scope" {
		t.Fatalf("expected only auto-wedged-scope reconciled, got %v", reconciled)
	}

	allEvs, err := events.List(vaultDir, "", false, 0)
	if err != nil {
		t.Fatal(err)
	}
	byEvID := map[string]events.Event{}
	for _, ev := range allEvs {
		byEvID[ev.ID] = ev
	}
	if got := byEvID[evFail.ID]; got.Status != events.StatusPending || got.LeaseID != "" || got.LeaseConvID != "" {
		t.Fatalf("expected failed event lease released immediately, got: %+v", got)
	}
	if got := byEvID[evHealthy.ID]; got.Status != events.StatusLeased || got.LeaseConvID != healthyConv {
		t.Fatalf("expected healthy running event to remain leased, got: %+v", got)
	}

	tasks, _ := ListTasks(vaultDir)
	bySlug := map[string]ActiveTask{}
	for _, tk := range tasks {
		bySlug[tk.TaskSlug] = tk
	}
	if bySlug["auto-wedged-scope"].Status != "archived" || !strings.Contains(bySlug["auto-wedged-scope"].LastSummary, "Executor error at step 1") {
		t.Fatalf("expected auto-wedged-scope archived with step 1 summary, got: %+v", bySlug["auto-wedged-scope"])
	}
	if bySlug["auto-healthy-scope"].Status != "active" {
		t.Fatalf("expected auto-healthy-scope to remain active, got: %+v", bySlug["auto-healthy-scope"])
	}
}

func TestRegisterTaskResetsStaleLastSummaryOnAutoRetry(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 18, 18, 20, 0, 0, time.UTC)

	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "auto-mail-watch-evt1",
		Title:          "[Auto] Mail Watch",
		Kind:           "auto",
		Scope:          "mail-watch",
		ConversationID: "conv-attempt-1",
		TurnCount:      2,
		LastSummary:    "Executor error at step 1: stale failure summary",
		CreatedAt:      now.Format(time.RFC3339),
		LastUpdatedAt:  now.Format(time.RFC3339),
		Status:         "archived",
	}); err != nil {
		t.Fatalf("RegisterTask attempt 1: %v", err)
	}

	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "auto-mail-watch-evt1",
		Title:          "[Auto] Mail Watch",
		Kind:           "auto",
		Scope:          "mail-watch",
		ConversationID: "conv-attempt-2",
		LastUpdatedAt:  now.Add(1 * time.Minute).Format(time.RFC3339),
		Status:         "active",
	}); err != nil {
		t.Fatalf("RegisterTask retry reactivation: %v", err)
	}

	got, ok := FindTaskByConversationID(vaultDir, "conv-attempt-2")
	if !ok {
		t.Fatalf("expected retry task bound to conv-attempt-2")
	}
	if got.LastSummary != "" {
		t.Fatalf("expected stale LastSummary reset on auto task retry, got %q", got.LastSummary)
	}
	if got.TurnCount != 0 {
		t.Fatalf("expected TurnCount reset to 0 on new retry conversation, got %d", got.TurnCount)
	}

	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "auto-mail-watch-evt1",
		ConversationID: "conv-attempt-2",
		LastSummary:    "Attempt 2 partial summary",
		Status:         "active",
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "auto-mail-watch-evt1",
		Kind:           "auto",
		ConversationID: "conv-attempt-3",
		Status:         "active",
	}); err != nil {
		t.Fatal(err)
	}
	got3, ok := FindTaskByConversationID(vaultDir, "conv-attempt-3")
	if !ok || got3.LastSummary != "" {
		t.Fatalf("expected stale LastSummary reset when active auto task switches to conv-attempt-3, got ok=%v summary=%q", ok, got3.LastSummary)
	}

	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "auto-mail-watch-evt1",
		ConversationID: "conv-attempt-3",
		LastSummary:    "Attempt 3 completed cleanly",
		Status:         "completed",
	}); err != nil {
		t.Fatal(err)
	}
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       "auto-mail-watch-evt1",
		ConversationID: "conv-attempt-3",
		Status:         "archived",
	}); err != nil {
		t.Fatal(err)
	}
	gotArch, _ := FindTaskByConversationID(vaultDir, "conv-attempt-3")
	if gotArch.LastSummary != "Attempt 3 completed cleanly" {
		t.Fatalf("expected normal archival update to preserve LastSummary, got %q", gotArch.LastSummary)
	}
}

func TestDispatchPendingEventsRetryResetsStaleLastSummary(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 18, 18, 30, 0, 0, time.UTC)
	uAttempt1 := "dddddddd-1111-4111-8111-111111111101"
	uAttempt2 := "dddddddd-2222-4222-8222-222222222202"
	spawnCount := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/exa.language_server_pb.LanguageServerService/GetCascadeModelConfigData":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"clientModelConfigs": []map[string]interface{}{
					{
						"modelId": "gemini-4-hydrogen-low",
						"modelOrAlias": map[string]interface{}{
							"model": "MODEL_PLACEHOLDER_M527",
						},
					},
				},
			})
		case "/exa.language_server_pb.LanguageServerService/StartCascade":
			spawnCount++
			cid := uAttempt1
			if spawnCount >= 2 {
				cid = uAttempt2
			}
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"cascadeId": cid})
		case "/exa.language_server_pb.LanguageServerService/SendUserCascadeMessage":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{})
		case "/exa.language_server_pb.LanguageServerService/GetCascadeTrajectory":
			_ = json.NewEncoder(w).Encode(map[string]interface{}{
				"status":        "CASCADE_RUN_STATUS_RUNNING",
				"numTotalSteps": 2,
				"trajectory": map[string]interface{}{
					"steps": []map[string]interface{}{
						{"type": "CORTEX_STEP_TYPE_USER_INPUT", "status": "CORTEX_STEP_STATUS_DONE"},
						{"type": "CORTEX_STEP_TYPE_PLANNER_RESPONSE", "status": "CORTEX_STEP_STATUS_RUNNING"},
					},
				},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	t.Setenv("ANTIGRAVITY_LS_ADDRESS", strings.TrimPrefix(srv.URL, "http://"))
	t.Setenv("ANTIGRAVITY_CSRF_TOKEN", "csrf-retry")
	t.Setenv("ANTIGRAVITY_PROJECT_ID", "")

	ev, _, err := events.Enqueue(vaultDir, events.Event{
		Source:   "gatekeeper",
		ScopeKey: "summary-reset-scope",
		Skill:    "inbox-triage",
		Title:    "[Auto] Summary Reset",
		Prompt:   "Check mail",
		Model:    "gemini-4-hydrogen-low",
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	autoSlug := "auto-" + slugify("summary-reset-scope-"+ev.ID)

	if _, err := DispatchPendingEvents(vaultDir, now, false); err != nil {
		t.Fatalf("Attempt 1 DispatchPendingEvents: %v", err)
	}
	if err := RegisterTask(vaultDir, ActiveTask{
		TaskSlug:       autoSlug,
		ConversationID: uAttempt1,
		LastSummary:    "Stale summary from attempt 1",
		Status:         "archived",
	}); err != nil {
		t.Fatal(err)
	}

	tReap := now.Add(events.DefaultLeaseTTL + time.Minute)
	_, _ = DispatchPendingEvents(vaultDir, tReap, false)
	tRetry := tReap.Add(2 * time.Minute)
	if _, err := DispatchPendingEvents(vaultDir, tRetry, false); err != nil {
		t.Fatalf("Attempt 2 DispatchPendingEvents: %v", err)
	}

	got, ok := FindTaskByConversationID(vaultDir, uAttempt2)
	if !ok {
		t.Fatalf("expected task bound to %s", uAttempt2)
	}
	if got.LastSummary != "" {
		t.Fatalf("expected stale LastSummary cleared on DispatchPendingEvents retry, got %q", got.LastSummary)
	}
}
