package outbox

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const MaxInterruptDeliveryAttempts = 5

// InterruptDeliverFunc delivers a formatted proactive interrupt card to the active user surface
// (e.g., [HQ] Front Desk via LanguageServerService / agentapi or configured ChannelAdapter).
type InterruptDeliverFunc func(item OutboxItem, formattedCard string) error

var (
	reCommandCue   = regexp.MustCompile(`(?i)\b(?:run|execute|call|invoke|shell|bash)\s+[` + "`" + `"]?hq\s+(?:approve|propose|spawn|subagent|exec-sandboxed)`)
	reInjectionCue = regexp.MustCompile(`(?i)ignore\s+(?:all\s+)?(?:previous|prior|system)\s+instructions`)
	reFenceTag     = regexp.MustCompile(`(?i)</?hq_system_notification\b[^>]*>`)
)

// SanitizeAntiCue strips control characters, neutralizes XML fence breakouts, and scrubs
// imperative command/injection cues so untrusted notification summaries remain passive data.
func SanitizeAntiCue(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	var cleaned strings.Builder
	cleaned.Grow(len(s))
	for _, r := range s {
		if r < 0x20 && r != '\n' && r != '\t' {
			continue
		}
		cleaned.WriteRune(r)
	}
	out := cleaned.String()
	out = reFenceTag.ReplaceAllString(out, "[redacted-fence-tag]")
	out = reCommandCue.ReplaceAllString(out, "[redacted-command-cue]")
	out = reInjectionCue.ReplaceAllString(out, "[redacted-injection-cue]")
	if maxRunes > 0 {
		if runes := []rune(out); len(runes) > maxRunes {
			out = string(runes[:maxRunes-3]) + "..."
		}
	}
	return strings.TrimSpace(out)
}

func deliveryBackoff(attempt int) time.Duration {
	switch {
	case attempt <= 1:
		return 30 * time.Second
	case attempt == 2:
		return 2 * time.Minute
	case attempt == 3:
		return 10 * time.Minute
	default:
		return 30 * time.Minute
	}
}

func hasActiveBriefLease(it OutboxItem, now time.Time) bool {
	exp := strings.TrimSpace(it.BriefLeaseExpiresAt)
	if exp == "" {
		return false
	}
	t, err := time.Parse(time.RFC3339, exp)
	if err != nil {
		return false
	}
	return now.Before(t)
}

// FormatInterruptCard renders a clean, anti-cue-sanitized Markdown alert card for user delivery
// (passive-notification handling is enforced via hidden <user_rules> + Ring-0 presence/PreToolUse gates,
// keeping the visible Front Desk thread free of XML fence boilerplate).
func FormatInterruptCard(it OutboxItem) string {
	badge := "🔔 Proactive Alert"
	if it.Critical && !it.Tainted {
		badge = "🚨 Critical Breach Alert"
	}
	scope := SanitizeAntiCue(it.SourceSkill, 80)
	if strings.TrimSpace(it.Scope) != "" {
		scope += " · " + SanitizeAntiCue(it.Scope, 80)
	}
	title := SanitizeAntiCue(it.Title, 200)
	summary := SanitizeAntiCue(it.Summary, 1200)

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### %s (`%s`)\n", badge, scope))
	sb.WriteString(fmt.Sprintf("**%s**", title))
	if summary != "" {
		sb.WriteString("\n\n" + summary)
	}
	if len(it.ProposalIDs) > 0 {
		sb.WriteString(fmt.Sprintf("\n\n- **Pending Approval ID(s):** `%s`", strings.Join(it.ProposalIDs, "`, `")))
	}
	return sb.String()
}

