package kernel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/store"
)

// ArmedTrigger tracks a proactive schedule from local/triggers.yaml that has been armed
// in the active Front Desk conversation via Antigravity's native `schedule` tool.
type ArmedTrigger struct {
	ID      string `json:"id"`
	TaskID  string `json:"taskId,omitempty"`
	Cron    string `json:"cron"`
	Skill   string `json:"skill,omitempty"`
	Reason  string `json:"reason,omitempty"`
	ArmedAt string `json:"armedAt"`
}

// AdHocCron tracks a user-requested recurring conversation cron in the active Front Desk
// conversation that is NOT managed by local/triggers.yaml.
type AdHocCron struct {
	TaskID  string `json:"taskId,omitempty"`
	Cron    string `json:"cron"`
	Summary string `json:"summary"`
	ArmedAt string `json:"armedAt,omitempty"`
}

// FrontDeskLease tracks the single active "Front Desk" intake conversationId in local/memory/front_desk.json
// to prevent split-brain race conditions across multiple top-level conversations.
type FrontDeskLease struct {
	ConversationID    string                  `json:"conversationId"`
	ArtifactDir       string                  `json:"artifactDir,omitempty"`
	ProjectID         string                  `json:"projectId,omitempty"`
	WorkspaceDir      string                  `json:"workspaceDir,omitempty"`
	ConsolePinned     bool                    `json:"consolePinned,omitempty"`
	LiveConsolePinned bool                    `json:"liveConsolePinned,omitempty"`
	ArmedTriggers     map[string]ArmedTrigger `json:"armedTriggers,omitempty"`
	AdHocCrons        []AdHocCron             `json:"adHocCrons,omitempty"`
	KilledTaskIDs     []string                `json:"killedTaskIds,omitempty"`
	UpdatedAt         string                  `json:"updatedAt"`
}

// FrontDeskPinTip is the standardized recommendation for users to pin the [HQ] Front Desk conversation to their Antigravity sidebar.
const FrontDeskPinTip = "Pin the '[HQ] Front Desk' conversation to your Antigravity sidebar so it stays permanently anchored above ephemeral task threads."

// FormatFrontDeskPinTip returns a user-facing formatted tip string.
func FormatFrontDeskPinTip() string {
	return "💡 Tip: " + FrontDeskPinTip
}

func frontDeskPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "front_desk.json")
}

func frontDeskHistoryPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "front_desk_history.json")
}

func recordHistoricalFrontDeskID(vaultDir, convID string) {
	cid := normalizeUUID(convID)
	if vaultDir == "" || cid == "" {
		return
	}
	var hist []string
	if data, err := os.ReadFile(frontDeskHistoryPath(vaultDir)); err == nil {
		_ = json.Unmarshal(data, &hist)
	}
	seen := make(map[string]bool, len(hist)+1)
	out := make([]string, 0, len(hist)+1)
	for _, h := range hist {
		nh := normalizeUUID(h)
		if nh != "" && nh != cid && !seen[nh] {
			seen[nh] = true
			out = append(out, nh)
		}
	}
	out = append(out, cid)
	if len(out) > 32 {
		out = out[len(out)-32:]
	}
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
	if b, err := json.MarshalIndent(out, "", "  "); err == nil {
		_ = store.WriteAtomic(frontDeskHistoryPath(vaultDir), append(b, '\n'), 0o644)
	}
}

// GetKnownFrontDeskIDSet returns all normalized conversation IDs that currently hold
// or previously held the Front Desk lease (preventing Front Desk rotation or temporary
// vacancy from stripping historical Layer-1 subagents).
func GetKnownFrontDeskIDSet(vaultDir string) map[string]bool {
	out := make(map[string]bool)
	if fd, ok := GetFrontDesk(vaultDir); ok && strings.TrimSpace(fd.ConversationID) != "" {
		out[normalizeUUID(fd.ConversationID)] = true
	}
	if data, err := os.ReadFile(frontDeskHistoryPath(vaultDir)); err == nil {
		var hist []string
		if json.Unmarshal(data, &hist) == nil {
			for _, h := range hist {
				if nh := normalizeUUID(h); nh != "" {
					out[nh] = true
				}
			}
		}
	}
	if tasks, err := ListTasks(vaultDir); err == nil {
		for _, t := range tasks {
			if strings.HasPrefix(t.TaskSlug, "headquarter-front-desk-") && strings.TrimSpace(t.ConversationID) != "" {
				out[normalizeUUID(t.ConversationID)] = true
			}
		}
	}
	return out
}

