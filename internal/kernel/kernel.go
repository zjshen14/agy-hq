package kernel

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// QuotaState tracks daily background spawns and circuit-breaker status in local/memory/quota-YYYY-MM-DD.json.
type QuotaState struct {
	Date            string       `json:"date"`
	SpawnsToday     int          `json:"spawnsToday"`
	MaxDailySpawns  int          `json:"maxDailySpawns"`
	CircuitOpen     bool         `json:"circuitOpen"`
	CircuitReason   string       `json:"circuitReason,omitempty"`
	LastHeartbeatAt string       `json:"lastHeartbeatAt,omitempty"`
	LastStatus      string       `json:"lastStatus,omitempty"`
	Events          []SpawnEvent `json:"events,omitempty"`
}

// SpawnEvent records a background task spawn or 0-token gatekeeper event.
type SpawnEvent struct {
	Timestamp string `json:"timestamp"`
	Trigger   string `json:"trigger"`
	Skill     string `json:"skill,omitempty"`
	Model     string `json:"model,omitempty"`
	Outcome   string `json:"outcome"`
}

var (
	reTaxForm   = regexp.MustCompile(`(?i)(w-?2|1099|1098|k-?1|tax)`)
	reSecFiling = regexp.MustCompile(`(?i)(10-[kq]|8-k|earnings)`)
)

// ClassifyInboxFile routes a filename in local/inbox/ to the matching skill name.
// Unmatched files fall back to "inbox-triage" so open-ended drops are always handled.
func ClassifyInboxFile(filename string) string {
	ext := strings.ToLower(filepath.Ext(filename))
	switch ext {
	case ".csv", ".ofx", ".qfx":
		return "ledger-ingest"
	case ".pdf":
		if reTaxForm.MatchString(filename) {
			return "tax-prep"
		}
		if reSecFiling.MatchString(filename) {
			return "thesis-check"
		}
	case ".htm", ".html":
		if reSecFiling.MatchString(filename) {
			return "thesis-check"
		}
	}
	return "inbox-triage"
}

func quotaFilePath(vaultDir string, now time.Time) string {
	return filepath.Join(vaultDir, "memory", fmt.Sprintf("quota-%s.json", now.Format("2006-01-02")))
}

// LoadQuota reads today's QuotaState from local/memory/quota-YYYY-MM-DD.json.
func LoadQuota(vaultDir string) QuotaState {
	now := time.Now()
	p := quotaFilePath(vaultDir, now)
	state := QuotaState{
		Date:           now.Format("2006-01-02"),
		MaxDailySpawns: 15,
	}
	if data, err := os.ReadFile(p); err == nil {
		_ = json.Unmarshal(data, &state)
	}
	if state.MaxDailySpawns <= 0 {
		state.MaxDailySpawns = 15
	}
	return state
}

// SaveQuota persists today's QuotaState.
func SaveQuota(vaultDir string, state QuotaState) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(quotaFilePath(vaultDir, time.Now()), data, 0o644)
}

// SpawnFlashTask invokes `agentapi new-conversation --model=flash` (or simulates it if dryRun is true),
// enforcing the daily spawn budget and tripping the circuit breaker on RESOURCE_EXHAUSTED.
func SpawnFlashTask(vaultDir, trigger, skill, title, prompt string, dryRun bool) (string, error) {
	state := LoadQuota(vaultDir)
	if state.CircuitOpen {
		return "", fmt.Errorf("quota circuit breaker is OPEN: %s", state.CircuitReason)
	}
	if state.SpawnsToday >= state.MaxDailySpawns {
		state.CircuitOpen = true
		state.CircuitReason = fmt.Sprintf("daily spawn budget reached (%d/%d)", state.SpawnsToday, state.MaxDailySpawns)
		_ = SaveQuota(vaultDir, state)
		return "", fmt.Errorf("daily spawn budget exhausted (%d/%d)", state.SpawnsToday, state.MaxDailySpawns)
	}

	var output string
	if dryRun {
		output = fmt.Sprintf("[dry-run] agentapi new-conversation --model=flash --title=%q (skill=%s)", title, skill)
	} else {
		cmd := exec.Command("agentapi", "new-conversation", "--model=flash", "--title="+title, "--", prompt)
		outBytes, err := cmd.CombinedOutput()
		output = strings.TrimSpace(string(outBytes))
		if err != nil {
			if strings.Contains(output, "RESOURCE_EXHAUSTED") || strings.Contains(output, "429") {
				state.CircuitOpen = true
				state.CircuitReason = "RESOURCE_EXHAUSTED (429) detected from agentapi"
			}
			state.Events = append(state.Events, SpawnEvent{
				Timestamp: time.Now().Format(time.RFC3339),
				Trigger:   trigger,
				Skill:     skill,
				Model:     "flash",
				Outcome:   "error: " + err.Error(),
			})
			_ = SaveQuota(vaultDir, state)
			return output, fmt.Errorf("agentapi error: %w (%s)", err, output)
		}
	}

	state.SpawnsToday++
	state.LastStatus = fmt.Sprintf("SPAWNED (%s -> /%s)", trigger, skill)
	state.Events = append(state.Events, SpawnEvent{
		Timestamp: time.Now().Format(time.RFC3339),
		Trigger:   trigger,
		Skill:     skill,
		Model:     "flash",
		Outcome:   output,
	})
	_ = SaveQuota(vaultDir, state)
	return output, nil
}

