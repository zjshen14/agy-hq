package webui

import (
	"fmt"
	"strings"

	"github.com/zjshen14/agy-hq/internal/delegates"
)

// appendDelegatesMarkdownSection appends the read-only Delegated Peers status snippet
// to CONSOLE.md / hq_console.md when a delegate policy or active delegate transport exists.
func appendDelegatesMarkdownSection(b *strings.Builder, sum delegates.DashboardSummary) {
	if !sum.PolicyConfigured && sum.TransportCount == 0 && sum.PendingNudges == 0 {
		return
	}
	fmt.Fprintf(b, "## 🛡️ Delegated Peers (`%d/%d` Policies Enabled · `%d` Transport(s) · Replies 1h: `%d/%d` · Queued Nudges: `%d`)\n",
		sum.EnabledPolicyCount, sum.PolicyCount, sum.TransportCount, sum.RepliesLastHour, sum.GlobalMaxPerHour, sum.PendingNudges)
	if len(sum.RecentAudit) == 0 {
		b.WriteString("- No recent delegated peer activity.\n\n---\n\n")
		return
	}
	limit := len(sum.RecentAudit)
	if limit > 5 {
		limit = 5
	}
	for _, a := range sum.RecentAudit[:limit] {
		fmt.Fprintf(b, "- `%s` · **%s** (`%s` · `%s` -> `%s`): %s\n",
			a.Timestamp, mdEscapeInline(a.Action), mdEscapeInline(a.PolicyID), mdEscapeInline(a.CallerID), mdEscapeInline(a.Outcome), mdEscapeInline(truncateRunes(a.Detail, 100)))
	}
	b.WriteString("\n---\n\n")
}
