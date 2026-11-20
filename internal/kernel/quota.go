package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/store"
)

// QuotaState tracks daily background spawns and circuit-breaker status in local/memory/quota-YYYY-MM-DD.json.
// SpawnsToday records total Layer-1 spawns (AutoSpawnsToday + InteractiveSpawnsToday), while MaxDailySpawns
// governs automated/daemon background spawns (`AutoSpawnsToday`) so interactive user delegations never starve
// or get blocked by background watcher caps.
type QuotaState struct {
	Date                   string       `json:"date"`
	SpawnsToday            int          `json:"spawnsToday"`
	AutoSpawnsToday        int          `json:"autoSpawnsToday,omitempty"`
	InteractiveSpawnsToday int          `json:"interactiveSpawnsToday,omitempty"`
	GatekeeperSavesToday   int          `json:"gatekeeperSavesToday,omitempty"`
	MaxDailySpawns         int          `json:"maxDailySpawns"`
	CircuitOpen            bool         `json:"circuitOpen"`
	CircuitReason          string       `json:"circuitReason,omitempty"`
	LastHeartbeatAt        string       `json:"lastHeartbeatAt,omitempty"`
	LastStatus             string       `json:"lastStatus,omitempty"`
	Events                 []SpawnEvent `json:"events,omitempty"`
}

// EffectiveAutoSpawns returns today's automated background spawn count used for MaxDailySpawns enforcement,
// preserving backward compatibility for legacy persisted quota files without split counters.
func (q QuotaState) EffectiveAutoSpawns() int {
	if q.AutoSpawnsToday > 0 || q.InteractiveSpawnsToday > 0 {
		return q.AutoSpawnsToday
	}
	if len(q.Events) > 0 {
		auto := 0
		for _, ev := range q.Events {
			if strings.HasPrefix(strings.TrimSpace(ev.Outcome), "error:") {
				continue
			}
			if !isInteractiveSpawnTrigger(ev.Trigger) {
				auto++
			}
		}
		return auto
	}
	return q.SpawnsToday
}

// SpawnEvent records a background task spawn or 0-token gatekeeper event.
type SpawnEvent struct {
	Timestamp string `json:"timestamp"`
	Trigger   string `json:"trigger"`
	Skill     string `json:"skill,omitempty"`
	Model     string `json:"model,omitempty"`
	Outcome   string `json:"outcome"`
}

func quotaFilePath(vaultDir string, now time.Time) string {
	return filepath.Join(vaultDir, "memory", fmt.Sprintf("quota-%s.json", now.Format("2006-01-02")))
}

// VaultQuotaConfig holds parsed settings from the top-level `quota:` section of <vault>/config.yaml.
type VaultQuotaConfig struct {
	MaxParallelRuns int
	MaxDailySpawns  int
	BackgroundModel string
}

func stripInlineYAMLComment(val string) string {
	inSingle, inDouble := false, false
	for i := 0; i < len(val); i++ {
		switch val[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble && (i == 0 || val[i-1] == ' ' || val[i-1] == '\t') {
				return strings.TrimSpace(val[:i])
			}
		}
	}
	return strings.TrimSpace(val)
}

// LoadVaultQuotaConfig reads top-level `quota:` settings (`max_daily_spawns`, `max_parallel_runs`, `background_model`)
// from <vault>/config.yaml if present.
func LoadVaultQuotaConfig(vaultDir string) VaultQuotaConfig {
	var cfg VaultQuotaConfig
	if strings.TrimSpace(vaultDir) == "" {
		return cfg
	}
	raw, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml"))
	if err != nil {
		return cfg
	}
	inQuota := false
	for _, line := range strings.Split(string(raw), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		if indent == 0 {
			inQuota = trimmed == "quota:" || strings.HasPrefix(trimmed, "quota:")
			continue
		}
		if !inQuota {
			continue
		}
		k, v, ok := strings.Cut(trimmed, ":")
		if !ok {
			continue
		}
		key := strings.TrimSpace(k)
		val := strings.Trim(stripInlineYAMLComment(v), `"'`)
		switch key {
		case "max_daily_spawns":
			if n, pErr := strconv.Atoi(val); pErr == nil && n > 0 {
				cfg.MaxDailySpawns = n
			}
		case "max_parallel_runs":
			if n, pErr := strconv.Atoi(val); pErr == nil && n > 0 {
				cfg.MaxParallelRuns = n
			}
		case "background_model":
			cfg.BackgroundModel = val
		}
	}
	return cfg
}

