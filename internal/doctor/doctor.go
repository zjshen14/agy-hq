package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/ci"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/plugin"
	"github.com/zjshen14/agy-hq/internal/vault"
)

// CheckResult represents a single diagnostic check in `hq doctor`.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // PASS, WARN, FAIL
	Detail string `json:"detail"`
}

// Run executes environment, sandbox, CLI, and Vault health checks.
func Run(vaultDir string) []CheckResult {
	var results []CheckResult

	// 0. Check Go Toolchain & Runtime
	if goPath, gErr := exec.LookPath("go"); gErr == nil && goPath != "" {
		results = append(results, CheckResult{
			Name:   "Go Toolchain",
			Status: "PASS",
			Detail: fmt.Sprintf("%s (%s)", runtime.Version(), goPath),
		})
	} else {
		results = append(results, CheckResult{
			Name:   "Go Toolchain",
			Status: "WARN",
			Detail: fmt.Sprintf("compiled with %s, but 'go' binary not found on PATH (needed for './bin/hq upgrade' and CI)", runtime.Version()),
		})
	}

	// 1. Check Antigravity permission preset in ~/.gemini/config/config.json
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".gemini", "config", "config.json")
	if data, err := os.ReadFile(cfgPath); err == nil {
		var rootCfg map[string]interface{}
		if json.Unmarshal(data, &rootCfg) == nil {
			cfg, _ := rootCfg["userSettings"].(map[string]interface{})
			if cfg == nil {
				cfg = rootCfg
			}
			preset, _ := cfg["permissionPreset"].(string)
			if strings.Contains(strings.ToUpper(preset), "TURBO") {
				results = append(results, CheckResult{
					Name:   "Antigravity Sandbox Preset",
					Status: "WARN",
					Detail: fmt.Sprintf("permissionPreset is %q (Seatbelt sandbox off). Switch to Default or Auto before running untrusted background automations.", preset),
				})
			} else {
				results = append(results, CheckResult{
					Name:   "Antigravity Sandbox Preset",
					Status: "PASS",
					Detail: fmt.Sprintf("permissionPreset is %q", preset),
				})
			}
			rc, _ := cfg["remoteControlEnabled"].(bool)
			if rc {
				results = append(results, CheckResult{
					Name:   "Antigravity Remote Control",
					Status: "PASS",
					Detail: "remoteControlEnabled is true in config.json",
				})
			} else {
				results = append(results, CheckResult{
					Name:   "Antigravity Remote Control",
					Status: "WARN",
					Detail: "remoteControlEnabled is false; enable it with `agy remote-control start`",
				})
			}
		}
	} else {
		results = append(results, CheckResult{
			Name:   "Antigravity Config",
			Status: "WARN",
			Detail: "Could not read ~/.gemini/config/config.json",
		})
	}

	// 2. Check agy and agentapi CLIs
	for _, bin := range []string{"agy", "agentapi"} {
		path, err := exec.LookPath(bin)
		if err != nil {
			for _, candidate := range []string{
				filepath.Join(home, ".local", "bin", bin),
				filepath.Join(home, ".gemini", "antigravity", "bin", bin),
			} {
				if st, sErr := os.Stat(candidate); sErr == nil && !st.IsDir() {
					path = candidate
					err = nil
					break
				}
			}
		}
		if err == nil && path != "" {
			results = append(results, CheckResult{
				Name:   bin + " CLI",
				Status: "PASS",
				Detail: path,
			})
		} else {
			results = append(results, CheckResult{
				Name:   bin + " CLI",
				Status: "WARN",
				Detail: bin + " not found on PATH",
			})
		}
	}

	// 3. Check local/ Vault initialization & git repo isolation
	if _, err := os.Stat(filepath.Join(vaultDir, "USER.md")); err == nil {
		results = append(results, CheckResult{
			Name:   "Private Vault (local/)",
			Status: "PASS",
			Detail: fmt.Sprintf("Initialized at %s", vaultDir),
		})
	} else {
		results = append(results, CheckResult{
			Name:   "Private Vault (local/)",
			Status: "WARN",
			Detail: "Not initialized yet; run `./bin/hq init`",
		})
	}

	// 4. Check Front Desk Intake Lease & Console Artifact
	kernel.ReconcileFrontDeskFromTranscript(vaultDir, "")
	if fd, ok := kernel.GetFrontDesk(vaultDir); ok && fd.ConversationID != "" {
		results = append(results, CheckResult{
			Name:   "Front Desk Intake Lease",
			Status: "PASS",
			Detail: fmt.Sprintf("Claimed by %s", fd.ConversationID),
		})
		artDir := strings.TrimSpace(fd.ArtifactDir)
		if artDir != "" {
			artPath := filepath.Join(artDir, "hq_console.md")
			metaPath := filepath.Join(artDir, "hq_console.md.metadata.json")
			_, errArt := os.Stat(artPath)
			_, errMeta := os.Stat(metaPath)
			if errArt == nil && errMeta == nil {
				results = append(results, CheckResult{
					Name:   "Front Desk Console Artifact",
					Status: "PASS",
					Detail: fmt.Sprintf("Synced at %s (UI pinned=%v)", artPath, fd.ConsolePinned),
				})
			} else {
				results = append(results, CheckResult{
					Name:   "Front Desk Console Artifact",
					Status: "WARN",
					Detail: fmt.Sprintf("Missing %s; run './bin/hq console --sync'", artPath),
				})
			}
		}
	} else if convID := vault.ResolveConversationID(); convID != "" {
		if !kernel.IsSubagentConversation(vaultDir, convID) {
			results = append(results, CheckResult{
				Name:   "Front Desk Intake Lease",
				Status: "WARN",
				Detail: fmt.Sprintf("Vacant while in active conversation %s; run './bin/hq claim-desk --conv=%s'", convID, convID),
			})
		} else {
			results = append(results, CheckResult{
				Name:   "Front Desk Intake Lease",
				Status: "PASS",
				Detail: "Vacant (subagent active; will auto-claim on Turn 1 of next Front Desk conversation)",
			})
		}
	} else {
		results = append(results, CheckResult{
			Name:   "Front Desk Intake Lease",
			Status: "PASS",
			Detail: "Vacant (will auto-claim on Turn 1 of next Antigravity conversation)",
		})
	}

	// 5. Check Scheduled Triggers & Daemon Heartbeat
	dInfo := kernel.GetDaemonInfo(vaultDir)
	armed, unarmed := kernel.GetUnarmedTriggers(vaultDir)
	totalTriggers := len(armed) + len(unarmed)
	if dInfo.Running {
		results = append(results, CheckResult{
			Name:   "Scheduled Triggers & Heartbeat",
			Status: "PASS",
			Detail: fmt.Sprintf("Running (daemon PID %d, %d schedules declared, %d/%d armed in Front Desk)", dInfo.PID, dInfo.SchedulesCount, len(armed), totalTriggers),
		})
	} else {
		results = append(results, CheckResult{
			Name:   "Scheduled Triggers & Heartbeat",
			Status: "WARN",
			Detail: fmt.Sprintf("%d schedules declared in triggers.yaml, but background daemon is inactive; run './bin/hq daemon install' or './bin/hq daemon' (%d/%d armed in Front Desk)", dInfo.SchedulesCount, len(armed), totalTriggers),
		})
	}

	// 6. Check Task Board & Quota Integrity
	tasks, _ := kernel.ListTasks(vaultDir)
	corruptedTitles := 0
	for _, t := range tasks {
		if strings.Contains(t.Title, "\n") || strings.Contains(t.Title, "|") {
			corruptedTitles++
		}
	}
	quota := kernel.LoadQuota(vaultDir)
	if corruptedTitles > 0 {
		results = append(results, CheckResult{
			Name:   "Task Board & Quota Integrity",
			Status: "WARN",
			Detail: fmt.Sprintf("%d task(s) have unsanitized multi-line titles; run './bin/hq migrate'", corruptedTitles),
		})
	} else if quota.CircuitOpen {
		results = append(results, CheckResult{
			Name:   "Task Board & Quota Integrity",
			Status: "WARN",
			Detail: fmt.Sprintf("%d tasks tracked; quota circuit open (%s)", len(tasks), quota.CircuitReason),
		})
	} else {
		results = append(results, CheckResult{
			Name:   "Task Board & Quota Integrity",
			Status: "PASS",
			Detail: fmt.Sprintf("%d tasks tracked, daily spawns %d/%d", len(tasks), quota.SpawnsToday, quota.MaxDailySpawns),
		})
	}

	// 7. Check Memory Roadmap Index Budget & Knowledge Wiki
	if rep, err := vault.AuditMemory(vaultDir, nil); err == nil && rep.MemoryLines > 0 {
		if rep.OverBudget {
			results = append(results, CheckResult{
				Name:   "Memory Roadmap Budget",
				Status: "WARN",
				Detail: fmt.Sprintf("MEMORY.md is %d/%d lines (~%d tokens); distill details into local/memory/topics/", rep.MemoryLines, rep.MemoryMaxLines, rep.EstimatedTokens),
			})
		} else {
			results = append(results, CheckResult{
				Name:   "Memory Roadmap Budget",
				Status: "PASS",
				Detail: fmt.Sprintf("MEMORY.md %d/%d lines (~%d tokens), %d topic page(s), %d active fact(s)", rep.MemoryLines, rep.MemoryMaxLines, rep.EstimatedTokens, len(rep.Topics), rep.ActiveFactCount),
			})
		}
	}

	// 8. Check Google Chat Integration (if configured)
	if gchatReg, err := gchat.LoadRegistry(vaultDir); err == nil && len(gchatReg.Accounts) > 0 {
		defCfg, ok := gchatReg.Accounts[gchatReg.DefaultAccount]
		if !ok {
			for _, c := range gchatReg.Accounts {
				defCfg = c
				break
			}
		}
		detail := fmt.Sprintf("%d account(s) configured (default=%s [%s], transport=%s)", len(gchatReg.Accounts), defCfg.Alias, defCfg.Email, defCfg.Transport)
		if defCfg.Transport == "cdp" {
			if cdpOK, cdpDetail := gchat.CheckCDPEndpoint(defCfg.CDPEndpoint); cdpOK {
				detail += " · " + cdpDetail
			} else {
				detail += fmt.Sprintf(" · CDP standby (fallback=gmail-bridge:%s)", defCfg.GmailFallbackAlias)
			}
		}
		results = append(results, CheckResult{
			Name:   "Google Chat Integration",
			Status: "PASS",
			Detail: detail,
		})
	}

	// 9. Check Local CI & Git Hook Guards (.git/hooks/pre-commit & pre-push) when inside agy-hq repo
	rootDir, rootErr := vault.ResolveRoot()
	if rootErr == nil && vault.IsAgyHQRepo(rootDir) {
		if _, sErr := os.Stat(filepath.Join(rootDir, ".git")); sErr == nil {
			if hooksOK, hooksDetail := ci.CheckGitHooksInstalled(rootDir); hooksOK {
				results = append(results, CheckResult{
					Name:   "CI & Git Hook Guards",
					Status: "PASS",
					Detail: hooksDetail,
				})
			} else {
				results = append(results, CheckResult{
					Name:   "CI & Git Hook Guards",
					Status: "WARN",
					Detail: hooksDetail,
				})
			}
		}
	}

	// 10. Check Token Budget & Front Desk Context Health (RFC-0012)
	obsSum := obs.BuildDashboardSummary(vaultDir, time.Time{})
	if obsSum.FrontDeskContextStatus == "CRITICAL" || obsSum.DailyBudgetPct >= 100 || obsSum.DeepBudgetPct >= 100 {
		results = append(results, CheckResult{
			Name:   "Token Budget & Context Health",
			Status: "WARN",
			Detail: fmt.Sprintf("Today uncached %s / %s (%.1f%%), Front Desk context %s / 256k (%s); run 'hq obs anomalies' or '/compact'",
				obs.FormatTokensCompact(obsSum.TodayUncachedTokens),
				obs.FormatTokensCompact(obsSum.Budget.DailyUncachedBudget),
				obsSum.DailyBudgetPct,
				obs.FormatTokensCompact(obsSum.FrontDeskContextTokens),
				obsSum.FrontDeskContextStatus,
			),
		})
	} else if obsSum.FrontDeskContextStatus == "WARN" || obsSum.DailyBudgetPct >= 80 || obsSum.DeepBudgetPct >= 80 {
		results = append(results, CheckResult{
			Name:   "Token Budget & Context Health",
			Status: "WARN",
			Detail: fmt.Sprintf("Today uncached %s / %s (%.1f%%), CHR %.1f%%, Front Desk context %s / 256k (%s)",
				obs.FormatTokensCompact(obsSum.TodayUncachedTokens),
				obs.FormatTokensCompact(obsSum.Budget.DailyUncachedBudget),
				obsSum.DailyBudgetPct,
				obsSum.TodayCacheHitRatio*100.0,
				obs.FormatTokensCompact(obsSum.FrontDeskContextTokens),
				obsSum.FrontDeskContextStatus,
			),
		})
	} else {
		results = append(results, CheckResult{
			Name:   "Token Budget & Context Health",
			Status: "PASS",
			Detail: fmt.Sprintf("Today uncached %s / %s (%.1f%%), CHR %.1f%%, Front Desk context %s / 256k (%s)",
				obs.FormatTokensCompact(obsSum.TodayUncachedTokens),
				obs.FormatTokensCompact(obsSum.Budget.DailyUncachedBudget),
				obsSum.DailyBudgetPct,
				obsSum.TodayCacheHitRatio*100.0,
				obs.FormatTokensCompact(obsSum.FrontDeskContextTokens),
				obsSum.FrontDeskContextStatus,
			),
		})
	}

	// 11. Check Plugin & Binary Sync (RFC-0015 — project-scoped by default, --global opt-in)
	if pSt, pErr := plugin.Status(vaultDir, rootDir); pErr == nil && pSt != nil {
		switch {
		case pSt.DaemonSkewed:
			results = append(results, CheckResult{
				Name:   "Plugin & Binary Sync",
				Status: "WARN",
				Detail: fmt.Sprintf("Live daemon version (%s / %s) is skewed vs hq binary (v%s / %s); run 'hq plugin install' or 'hq upgrade'",
					pSt.DaemonVersion, pSt.DaemonCommit, pSt.EngineVersion, pSt.EngineCommit),
			})
		case pSt.LegacySidecarFound:
			results = append(results, CheckResult{
				Name:   "Plugin & Binary Sync",
				Status: "WARN",
				Detail: fmt.Sprintf("Legacy standalone sidecar copy detected at %s; run 'hq plugin install' to tidy up",
					pSt.LegacySidecarDir),
			})
		case pSt.GlobalPluginInstalled && pSt.WorkspacePluginFound && (!pSt.BundleInSync || !pSt.VersionInSync):
			results = append(results, CheckResult{
				Name:   "Plugin & Binary Sync",
				Status: "WARN",
				Detail: fmt.Sprintf("Global plugin (%s, v%s) has drifted from workspace source (v%s); run 'hq plugin install --global' or 'hq plugin install' to scope to project",
					pSt.GlobalPluginDir, pSt.GlobalPluginVersion, pSt.EngineVersion),
			})
		case pSt.SharedPluginInstalled && pSt.WorkspacePluginFound && (!pSt.BundleInSync || !pSt.VersionInSync):
			results = append(results, CheckResult{
				Name:   "Plugin & Binary Sync",
				Status: "WARN",
				Detail: fmt.Sprintf("Shared plugin bundle (%s, v%s) has drifted from workspace source (v%s); run 'hq plugin install'",
					pSt.SharedPluginDir, pSt.SharedPluginVersion, pSt.EngineVersion),
			})
		case (pSt.GlobalPluginInstalled || pSt.SharedPluginInstalled) && !pSt.GlobalBinaryInstalled:
			results = append(results, CheckResult{
				Name:   "Plugin & Binary Sync",
				Status: "WARN",
				Detail: fmt.Sprintf("Plugin bundle staged, but %s is missing; run 'hq plugin install'",
					pSt.GlobalBinaryPath),
			})
		case (pSt.GlobalPluginInstalled || pSt.SharedPluginInstalled) && !pSt.BinDirOnPath:
			results = append(results, CheckResult{
				Name:   "Plugin & Binary Sync",
				Status: "WARN",
				Detail: fmt.Sprintf("hq v%s installed (%s), but %s is not in PATH (hooks auto-add ~/.local/bin, add it to shell rc for interactive CLI)",
					pSt.EngineVersion, pSt.Scope, filepath.Dir(pSt.GlobalBinaryPath)),
			})
		case pSt.GlobalPluginInstalled:
			results = append(results, CheckResult{
				Name:   "Plugin & Binary Sync",
				Status: "PASS",
				Detail: fmt.Sprintf("Global scope active at %s (v%s · vaultSource=%s; run 'hq plugin install' without --global to scope strictly to project)",
					pSt.GlobalPluginDir, pSt.GlobalPluginVersion, pSt.VaultResolutionSource),
			})
		case pSt.SharedPluginInstalled:
			results = append(results, CheckResult{
				Name:   "Plugin & Binary Sync",
				Status: "PASS",
				Detail: fmt.Sprintf("Project-scoped mode active (v%s · binary=%s · vaultSource=%s; zero global leakage to unrelated projects)",
					pSt.SharedPluginVersion, pSt.GlobalBinaryPath, pSt.VaultResolutionSource),
			})
		default:
			results = append(results, CheckResult{
				Name:   "Plugin & Binary Sync",
				Status: "PASS",
				Detail: fmt.Sprintf("Workspace mode (v%s · vaultSource=%s; run 'hq plugin install' to install ~/.local/bin/hq in project scope)",
					pSt.EngineVersion, pSt.VaultResolutionSource),
			})
		}
	}

	// 12. Check Sidebar Archive Sync Queue (RFC-0016)
	sq := kernel.InspectSidebarSyncQueue(vaultDir)
	if sq.HighAttemptCount > 0 || sq.PendingCount >= 10 {
		results = append(results, CheckResult{
			Name:   "Sidebar Archive Sync Queue",
			Status: "WARN",
			Detail: fmt.Sprintf("%d pending (%d with >=5 retries, oldest=%s); run 'hq tasks sync-sidebar' inside Antigravity",
				sq.PendingCount, sq.HighAttemptCount, sq.OldestEnqueuedAt),
		})
	} else if sq.PendingCount > 0 {
		results = append(results, CheckResult{
			Name:   "Sidebar Archive Sync Queue",
			Status: "PASS",
			Detail: fmt.Sprintf("%d queued transition(s) awaiting next Antigravity LanguageServer sync", sq.PendingCount),
		})
	} else {
		results = append(results, CheckResult{
			Name:   "Sidebar Archive Sync Queue",
			Status: "PASS",
			Detail: "0 pending retries (in sync)",
		})
	}

	return results
}