// ProcessInbox scans local/inbox/ for newly dropped files, moves each to local/inbox/queued/,
// routes it to its matching skill, and spawns (or dry-runs) a Flash task.
func ProcessInbox(vaultDir string, dryRun bool) ([]string, error) {
	inboxDir := filepath.Join(vaultDir, "inbox")
	queuedDir := filepath.Join(inboxDir, "queued")
	entries, err := os.ReadDir(inboxDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var processed []string
	for _, e := range entries {
		if e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if err := os.MkdirAll(queuedDir, 0o755); err != nil {
			return processed, err
		}
		srcPath := filepath.Join(inboxDir, e.Name())
		dstPath := filepath.Join(queuedDir, e.Name())
		if err := os.Rename(srcPath, dstPath); err != nil {
			continue
		}
		skill := ClassifyInboxFile(e.Name())
		title := fmt.Sprintf("Inbox Drop: %s (/%s)", e.Name(), skill)
		prompt := fmt.Sprintf("A new file was dropped into the inbox and moved to %s. Activate the %s skill and process this file.", dstPath, skill)
		out, err := SpawnFlashTask(vaultDir, "inbox-drop:"+e.Name(), skill, title, prompt, dryRun)
		if err != nil {
			processed = append(processed, fmt.Sprintf("%s -> /%s (queued; spawn deferred: %v)", e.Name(), skill, err))
		} else {
			processed = append(processed, fmt.Sprintf("%s -> /%s (%s)", e.Name(), skill, out))
		}
	}
	return processed, nil
}

// RunHeartbeat executes the 30-minute zero-token Go pre-check loop.
// If no inbox files or dead-man's switch alerts are detected, zero LLM tokens are used.
func RunHeartbeat(vaultDir string, dryRun bool) (string, error) {
	now := time.Now()
	inboxActions, err := ProcessInbox(vaultDir, dryRun)
	if err != nil {
		return "", err
	}

	state := LoadQuota(vaultDir)
	state.LastHeartbeatAt = now.Format(time.RFC3339)

	// Dead-man's switch: after 08:00 local time, check if today's morning brief file exists
	briefPath := filepath.Join(vaultDir, "notes", "general", fmt.Sprintf("brief-%s.md", now.Format("2006-01-02")))
	deadManWarning := ""
	if now.Hour() >= 8 {
		if _, err := os.Stat(briefPath); os.IsNotExist(err) {
			deadManWarning = fmt.Sprintf(" [WARN: %s not generated yet]", filepath.Base(briefPath))
		}
	}

	if len(inboxActions) == 0 {
		state.LastStatus = "HEARTBEAT_OK (0 LLM tokens)" + deadManWarning
		_ = SaveQuota(vaultDir, state)
		return state.LastStatus, nil
	}

	summary := fmt.Sprintf("Processed %d inbox item(s): %s%s", len(inboxActions), strings.Join(inboxActions, "; "), deadManWarning)
	state.LastStatus = summary
	_ = SaveQuota(vaultDir, state)
	return summary, nil
}
