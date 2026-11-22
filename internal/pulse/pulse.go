package pulse

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/workspace"
)

// CalendarSlot represents a cached upcoming calendar event for D1 pre-meeting prep.
type CalendarSlot struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	StartAt   string   `json:"startAt"` // RFC3339
	EndAt     string   `json:"endAt,omitempty"`
	Attendees []string `json:"attendees,omitempty"`
	Scope     string   `json:"scope,omitempty"`
	Location  string   `json:"location,omitempty"`
}

// DraftEntry represents a cached standalone unsent draft for D10 stale-draft checks.
type DraftEntry struct {
	ID        string `json:"id"`
	Connector string `json:"connector,omitempty"`
	Recipient string `json:"recipient,omitempty"`
	Subject   string `json:"subject"`
	UpdatedAt string `json:"updatedAt"` // RFC3339
}

// State persists deduplication watermarks for `hq pulse` under RankPulse = 32.
type State struct {
	SchemaVersion       int               `json:"schemaVersion"`
	LastRunAt           string            `json:"lastRunAt,omitempty"`
	FiredKeys           map[string]string `json:"firedKeys,omitempty"`           // dedupKey -> RFC3339
	WorkspaceNeedsOwner map[string]int    `json:"workspaceNeedsOwner,omitempty"` // wsKey -> last seen NeedsOwner count
}

// PrecheckOptions configures `RunPrecheck`.
type PrecheckOptions struct {
	Now    time.Time
	DryRun bool
}

// Finding describes one triggered detector observation.
type Finding struct {
	Detector string `json:"detector"`
	DedupKey string `json:"dedupKey"`
	Tier     string `json:"tier"`
	Title    string `json:"title"`
	OutboxID string `json:"outboxId,omitempty"`
}

// PrecheckReport summarizes a single `hq pulse precheck` tick.
type PrecheckReport struct {
	CheckedAt     string    `json:"checkedAt"`
	DryRun        bool      `json:"dryRun,omitempty"`
	Disabled      bool      `json:"disabled,omitempty"`
	EmittedCount  int       `json:"emittedCount"`
	SkippedCount  int       `json:"skippedCount"`
	Findings      []Finding `json:"findings,omitempty"`
	ZeroTokenSave bool      `json:"zeroTokenSave"`
}

func statePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "pulse", "state.json")
}

func stateLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "pulse", ".pulse.lock")
}

// LoadState reads <vault>/memory/pulse/state.json under RankPulse.
func LoadState(vaultDir string) (State, error) {
	st := State{
		SchemaVersion:       1,
		FiredKeys:           make(map[string]string),
		WorkspaceNeedsOwner: make(map[string]int),
	}
	err := store.With(context.Background(), store.RankPulse, stateLockPath(vaultDir), func(_ context.Context) error {
		raw, rErr := os.ReadFile(statePath(vaultDir))
		if rErr != nil {
			if os.IsNotExist(rErr) {
				return nil
			}
			return rErr
		}
		if len(raw) == 0 {
			return nil
		}
		if err := json.Unmarshal(raw, &st); err != nil {
			return err
		}
		if st.FiredKeys == nil {
			st.FiredKeys = make(map[string]string)
		}
		if st.WorkspaceNeedsOwner == nil {
			st.WorkspaceNeedsOwner = make(map[string]int)
		}
		return nil
	})
	return st, err
}

func saveState(vaultDir string, st State) error {
	if st.SchemaVersion == 0 {
		st.SchemaVersion = 1
	}
	return store.With(context.Background(), store.RankPulse, stateLockPath(vaultDir), func(_ context.Context) error {
		b, err := json.MarshalIndent(st, "", "  ")
		if err != nil {
			return err
		}
		return store.WriteAtomic(statePath(vaultDir), append(b, '\n'), 0o600)
	})
}

func loadCalendarCache(vaultDir string) []CalendarSlot {
	raw, err := os.ReadFile(filepath.Join(vaultDir, "memory", "pulse", "calendar_cache.json"))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var slots []CalendarSlot
	_ = json.Unmarshal(raw, &slots)
	return slots
}

func loadDraftsCache(vaultDir string) []DraftEntry {
	raw, err := os.ReadFile(filepath.Join(vaultDir, "memory", "pulse", "drafts_cache.json"))
	if err != nil || len(raw) == 0 {
		return nil
	}
	var drafts []DraftEntry
	_ = json.Unmarshal(raw, &drafts)
	return drafts
}

