package webui

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/living"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/store"
)

const ConsoleLivingSpaceSlug = "hq-console"

// BuildConsoleSpaceDoc deterministically compiles live DashboardState into a mobile-first,
// 0-token 4-tab Living Space (`hq-console`: Needs Me, Work, Tokens, Ops) accessible both remotely
// in Antigravity Remote Control (`hq_console_live.html`) and locally at `/living-spaces/hq-console`.
func BuildConsoleSpaceDoc(vaultDir string, state DashboardState, now time.Time) living.SpaceDoc {
	if now.IsZero() {
		now = time.Now()
	}
	cfg := living.LoadConfig(vaultDir)

	biText := func(en, zh string) living.LocalizedText {
		if len(cfg.DefaultLocales) > 1 {
			return living.MultiText(map[string]string{"en": en, "zh": zh})
		}
		return living.Text(en)
	}

	sel := state.TaskSelection
	actionCount := ConsoleActionCount(state)
	actionTone := "ok"
	if actionCount > 0 {
		actionTone = "warn"
	}
	obsSum := state.Observability
	tokHeadline := fmt.Sprintf("%s · %.0f%% · %s",
		obs.FormatTokensCompact(obsSum.TodayUncachedTokens),
		obsSum.TodayCacheHitRatio*100.0,
		FormatUSD(obsSum.TodayEstCostUSD),
	)

	var blocks []living.BlockSpec

	// 1. Always-visible KPI banner across all 4 tabs
	blocks = append(blocks, living.BlockSpec{
		ID:   "console-kpi",
		Type: living.BlockKPIStrip,
		Metrics: []living.KPIMetric{
			{Label: biText("Action Required", "待处理决策"), Value: living.Text(fmt.Sprintf("%d", actionCount)), Tone: actionTone},
			{Label: biText("Active Tasks", "活跃任务"), Value: living.Text(fmt.Sprintf("%d", sel.ActiveCount)), Tone: "accent"},
			{Label: biText("Active Goals", "进行中目标"), Value: living.Text(fmt.Sprintf("%d", len(state.Goals))), Tone: "neutral"},
			{Label: biText("Today Uncached · Cache · Cost", "今日消耗 · 缓存 · 估算"), Value: living.Text(tokHeadline), Tone: "neutral"},
		},
	})

	// 2. Needs Me Tab: Pending Approvals, Expired Approvals, Decision Cards, Goals & Ideas
	var actionNotes []living.CalloutNote
	for _, p := range state.Pending {
		short := p.SHA256
		if len(short) > 12 {
			short = short[:12]
		}
		var bodyParts []string
		bodyParts = append(bodyParts, fmt.Sprintf("ID: %s · %s · sha256:%s", p.ID, p.ActionType, short))
		if p.ExpiresAt != "" {
			bodyParts = append(bodyParts, "Expires: "+p.ExpiresAt)
		}
		if len(p.RiskFlags) > 0 {
			bodyParts = append(bodyParts, "⚠️ PII Warning: "+strings.Join(p.RiskFlags, ", "))
		}
		if p.Metadata != nil {
			preview := p.Metadata["draftPayload"]
			if preview == "" {
				preview = p.Metadata["bodyPreview"]
			}
			if preview != "" {
				bodyParts = append(bodyParts, "Preview: "+store.TruncateRunes(preview, 280))
			}
		}
		bodyParts = append(bodyParts, "Cmd: "+store.TruncateRunes(p.Command, 180))
		actionNotes = append(actionNotes, living.CalloutNote{
			Tone:         "warn",
			Title:        living.Text("Pending Approval: " + p.Summary),
			Body:         living.Text(strings.Join(bodyParts, "\n")),
			Perspectives: []string{"needs_me"},
			CopyText:     fmt.Sprintf("hq approve %s --sha256=%s", p.ID, short),
			CopyLabel:    biText("Copy Approve", "复制批准命令"),
			CopyAltText:  fmt.Sprintf("hq reject %s --dismiss", p.ID),
			CopyAltLabel: biText("Copy Reject", "复制拒绝命令"),
		})
	}
	for _, rec := range state.ProactiveV3.ExpiredProposals {
		actionNotes = append(actionNotes, living.CalloutNote{
			Tone:         "warn",
			Title:        living.Text("Timed-Out Unsent: " + rec.Proposal.Summary),
			Body:         living.Text(fmt.Sprintf("ID: %s (%s) expired at %s without being sent.", rec.Proposal.ID, rec.Proposal.ActionType, rec.Timestamp)),
			Perspectives: []string{"needs_me"},
			CopyText:     fmt.Sprintf("hq approvals restage %s", rec.Proposal.ID),
			CopyLabel:    biText("Copy Re-open (24h)", "复制重开命令"),
			CopyAltText:  fmt.Sprintf("hq approvals dismiss %s", rec.Proposal.ID),
			CopyAltLabel: biText("Copy Dismiss", "复制清除命令"),
		})
	}
	for _, dc := range state.ProactiveV3.DecisionCards {
		var optLines []string
		primaryCmd, primaryLbl := "", ""
		altCmd, altLbl := "", ""
		for idx, opt := range dc.Decisions {
			cmd := fmt.Sprintf("hq outbox act %s --option=%s", dc.ID, opt.ID)
			optLines = append(optLines, fmt.Sprintf("[%s] %s", opt.ID, opt.Label))
			if idx == 0 {
				primaryCmd, primaryLbl = cmd, opt.Label
			} else if idx == 1 {
				altCmd, altLbl = cmd, opt.Label
			}
		}
		actionNotes = append(actionNotes, living.CalloutNote{
			Tone:         "info",
			Title:        living.Text("Decision (" + dc.SourceSkill + "): " + dc.Title),
			Body:         living.Text(store.TruncateRunes(dc.Summary, 240) + "\nOptions: " + strings.Join(optLines, " · ")),
			Perspectives: []string{"needs_me"},
			CopyText:     primaryCmd,
			CopyLabel:    living.Text(store.TruncateRunes(primaryLbl, 28)),
			CopyAltText:  altCmd,
			CopyAltLabel: living.Text(store.TruncateRunes(altLbl, 28)),
		})
	}
	if len(actionNotes) == 0 {
		actionNotes = append(actionNotes, living.CalloutNote{
			Tone:         "ok",
			Title:        biText("All Clear — Zero Pending Decisions", "全部已清零 — 暂无待审批事项"),
			Body:         biText("No pending approvals, timed-out cards, or unread decision nudges right now.", "当前没有待批准操作、过期草稿或决策提醒。"),
			Perspectives: []string{"needs_me"},
		})
	}
	blocks = append(blocks, living.BlockSpec{
		ID:           "console-actions",
		Type:         living.BlockCalloutNotes,
		Title:        biText("Action Required & Quick Decisions", "待处理决策与快捷操作"),
		Perspectives: []string{"needs_me"},
		Notes:        actionNotes,
	})

	if len(state.Goals) > 0 || len(state.OverdueWaits) > 0 {
		var goalRows []living.TableRow
		for _, g := range state.Goals {
			prog := g.Readiness()
			ready := fmt.Sprintf("%d/%d (%d%%)", prog.Confirmed, prog.Active, prog.Percent)
			if prog.Skipped > 0 {
				ready += fmt.Sprintf(" · %d skipped", prog.Skipped)
			}
			when := g.WindowStartAt
			if when == "" {
				when = g.TargetDate
			}
			status := g.Status
			if g.ParkedUntil != "" {
				status = "parked (" + g.ParkedUntil + ")"
			}
			goalRows = append(goalRows, living.TableRow{
				Cells: []living.LocalizedText{
					living.Text(g.Title),
					living.Text(g.Scope),
					living.Text(ready),
					living.Text(when),
					living.Text(status),
				},
			})
		}
		if len(goalRows) > 0 {
			blocks = append(blocks, living.BlockSpec{
				ID:           "console-goals",
				Type:         living.BlockComparisonTable,
				Title:        biText("Active Goals & 3-State Readiness", "进行中目标与三态进度"),
				Perspectives: []string{"needs_me"},
				Columns: []living.LocalizedText{
					biText("Goal", "目标"),
					biText("Scope", "领域"),
					biText("Readiness", "准备度"),
					biText("Target / Window", "时间窗口"),
					biText("Status", "状态"),
				},
				Rows: goalRows,
			})
		}
		if len(state.OverdueWaits) > 0 {
			var waitNotes []living.CalloutNote
			for _, ow := range state.OverdueWaits {
				waitNotes = append(waitNotes, living.CalloutNote{
					Tone:         "warn",
					Title:        living.Text(fmt.Sprintf("Overdue SLA: %s (%s)", ow.Waiting.Counterparty, ow.GoalSlug)),
					Body:         living.Text(fmt.Sprintf("%s · via %s (due %s)", ow.Waiting.Summary, ow.Waiting.Channel, ow.Waiting.FollowUpAfter)),
					Perspectives: []string{"needs_me"},
					CopyText:     fmt.Sprintf("hq goals mark-replied --goal=%s %s", ow.GoalSlug, ow.Waiting.ID),
					CopyLabel:    biText("Copy Mark Replied", "复制标记已回复"),
				})
			}
			blocks = append(blocks, living.BlockSpec{
				ID:           "console-waits",
				Type:         living.BlockCalloutNotes,
				Title:        biText("Overdue Counterparty SLAs", "超期跟进事项"),
				Perspectives: []string{"needs_me"},
				Notes:        waitNotes,
			})
		}
	}

	if len(state.Ideas) > 0 {
		var ideaNotes []living.CalloutNote
		for _, it := range state.Ideas {
			ideaNotes = append(ideaNotes, living.CalloutNote{
				Tone:         "info",
				Title:        living.Text("💡 " + it.Title),
				Body:         living.Text(fmt.Sprintf("[%s] %s", it.SourceSkill, store.TruncateRunes(it.Summary, 220))),
				Perspectives: []string{"needs_me"},
				CopyText:     fmt.Sprintf("hq ideas promote %s", it.ID),
				CopyLabel:    biText("Copy Promote", "复制升级任务"),
				CopyAltText:  fmt.Sprintf("hq ideas dismiss %s", it.ID),
				CopyAltLabel: biText("Copy Dismiss", "复制忽略"),
			})
		}
		blocks = append(blocks, living.BlockSpec{
			ID:           "console-ideas",
			Type:         living.BlockCalloutNotes,
			Title:        biText("Proactive Ideas Feed", "主动建议列表"),
			Perspectives: []string{"needs_me"},
			Notes:        ideaNotes,
		})
	}

	// 3. Work Tab: Task Board, Active Workspaces, and Living Spaces
	if len(sel.VisibleRows) > 0 {
		var taskNotes []living.CalloutNote
		wsByKey := surfaceEntriesByKey(state.Workspaces)
		for _, t := range sel.VisibleRows {
			cleanTitle := kernel.SanitizeTaskTitle(t.Title, t.TaskSlug)
			eff := t.EffectiveStatus(now)
			tone := "info"
			if eff == "active" {
				tone = "ok"
			} else if eff == "stalled" {
				tone = "warn"
			}
			var meta []string
			meta = append(meta, eff, t.TaskSlug)
			if age := t.FormatRelativeAge(now); age != "" {
				meta = append(meta, age)
			}
			if wsBadge := taskWorkspaceBadge(t, wsByKey); wsBadge != "" {
				meta = append(meta, wsBadge)
			}
			if tb, ok := obsSum.TaskBadges[strings.ToLower(strings.TrimSpace(t.ConversationID))]; ok && tb.GrossTokens > 0 {
				meta = append(meta, tb.CompactLabel)
			}
			body := strings.Join(meta, " · ")
			if s := store.TruncateRunes(strings.Join(strings.Fields(t.LastSummary), " "), 160); s != "" {
				body += "\n" + s
			}
			linkURL := ""
			if strings.TrimSpace(t.ConversationID) != "" {
				linkURL = "conversation://" + strings.TrimSpace(t.ConversationID)
			}
			taskNotes = append(taskNotes, living.CalloutNote{
				Tone:         tone,
				Title:        living.Text(cleanTitle),
				Body:         living.Text(body),
				Perspectives: []string{"work"},
				LinkURL:      linkURL,
				LinkLabel:    biText("Open Thread ↗", "打开会话 ↗"),
			})
		}
		blocks = append(blocks, living.BlockSpec{
			ID:           "console-tasks",
			Type:         living.BlockCalloutNotes,
			Title:        biText(fmt.Sprintf("Task Board (%d Active · %d Idle)", sel.ActiveCount, sel.IdleCount), fmt.Sprintf("任务看板 (%d 活跃 · %d 闲置)", sel.ActiveCount, sel.IdleCount)),
			Perspectives: []string{"work"},
			Notes:        taskNotes,
		})
	} else {
		blocks = append(blocks, living.BlockSpec{
			ID:           "console-tasks-empty",
			Type:         living.BlockCalloutNotes,
			Title:        biText("Task Board", "任务看板"),
			Perspectives: []string{"work"},
			Notes: []living.CalloutNote{{
				Tone:         "info",
				Title:        biText("No Active Tasks", "暂无活跃任务"),
				Body:         biText("No active or recently completed tasks on the live board.", "当前看板没有活跃或近期完成的任务。"),
				Perspectives: []string{"work"},
			}},
		})
	}

	if len(state.Workspaces) > 0 {
		var wsRows []living.TableRow
		for _, w := range state.Workspaces {
			badge := ""
			if w.NeedsOwner > 0 {
				badge = "warn"
			}
			wsRows = append(wsRows, living.TableRow{
				Cells: []living.LocalizedText{
					living.Text(w.Name),
					living.Text(w.Provider),
					living.Text(string(w.State)),
					living.Text(fmt.Sprintf("%d", w.OpenChanges)),
					living.Text(fmt.Sprintf("%d", w.NeedsOwner)),
				},
				Badge: badge,
			})
		}
		blocks = append(blocks, living.BlockSpec{
			ID:           "console-workspaces",
			Type:         living.BlockComparisonTable,
			Title:        biText("Active Workspaces", "活跃工作区"),
			Perspectives: []string{"work"},
			Columns: []living.LocalizedText{
				biText("Workspace", "工作区"),
				biText("Provider", "提供方"),
				biText("State", "状态"),
				biText("Open PRs/CLs", "未合并变更"),
				biText("Needs Owner", "待本人处理"),
			},
			Rows: wsRows,
		})
	}

	var otherSpaces []living.CalloutNote
	for _, ls := range state.ProactiveV3.LivingSpaces {
		if ls.Slug == ConsoleLivingSpaceSlug {
			continue
		}
		otherSpaces = append(otherSpaces, living.CalloutNote{
			Tone:         "info",
			Title:        living.Text("🧩 " + ls.Title),
			Body:         living.Text(fmt.Sprintf("Slug: %s · Updated %s", ls.Slug, ls.UpdatedAt)),
			Perspectives: []string{"work"},
			CopyText:     fmt.Sprintf("hq space export-artifact %s", ls.Slug),
			CopyLabel:    biText("Copy Export to Mobile", "复制推送到移动端"),
		})
	}
	if len(otherSpaces) > 0 {
		blocks = append(blocks, living.BlockSpec{
			ID:           "console-spaces-index",
			Type:         living.BlockCalloutNotes,
			Title:        biText("Your Living Spaces", "我的动态空间"),
			Perspectives: []string{"work"},
			Notes:        otherSpaces,
		})
	}

	// 4. Tokens Tab: Dual-Marker 512k Context Bar, Today/7d Per-Model, Per-Task Root Trees, Cache Savings & Trends
	blocks = append(blocks, buildConsoleTokensBlocks(obsSum, biText)...)

	// 5. Ops Tab: Schedules, System Health (`hq doctor`), Recent Journal & Reversals
	if len(state.Schedules) > 0 {
		var schedRows []living.TableRow
		for _, sc := range state.Schedules {
			status := "active"
			badge := ""
			if !sc.Enabled {
				status = "paused"
			} else if sc.LastError != "" {
				status = "error: " + store.TruncateRunes(sc.LastError, 60)
				badge = "warn"
			} else if sc.LastStatus != "" && sc.LastStatus != "ok" {
				status = store.TruncateRunes(sc.LastStatus, 60)
			}
			nextRun := sc.NextRunHuman
			if nextRun == "" {
				nextRun = sc.Cron
			}
			schedRows = append(schedRows, living.TableRow{
				Cells: []living.LocalizedText{
					living.Text(sc.ID),
					living.Text(nextRun),
					living.Text(sc.Runner),
					living.Text(status),
				},
				Badge: badge,
			})
		}
		blocks = append(blocks, living.BlockSpec{
			ID:           "console-schedules",
			Type:         living.BlockComparisonTable,
			Title:        biText("Scheduled Automations", "定时自动化任务"),
			Perspectives: []string{"ops"},
			Columns: []living.LocalizedText{
				biText("Schedule", "任务"),
				biText("Next Run", "下次运行"),
				biText("Runner", "执行器"),
				biText("Status", "状态"),
			},
			Rows: schedRows,
		})
	}

	var healthRows []living.TableRow
	for _, h := range state.Health {
		badge := ""
		if h.Status != "PASS" {
			badge = "warn"
		}
		healthRows = append(healthRows, living.TableRow{
			Cells: []living.LocalizedText{
				living.Text(h.Name),
				living.Text(h.Status),
				living.Text(store.TruncateRunes(h.Detail, 120)),
			},
			Badge: badge,
		})
	}
	if len(healthRows) > 0 {
		blocks = append(blocks, living.BlockSpec{
			ID:           "console-health",
			Type:         living.BlockComparisonTable,
			Title:        biText("System Health (hq doctor)", "系统健康检查 (hq doctor)"),
			Perspectives: []string{"ops"},
			Columns: []living.LocalizedText{
				biText("Check", "检查项"),
				biText("Status", "状态"),
				biText("Detail", "详情"),
			},
			Rows: healthRows,
		})
	}

	if len(state.ProactiveV3.RecentActivity) > 0 {
		var revNotes []living.CalloutNote
		for _, a := range state.ProactiveV3.RecentActivity {
			revNotes = append(revNotes, living.CalloutNote{
				Tone:         "info",
				Title:        living.Text(a.Kind + " · " + a.Target),
				Body:         living.Text(fmt.Sprintf("%s (%s)", a.Summary, a.Timestamp)),
				Perspectives: []string{"ops"},
			})
		}
		blocks = append(blocks, living.BlockSpec{
			ID:           "console-reversals",
			Type:         living.BlockCalloutNotes,
			Title:        biText("Recent User Reversals (hq cancel)", "近期撤销记录 (hq cancel)"),
			Perspectives: []string{"ops"},
			Notes:        revNotes,
		})
	}

	return living.SpaceDoc{
		SchemaVersion:      1,
		Slug:               ConsoleLivingSpaceSlug,
		Title:              biText("agy-hq Live Console", "agy-hq 实时控制台"),
		Subtitle:           biText("Synced "+state.UpdatedAt+" · Mobile & Remote Control ready (tap Copy chips to act via chat)", "同步于 "+state.UpdatedAt+" · 支持手机端 Remote Control（点击复制按钮贴入对话即可执行）"),
		Locales:            cfg.DefaultLocales,
		LocaleLabels:       cfg.LocaleLabels,
		DefaultPerspective: ConsoleDefaultTab(state),
		HideAllPerspective: true,
		Groups:             []string{"Today", "7d"},
		DefaultGroup:       "today",
		HideAllGroup:       true,
		GroupPerspectives:  []string{"tokens"},
		Perspectives: []living.PerspectiveSpec{
			{ID: "needs_me", Label: biText("Needs Me", "待我处理")},
			{ID: "work", Label: biText("Work", "工作看板")},
			{ID: "tokens", Label: biText("Tokens", "Token 用量")},
			{ID: "ops", Label: biText("Ops", "运维健康")},
		},
		Blocks:    blocks,
		UpdatedAt: now.UTC().Format(time.RFC3339),
	}
}

