package kernel

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// TriggerCursor tracks deterministic sidecar pre-check execution & SHA-256 deduplication
// in local/memory/trigger_cursors.json so unchanged ticks consume 0 LLM tokens.
type TriggerCursor struct {
	ID               string `json:"id"`
	LastCheckedAt    string `json:"lastCheckedAt,omitempty"`
	LastCronSlot     string `json:"lastCronSlot,omitempty"`
	LastTriggeredAt  string `json:"lastTriggeredAt,omitempty"`
	LastStdoutSHA256 string `json:"lastStdoutSha256,omitempty"`
	SkipsCount       int    `json:"skipsCount"`
}

func triggerCursorsPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "trigger_cursors.json")
}

// LoadTriggerCursors reads local/memory/trigger_cursors.json.
func LoadTriggerCursors(vaultDir string) map[string]TriggerCursor {
	out := make(map[string]TriggerCursor)
	data, err := os.ReadFile(triggerCursorsPath(vaultDir))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(data, &out)
	return out
}

// SaveTriggerCursors writes local/memory/trigger_cursors.json.
func SaveTriggerCursors(vaultDir string, cursors map[string]TriggerCursor) error {
	p := triggerCursorsPath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(cursors, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

func matchCronField(expr string, val, minVal, maxVal int, allowSevenAsZero bool) bool {
	expr = strings.TrimSpace(expr)
	if expr == "" || expr == "*" {
		return true
	}
	for _, part := range strings.Split(expr, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		step := 1
		if slashIdx := strings.Index(part, "/"); slashIdx >= 0 {
			s, err := strconv.Atoi(strings.TrimSpace(part[slashIdx+1:]))
			if err != nil || s <= 0 {
				continue
			}
			step = s
			part = strings.TrimSpace(part[:slashIdx])
		}
		if part == "*" {
			if (val-minVal)%step == 0 {
				return true
			}
			continue
		}
		if dashIdx := strings.Index(part, "-"); dashIdx >= 0 {
			lo, err1 := strconv.Atoi(strings.TrimSpace(part[:dashIdx]))
			hi, err2 := strconv.Atoi(strings.TrimSpace(part[dashIdx+1:]))
			if err1 != nil || err2 != nil {
				continue
			}
			if allowSevenAsZero && hi == 7 && val == 0 {
				return true
			}
			if val >= lo && val <= hi && (val-lo)%step == 0 {
				return true
			}
			continue
		}
		target, err := strconv.Atoi(part)
		if err != nil {
			continue
		}
		if allowSevenAsZero && target == 7 && val == 0 {
			return true
		}
		if val == target {
			return true
		}
	}
	return false
}

// MatchesCron evaluates whether reference time `now` matches a 5-field cron expression
// (with optional leading CRON_TZ=<IANA> prefix) and returns the minute-granularity slot key.
func MatchesCron(cronExpr string, now time.Time) (bool, string) {
	fields := strings.Fields(strings.TrimSpace(cronExpr))
	loc := now.Location()
	if len(fields) > 0 && strings.HasPrefix(strings.ToUpper(fields[0]), "CRON_TZ=") {
		tzName := strings.TrimSpace(fields[0][len("CRON_TZ="):])
		if l, err := time.LoadLocation(tzName); err == nil {
			loc = l
		}
		fields = fields[1:]
	}
	if len(fields) != 5 {
		return false, ""
	}
	t := now.In(loc)
	if !matchCronField(fields[0], t.Minute(), 0, 59, false) {
		return false, ""
	}
	if !matchCronField(fields[1], t.Hour(), 0, 23, false) {
		return false, ""
	}
	if !matchCronField(fields[2], t.Day(), 1, 31, false) {
		return false, ""
	}
	if !matchCronField(fields[3], int(t.Month()), 1, 12, false) {
		return false, ""
	}
	if !matchCronField(fields[4], int(t.Weekday()), 0, 6, true) {
		return false, ""
	}
	return true, t.Format("2006-01-02T15:04-07:00")
}

// EvaluateDaemonGatekeepers executes 0-token Go sidecar pre-checks for `daemon_gatekeeper`
// and `isolated_worker` schedules declared in local/triggers.yaml.
// If forceCheck is true, cron minute matching is bypassed so callers/tests can evaluate pre-checks immediately.
func EvaluateDaemonGatekeepers(vaultDir string, now time.Time, forceCheck bool, dryRun bool) (actions []string, zeroTokenSaves int, err error) {
	if now.IsZero() {
		now = time.Now()
	}
	cfg, err := LoadTriggers(vaultDir)
	if err != nil || cfg == nil || len(cfg.Schedules) == 0 {
		return nil, 0, nil
	}

	cursors := LoadTriggerCursors(vaultDir)
	dirtyCursors := false
	rootDir := filepath.Dir(filepath.Clean(vaultDir))
	if envRoot := strings.TrimSpace(os.Getenv("HQ_ROOT_DIR")); envRoot != "" {
		if abs, aErr := filepath.Abs(envRoot); aErr == nil {
			rootDir = abs
		}
	}

	for _, s := range cfg.Schedules {
		if forceCheck {
			if len(s.PrecheckCmd) == 0 {
				continue
			}
		} else {
			mode := s.EffectiveMode()
			if mode != "daemon_gatekeeper" && mode != "isolated_worker" {
				continue
			}
		}

		cur := cursors[s.ID]
		slotKey := now.UTC().Format("2006-01-02T15:04Z")
		if !forceCheck {
			matched, sk := MatchesCron(s.Cron, now)
			if !matched || (sk != "" && cur.LastCronSlot == sk) {
				continue
			}
			slotKey = sk
		}

		cur.ID = s.ID
		cur.LastCheckedAt = now.UTC().Format(time.RFC3339)
		cur.LastCronSlot = slotKey
		dirtyCursors = true

		shouldSpawn := false
		precheckDigest := ""

		if len(s.PrecheckCmd) > 0 {
			bin0 := s.PrecheckCmd[0]
			if bin0 == "hq" || bin0 == "./bin/hq" || bin0 == "bin/hq" {
				cand := filepath.Join(rootDir, bin0)
				if bin0 != "hq" {
					if st, sErr := os.Stat(cand); sErr == nil && !st.IsDir() {
						bin0 = cand
					} else if exe, eErr := os.Executable(); eErr == nil && exe != "" && strings.HasPrefix(filepath.Base(exe), "hq") {
						bin0 = exe
					} else if lp, lErr := exec.LookPath("hq"); lErr == nil && lp != "" {
						bin0 = lp
					}
				} else if exe, eErr := os.Executable(); eErr == nil && exe != "" && strings.HasPrefix(filepath.Base(exe), "hq") {
					bin0 = exe
				} else if lp, lErr := exec.LookPath("hq"); lErr == nil && lp != "" {
					bin0 = lp
				} else if st, sErr := os.Stat(filepath.Join(rootDir, "bin", "hq")); sErr == nil && !st.IsDir() {
					bin0 = filepath.Join(rootDir, "bin", "hq")
				}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			cmd := exec.CommandContext(ctx, bin0, s.PrecheckCmd[1:]...)
			cmd.Dir = rootDir
			cmd.Env = append(sanitizedGitCmd().Env, "HQ_ROOT_DIR="+rootDir, "HQ_VAULT_DIR="+vaultDir)
			outBytes, cmdErr := cmd.CombinedOutput()
			cancel()

			trimmedOut := strings.TrimSpace(string(outBytes))
			sum := sha256.Sum256([]byte(trimmedOut))
			shaHex := hex.EncodeToString(sum[:])

			dedup := s.EffectiveDedupMode()
			switch dedup {
			case "none":
				shouldSpawn = cmdErr == nil
			case "exit_zero_nonempty":
				shouldSpawn = cmdErr == nil && trimmedOut != ""
			default: // "stdout_sha256"
				if cmdErr == nil && trimmedOut != "" && shaHex != cur.LastStdoutSHA256 {
					shouldSpawn = true
					cur.LastStdoutSHA256 = shaHex
				}
			}

			if !shouldSpawn {
				cur.SkipsCount++
				cursors[s.ID] = cur
				zeroTokenSaves++
				continue
			}
			if runes := []rune(trimmedOut); len(runes) > 300 {
				precheckDigest = string(runes[:300]) + "..."
			} else {
				precheckDigest = trimmedOut
			}
		} else {
			// isolated_worker without a precheck command (e.g., nightly sleep-consolidate)
			shouldSpawn = true
		}

		if shouldSpawn {
			cur.LastTriggeredAt = now.UTC().Format(time.RFC3339)
			cursors[s.ID] = cur

			modelID, _, _ := ResolveModel(s.DefaultPrompt(), s.Model, "", "")
			prompt := s.DefaultPrompt()
			if precheckDigest != "" {
				prompt += fmt.Sprintf("\n\nGatekeeper Delta Output:\n%s", precheckDigest)
			}
			if deliv := s.EffectiveDelivery(); deliv != "" {
				prompt += fmt.Sprintf("\n\nDelivery Policy: %s (route non-critical findings via `hq outbox add --tier=%s` rather than interrupting unnecessarily).", deliv, deliv)
			}
			title := fmt.Sprintf("[Auto] %s (/%s)", s.ID, s.Skill)
			out, spawnErr := SpawnTask(vaultDir, "gatekeeper:"+s.ID, s.Skill, title, prompt, modelID, dryRun)
			if spawnErr != nil {
				actions = append(actions, fmt.Sprintf("gatekeeper:%s -> /%s (deferred: %v)", s.ID, s.Skill, spawnErr))
			} else {
				actions = append(actions, fmt.Sprintf("gatekeeper:%s -> /%s (%s)", s.ID, s.Skill, out))
			}
		}
	}

	if dirtyCursors {
		_ = SaveTriggerCursors(vaultDir, cursors)
	}
	if zeroTokenSaves > 0 {
		st := LoadQuota(vaultDir)
		st.GatekeeperSavesToday += zeroTokenSaves
		_ = SaveQuota(vaultDir, st)
	}
	return actions, zeroTokenSaves, nil
}

// CoalescedTriageEntry buffers non-urgent Gmail/GChat batch items inside a coalescing window
// so bursty traffic triggers 1 consolidated Flash spawn instead of N separate turns.
type CoalescedTriageEntry struct {
	Channel       string   `json:"channel"` // gmail | gchat
	Account       string   `json:"account"`
	BatchIDs      []string `json:"batchIds"`
	Targets       []string `json:"targets"` // threadIds or spaceIds
	FirstQueuedAt string   `json:"firstQueuedAt"`
}

func triageCoalescePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "triage_coalesce.json")
}

func loadCoalescedTriage(vaultDir string) []CoalescedTriageEntry {
	data, err := os.ReadFile(triageCoalescePath(vaultDir))
	if err != nil {
		return nil
	}
	var entries []CoalescedTriageEntry
	_ = json.Unmarshal(data, &entries)
	return entries
}

func saveCoalescedTriage(vaultDir string, entries []CoalescedTriageEntry) {
	p := triageCoalescePath(vaultDir)
	if len(entries) == 0 {
		_ = os.Remove(p)
		return
	}
	_ = os.MkdirAll(filepath.Dir(p), 0o700)
	if b, err := json.MarshalIndent(entries, "", "  "); err == nil {
		_ = os.WriteFile(p, b, 0o600)
	}
}

// EnqueueOrFlushCoalescedTriage appends newly discovered triage batches and returns any entries
// ready to spawn now (immediately when window <= 0, or once now - FirstQueuedAt >= window).
func EnqueueOrFlushCoalescedTriage(vaultDir, channel, account, batchID string, targets []string, window time.Duration, now time.Time) []CoalescedTriageEntry {
	if now.IsZero() {
		now = time.Now()
	}
	if window <= 0 && batchID != "" {
		return []CoalescedTriageEntry{{
			Channel:       channel,
			Account:       account,
			BatchIDs:      []string{batchID},
			Targets:       targets,
			FirstQueuedAt: now.UTC().Format(time.RFC3339),
		}}
	}

	entries := loadCoalescedTriage(vaultDir)
	if batchID != "" {
		merged := false
		for i := range entries {
			if entries[i].Channel == channel && entries[i].Account == account {
				entries[i].BatchIDs = appendUniqueStrings(entries[i].BatchIDs, batchID)
				entries[i].Targets = appendUniqueStrings(entries[i].Targets, targets...)
				merged = true
				break
			}
		}
		if !merged {
			entries = append(entries, CoalescedTriageEntry{
				Channel:       channel,
				Account:       account,
				BatchIDs:      []string{batchID},
				Targets:       appendUniqueStrings(nil, targets...),
				FirstQueuedAt: now.UTC().Format(time.RFC3339),
			})
		}
	}

	var ready []CoalescedTriageEntry
	var remaining []CoalescedTriageEntry
	for _, e := range entries {
		if channel != "" && e.Channel != channel {
			remaining = append(remaining, e)
			continue
		}
		if window <= 0 {
			ready = append(ready, e)
			continue
		}
		ts, err := time.Parse(time.RFC3339, e.FirstQueuedAt)
		if err != nil || !now.Before(ts.Add(window)) {
			ready = append(ready, e)
		} else {
			remaining = append(remaining, e)
		}
	}
	saveCoalescedTriage(vaultDir, remaining)
	return ready
}

func appendUniqueStrings(dst []string, vals ...string) []string {
	seen := make(map[string]bool, len(dst))
	for _, v := range dst {
		seen[v] = true
	}
	for _, v := range vals {
		v = strings.TrimSpace(v)
		if v != "" && !seen[v] {
			seen[v] = true
			dst = append(dst, v)
		}
	}
	return dst
}
