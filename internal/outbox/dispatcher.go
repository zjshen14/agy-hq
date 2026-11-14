package outbox

import (
	"fmt"
	"strings"
	"time"
)

const MaxInterruptDeliveryAttempts = 5

// InterruptDeliverFunc delivers a formatted proactive interrupt card to the active user surface
// (e.g., [HQ] Front Desk via LanguageServerService / agentapi or configured ChannelAdapter).
type InterruptDeliverFunc func(item OutboxItem, formattedCard string) error

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

// FormatInterruptCard renders a concise Markdown alert card for user delivery.
func FormatInterruptCard(it OutboxItem) string {
	badge := "🔔 Proactive Alert"
	if it.Critical {
		badge = "🚨 Critical Breach Alert"
	}
	scope := it.SourceSkill
	if strings.TrimSpace(it.Scope) != "" {
		scope += " · " + strings.TrimSpace(it.Scope)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("### %s (`%s`)\n", badge, scope))
	sb.WriteString(fmt.Sprintf("**%s**\n\n", strings.TrimSpace(it.Title)))
	if strings.TrimSpace(it.Summary) != "" {
		sb.WriteString(strings.TrimSpace(it.Summary) + "\n")
	}
	if len(it.ProposalIDs) > 0 {
		sb.WriteString(fmt.Sprintf("\n- **Pending Approval ID(s):** `%s`\n", strings.Join(it.ProposalIDs, "`, `")))
	}
	return strings.TrimSpace(sb.String())
}

// DispatchPendingInterrupts scans pending TierInterrupt items, holds non-critical items during quiet hours,
// invokes deliverFn, retries transient delivery errors with exponential backoff, and logs delivered alerts to reported.jsonl.
func DispatchPendingInterrupts(vaultDir string, now time.Time, deliverFn InterruptDeliverFunc) ([]OutboxItem, error) {
	if deliverFn == nil {
		return nil, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	items, err := LoadAll(vaultDir)
	if err != nil || len(items) == 0 {
		return nil, err
	}
	cfg := LoadAttentionConfig(vaultDir)
	inQuiet := InQuietHours(cfg, now)
	nowStr := now.UTC().Format(time.RFC3339)

	var delivered []OutboxItem
	dirty := false

	for i := range items {
		it := &items[i]
		if it.Tier != TierInterrupt || it.Status != StatusPending {
			continue
		}
		if inQuiet && !(it.Critical && cfg.QuietHours.AllowCriticalBreach) {
			continue
		}
		if it.NextAttemptAt != "" {
			if nextAt, pErr := time.Parse(time.RFC3339, it.NextAttemptAt); pErr == nil && now.Before(nextAt) {
				continue
			}
		}

		card := FormatInterruptCard(*it)
		if dErr := deliverFn(*it, card); dErr != nil {
			it.DeliveryAttempts++
			it.LastDeliveryError = strings.TrimSpace(dErr.Error())
			it.UpdatedAt = nowStr
			if it.DeliveryAttempts >= MaxInterruptDeliveryAttempts {
				// Downgrade to brief_rollup after exhausting live push retries so nothing is lost.
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
		it.UpdatedAt = nowStr
		delivered = append(delivered, *it)
		dirty = true
	}

	if dirty {
		if err := saveCanonical(vaultDir, items); err != nil {
			return delivered, err
		}
	}
	for _, it := range delivered {
		_ = RecordReportedFromItem(vaultDir, it, now)
	}
	return delivered, nil
}
