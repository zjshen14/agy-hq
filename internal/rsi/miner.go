package rsi

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/kernel"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/transcript"
	"github.com/zjshen14/agy-hq/internal/vault"
)

// FrictionCategory classifies the type of agentic friction observed in ground-truth telemetry.
type FrictionCategory string

const (
	FrictionErrorRecovery    FrictionCategory = "error_recovery_chain"
	FrictionPreTool          FrictionCategory = "pretool_friction"
	FrictionUserIntervention FrictionCategory = "user_intervention"
	FrictionDraftEdited      FrictionCategory = "draft_edited"
	FrictionQuotaAnomaly     FrictionCategory = "quota_anomaly"
	FrictionProactiveNoise   FrictionCategory = "proactive_noise"
	FrictionWorkflowBloat    FrictionCategory = "workflow_bloat"
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
	ID               string                `json:"id"`
	Fingerprint      string                `json:"fingerprint"`
	Category         FrictionCategory      `json:"category"`
	FaultLayer       AttributionFaultLayer `json:"faultLayer"`
	FailureClass     string                `json:"failureClass,omitempty"`    // "process" | "content"
	TargetSubstrate  string                `json:"targetSubstrate,omitempty"` // "ring1_cli_triage" | "compile_script_or_route" | "ring2_skill_or_overlay" | "ring3_memory"
	Origin           string                `json:"origin,omitempty"`          // "auto_mined" | "user_correction"
	SkillOrPkg       string                `json:"skillOrPkg"`
	ToolName         string                `json:"toolName"`
	CommandPattern   string                `json:"commandPattern,omitempty"`
	ErrorExcerpt     string                `json:"errorExcerpt"`
	RecoveryAction   string                `json:"recoveryAction,omitempty"`
	WastedSteps      int                   `json:"wastedSteps"`
	MeanWastedTokens int64                 `json:"meanWastedTokens,omitempty"`
	ImpactScore      int64                 `json:"impactScore,omitempty"`
	OccurrenceCount  int                   `json:"occurrenceCount"`
	ConversationIDs  []string              `json:"conversationIds"`
	ParentConvID     string                `json:"parentConversationId,omitempty"`
	ModelName        string                `json:"modelName,omitempty"`
	FirstSeenAt      string                `json:"firstSeenAt"`
	LastSeenAt       string                `json:"lastSeenAt"`
	Tainted          bool                  `json:"tainted"`
	Status           string                `json:"status"` // "open", "hypothesized", "applied", "ignored"
}

