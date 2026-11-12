package rsi

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hooks"
	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/outbox"
)

// FrictionCategory classifies the type of agentic friction observed in ground-truth telemetry.
type FrictionCategory string

const (
	FrictionErrorRecovery    FrictionCategory = "error_recovery_chain"
	FrictionPreTool          FrictionCategory = "pretool_friction"
	FrictionUserIntervention FrictionCategory = "user_intervention"
	FrictionDraftEdited      FrictionCategory = "draft_edited"
	FrictionRepeatedWorkflow FrictionCategory = "repeated_workflow"
	FrictionQuotaAnomaly     FrictionCategory = "quota_anomaly"
	FrictionProactiveNoise   FrictionCategory = "proactive_noise"
)

// AttributionFaultLayer identifies where in a multi-agent hierarchy a failure originated (Who&When).
type AttributionFaultLayer string

const (
	FaultWorkerACIOrSkill AttributionFaultLayer = "worker_aci_or_skill"
	FaultDelegationPrompt AttributionFaultLayer = "delegation_prompt"
	FaultModelRouting     AttributionFaultLayer = "model_routing"
)

// FrictionEvent is an immutable ground-truth observation persisted in local/memory/rsi/observations.jsonl.
type FrictionEvent struct {
	ID              string                `json:"id"`
	Fingerprint     string                `json:"fingerprint"`
	Category        FrictionCategory      `json:"category"`
	FaultLayer      AttributionFaultLayer `json:"faultLayer"`
	SkillOrPkg      string                `json:"skillOrPkg"`
	ToolName        string                `json:"toolName"`
	CommandPattern  string                `json:"commandPattern,omitempty"`
	ErrorExcerpt    string                `json:"errorExcerpt"`
	RecoveryAction  string                `json:"recoveryAction,omitempty"`
	WastedSteps     int                   `json:"wastedSteps"`
	OccurrenceCount int                   `json:"occurrenceCount"`
	ConversationIDs []string              `json:"conversationIds"`
	ParentConvID    string                `json:"parentConversationId,omitempty"`
	ModelName       string                `json:"modelName,omitempty"`
	FirstSeenAt     string                `json:"firstSeenAt"`
	LastSeenAt      string                `json:"lastSeenAt"`
	Tainted         bool                  `json:"tainted"`
	Status          string                `json:"status"` // "open", "hypothesized", "applied", "ignored"
}

// ObservationsPath returns local/memory/rsi/observations.jsonl.
func ObservationsPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "rsi", "observations.jsonl")
}

// LoadObservations reads all FrictionEvent records from local/memory/rsi/observations.jsonl.
func LoadObservations(vaultDir string) ([]FrictionEvent, error) {
	p := ObservationsPath(vaultDir)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []FrictionEvent
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var ev FrictionEvent
		if err := json.Unmarshal([]byte(line), &ev); err == nil && ev.ID != "" {
			out = append(out, ev)
		}
	}
	return out, scanner.Err()
}

// SaveObservations atomically writes the canonical FrictionEvent slice to local/memory/rsi/observations.jsonl.
func SaveObservations(vaultDir string, events []FrictionEvent) error {
	p := ObservationsPath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	var sb strings.Builder
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return os.WriteFile(p, []byte(sb.String()), 0o644)
}

func computeFingerprint(cat FrictionCategory, skillOrPkg, toolName, normalizedSig string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s", cat, skillOrPkg, toolName, strings.ToLower(strings.TrimSpace(normalizedSig)))))
	return hex.EncodeToString(h[:])[:16]
}

func inferSkillOrPkg(cmdOrContent string, filesModified []string) string {
	lower := strings.ToLower(cmdOrContent)
	switch {
	case strings.Contains(lower, "gchat"):
		return "gchat"
	case strings.Contains(lower, "gmail"):
		return "gmail"
	case strings.Contains(lower, "thesis") || strings.Contains(lower, "10-k") || strings.Contains(lower, "10-q"):
		return "thesis-check"
	case strings.Contains(lower, "filing-diff"):
		return "filing-diff"
	case strings.Contains(lower, "tax"):
		return "tax-prep"
	case strings.Contains(lower, "ledger") || strings.Contains(lower, "beancount"):
		return "ledger-ingest"
	case strings.Contains(lower, "sleep-consolidate") || strings.Contains(lower, "verify-transcript"):
		return "sleep-consolidate"
	case strings.Contains(lower, "morning-brief"):
		return "morning-brief"
	case strings.Contains(lower, "worktree") || strings.Contains(lower, "gh pr"):
		return "swe-pulse"
	}
	for _, fp := range filesModified {
		slash := filepath.ToSlash(fp)
		if strings.Contains(slash, "/skills/") {
			parts := strings.Split(slash, "/skills/")
			if len(parts) > 1 {
				seg := strings.Split(parts[1], "/")[0]
				if seg != "" {
					return seg
				}
			}
		}
		if strings.Contains(slash, "internal/hooks") {
			return "internal/hooks"
		}
		if strings.Contains(slash, "internal/kernel") {
			return "internal/kernel"
		}
	}
	return "swe-pulse"
}