func isInteractiveSpawnTrigger(trigger string) bool {
	t := strings.TrimSpace(trigger)
	return t == "invoke_subagent" ||
		strings.HasPrefix(t, "hq-subagent:") ||
		strings.HasPrefix(t, "manual-spawn:") ||
		strings.HasPrefix(t, "idea-promote:")
}

func isDailySpawnCapReason(reason string) bool {
	r := strings.ToLower(strings.TrimSpace(reason))
	return strings.HasPrefix(r, "daily spawn budget reached") || strings.HasPrefix(r, "daily spawn budget exhausted")
}

// SetVaultMaxDailySpawns updates `quota.max_daily_spawns` in <vault>/config.yaml (when present) and persists
// the new cap in QuotaState, automatically closing any spawn-count circuit breaker when below the new cap.
func SetVaultMaxDailySpawns(vaultDir string, maxDailySpawns int) (QuotaState, error) {
	if maxDailySpawns <= 0 {
		return QuotaState{}, fmt.Errorf("maxDailySpawns must be > 0 (got %d)", maxDailySpawns)
	}
	cfgPath := filepath.Join(vaultDir, "config.yaml")
	if raw, err := os.ReadFile(cfgPath); err == nil {
		lines := strings.Split(string(raw), "\n")
		inQuota := false
		quotaHeaderIdx := -1
		replaced := false
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			indent := len(line) - len(strings.TrimLeft(line, " \t"))
			if indent == 0 {
				if trimmed == "quota:" || strings.HasPrefix(trimmed, "quota:") {
					inQuota = true
					quotaHeaderIdx = i
				} else {
					inQuota = false
				}
				continue
			}
			if inQuota {
				if k, _, ok := strings.Cut(trimmed, ":"); ok && strings.TrimSpace(k) == "max_daily_spawns" {
					prefix := line[:indent]
					lines[i] = fmt.Sprintf("%smax_daily_spawns: %d", prefix, maxDailySpawns)
					replaced = true
					break
				}
			}
		}
		var updatedYAML string
		if replaced {
			updatedYAML = strings.Join(lines, "\n")
		} else if quotaHeaderIdx >= 0 {
			out := make([]string, 0, len(lines)+1)
			out = append(out, lines[:quotaHeaderIdx+1]...)
			out = append(out, fmt.Sprintf("  max_daily_spawns: %d", maxDailySpawns))
			out = append(out, lines[quotaHeaderIdx+1:]...)
			updatedYAML = strings.Join(out, "\n")
		} else {
			base := strings.TrimRight(string(raw), "\n")
			updatedYAML = fmt.Sprintf("%s\n\nquota:\n  max_daily_spawns: %d\n", base, maxDailySpawns)
		}
		mode := os.FileMode(0o600)
		if st, sErr := os.Stat(cfgPath); sErr == nil {
			mode = st.Mode().Perm()
		}
		if wErr := store.WriteAtomic(cfgPath, []byte(updatedYAML), mode); wErr != nil {
			return QuotaState{}, wErr
		}
	}

	state := LoadQuota(vaultDir)
	state.MaxDailySpawns = maxDailySpawns
	if state.CircuitOpen && isDailySpawnCapReason(state.CircuitReason) && state.EffectiveAutoSpawns() < state.MaxDailySpawns {
		state.CircuitOpen = false
		state.CircuitReason = ""
	}
	if err := SaveQuota(vaultDir, state); err != nil {
		return state, err
	}
	return state, nil
}