// GetFrontDesk reads the currently active Front Desk conversation lease.
func GetFrontDesk(vaultDir string) (FrontDeskLease, bool) {
	var lease FrontDeskLease
	data, err := os.ReadFile(frontDeskPath(vaultDir))
	if err != nil {
		return lease, false
	}
	if err := json.Unmarshal(data, &lease); err != nil || lease.ConversationID == "" {
		return lease, false
	}
	return lease, true
}

func saveFrontDeskLease(vaultDir string, lease FrontDeskLease) error {
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(lease, "", "  ")
	if err != nil {
		return err
	}
	return store.WriteAtomic(frontDeskPath(vaultDir), append(data, '\n'), 0o644)
}

func removeConsoleArtifacts(artifactDir string) {
	if artifactDir == "" {
		return
	}
	for _, name := range []string{
		"hq_console.md",
		"hq_console.md.metadata.json",
		"hq_console_live.html",
		"hq_console_live.html.metadata.json",
		"hq_console_mobile.md",
		"hq_console_mobile.md.metadata.json",
	} {
		_ = os.Remove(filepath.Join(artifactDir, name))
	}
}

// SetFrontDesk atomically claims the single Front Desk intake lease for convID.
// If convID already holds the lease, ConsolePinned and ArmedTriggers are preserved.
func SetFrontDesk(vaultDir, convID, artifactDir string) error {
	if convID == "" {
		return nil
	}
	if IsSubagentConversation(vaultDir, convID) {
		return fmt.Errorf("cannot claim Front Desk lease for a child subagent")
	}
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	artifactDir = strings.TrimSpace(artifactDir)
	prev, hasPrev := GetFrontDesk(vaultDir)
	if artifactDir == "" && hasPrev && prev.ConversationID == convID && prev.ArtifactDir != "" {
		artifactDir = prev.ArtifactDir
	}
	if artifactDir == "" {
		if roots := hostpaths.Resolve(vaultDir); roots.BrainDir != "" {
			artifactDir = filepath.Join(roots.BrainDir, convID)
		}
	}
	if artifactDir != "" {
		_ = os.MkdirAll(artifactDir, 0o755)
	}
	if hasPrev && prev.ConversationID != convID {
		removeConsoleArtifacts(prev.ArtifactDir)
	}
	lease := FrontDeskLease{
		ConversationID: convID,
		ArtifactDir:    artifactDir,
		UpdatedAt:      time.Now().Format(time.RFC3339),
	}
	if hasPrev && prev.ConversationID == convID {
		lease.ConsolePinned = prev.ConsolePinned
		lease.LiveConsolePinned = prev.LiveConsolePinned
		lease.ArmedTriggers = prev.ArmedTriggers
		lease.AdHocCrons = prev.AdHocCrons
		lease.KilledTaskIDs = prev.KilledTaskIDs
		lease.ProjectID = prev.ProjectID
		lease.WorkspaceDir = prev.WorkspaceDir
	}
	if lease.WorkspaceDir == "" && vaultDir != "" {
		cleanV := filepath.Clean(vaultDir)
		if filepath.Base(cleanV) == "local" {
			lease.WorkspaceDir = filepath.Dir(cleanV)
		}
	}
	candProjID := lease.ProjectID
	if os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") == "" {
		envConv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
		if projID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID")); projID != "" && (envConv == "" || envConv == convID) {
			if valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, projID, lease.WorkspaceDir); !regAvail || valid {
				candProjID = projID
			}
		}
	}
	if lease.WorkspaceDir != "" {
		verifiedProjID, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, lease.WorkspaceDir, candProjID)
		lease.ProjectID = verifiedProjID
	} else {
		lease.ProjectID = candProjID
	}
	if artifactDir != "" {
		_ = store.WriteAtomic(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"), []byte(artifactDir), 0o644)
	}
	_ = os.Remove(filepath.Join(vaultDir, "memory", ".vacated"))
	if err := saveFrontDeskLease(vaultDir, lease); err != nil {
		return err
	}
	if hasPrev && strings.TrimSpace(prev.ConversationID) != "" && normalizeUUID(prev.ConversationID) != normalizeUUID(convID) {
		recordHistoricalFrontDeskID(vaultDir, prev.ConversationID)
	}
	recordHistoricalFrontDeskID(vaultDir, convID)
	ReconcileFrontDeskFromTranscript(vaultDir, "")
	return nil
}