func attributeFaultLayer(parentConvID, modelName string, firstFailStepIdx int, wastedSteps int, errorExcerpt string) AttributionFaultLayer {
	lowerErr := strings.ToLower(errorExcerpt)
	if parentConvID != "" && firstFailStepIdx <= 2 &&
		(strings.Contains(lowerErr, "no such file or directory") ||
			strings.Contains(lowerErr, "file does not exist") ||
			strings.Contains(lowerErr, "not a git repository") ||
			strings.Contains(lowerErr, "missing required")) {
		return FaultDelegationPrompt
	}
	lowerModel := strings.ToLower(modelName)
	if wastedSteps >= 3 && (strings.Contains(lowerModel, "flash-low") || strings.Contains(lowerModel, "flash_lite") || strings.Contains(lowerModel, "lite")) {
		return FaultModelRouting
	}
	return FaultWorkerACIOrSkill
}

// Mine executes Stage 1 (0-token deterministic trace & intervention mining) across journals,
// transcripts, user interventions, and edited drafts, persisting deduplicated FrictionEvents
// to local/memory/rsi/observations.jsonl and updating active canary records in archive.jsonl.
func Mine(rootDir, vaultDir string, since time.Duration) ([]FrictionEvent, error) {
	existing, _ := LoadObservations(vaultDir)
	priorByFP := make(map[string]FrictionEvent, len(existing))
	byFP := make(map[string]*FrictionEvent, len(existing))
	var order []string
	for i := range existing {
		ev := existing[i]
		priorByFP[ev.Fingerprint] = ev
		if since > 0 || ev.Status != "open" {
			evCopy := ev
			byFP[ev.Fingerprint] = &evCopy
			order = append(order, ev.Fingerprint)
		}
	}

	now := time.Now().UTC()
	cutoff := time.Time{}
	if since > 0 {
		cutoff = now.Add(-since)
	}

	tasksByConv := make(map[string]kernel.ActiveTask)
	if tasks, err := kernel.ListTasks(vaultDir); err == nil {
		for _, t := range tasks {
			if t.ConversationID != "" {
				tasksByConv[t.ConversationID] = t
			}
		}
	}

	upsert := func(cat FrictionCategory, fault AttributionFaultLayer, skillOrPkg, toolName, cmdPat, errExcerpt, recovery string, wasted int, convID, parentConv, modelName, ts string, tainted bool) {
		if ts == "" {
			ts = now.Format(time.RFC3339)
		}
		if wasted <= 0 {
			wasted = 1
		}
		sig := cmdPat
		if sig == "" {
			sig = errExcerpt
		}
		if len(sig) > 120 {
			sig = sig[:120]
		}
		fp := computeFingerprint(cat, skillOrPkg, toolName, sig)
		if cur, ok := byFP[fp]; ok {
			alreadyHasConv := false
			for _, c := range cur.ConversationIDs {
				if c == convID && convID != "" {
					alreadyHasConv = true
					break
				}
			}
			if !alreadyHasConv && convID != "" {
				cur.ConversationIDs = append(cur.ConversationIDs, convID)
				cur.OccurrenceCount++
			} else if convID == "" {
				cur.OccurrenceCount++
			}
			if wasted > cur.WastedSteps {
				cur.WastedSteps = wasted
			}
			if recovery != "" && cur.RecoveryAction == "" {
				cur.RecoveryAction = recovery
			}
			if tainted {
				cur.Tainted = true
			}
			cur.LastSeenAt = ts
			return
		}

		id := fmt.Sprintf("obs-%s-%s", now.Format("20060102"), fp[:8])
		firstSeen := ts
		status := "open"
		if prev, hadPrev := priorByFP[fp]; hadPrev {
			id = prev.ID
			if prev.FirstSeenAt != "" {
				firstSeen = prev.FirstSeenAt
			}
			if prev.Status != "" {
				status = prev.Status
			}
		}
		var convs []string
		if convID != "" {
			convs = []string{convID}
		}
		ev := &FrictionEvent{
			ID:              id,
			Fingerprint:     fp,
			Category:        cat,
			FaultLayer:      fault,
			SkillOrPkg:      skillOrPkg,
			ToolName:        toolName,
			CommandPattern:  cmdPat,
			ErrorExcerpt:    errExcerpt,
			RecoveryAction:  recovery,
			WastedSteps:     wasted,
			OccurrenceCount: 1,
			ConversationIDs: convs,
			ParentConvID:    parentConv,
			ModelName:       modelName,
			FirstSeenAt:     firstSeen,
			LastSeenAt:      ts,
			Tainted:         tainted,
			Status:          status,
		}
		byFP[fp] = ev
		order = append(order, fp)
	}

	// 1. Scan turn journals in local/memory/journal/*/*.json and their linked transcripts
	journalRoot := filepath.Join(vaultDir, "memory", "journal")
	var observedCleanSkills = make(map[string]int)
	var observedFingerprintsThisRun = make(map[string]bool)

	if dateDirs, err := os.ReadDir(journalRoot); err == nil {
		for _, d := range dateDirs {
			if !d.IsDir() {
				continue
			}
			files, _ := os.ReadDir(filepath.Join(journalRoot, d.Name()))
			sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
			for _, f := range files {
				if !strings.HasSuffix(f.Name(), ".json") {
					continue
				}
				raw, err := os.ReadFile(filepath.Join(journalRoot, d.Name(), f.Name()))
				if err != nil {
					continue
				}
				var je hooks.JournalEntry
				if err := json.Unmarshal(raw, &je); err != nil {
					continue
				}
				if !cutoff.IsZero() && je.Timestamp != "" {
					if t, err := time.Parse(time.RFC3339, je.Timestamp); err == nil && t.Before(cutoff) {
						continue
					}
				}
				parentConv := je.ParentConvID
				if parentConv == "" {
					if t, ok := tasksByConv[je.ConversationID]; ok {
						parentConv = t.ParentConversationID
					}
				}
				skillOrPkg := inferSkillOrPkg(je.UserPrompt+" "+je.ResponseSummary, je.FilesModified)
				if je.TurnErrorCount == 0 && je.ToolErrorCount == 0 {
					observedCleanSkills[skillOrPkg]++
				}

				if je.TranscriptPath != "" {
					mineTranscript(je, parentConv, skillOrPkg, upsert, observedFingerprintsThisRun)
				}
			}
		}
	}

	// 2. Scan local/memory/interventions.jsonl for explicit user feedback & proposal rejections
	interventionsFile := filepath.Join(vaultDir, "memory", "interventions.jsonl")
	if f, err := os.Open(interventionsFile); err == nil {
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var item struct {
				Timestamp  string `json:"timestamp"`
				Type       string `json:"type"`
				Category   string `json:"category"`
				Lesson     string `json:"lesson"`
				Decision   string `json:"decision"`
				ProposalID string `json:"proposalId"`
				ActionType string `json:"actionType"`
				Summary    string `json:"summary"`
				Command    string `json:"command"`
				Reason     string `json:"reason"`
				Proposal   struct {
					ID         string `json:"id"`
					ActionType string `json:"actionType"`
					Summary    string `json:"summary"`
					Command    string `json:"command"`
				} `json:"proposal"`
			}
			if json.Unmarshal([]byte(line), &item) != nil {
				continue
			}
			if item.Type == "user_feedback" && (item.Summary != "" || item.Lesson != "") {
				skill := inferSkillOrPkg(item.Category+" "+item.Summary+" "+item.Lesson, nil)
				upsert(
					FrictionUserIntervention,
					FaultWorkerACIOrSkill,
					skill,
					"user_feedback",
					item.Category,
					item.Summary,
					item.Lesson,
					2,
					"feedback-"+item.Timestamp,
					"",
					"",
					item.Timestamp,
					false,
				)
				continue
			}
			propID := item.ProposalID
			actType := item.ActionType
			summary := item.Summary
			cmd := item.Command
			if propID == "" && item.Proposal.ID != "" {
				propID = item.Proposal.ID
				actType = item.Proposal.ActionType
				summary = item.Proposal.Summary
				cmd = item.Proposal.Command
			}
			if propID != "" && (item.Decision == "rejected" || item.Reason != "") {
				skill := inferSkillOrPkg(actType+" "+cmd+" "+summary, nil)
				upsert(
					FrictionUserIntervention,
					FaultWorkerACIOrSkill,
					skill,
					"hq_propose",
					cmd,
					fmt.Sprintf("Rejected proposal (%s): %s", actType, item.Reason),
					fmt.Sprintf("Honor user rejection feedback: %s", item.Reason),
					2,
					propID,
					"",
					"",
					item.Timestamp,
					false,
				)
			}
		}
		_ = f.Close()
	}

	// 3. Scan local/private/{gmail,gchat}/drafts/*.json for PRELUDE implicit draft edits
	for _, channel := range []string{"gmail", "gchat"} {
		draftsDir := filepath.Join(vaultDir, "private", channel, "drafts")
		entries, err := os.ReadDir(draftsDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(draftsDir, e.Name()))
			if err != nil {
				continue
			}
			var d struct {
				DraftID        string `json:"draftId"`
				Body           string `json:"body"`
				OriginalBody   string `json:"originalBody,omitempty"`
				UserEditedBody string `json:"userEditedBody,omitempty"`
				UpdatedAt      string `json:"updatedAt,omitempty"`
				ConversationID string `json:"conversationId,omitempty"`
			}
			if json.Unmarshal(raw, &d) == nil && d.DraftID != "" {
				edited := strings.TrimSpace(d.UserEditedBody)
				orig := strings.TrimSpace(d.OriginalBody)
				cur := strings.TrimSpace(d.Body)
				if (edited != "" && edited != cur) || (orig != "" && orig != cur) {
					afterBody := cur
					beforeBody := orig
					if edited != "" {
						afterBody = edited
						if beforeBody == "" {
							beforeBody = cur
						}
					}
					diffNote := summarizeDraftEditDiff(beforeBody, afterBody)
					upsert(
						FrictionDraftEdited,
						FaultWorkerACIOrSkill,
						channel,
						"hq_"+channel+"_draft",
						"draft:"+d.DraftID,
						fmt.Sprintf("User edited %s draft %s before sending (%s)", channel, d.DraftID, diffNote),
						fmt.Sprintf("Apply learned communication preference in %s drafts: %s", channel, diffNote),
						2,
						d.ConversationID,
						"",
						"",
						d.UpdatedAt,
						false, // Draft edits are explicit user corrections on outbound text
					)
				}
			}
		}
	}

	// 3b. Scan token anomalies (A3 RunawayTurnBurn & A4 TierMisroutingBurn) as FrictionQuotaAnomaly
	if obsIdx, err := obs.LoadOrRefreshIndex(vaultDir); err == nil && obsIdx != nil {
		budget := obs.LoadBudgetConfig(vaultDir)
		for _, anom := range obs.EvaluateAnomalies(vaultDir, obsIdx, budget, now) {
			if anom.Code != "A3" && anom.Code != "A4" {
				continue
			}
			if !cutoff.IsZero() && anom.Timestamp != "" {
				if ts, tErr := time.Parse(time.RFC3339, anom.Timestamp); tErr == nil && ts.Before(cutoff) {
					continue
				}
			}
			cm := obsIdx.Conversations[anom.ConversationID]
			upsert(
				FrictionQuotaAnomaly,
				FaultModelRouting,
				"swe-pulse",
				anom.Kind,
				anom.Code+":"+anom.TaskSlug,
				anom.Summary,
				anom.Remediation,
				3,
				anom.ConversationID,
				cm.ParentConversationID,
				cm.Model,
				anom.Timestamp,
				false,
			)
		}
	}

	// 3c. Scan outbox ideas_feed dismissal rates (>70% across >=5 resolved ideas) as FrictionProactiveNoise (RFC-0014)
	if stats, err := outbox.ComputeSkillDismissalStats(vaultDir); err == nil {
		for _, st := range stats {
			if st.TotalResolved < 5 || st.DismissRate <= 0.70 {
				continue
			}
			reasonsSuffix := ""
			if st.SampleReasons != "" {
				reasonsSuffix = fmt.Sprintf(" (user reasons: %s)", st.SampleReasons)
			}
			upsert(
				FrictionProactiveNoise,
				FaultWorkerACIOrSkill,
				st.Skill,
				"hq_outbox",
				"ideas_feed:"+st.Skill,
				fmt.Sprintf("Skill %s has %.0f%% proactive idea dismissal rate (%d/%d dismissed)%s", st.Skill, st.DismissRate*100.0, st.DismissedCount, st.TotalResolved, reasonsSuffix),
				fmt.Sprintf("Tighten proactive idea generation threshold in %s/SKILL.md so low-signal suggestions are suppressed", st.Skill),
				st.DismissedCount,
				"outbox-"+st.Skill,
				"",
				"",
				st.LastSeenAt,
				false,
			)
		}
	}

	var result []FrictionEvent
	for _, fp := range order {
		if ev, ok := byFP[fp]; ok {
			result = append(result, *ev)
		}
	}
	if err := SaveObservations(vaultDir, result); err != nil {
		return result, err
	}

	// 4. Update active Canary records in local/memory/rsi/archive.jsonl
	_ = updateCanaryRecords(vaultDir, observedCleanSkills, observedFingerprintsThisRun)

	return result, nil
}

