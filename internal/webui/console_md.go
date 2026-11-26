package webui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/living"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/timers"
)

func stripConsoleTimestampHeader(s string) string {
	if idx := strings.IndexByte(s, '\n'); idx >= 0 && strings.HasPrefix(s, "# ") {
		return s[idx+1:]
	}
	return s
}

// SyncMarkdownConsole deterministically regenerates `local/CONSOLE.md` and (if present)
// `<artifactDirectoryPath>/hq_console.md` from live Vault state in 0 LLM tokens.
func SyncMarkdownConsole(vaultDir string) error {
	return syncMarkdownConsole(vaultDir, 60*time.Second)
}

// SyncMarkdownConsoleForHook refreshes CONSOLE.md / hq_console.md on hook hot paths
// (SessionStart, PreToolUse, Stop) using the fast persisted observability summary cache
// and bounding total wall time so cosmetic console sync can never push a hook over its
// SIGKILL timeout budget. Inside Go tests it runs synchronously for deterministic assertions.
func SyncMarkdownConsoleForHook(vaultDir string, timeout time.Duration) {
	if timeout <= 0 || store.InGoTest() {
		_ = syncMarkdownConsole(vaultDir, 5*time.Minute)
		return
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = syncMarkdownConsole(vaultDir, 5*time.Minute)
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
	}
}