func buildConsoleTokensBlocks(obsSum obs.DashboardSummary, biText func(string, string) living.LocalizedText) []living.BlockSpec {
	var out []living.BlockSpec
	ceiling := obsSum.Budget.EffectiveContextCeiling()
	ceilStr := obs.FormatTokensCompact(ceiling)
	fdPct := ContextFillPct(obsSum.FrontDeskContextTokens, ceiling)
	warnPct := ContextFillPct(obsSum.Budget.WarnContextTokens, ceiling)
	critPct := ContextFillPct(obsSum.Budget.CritContextTokens, ceiling)

	fdTone := "ok"
	switch obsSum.FrontDeskContextStatus {
	case "CRITICAL":
		fdTone = "danger"
	case "WARN":
		fdTone = "warn"
	}
	fdVal := fmt.Sprintf("%s / %s (%.1f%%)", obs.FormatTokensCompact(obsSum.FrontDeskContextTokens), ceilStr, fdPct)
	if obsSum.FrontDeskCompactedPending {
		fdVal = fmt.Sprintf("Compacted (pending next turn) / %s", ceilStr)
	}
	fdSub := fmt.Sprintf("Status: %s · Cold-wakeup markers: %s warn / %s crit · Compactions: %d · Cache Hit: %.1f%%",
		obsSum.FrontDeskContextStatus,
		obs.FormatTokensCompact(obsSum.Budget.WarnContextTokens),
		obs.FormatTokensCompact(obsSum.Budget.CritContextTokens),
		obsSum.FrontDeskCompactions,
		obsSum.FrontDeskCacheHitRatio*100.0,
	)
	if obsSum.FrontDeskPostCompactTokens > 0 && !obsSum.FrontDeskCompactedPending {
		fdSub += fmt.Sprintf(" · Post-compact baseline: %s", obs.FormatTokensCompact(obsSum.FrontDeskPostCompactTokens))
	}

	ctxBars := []living.MetricBarRow{
		{
			Label:         biText("[HQ] Front Desk Context Window", "[HQ] 前台上下文窗口"),
			Value:         living.Text(fdVal),
			Sub:           living.Text(fdSub),
			Pct:           fdPct,
			WarnMarkerPct: warnPct,
			CritMarkerPct: critPct,
			Tone:          fdTone,
			Perspectives:  []string{"tokens"},
		},
	}
	if !obsSum.Budget.TokenCircuitDisabled && obsSum.Budget.DailyUncachedBudget > 0 {
		bTone := "accent"
		if obsSum.DailyBudgetPct >= 100 {
			bTone = "danger"
		} else if obsSum.DailyBudgetPct >= 80 {
			bTone = "warn"
		}
		ctxBars = append(ctxBars, living.MetricBarRow{
			Label:        biText("Daily Uncached Token Budget", "今日非缓存 Token 预算"),
			Value:        living.Text(fmt.Sprintf("%s / %s (%.1f%%)", obs.FormatTokensCompact(obsSum.TodayUncachedTokens), obs.FormatTokensCompact(obsSum.Budget.DailyUncachedBudget), obsSum.DailyBudgetPct)),
			Sub:          living.Text(fmt.Sprintf("Deep tier today: %s / %s (%.1f%%)", obs.FormatTokensCompact(obsSum.TodayDeepUncachedTokens), obs.FormatTokensCompact(obsSum.Budget.DailyDeepUncachedBudget), obsSum.DeepBudgetPct)),
			Pct:          obsSum.DailyBudgetPct,
			Tone:         bTone,
			Perspectives: []string{"tokens"},
		})
	}
	out = append(out, living.BlockSpec{
		ID:           "tokens-context",
		Type:         living.BlockMetricBars,
		Title:        biText("Context Hygiene & Daily Budget", "上下文水位与每日预算"),
		Perspectives: []string{"tokens"},
		Bars:         ctxBars,
	})

	// Windowed sections for "today" and "7d"
	for _, winKey := range []string{"today", "7d"} {
		wb, ok := obsSum.Windows[winKey]
		if !ok {
			wb = obs.WindowBreakdown{Window: winKey}
		}
		winLabelEN := "Today"
		winLabelZH := "今日"
		if winKey == "7d" {
			winLabelEN = "Last 7 Days"
			winLabelZH = "近 7 天"
		}

		// Summary & Prefix-Cache Efficiency Cards
		coldTone := "ok"
		if wb.ColdSpikes >= 3 {
			coldTone = "warn"
		}
		out = append(out, living.BlockSpec{
			ID:           "tokens-summary-" + winKey,
			Type:         living.BlockCalloutNotes,
			Group:        winKey,
			Title:        biText(winLabelEN+" Token & Prefix-Cache Summary", winLabelZH+" Token 与前缀缓存概览"),
			Perspectives: []string{"tokens"},
			Notes: []living.CalloutNote{
				{
					Tone:  "info",
					Title: biText("Uncached Burn & Est. API Cost", "非缓存消耗与 API 等效成本"),
					Body: living.Text(fmt.Sprintf("Uncached: %s (In: %s · Out: %s)\nEst. API-Equiv Cost: %s across %d turns (%d threads)",
						obs.FormatTokensCompact(wb.UncachedTokens),
						obs.FormatTokensCompact(wb.InputTokens),
						obs.FormatTokensCompact(wb.OutputTokens),
						FormatUSD(wb.EstCostUSD),
						wb.TurnCount,
						wb.ConvCount,
					)),
					Group:        winKey,
					Perspectives: []string{"tokens"},
				},
				{
					Tone:  coldTone,
					Title: biText("Prefix Cache Hit Ratio & Savings", "前缀缓存命中率与节省"),
					Body: living.Text(fmt.Sprintf("Cache Hit: %.1f%% (%s cached / %s gross)\nPrefix-Cache Saved: %s · Cold Wakeups (>=50k): %d (%s)",
						wb.CacheHitRatio*100.0,
						obs.FormatTokensCompact(wb.CacheReadTokens),
						obs.FormatTokensCompact(wb.GrossTokens),
						FormatUSD(wb.CacheSavedUSD),
						wb.ColdSpikes,
						obs.FormatTokensCompact(wb.ColdSpikeTokens),
					)),
					Group:        winKey,
					Perspectives: []string{"tokens"},
				},
			},
		})

		// Per-Model Table
		var modelRows []living.TableRow
		for _, mr := range wb.ByModel {
			modelRows = append(modelRows, living.TableRow{
				Group:        winKey,
				Perspectives: []string{"tokens"},
				Cells: []living.LocalizedText{
					living.Text(mr.Model),
					living.Text(mr.Tier),
					living.Text(fmt.Sprintf("%d", mr.TurnCount)),
					living.Text(fmt.Sprintf("%s (%s in + %s out)", obs.FormatTokensCompact(mr.UncachedTokens), obs.FormatTokensCompact(mr.InputTokens), obs.FormatTokensCompact(mr.OutputTokens))),
					living.Text(fmt.Sprintf("%s (%.0f%%)", obs.FormatTokensCompact(mr.CacheReadTokens), mr.CacheHitRatio*100.0)),
					living.Text(FormatUSD(mr.EstCostUSD)),
				},
			})
		}
		if len(modelRows) == 0 {
			modelRows = append(modelRows, living.TableRow{
				Group:        winKey,
				Perspectives: []string{"tokens"},
				Cells: []living.LocalizedText{
					biText("No turns in window", "窗口内暂无调用"),
					living.Text("—"),
					living.Text("0"),
					living.Text("0"),
					living.Text("0%"),
					living.Text("$0.00"),
				},
			})
		}
		out = append(out, living.BlockSpec{
			ID:           "tokens-models-" + winKey,
			Type:         living.BlockComparisonTable,
			Group:        winKey,
			Title:        biText(winLabelEN+" Usage by Model", winLabelZH+" 按模型用量明细"),
			Perspectives: []string{"tokens"},
			Columns: []living.LocalizedText{
				biText("Model", "模型"),
				biText("Tier", "层级"),
				biText("Turns", "轮次"),
				biText("Uncached (In + Out)", "非缓存 (输入+输出)"),
				biText("Cache Read (Hit %)", "缓存读取 (命中率)"),
				biText("Est. Cost", "等效成本"),
			},
			Rows: modelRows,
		})

		// Disjoint Per-Task Root Trees Accordion
		if len(wb.TopRootTrees) > 0 {
			var entries []living.TimelineEntry
			for idx, rt := range wb.TopRootTrees {
				var details []living.LocalizedText
				if rt.SubagentUncached > 0 {
					details = append(details, living.Text(fmt.Sprintf("Coordinator/Task Self: %s uncached · Subagents (%d): %s uncached",
						obs.FormatTokensCompact(rt.SelfUncached),
						rt.DescendantCount,
						obs.FormatTokensCompact(rt.SubagentUncached),
					)))
				} else {
					details = append(details, living.Text(fmt.Sprintf("Self: %s uncached (In: %s · Out: %s) · Peak Context: %s",
						obs.FormatTokensCompact(rt.UncachedTokens),
						obs.FormatTokensCompact(rt.InputTokens),
						obs.FormatTokensCompact(rt.OutputTokens),
						obs.FormatTokensCompact(rt.PeakContext),
					)))
				}
				for _, ch := range rt.Children {
					details = append(details, living.Text(fmt.Sprintf("↳ %s (%s): %s uncached · %.0f%% cached · %s",
						ch.Title,
						ch.Model,
						obs.FormatTokensCompact(ch.UncachedTokens),
						ch.CacheHitRatio*100.0,
						FormatUSD(ch.EstCostUSD),
					)))
				}
				entries = append(entries, living.TimelineEntry{
					ID:           fmt.Sprintf("tree-%s-%d", winKey, idx),
					DateLabel:    living.Text(FormatUSD(rt.EstCostUSD)),
					Title:        living.Text(store.TruncateRunes(rt.Title, 56)),
					Summary:      living.Text(fmt.Sprintf("%s uncached · %.0f%% cached", obs.FormatTokensCompact(rt.UncachedTokens), rt.CacheHitRatio*100.0)),
					Group:        winKey,
					Perspectives: []string{"tokens"},
					Details:      details,
				})
			}
			out = append(out, living.BlockSpec{
				ID:           "tokens-trees-" + winKey,
				Type:         living.BlockTimelineAccordion,
				Group:        winKey,
				Title:        biText(winLabelEN+" Top Task & Coordinator Trees (Disjoint)", winLabelZH+" 按任务树归集消耗（无重叠）"),
				Perspectives: []string{"tokens"},
				Entries:      entries,
			})
		}
	}

	// 7-Day Daily Trend Bars (visible on 7d filter)
	if len(obsSum.DailyTrend) > 0 {
		var maxDay int64 = 1
		for _, dp := range obsSum.DailyTrend {
			if dp.UncachedTokens > maxDay {
				maxDay = dp.UncachedTokens
			}
		}
		var dayBars []living.MetricBarRow
		for _, dp := range obsSum.DailyTrend {
			pct := (float64(dp.UncachedTokens) / float64(maxDay)) * 100.0
			dayBars = append(dayBars, living.MetricBarRow{
				Label:        living.Text(dp.Date),
				Value:        living.Text(fmt.Sprintf("%s uncached · %s", obs.FormatTokensCompact(dp.UncachedTokens), FormatUSD(dp.EstCostUSD))),
				Sub:          living.Text(fmt.Sprintf("Cache read: %s (%.0f%% hit) · Gross: %s", obs.FormatTokensCompact(dp.CacheReadTokens), dp.CacheHitRatio*100.0, obs.FormatTokensCompact(dp.GrossTokens))),
				Pct:          pct,
				Tone:         "accent",
				Group:        "7d",
				Perspectives: []string{"tokens"},
			})
		}
		out = append(out, living.BlockSpec{
			ID:           "tokens-trend-7d",
			Type:         living.BlockMetricBars,
			Group:        "7d",
			Title:        biText("7-Day Daily Burn Trend", "近 7 天每日消耗趋势"),
			Perspectives: []string{"tokens"},
			Bars:         dayBars,
		})
	}

	// Active 24h Anomalies (visible across Today & 7d on Tokens tab)
	if len(obsSum.Anomalies) > 0 {
		var anomNotes []living.CalloutNote
		for _, a := range obsSum.Anomalies {
			tone := "warn"
			if a.Severity == "CRITICAL" {
				tone = "danger"
			}
			copyCmd := ""
			if a.Code == "A1" {
				copyCmd = "/compact"
			} else if a.ConversationID != "" {
				copyCmd = "hq obs inspect " + store.ShortConvID(a.ConversationID)
			}
			anomNotes = append(anomNotes, living.CalloutNote{
				Tone:         tone,
				Title:        living.Text(fmt.Sprintf("[%s · %s] %s", a.Code, a.Severity, a.Kind)),
				Body:         living.Text(a.Summary + "\nFix: " + a.Remediation),
				Perspectives: []string{"tokens"},
				CopyText:     copyCmd,
				CopyLabel:    biText("Copy Action", "复制处理命令"),
			})
		}
		out = append(out, living.BlockSpec{
			ID:           "tokens-anomalies",
			Type:         living.BlockCalloutNotes,
			Title:        biText(fmt.Sprintf("Active 24h Token & Context Alerts (%d)", len(obsSum.Anomalies)), fmt.Sprintf("24 小时活跃异常告警 (%d)", len(obsSum.Anomalies))),
			Perspectives: []string{"tokens"},
			Notes:        anomNotes,
		})
	}

	return out
}