func loadEntityNames(vaultDir string) []string {
	entries, err := os.ReadDir(filepath.Join(vaultDir, "memory", "entities"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		base := strings.TrimSuffix(e.Name(), ".md")
		if base != "" {
			names = append(names, base)
		}
	}
	return names
}

type pendingDelegateMsg struct {
	QueueFile string
	ID        string
	From      string
	Summary   string
	CreatedAt time.Time
}

func loadUnackedDelegateMessages(vaultDir string) []pendingDelegateMsg {
	qDir := filepath.Join(vaultDir, "memory", "delegates_queues")
	entries, err := os.ReadDir(qDir)
	if err != nil {
		return nil
	}
	var out []pendingDelegateMsg
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
			continue
		}
		f, oErr := os.Open(filepath.Join(qDir, e.Name()))
		if oErr != nil {
			continue
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var raw struct {
				ID        string `json:"id"`
				From      string `json:"from"`
				Summary   string `json:"summary"`
				Status    string `json:"status"`
				Acked     bool   `json:"acked"`
				CreatedAt string `json:"createdAt"`
			}
			if json.Unmarshal(sc.Bytes(), &raw) != nil || raw.ID == "" {
				continue
			}
			if raw.Acked || strings.EqualFold(raw.Status, "acked") || strings.EqualFold(raw.Status, "done") {
				continue
			}
			ts, _ := time.Parse(time.RFC3339, raw.CreatedAt)
			out = append(out, pendingDelegateMsg{
				QueueFile: e.Name(),
				ID:        raw.ID,
				From:      raw.From,
				Summary:   raw.Summary,
				CreatedAt: ts,
			})
		}
		_ = f.Close()
	}
	return out
}

type candidateEmission struct {
	detector string
	dedupKey string
	item     outbox.OutboxItem
	unpark   string // optional goal slug to unpark after emission
}