func syncMarkdownConsole(vaultDir string, obsCacheMaxAge time.Duration) error {
	invalidateStateCache(vaultDir)
	kernel.ReconcileFrontDeskFromTranscript(vaultDir, "")
	_, _ = approvals.ReconcileAskRejections(vaultDir, "", "", time.Now())
	_, _ = approvals.ReconcileLocalGitProposals(vaultDir, time.Now())
	state := loadStateWithObsMaxAge(vaultDir, "", obsCacheMaxAge)
	var b strings.Builder

	fmt.Fprintf(&b, "# 🏛️ agy-hq Console & Live Status (%s)\n\n", time.Now().Format("2006-01-02 15:04:05"))
	if state.hasFrontDesk && strings.TrimSpace(state.frontDesk.ArtifactDir) != "" {
		livePath := filepath.ToSlash(filepath.Join(strings.TrimSpace(state.frontDesk.ArtifactDir), "hq_console_live.html"))
		fmt.Fprintf(&b, "[📱 Interactive Mobile Console](file://%s) · [🎛️ Open Interactive Console (Side Pane)](sidecar://agy-hq-console/) · [🌐 Browser UI](http://127.0.0.1:4242)\n\n", livePath)
	} else {
		b.WriteString("[🎛️ Open Interactive Console (Side Pane)](sidecar://agy-hq-console/) · [🌐 Browser UI](http://127.0.0.1:4242)\n\n")
	}

	if len(state.Pending) > 0 {
		fmt.Fprintf(&b, "> [!IMPORTANT]\n> **Pending Approvals (`%d` item(s) waiting):**\n", len(state.Pending))
		for _, p := range state.Pending {
			short := p.SHA256
			if len(short) > 12 {
				short = short[:12]
			}
			piiBadge := ""
			if len(p.RiskFlags) > 0 {
				piiBadge = fmt.Sprintf(" · ⚠️ **PII WARNING [%s]**", strings.Join(p.RiskFlags, ", "))
			}
			expBadge := ""
			if p.ExpiresAt != "" {
				expBadge = fmt.Sprintf(" · `expires:%s`", p.ExpiresAt)
			}
			fmt.Fprintf(&b, "> - **`%s`** (`%s` · `sha256:%s`%s%s): %s — `%s`\n", p.ID, p.ActionType, short, expBadge, piiBadge, p.Summary, p.Command)
			payload := ""
			if p.Metadata != nil {
				if dp := p.Metadata["draftPayload"]; dp != "" {
					payload = dp
				} else if bp := p.Metadata["bodyPreview"]; bp != "" {
					payload = bp
				}
			}
			if payload != "" {
				previewText := store.TruncateRunes(payload, 600)
				b.WriteString(">   > **Message Preview:**\n")
				for _, line := range strings.Split(strings.TrimSpace(previewText), "\n") {
					fmt.Fprintf(&b, ">   > %s\n", line)
				}
			}
		}
		b.WriteString("\n---\n\n")
	} else {
		b.WriteString("> [!NOTE]\n> **Pending Approvals (`0` items):** All staged actions have been resolved.\n\n")
	}

	if len(state.ProactiveV3.ExpiredProposals) > 0 {
		fmt.Fprintf(&b, "> [!WARNING]\n> **Timed-Out Approvals Unsent (`%d` item(s)):**\n", len(state.ProactiveV3.ExpiredProposals))
		for _, rec := range state.ProactiveV3.ExpiredProposals {
			fmt.Fprintf(&b, "> - **`%s`** (`%s` · expired `%s`): %s — run `./bin/hq approvals restage %s`\n",
				rec.Proposal.ID, rec.Proposal.ActionType, rec.Timestamp, rec.Proposal.Summary, rec.Proposal.ID)
		}
		b.WriteString("\n")
	}

	if len(state.ProactiveV3.DecisionCards) > 0 {
		fmt.Fprintf(&b, "## ⚡ Active Decision Cards (`%d`)\n", len(state.ProactiveV3.DecisionCards))
		for _, dc := range state.ProactiveV3.DecisionCards {
			var opts []string
			for _, d := range dc.Decisions {
				opts = append(opts, fmt.Sprintf("`%s`: %s", d.ID, d.Label))
			}
			fmt.Fprintf(&b, "- **`%s`** (`%s`): **%s** — %s\n  - Options: %s *(run `./bin/hq outbox act %s --option=<id>`)*\n",
				dc.ID, dc.SourceSkill, dc.Title, store.TruncateRunes(dc.Summary, 160), strings.Join(opts, " · "), dc.ID)
		}
		b.WriteString("\n---\n\n")
	}

	var userSpaces []living.SpaceSummary
	for _, ls := range state.ProactiveV3.LivingSpaces {
		if ls.Slug != ConsoleLivingSpaceSlug {
			userSpaces = append(userSpaces, ls)
		}
	}
	if len(userSpaces) > 0 {
		fmt.Fprintf(&b, "## 🧩 Living Spaces (`%d` Local Micro-Apps)\n", len(userSpaces))
		for _, ls := range userSpaces {
			goalNote := ""
			if ls.GoalSlug != "" {
				goalNote = fmt.Sprintf(" · goal `%s`", ls.GoalSlug)
			}
			fmt.Fprintf(&b, "- **[%s](http://127.0.0.1:4242/living-spaces/%s)** (`%s`%s · updated `%s`)\n",
				ls.Title, ls.Slug, ls.Slug, goalNote, ls.UpdatedAt)
		}
		b.WriteString("\n---\n\n")
	}

	if len(state.Goals) > 0 || len(state.OverdueWaits) > 0 {
		fmt.Fprintf(&b, "## 🎯 Active Goals & Readiness (`%d` Goal(s) · `%d` Overdue Wait(s))\n", len(state.Goals), len(state.OverdueWaits))
		for _, g := range state.Goals {
			prog := g.Readiness()
			dueTag := ""
			if g.WindowStartAt != "" {
				dueTag = fmt.Sprintf(" · window `%s`", g.WindowStartAt)
			} else if g.TargetDate != "" {
				dueTag = fmt.Sprintf(" · target `%s`", g.TargetDate)
			}
			parkTag := ""
			if g.ParkedUntil != "" {
				parkTag = fmt.Sprintf(" · ⏸️ parked until `%s`", g.ParkedUntil)
			}
			skipTag := ""
			if prog.Skipped > 0 {
				skipTag = fmt.Sprintf(", %d skipped", prog.Skipped)
			}
			fmt.Fprintf(&b, "- **`%s`** — %s (`%d/%d` ready%s%s%s)\n", g.Slug, g.Title, prog.Confirmed, prog.Active, skipTag, dueTag, parkTag)
		}
		for _, ow := range state.OverdueWaits {
			fmt.Fprintf(&b, "- ⚠️ **Overdue SLA (`%s`):** Waiting on **%s** for *%s* (`%s` · deadline `%s`)\n", ow.GoalSlug, ow.Waiting.Counterparty, ow.Waiting.Summary, ow.Waiting.Channel, ow.Waiting.FollowUpAfter)
		}
		b.WriteString("\n---\n\n")
	}

	now := time.Now()
	sel := state.TaskSelection
	wsByKey := surfaceEntriesByKey(state.Workspaces)

	if state.Layer1Count == 0 && state.ColdTaskCount == 0 {
		fmt.Fprintf(&b, "## 📋 2. Task Board (`0` Tracked Conversations)\n")
		b.WriteString("- None registered in `local/memory/tasks.json`.\n\n---\n\n")
	} else {
		fmt.Fprintf(&b, "## 📋 2. Task Board (`%d` Active · `%d` Idle · `%d` Completed · `%d` Archived)\n",
			sel.ActiveCount, sel.IdleCount, sel.CompletedCount, sel.ArchivedHotCount+sel.ArchivedColdCount)
		if len(sel.VisibleRows) == 0 {
			b.WriteString("- No active or recently completed tasks on the live board.\n")
		} else {
			b.WriteString("| Status | Task Conversation | Slug | Latest Progress |\n| :--- | :--- | :--- | :--- |\n")
			for _, t := range sel.VisibleRows {
				cleanTitle := kernel.SanitizeTaskTitle(t.Title, t.TaskSlug)
				oneLine := store.TruncateRunes(strings.ReplaceAll(strings.Join(strings.Fields(t.LastSummary), " "), "|", "\\|"), 130)
				eff := t.EffectiveStatus(now)
				badge := "🟢 `active`"
				switch eff {
				case "idle":
					badge = "⏸️ `idle`"
				case "stalled":
					badge = "⚠️ `stalled`"
				case "completed":
					badge = "✅ `completed`"
				case "cancelled":
					badge = "🛑 `cancelled`"
				case "archived":
					badge = "📦 `archived`"
				}
				if t.Pinned && eff == "active" {
					badge = "📌 🟢 `active`"
				}
				slugCell := fmt.Sprintf("`%s`", t.TaskSlug)
				var metaParts []string
				if age := t.FormatRelativeAge(now); age != "" {
					metaParts = append(metaParts, age)
				}
				if existingWTs := t.ExistingWorktreeDirs(); len(existingWTs) == 1 {
					metaParts = append(metaParts, "🌿 wt")
				} else if len(existingWTs) > 1 {
					metaParts = append(metaParts, fmt.Sprintf("🌿 %d wts", len(existingWTs)))
				}
				if wsBadge := taskWorkspaceBadge(t, wsByKey); wsBadge != "" {
					metaParts = append(metaParts, wsBadge)
				}
				if tb, ok := state.Observability.TaskBadges[strings.ToLower(strings.TrimSpace(t.ConversationID))]; ok && tb.GrossTokens > 0 {
					metaParts = append(metaParts, "🪙 "+obs.FormatTokensCompact(tb.TreeUncachedTokens))
				}
				if len(metaParts) > 0 {
					slugCell += fmt.Sprintf(" · *%s*", strings.Join(metaParts, " · "))
				}
				fmt.Fprintf(&b, "| %s | **[%s](conversation://%s)** | %s | %s |\n", badge, cleanTitle, t.ConversationID, slugCell, oneLine)
			}
		}
		if sel.ArchivedHotCount > 0 {
			fmt.Fprintf(&b, "\n- *%d archived task(s) preserved in local/memory/tasks.json*\n", sel.ArchivedHotCount)
		}
		if sel.HiddenCompletedCount > 0 || sel.HiddenIdleCount > 0 || sel.ArchivedColdCount > 0 {
			fmt.Fprintf(&b, "- *%d older completed, %d idle, and %d cold-archived task(s) omitted from live console — run `./bin/hq tasks --all` or `./bin/hq tasks --archive` to inspect.*\n",
				sel.HiddenCompletedCount, sel.HiddenIdleCount, sel.ArchivedColdCount)
		}
		b.WriteString("\n---\n\n")
	}

	b.WriteString(formatConsoleWorkspacesSection(state.Workspaces))
	b.WriteString("\n---\n\n")

	if len(state.ActiveSpaces) > 0 {
		fmt.Fprintf(&b, "## 🗂️ Active Console Spaces (`%d` Active)\n", len(state.ActiveSpaces))
		for _, sp := range state.ActiveSpaces {
			pinTag := ""
			if sp.OverrideMode == "pin" {
				pinTag = " · `PINNED`"
			}
			fmt.Fprintf(&b, "- **%s** (`%s` · `%s`%s)\n", sp.Title, sp.ID, sp.Schedule, pinTag)
			for _, card := range sp.RenderedCards {
				fmt.Fprintf(&b, "  - *%s:* %s\n", card.Title, strings.Join(card.Lines, " | "))
			}
		}
		b.WriteString("\n---\n\n")
	}

	if len(state.Ideas) > 0 {
		fmt.Fprintf(&b, "## 💡 Proactive Ideas Feed (`%d` Pending)\n", len(state.Ideas))
		for _, it := range state.Ideas {
			fmt.Fprintf(&b, "- **`%s`** (`%s`): **%s** — %s *(run `./bin/hq ideas promote %s` or `dismiss %s`)*\n", it.ID, it.SourceSkill, it.Title, it.Summary, it.ID, it.ID)
		}
		b.WriteString("\n---\n\n")
	}

	b.WriteString("## 🔧 Ops & Diagnostics Inspector\n\n")
	b.WriteString("## 📊 1. Front Desk Lease & Daily Quota\n")
	if state.hasFrontDesk {
		fd := state.frontDesk
		fmt.Fprintf(&b, "- **Active Front Desk:** [`[HQ] Front Desk`](conversation://%s) (`%s`)\n", fd.ConversationID, fd.ConversationID)
		pinStatus := "⚠️ Pending Turn-1 write_file pin"
		if fd.ConsolePinned && fd.LiveConsolePinned {
			pinStatus = "✅ Pinned in UI (hq_console.md + hq_console_live.html)"
		} else if fd.ConsolePinned || fd.LiveConsolePinned {
			pinStatus = fmt.Sprintf("⚠️ Partial UI pin (md=%v, live=%v)", fd.ConsolePinned, fd.LiveConsolePinned)
		}
		fmt.Fprintf(&b, "- **Console Artifact Pin:** `%s`\n", pinStatus)
		armed, unarmed := kernel.GetUnarmedTriggers(vaultDir)
		totalTriggers := len(armed) + len(unarmed)
		if totalTriggers > 0 {
			if kernel.IsDaemonActiveFast(vaultDir) {
				fmt.Fprintf(&b, "- **Proactive Schedules (`triggers.yaml`):** `%d / %d active in hq daemon`\n", len(armed), totalTriggers)
			} else {
				fmt.Fprintf(&b, "- **Proactive Schedules (`triggers.yaml`):** `%d / %d armed in Front Desk`\n", len(armed), totalTriggers)
			}
		}
		if len(fd.AdHocCrons) > 0 {
			fmt.Fprintf(&b, "- **Ad-Hoc Conversation Crons (Front Desk):** `%d active`\n", len(fd.AdHocCrons))
		}
	} else {
		b.WriteString("- **Active Front Desk:** `VACANT` *(the next new conversation opened in `headquarter` will automatically claim `[HQ] Front Desk`)*\n")
	}
	fmt.Fprintf(&b, "- **Daemon & Connector Health:** `%s`\n", kernel.BuildMonitorHealthSummary(vaultDir, time.Now()).FormatLine(time.Now()))
	circuitBadge := "BUDGET OK"
	if state.Quota.CircuitOpen {
		circuitBadge = "CIRCUIT OPEN: " + state.Quota.CircuitReason
	}
	gatekeeperNote := ""
	if state.Quota.GatekeeperSavesToday > 0 {
		gatekeeperNote = fmt.Sprintf(" · 0-Token Gatekeeper Skips: `%d`", state.Quota.GatekeeperSavesToday)
	}
	interNote := ""
	if state.Quota.InteractiveSpawnsToday > 0 {
		interNote = fmt.Sprintf(" · Interactive: `+%d`", state.Quota.InteractiveSpawnsToday)
	}
	fmt.Fprintf(&b, "- **Background Auto Spawns Today:** `%d / %d` (`%s`%s%s)\n", state.Quota.EffectiveAutoSpawns(), state.Quota.MaxDailySpawns, circuitBadge, interNote, gatekeeperNote)
	ceilStr := obs.FormatTokensCompact(state.Observability.Budget.EffectiveContextCeiling())
	if state.Observability.Budget.TokenCircuitDisabled {
		fmt.Fprintf(&b, "- **Token Burn Today (Uncached):** `%s` (`budget guard disabled` · Gross: `%s` · Prefix CHR: `%.1f%%` · Est. Cost: `%s` · Cache Saved: `%s` · Deep: `%s`)\n",
			obs.FormatTokensCompact(state.Observability.TodayUncachedTokens),
			obs.FormatTokensCompact(state.Observability.TodayGrossTokens),
			state.Observability.TodayCacheHitRatio*100.0,
			FormatUSD(state.Observability.TodayEstCostUSD),
			FormatUSD(state.Observability.TodayCacheSavedUSD),
			obs.FormatTokensCompact(state.Observability.TodayDeepUncachedTokens),
		)
	} else {
		fmt.Fprintf(&b, "- **Token Burn Today (Uncached / Budget):** `%s / %s` (`%.1f%%` · Gross: `%s` · Prefix CHR: `%.1f%%` · Est. Cost: `%s` · Cache Saved: `%s` · Deep: `%s / %s`)\n",
			obs.FormatTokensCompact(state.Observability.TodayUncachedTokens),
			obs.FormatTokensCompact(state.Observability.Budget.DailyUncachedBudget),
			state.Observability.DailyBudgetPct,
			obs.FormatTokensCompact(state.Observability.TodayGrossTokens),
			state.Observability.TodayCacheHitRatio*100.0,
			FormatUSD(state.Observability.TodayEstCostUSD),
			FormatUSD(state.Observability.TodayCacheSavedUSD),
			obs.FormatTokensCompact(state.Observability.TodayDeepUncachedTokens),
			obs.FormatTokensCompact(state.Observability.Budget.DailyDeepUncachedBudget),
		)
	}
	fdCtxDisplay := obs.FormatTokensCompact(state.Observability.FrontDeskContextTokens) + " / " + ceilStr
	if state.Observability.FrontDeskCompactedPending {
		fdCtxDisplay = "compacted (pending next turn) / " + ceilStr
	}
	fmt.Fprintf(&b, "- **Front Desk Context Watermark:** `%s` (`%s` · Compactions: `%d` · CHR: `%.1f%%`)\n",
		fdCtxDisplay,
		state.Observability.FrontDeskContextStatus,
		state.Observability.FrontDeskCompactions,
		state.Observability.FrontDeskCacheHitRatio*100.0,
	)
	lastHB := state.Quota.LastStatus
	if lastHB == "" {
		lastHB = "Not run yet today"
	}
	evStats := events.GetSummaryStats(vaultDir, time.Now())
	armedTimers, _ := timers.List(vaultDir, false)
	if evStats.PendingCount > 0 || evStats.LeasedCount > 0 || evStats.Dropped24h > 0 || len(armedTimers) > 0 {
		fmt.Fprintf(&b, "- **Durable Event Inbox & Timers:** `pending=%d` · `leased=%d` · `dropped24h=%d` · `armedTimers=%d`\n",
			evStats.PendingCount, evStats.LeasedCount, evStats.Dropped24h, len(armedTimers))
	}
	fmt.Fprintf(&b, "- **Last Heartbeat:** `%s`\n\n---\n\n", lastHB)

	if len(state.Schedules) > 0 {
		fmt.Fprintf(&b, "## ⏰ Scheduled Automations (`%d` Configured)\n", len(state.Schedules))
		b.WriteString("| Schedule | Runner | Next Run / Cron | Latest Activity |\n| :--- | :--- | :--- | :--- |\n")
		for _, sc := range state.Schedules {
			stateIcon := "🟢"
			if !sc.Enabled {
				stateIcon = "⏸️"
			} else if sc.LastError != "" || (sc.LastStatus != "" && sc.LastStatus != "ok" && strings.Contains(sc.LastStatus, "error")) {
				stateIcon = "⚠️"
			} else if sc.LastStatus != "" && sc.LastStatus != "ok" {
				stateIcon = "⏸️"
			} else if sc.Kind == "conversation_cron" {
				stateIcon = "🕒"
			}
			nameCell := fmt.Sprintf("%s **`%s`**", stateIcon, sc.ID)
			if sc.Summary != "" {
				sumClean := store.TruncateRunes(strings.ReplaceAll(strings.Join(strings.Fields(sc.Summary), " "), "|", "\\|"), 70)
				nameCell += " — " + sumClean
			}
			var actParts []string
			if sc.LastTriggeredAt != "" {
				actParts = append(actParts, "fired `"+sc.LastTriggeredAt+"`")
			} else if sc.LastCheckedAt != "" {
				actParts = append(actParts, "checked `"+sc.LastCheckedAt+"`")
			}
			if sc.SkipsCount > 0 {
				actParts = append(actParts, fmt.Sprintf("`%d` skips", sc.SkipsCount))
			}
			if sc.LatestConvID != "" {
				shortID := sc.LatestConvID
				if len(shortID) > 8 {
					shortID = shortID[:8]
				}
				actParts = append(actParts, fmt.Sprintf("[`%s`](conversation://%s)", shortID, sc.LatestConvID))
			}
			if sc.LastError != "" {
				errClean := strings.ReplaceAll(strings.Join(strings.Fields(sc.LastError), " "), "|", "\\|")
				actParts = append(actParts, "⚠️ "+errClean)
			} else if sc.LastStatus != "" && sc.LastStatus != "ok" {
				stClean := strings.ReplaceAll(strings.Join(strings.Fields(sc.LastStatus), " "), "|", "\\|")
				actParts = append(actParts, "⏸️ "+stClean)
			}
			actCell := "—"
			if len(actParts) > 0 {
				actCell = strings.Join(actParts, " · ")
			}
			cronClean := strings.ReplaceAll(sc.Cron, "|", "\\|")
			if sc.NextRunHuman != "" {
				cronClean = sc.NextRunHuman + " · `" + cronClean + "`"
			} else {
				cronClean = "`" + cronClean + "`"
			}
			runnerClean := strings.ReplaceAll(sc.Runner, "|", "\\|")
			fmt.Fprintf(&b, "| %s | `%s` | %s | %s |\n", nameCell, runnerClean, cronClean, actCell)
		}
		b.WriteString("\n---\n\n")
	}

	b.WriteString("## 🕒 4. Recent Turn Activity (Today's Journal)\n")
	if len(state.TodayTurns) == 0 {
		b.WriteString("- No turns logged yet today.\n\n---\n\n")
	} else {
		limit := len(state.TodayTurns)
		if limit > 5 {
			limit = 5
		}
		for _, j := range state.TodayTurns[:limit] {
			rawPrompt := j.UserPrompt
			if strings.TrimSpace(rawPrompt) == "" {
				rawPrompt = j.ResponseSummary
			}
			prompt := store.TruncateRunes(strings.Join(strings.Fields(rawPrompt), " "), 90)
			shortConv := j.ConversationID
			if len(shortConv) > 8 {
				shortConv = shortConv[:8]
			}
			fmt.Fprintf(&b, "- `%s` · [`%s`](conversation://%s) (`%s`): %s\n", j.Timestamp, shortConv, j.ConversationID, j.ModelName, prompt)
		}
		b.WriteString("\n---\n\n")
	}

	if len(state.ProactiveV3.RecentActivity) > 0 {
		b.WriteString("## ↩️ Recent User Reversals (`hq cancel`)\n")
		for _, a := range state.ProactiveV3.RecentActivity {
			fmt.Fprintf(&b, "- `%s` (`%s`): %s\n", a.Timestamp, a.Kind, a.Summary)
		}
		b.WriteString("\n---\n\n")
	}

	appendDelegatesMarkdownSection(&b, state.Delegates)

	b.WriteString("## 🩺 5. System Health (`hq doctor`)\n")
	var warnChecks []string
	passCount := 0
	tokPassDetail := ""
	for _, h := range state.Health {
		if h.Status == "PASS" {
			passCount++
			if h.Name == "Token Budget & Context Health" {
				tokPassDetail = h.Detail
			}
		} else {
			warnChecks = append(warnChecks, fmt.Sprintf("| **%s** | ⚠️ `%s` | %s |", h.Name, h.Status, h.Detail))
		}
	}
	if len(warnChecks) > 0 {
		b.WriteString("| Check | Status | Detail |\n| :--- | :--- | :--- |\n")
		for _, row := range warnChecks {
			b.WriteString(row + "\n")
		}
		if passCount > 0 {
			if tokPassDetail != "" {
				fmt.Fprintf(&b, "\n- ✅ *%d other check(s) `PASS` (%s) — see **Ops** tab in Interactive Console or run `./bin/hq doctor`.*\n", passCount, tokPassDetail)
			} else {
				fmt.Fprintf(&b, "\n- ✅ *%d other check(s) `PASS` — see **Ops** tab in Interactive Console or run `./bin/hq doctor`.*\n", passCount)
			}
		}
	} else {
		if tokPassDetail != "" {
			fmt.Fprintf(&b, "- ✅ **All `%d` checks `PASS`** *(%s)* — see **Ops** tab in Interactive Console or run `./bin/hq doctor` for full breakdown.\n", passCount, tokPassDetail)
		} else {
			fmt.Fprintf(&b, "- ✅ **All `%d` checks `PASS`** — see **Ops** tab in Interactive Console or run `./bin/hq doctor` for full breakdown.\n", passCount)
		}
	}

	rendered := b.String()
	content := []byte(rendered)
	consolePath := filepath.Join(vaultDir, "CONSOLE.md")
	unchangedBody := false
	if prevBytes, rErr := os.ReadFile(consolePath); rErr == nil {
		unchangedBody = stripConsoleTimestampHeader(string(prevBytes)) == stripConsoleTimestampHeader(rendered)
	}
	if !unchangedBody {
		if err := store.WriteAtomic(consolePath, content, 0o644); err != nil {
			return err
		}
	}

	if state.hasFrontDesk && strings.TrimSpace(state.frontDesk.ArtifactDir) != "" {
		artDir := strings.TrimSpace(state.frontDesk.ArtifactDir)
		artConsole := filepath.Join(artDir, "hq_console.md")
		artMeta := filepath.Join(artDir, "hq_console.md.metadata.json")
		_, errArt := os.Stat(artConsole)
		_, errMeta := os.Stat(artMeta)
		if !unchangedBody || errArt != nil || errMeta != nil {
			_ = os.MkdirAll(artDir, 0o755)
			meta := map[string]interface{}{
				"summary":         "Live [HQ] Front Desk Console — Pending Approvals, Task Board, Active Worktrees & Workspaces, Recent Turn Activity, and System Health.",
				"updatedAt":       time.Now().UTC().Format(time.RFC3339Nano),
				"requestFeedback": false,
				"userFacing":      true,
			}
			metaBytes, _ := json.MarshalIndent(meta, "", "  ")
			_ = os.Remove(filepath.Join(artDir, "hq_console_mobile.md"))
			_ = os.Remove(filepath.Join(artDir, "hq_console_mobile.md.metadata.json"))
			_ = store.WriteAtomic(artConsole, content, 0o644)
			_ = store.WriteAtomic(artMeta, metaBytes, 0o644)
		}
	}
	_ = SyncConsoleLiveArtifact(vaultDir, state, unchangedBody)
	return nil
}