// SyncConsoleLivingSpace compiles DashboardState into `<vault>/living_spaces/hq-console/` AND
// `<frontDesk.ArtifactDir>/hq_console_live.html` (+ `.metadata.json`) in 0 LLM tokens,
// skipping disk writes when unchangedBody is true and output artifacts already exist.
func SyncConsoleLivingSpace(vaultDir string, state DashboardState, unchangedBody bool) error {
	now := time.Now()
	indexPath := filepath.Join(living.SpaceDir(vaultDir, ConsoleLivingSpaceSlug), "index.html")
	_, idxErr := os.Stat(indexPath)

	artDir := ""
	if state.hasFrontDesk {
		artDir = strings.TrimSpace(state.frontDesk.ArtifactDir)
	}
	artHTML := ""
	artMeta := ""
	needArt := false
	if artDir != "" {
		artHTML = filepath.Join(artDir, "hq_console_live.html")
		artMeta = artHTML + ".metadata.json"
		_, errH := os.Stat(artHTML)
		_, errM := os.Stat(artMeta)
		needArt = errH != nil || errM != nil
	}
	if unchangedBody && idxErr == nil && !needArt {
		return nil
	}

	doc := BuildConsoleSpaceDoc(vaultDir, state, now)
	htmlBytes, err := living.RenderHTML(doc, living.RenderOptions{Surface: "artifact"})
	if err != nil {
		return err
	}
	if artDir != "" && !needArt {
		if prevArt, rErr := os.ReadFile(artHTML); rErr == nil && stripConsoleHTMLTimestamps(string(prevArt)) == stripConsoleHTMLTimestamps(string(htmlBytes)) {
			if idxErr == nil {
				return nil
			}
		}
	}
	if _, err := living.Save(vaultDir, doc, now); err != nil {
		return err
	}
	if _, _, err := living.Build(vaultDir, ConsoleLivingSpaceSlug); err != nil {
		return err
	}
	if artDir != "" {
		_ = os.MkdirAll(artDir, 0o755)
		if err := store.WriteAtomic(artHTML, htmlBytes, 0o644); err != nil {
			return err
		}
		meta := map[string]interface{}{
			"artifactType":    "ARTIFACT_TYPE_OTHER",
			"requestFeedback": false,
			"summary":         "Live Interactive [HQ] Console (Mobile & Remote Control Ready) — Needs Me, Work, Tokens, and Ops",
			"updatedAt":       now.UTC().Format(time.RFC3339Nano),
			"userFacing":      true,
		}
		metaBytes, _ := json.MarshalIndent(meta, "", "  ")
		_ = store.WriteAtomic(artMeta, append(metaBytes, '\n'), 0o644)
	}
	return nil
}

func stripConsoleHTMLTimestamps(s string) string {
	// Strip volatile sync clock strings (HH:MM:SS and RFC3339 timestamps) so identical console snapshots skip disk writes.
	var out strings.Builder
	out.Grow(len(s))
	i := 0
	for i < len(s) {
		if i+20 <= len(s) && s[i] == '2' && s[i+1] == '0' && s[i+4] == '-' && s[i+7] == '-' && s[i+10] == 'T' && s[i+13] == ':' && s[i+16] == ':' {
			i += 20
			continue
		}
		if i+8 <= len(s) && s[i+2] == ':' && s[i+5] == ':' && s[i] >= '0' && s[i] <= '2' && s[i+1] >= '0' && s[i+1] <= '9' && s[i+3] >= '0' && s[i+3] <= '5' && s[i+4] >= '0' && s[i+4] <= '9' && s[i+6] >= '0' && s[i+6] <= '5' && s[i+7] >= '0' && s[i+7] <= '9' {
			i += 8
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}