// SetFrontDeskWorkspace records the primary workspace root associated with the active Front Desk conversation.
func SetFrontDeskWorkspace(vaultDir, convID, workspaceDir string) error {
	workspaceDir = strings.TrimSpace(workspaceDir)
	if workspaceDir == "" || convID == "" {
		return nil
	}
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || fd.ConversationID != convID {
		return nil
	}
	if abs, err := filepath.Abs(workspaceDir); err == nil {
		workspaceDir = abs
	}
	verifiedProjID, _, repaired := ResolveVerifiedWorkspaceProjectID(vaultDir, workspaceDir, fd.ProjectID)
	if fd.WorkspaceDir == workspaceDir && (!repaired || fd.ProjectID == verifiedProjID) {
		return nil
	}
	fd.WorkspaceDir = workspaceDir
	fd.ProjectID = verifiedProjID
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
}

func primaryVaultWorkspaceDir(vaultDir string) string {
	if strings.TrimSpace(vaultDir) == "" {
		return ""
	}
	if fd, ok := GetFrontDesk(vaultDir); ok && strings.TrimSpace(fd.WorkspaceDir) != "" {
		w := strings.TrimSpace(fd.WorkspaceDir)
		if abs, err := filepath.Abs(w); err == nil {
			w = abs
		}
		return filepath.Clean(w)
	}
	cleanV := filepath.Clean(vaultDir)
	if abs, err := filepath.Abs(cleanV); err == nil {
		cleanV = abs
	}
	if filepath.Base(cleanV) == "local" {
		return filepath.Dir(cleanV)
	}
	return ""
}

func isCallerInPrimaryVaultWorkspace(vaultDir string) bool {
	if strings.TrimSpace(vaultDir) == "" || os.Getenv("ANTIGRAVITY_SIDECAR_WEB_PORT") != "" {
		return false
	}
	primary := primaryVaultWorkspaceDir(vaultDir)
	envProj := strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
	if envProj != "" && primary != "" {
		if valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, envProj, primary); regAvail && !valid {
			return false
		}
	}
	convID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	fd, hasFD := GetFrontDesk(vaultDir)
	if convID != "" {
		if hasFD && fd.ConversationID == convID {
			return true
		}
		if envProj != "" && primary != "" {
			if valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, envProj, primary); regAvail && valid {
				return true
			}
		}
		if hasFD && fd.ConversationID != "" {
			// A different conversation without registry proof must never impersonate the primary Vault project via cwd alone.
			return false
		}
	}
	if primary == "" {
		return false
	}
	if envProj != "" {
		if valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, envProj, primary); regAvail {
			return valid
		}
		if hasFD && strings.TrimSpace(fd.ProjectID) != "" && envProj != strings.TrimSpace(fd.ProjectID) {
			return false
		}
	}
	wd, err := os.Getwd()
	if err != nil || strings.TrimSpace(wd) == "" {
		return false
	}
	if abs, aErr := filepath.Abs(wd); aErr == nil {
		wd = abs
	}
	return filepath.Clean(wd) == filepath.Clean(primary)
}

// BackfillFrontDeskProjectID persists or repairs ANTIGRAVITY_PROJECT_ID in front_desk.json,
// validating existing and candidate projectIds against Antigravity's `<ConfigDir>/projects/*.json` registry.
func BackfillFrontDeskProjectID(vaultDir string) {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return
	}
	wsDir := primaryVaultWorkspaceDir(vaultDir)
	curProj := strings.TrimSpace(fd.ProjectID)
	if curProj != "" {
		valid, regAvail := ValidateProjectMatchesWorkspace(vaultDir, curProj, wsDir)
		if !regAvail || valid {
			return
		}
		// Existing ProjectID is poisoned (points to an unrelated workspace); repair or clear it.
		cand := ""
		if isCallerInPrimaryVaultWorkspace(vaultDir) {
			cand = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
		}
		repairedID, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, wsDir, cand)
		if repairedID != curProj {
			fd.ProjectID = repairedID
			fd.UpdatedAt = time.Now().Format(time.RFC3339)
			_ = saveFrontDeskLease(vaultDir, fd)
		}
		return
	}

	cand := ""
	if isCallerInPrimaryVaultWorkspace(vaultDir) {
		cand = strings.TrimSpace(os.Getenv("ANTIGRAVITY_PROJECT_ID"))
	}
	verifiedID, _, _ := ResolveVerifiedWorkspaceProjectID(vaultDir, wsDir, cand)
	if verifiedID == "" {
		return
	}
	fd.ProjectID = verifiedID
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	_ = saveFrontDeskLease(vaultDir, fd)
}