// LoadQuota reads today's QuotaState from local/memory/quota-YYYY-MM-DD.json or quota_state.json,
// honoring `quota.max_daily_spawns` from <vault>/config.yaml when configured.
func LoadQuota(vaultDir string) QuotaState {
	now := time.Now()
	today := now.Format("2006-01-02")
	cfgQuota := LoadVaultQuotaConfig(vaultDir)
	defaultCap := 15
	if cfgQuota.MaxDailySpawns > 0 {
		defaultCap = cfgQuota.MaxDailySpawns
	}
	p := quotaFilePath(vaultDir, now)
	state := QuotaState{
		Date:           today,
		MaxDailySpawns: defaultCap,
	}
	data, err := os.ReadFile(p)
	if err != nil {
		fallbackPath := filepath.Join(vaultDir, "memory", "quota_state.json")
		if fbData, fbErr := os.ReadFile(fallbackPath); fbErr == nil {
			var fbState QuotaState
			if unErr := json.Unmarshal(fbData, &fbState); unErr == nil {
				if fbState.Date == today {
					state = fbState
				} else if fbState.MaxDailySpawns > 0 {
					state.MaxDailySpawns = fbState.MaxDailySpawns
				}
			}
		}
	} else {
		_ = json.Unmarshal(data, &state)
	}
	if cfgQuota.MaxDailySpawns > 0 {
		state.MaxDailySpawns = cfgQuota.MaxDailySpawns
	} else if state.MaxDailySpawns <= 0 {
		state.MaxDailySpawns = 15
	}
	if state.Date != today {
		state.Date = today
		state.SpawnsToday = 0
		state.AutoSpawnsToday = 0
		state.InteractiveSpawnsToday = 0
		state.GatekeeperSavesToday = 0
		state.CircuitOpen = false
		state.CircuitReason = ""
		state.Events = nil
	} else if state.SpawnsToday > 0 && state.AutoSpawnsToday == 0 && state.InteractiveSpawnsToday == 0 && len(state.Events) > 0 {
		auto, inter := 0, 0
		for _, ev := range state.Events {
			if strings.HasPrefix(strings.TrimSpace(ev.Outcome), "error:") {
				continue
			}
			if isInteractiveSpawnTrigger(ev.Trigger) {
				inter++
			} else {
				auto++
			}
		}
		if auto+inter > 0 {
			state.AutoSpawnsToday = auto
			state.InteractiveSpawnsToday = inter
		}
	}
	if state.CircuitOpen && isDailySpawnCapReason(state.CircuitReason) && state.EffectiveAutoSpawns() < state.MaxDailySpawns {
		state.CircuitOpen = false
		state.CircuitReason = ""
	}
	return state
}

func quotaLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".quota.lock")
}

func (s *QuotaState) normalizeSpawnCounters() {
	if s.SpawnsToday > 0 && s.AutoSpawnsToday == 0 && s.InteractiveSpawnsToday == 0 {
		s.AutoSpawnsToday = s.EffectiveAutoSpawns()
	}
}

func saveQuotaUnlocked(vaultDir string, state QuotaState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	datedPath := quotaFilePath(vaultDir, time.Now())
	if err := store.WriteAtomic(datedPath, data, 0o644); err != nil {
		return err
	}
	return store.WriteAtomic(filepath.Join(vaultDir, "memory", "quota_state.json"), data, 0o644)
}

// SaveQuota persists today's QuotaState atomically to quota-YYYY-MM-DD.json and quota_state.json under RankQuota.
func SaveQuota(vaultDir string, state QuotaState) error {
	return store.With(context.Background(), store.RankQuota, quotaLockPath(vaultDir), func(_ context.Context) error {
		return saveQuotaUnlocked(vaultDir, state)
	})
}

// UpdateQuota executes an atomic locked read-modify-write update on today's QuotaState under RankQuota.
func UpdateQuota(vaultDir string, fn func(state *QuotaState) (bool, error)) error {
	return store.With(context.Background(), store.RankQuota, quotaLockPath(vaultDir), func(_ context.Context) error {
		st := LoadQuota(vaultDir)
		changed, err := fn(&st)
		if err != nil || !changed {
			return err
		}
		return saveQuotaUnlocked(vaultDir, st)
	})
}

// RecordSubagentSpawn records an interactive Layer-1 subagent invocation (`InteractiveSpawnsToday` + `SpawnsToday`)
// without consuming the daemon's automated background spawn budget (`AutoSpawnsToday`).
func RecordSubagentSpawn(vaultDir, role, model, prompt string) error {
	summary := truncateRunes(prompt, 200)
	now := time.Now()
	return UpdateQuota(vaultDir, func(state *QuotaState) (bool, error) {
		if n := len(state.Events); n > 0 {
			last := state.Events[n-1]
			if last.Trigger == "invoke_subagent" && last.Skill == role && last.Model == model && last.Outcome == summary {
				if ts, err := time.Parse(time.RFC3339, last.Timestamp); err == nil && now.Sub(ts) >= 0 && now.Sub(ts) < 2*time.Second {
					return false, nil
				}
			}
		}
		state.normalizeSpawnCounters()
		state.InteractiveSpawnsToday++
		state.SpawnsToday++
		state.LastStatus = fmt.Sprintf("Subagent spawned: %s (%s)", role, now.Format("15:04:05"))
		state.Events = append(state.Events, SpawnEvent{
			Timestamp: now.Format(time.RFC3339),
			Trigger:   "invoke_subagent",
			Skill:     role,
			Model:     model,
			Outcome:   summary,
		})
		return true, nil
	})
}
