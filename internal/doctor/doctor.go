package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/zjshen14/agy-hq/internal/ci"
	"github.com/zjshen14/agy-hq/internal/gchat"
	"github.com/zjshen14/agy-hq/internal/kernel"
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
		results = append(results, CheckResult{
			Name:   "Front Desk Intake Lease",
			Status: "WARN",
			Detail: fmt.Sprintf("Vacant while in active conversation %s; run './bin/hq claim-desk --conv=%s'", convID, convID),
		})
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

	// 9. Check Local CI & Git Hook Guards (.git/hooks/pre-commit & pre-push)
	if rootDir, err := vault.ResolveRoot(); err == nil {
		if _, sErr := os.Stat(filepath.Join(rootDir, ".git", "hooks")); sErr == nil {
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

	return results
}