// DispatchPendingInterrupts executes a 3-phase lock-safe delivery pipeline:
// Phase A (under RankOutbox): reaps stale dispatching claims, skips brief-leased items, and claims StatusDispatching.
// Phase B (unlocked): calls deliverFn over HTTP/IPC without holding RankOutbox.
// Phase C (under RankOutbox): reloads items.jsonl (preserving concurrent Add writes) and commits StatusDelivered or retry backoff.
func DispatchPendingInterrupts(vaultDir string, now time.Time, deliverFn InterruptDeliverFunc) ([]OutboxItem, error) {
	if deliverFn == nil {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	cfg := LoadAttentionConfig(vaultDir)
	inQuiet := InQuietHours(cfg, now)
	nowStr := now.UTC().Format(time.RFC3339)
	claimUntilStr := now.Add(DispatchingClaimTTL).UTC().Format(time.RFC3339)

	// Phase A: Claim eligible pending interrupts under RankOutbox.
	var claimed []OutboxItem
	err := WithOutboxLock(vaultDir, func() error {
		items, lErr := LoadAllUnlocked(vaultDir)
		if lErr != nil || len(items) == 0 {
			return lErr
		}
		dirty := false
		for i := range items {
			it := &items[i]
			// Reap crashed StatusDispatching claims past DispatchClaimUntil.
			if it.Status == StatusDispatching {
				if it.DispatchClaimUntil == "" {
					it.Status = StatusPending
					dirty = true
				} else if u, pErr := time.Parse(time.RFC3339, it.DispatchClaimUntil); pErr != nil || !now.Before(u) {
					it.Status = StatusPending
					it.DispatchClaimUntil = ""
					dirty = true
				}
			}
			if it.Tier != TierInterrupt || it.Status != StatusPending {
				continue
			}
			if hasActiveBriefLease(*it, now) {
				continue
			}
			if inQuiet && !(it.Critical && !it.Tainted && cfg.QuietHours.AllowCriticalBreach) {
				continue
			}
			if it.NextAttemptAt != "" {
				if nextAt, pErr := time.Parse(time.RFC3339, it.NextAttemptAt); pErr == nil && now.Before(nextAt) {
					continue
				}
			}
			it.Status = StatusDispatching
			it.DispatchClaimUntil = claimUntilStr
			it.UpdatedAt = nowStr
			claimed = append(claimed, *it)
			dirty = true
		}
		if !dirty {
			return nil
		}
		return SaveCanonicalUnlocked(vaultDir, items)
	})
	if err != nil || len(claimed) == 0 {
		return nil, err
	}

	// Phase B: Deliver outside the lock so slow LS/HTTP calls never block producers.
	type deliveryOutcome struct {
		id  string
		err error
	}
	outcomes := make(map[string]deliveryOutcome, len(claimed))
	for _, it := range claimed {
		card := FormatInterruptCard(it)
		dErr := deliverFn(it, card)
		outcomes[it.ID] = deliveryOutcome{id: it.ID, err: dErr}
	}

	// Phase C: Re-acquire RankOutbox, reload items.jsonl, and commit outcomes.
	var delivered []OutboxItem
	err = WithOutboxLock(vaultDir, func() error {
		items, lErr := LoadAllUnlocked(vaultDir)
		if lErr != nil {
			return lErr
		}
		dirty := false
		for i := range items {
			it := &items[i]
			out, ok := outcomes[it.ID]
			if !ok || it.Status != StatusDispatching {
				continue
			}
			it.DispatchClaimUntil = ""
			it.UpdatedAt = nowStr
			if out.err != nil {
				it.Status = StatusPending
				it.DeliveryAttempts++
				it.LastDeliveryError = strings.TrimSpace(out.err.Error())
				if it.DeliveryAttempts >= MaxInterruptDeliveryAttempts {
					it.Tier = TierBriefRollup
					it.DowngradedBy = "delivery_retry_exhausted"
					it.NextAttemptAt = ""
				} else {
					it.NextAttemptAt = now.Add(deliveryBackoff(it.DeliveryAttempts)).UTC().Format(time.RFC3339)
				}
				dirty = true
				continue
			}
			it.Status = StatusDelivered
			it.DeliveryAttempts++
			it.NextAttemptAt = ""
			it.LastDeliveryError = ""
			delivered = append(delivered, *it)
			dirty = true
		}
		if !dirty {
			return nil
		}
		return SaveCanonicalUnlocked(vaultDir, items)
	})
	for _, it := range delivered {
		_ = RecordReportedFromItem(vaultDir, it, now)
	}
	return delivered, err
}