// MarkConsolePinned records that the active Front Desk has pinned hq_console.md via write_file.
func MarkConsolePinned(vaultDir, convID string) error {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return nil
	}
	if convID != "" && fd.ConversationID != convID {
		return nil
	}
	if fd.ConsolePinned {
		return nil
	}
	fd.ConsolePinned = true
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
}

// MarkLiveConsolePinned records that the active Front Desk has pinned hq_console_live.html via write_file.
func MarkLiveConsolePinned(vaultDir, convID string) error {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return nil
	}
	if convID != "" && fd.ConversationID != convID {
		return nil
	}
	if fd.LiveConsolePinned {
		return nil
	}
	fd.LiveConsolePinned = true
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
}

func normalizeFiveFieldCronExpr(cronExpr string) string {
	normCron := strings.Join(strings.Fields(strings.Trim(strings.TrimSpace(cronExpr), "\"")), " ")
	if strings.HasPrefix(strings.ToUpper(normCron), "CRON_TZ=") {
		parts := strings.Fields(normCron)
		if len(parts) > 1 {
			normCron = strings.Join(parts[1:], " ")
		}
	}
	return normCron
}

func matchScheduleTrigger(schedules []ScheduleTrigger, cronExpr, prompt string) (ScheduleTrigger, bool) {
	normCron := normalizeFiveFieldCronExpr(cronExpr)
	lowerPrompt := strings.ToLower(strings.Trim(strings.TrimSpace(prompt), "\""))
	for _, s := range schedules {
		if s.ID == "" {
			continue
		}
		lowerID := strings.ToLower(s.ID)
		if strings.Contains(lowerPrompt, "[hq-trigger:"+lowerID+"]") || strings.Contains(lowerPrompt, lowerID) {
			return s, true
		}
	}
	if normCron != "" {
		for _, s := range schedules {
			if s.FiveFieldCron() == normCron {
				return s, true
			}
		}
		return ScheduleTrigger{}, false
	}
	for _, s := range schedules {
		if s.Skill != "" && strings.Contains(lowerPrompt, strings.ToLower(s.Skill)) {
			return s, true
		}
	}
	return ScheduleTrigger{}, false
}

func loadAllKnownSchedules(vaultDir string) []ScheduleTrigger {
	if cfg, err := LoadEffectiveTriggers(vaultDir); err == nil && cfg != nil && len(cfg.Schedules) > 0 {
		return cfg.Schedules
	}
	if cfg, err := LoadTriggers(vaultDir); err == nil && cfg != nil {
		return cfg.Schedules
	}
	return nil
}

func upsertAdHocCron(list []AdHocCron, item AdHocCron) ([]AdHocCron, bool) {
	if item.TaskID == "" && item.Cron == "" {
		return list, false
	}
	for i := range list {
		cur := &list[i]
		if item.TaskID != "" && cur.TaskID == item.TaskID {
			if cur.Cron == item.Cron && cur.Summary == item.Summary {
				return list, false
			}
			cur.Cron = item.Cron
			cur.Summary = item.Summary
			return list, true
		}
		if cur.Cron == item.Cron && cur.Summary == item.Summary {
			if item.TaskID != "" && cur.TaskID == "" {
				cur.TaskID = item.TaskID
				return list, true
			}
			return list, false
		}
	}
	return append(list, item), true
}

