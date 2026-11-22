package kernel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/timers"
)

// ActivityRecord logs user-visible high-level lifecycle & reversal actions in <vault>/memory/activity.jsonl.
type ActivityRecord struct {
	Timestamp string   `json:"timestamp"`
	Kind      string   `json:"kind"` // e.g. "reverted_by_user"
	Target    string   `json:"target"`
	Reason    string   `json:"reason,omitempty"`
	Cancelled []string `json:"cancelled,omitempty"`
	Summary   string   `json:"summary"`
}

// CancelReport summarizes items cancelled by `hq cancel <id-or-scope>`.
type CancelReport struct {
	Target             string   `json:"target"`
	Reason             string   `json:"reason"`
	CancelledEvents    []string `json:"cancelledEvents,omitempty"`
	CancelledTimers    []string `json:"cancelledTimers,omitempty"`
	CancelledProposals []string `json:"cancelledProposals,omitempty"`
	TotalCancelled     int      `json:"totalCancelled"`
}

func activityLogPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "activity.jsonl")
}

// AppendActivity appends an ActivityRecord to <vault>/memory/activity.jsonl (0600, no symlink follow).
func AppendActivity(vaultDir string, rec ActivityRecord) error {
	if rec.Timestamp == "" {
		rec.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return store.AppendLineNoFollow(activityLogPath(vaultDir), append(b, '\n'), 0o600)
}

// LoadRecentActivity returns the most recent `limit` ActivityRecords (newest first).
func LoadRecentActivity(vaultDir string, limit int) []ActivityRecord {
	if limit <= 0 {
		limit = 20
	}
	f, err := os.Open(activityLogPath(vaultDir))
	if err != nil {
		return nil
	}
	defer f.Close()
	var all []ActivityRecord
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var rec ActivityRecord
		if json.Unmarshal([]byte(line), &rec) == nil && rec.Kind != "" {
			all = append(all, rec)
		}
	}
	if len(all) > limit {
		all = all[len(all)-limit:]
	}
	// Reverse to newest-first
	for i, j := 0, len(all)-1; i < j; i, j = i+1, j-1 {
		all[i], all[j] = all[j], all[i]
	}
	return all
}

// CancelTarget cancels pending/leased events, armed timers, and pending proposals matching `target`
// (exact ID, ScopeKey substring, Skill, or Title substring) in 0 LLM tokens and appends a `reverted_by_user` record.
func CancelTarget(vaultDir, target, reason string, now time.Time) (CancelReport, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return CancelReport{}, fmt.Errorf("cancel target id or scope is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "reverted_by_user"
	}
	low := strings.ToLower(target)
	rep := CancelReport{Target: target, Reason: reason}

	// 1. Cancel matching pending or leased events
	if evs, err := events.List(vaultDir, "", false, 0); err == nil {
		for _, ev := range evs {
			if ev.Status != events.StatusPending && ev.Status != events.StatusLeased {
				continue
			}
			if ev.ID == target ||
				strings.EqualFold(ev.ScopeKey, target) ||
				strings.Contains(strings.ToLower(ev.ScopeKey), low) ||
				strings.EqualFold(ev.Skill, target) ||
				strings.Contains(strings.ToLower(ev.Title), low) {
				if _, dErr := events.Drop(vaultDir, ev.ID, reason, now); dErr == nil {
					rep.CancelledEvents = append(rep.CancelledEvents, ev.ID)
				}
			}
		}
	}

	// 2. Cancel matching armed timers
	if tms, err := timers.List(vaultDir, false); err == nil {
		for _, tm := range tms {
			if tm.Status != timers.StatusArmed {
				continue
			}
			if tm.ID == target ||
				strings.EqualFold(tm.ScopeKey, target) ||
				strings.Contains(strings.ToLower(tm.ScopeKey), low) ||
				strings.EqualFold(tm.Skill, target) ||
				strings.Contains(strings.ToLower(tm.Title), low) {
				if _, cErr := timers.Cancel(vaultDir, tm.ID, now); cErr == nil {
					rep.CancelledTimers = append(rep.CancelledTimers, tm.ID)
				}
			}
		}
	}

	// 3. Reject/dismiss matching pending proposals
	if props, err := approvals.List(vaultDir); err == nil {
		for _, p := range props {
			if p.ID == target ||
				strings.EqualFold(p.OriginSkill, target) ||
				strings.EqualFold(p.TargetThreadKey, target) ||
				strings.Contains(strings.ToLower(p.Summary), low) {
				if rErr := approvals.RejectProposalWithOptions(vaultDir, p.ID, approvals.RejectOptions{
					Reason:         reason,
					SuppressResume: true,
				}); rErr == nil {
					rep.CancelledProposals = append(rep.CancelledProposals, p.ID)
				}
			}
		}
	}

	rep.TotalCancelled = len(rep.CancelledEvents) + len(rep.CancelledTimers) + len(rep.CancelledProposals)
	if rep.TotalCancelled > 0 {
		allIDs := make([]string, 0, rep.TotalCancelled)
		allIDs = append(allIDs, rep.CancelledEvents...)
		allIDs = append(allIDs, rep.CancelledTimers...)
		allIDs = append(allIDs, rep.CancelledProposals...)
		_ = AppendActivity(vaultDir, ActivityRecord{
			Timestamp: now.UTC().Format(time.RFC3339),
			Kind:      "reverted_by_user",
			Target:    target,
			Reason:    reason,
			Cancelled: allIDs,
			Summary:   fmt.Sprintf("Cancelled %d item(s) matching %q (%d events, %d timers, %d proposals)", rep.TotalCancelled, target, len(rep.CancelledEvents), len(rep.CancelledTimers), len(rep.CancelledProposals)),
		})
	}
	return rep, nil
}
