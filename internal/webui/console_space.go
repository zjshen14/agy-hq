package webui

import (
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
// 0-token Living Space (`hq-console`) accessible both remotely in Antigravity Remote Control
// (`hq_console_live.html`) and locally at `http://127.0.0.1:4242/living-spaces/hq-console`.
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
	actionCount := len(state.Pending) + len(state.ProactiveV3.ExpiredProposals) + len(state.ProactiveV3.DecisionCards)
	actionTone := "ok"
	if actionCount > 0 {
		actionTone = "warn"
	}
	budgetVal := obs.FormatTokensCompact(state.Observability.TodayUncachedTokens)
	if !state.Observability.Budget.TokenCircuitDisabled && state.Observability.Budget.DailyUncachedBudget > 0 {
		budgetVal = fmt.Sprintf("%s (%.0f%%)", budgetVal, state.Observability.DailyBudgetPct)
	}

	var blocks []living.BlockSpec

	// 1. Always-visible KPI banner across all tabs
	blocks = append(blocks, living.BlockSpec{
		ID:   "console-kpi",
		Type: living.BlockKPIStrip,
		Metrics: []living.KPIMetric{
			{Label: biText("Action Required", "待处理决策"), Value: living.Text(fmt.Sprintf("%d", actionCount)), Tone: actionTone},
			{Label: biText("Active Tasks", "活跃任务"), Value: living.Text(fmt.Sprintf("%d", sel.ActiveCount)), Tone: "accent"},
			{Label: biText("Active Goals", "进行中目标"), Value: living.Text(fmt.Sprintf("%d", len(state.Goals))), Tone: "neutral"},
			{Label: biText("Today Uncached · CHR", "今日消耗 · 缓存命中"), Value: living.Text(fmt.Sprintf("%s · %.0f%%", budgetVal, state.Observability.TodayCacheHitRatio*100.0)), Tone: "neutral"},
		},
	})

	// 2. Needs Me Tab: Pending Approvals, Expired Approvals, Decision Cards
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

	// 3. Needs Me Tab: Active Goals (3-state readiness) & Overdue SLAs
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
			badge := ""
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
				Badge: badge,
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

	// Pending Ideas Feed (Needs Me)
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

	// 4. Work & Spaces Tab: Active Tasks (with clickable conversation:// links!)
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
			if tb, ok := state.Observability.TaskBadges[strings.ToLower(strings.TrimSpace(t.ConversationID))]; ok && tb.GrossTokens > 0 {
				meta = append(meta, obs.FormatTokensCompact(tb.TreeUncachedTokens)+" tok")
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
	}

	// Active Workspaces (Work & Spaces)
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

	// Other Living Spaces Index (Work & Spaces)
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

	// 5. Ops & Health Tab: Schedules (with NextRunHuman!), Quota/Watermark, Reversals & Health
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

	// Recent Reversals (`hq cancel`) + System Health (`hq doctor`)
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
		DefaultPerspective: "needs_me",
		Perspectives: []living.PerspectiveSpec{
			{ID: "needs_me", Label: biText("Needs Me", "待我处理")},
			{ID: "work", Label: biText("Work & Spaces", "任务与空间")},
			{ID: "ops", Label: biText("Ops & Health", "运维与健康")},
		},
		Blocks:    blocks,
		UpdatedAt: now.UTC().Format(time.RFC3339),
	}
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
		if err := store.WriteAtomic(artHTML, htmlBytes, 0o600); err != nil {
			return err
		}
		metaJSON := fmt.Sprintf("{\n  \"artifactType\": \"ARTIFACT_TYPE_OTHER\",\n  \"summary\": \"Live Interactive [HQ] Console (Mobile & Remote Control Ready) — Needs Me, Work & Spaces, and Ops & Health\",\n  \"updatedAt\": %q,\n  \"userFacing\": true\n}\n", now.UTC().Format(time.RFC3339Nano))
		_ = store.WriteAtomic(artMeta, []byte(metaJSON), 0o600)
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