// RecordArmedTrigger records a scheduled trigger or ad-hoc user cron armed in the active Front Desk via `schedule`.
func RecordArmedTrigger(vaultDir, convID, cronExpr, prompt string) (string, error) {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return "", nil
	}
	if convID != "" && fd.ConversationID != convID {
		return "", nil
	}
	cleanCron := strings.Trim(strings.TrimSpace(cronExpr), "\"")
	cleanPrompt := strings.Trim(strings.TrimSpace(prompt), "\"")
	if fd.ArmedTriggers == nil {
		fd.ArmedTriggers = make(map[string]ArmedTrigger)
	}
	schedules := loadAllKnownSchedules(vaultDir)
	if matched, found := matchScheduleTrigger(schedules, cleanCron, cleanPrompt); found {
		prev := fd.ArmedTriggers[matched.ID]
		fd.ArmedTriggers[matched.ID] = ArmedTrigger{
			ID:      matched.ID,
			TaskID:  prev.TaskID,
			Cron:    matched.FiveFieldCron(),
			Skill:   matched.Skill,
			ArmedAt: time.Now().Format(time.RFC3339),
		}
		fd.UpdatedAt = time.Now().Format(time.RFC3339)
		return matched.ID, saveFrontDeskLease(vaultDir, fd)
	}
	if cleanCron != "" {
		summary := SanitizeTaskTitle(cleanPrompt, cleanCron)
		updated, changed := upsertAdHocCron(fd.AdHocCrons, AdHocCron{
			Cron:    cleanCron,
			Summary: summary,
			ArmedAt: time.Now().Format(time.RFC3339),
		})
		if changed {
			fd.AdHocCrons = updated
			fd.UpdatedAt = time.Now().Format(time.RFC3339)
			return "adhoc-" + slugify(cleanCron), saveFrontDeskLease(vaultDir, fd)
		}
		return "adhoc-" + slugify(cleanCron), nil
	}
	return "", nil
}

// RecordKilledFrontDeskTask marks a background timer/task in the active Front Desk as killed so
// transcript reconciliation never resurrects it, and removes matching armed/ad-hoc entries.
func RecordKilledFrontDeskTask(vaultDir, convID, taskID string) error {
	taskID = strings.Trim(strings.TrimSpace(taskID), "\"")
	if taskID == "" {
		return nil
	}
	fd, ok := GetFrontDesk(vaultDir)
	if !ok {
		return nil
	}
	if convID != "" && fd.ConversationID != convID {
		return nil
	}
	changed := false
	seenKilled := false
	for _, k := range fd.KilledTaskIDs {
		if k == taskID {
			seenKilled = true
			break
		}
	}
	if !seenKilled {
		fd.KilledTaskIDs = append(fd.KilledTaskIDs, taskID)
		changed = true
	}
	for id, at := range fd.ArmedTriggers {
		if at.TaskID == taskID {
			delete(fd.ArmedTriggers, id)
			changed = true
		}
	}
	if len(fd.AdHocCrons) > 0 {
		kept := fd.AdHocCrons[:0]
		for _, ac := range fd.AdHocCrons {
			if ac.TaskID == taskID {
				changed = true
				continue
			}
			kept = append(kept, ac)
		}
		fd.AdHocCrons = kept
	}
	if !changed {
		return nil
	}
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
}

func parseGenericCronLaunchContent(content string) (taskID, cronExpr, prompt string, ok bool) {
	if !strings.Contains(content, "Tool is running as a background task with task id:") ||
		!strings.Contains(content, "Task Description: Cron:") {
		return "", "", "", false
	}
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if strings.HasPrefix(line, "Tool is running as a background task with task id:") {
			taskID = strings.TrimSpace(strings.TrimPrefix(line, "Tool is running as a background task with task id:"))
		} else if strings.HasPrefix(line, "Task Description:") {
			desc := strings.TrimSpace(strings.TrimPrefix(line, "Task Description:"))
			if strings.HasPrefix(desc, "Cron:") {
				rest := strings.TrimSpace(strings.TrimPrefix(desc, "Cron:"))
				c, p, hasPrompt := strings.Cut(rest, ", Prompt:")
				cronExpr = strings.Trim(strings.TrimSpace(c), "\"")
				if hasPrompt {
					prompt = strings.Trim(strings.TrimSpace(p), "\"")
				}
			}
		}
	}
	if taskID == "" || cronExpr == "" {
		return "", "", "", false
	}
	return taskID, cronExpr, prompt, true
}

// ReconcileFrontDeskFromTranscript inspects the active Front Desk conversation's transcript.jsonl
// (if present on disk) to backfill ConsolePinned, harness ArmedTriggers (with TaskIDs), AdHocCrons,
// and KilledTaskIDs.
type fdTranscriptStamp struct {
	transcriptPath string
	tSize          int64
	tModNano       int64
	fdSize         int64
	fdModNano      int64
}

var (
	fdReconcileMu    sync.Mutex
	fdReconcileCache = make(map[string]fdTranscriptStamp)
)

