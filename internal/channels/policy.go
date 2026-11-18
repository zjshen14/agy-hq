package channels

import (
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/outbox"
)

var (
	reHTTPScheme  = regexp.MustCompile(`(?i)\bhttp://`)
	reHTTPSScheme = regexp.MustCompile(`(?i)\bhttps://`)
)

// DefangURLs neutralizes http:// and https:// schemes in tainted notification bodies
// so external chat clients never auto-fetch attacker tracking beacons (`Finding R4`).
func DefangURLs(s string) string {
	if strings.TrimSpace(s) == "" {
		return ""
	}
	out := reHTTPSScheme.ReplaceAllString(s, "hxxps://")
	out = reHTTPScheme.ReplaceAllString(out, "hxxp://")
	return out
}

func truncateUTF8Runes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return s
	}
	runes := []rune(s)
	if len(runes) <= maxRunes {
		return s
	}
	if maxRunes <= 3 {
		return string(runes[:maxRunes])
	}
	return string(runes[:maxRunes-3]) + "..."
}

// SanitizeAndFormatNotification applies SanitizeAntiCue, tainted URL defanging, privacy_mode,
// and UTF-8 rune truncation uniformly across every outbound bridge notification.
func SanitizeAndFormatNotification(in NotificationPayload, pol ResolvedPolicy) NotificationPayload {
	out := in
	out.SourceSkill = outbox.SanitizeAntiCue(in.SourceSkill, 80)
	out.Scope = outbox.SanitizeAntiCue(in.Scope, 120)

	if pol.PrivacyMode == PrivacyModeZeroBody {
		switch in.Kind {
		case KindDeskReply:
			out.Title = "agy-hq: New reply in [HQ] Front Desk"
			out.BodyMarkdown = "Open [HQ] Front Desk to view response details."
		case KindTaskBlocked:
			out.Title = "agy-hq: 1 background task awaiting approval"
			out.BodyMarkdown = "Open [HQ] Front Desk or Web Console to review."
		case KindTaskDone:
			out.Title = "agy-hq: 1 background task completed"
			out.BodyMarkdown = "Open [HQ] Front Desk to view completion summary."
		case KindTestPing:
			out.Title = "agy-hq: Channel bridge connectivity check"
			out.BodyMarkdown = "Zero-body connectivity test succeeded."
		default:
			out.Title = "agy-hq: 1 urgent item awaiting review in Front Desk"
			out.BodyMarkdown = "Open [HQ] Front Desk to inspect details."
		}
		out.ProposalIDs = nil
		out.Actions = nil
		return out
	}

	maxRunes := pol.MaxBodyRunes
	if maxRunes <= 0 {
		maxRunes = DefaultMaxBodyRunes
	}
	if maxRunes > HardMaxBodyRunes {
		maxRunes = HardMaxBodyRunes
	}

	out.Title = outbox.SanitizeAntiCue(in.Title, 200)
	body := outbox.SanitizeAntiCue(in.BodyMarkdown, maxRunes)
	if in.TaintedOrigin {
		out.Title = DefangURLs(out.Title)
		body = DefangURLs(body)
	}
	out.BodyMarkdown = truncateUTF8Runes(body, maxRunes)
	return out
}

// PolicyDisposition describes whether a queued notification should send now, hold for quiet hours, or roll up to Morning Brief.
type PolicyDisposition string

const (
	DispositionSendNow      PolicyDisposition = "send_now"
	DispositionHoldQuiet    PolicyDisposition = "hold_quiet_hours"
	DispositionRollupRate   PolicyDisposition = "rollup_rate_capped"
	DispositionRollupQuiet  PolicyDisposition = "rollup_quiet_hours"
	DispositionDropDisabled PolicyDisposition = "drop_disabled"
)

func filterRolling1H(tsList []string, now time.Time) []string {
	cutoff := now.Add(-1 * time.Hour)
	var kept []string
	for _, raw := range tsList {
		if t, err := time.Parse(time.RFC3339, raw); err == nil && !t.Before(cutoff) {
			kept = append(kept, raw)
		}
	}
	return kept
}

// EvaluateNotificationPolicy enforces quiet hours, non-urgent hourly rate caps, and runaway ceilings.
// Mutates rw in place when DispositionSendNow is returned.
func EvaluateNotificationPolicy(
	item QueuedNotification,
	pol ResolvedPolicy,
	attCfg outbox.AttentionConfig,
	rw *RateWindowState,
	now time.Time,
) PolicyDisposition {
	kind := item.Notification.Kind

	// User-initiated test pings and correlated `desk_reply` turns bypass quiet hours and unsolicited rate caps.
	if kind == KindTestPing || kind == KindDeskReply {
		return DispositionSendNow
	}

	switch kind {
	case KindInterrupt, KindCriticalBreach:
		if !pol.DeliverInterrupts {
			return DispositionDropDisabled
		}
	case KindTaskDone:
		if !pol.DeliverTaskDone {
			return DispositionDropDisabled
		}
	case KindTaskBlocked:
		if !pol.DeliverTaskBlocked {
			return DispositionDropDisabled
		}
	}

	inQuiet := pol.RespectQuietHours && outbox.InQuietHours(attCfg, now)
	if inQuiet {
		canPierce := (item.Critical || kind == KindCriticalBreach) &&
			!item.Notification.TaintedOrigin &&
			attCfg.QuietHours.AllowCriticalBreach &&
			pol.AllowCriticalBreach
		if !canPierce {
			if kind == KindTaskDone || kind == KindTaskBlocked {
				return DispositionRollupQuiet
			}
			return DispositionHoldQuiet
		}
	}

	rw.NonUrgentTimestamps = filterRolling1H(rw.NonUrgentTimestamps, now)
	rw.UnsolicitedTimestamps = filterRolling1H(rw.UnsolicitedTimestamps, now)

	if len(rw.UnsolicitedTimestamps) >= RunawayUnsolicitedCap1H {
		return DispositionRollupRate
	}
	if kind == KindTaskDone || kind == KindTaskBlocked {
		if pol.MaxNonUrgentPerHour <= 0 || len(rw.NonUrgentTimestamps) >= pol.MaxNonUrgentPerHour {
			return DispositionRollupRate
		}
		rw.NonUrgentTimestamps = append(rw.NonUrgentTimestamps, now.UTC().Format(time.RFC3339))
	}
	rw.UnsolicitedTimestamps = append(rw.UnsolicitedTimestamps, now.UTC().Format(time.RFC3339))
	return DispositionSendNow
}