func withRSILock(vaultDir string, fn func() error) error {
	memDir := filepath.Join(vaultDir, "memory")
	if err := os.MkdirAll(memDir, 0o700); err != nil {
		return err
	}
	return store.With(context.Background(), store.RankMemory, filepath.Join(memDir, ".memory.lock"), func(context.Context) error {
		return fn()
	})
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

// SaveObservations atomically writes the canonical FrictionEvent slice to local/memory/rsi/observations.jsonl
// under store.RankMemory with 0600 permissions.
func SaveObservations(vaultDir string, events []FrictionEvent) error {
	p := ObservationsPath(vaultDir)
	var sb strings.Builder
	for _, ev := range events {
		b, err := json.Marshal(ev)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	data := []byte(sb.String())
	return withRSILock(vaultDir, func() error {
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		return store.WriteAtomic(p, data, 0o600)
	})
}

var (
	reNormRFC3339  = regexp.MustCompile(`\b20\d{2}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})?\b`)
	reNormUUID     = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reNormFlagID   = regexp.MustCompile(`(--(?:thread|id|conv|proposal|sha256|expect-diff-sha256)=)[^\s"']+`)
	reNormPrefixed = regexp.MustCompile(`\b(?:prop|obs|rsi|conv|task|msg|thread|draft)-[A-Za-z0-9_-]{4,}\b`)
	reNormLongHex  = regexp.MustCompile(`\b[0-9a-fA-F]{10,}\b`)
	reNormHomePath = regexp.MustCompile(`(/Users/|/home/|C:\\Users\\)[A-Za-z0-9._\-]+(?:/[^\s"']*)?`)
	reNormTmpPath  = regexp.MustCompile(`/tmp/[^\s"']+`)
	reNormLineNum  = regexp.MustCompile(`:\d+(?::\d+)?\b`)
	reNormMultiSpc = regexp.MustCompile(`\s+`)
)

// NormalizeForFingerprint strips volatile timestamps, UUIDs, thread/proposal IDs, and local paths
// so identical recurring root causes collapse into one canonical fingerprint (RFC-0029 D12).
func NormalizeForFingerprint(s string) string {
	s = transcript.StripTimestampPrefix(s)
	s = reNormRFC3339.ReplaceAllString(s, "<ts>")
	s = reNormUUID.ReplaceAllString(s, "<uuid>")
	s = reNormFlagID.ReplaceAllString(s, "$1<id>")
	s = reNormPrefixed.ReplaceAllString(s, "<id>")
	s = reNormLongHex.ReplaceAllString(s, "<hex>")
	s = reNormHomePath.ReplaceAllString(s, "<path>")
	s = reNormTmpPath.ReplaceAllString(s, "<tmp>")
	s = reNormLineNum.ReplaceAllString(s, ":<n>")
	s = reNormMultiSpc.ReplaceAllString(strings.TrimSpace(s), " ")
	if len(s) > 120 {
		s = s[:120]
	}
	return strings.ToLower(s)
}

func computeFingerprint(cat FrictionCategory, skillOrPkg, toolName, normalizedSig string) string {
	norm := NormalizeForFingerprint(normalizedSig)
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s", cat, skillOrPkg, toolName, norm)))
	return hex.EncodeToString(h[:])[:16]
}

func computeImpactScore(occ, wasted int, meanTokens int64) int64 {
	if occ <= 0 {
		occ = 1
	}
	if wasted <= 0 {
		wasted = 1
	}
	stepScore := int64(occ*wasted) * 100
	tokenScore := (meanTokens * int64(occ)) / 1000
	return stepScore + tokenScore
}

func isDevTDDOrBenignNoMatch(cmdLine, errExcerpt string) bool {
	c := strings.ToLower(strings.TrimSpace(cmdLine))
	e := strings.ToLower(strings.TrimSpace(errExcerpt))
	if c == "" {
		return false
	}
	// Strip leading cd ... && prefix when inspecting the actual command.
	lastSeg := c
	if idx := strings.LastIndex(c, "&&"); idx >= 0 {
		lastSeg = strings.TrimSpace(c[idx+2:])
	}
	// Normal engineering TDD / build / CI runs are expected to fail while iterating.
	for _, devPrefix := range []string{
		"go test", "go build", "go vet", "gofmt",
		"./bin/hq ci ", "bin/hq ci ", "hq ci ",
		"npm test", "pnpm test", "yarn test", "pytest", "cargo test", "cargo check", "make test",
	} {
		if strings.HasPrefix(lastSeg, devPrefix) || strings.HasPrefix(c, devPrefix) {
			return true
		}
	}
	// Benign grep/rg exit-1 no-match (not a usage/syntax/permission error).
	if strings.HasPrefix(lastSeg, "grep ") || strings.HasPrefix(lastSeg, "rg ") || strings.HasPrefix(lastSeg, "git grep ") {
		if strings.Contains(e, "command exited with code 1") &&
			!strings.Contains(e, "invalid option") &&
			!strings.Contains(e, "unrecognized option") &&
			!strings.Contains(e, "no such file") &&
			!strings.Contains(e, "permission denied") {
			return true
		}
	}
	return false
}

func isOwnedHQCommand(cmdLine string) bool {
	c := strings.ToLower(strings.TrimSpace(cmdLine))
	if idx := strings.LastIndex(c, "&&"); idx >= 0 {
		c = strings.TrimSpace(c[idx+2:])
	}
	return strings.HasPrefix(c, "hq ") || strings.HasPrefix(c, "./bin/hq ") || strings.HasPrefix(c, "bin/hq ")
}

func classifyFailureAndSubstrate(cat FrictionCategory, skillOrPkg, toolName, cmdLine, errExcerpt string) (string, string) {
	lowerErr := strings.ToLower(errExcerpt)
	lowerCmd := strings.ToLower(cmdLine)
	namedSkill := skillOrPkg != "" && skillOrPkg != "general" && !strings.HasPrefix(skillOrPkg, "internal/")

	switch cat {
	case FrictionQuotaAnomaly, FrictionWorkflowBloat:
		return "process", "compile_script_or_route"
	case FrictionDraftEdited, FrictionProactiveNoise:
		if namedSkill {
			return "content", "ring2_skill_or_overlay"
		}
		return "content", "ring3_memory"
	case FrictionUserIntervention:
		if namedSkill {
			return "content", "ring2_skill_or_overlay"
		}
		return "content", "ring3_memory"
	case FrictionPreTool:
		if namedSkill {
			return "process", "ring2_skill_or_overlay"
		}
		return "process", "ring3_memory"
	default: // FrictionErrorRecovery
		if strings.HasPrefix(skillOrPkg, "internal/") {
			return "process", "ring1_cli_triage"
		}
		if isOwnedHQCommand(cmdLine) {
			if strings.Contains(lowerErr, "panic:") || strings.Contains(lowerErr, "runtime error") || !namedSkill {
				return "process", "ring1_cli_triage"
			}
			return "process", "ring2_skill_or_overlay"
		}
		if namedSkill {
			return "process", "ring2_skill_or_overlay"
		}
		if strings.Contains(lowerCmd, "python") || strings.Contains(lowerCmd, "node ") || strings.Contains(lowerCmd, "curl ") || strings.Contains(lowerCmd, "jq ") {
			return "process", "compile_script_or_route"
		}
		return "process", "ring3_memory"
	}
}

func inferSkillOrPkg(cmdOrContent string, filesModified []string, vaultSkills ...[]string) string {
	lower := strings.ToLower(cmdOrContent)
	if len(vaultSkills) > 0 {
		for _, sk := range vaultSkills[0] {
			skLower := strings.ToLower(strings.TrimSpace(sk))
			if skLower == "" {
				continue
			}
			if strings.Contains(lower, "/"+skLower) ||
				strings.Contains(lower, "skills/"+skLower) ||
				strings.Contains(lower, " "+skLower+" ") ||
				strings.HasPrefix(lower, skLower+" ") ||
				strings.HasSuffix(lower, " "+skLower) ||
				lower == skLower {
				return sk
			}
		}
	}
	switch {
	case strings.Contains(lower, "gchat"):
		return "gchat"
	case strings.Contains(lower, "gmail"):
		return "gmail"
	case strings.Contains(lower, "gcalendar"):
		return "gcalendar"
	case strings.Contains(lower, "gdrive"):
		return "gdrive"
	case strings.Contains(lower, "gdocs"):
		return "gdocs"
	case strings.Contains(lower, "gsheets"):
		return "gsheets"
	case strings.Contains(lower, "gslides"):
		return "gslides"
	case strings.Contains(lower, "inbox-triage"):
		return "inbox-triage"
	case strings.Contains(lower, "thesis") || strings.Contains(lower, "10-k") || strings.Contains(lower, "10-q"):
		return "thesis-check"
	case strings.Contains(lower, "filing-diff"):
		return "filing-diff"
	case strings.Contains(lower, "tax-prep") || strings.Contains(lower, "skills/tax-prep"):
		return "tax-prep"
	case strings.Contains(lower, "ledger-ingest") || strings.Contains(lower, "beancount") || strings.Contains(lower, "hledger"):
		return "ledger-ingest"
	case strings.Contains(lower, "sleep-consolidate") || strings.Contains(lower, "verify-transcript"):
		return "sleep-consolidate"
	case strings.Contains(lower, "morning-brief") || strings.Contains(lower, "brief collect") || strings.Contains(lower, "brief commit"):
		return "morning-brief"
	case strings.Contains(lower, "worktree") || strings.Contains(lower, "gh pr") || strings.Contains(lower, "swe-pulse"):
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
	return "general"
}

func listInstalledVaultSkillSlugs(vaultDir string) []string {
	skillsDir := filepath.Join(vaultDir, "skills")
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if _, sErr := os.Stat(filepath.Join(skillsDir, e.Name(), "SKILL.md")); sErr == nil {
			out = append(out, e.Name())
		}
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i]) > len(out[j]) })
	return out
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