func summarizeDraftEditDiff(before, after string) string {
	if len(after) < len(before) && (strings.Contains(before, "Best regards") || strings.Contains(before, "Sincerely")) && !strings.Contains(after, "Best regards") {
		return "User removed formal sign-off ('Best regards'/'Sincerely') in favor of direct concise tone"
	}
	if strings.Count(after, "- ") > strings.Count(before, "- ") {
		return "User reformatted prose paragraph into concise bullet points"
	}
	return fmt.Sprintf("User refined draft phrasing (%d chars -> %d chars)", len(before), len(after))
}

func mineTranscript(
	je hooks.JournalEntry,
	parentConv string,
	defaultSkill string,
	upsert func(cat FrictionCategory, fault AttributionFaultLayer, skillOrPkg, toolName, cmdPat, errExcerpt, recovery string, wasted int, convID, parentConv, modelName, ts string, tainted bool),
	observedFPs map[string]bool,
) {
	f, err := os.Open(je.TranscriptPath)
	if err != nil {
		return
	}
	defer f.Close()

	type parsedStep struct {
		StepIndex int
		Type      string
		Status    string
		Content   string
		ToolName  string
		CmdLine   string
		IsError   bool
		Transient bool
		IsDenial  bool
	}

	var steps []parsedStep
	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 2*1024*1024)

	lastToolName := ""
	lastCmdLine := ""
	lastPlannerHadTools := false
	lastPlannerHadRunCmd := false
	tainted := je.TaintedSession

	for scanner.Scan() {
		var raw struct {
			StepIndex int    `json:"step_index"`
			Type      string `json:"type"`
			Status    string `json:"status"`
			Content   string `json:"content"`
			ToolCalls []struct {
				Name string                 `json:"name"`
				Args map[string]interface{} `json:"args"`
			} `json:"tool_calls"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &raw); err != nil {
			continue
		}
		if raw.Type == "PLANNER_RESPONSE" {
			lastPlannerHadTools = len(raw.ToolCalls) > 0
			lastPlannerHadRunCmd = false
			lastToolName = ""
			lastCmdLine = ""
			for _, tc := range raw.ToolCalls {
				lastToolName = tc.Name
				if tc.Name == "run_command" {
					lastPlannerHadRunCmd = true
					if c, ok := tc.Args["CommandLine"].(string); ok {
						lastCmdLine = strings.TrimSpace(c)
						if strings.HasPrefix(lastCmdLine, "./bin/hq gmail read-quarantined") ||
							strings.HasPrefix(lastCmdLine, "./bin/hq gchat read-quarantined") ||
							strings.HasPrefix(lastCmdLine, "bin/hq gmail read-quarantined") ||
							strings.HasPrefix(lastCmdLine, "bin/hq gchat read-quarantined") {
							tainted = true
						}
					}
				}
			}
		}

		isFileViewerOutput := strings.HasPrefix(raw.Content, "Showing lines ") || strings.Contains(raw.Content, "\nShowing lines ")
		isReadViewer := isFileViewerOutput || (lastPlannerHadTools && !lastPlannerHadRunCmd)
		isCmdExitErr := false
		if !isReadViewer {
			for _, line := range strings.Split(raw.Content, "\n") {
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "Command exited with code ") {
					rest := strings.TrimSpace(strings.TrimPrefix(trimmed, "Command exited with code "))
					if len(rest) > 0 && rest[0] >= '1' && rest[0] <= '9' {
						isCmdExitErr = true
						break
					}
				}
			}
		}
		isErr := strings.EqualFold(strings.TrimSpace(raw.Status), "ERROR") ||
			(!isFileViewerOutput && (strings.Contains(raw.Content, "Encountered error in tool execution:") || strings.Contains(raw.Content, "Encountered error in step execution:"))) ||
			isCmdExitErr

		lower := strings.ToLower(raw.Content)
		isTransient := strings.Contains(lower, "429 too many requests") ||
			strings.Contains(lower, "status 429") ||
			strings.Contains(lower, "502 bad gateway") ||
			strings.Contains(lower, "503 service unavailable") ||
			strings.Contains(lower, "504 gateway timeout") ||
			strings.Contains(lower, "etimedout") ||
			strings.Contains(lower, "econnreset")

		isDenial := !isReadViewer && strings.Contains(raw.Content, "Blocked by agy-hq") &&
			(strings.EqualFold(strings.TrimSpace(raw.Status), "ERROR") ||
				strings.Contains(raw.Content, "Encountered error") ||
				strings.HasPrefix(strings.TrimSpace(raw.Content), "Blocked by agy-hq") ||
				strings.Contains(raw.Content, `"decision":"deny"`))

		steps = append(steps, parsedStep{
			StepIndex: raw.StepIndex,
			Type:      raw.Type,
			Status:    raw.Status,
			Content:   raw.Content,
			ToolName:  lastToolName,
			CmdLine:   lastCmdLine,
			IsError:   isErr,
			Transient: isTransient,
			IsDenial:  isDenial,
		})
	}

	// Count transient errors; skip single transient errors unless >= 3
	transientCount := 0
	for _, s := range steps {
		if s.Transient {
			transientCount++
		}
	}

	for i, s := range steps {
		if s.IsDenial {
			tool := s.ToolName
			if tool == "" {
				tool = "run_command"
			}
			skill := inferSkillOrPkg(s.CmdLine+" "+s.Content, je.FilesModified)
			excerpt := firstLineExcerpt(s.Content, 180)
			fp := computeFingerprint(FrictionPreTool, skill, tool, s.CmdLine)
			observedFPs[fp] = true
			upsert(
				FrictionPreTool,
				FaultWorkerACIOrSkill,
				skill,
				tool,
				s.CmdLine,
				excerpt,
				"Stage outbound or restricted command via ./bin/hq propose or delegate to background subagent",
				1,
				je.ConversationID,
				parentConv,
				je.ModelName,
				je.Timestamp,
				tainted,
			)
			continue
		}

		if !s.IsError {
			continue
		}
		if s.Transient && transientCount < 3 {
			continue
		}

		// Look ahead for recovery step
		recovery := ""
		wasted := 1
		for j := i + 1; j < len(steps); j++ {
			if steps[j].IsError {
				wasted++
				continue
			}
			if steps[j].CmdLine != "" && steps[j].CmdLine != s.CmdLine {
				recovery = steps[j].CmdLine
				break
			}
			if steps[j].Type == "PLANNER_RESPONSE" && strings.TrimSpace(steps[j].Content) != "" {
				recovery = firstLineExcerpt(steps[j].Content, 160)
				break
			}
		}

		tool := s.ToolName
		if tool == "" {
			tool = "run_command"
		}
		skill := inferSkillOrPkg(s.CmdLine+" "+s.Content, je.FilesModified)
		if skill == "" {
			skill = defaultSkill
		}
		excerpt := firstLineExcerpt(s.Content, 180)
		fault := attributeFaultLayer(parentConv, je.ModelName, i, wasted, excerpt)
		sig := s.CmdLine
		if sig == "" {
			sig = excerpt
		}
		fp := computeFingerprint(FrictionErrorRecovery, skill, tool, sig)
		observedFPs[fp] = true
		upsert(
			FrictionErrorRecovery,
			fault,
			skill,
			tool,
			s.CmdLine,
			excerpt,
			recovery,
			wasted,
			je.ConversationID,
			parentConv,
			je.ModelName,
			je.Timestamp,
			tainted,
		)
	}
}

func firstLineExcerpt(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, "Command exited with code "); idx >= 0 {
		rest := strings.TrimSpace(s[idx+len("Command exited with code "):])
		if len(rest) > 0 && rest[0] >= '1' && rest[0] <= '9' {
			s = strings.TrimSpace(s[idx:])
		}
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if runes := []rune(s); len(runes) > maxLen {
		return string(runes[:maxLen]) + "..."
	}
	return s
}