// RunPrecheck executes all 10 deterministic 0-token RFC-0027 detectors (`D1`–`D10`) using a
// snapshot-then-commit lock discipline (RankPulse is never held while reading/writing other stores).
func RunPrecheck(ctx context.Context, vaultDir string, opts PrecheckOptions) (PrecheckReport, error) {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	cfg := LoadConfig(vaultDir)
	rep := PrecheckReport{
		CheckedAt:     now.UTC().Format(time.RFC3339),
		DryRun:        opts.DryRun,
		ZeroTokenSave: true,
	}
	if !cfg.Enabled {
		rep.Disabled = true
		return rep, nil
	}

	// Phase 1: Snapshot Pulse state under RankPulse = 32, then release lock immediately.
	state, err := LoadState(vaultDir)
	if err != nil {
		return rep, err
	}

	// Phase 2: Lock-free reads of Goals, Approvals, Workspaces, Delegates, Calendar & Drafts caches.
	goals, _ := kernel.ListGoals(vaultDir)
	pendingProps, _ := approvals.List(vaultDir)
	expiredProps, _ := approvals.ListExpiredProposals(vaultDir, 0, now)
	wsLedger, _ := workspace.LoadLedger(vaultDir)
	calSlots := loadCalendarCache(vaultDir)
	draftEntries := loadDraftsCache(vaultDir)
	entityNames := loadEntityNames(vaultDir)
	delegateMsgs := loadUnackedDelegateMessages(vaultDir)

	var candidates []candidateEmission
	alreadyFired := func(key string) bool {
		_, ok := state.FiredKeys[key]
		return ok
	}

	// --- D1: pre_meeting_prep ---
	for _, slot := range calSlots {
		startAt, ok := kernel.ParseFlexibleTime(slot.StartAt, "")
		if !ok {
			continue
		}
		until := startAt.Sub(now)
		if until < 5*time.Minute || until > cfg.PreMeetingLead {
			continue
		}
		dk := fmt.Sprintf("d1:meeting:%s:%s", slot.ID, startAt.UTC().Format("20060102T1504"))
		if alreadyFired(dk) {
			rep.SkippedCount++
			continue
		}
		var ctxBullets []string
		matchedText := strings.ToLower(slot.Title + " " + strings.Join(slot.Attendees, " "))
		for _, ent := range entityNames {
			if strings.Contains(matchedText, strings.ToLower(strings.ReplaceAll(ent, "-", " "))) || strings.Contains(matchedText, strings.ToLower(ent)) {
				ctxBullets = append(ctxBullets, fmt.Sprintf("Contact dossier matched: `%s`", ent))
			}
		}
		for _, g := range goals {
			if g.Status != "active" || g.IsParked(goals, now) {
				continue
			}
			for _, w := range g.WaitingOn {
				if w.Status != "replied" && strings.Contains(matchedText, strings.ToLower(strings.TrimSpace(w.Counterparty))) {
					ctxBullets = append(ctxBullets, fmt.Sprintf("Open WaitingOn (%s): %s", w.Counterparty, w.Summary))
				}
			}
		}
		for _, entry := range wsLedger.Entries {
			needs := 0
			for _, ch := range entry.Status.Changes {
				if ch.NeedsOwner {
					needs++
				}
			}
			if needs > 0 && (slot.Scope == "" || strings.Contains(matchedText, strings.ToLower(entry.Name)) || strings.Contains(matchedText, "1:1") || strings.Contains(matchedText, "sync")) {
				ctxBullets = append(ctxBullets, fmt.Sprintf("Workspace `%s` has %d change(s) needing owner action", entry.Name, needs))
			}
		}
		var expiredID string
		for _, rec := range expiredProps {
			if strings.Contains(matchedText, strings.ToLower(rec.Proposal.Summary)) || (rec.Proposal.TargetThreadKey != "" && len(ctxBullets) > 0) {
				expiredID = rec.Proposal.ID
				ctxBullets = append(ctxBullets, fmt.Sprintf("Prior staged draft `%s` expired unsent", expiredID))
				break
			}
		}
		scope := slot.Scope
		if scope == "" {
			scope = "work"
		}
		summary := fmt.Sprintf("Starts in %dm (%s).", int(until.Minutes()), startAt.Format("15:04 MST"))
		if len(ctxBullets) > 0 {
			summary += "\n- " + strings.Join(ctxBullets, "\n- ")
		}
		decs := []outbox.DecisionOption{
			{ID: "handled", Label: "Got It — Dismiss", Kind: outbox.ActionDismissHandled, Recommended: true},
		}
		if expiredID != "" {
			decs = append([]outbox.DecisionOption{
				{ID: "restage", Label: "Re-open Timed-Out Draft", Kind: outbox.ActionRestageProposal, ProposalID: expiredID, Recommended: true},
			}, decs...)
			decs[1].Recommended = false
		}
		candidates = append(candidates, candidateEmission{
			detector: "D1:pre_meeting_prep",
			dedupKey: dk,
			item: outbox.OutboxItem{
				SourceSkill: "pulse",
				Scope:       scope,
				Tier:        outbox.TierInterrupt,
				IncidentKey: "pulse:d1:" + slot.ID,
				Title:       fmt.Sprintf("Pre-Meeting Prep (%dm): %s", int(until.Minutes()), slot.Title),
				Summary:     summary,
				Decisions:   decs,
			},
		})
	}

	// --- D2: horizon_countdown & cutoff_lead ---
	for _, g := range goals {
		if g.Status != "active" || g.IsParked(goals, now) {
			continue
		}
		prog := g.Readiness()
		anchorStr := g.WindowStartAt
		if anchorStr == "" {
			anchorStr = g.TargetDate
		}
		if anchor, ok := kernel.ParseFlexibleTime(anchorStr, g.Timezone); ok && anchor.After(now) {
			rem := anchor.Sub(now)
			sortedH := append([]time.Duration(nil), cfg.CountdownHorizons...)
			sort.Slice(sortedH, func(i, j int) bool { return sortedH[i] < sortedH[j] })
			for _, h := range sortedH {
				if rem <= h && rem > 0 {
					hLabel := fmt.Sprintf("%dh", int(h.Hours()))
					dk := fmt.Sprintf("d2:goal:%s:%s", g.Slug, hLabel)
					if alreadyFired(dk) {
						rep.SkippedCount++
						break
					}
					tier := outbox.TierBriefRollup
					if h <= 72*time.Hour && prog.Open > 0 {
						tier = outbox.TierInterrupt
					}
					summary := fmt.Sprintf("Starts in %dh (%s). Readiness: %d/%d confirmed (%d open, %d skipped). Ignore if already handled.",
						int(rem.Hours()), anchorStr, prog.Confirmed, prog.Active, prog.Open, prog.Skipped)
					candidates = append(candidates, candidateEmission{
						detector: "D2:horizon_countdown",
						dedupKey: dk,
						item: outbox.OutboxItem{
							SourceSkill:   "pulse",
							Scope:         g.Scope,
							Tier:          tier,
							IncidentKey:   "pulse:goal:" + g.Slug,
							Title:         fmt.Sprintf("Horizon (%s): %s — %d open item(s)", hLabel, g.Title, prog.Open),
							Summary:       summary,
							SuggestedSlug: g.Slug,
							Decisions: []outbox.DecisionOption{
								{ID: "handled", Label: "Already Handled — Dismiss", Kind: outbox.ActionDismissHandled, Recommended: true},
								{ID: "park", Label: "Park 24h", Kind: outbox.ActionParkUntil, GoalSlug: g.Slug, ParkHorizon: "24h"},
							},
						},
					})
					break // emit only the single tightest horizon bucket
				}
			}
		}
		// Checklist CutoffAt - cutoff_lead
		for _, item := range g.Checklist {
			if item.Status != kernel.ChecklistOpen || item.CutoffAt == "" {
				continue
			}
			cutoff, ok := kernel.ParseFlexibleEndTime(item.CutoffAt, g.Timezone)
			if !ok {
				continue
			}
			rem := cutoff.Sub(now)
			if rem > 0 && rem <= cfg.CutoffLead {
				dk := fmt.Sprintf("d2:cutoff:%s:%s", g.Slug, item.ID)
				if alreadyFired(dk) {
					rep.SkippedCount++
					continue
				}
				candidates = append(candidates, candidateEmission{
					detector: "D2:cutoff_lead",
					dedupKey: dk,
					item: outbox.OutboxItem{
						SourceSkill:   "pulse",
						Scope:         g.Scope,
						Tier:          outbox.TierInterrupt,
						IncidentKey:   "pulse:cutoff:" + g.Slug,
						Title:         fmt.Sprintf("Cutoff in %dh: %s (%s)", int(rem.Hours())+1, item.Title, g.Title),
						Summary:       fmt.Sprintf("Checklist item %q reaches cutoff at %s. Ignore if already handled.", item.Title, item.CutoffAt),
						SuggestedSlug: g.Slug,
						Decisions: []outbox.DecisionOption{
							{ID: "handled", Label: "Done — Dismiss", Kind: outbox.ActionDismissHandled, Recommended: true},
							{ID: "skip", Label: "Skip Permanently", Kind: outbox.ActionSkipChecklist, GoalSlug: g.Slug, ChecklistItemID: item.ID, Reason: "Skipped via pulse cutoff card"},
						},
					},
				})
			}
		}
	}

	// --- D3: blocking_window_ripple ---
	for _, blk := range goals {
		if blk.Status != "active" || !blk.BlocksAvailability || blk.WindowStartAt == "" || blk.WindowEndAt == "" {
			continue
		}
		winStart, ok1 := kernel.ParseFlexibleTime(blk.WindowStartAt, blk.Timezone)
		winEnd, ok2 := kernel.ParseFlexibleEndTime(blk.WindowEndAt, blk.Timezone)
		if !ok1 || !ok2 {
			continue
		}
		if winStart.Sub(now) > 72*time.Hour || now.After(winEnd) {
			continue
		}
		rippleEnd := winEnd.Add(48 * time.Hour)
		for _, other := range goals {
			if other.Status != "active" || other.Slug == blk.Slug || other.IsParked(goals, now) {
				continue
			}
			if td, ok := kernel.ParseFlexibleEndTime(other.TargetDate, other.Timezone); ok && !td.Before(winStart) && !td.After(rippleEnd) {
				dk := fmt.Sprintf("d3:ripple:%s:%s", blk.Slug, other.Slug)
				if !alreadyFired(dk) {
					candidates = append(candidates, candidateEmission{
						detector: "D3:blocking_window_ripple",
						dedupKey: dk,
						item: outbox.OutboxItem{
							SourceSkill:   "pulse",
							Scope:         other.Scope,
							Tier:          outbox.TierInterrupt,
							IncidentKey:   "pulse:ripple:" + blk.Slug,
							Title:         fmt.Sprintf("Complete before %s: %s (due %s)", blk.Title, other.Title, other.TargetDate),
							Summary:       fmt.Sprintf("Goal %q is due %s, which collides with your unavailable window %q (%s to %s). Handle early or park until after.", other.Title, other.TargetDate, blk.Title, blk.WindowStartAt, blk.WindowEndAt),
							SuggestedSlug: other.Slug,
							Decisions: []outbox.DecisionOption{
								{ID: "handled", Label: "Already Handled", Kind: outbox.ActionDismissHandled, Recommended: true},
								{ID: "park", Label: "Park Until After Window", Kind: outbox.ActionParkUntil, GoalSlug: other.Slug, ParkHorizon: "after:" + blk.Slug},
							},
						},
					})
				} else {
					rep.SkippedCount++
				}
			}
			for _, ci := range other.Checklist {
				if ci.Status != kernel.ChecklistOpen || !ci.MustCompleteBeforeWindow {
					continue
				}
				dk := fmt.Sprintf("d3:ripple-item:%s:%s:%s", blk.Slug, other.Slug, ci.ID)
				if !alreadyFired(dk) {
					candidates = append(candidates, candidateEmission{
						detector: "D3:blocking_window_ripple",
						dedupKey: dk,
						item: outbox.OutboxItem{
							SourceSkill: "pulse",
							Scope:       other.Scope,
							Tier:        outbox.TierInterrupt,
							IncidentKey: "pulse:ripple:" + blk.Slug,
							Title:       fmt.Sprintf("Pre-window blocker (%s): %s", blk.Title, ci.Title),
							Summary:     fmt.Sprintf("Open checklist item %q on %q is marked mustCompleteBeforeWindow ahead of %q (%s).", ci.Title, other.Title, blk.Title, blk.WindowStartAt),
							Decisions: []outbox.DecisionOption{
								{ID: "handled", Label: "Done — Dismiss", Kind: outbox.ActionDismissHandled, Recommended: true},
								{ID: "skip", Label: "Skip Item", Kind: outbox.ActionSkipChecklist, GoalSlug: other.Slug, ChecklistItemID: ci.ID, Reason: "Skipped ahead of unavailable window"},
							},
						},
					})
				}
			}
		}
	}

	// --- D4: window_transition_ping ---
	for _, g := range goals {
		if g.Status != "active" || g.WindowStartAt == "" {
			continue
		}
		startAt, ok := kernel.ParseFlexibleTime(g.WindowStartAt, g.Timezone)
		if !ok {
			continue
		}
		since := now.Sub(startAt)
		if since >= 0 && since <= 6*time.Hour {
			dk := fmt.Sprintf("d4:transition:%s:%s", g.Slug, startAt.UTC().Format("20060102"))
			if !alreadyFired(dk) {
				spaceHint := ""
				if g.SpaceSlug != "" {
					spaceHint = fmt.Sprintf(" Open Living Space: `%s`.", g.SpaceSlug)
				}
				candidates = append(candidates, candidateEmission{
					detector: "D4:window_transition_ping",
					dedupKey: dk,
					item: outbox.OutboxItem{
						SourceSkill: "pulse",
						Scope:       g.Scope,
						Tier:        outbox.TierInterrupt,
						IncidentKey: "pulse:transition:" + g.Slug,
						Title:       fmt.Sprintf("Window Started: %s", g.Title),
						Summary:     fmt.Sprintf("%q has begun (%s).%s", g.Title, g.WindowStartAt, spaceHint),
						Decisions: []outbox.DecisionOption{
							{ID: "handled", Label: "Got It", Kind: outbox.ActionDismissHandled, Recommended: true},
						},
					},
				})
			} else {
				rep.SkippedCount++
			}
		}
	}

	// --- D5: interval_and_coverage_conflict ---
	for _, g := range goals {
		if g.Status != "active" || g.IsParked(goals, now) {
			continue
		}
		// 5a: Confirmed timed slots overlap or gap < MinSlotBuffer
		type timedSlot struct {
			item  kernel.ChecklistItem
			start time.Time
			end   time.Time
		}
		var slots []timedSlot
		for _, ci := range g.Checklist {
			if ci.Status != kernel.ChecklistConfirmed || ci.WindowStartAt == "" || ci.WindowEndAt == "" {
				continue
			}
			// Only compare explicit timestamps containing 'T' (time-of-day) for buffer checks
			if !strings.Contains(ci.WindowStartAt, "T") || !strings.Contains(ci.WindowEndAt, "T") {
				continue
			}
			st, ok1 := kernel.ParseFlexibleTime(ci.WindowStartAt, g.Timezone)
			en, ok2 := kernel.ParseFlexibleTime(ci.WindowEndAt, g.Timezone)
			if ok1 && ok2 && en.After(st) {
				slots = append(slots, timedSlot{item: ci, start: st, end: en})
			}
		}
		sort.Slice(slots, func(i, j int) bool { return slots[i].start.Before(slots[j].start) })
		for i := 0; i+1 < len(slots); i++ {
			gap := slots[i+1].start.Sub(slots[i].end)
			if gap < cfg.MinSlotBuffer {
				dk := fmt.Sprintf("d5:buffer:%s:%s:%s", g.Slug, slots[i].item.ID, slots[i+1].item.ID)
				if !alreadyFired(dk) {
					kind := fmt.Sprintf("only %dm buffer", int(gap.Minutes()))
					if gap < 0 {
						kind = fmt.Sprintf("overlaps by %dm", int((-gap).Minutes()))
					}
					candidates = append(candidates, candidateEmission{
						detector: "D5:interval_conflict",
						dedupKey: dk,
						item: outbox.OutboxItem{
							SourceSkill: "pulse",
							Scope:       g.Scope,
							Tier:        outbox.TierInterrupt,
							IncidentKey: "pulse:conflict:" + g.Slug,
							Title:       fmt.Sprintf("Schedule Conflict (%s): %s -> %s", kind, slots[i].item.Title, slots[i+1].item.Title),
							Summary:     fmt.Sprintf("In %q, %q ends %s and %q starts %s (%s; minimum buffer %dm).", g.Title, slots[i].item.Title, slots[i].end.Format("15:04"), slots[i+1].item.Title, slots[i+1].start.Format("15:04"), kind, int(cfg.MinSlotBuffer.Minutes())),
							Decisions: []outbox.DecisionOption{
								{ID: "handled", Label: "Acknowledged", Kind: outbox.ActionDismissHandled, Recommended: true},
							},
						},
					})
				}
			}
		}
		// 5b: RequiredCoverageTag daily gap check
		if reqTag := strings.TrimSpace(g.RequiredCoverageTag); reqTag != "" && g.WindowStartAt != "" && g.WindowEndAt != "" {
			wStart, ok1 := kernel.ParseFlexibleTime(g.WindowStartAt, g.Timezone)
			wEnd, ok2 := kernel.ParseFlexibleTime(g.WindowEndAt, g.Timezone)
			if ok1 && ok2 && wEnd.After(wStart) {
				var missingDays []string
				for day := wStart.Truncate(24 * time.Hour); day.Before(wEnd); day = day.Add(24 * time.Hour) {
					covered := false
					for _, ci := range g.Checklist {
						if ci.Status != kernel.ChecklistConfirmed || !strings.EqualFold(strings.TrimSpace(ci.Tag), reqTag) {
							continue
						}
						cs, okS := kernel.ParseFlexibleTime(ci.WindowStartAt, g.Timezone)
						ce, okE := kernel.ParseFlexibleEndTime(ci.WindowEndAt, g.Timezone)
						if okS && okE && !day.Before(cs.Truncate(24*time.Hour)) && !day.After(ce) {
							covered = true
							break
						}
					}
					if !covered {
						missingDays = append(missingDays, day.Format("2006-01-02"))
					}
				}
				if len(missingDays) > 0 {
					dk := fmt.Sprintf("d5:coverage:%s:%s:%s", g.Slug, reqTag, strings.Join(missingDays, ","))
					if !alreadyFired(dk) {
						candidates = append(candidates, candidateEmission{
							detector: "D5:coverage_gap",
							dedupKey: dk,
							item: outbox.OutboxItem{
								SourceSkill: "pulse",
								Scope:       g.Scope,
								Tier:        outbox.TierBriefRollup,
								IncidentKey: "pulse:coverage:" + g.Slug,
								Title:       fmt.Sprintf("Coverage Gap (%s) in %s: %d day(s) uncovered", reqTag, g.Title, len(missingDays)),
								Summary:     fmt.Sprintf("No confirmed `%s` item covers: %s.", reqTag, strings.Join(missingDays, ", ")),
								Decisions: []outbox.DecisionOption{
									{ID: "handled", Label: "Got It", Kind: outbox.ActionDismissHandled, Recommended: true},
								},
							},
						})
					}
				}
			}
		}
	}

	// --- D6: proposal_ttl_and_expiry ---
	for _, p := range pendingProps {
		if p.ExpiresAt == "" {
			continue
		}
		expAt, pErr := time.Parse(time.RFC3339, p.ExpiresAt)
		if pErr != nil {
			continue
		}
		rem := expAt.Sub(now)
		if rem > 0 && rem <= 2*time.Hour {
			dk := "d6:expiring:" + p.ID
			if !alreadyFired(dk) {
				sha8 := p.SHA256
				if len(sha8) > 12 {
					sha8 = sha8[:12]
				}
				candidates = append(candidates, candidateEmission{
					detector: "D6:proposal_ttl",
					dedupKey: dk,
					item: outbox.OutboxItem{
						SourceSkill: "pulse",
						Scope:       "approvals",
						Tier:        outbox.TierInterrupt,
						IncidentKey: "pulse:proposal:" + p.ID,
						ProposalIDs: []string{p.ID},
						Title:       fmt.Sprintf("Approval Expiring in %dm: %s", int(rem.Minutes()), p.Summary),
						Summary:     fmt.Sprintf("Staged proposal `%s` (%s) expires at %s.", p.ID, p.ActionType, p.ExpiresAt),
						Decisions: []outbox.DecisionOption{
							{ID: "approve", Label: "Approve & Execute", Kind: outbox.ActionApproveProposal, ProposalID: p.ID, ProposalSHA: sha8, Recommended: true},
							{ID: "reject", Label: "Reject / Dismiss", Kind: outbox.ActionRejectProposal, ProposalID: p.ID},
						},
					},
				})
			}
		}
	}
	for _, rec := range expiredProps {
		dk := "d6:expired:" + rec.Proposal.ID
		if !alreadyFired(dk) {
			candidates = append(candidates, candidateEmission{
				detector: "D6:proposal_expired",
				dedupKey: dk,
				item: outbox.OutboxItem{
					SourceSkill: "pulse",
					Scope:       "approvals",
					Tier:        outbox.TierInterrupt,
					IncidentKey: "pulse:proposal:" + rec.Proposal.ID,
					ProposalIDs: []string{rec.Proposal.ID},
					Title:       fmt.Sprintf("Staged Approval Expired Unsent: %s", rec.Proposal.Summary),
					Summary:     fmt.Sprintf("Proposal `%s` (%s) expired at %s without being sent. Re-open a fresh 24h approval card or dismiss if no longer needed.", rec.Proposal.ID, rec.Proposal.ActionType, rec.Timestamp),
					Decisions: []outbox.DecisionOption{
						{ID: "restage", Label: "Re-open Timed-Out Card", Kind: outbox.ActionRestageProposal, ProposalID: rec.Proposal.ID, Recommended: true},
						{ID: "handled", Label: "No Longer Needed — Dismiss", Kind: outbox.ActionDismissHandled},
					},
				},
			})
		} else {
			rep.SkippedCount++
		}
	}

	// --- D7: waiting_sla_breach (with expired-proposal annotation) ---
	for _, g := range goals {
		if g.Status != "active" || g.IsParked(goals, now) {
			continue
		}
		for _, w := range g.WaitingOn {
			if w.Status != "waiting" || w.FollowUpAfter == "" {
				continue
			}
			dueAt, pErr := time.Parse(time.RFC3339, w.FollowUpAfter)
			if pErr != nil || now.Before(dueAt) {
				continue
			}
			dk := fmt.Sprintf("d7:wait:%s:%s", g.Slug, w.ID)
			if alreadyFired(dk) {
				rep.SkippedCount++
				continue
			}
			tk := w.ThreadKey
			if tk == "" {
				tk = w.ThreadOrSpace
			}
			expNote := ""
			decs := []outbox.DecisionOption{
				{ID: "handled", Label: "Already Followed Up", Kind: outbox.ActionDismissHandled, Recommended: true},
				{ID: "park", Label: "Park 24h", Kind: outbox.ActionParkUntil, GoalSlug: g.Slug, ParkHorizon: "24h"},
			}
			if exp := approvals.FindUnrestagedExpiredOfferForThread(vaultDir, w.Connector, w.Account, tk, 30*24*time.Hour, now); exp != nil {
				expNote = fmt.Sprintf(" Note: prior staged reply `%s` expired unsent on %s.", exp.Proposal.ID, exp.Timestamp)
				decs = append([]outbox.DecisionOption{
					{ID: "restage", Label: "Re-open Expired Reply", Kind: outbox.ActionRestageProposal, ProposalID: exp.Proposal.ID, Recommended: true},
				}, decs...)
				decs[1].Recommended = false
			}
			candidates = append(candidates, candidateEmission{
				detector: "D7:waiting_sla_breach",
				dedupKey: dk,
				item: outbox.OutboxItem{
					SourceSkill: "pulse",
					Scope:       g.Scope,
					Tier:        outbox.TierBriefRollup,
					IncidentKey: "pulse:wait:" + g.Slug + ":" + w.ID,
					Title:       fmt.Sprintf("Waiting-On SLA Overdue: %s (%s)", w.Counterparty, g.Title),
					Summary:     fmt.Sprintf("Waiting on %s for %q (SLA %s).%s", w.Counterparty, w.Summary, w.FollowUpAfter, expNote),
					Decisions:   decs,
				},
			})
		}
	}

	// --- D8: parked_horizon_wakeup ---
	for _, g := range goals {
		if g.Status != "active" || strings.TrimSpace(g.ParkedUntil) == "" {
			continue
		}
		if g.IsParked(goals, now) {
			continue
		}
		dk := fmt.Sprintf("d8:wakeup:%s:%s", g.Slug, g.ParkedUntil)
		if !alreadyFired(dk) {
			candidates = append(candidates, candidateEmission{
				detector: "D8:parked_horizon_wakeup",
				dedupKey: dk,
				unpark:   g.Slug,
				item: outbox.OutboxItem{
					SourceSkill:   "pulse",
					Scope:         g.Scope,
					Tier:          outbox.TierBriefRollup,
					IncidentKey:   "pulse:wakeup:" + g.Slug,
					Title:         fmt.Sprintf("Parked Goal Resurfaced: %s", g.Title),
					Summary:       fmt.Sprintf("Hold `%s` has elapsed for %q; goal is active again.", g.ParkedUntil, g.Title),
					SuggestedSlug: g.Slug,
					Decisions: []outbox.DecisionOption{
						{ID: "handled", Label: "Acknowledge", Kind: outbox.ActionDismissHandled, Recommended: true},
						{ID: "park", Label: "Park Another 48h", Kind: outbox.ActionParkUntil, GoalSlug: g.Slug, ParkHorizon: "48h"},
					},
				},
			})
		}
	}

	// --- D9: workspace_and_review_pulse ---
	nextWSCounts := make(map[string]int, len(wsLedger.Entries))
	var wsKeys []string
	for k := range wsLedger.Entries {
		wsKeys = append(wsKeys, k)
	}
	sort.Strings(wsKeys)
	for _, k := range wsKeys {
		entry := wsLedger.Entries[k]
		needs := 0
		var needTitles []string
		for _, ch := range entry.Status.Changes {
			if ch.NeedsOwner {
				needs++
				lbl := ch.ID
				if ch.Title != "" {
					lbl += " (" + ch.Title + ")"
				}
				needTitles = append(needTitles, lbl)
			}
		}
		nextWSCounts[k] = needs
		prevNeeds := state.WorkspaceNeedsOwner[k]
		if needs > 0 && needs > prevNeeds {
			dk := fmt.Sprintf("d9:ws-needs:%s:%d", k, needs)
			if !alreadyFired(dk) {
				candidates = append(candidates, candidateEmission{
					detector: "D9:workspace_review_pulse",
					dedupKey: dk,
					item: outbox.OutboxItem{
						SourceSkill: "pulse",
						Scope:       "engineering",
						Tier:        outbox.TierInterrupt,
						IncidentKey: "pulse:ws:" + k,
						Title:       fmt.Sprintf("Workspace Needs Action (%d): %s", needs, entry.Name),
						Summary:     fmt.Sprintf("Owned workspace `%s` (%s) has %d change(s) waiting on owner: %s.", entry.Name, entry.Provider, needs, strings.Join(needTitles, ", ")),
						Decisions: []outbox.DecisionOption{
							{ID: "handled", Label: "I'm On It — Dismiss", Kind: outbox.ActionDismissHandled, Recommended: true},
							{ID: "park", Label: "Park 4h", Kind: outbox.ActionParkUntil, ParkHorizon: "4h"},
							{ID: "noisy", Label: "Mute Workspace Nudges", Kind: outbox.ActionDismissNoisy},
						},
					},
				})
			}
		}
	}
	for _, dm := range delegateMsgs {
		if !dm.CreatedAt.IsZero() && now.Sub(dm.CreatedAt) < 15*time.Minute {
			continue
		}
		dk := "d9:delegate:" + dm.ID
		if !alreadyFired(dk) {
			candidates = append(candidates, candidateEmission{
				detector: "D9:delegate_queue",
				dedupKey: dk,
				item: outbox.OutboxItem{
					SourceSkill: "pulse",
					Scope:       "delegates",
					Tier:        outbox.TierBriefRollup,
					IncidentKey: "pulse:delegate:" + dm.QueueFile,
					Title:       fmt.Sprintf("Unacked Delegate Item from %s", dm.From),
					Summary:     fmt.Sprintf("Queued delegate message `%s`: %s", dm.ID, dm.Summary),
					Decisions: []outbox.DecisionOption{
						{ID: "handled", Label: "Handled", Kind: outbox.ActionDismissHandled, Recommended: true},
					},
				},
			})
		}
	}

	// --- D10: stale_unsent_draft ---
	for _, dr := range draftEntries {
		updAt, ok := kernel.ParseFlexibleTime(dr.UpdatedAt, "")
		if !ok || now.Sub(updAt) < cfg.StaleDraftAge {
			continue
		}
		dk := "d10:draft:" + dr.ID
		if !alreadyFired(dk) {
			candidates = append(candidates, candidateEmission{
				detector: "D10:stale_unsent_draft",
				dedupKey: dk,
				item: outbox.OutboxItem{
					SourceSkill: "pulse",
					Scope:       "drafts",
					Tier:        outbox.TierIdeasFeed,
					IncidentKey: "pulse:draft:" + dr.ID,
					Title:       fmt.Sprintf("Stale Unsent Draft (%dh old): %s", int(now.Sub(updAt).Hours()), dr.Subject),
					Summary:     fmt.Sprintf("Draft `%s` to %s has been sitting unsent since %s.", dr.ID, dr.Recipient, dr.UpdatedAt),
					Decisions: []outbox.DecisionOption{
						{ID: "handled", Label: "Dismiss", Kind: outbox.ActionDismissHandled, Recommended: true},
					},
				},
			})
		}
	}

	// Phase 3: Emit candidates outside RankPulse, then commit state under RankPulse.
	_ = ctx
	nowStr := now.UTC().Format(time.RFC3339)
	for _, cand := range candidates {
		f := Finding{
			Detector: cand.detector,
			DedupKey: cand.dedupKey,
			Tier:     cand.item.Tier,
			Title:    cand.item.Title,
		}
		if !opts.DryRun {
			added, _, addErr := outbox.Add(vaultDir, cand.item, now)
			if addErr == nil {
				f.OutboxID = added.ID
				f.Tier = added.Tier
			}
			if cand.unpark != "" {
				_, _ = kernel.UnparkGoal(vaultDir, cand.unpark, now)
			}
			state.FiredKeys[cand.dedupKey] = nowStr
		}
		rep.Findings = append(rep.Findings, f)
		rep.EmittedCount++
	}

	if !opts.DryRun {
		state.LastRunAt = nowStr
		state.WorkspaceNeedsOwner = nextWSCounts
		// Prune FiredKeys older than 30 days to keep state.json compact
		for k, v := range state.FiredKeys {
			if ts, pErr := time.Parse(time.RFC3339, v); pErr == nil && now.Sub(ts) > 30*24*time.Hour {
				delete(state.FiredKeys, k)
			}
		}
		if err := saveState(vaultDir, state); err != nil {
			return rep, err
		}
	}
	return rep, nil
}