// MineSinceWatermark runs Mine starting from the last committed sleep-consolidate watermark
// (scanning all unarchived hot journals when no dream cursor exists yet).
func MineSinceWatermark(rootDir, vaultDir string) ([]FrictionEvent, error) {
	now := time.Now().UTC()
	cur := vault.LoadDreamCursor(vaultDir)
	var since time.Duration
	if cur.LastCommittedAt != "" {
		if ts, err := time.Parse(time.RFC3339, cur.LastCommittedAt); err == nil {
			if d := now.Sub(ts); d > 0 && d < 30*24*time.Hour {
				since = d
			}
		}
	}
	return Mine(rootDir, vaultDir, since)
}

// RecordCorrection silently persists an explicit user workflow correction as a high-priority
// FrictionEvent (`origin="user_correction"`, `distinctConvs >= 1` eligible for immediate Ring-2 synthesis).
func RecordCorrection(vaultDir, skillSlug, convID, summary, diagnosis string) (FrictionEvent, error) {
	skillSlug = strings.TrimSpace(skillSlug)
	summary = strings.TrimSpace(summary)
	diagnosis = strings.TrimSpace(diagnosis)
	if skillSlug == "" || skillSlug == "general" {
		return FrictionEvent{}, fmt.Errorf("a named skill slug is required for record-correction (e.g., --skill=gchat)")
	}
	if summary == "" {
		return FrictionEvent{}, fmt.Errorf("--summary is required for record-correction")
	}
	if NonOverridableQuarantineSkills[skillSlug] {
		return FrictionEvent{}, fmt.Errorf("quarantine skill %q cannot be mutated via RSI", skillSlug)
	}
	if diagnosis == "" {
		diagnosis = summary
	}

	now := time.Now().UTC()
	ts := now.Format(time.RFC3339)
	fp := computeFingerprint(FrictionUserIntervention, skillSlug, "user_correction", summary)
	ev := FrictionEvent{
		ID:              fmt.Sprintf("obs-%s-%s", now.Format("20060102"), fp[:8]),
		Fingerprint:     fp,
		Category:        FrictionUserIntervention,
		FaultLayer:      FaultWorkerACIOrSkill,
		FailureClass:    "content",
		TargetSubstrate: "ring2_skill_or_overlay",
		Origin:          "user_correction",
		SkillOrPkg:      skillSlug,
		ToolName:        "user_correction",
		CommandPattern:  "correction:" + skillSlug,
		ErrorExcerpt:    summary,
		RecoveryAction:  diagnosis,
		WastedSteps:     2,
		ImpactScore:     computeImpactScore(1, 2, 0),
		OccurrenceCount: 1,
		FirstSeenAt:     ts,
		LastSeenAt:      ts,
		Tainted:         false,
		Status:          "open",
	}
	if strings.TrimSpace(convID) != "" {
		ev.ConversationIDs = []string{strings.TrimSpace(convID)}
	}

	existing, err := LoadObservations(vaultDir)
	if err != nil {
		return FrictionEvent{}, err
	}
	updated := false
	for i := range existing {
		if existing[i].Fingerprint == fp {
			existing[i].OccurrenceCount++
			existing[i].LastSeenAt = ts
			existing[i].RecoveryAction = diagnosis
			existing[i].Origin = "user_correction"
			existing[i].TargetSubstrate = "ring2_skill_or_overlay"
			existing[i].FailureClass = "content"
			if convID != "" && !containsString(existing[i].ConversationIDs, convID) {
				existing[i].ConversationIDs = append(existing[i].ConversationIDs, convID)
			}
			if existing[i].Status == "ignored" {
				existing[i].Status = "open"
			}
			existing[i].ImpactScore = computeImpactScore(existing[i].OccurrenceCount, existing[i].WastedSteps, existing[i].MeanWastedTokens)
			ev = existing[i]
			updated = true
			break
		}
	}
	if !updated {
		existing = append(existing, ev)
	}
	if err := SaveObservations(vaultDir, existing); err != nil {
		return FrictionEvent{}, err
	}
	return ev, nil
}