func ReconcileFrontDeskFromTranscript(vaultDir, transcriptPath string) bool {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || fd.ConversationID == "" {
		return false
	}
	if transcriptPath == "" && fd.ArtifactDir != "" {
		transcriptPath = filepath.Join(fd.ArtifactDir, ".system_generated", "logs", "transcript.jsonl")
	}
	if transcriptPath == "" {
		return false
	}
	tInfo, tErr := os.Stat(transcriptPath)
	if tErr != nil {
		return false
	}
	fdInfo, _ := os.Stat(frontDeskPath(vaultDir))
	curStamp := fdTranscriptStamp{
		transcriptPath: transcriptPath,
		tSize:          tInfo.Size(),
		tModNano:       tInfo.ModTime().UnixNano(),
	}
	if fdInfo != nil {
		curStamp.fdSize = fdInfo.Size()
		curStamp.fdModNano = fdInfo.ModTime().UnixNano()
	}
	cacheKey := filepath.Clean(vaultDir)
	fdReconcileMu.Lock()
	prevStamp, hasPrev := fdReconcileCache[cacheKey]
	fdReconcileMu.Unlock()
	if hasPrev && prevStamp == curStamp {
		return false
	}
	f, err := os.Open(transcriptPath)
	if err != nil {
		return false
	}
	defer f.Close()

	schedules := loadAllKnownSchedules(vaultDir)
	knownSched := make(map[string]bool, len(schedules)*2)
	for _, s := range schedules {
		if s.ID != "" {
			knownSched[s.ID] = true
			knownSched[s.FQID()] = true
		}
	}

	killedSet := make(map[string]bool, len(fd.KilledTaskIDs))
	for _, kid := range fd.KilledTaskIDs {
		if k := strings.TrimSpace(kid); k != "" {
			killedSet[k] = true
		}
	}

	type transcriptCronItem struct {
		taskID   string
		cronExpr string
		prompt   string
		armedAt  string
	}
	var launchedCrons []transcriptCronItem
	launchedKeys := make(map[string]bool)
	var fallbackCalls []transcriptCronItem

	changed := false
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	for scanner.Scan() {
		var step struct {
			Type      string `json:"type"`
			CreatedAt string `json:"created_at"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			} `json:"tool_calls"`
		}
		if json.Unmarshal(scanner.Bytes(), &step) != nil {
			continue
		}
		for _, tc := range step.ToolCalls {
			switch tc.Name {
			case "write_file", "edit_file":
				rawPath, _ := tc.Args["Path"].(string)
				p := strings.Trim(strings.TrimSpace(rawPath), "\"")
				slashP := filepath.ToSlash(p)
				if strings.HasSuffix(slashP, "/hq_console.md") || p == "hq_console.md" {
					if !fd.ConsolePinned {
						fd.ConsolePinned = true
						changed = true
					}
				}
				if strings.HasSuffix(slashP, "/hq_console_live.html") || p == "hq_console_live.html" {
					if !fd.LiveConsolePinned {
						fd.LiveConsolePinned = true
						changed = true
					}
				}
			case "manage_task":
				action, _ := tc.Args["Action"].(string)
				if strings.EqualFold(strings.Trim(strings.TrimSpace(action), "\""), "kill") {
					rawTID, _ := tc.Args["TaskId"].(string)
					if tid := strings.Trim(strings.TrimSpace(rawTID), "\""); tid != "" {
						if !killedSet[tid] {
							killedSet[tid] = true
							fd.KilledTaskIDs = append(fd.KilledTaskIDs, tid)
							changed = true
						}
					}
				}
			case "schedule":
				rawCron, _ := tc.Args["CronExpression"].(string)
				rawPrompt, _ := tc.Args["Prompt"].(string)
				cronExpr := strings.Trim(strings.TrimSpace(rawCron), "\"")
				prompt := strings.Trim(strings.TrimSpace(rawPrompt), "\"")
				if cronExpr == "" && prompt == "" {
					continue
				}
				fallbackCalls = append(fallbackCalls, transcriptCronItem{
					cronExpr: cronExpr,
					prompt:   prompt,
					armedAt:  step.CreatedAt,
				})
			}
		}
		if tid, cExpr, pText, isCronLaunch := parseGenericCronLaunchContent(step.Content); isCronLaunch {
			launchedCrons = append(launchedCrons, transcriptCronItem{
				taskID:   tid,
				cronExpr: cExpr,
				prompt:   pText,
				armedAt:  step.CreatedAt,
			})
			launchedKeys[normalizeFiveFieldCronExpr(cExpr)+"\x00"+pText] = true
		}
	}

	if fd.ArmedTriggers == nil {
		fd.ArmedTriggers = make(map[string]ArmedTrigger)
	}
	for id, at := range fd.ArmedTriggers {
		if (at.TaskID != "" && killedSet[at.TaskID]) || !knownSched[id] {
			delete(fd.ArmedTriggers, id)
			changed = true
		}
	}
	chronological := make([]transcriptCronItem, 0, len(launchedCrons)+len(fallbackCalls))
	chronological = append(chronological, launchedCrons...)
	for _, fc := range fallbackCalls {
		key := normalizeFiveFieldCronExpr(fc.cronExpr) + "\x00" + fc.prompt
		if launchedKeys[key] {
			continue
		}
		chronological = append(chronological, fc)
	}

	// Keep only the latest launch per (normalized cron, job title prefix) signature so pre-restart
	// timers wiped by an Antigravity server restart never resurrect after their post-restart replacement is killed.
	cronSig := func(cronExpr, prompt string) string {
		head := SanitizeTaskTitle(prompt, cronExpr)
		if before, _, hasColon := strings.Cut(head, ":"); hasColon && strings.TrimSpace(before) != "" {
			head = strings.TrimSpace(before)
		}
		return normalizeFiveFieldCronExpr(cronExpr) + "\x00" + strings.ToLower(head)
	}
	latestIdxBySig := make(map[string]int, len(chronological))
	for i, item := range chronological {
		latestIdxBySig[cronSig(item.cronExpr, item.prompt)] = i
	}
	activeItems := make([]transcriptCronItem, 0, len(latestIdxBySig))
	for i, item := range chronological {
		if latestIdxBySig[cronSig(item.cronExpr, item.prompt)] != i {
			continue
		}
		if item.taskID != "" && killedSet[item.taskID] {
			continue
		}
		activeItems = append(activeItems, item)
	}

	var rebuiltAdHoc []AdHocCron
	rebuiltAdHocSigs := make(map[string]bool)
	for _, item := range activeItems {
		armedAt := strings.TrimSpace(item.armedAt)
		if armedAt == "" {
			armedAt = time.Now().Format(time.RFC3339)
		}
		if matched, found := matchScheduleTrigger(schedules, item.cronExpr, item.prompt); found {
			prev, exists := fd.ArmedTriggers[matched.ID]
			nextTaskID := item.taskID
			if nextTaskID == "" {
				nextTaskID = prev.TaskID
			}
			if !exists || prev.TaskID != nextTaskID || prev.Cron != matched.FiveFieldCron() {
				fd.ArmedTriggers[matched.ID] = ArmedTrigger{
					ID:      matched.ID,
					TaskID:  nextTaskID,
					Cron:    matched.FiveFieldCron(),
					Skill:   matched.Skill,
					ArmedAt: armedAt,
				}
				changed = true
			}
		} else if item.cronExpr != "" {
			summary := SanitizeTaskTitle(item.prompt, item.cronExpr)
			rebuiltAdHoc, _ = upsertAdHocCron(rebuiltAdHoc, AdHocCron{
				TaskID:  item.taskID,
				Cron:    item.cronExpr,
				Summary: summary,
				ArmedAt: armedAt,
			})
			rebuiltAdHocSigs[cronSig(item.cronExpr, summary)] = true
		}
	}
	// Preserve any just-armed PreToolUse ad-hoc cron (TaskID == "") not yet written to transcript.jsonl.
	for _, ac := range fd.AdHocCrons {
		if ac.TaskID == "" {
			if !rebuiltAdHocSigs[cronSig(ac.Cron, ac.Summary)] {
				rebuiltAdHoc, _ = upsertAdHocCron(rebuiltAdHoc, ac)
			}
		}
	}
	if !slices.Equal(rebuiltAdHoc, fd.AdHocCrons) {
		fd.AdHocCrons = rebuiltAdHoc
		changed = true
	}

	if changed {
		fd.UpdatedAt = time.Now().Format(time.RFC3339)
		_ = saveFrontDeskLease(vaultDir, fd)
		if fdInfoAfter, err := os.Stat(frontDeskPath(vaultDir)); err == nil {
			curStamp.fdSize = fdInfoAfter.Size()
			curStamp.fdModNano = fdInfoAfter.ModTime().UnixNano()
		}
	}
	fdReconcileMu.Lock()
	fdReconcileCache[cacheKey] = curStamp
	fdReconcileMu.Unlock()
	return changed
}

// GetUnarmedTriggers compares effective schedules (core + packs + extensions.json overrides) against the active Front Desk's ArmedTriggers
// (or active background daemon ownership when `hq daemon` is running)
// and returns the armed and unarmed ScheduleTrigger slices for enabled schedules.
func GetUnarmedTriggers(vaultDir string) (armed []ScheduleTrigger, unarmed []ScheduleTrigger) {
	cfg, err := LoadEffectiveTriggers(vaultDir)
	if err != nil && (cfg == nil || len(cfg.Schedules) == 0) {
		return nil, nil
	}
	if cfg == nil {
		return nil, nil
	}
	daemonActive := IsDaemonActiveFast(vaultDir)
	fd, ok := GetFrontDesk(vaultDir)
	for _, s := range cfg.Schedules {
		if !s.IsEnabled() {
			continue
		}
		if daemonActive {
			armed = append(armed, s)
			continue
		}
		if ok && fd.ArmedTriggers != nil {
			if _, isArmed := fd.ArmedTriggers[s.ID]; isArmed {
				armed = append(armed, s)
				continue
			}
			if _, isArmedFQ := fd.ArmedTriggers[s.FQID()]; isArmedFQ {
				armed = append(armed, s)
				continue
			}
		}
		unarmed = append(unarmed, s)
	}
	return armed, unarmed
}

// GetDisarmTriggers returns armed Front Desk harness schedules that are either paused via
// <vault>/extensions.json (`Reason: "paused"`) OR superseded because `hq daemon` is active
// (`Reason: "migrated_to_daemon"`), so SessionStart can instruct the Front Desk coordinator to
// cancel the duplicate timer via manage_task(Action="kill"). Ad-hoc user crons are never included.
func GetDisarmTriggers(vaultDir string) []ArmedTrigger {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || len(fd.ArmedTriggers) == 0 {
		return nil
	}
	cfg, _ := LoadEffectiveTriggers(vaultDir)
	if cfg == nil {
		return nil
	}
	daemonActive := IsDaemonActiveFast(vaultDir)
	disabled := make(map[string]bool)
	for _, s := range cfg.Schedules {
		if !s.IsEnabled() {
			disabled[s.ID] = true
			disabled[s.FQID()] = true
		}
	}
	var out []ArmedTrigger
	for k, at := range fd.ArmedTriggers {
		if disabled[k] || disabled[at.ID] {
			at.Reason = "paused"
			out = append(out, at)
		} else if daemonActive {
			at.Reason = "migrated_to_daemon"
			out = append(out, at)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ClearArmedTrigger removes a paused/disarmed trigger record from front_desk.json if present.
func ClearArmedTrigger(vaultDir, scheduleID string) error {
	fd, ok := GetFrontDesk(vaultDir)
	if !ok || len(fd.ArmedTriggers) == 0 {
		return nil
	}
	fqid := ScheduleTrigger{ID: scheduleID}.FQID()
	bare := strings.TrimPrefix(fqid, "core/")
	changed := false
	if _, exists := fd.ArmedTriggers[scheduleID]; exists {
		delete(fd.ArmedTriggers, scheduleID)
		changed = true
	}
	if _, exists := fd.ArmedTriggers[fqid]; exists {
		delete(fd.ArmedTriggers, fqid)
		changed = true
	}
	if _, exists := fd.ArmedTriggers[bare]; exists {
		delete(fd.ArmedTriggers, bare)
		changed = true
	}
	if !changed {
		return nil
	}
	fd.UpdatedAt = time.Now().Format(time.RFC3339)
	return saveFrontDeskLease(vaultDir, fd)
}

// ReleaseFrontDesk clears local/memory/front_desk.json so the next new conversation
// automatically claims the Front Desk intake lease on SessionStart.
func ReleaseFrontDesk(vaultDir string) error {
	if prev, ok := GetFrontDesk(vaultDir); ok {
		if strings.TrimSpace(prev.ConversationID) != "" {
			recordHistoricalFrontDeskID(vaultDir, prev.ConversationID)
		}
		removeConsoleArtifacts(prev.ArtifactDir)
	}
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
	_ = os.Remove(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt"))
	_ = os.Remove(filepath.Join(vaultDir, "memory", "front_desk_turn.json"))
	_ = store.WriteAtomic(filepath.Join(vaultDir, "memory", ".vacated"), []byte(time.Now().Format(time.RFC3339)), 0o644)
	err := os.Remove(frontDeskPath(vaultDir))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
