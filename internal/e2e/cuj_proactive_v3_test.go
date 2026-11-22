package e2e

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/pulse"
	"github.com/zjshen14/agy-hq/internal/rsi"
	"github.com/zjshen14/agy-hq/internal/timers"
	"github.com/zjshen14/agy-hq/internal/webui"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

func mustRunV3(t *testing.T, env *hqEnv, args ...string) string {
	t.Helper()
	out, err := env.run("", args...)
	if err != nil {
		t.Fatalf("hq %v failed: %v\n%s", args, err, out)
	}
	return out
}

// CUJ-45: 0-Token Pulse Look-Ahead (D1-D10), 3-State Goal Readiness, Unavailable-Window Ripple,
// Workspace NeedsOwner Nudge, 3m Outbox Burst Coalescing & Scope Rules (RFC-0027).
func TestCUJ45_PulseAnticipationReadinessAndBurstCoalescing(t *testing.T) {
	env := setupHQEnv(t)

	// Configure 3m burst coalescing + weekday personal scope rule in Vault config.yaml
	cfgYAML := `attention:
  mode: balanced
  cooldown_per_topic: "4h"
  coalesce_window: "3m"
  incident_correlation_window: "30m"
  scope_rules:
    - scopes: ["personal"]
      schedule: "weekdays 09:00-17:30 UTC"
      downgrade_non_critical_interrupt_to: "brief_rollup"
pulse:
  enabled: true
  pre_meeting_lead: "30m"
  countdown_horizons: ["168h", "72h", "24h"]
  cutoff_lead: "6h"
  min_slot_buffer: "45m"
  stale_draft_age: "4h"
`
	_ = os.MkdirAll(env.vaultDir, 0o700)
	if err := os.WriteFile(filepath.Join(env.vaultDir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}

	// 1. Create an unavailable-window Goal with 3-state checklist items via CLI
	winStart := time.Now().Add(24 * time.Hour).UTC().Format("2006-01-02")
	winEnd := time.Now().Add(5 * 24 * time.Hour).UTC().Format("2006-01-02")
	dueDuring := time.Now().Add(3 * 24 * time.Hour).UTC().Format("2006-01-02")

	mustRunV3(t, env, "goals", "add", "--slug=q4-cutover", "--title=Q4 Release Cutover", "--scope=engineering",
		"--window-start="+winStart, "--window-end="+winEnd, "--tz=UTC", "--blocks-availability", "--coverage-tag=oncall")
	mustRunV3(t, env, "goals", "checklist", "add", "--goal=q4-cutover", "--id=chk-ci", "--title=CI Matrix Green", "--status=open")
	mustRunV3(t, env, "goals", "checklist", "confirm", "--goal=q4-cutover", "--id=chk-ci", "--ref=PR#404", "--note=Green")
	mustRunV3(t, env, "goals", "checklist", "add", "--goal=q4-cutover", "--id=chk-docs", "--title=Publish Changelog", "--status=open")
	mustRunV3(t, env, "goals", "checklist", "add", "--goal=q4-cutover", "--id=chk-opt", "--title=Optional Webinar", "--status=open")
	mustRunV3(t, env, "goals", "checklist", "skip", "--goal=q4-cutover", "--id=chk-opt", "--reason=Deferred")

	// Seed colliding Goal due during unavailable window (D3)
	mustRunV3(t, env, "goals", "add", "--slug=vendor-signoff", "--title=Vendor Security Signoff", "--scope=ops", "--target="+dueDuring)

	// Seed D1 upcoming 1:1 in 20m + D9 workspace NeedsOwner: true
	_ = os.MkdirAll(filepath.Join(env.vaultDir, "memory", "pulse"), 0o700)
	calBytes, _ := json.Marshal([]pulse.CalendarSlot{
		{ID: "cal-1on1", Title: "1:1 Sync with Alice", StartAt: time.Now().Add(20 * time.Minute).UTC().Format(time.RFC3339), Attendees: []string{"alice@example.com"}, Scope: "engineering"},
	})
	_ = os.WriteFile(filepath.Join(env.vaultDir, "memory", "pulse", "calendar_cache.json"), calBytes, 0o600)

	_ = workspace.UpdateLedger(env.vaultDir, func(l *workspace.Ledger) (bool, error) {
		l.Put(workspace.LedgerEntry{
			Provider:    "core/git-worktree",
			WorkspaceID: "ws-review",
			Name:        "headquarter-review",
			Root:        filepath.Join(env.vaultDir, "worktrees", "headquarter-review"),
			Ownership:   workspace.OwnershipOwned,
			Status: workspace.LedgerStatus{
				State:   workspace.StateClean,
				Changes: []workspace.ChangeRef{{ID: "PR#404", State: workspace.ChangeInReview, NeedsOwner: true, Title: "Reviewer requested change"}},
			},
		})
		return true, nil
	})

	// 2. Run `hq pulse precheck --json` -> fires D1, D2, D3, D9 in 0 LLM tokens
	out1 := mustRunV3(t, env, "pulse", "precheck", "--json")
	var rep1 pulse.PrecheckReport
	if err := json.Unmarshal([]byte(out1), &rep1); err != nil {
		t.Fatalf("parse pulse precheck JSON: %v\n%s", err, out1)
	}
	if rep1.EmittedCount < 4 {
		t.Fatalf("expected >=4 pulse findings, got %+v", rep1)
	}
	foundDet := map[string]bool{}
	for _, f := range rep1.Findings {
		foundDet[f.Detector] = true
	}
	for _, want := range []string{"D1:pre_meeting_prep", "D3:blocking_window_ripple", "D9:workspace_review_pulse"} {
		if !foundDet[want] {
			t.Errorf("missing expected detector %s in %+v", want, rep1.Findings)
		}
	}

	// 3. Re-run immediately -> 0 new emissions (idempotent 0-token gatekeeper)
	out2 := mustRunV3(t, env, "pulse", "precheck", "--json")
	var rep2 pulse.PrecheckReport
	if err := json.Unmarshal([]byte(out2), &rep2); err != nil {
		t.Fatalf("parse pulse precheck 2: %v", err)
	}
	if rep2.EmittedCount != 0 {
		t.Fatalf("expected 0 new emissions on second idle tick, got %d: %+v", rep2.EmittedCount, rep2.Findings)
	}

	// 4. Verify 3m burst coalescing via CLI `hq outbox add --incident-key=...`
	mustRunV3(t, env, "outbox", "add", "--tier=interrupt", "--skill=inbox-triage", "--scope=travel",
		"--incident-key=sender:airline@example.com", "--title=Verification Code 1", "--summary=First security email")
	secondBurst := mustRunV3(t, env, "outbox", "add", "--tier=interrupt", "--skill=inbox-triage", "--scope=travel",
		"--incident-key=sender:airline@example.com", "--title=Verification Code 2", "--summary=Second security email", "--json")
	if !strings.Contains(secondBurst, `"cooledDown": true`) || !strings.Contains(secondBurst, "Verification Code 2") {
		t.Fatalf("expected burst coalescing merge on second alert, got:\n%s", secondBurst)
	}

	// 5. Verify 20-case golden `hq outbox eval` passes 100%
	evalOut := mustRunV3(t, env, "outbox", "eval", "--json")
	var evalRep outbox.EvalReport
	if err := json.Unmarshal([]byte(evalOut), &evalRep); err != nil || evalRep.TotalCases < 20 || !evalRep.AllThresholdsMet {
		t.Fatalf("expected >=20 passing eval cases, got %+v (%v)", evalRep, err)
	}
}

// CUJ-46: 1-Tap DecisionOption Cards, Ring-0 Taint Safety, >12h Expired Proposal Catch,
// 0-Token `hq approvals restage`, `hq cancel`, and `dismiss_noisy` -> RSI Mining (RFC-0027).
func TestCUJ46_DecisionOptionCardsTaintSafetyAndProposalRestage(t *testing.T) {
	env := setupHQEnv(t)
	now := time.Now()

	// 1. Stage a proposal and expire it >14h ago (beyond legacy 12h brief window)
	prop, err := approvals.CreateWithOptions(env.vaultDir, approvals.CreateOptions{
		ActionType:      "messaging",
		Summary:         "Reply to Alice on Q4 API spec",
		Command:         "hq gchat send --to=alice@example.com --body=Approved",
		CommandArgv:     []string{"hq", "gchat", "send", "--to=alice@example.com", "--body=Approved"},
		TargetConnector: "gchat",
		TargetAccount:   "work",
		TargetThreadKey: "thr-api-spec",
		OriginSkill:     "inbox-triage",
	})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = approvals.ExpireStaleProposals(env.vaultDir, now.Add(48*time.Hour))

	// `hq approvals expired` catches it unconditionally
	expOut := mustRunV3(t, env, "approvals", "expired", "--json")
	if !strings.Contains(expOut, prop.ID) {
		t.Fatalf("expected expired proposal %s in `hq approvals expired`, got:\n%s", prop.ID, expOut)
	}

	// 2. Ring-0 taint safety: tainted outbox item rejecting cross-proposal DecisionOption
	if _, _, tErr := outbox.Add(env.vaultDir, outbox.OutboxItem{
		SourceSkill: "inbox-triage",
		Tier:        outbox.TierInterrupt,
		Tainted:     true,
		ProposalIDs: []string{prop.ID},
		Title:       "External email",
		Summary:     "Untrusted summary",
		Decisions: []outbox.DecisionOption{
			{ID: "smuggle", Label: "Approve Unrelated", Kind: outbox.ActionApproveProposal, ProposalID: "prop-9999999999-abcdef"},
		},
	}, now); tErr == nil {
		t.Fatalf("expected Ring-0 taint check to reject unrelated proposal reference in DecisionOption")
	}

	// 3. Emit a valid DecisionOption card via `hq pulse precheck` (D6 catches expired proposal) and execute `restage` in 0 tokens via CLI
	pulseOut := mustRunV3(t, env, "pulse", "precheck", "--json")
	var pRep pulse.PrecheckReport
	_ = json.Unmarshal([]byte(pulseOut), &pRep)
	expiredOutboxID := ""
	for _, f := range pRep.Findings {
		if f.Detector == "D6:proposal_expired" {
			expiredOutboxID = f.OutboxID
			break
		}
	}
	if expiredOutboxID == "" {
		t.Fatalf("expected D6:proposal_expired finding, got %+v", pRep.Findings)
	}
	actOut := mustRunV3(t, env, "outbox", "act", expiredOutboxID, "--option=restage", "--json")
	if !strings.Contains(actOut, `"newProposalId": "prop-`) {
		t.Fatalf("expected 0-token restage to mint fresh proposal ID, got:\n%s", actOut)
	}

	// 4. Verify `hq cancel` reverses queued event + armed timer + pending proposal in 1 command and logs activity
	_, _, _ = events.Enqueue(env.vaultDir, events.Event{
		Source:   "decision_action",
		ScopeKey: "scope:abort-me",
		Skill:    "swe-pulse",
		Title:    "Investigate abort-me",
		Prompt:   "Check abort-me",
	}, now)
	_, _ = timers.SetFollowUp(env.vaultDir, timers.FollowUpRequest{
		InDuration: 30 * time.Minute,
		ScopeKey:   "scope:abort-me",
		Title:      "Follow up abort-me",
		Prompt:     "Ping",
	}, now)
	cancelOut := mustRunV3(t, env, "cancel", "abort-me", "--reason=Changed my mind", "--json")
	var cRep kernel.CancelReport
	if err := json.Unmarshal([]byte(cancelOut), &cRep); err != nil || cRep.TotalCancelled != 2 {
		t.Fatalf("expected 2 items cancelled (event+timer), got %+v (%v)", cRep, err)
	}
	if acts := kernel.LoadRecentActivity(env.vaultDir, 5); len(acts) == 0 || acts[0].Kind != "reverted_by_user" {
		t.Fatalf("expected reverted_by_user in activity.jsonl, got %+v", acts)
	}

	// 5. Seed 5 interrupts for noisy-skill with 3 marked dismiss_noisy (60% >= 40%) -> `hq rsi mine` detects FrictionProactiveNoise
	for i := 1; i <= 5; i++ {
		id := "nint-" + string(rune('0'+i))
		_, _, _ = outbox.Add(env.vaultDir, outbox.OutboxItem{
			ID:          id,
			SourceSkill: "noisy-watcher",
			Scope:       "ops",
			Tier:        outbox.TierInterrupt,
			Title:       "Routine ping #" + id,
			Summary:     "Low value alert",
			Decisions: []outbox.DecisionOption{
				{ID: "handled", Label: "Handled", Kind: outbox.ActionDismissHandled},
				{ID: "noisy", Label: "Mute", Kind: outbox.ActionDismissNoisy},
			},
		}, now.Add(time.Duration(i)*time.Minute))
		opt := "handled"
		if i <= 3 {
			opt = "noisy"
		}
		mustRunV3(t, env, "outbox", "act", id, "--option="+opt)
	}
	obsList, err := rsi.Mine(env.rootDir, env.vaultDir, 24*time.Hour)
	if err != nil {
		t.Fatalf("rsi.Mine: %v", err)
	}
	foundNoise := false
	for _, ob := range obsList {
		if ob.Category == rsi.FrictionProactiveNoise && ob.SkillOrPkg == "noisy-watcher" && strings.Contains(ob.CommandPattern, "interrupt_noisy:") {
			foundNoise = true
		}
	}
	if !foundNoise {
		t.Fatalf("expected FrictionProactiveNoise for noisy-watcher (>=40%% dismiss_noisy), got: %+v", obsList)
	}
}

// CUJ-47: Composable Living Spaces — Single- vs. Multi-Locale Framework/Vault Split,
// 0-Token Goal & Workspace Data Sources, Remote Artifact Export & `:4242` Strict CSP Sandbox (RFC-0027).
func TestCUJ47_ComposableLivingSpacesI18nDataSourcesAndCSPSandbox(t *testing.T) {
	env := setupHQEnv(t)
	now := time.Now()

	// Seed a workspace + 3-state Goal
	_ = workspace.UpdateLedger(env.vaultDir, func(l *workspace.Ledger) (bool, error) {
		l.Put(workspace.LedgerEntry{
			Provider:    "core/git-worktree",
			WorkspaceID: "ws-launch",
			Name:        "headquarter-launch",
			Root:        filepath.Join(env.vaultDir, "worktrees", "headquarter-launch"),
			Ownership:   workspace.OwnershipOwned,
			Status: workspace.LedgerStatus{
				State:   workspace.StateClean,
				Changes: []workspace.ChangeRef{{ID: "PR#77", State: workspace.ChangeInReview, NeedsOwner: true}},
			},
		})
		return true, nil
	})
	mustRunV3(t, env, "goals", "add", "--slug=launch-v1", "--title=Launch v1.0", "--scope=engineering", "--window-start=2026-12-01")
	mustRunV3(t, env, "goals", "checklist", "add", "--goal=launch-v1", "--id=c1", "--title=Cut tag", "--status=confirmed", "--ref=v1.0.0")
	mustRunV3(t, env, "goals", "checklist", "add", "--goal=launch-v1", "--id=c2", "--title=Publish notes", "--status=open")
	mustRunV3(t, env, "goals", "checklist", "add", "--goal=launch-v1", "--id=c3", "--title=Optional blog", "--status=open")
	mustRunV3(t, env, "goals", "checklist", "skip", "--goal=launch-v1", "--id=c3", "--reason=Skipped")

	// 1. Create single-locale Living Space from JSON with hq_builtin workspaces dataSource
	spaceSpec := `{
  "slug": "launch-cockpit",
  "title": "Launch v1.0 War-Room",
  "goalSlug": "launch-v1",
  "perspectives": [{"id": "exec", "label": "Stakeholder"}, {"id": "eng", "label": "Engineering"}],
  "dataSources": [{"id": "ds-ws", "kind": "hq_builtin", "target": "workspaces"}],
  "blocks": [
    {"id": "ws-tbl", "type": "comparison_table", "title": "Active Workspaces", "dataSourceId": "ds-ws"},
    {"id": "notes", "type": "callout_notes", "title": "Runbook", "notes": [{"tone": "warn", "title": "Rollback", "body": "Revert tag"}]}
  ]
}`
	specPath := filepath.Join(t.TempDir(), "launch.json")
	_ = os.WriteFile(specPath, []byte(spaceSpec), 0o600)

	mustRunV3(t, env, "space", "create", "--from-json="+specPath)
	mustRunV3(t, env, "space", "refresh", "launch-cockpit")

	// Verify single-locale hides language switcher, includes live workspace row + 1/2 readiness
	rawHTMLBytes, err := os.ReadFile(filepath.Join(env.vaultDir, "living_spaces", "launch-cockpit", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	rawHTML := string(rawHTMLBytes)
	if strings.Contains(rawHTML, `class="lang-switch"`) {
		t.Fatalf("single-locale framework default must hide lang-switch")
	}
	if !strings.Contains(rawHTML, "headquarter-launch") || !strings.Contains(rawHTML, "1 / 2 (50%)") || !strings.Contains(rawHTML, "Skipped (1") {
		t.Fatalf("expected refreshed workspace row and 3-state readiness in compiled HTML")
	}

	// 2. Export as Antigravity generative_ui artifact (remote-compatible without localhost:4242)
	artDir := filepath.Join(t.TempDir(), "brain-art")
	mustRunV3(t, env, "space", "export-artifact", "launch-cockpit", "--dir="+artDir)
	if _, err := os.Stat(filepath.Join(artDir, "space_launch-cockpit.html")); err != nil {
		t.Fatalf("expected exported artifact HTML: %v", err)
	}
	if _, err := os.Stat(filepath.Join(artDir, "space_launch-cockpit.html.metadata.json")); err != nil {
		t.Fatalf("expected exported artifact metadata JSON: %v", err)
	}

	// 3. Vault bilingual override ["en", "zh"] enables instant client-side language toggle
	_ = os.WriteFile(filepath.Join(env.vaultDir, "config.yaml"), []byte("living_spaces:\n  default_locales: [\"en\", \"zh\"]\n"), 0o600)
	biSpec := `{"slug":"bilingual-plan","title":{"en":"Team Offsite","zh":"团队团建"}}`
	biPath := filepath.Join(t.TempDir(), "bi.json")
	_ = os.WriteFile(biPath, []byte(biSpec), 0o600)
	mustRunV3(t, env, "space", "create", "--from-json="+biPath)
	biHTML, _ := os.ReadFile(filepath.Join(env.vaultDir, "living_spaces", "bilingual-plan", "index.html"))
	if !strings.Contains(string(biHTML), `class="lang-switch"`) || !strings.Contains(string(biHTML), "团队团建") {
		t.Fatalf("expected bilingual lang-switch and zh translation when configured in Vault")
	}

	// 4. Verify `:4242/living-spaces/launch-cockpit` enforces <iframe sandbox="allow-scripts"> (no allow-same-origin) + default-src 'none' CSP
	h := webui.NewHandler(env.vaultDir)
	reqWrap := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/living-spaces/launch-cockpit", nil)
	recWrap := httptest.NewRecorder()
	h.ServeHTTP(recWrap, reqWrap)
	if recWrap.Code != http.StatusOK || !strings.Contains(recWrap.Body.String(), `sandbox="allow-scripts"`) || strings.Contains(recWrap.Body.String(), "allow-same-origin") {
		t.Fatalf("unexpected wrapper sandbox response (%d): %s", recWrap.Code, recWrap.Body.String())
	}
	reqRaw := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:4242/living-spaces/launch-cockpit/raw", nil)
	recRaw := httptest.NewRecorder()
	h.ServeHTTP(recRaw, reqRaw)
	if recRaw.Code != http.StatusOK || !strings.Contains(recRaw.Header().Get("Content-Security-Policy"), "default-src 'none'") {
		t.Fatalf("expected strict CSP on raw living space (%d): csp=%q", recRaw.Code, recRaw.Header().Get("Content-Security-Policy"))
	}
	_ = now
}