// Mine executes Stage 1 (0-token deterministic trace & intervention mining) across journals,
// transcripts, user interventions, edited drafts, token anomalies, and proactive outbox feedback,
// persisting impact-weighted FrictionEvents to local/memory/rsi/observations.jsonl and updating
// causal Canary records in archive.jsonl.
func Mine(rootDir, vaultDir string, since time.Duration) ([]FrictionEvent, error) {
	existing, _ := LoadObservations(vaultDir)
	priorByFP := make(map[string]FrictionEvent, len(existing))
	byFP := make(map[string]*FrictionEvent, len(existing))
	var order []string
	for i := range existing {
		ev := existing[i]
		priorByFP[ev.Fingerprint] = ev
		if since > 0 || ev.Status != "open" || ev.Origin == "user_correction" {
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

	// Load token telemetry once for impact weighting and canary regression checks.
	var obsIdx *obs.ObsIndex
	if idx, err := obs.LoadOrRefreshIndex(vaultDir); err == nil && idx != nil {
		obsIdx = idx
	}

	convTokens := func(convID string) int64 {
		if obsIdx == nil || convID == "" {
			return 0
		}
		if cm, ok := obsIdx.Conversations[convID]; ok {
			if cm.Self.GrossTokens > 0 {
				return cm.Self.GrossTokens
			}
			return cm.Self.InputTokens + cm.Self.OutputTokens
		}
		return 0
	}

	upsert := func(cat FrictionCategory, fault AttributionFaultLayer, skillOrPkg, toolName, cmdPat, errExcerpt, recovery, origin string, wasted int, convID, parentConv, modelName, ts string, tainted bool) {
		if ts == "" {
			ts = now.Format(time.RFC3339)
		}
		if wasted <= 0 {
			wasted = 1
		}
		if origin == "" {
			origin = "auto_mined"
		}
		errExcerpt = transcript.FirstLineExcerpt(errExcerpt, 180)
		sig := cmdPat
		if sig == "" {
			sig = errExcerpt
		}
		fp := computeFingerprint(cat, skillOrPkg, toolName, sig)
		failClass, substrate := classifyFailureAndSubstrate(cat, skillOrPkg, toolName, cmdPat, errExcerpt)
		toks := convTokens(convID)

		if cur, ok := byFP[fp]; ok {
			alreadyHasConv := convID != "" && containsString(cur.ConversationIDs, convID)
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
			if origin == "user_correction" {
				cur.Origin = "user_correction"
			}
			if cur.FailureClass == "" {
				cur.FailureClass = failClass
			}
			if cur.TargetSubstrate == "" {
				cur.TargetSubstrate = substrate
			}
			if toks > 0 {
				if cur.MeanWastedTokens == 0 {
					cur.MeanWastedTokens = toks
				} else {
					cur.MeanWastedTokens = (cur.MeanWastedTokens + toks) / 2
				}
			}
			cur.ImpactScore = computeImpactScore(cur.OccurrenceCount, cur.WastedSteps, cur.MeanWastedTokens)
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
			ID:               id,
			Fingerprint:      fp,
			Category:         cat,
			FaultLayer:       fault,
			FailureClass:     failClass,
			TargetSubstrate:  substrate,
			Origin:           origin,
			SkillOrPkg:       skillOrPkg,
			ToolName:         toolName,
			CommandPattern:   cmdPat,
			ErrorExcerpt:     errExcerpt,
			RecoveryAction:   recovery,
			WastedSteps:      wasted,
			MeanWastedTokens: toks,
			ImpactScore:      computeImpactScore(1, wasted, toks),
			OccurrenceCount:  1,
			ConversationIDs:  convs,
			ParentConvID:     parentConv,
			ModelName:        modelName,
			FirstSeenAt:      firstSeen,
			LastSeenAt:       ts,
			Tainted:          tainted,
			Status:           status,
		}
		byFP[fp] = ev
		order = append(order, fp)
	}

	// 1. Walk turn journals via shared vault.WalkJournals and parse transcripts via shared internal/transcript
	vaultSkills := listInstalledVaultSkillSlugs(vaultDir)
	observedCleanSkills := make(map[string]int)
	observedSkillStats := make(map[string]*skillWindowStats)
	observedFingerprintsThisRun := make(map[string]bool)

	_ = vault.WalkJournals(vaultDir, cutoff, false, func(_ string, je vault.JournalRecord) bool {
		if t, ok := tasksByConv[je.ConversationID]; ok && t.Ephemeral {
			return true
		}
		parentConv := je.ParentConvID
		if parentConv == "" {
			if t, ok := tasksByConv[je.ConversationID]; ok {
				parentConv = t.ParentConversationID
			}
		}
		// Authoritative taint check against persistent taint ledger (RFC-0029 D3).
		_, ledgerTainted := taint.IsTainted(vaultDir, je.ConversationID)
		turnTainted := je.TaintedSession || ledgerTainted

		defaultSkill := inferSkillOrPkg(je.UserPrompt+" "+je.ResponseSummary, je.FilesModified, vaultSkills)
		st := observedSkillStats[defaultSkill]
		if st == nil {
			st = &skillWindowStats{}
			observedSkillStats[defaultSkill] = st
		}
		st.Turns++
		turnErrs := je.TurnErrorCount
		if turnErrs == 0 && je.StepStartIdx == 0 && je.TurnStepCount == 0 {
			turnErrs = je.ToolErrorCount
		}
		st.Errors += turnErrs
		if toks := convTokens(je.ConversationID); toks > 0 {
			st.TotalTokens += toks
			st.TokenSamples++
		}
		if turnErrs == 0 {
			observedCleanSkills[defaultSkill]++
		}

		// Clean-exit high-cost workflow detection (compile_script_or_route candidate)
		if je.TurnStepCount >= 25 && turnErrs == 0 && !turnTainted {
			wfSig := fmt.Sprintf("high-step:%s", defaultSkill)
			upsert(
				FrictionWorkflowBloat,
				FaultWorkerACIOrSkill,
				defaultSkill,
				"workflow_steps",
				wfSig,
				fmt.Sprintf("Recurring high-step clean-exit workflow (%d steps in %s)", je.TurnStepCount, defaultSkill),
				fmt.Sprintf("Compile repetitive multi-step %s workflow into a deterministic helper script or tight skill route", defaultSkill),
				"auto_mined",
				je.TurnStepCount/5,
				je.ConversationID,
				parentConv,
				je.ModelName,
				je.Timestamp,
				false,
			)
		}

		if je.TranscriptPath != "" {
			mineTranscript(je, parentConv, defaultSkill, vaultSkills, turnTainted, upsert, observedFingerprintsThisRun)
		}
		return true
	})

	// 2. Scan local/memory/interventions.jsonl for explicit user feedback & substantive proposal rejections
	interventionsFile := filepath.Join(vaultDir, "memory", "interventions.jsonl")
	if f, err := os.Open(interventionsFile); err == nil {
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
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
				Dismissed  bool   `json:"dismissed"`
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
			if !cutoff.IsZero() && item.Timestamp != "" {
				if ts, tErr := time.Parse(time.RFC3339, item.Timestamp); tErr == nil && ts.Before(cutoff) {
					continue
				}
			}
			if item.Type == "user_feedback" && (item.Summary != "" || item.Lesson != "") {
				skill := inferSkillOrPkg(item.Category+" "+item.Summary+" "+item.Lesson, nil, vaultSkills)
				upsert(
					FrictionUserIntervention,
					FaultWorkerACIOrSkill,
					skill,
					"user_feedback",
					item.Category,
					item.Summary,
					item.Lesson,
					"user_correction",
					2,
					"feedback-"+item.Timestamp,
					"",
					"",
					item.Timestamp,
					false,
				)
				continue
			}
			if item.Dismissed {
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
			reasonTrim := strings.TrimSpace(item.Reason)
			lowerReason := strings.ToLower(reasonTrim)
			if lowerReason == "" || strings.Contains(lowerReason, "dismiss") || strings.Contains(lowerReason, "expired") || strings.Contains(lowerReason, "cleanup") {
				// Skip housekeeping/empty-reason rejections so stale queue cleanup does not pollute RSI observations.
				continue
			}
			if propID != "" && item.Decision == "rejected" {
				skill := inferSkillOrPkg(actType+" "+cmd+" "+summary, nil, vaultSkills)
				upsert(
					FrictionUserIntervention,
					FaultWorkerACIOrSkill,
					skill,
					"hq_propose",
					cmd,
					fmt.Sprintf("Rejected proposal (%s): %s", actType, reasonTrim),
					fmt.Sprintf("Honor user rejection feedback: %s", reasonTrim),
					"auto_mined",
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
						"user_correction",
						2,
						d.ConversationID,
						"",
						"",
						d.UpdatedAt,
						false,
					)
				}
			}
		}
	}

	// 3b. Scan token anomalies (A3 RunawayTurnBurn & A4 TierMisroutingBurn) as FrictionQuotaAnomaly
	if obsIdx != nil {
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
			anomSkill := inferSkillOrPkg(anom.TaskSlug+" "+cm.Title, nil, vaultSkills)
			upsert(
				FrictionQuotaAnomaly,
				FaultModelRouting,
				anomSkill,
				anom.Kind,
				anom.Code+":"+anom.TaskSlug,
				anom.Summary,
				anom.Remediation,
				"auto_mined",
				3,
				anom.ConversationID,
				cm.ParentConversationID,
				cm.Model,
				anom.Timestamp,
				false,
			)
		}
	}

	// 3c. Scan outbox proactive noise rates (RFC-0014 & RFC-0027)
	if stats, err := outbox.ComputeSkillDismissalStats(vaultDir); err == nil {
		for _, st := range stats {
			reasonsSuffix := ""
			if st.SampleReasons != "" {
				reasonsSuffix = fmt.Sprintf(" (user reasons: %s)", st.SampleReasons)
			}
			if st.TotalResolved >= 5 && st.DismissRate > 0.70 {
				upsert(
					FrictionProactiveNoise,
					FaultWorkerACIOrSkill,
					st.Skill,
					"hq_outbox",
					"ideas_feed:"+st.Skill,
					fmt.Sprintf("Skill %s has %.0f%% proactive idea dismissal rate (%d/%d dismissed)%s", st.Skill, st.DismissRate*100.0, st.DismissedCount, st.TotalResolved, reasonsSuffix),
					fmt.Sprintf("Tighten proactive idea generation threshold in Vault overlay local/memory/rsi/rules/%s.md so low-signal suggestions are suppressed", st.Skill),
					"auto_mined",
					st.DismissedCount,
					"outbox-"+st.Skill,
					"",
					"",
					st.LastSeenAt,
					false,
				)
			}
			if st.InterruptResolvedCount >= 5 && st.InterruptNoisyRate >= 0.40 {
				upsert(
					FrictionProactiveNoise,
					FaultWorkerACIOrSkill,
					st.Skill,
					"hq_outbox",
					"interrupt_noisy:"+st.Skill,
					fmt.Sprintf("Skill %s has %.0f%% noisy interrupt dismissal rate (%d/%d marked noisy; %d handled, %d parked)%s", st.Skill, st.InterruptNoisyRate*100.0, st.InterruptNoisyCount, st.InterruptResolvedCount, st.InterruptActedCount, st.InterruptParkedCount, reasonsSuffix),
					fmt.Sprintf("Add a Vault-local rule in local/memory/rsi/rules/%s.md downgrading routine %s alerts from --tier=interrupt to --tier=brief_rollup or --tier=ideas_feed", st.Skill, st.Skill),
					"auto_mined",
					st.InterruptNoisyCount,
					"outbox-interrupt-"+st.Skill,
					"",
					"",
					st.LastSeenAt,
					false,
				)
			}
			if st.ProposalTotalCount >= 4 && st.ProposalExpiredRate >= 0.50 {
				upsert(
					FrictionProactiveNoise,
					FaultWorkerACIOrSkill,
					st.Skill,
					"hq_approvals",
					"proposal_expired:"+st.Skill,
					fmt.Sprintf("Skill %s has %.0f%% staged proposal expiration rate (%d/%d expired unsent)", st.Skill, st.ProposalExpiredRate*100.0, st.ProposalExpiredCount, st.ProposalTotalCount),
					fmt.Sprintf("Stage outbound proposals for %s only when high-confidence or bundle into Morning Brief via local/memory/rsi/rules/%s.md", st.Skill, st.Skill),
					"auto_mined",
					st.ProposalExpiredCount,
					"approvals-expired-"+st.Skill,
					"",
					"",
					st.LastSeenAt,
					false,
				)
			}
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

	// 4. Update causal Canary records & GUARD regression checks in local/memory/rsi/archive.jsonl
	_ = updateCanaryRecords(rootDir, vaultDir, observedCleanSkills, observedSkillStats, observedFingerprintsThisRun)

	return result, nil
}

type skillWindowStats struct {
	Turns        int
	Errors       int
	TotalTokens  int64
	TokenSamples int
}

func (s *skillWindowStats) errorRate() float64 {
	if s == nil || s.Turns <= 0 {
		return 0
	}
	return float64(s.Errors) / float64(s.Turns)
}

func (s *skillWindowStats) meanTokens() float64 {
	if s == nil || s.TokenSamples <= 0 {
		return 0
	}
	return float64(s.TotalTokens) / float64(s.TokenSamples)
}

func containsString(list []string, target string) bool {
	for _, item := range list {
		if item == target {
			return true
		}
	}
	return false
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
	je vault.JournalRecord,
	parentConv string,
	defaultSkill string,
	vaultSkills []string,
	initialTainted bool,
	upsert func(cat FrictionCategory, fault AttributionFaultLayer, skillOrPkg, toolName, cmdPat, errExcerpt, recovery, origin string, wasted int, convID, parentConv, modelName, ts string, tainted bool),
	observedFPs map[string]bool,
) {
	steps, transcriptTainted, err := transcript.ParseSteps(je.TranscriptPath, je.StepStartIdx)
	if err != nil || len(steps) == 0 {
		return
	}
	tainted := initialTainted || transcriptTainted

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
			skill := inferSkillForStep(s.CmdLine, s.Content, je.FilesModified, defaultSkill, vaultSkills)
			excerpt := transcript.FirstLineExcerpt(s.Content, 180)
			fp := computeFingerprint(FrictionPreTool, skill, tool, s.CmdLine)
			observedFPs[fp] = true
			upsert(
				FrictionPreTool,
				FaultWorkerACIOrSkill,
				skill,
				tool,
				s.CmdLine,
				excerpt,
				"Stage outbound or restricted command via hq propose or delegate to background subagent",
				"auto_mined",
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
		excerpt := transcript.FirstLineExcerpt(s.Content, 180)
		if isDevTDDOrBenignNoMatch(s.CmdLine, excerpt) {
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
				recovery = transcript.FirstLineExcerpt(steps[j].Content, 160)
				break
			}
		}

		tool := s.ToolName
		if tool == "" {
			tool = "run_command"
		}
		skill := inferSkillForStep(s.CmdLine, s.Content, je.FilesModified, defaultSkill, vaultSkills)
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
			"auto_mined",
			wasted,
			je.ConversationID,
			parentConv,
			je.ModelName,
			je.Timestamp,
			tainted,
		)
	}
}

// inferSkillForStep attributes the failing step to its own command/file target FIRST (RFC-0029 D10),
// preventing unrelated keywords in Front Desk summaries from hijacking core CLI or shell failures.
func inferSkillForStep(cmdLine, content string, filesModified []string, defaultSkill string, vaultSkills []string) string {
	stepSkill := inferSkillOrPkg(cmdLine+" "+content, nil, vaultSkills)
	if stepSkill != "" && stepSkill != "general" {
		return stepSkill
	}
	if isOwnedHQCommand(cmdLine) {
		return "general"
	}
	fileSkill := inferSkillOrPkg("", filesModified, vaultSkills)
	if fileSkill != "" && fileSkill != "general" {
		return fileSkill
	}
	if defaultSkill != "" {
		return defaultSkill
	}
	return "general"
}
