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

	"github.com/zjshen14/agy-hq/internal/events"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/routing"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/watcher"
)

// TriggerCursor tracks deterministic sidecar pre-check execution, SHA-256 deduplication,
// and pre-check error health in local/memory/trigger_cursors.json so unchanged ticks consume 0 LLM tokens.
type TriggerCursor struct {
	ID                string `json:"id"`
	LastCheckedAt     string `json:"lastCheckedAt,omitempty"`
	LastCronSlot      string `json:"lastCronSlot,omitempty"`
	LastTriggeredAt   string `json:"lastTriggeredAt,omitempty"`
	LastStdoutSHA256  string `json:"lastStdoutSha256,omitempty"`
	LastStatus        string `json:"lastStatus,omitempty"`
	SkipsCount        int    `json:"skipsCount"`
	ConsecutiveErrors int    `json:"consecutiveErrors,omitempty"`
	LastError         string `json:"lastError,omitempty"`
	LastErrorAt       string `json:"lastErrorAt,omitempty"`
}

func triggerCursorsPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "trigger_cursors.json")
}

func triggerCursorsLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", ".trigger_cursors.lock")
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

func saveTriggerCursorsUnlocked(vaultDir string, cursors map[string]TriggerCursor) error {
	b, err := json.MarshalIndent(cursors, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(triggerCursorsPath(vaultDir), append(b, '\n'), 0o600)
}

func resolveScheduleModel(vaultDir string, s ScheduleTrigger) string {
	prompt := s.DefaultPrompt()
	tier := strings.TrimSpace(s.Tier)
	explicitM := strings.TrimSpace(s.Model)
	if tier != "" {
		builtinM, _, _ := routing.ResolveBuiltin(prompt, "", "", tier)
		if explicitM == "" || strings.EqualFold(explicitM, builtinM) {
			return routing.Resolve(vaultDir, routing.Request{
				Prompt:       prompt,
				ExplicitTier: tier,
			}).ModelID
		}
	}
	return routing.Resolve(vaultDir, routing.Request{
		Prompt:        prompt,
		ExplicitModel: explicitM,
		ExplicitTier:  tier,
	}).ModelID
}

func resolveScheduleDispatch(vaultDir string, s ScheduleTrigger, precheckDigest string) (modelID, effectiveSkill, prompt string) {
	modelID = resolveScheduleModel(vaultDir, s)
	effectiveSkill = s.Skill
	prompt = s.DefaultPrompt()
	if strings.TrimSpace(s.Skill) != "" {
		if disp := extension.ResolveSkillDispatch(vaultDir, s.Skill); disp.Overridden && disp.Trust == "ok" {
			if strings.TrimSpace(disp.Skill) != "" {
				effectiveSkill = strings.TrimSpace(disp.Skill)
			}
			if strings.TrimSpace(disp.SkillPath) != "" {
				prompt += fmt.Sprintf("\n\nCompanion Skill Override: Activate %s (%s) at %s.", effectiveSkill, disp.ActiveTarget, disp.SkillPath)
			}
		}
	}
	if precheckDigest != "" {
		prompt += fmt.Sprintf("\n\nGatekeeper Delta Output:\n%s", precheckDigest)
	}
	if deliv := s.EffectiveDelivery(); deliv != "" {
		prompt += fmt.Sprintf("\n\nDelivery Policy: %s (route non-critical findings via `hq outbox add --tier=%s` rather than interrupting unnecessarily).", deliv, deliv)
	}
	return modelID, effectiveSkill, prompt
}

// RegisteredPulsePrecheck allows cmd/hq to wire internal/pulse.RunPrecheck in-process without import cycles.
var RegisteredPulsePrecheck func(ctx context.Context, vaultDir string, now time.Time, dryRun bool) ([]byte, error)

func isSelfPulsePrecheckArgs(args []string) bool {
	return len(args) >= 2 && args[0] == "pulse" && args[1] == "precheck"
}

func parseSelfWatchPrecheckArgs(args []string) (watcher.PrecheckOptions, bool) {
	if len(args) < 2 || args[0] != "watch" || args[1] != "precheck" {
		return watcher.PrecheckOptions{}, false
	}
	var opts watcher.PrecheckOptions
	rest := args[2:]
	for i := 0; i < len(rest); i++ {
		a := strings.TrimSpace(rest[i])
		switch {
		case a == "" || a == "--json" || a == "-json":
			continue
		case a == "--desktop-only" || a == "-desktop-only":
			opts.DesktopOnly = true
		case strings.HasPrefix(a, "--connector="):
			opts.ConnectorFilter = strings.TrimSpace(strings.TrimPrefix(a, "--connector="))
		case strings.HasPrefix(a, "-connector="):
			opts.ConnectorFilter = strings.TrimSpace(strings.TrimPrefix(a, "-connector="))
		case (a == "--connector" || a == "-connector") && i+1 < len(rest):
			i++
			opts.ConnectorFilter = strings.TrimSpace(rest[i])
		case strings.HasPrefix(a, "--account="):
			opts.AccountFilter = strings.TrimSpace(strings.TrimPrefix(a, "--account="))
		case strings.HasPrefix(a, "-account="):
			opts.AccountFilter = strings.TrimSpace(strings.TrimPrefix(a, "-account="))
		case (a == "--account" || a == "-account") && i+1 < len(rest):
			i++
			opts.AccountFilter = strings.TrimSpace(rest[i])
		default:
			return watcher.PrecheckOptions{}, false
		}
	}
	return opts, true
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
// and `isolated_worker` schedules declared in local/triggers.yaml (plus enabled companion packs).
// If forceCheck is true, cron minute matching is bypassed so callers/tests can evaluate pre-checks immediately.
func EvaluateDaemonGatekeepers(vaultDir string, now time.Time, forceCheck bool, dryRun bool) (actions []string, zeroTokenSaves int, err error) {
	return evaluateGatekeepersCore(vaultDir, "", now, forceCheck, true, dryRun)
}

// TickScheduleOnce evaluates a single schedule on a scheduler/sidecar tick (`hq triggers fire --scheduled <id>`),
// exiting cleanly with a non-empty skipReason and 0 actions
// when the schedule is unconfigured, paused/disabled, not due in the current cron minute, already
// executed for the current cron slot, or is a `front_desk_cron` with no claimed Front Desk lease.
func TickScheduleOnce(vaultDir, targetID string, dryRun bool, now time.Time) (actions []string, skipReason string, err error) {
	targetFQID := extension.NormalizeScheduleFQID(targetID)
	if targetFQID == "" {
		return nil, "", fmt.Errorf("schedule id is required")
	}
	if now.IsZero() {
		now = time.Now()
	}
	cfg, _ := LoadEffectiveTriggers(vaultDir)
	if cfg == nil || len(cfg.Schedules) == 0 {
		return nil, "not_configured", nil
	}
	var matchedSched *ScheduleTrigger
	for i := range cfg.Schedules {
		s := &cfg.Schedules[i]
		if s.FQID() == targetFQID || s.ID == targetID {
			matchedSched = s
			break
		}
	}
	if matchedSched == nil {
		return nil, "not_configured", nil
	}
	if !matchedSched.IsEnabled() {
		return nil, "paused", nil
	}
	cronMatched, slotKey := MatchesCron(matchedSched.Cron, now)
	if !cronMatched {
		return nil, "not_due", nil
	}
	cursors := LoadTriggerCursors(vaultDir)
	if slotKey != "" && cursors[matchedSched.ID].LastCronSlot == slotKey {
		return nil, "already_triggered", nil
	}
	if matchedSched.EffectiveMode() == "front_desk_cron" {
		if fd, hasFD := GetFrontDesk(vaultDir); !hasFD || strings.TrimSpace(fd.ConversationID) == "" {
			return nil, "no_front_desk_lease", nil
		}
	}
	acts, _, evalErr := evaluateGatekeepersCore(vaultDir, matchedSched.FQID(), now, false, true, dryRun)
	return acts, "", evalErr
}

// FireScheduleOnce immediately evaluates or enqueues a single schedule by bare ID or FQID (`hq triggers fire <id>`),
// acting as an explicit manual override (honoring its precheck unless bypassPrecheck is true).
func FireScheduleOnce(vaultDir, targetID string, bypassPrecheck bool, dryRun bool, now time.Time) ([]string, error) {
	targetFQID := extension.NormalizeScheduleFQID(targetID)
	if targetFQID == "" {
		return nil, fmt.Errorf("schedule id is required")
	}
	cfg, _ := LoadEffectiveTriggers(vaultDir)
	if cfg == nil {
		return nil, fmt.Errorf("no triggers configured")
	}
	found := false
	for _, s := range cfg.Schedules {
		if s.FQID() == targetFQID || s.ID == targetID {
			found = true
			if bypassPrecheck {
				if now.IsZero() {
					now = time.Now()
				}
				modelID, effSkill, prompt := resolveScheduleDispatch(vaultDir, s, "")
				title := fmt.Sprintf("[Auto] %s (/%s)", s.ID, effSkill)
				slotKey := now.UTC().Format("2006-01-02T15:04:05Z")
				_, _, enqErr := events.Enqueue(vaultDir, events.Event{
					Source:   "gatekeeper",
					DedupKey: fmt.Sprintf("manual-fire:%s:%s", s.FQID(), slotKey),
					ScopeKey: "gatekeeper:" + s.ID,
					Skill:    effSkill,
					Model:    modelID,
					Title:    title,
					Prompt:   prompt,
					Metadata: map[string]string{
						"trigger":      "gatekeeper:" + s.ID,
						"action_label": fmt.Sprintf("fire:%s -> /%s", s.ID, effSkill),
						"schedule_id":  s.ID,
					},
				}, now)
				if enqErr != nil {
					return nil, enqErr
				}
				return DispatchPendingEvents(vaultDir, now, dryRun)
			}
			break
		}
	}
	if !found {
		return nil, fmt.Errorf("schedule %q not found", targetID)
	}
	actions, _, evalErr := evaluateGatekeepersCore(vaultDir, targetFQID, now, true, false, dryRun)
	return actions, evalErr
}

func evaluateGatekeepersCore(vaultDir, onlyFQID string, now time.Time, forceCheck bool, scheduledMode bool, dryRun bool) (actions []string, zeroTokenSaves int, err error) {
	if now.IsZero() {
		now = time.Now()
	}
	cfg, err := LoadEffectiveTriggers(vaultDir)
	if err != nil && (cfg == nil || len(cfg.Schedules) == 0) {
		return nil, 0, nil
	}
	if cfg == nil || len(cfg.Schedules) == 0 {
		return nil, 0, nil
	}

	rootDir := filepath.Dir(filepath.Clean(vaultDir))
	if envRoot := strings.TrimSpace(os.Getenv("HQ_ROOT_DIR")); envRoot != "" {
		if abs, aErr := filepath.Abs(envRoot); aErr == nil {
			rootDir = abs
		}
	}

	type scheduledCandidate struct {
		sched         ScheduleTrigger
		slotKey       string
		prevStdoutSHA string
	}
	var candidates []scheduledCandidate
	_ = store.With(context.Background(), store.RankTriggerCur, triggerCursorsLockPath(vaultDir), func(_ context.Context) error {
		cursors := LoadTriggerCursors(vaultDir)
		for _, s := range cfg.Schedules {
			if onlyFQID != "" && s.FQID() != onlyFQID {
				continue
			}
			if scheduledMode && !s.IsEnabled() {
				continue
			}

			if forceCheck && onlyFQID == "" {
				if len(s.PrecheckCmd) == 0 {
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
			candidates = append(candidates, scheduledCandidate{
				sched:         s,
				slotKey:       slotKey,
				prevStdoutSHA: cur.LastStdoutSHA256,
			})
		}
		return nil
	})
	if len(candidates) == 0 {
		return nil, 0, nil
	}

	type cursorMutation struct {
		id            string
		slotKey       string
		errMsg        string
		statusNote    string
		pausedNoSkip  bool
		skipped       bool
		spawned       bool
		nextStdoutSHA string
		alertSkill    string
	}
	var mutations []cursorMutation

	scrubExternalPrecheckEnv := func(base []string) []string {
		blocked := map[string]bool{
			"ANTIGRAVITY_CSRF_TOKEN":       true,
			"ANTIGRAVITY_LS_ADDRESS":       true,
			"ANTIGRAVITY_AGENTAPI_EXE":     true,
			"ANTIGRAVITY_CREDENTIAL_SCOPE": true,
			"ANTIGRAVITY_PLUGIN_SHIM_DIR":  true,
		}
		out := make([]string, 0, len(base)+1)
		for _, kv := range base {
			k, _, ok := strings.Cut(kv, "=")
			if ok && blocked[k] {
				continue
			}
			out = append(out, kv)
		}
		return append(out, "ANTIGRAVITY_PLUGIN_SHIM_DIR=/nonexistent-hq-sentinel")
	}

	for _, cand := range candidates {
		s := cand.sched
		slotKey := cand.slotKey
		shouldSpawn := false
		precheckDigest := ""
		nextStdoutSHA := cand.prevStdoutSHA
		shaHex := "no-precheck"
		isExternalPrecheck := false

		if len(s.PrecheckCmd) > 0 {
			bin0 := s.PrecheckCmd[0]
			isSelfHQ := bin0 == "hq" || bin0 == "./bin/hq" || bin0 == "bin/hq" || extension.IsSelfHQBinary(bin0)
			isExternalPrecheck = !isSelfHQ

			var binResolveErr error
			if strings.HasPrefix(s.Source, "pack:") && isExternalPrecheck {
				bin0, binResolveErr = extension.VerifyTrustedBinary(vaultDir, s.Source, bin0)
			} else if isSelfHQ {
				candBin := filepath.Join(rootDir, bin0)
				if bin0 != "hq" {
					if st, sErr := os.Stat(candBin); sErr == nil && !st.IsDir() {
						bin0 = candBin
					} else if exe, eErr := os.Executable(); eErr == nil && exe != "" && strings.HasPrefix(filepath.Base(exe), "hq") {
						bin0 = exe
					} else if lp, lErr := exec.LookPath("hq"); lErr == nil && lp != "" {
						bin0 = lp
					}
				} else if exe, eErr := os.Executable(); eErr == nil && exe != "" && strings.HasPrefix(filepath.Base(exe), "hq") {
					bin0 = exe
				} else if st, sErr := os.Stat(filepath.Join(rootDir, "bin", "hq")); sErr == nil && !st.IsDir() {
					bin0 = filepath.Join(rootDir, "bin", "hq")
				} else if lp, lErr := exec.LookPath("hq"); lErr == nil && lp != "" {
					bin0 = lp
				}
			}

			var outBytes []byte
			var cmdErr error
			timeout := 15 * time.Second
			if s.PrecheckTimeoutMs > 0 {
				timeout = time.Duration(s.PrecheckTimeoutMs) * time.Millisecond
			}
			if binResolveErr != nil {
				cmdErr = binResolveErr
			} else if isSelfHQ && func() bool {
				watchOpts, ok := parseSelfWatchPrecheckArgs(s.PrecheckCmd[1:])
				if !ok {
					return false
				}
				if dryRun {
					outBytes = []byte("{\"enqueued_total\":0}\n")
					return true
				}
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				watchOpts.Now = now
				sum, pErr := watcher.RunPrecheck(ctx, vaultDir, watchOpts)
				cancel()
				cmdErr = pErr
				if pErr == nil {
					outBytes, _ = json.Marshal(sum)
				}
				return true
			}() {
				// handled in-process via watcher.RunPrecheck
			} else if isSelfHQ && RegisteredPulsePrecheck != nil && isSelfPulsePrecheckArgs(s.PrecheckCmd[1:]) {
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				outBytes, cmdErr = RegisteredPulsePrecheck(ctx, vaultDir, now, dryRun)
				cancel()
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), timeout)
				cmd := exec.CommandContext(ctx, bin0, s.PrecheckCmd[1:]...)
				cmd.Dir = rootDir
				env := append(sanitizedGitCmd().Env, "HQ_ROOT_DIR="+rootDir, "HQ_VAULT_DIR="+vaultDir)
				if isExternalPrecheck {
					env = scrubExternalPrecheckEnv(env)
				}
				cmd.Env = env
				outBytes, cmdErr = cmd.CombinedOutput()
				cancel()
			}

			trimmedOut := strings.TrimSpace(string(outBytes))
			if cmdErr != nil {
				errMsg := cmdErr.Error()
				if trimmedOut != "" {
					errMsg = errMsg + ": " + trimmedOut
				}
				errMsg = store.TruncateRunes(errMsg, 240)
				skillName := s.Skill
				if skillName == "" {
					skillName = s.ID
				}
				mutations = append(mutations, cursorMutation{
					id:         s.ID,
					slotKey:    slotKey,
					errMsg:     errMsg,
					alertSkill: skillName,
				})
				continue
			}

			sum := sha256.Sum256([]byte(trimmedOut))
			shaHex = hex.EncodeToString(sum[:])

			dedup := s.EffectiveDedupMode()
			switch dedup {
			case "precheck_enqueues":
				// Precheck command (e.g. `hq watch precheck --json`) already enqueued connector:* events directly;
				// never spawn a duplicate gatekeeper event. Still advance trigger cursor so cron slot isn't re-polled,
				// while distinguishing paused/errored connector polls from genuine 0-token idle skips.
				var preSum struct {
					EnqueuedTotal int `json:"enqueued_total"`
					EmittedCount  int `json:"emittedCount"`
					Results       []struct {
						Connector string `json:"connector"`
						Account   string `json:"account"`
						Status    string `json:"status"`
					} `json:"results"`
				}
				if trimmedOut != "" {
					_ = json.Unmarshal([]byte(trimmedOut), &preSum)
				}
				nonOKStatus := ""
				anyOK := false
				for _, r := range preSum.Results {
					if r.Status == "ok" {
						anyOK = true
					} else if nonOKStatus == "" && strings.TrimSpace(r.Status) != "" {
						nonOKStatus = fmt.Sprintf("%s:%s %s", r.Connector, r.Account, r.Status)
					}
				}
				totalTriggered := preSum.EnqueuedTotal + preSum.EmittedCount
				if totalTriggered > 0 {
					actions = append(actions, fmt.Sprintf("precheck_enqueued:%s=%d", s.ID, totalTriggered))
					mutations = append(mutations, cursorMutation{
						id:            s.ID,
						slotKey:       slotKey,
						statusNote:    "ok",
						spawned:       true,
						nextStdoutSHA: shaHex,
					})
					continue
				}
				if len(preSum.Results) > 0 && !anyOK && nonOKStatus != "" {
					mutations = append(mutations, cursorMutation{
						id:           s.ID,
						slotKey:      slotKey,
						statusNote:   nonOKStatus,
						pausedNoSkip: true,
					})
					continue
				}
				shouldSpawn = false
			case "none":
				shouldSpawn = true
			case "exit_zero_nonempty":
				shouldSpawn = trimmedOut != ""
			default: // "stdout_sha256"
				if trimmedOut != "" && shaHex != cand.prevStdoutSHA {
					shouldSpawn = true
					nextStdoutSHA = shaHex
				}
			}

			if !shouldSpawn {
				zeroTokenSaves++
				mutations = append(mutations, cursorMutation{
					id:      s.ID,
					slotKey: slotKey,
					skipped: true,
				})
				continue
			}
			precheckDigest = store.TruncateRunes(trimmedOut, 300)
		} else {
			shouldSpawn = true
		}

		if shouldSpawn {
			sessionID := ""
			if s.EffectiveMode() == "front_desk_cron" {
				if fd, hasFD := GetFrontDesk(vaultDir); hasFD && strings.TrimSpace(fd.ConversationID) != "" {
					if dryRun || HasReachableLSSession(vaultDir) {
						sessionID = strings.TrimSpace(fd.ConversationID)
					} else if _, lpErr := exec.LookPath("agentapi"); lpErr == nil {
						sessionID = strings.TrimSpace(fd.ConversationID)
					}
				}
				if sessionID == "" && scheduledMode {
					mutations = append(mutations, cursorMutation{
						id:      s.ID,
						slotKey: slotKey,
						skipped: true,
					})
					continue
				}
			}

			modelID, effSkill, prompt := resolveScheduleDispatch(vaultDir, s, precheckDigest)
			title := fmt.Sprintf("[Auto] %s (/%s)", s.ID, effSkill)
			dedupKey := fmt.Sprintf("gatekeeper:%s:%s:%s", s.ID, slotKey, shaHex)
			schedTickVal := "0"
			if scheduledMode {
				schedTickVal = "1"
			}
			_, _, enqErr := events.Enqueue(vaultDir, events.Event{
				Source:       "gatekeeper",
				DedupKey:     dedupKey,
				ScopeKey:     "gatekeeper:" + s.ID,
				SessionID:    sessionID,
				Skill:        effSkill,
				Model:        modelID,
				Title:        title,
				Prompt:       prompt,
				TaintOnSpawn: isExternalPrecheck,
				Metadata: map[string]string{
					"trigger":        "gatekeeper:" + s.ID,
					"action_label":   fmt.Sprintf("gatekeeper:%s -> /%s", s.ID, effSkill),
					"schedule_id":    s.ID,
					"schedule_mode":  s.EffectiveMode(),
					"scheduled_tick": schedTickVal,
				},
			}, now)
			if enqErr == nil {
				mutations = append(mutations, cursorMutation{
					id:            s.ID,
					slotKey:       slotKey,
					spawned:       true,
					nextStdoutSHA: nextStdoutSHA,
				})
			}
		}
	}

	if len(mutations) > 0 {
		var alerts []cursorMutation
		_ = store.With(context.Background(), store.RankTriggerCur, triggerCursorsLockPath(vaultDir), func(_ context.Context) error {
			cursors := LoadTriggerCursors(vaultDir)
			for _, m := range mutations {
				cur := cursors[m.id]
				cur.ID = m.id
				cur.LastCheckedAt = now.UTC().Format(time.RFC3339)
				if m.errMsg != "" {
					cur.ConsecutiveErrors++
					cur.LastError = m.errMsg
					cur.LastErrorAt = now.UTC().Format(time.RFC3339)
					if cur.ConsecutiveErrors == 3 {
						alerts = append(alerts, m)
					}
				} else {
					cur.ConsecutiveErrors = 0
					cur.LastError = ""
					cur.LastErrorAt = ""
					if m.pausedNoSkip {
						cur.LastCronSlot = m.slotKey
						cur.LastStatus = m.statusNote
					} else if m.skipped {
						cur.LastCronSlot = m.slotKey
						cur.LastStatus = "ok"
						cur.SkipsCount++
					} else if m.spawned {
						cur.LastCronSlot = m.slotKey
						cur.LastStatus = "ok"
						cur.LastStdoutSHA256 = m.nextStdoutSHA
						cur.LastTriggeredAt = now.UTC().Format(time.RFC3339)
					}
				}
				cursors[m.id] = cur
			}
			return saveTriggerCursorsUnlocked(vaultDir, cursors)
		})
		for _, a := range alerts {
			_, _, _ = outbox.Add(vaultDir, outbox.OutboxItem{
				Tier:        outbox.TierBriefRollup,
				SourceSkill: a.alertSkill,
				Scope:       a.id,
				Title:       "Gatekeeper precheck failing: " + a.id,
				Summary:     fmt.Sprintf("Schedule %q failed 3 consecutive pre-checks without advancing cursor. Last error: %s", a.id, a.errMsg),
			}, now)
		}
	}

	if zeroTokenSaves > 0 {
		_ = UpdateQuota(vaultDir, func(st *QuotaState) (bool, error) {
			st.GatekeeperSavesToday += zeroTokenSaves
			return true, nil
		})
	}
	if dispatched, dispErr := DispatchPendingEvents(vaultDir, now, dryRun); dispErr == nil && len(dispatched) > 0 {
		actions = append(actions, dispatched...)
	}
	return actions, zeroTokenSaves, nil
}
