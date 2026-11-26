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

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/obs"
	"github.com/zjshen14/agy-hq/internal/outbox"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/vault"
)

// RSIArchiveRecord tracks an applied or rejected/reverted RSI mutation through its
// post-apply canary window and negative-memory tombstone lifecycle.
type RSIArchiveRecord struct {
	ID                 string   `json:"id"`
	ProposalID         string   `json:"proposalId,omitempty"`
	RuleID             string   `json:"ruleId,omitempty"`
	Origin             string   `json:"origin,omitempty"`
	ModelScope         string   `json:"modelScope,omitempty"`
	EditRationale      string   `json:"editRationale,omitempty"`
	PredictedOutcome   string   `json:"predictedOutcome,omitempty"`
	Ring               int      `json:"ring"`
	Title              string   `json:"title"`
	TargetFiles        []string `json:"targetFiles"`
	SourceFingerprints []string `json:"sourceFingerprints"`
	DiffSHA256         string   `json:"diffSha256"`
	VaultSnapshotRef   string   `json:"vaultSnapshotRef,omitempty"`
	AppliedAt          string   `json:"appliedAt"`
	TargetCanaryRuns   int      `json:"targetCanaryRuns"` // 15 for high-freq tools; 3 for weekly/quarterly skills
	SessionsObserved   int      `json:"sessionsObserved"`
	HelpfulCount       int      `json:"helpfulCount"`
	HarmfulCount       int      `json:"harmfulCount"`
	BaselineErrorRate  float64  `json:"baselineErrorRate,omitempty"`
	BaselineMeanTokens float64  `json:"baselineMeanTokens,omitempty"`
	RejectionReason    string   `json:"rejectionReason,omitempty"`
	RevertHypothesisID string   `json:"revertHypothesisId,omitempty"`
	Status             string   `json:"status"` // "canary", "promoted", "reverted", "rejected", "archived"
}

// ArchivePath returns local/memory/rsi/archive.jsonl.
func ArchivePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "rsi", "archive.jsonl")
}

// LoadArchive reads all RSIArchiveRecord entries from local/memory/rsi/archive.jsonl.
func LoadArchive(vaultDir string) ([]RSIArchiveRecord, error) {
	p := ArchivePath(vaultDir)
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var out []RSIArchiveRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec RSIArchiveRecord
		if json.Unmarshal([]byte(line), &rec) == nil && rec.ID != "" {
			out = append(out, rec)
		}
	}
	return out, scanner.Err()
}

// SaveArchive atomically writes the canonical RSIArchiveRecord slice to local/memory/rsi/archive.jsonl
// under store.RankMemory with 0600 permissions.
func SaveArchive(vaultDir string, recs []RSIArchiveRecord) error {
	p := ArchivePath(vaultDir)
	var sb strings.Builder
	for _, r := range recs {
		b, err := json.Marshal(r)
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

func targetCanaryRunsForTargets(targetFiles []string) int {
	for _, tf := range targetFiles {
		lower := strings.ToLower(tf)
		if strings.Contains(lower, "tax-prep") ||
			strings.Contains(lower, "filing-diff") ||
			strings.Contains(lower, "thesis-check") ||
			strings.Contains(lower, "ledger-ingest") ||
			strings.Contains(lower, "sleep-consolidate") {
			return 3
		}
	}
	return 15
}

// ruleExistsOnDisk verifies causally that a canary record's target file (and RuleID marker, if set)
// still exists on disk before crediting HelpfulCount (RFC-0029 D9).
func ruleExistsOnDisk(rootDir, vaultDir string, r RSIArchiveRecord) bool {
	if len(r.TargetFiles) == 0 {
		return false
	}
	for _, tf := range r.TargetFiles {
		absPath, err := ResolveHypothesisTargetPath(rootDir, vaultDir, nil, tf)
		if err != nil {
			continue
		}
		b, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		if r.RuleID == "" {
			return true
		}
		if strings.Contains(string(b), fmt.Sprintf(`<!-- rsi:rule id=%q`, r.RuleID)) {
			return true
		}
	}
	return false
}

func updateCanaryRecords(rootDir, vaultDir string, cleanSkills map[string]int, skillStats map[string]*skillWindowStats, observedFPs map[string]bool) error {
	recs, err := LoadArchive(vaultDir)
	if err != nil || len(recs) == 0 {
		return err
	}
	changed := false
	for i := range recs {
		r := &recs[i]
		if r.Status != "canary" {
			continue
		}
		// Causal existence check (RFC-0029 D9): deleted target files or removed rule blocks cannot accrue helpful hits.
		if !ruleExistsOnDisk(rootDir, vaultDir, *r) {
			r.Status = "archived"
			changed = true
			continue
		}

		recurred := false
		for _, fp := range r.SourceFingerprints {
			if observedFPs[fp] {
				recurred = true
				break
			}
		}
		skill := targetSkillFromFiles(r.TargetFiles)
		st := skillStats[skill]

		if recurred {
			r.SessionsObserved++
			r.HarmfulCount++
			changed = true
		} else {
			for _, tf := range r.TargetFiles {
				for sk, count := range cleanSkills {
					if count > 0 && sk != "" && sk != "general" && strings.Contains(tf, sk) {
						r.SessionsObserved++
						r.HelpfulCount++
						changed = true
						break
					}
				}
			}
		}

		// GUARD directional regret check (Q6): auto-stage 1-tap revert if target friction recurs >= 2x
		// OR target skill error/token rate regresses > 25% vs pre-apply baseline.
		regressedReason := ""
		if r.HarmfulCount >= 2 {
			regressedReason = fmt.Sprintf("target friction recurred %dx during canary window", r.HarmfulCount)
		} else if st != nil && st.Turns >= 3 {
			if r.BaselineErrorRate > 0 && st.errorRate() > r.BaselineErrorRate*1.25 {
				regressedReason = fmt.Sprintf("skill %q error rate regressed %.2f -> %.2f (>25%%)", skill, r.BaselineErrorRate, st.errorRate())
			} else if r.BaselineMeanTokens > 0 && st.meanTokens() > r.BaselineMeanTokens*1.25 {
				regressedReason = fmt.Sprintf("skill %q mean tokens regressed %.0f -> %.0f (>25%%)", skill, r.BaselineMeanTokens, st.meanTokens())
			}
		}
		if regressedReason != "" && r.RevertHypothesisID == "" && r.RuleID != "" {
			if revID := stageAutoRevertHypothesis(rootDir, vaultDir, *r, regressedReason); revID != "" {
				r.RevertHypothesisID = revID
				changed = true
			}
		}

		if r.TargetCanaryRuns <= 0 {
			r.TargetCanaryRuns = 3
		}
		if r.SessionsObserved >= r.TargetCanaryRuns && r.HarmfulCount == 0 && regressedReason == "" {
			r.Status = "promoted"
			changed = true
		}
	}
	if changed {
		return SaveArchive(vaultDir, recs)
	}
	return nil
}

func stageAutoRevertHypothesis(rootDir, vaultDir string, r RSIArchiveRecord, reason string) string {
	if len(r.TargetFiles) == 0 || r.RuleID == "" {
		return ""
	}
	tf := r.TargetFiles[0]
	absPath, err := ResolveHypothesisTargetPath(rootDir, vaultDir, nil, tf)
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(absPath)
	if err != nil {
		return ""
	}
	delta := &ACEDeltaOp{
		Op:      "deprecate_skill_rule",
		RuleID:  r.RuleID,
		Section: LearnedRulesHeading,
	}
	after, err := ApplyACEDelta(string(b), *delta)
	if err != nil {
		return ""
	}
	diff := BuildUnifiedDiff(tf, string(b), after)
	sum := sha256.Sum256([]byte(diff))
	now := time.Now().UTC()
	hID := fmt.Sprintf("rsi-%s-revert-%s", now.Format("20060102"), sanitizeBranchToken(r.RuleID))
	hyp := RSIHypothesis{
		ID:                 hID,
		SourceFingerprints: r.SourceFingerprints,
		Ring:               Ring2SkillTrigger,
		FaultLayer:         FaultWorkerACIOrSkill,
		Title:              fmt.Sprintf("Revert regressed RSI rule %s (%s)", r.RuleID, reason),
		RootCause:          fmt.Sprintf("GUARD canary regret check triggered for %s: %s", r.ID, reason),
		EditRationale:      "Automatically staged 1-tap revert because canary telemetry detected regression.",
		PredictedOutcome:   "Restore pre-apply skill reliability and token baseline.",
		Origin:             "revert",
		TargetFiles:        []string{tf},
		DeltaOp:            delta,
		ProposedDiff:       diff,
		DiffSHA256:         hex.EncodeToString(sum[:]),
		ExpectedSavedSteps: 4,
		Status:             "draft",
		Tier:               "personal",
		CreatedAt:          now.Format(time.RFC3339),
	}
	if err := SaveHypothesis(vaultDir, &hyp); err != nil {
		return ""
	}
	return hID
}

// Propose stages a verified RSIHypothesis into local/memory/pending/<propId>.json via approvals.CreateWithMetadata.
// For high-risk external-facing skills, requires a passing diff-SHA-bound behavioral safety probe first (Q5).
func Propose(rootDir, vaultDir, rsiID, convID, hqBin string) (*approvals.Proposal, *RSIHypothesis, error) {
	h, err := LoadHypothesis(vaultDir, rsiID)
	if err != nil {
		return nil, nil, fmt.Errorf("load hypothesis %s: %w", rsiID, err)
	}
	if h.Verification == nil || !h.Verification.AllPassed {
		return nil, h, fmt.Errorf("refusing to propose unverified or failed hypothesis %s (run `hq rsi verify %s` first)", h.ID, h.ID)
	}
	if h.Verification.VerifiedDiffSHA256 != h.DiffSHA256 {
		return nil, h, fmt.Errorf("hypothesis %s diff SHA-256 changed since verification; re-run `hq rsi verify %s`", h.ID, h.ID)
	}
	if h.Verification.RequiresLiveSafetyProbe {
		if !h.SafetyProbePassed || h.SafetyProbeDiffSHA != h.DiffSHA256 {
			return nil, h, fmt.Errorf("hypothesis %s targets a high-risk external-facing skill and requires a passing diff-SHA-bound behavioral safety probe first (run `hq rsi probes %s --json` then `hq rsi record-probe %s --passed=true --summary=...`)", h.ID, h.ID, h.ID)
		}
	}

	if hqBin == "" {
		hqBin = "./bin/hq"
	}
	shaShort := h.DiffSHA256
	if len(shaShort) > 12 {
		shaShort = shaShort[:12]
	}

	actionType := "rsi_skill_patch"
	if h.Ring == Ring1KernelCode {
		actionType = "rsi_patch"
	} else if isOverlayTargetList(h.TargetFiles) {
		actionType = "rsi_overlay"
	}

	tierLabel := strings.TrimSpace(h.Tier)
	if tierLabel == "" {
		tierLabel = "personal"
	}

	summary := fmt.Sprintf("[%s] %s (rsi:%s · diff:%s)", h.Ring.Label(), h.Title, h.ID, shaShort)
	cmdStr := fmt.Sprintf("%s rsi apply %s --expect-diff-sha256=%s", hqBin, h.ID, shaShort)

	diffPreview := strings.TrimSpace(h.ProposedDiff)
	if len(diffPreview) > 800 {
		diffPreview = diffPreview[:800] + "\n..."
	}

	probeTag := ""
	if h.Verification.RequiresLiveSafetyProbe {
		probeTag = " | Live Safety Probe: PASS"
	}
	gateSummary := fmt.Sprintf("Gate 1 (TCB/AST): PASS | Gate 2 (Counterfactual/Critic-1): PASS (%s) | Gate 3 (Tests/PII): PASS | Gate 4 (Budget): PASS%s", h.Verification.CounterfactualBasline, probeTag)

	meta := map[string]string{
		"rsiId":                h.ID,
		"ring":                 fmt.Sprintf("%d", int(h.Ring)),
		"ringLabel":            h.Ring.Label(),
		"tier":                 tierLabel,
		"title":                h.Title,
		"faultLayer":           string(h.FaultLayer),
		"targetFiles":          strings.Join(h.TargetFiles, ", "),
		"gateSummary":          gateSummary,
		"diffSha256":           h.DiffSHA256,
		"canonicalDiffPreview": diffPreview,
	}
	if h.Origin != "" {
		meta["origin"] = h.Origin
	}
	if h.EditRationale != "" {
		meta["editRationale"] = h.EditRationale
	}
	if h.PredictedOutcome != "" {
		meta["predictedOutcome"] = h.PredictedOutcome
	}
	if h.WorktreeDir != "" {
		meta["worktreeDir"] = h.WorktreeDir
	}
	if h.Branch != "" {
		meta["branch"] = h.Branch
	}

	workDir := rootDir
	if h.WorktreeDir != "" {
		workDir = h.WorktreeDir
	}
	prop, err := approvals.CreateWithMetadata(vaultDir, actionType, summary, cmdStr, workDir, convID, meta)
	if err != nil {
		return nil, h, err
	}

	h.Status = "proposed"
	_ = SaveHypothesis(vaultDir, h)
	return &prop, h, nil
}

func computeSkillPreApplyBaseline(vaultDir, skill string) (errRate float64, meanToks float64) {
	if skill == "" || skill == "general" {
		return 0, 0
	}
	cutoff := time.Now().UTC().Add(-14 * 24 * time.Hour)
	vaultSkills := listInstalledVaultSkillSlugs(vaultDir)
	var obsIdx *obs.ObsIndex
	if idx, err := obs.LoadOrRefreshIndex(vaultDir); err == nil && idx != nil {
		obsIdx = idx
	}
	turns, errs := 0, 0
	var tokSum int64
	tokTurns := 0
	_ = vault.WalkJournals(vaultDir, cutoff, false, func(_ string, rec vault.JournalRecord) bool {
		if rec.TaintedSession {
			return true
		}
		sk := inferSkillOrPkg(rec.UserPrompt+" "+rec.ResponseSummary, rec.FilesModified, vaultSkills)
		if sk != skill {
			return true
		}
		turns++
		e := rec.TurnErrorCount
		if e == 0 && rec.StepStartIdx == 0 && rec.TurnStepCount == 0 {
			e = rec.ToolErrorCount
		}
		errs += e
		if obsIdx != nil {
			if cm, ok := obsIdx.Conversations[rec.ConversationID]; ok {
				t := cm.Self.GrossTokens
				if t == 0 {
					t = cm.Self.InputTokens + cm.Self.OutputTokens
				}
				if t > 0 {
					tokSum += t
					tokTurns++
				}
			}
		}
		return true
	})
	if turns > 0 {
		errRate = float64(errs) / float64(turns)
	}
	if tokTurns > 0 {
		meanToks = float64(tokSum) / float64(tokTurns)
	}
	return errRate, meanToks
}

// Apply deterministically applies an approved RSIHypothesis after verifying its DiffSHA256,
// re-checking TCB & safety probe gates, taking a pre-apply Vault git snapshot, and recording
// a canary (or revert) entry in local/memory/rsi/archive.jsonl.
func Apply(rootDir, vaultDir, rsiID, expectDiffSHA string) (string, error) {
	h, err := LoadHypothesis(vaultDir, rsiID)
	if err != nil {
		return "", fmt.Errorf("load hypothesis %s: %w", rsiID, err)
	}
	if h.Verification == nil || !h.Verification.AllPassed {
		return "", fmt.Errorf("cannot apply unverified hypothesis %s", h.ID)
	}
	if h.Verification.RequiresLiveSafetyProbe && (!h.SafetyProbePassed || h.SafetyProbeDiffSHA != h.DiffSHA256) {
		return "", fmt.Errorf("cannot apply high-risk hypothesis %s without passing diff-SHA-bound behavioral safety probe", h.ID)
	}

	actualSum := sha256.Sum256([]byte(h.ProposedDiff))
	actualHex := hex.EncodeToString(actualSum[:])
	if actualHex != h.DiffSHA256 {
		return "", fmt.Errorf("diff integrity mismatch for %s", h.ID)
	}
	if h.Verification.VerifiedDiffSHA256 != "" && h.Verification.VerifiedDiffSHA256 != actualHex {
		return "", fmt.Errorf("verified diff SHA-256 mismatch for %s", h.ID)
	}
	expect := strings.ToLower(strings.TrimSpace(expectDiffSHA))
	if len(expect) < 8 || !strings.HasPrefix(strings.ToLower(h.DiffSHA256), expect) {
		return "", fmt.Errorf("expect-diff-sha256 mismatch (minimum 8 hex chars required): expected prefix %q, got %q", expect, h.DiffSHA256[:12])
	}

	obsList, _ := LoadObservations(vaultDir)
	if ok, reason := CheckTCBAndAntiGoodhart(rootDir, vaultDir, h, obsList); !ok {
		return "", fmt.Errorf("apply blocked by TCB re-verification: %s", reason)
	}

	// Capture pre-apply skill baseline for GUARD directional regret checks
	targetSkill := targetSkillFromFiles(h.TargetFiles)
	baseErrRate, baseMeanToks := computeSkillPreApplyBaseline(vaultDir, targetSkill)

	// Take Dual-Repo Pre-Apply Snapshot in local/.git (strictly outside store.RankMemory)
	snapshotTaken := vault.SnapshotVault(vaultDir, "pre-rsi-apply-"+h.ID)
	snapRef := "clean"
	if snapshotTaken {
		snapRef = "pre-rsi-apply-" + h.ID
	}

	if h.DeltaOp != nil {
		for _, tf := range h.TargetFiles {
			absPath, pathErr := ResolveHypothesisTargetPath(rootDir, vaultDir, h, tf)
			if pathErr != nil {
				return "", fmt.Errorf("refusing target file %q: %w", tf, pathErr)
			}
			if IsRing0Path(tf) {
				return "", fmt.Errorf("refusing target file %q inside Ring 0 Immutable TCB", tf)
			}
			rawContent, rErr := EnsureOverlaySkeletonContent(absPath, tf)
			if rErr != nil {
				return "", fmt.Errorf("read target %s: %w", tf, rErr)
			}
			after, err := ApplyACEDelta(rawContent, *h.DeltaOp)
			if err != nil {
				return "", fmt.Errorf("apply ACE delta to %s: %w", tf, err)
			}
			if err := os.MkdirAll(filepath.Dir(absPath), 0o700); err != nil {
				return "", fmt.Errorf("mkdir %s: %w", filepath.Dir(absPath), err)
			}
			if err := store.WriteAtomic(absPath, []byte(after), 0o600); err != nil {
				return "", fmt.Errorf("write target %s: %w", tf, err)
			}
		}
	}

	h.Status = "approved"
	_ = SaveHypothesis(vaultDir, h)

	if obs, err := LoadObservations(vaultDir); err == nil {
		obsChanged := false
		for i := range obs {
			for _, oid := range h.ObservationIDs {
				if obs[i].ID == oid {
					obs[i].Status = "applied"
					obsChanged = true
				}
			}
		}
		if obsChanged {
			_ = SaveObservations(vaultDir, obs)
		}
	}

	recs, _ := LoadArchive(vaultDir)
	ruleID := ""
	modelScope := "all"
	isDeprecation := false
	if h.DeltaOp != nil {
		ruleID = h.DeltaOp.RuleID
		if h.DeltaOp.ModelScope != "" {
			modelScope = h.DeltaOp.ModelScope
		}
		isDeprecation = h.DeltaOp.Op == "deprecate_skill_rule"
	}

	if isDeprecation {
		for i := range recs {
			if ruleID != "" && recs[i].RuleID == ruleID && (recs[i].Status == "canary" || recs[i].Status == "promoted") {
				if h.Origin == "revert" {
					recs[i].Status = "reverted"
				} else {
					recs[i].Status = "archived"
				}
			}
		}
		_ = SaveArchive(vaultDir, recs)
		return fmt.Sprintf("Applied RSI rule removal %s (%s) to %s (diff sha256: %s)\n",
			h.ID, ruleID, strings.Join(h.TargetFiles, ", "), h.DiffSHA256[:12]), nil
	}

	canaryRuns := targetCanaryRunsForTargets(h.TargetFiles)
	recs = append(recs, RSIArchiveRecord{
		ID:                 h.ID,
		RuleID:             ruleID,
		Origin:             h.Origin,
		ModelScope:         modelScope,
		EditRationale:      h.EditRationale,
		PredictedOutcome:   h.PredictedOutcome,
		Ring:               int(h.Ring),
		Title:              h.Title,
		TargetFiles:        h.TargetFiles,
		SourceFingerprints: h.SourceFingerprints,
		DiffSHA256:         h.DiffSHA256,
		VaultSnapshotRef:   snapRef,
		AppliedAt:          time.Now().UTC().Format(time.RFC3339),
		TargetCanaryRuns:   canaryRuns,
		SessionsObserved:   0,
		HelpfulCount:       0,
		HarmfulCount:       0,
		BaselineErrorRate:  baseErrRate,
		BaselineMeanTokens: baseMeanToks,
		Status:             "canary",
	})
	_ = SaveArchive(vaultDir, recs)

	return fmt.Sprintf("Applied RSI patch %s to %s (canary window: %d runs, diff sha256: %s)\n",
		h.ID, strings.Join(h.TargetFiles, ", "), canaryRuns, h.DiffSHA256[:12]), nil
}

// RecordTombstone marks a rejected RSI hypothesis as rejected and persists a negative-memory
// tombstone in archive.jsonl so future cycles do not re-propose the same mutation.
func RecordTombstone(vaultDir, rsiID, reason string) error {
	rsiID = strings.TrimSpace(rsiID)
	if rsiID == "" {
		return nil
	}
	h, err := LoadHypothesis(vaultDir, rsiID)
	if err != nil {
		return err
	}
	h.Status = "rejected"
	_ = SaveHypothesis(vaultDir, h)

	recs, _ := LoadArchive(vaultDir)
	for _, r := range recs {
		if r.ID == h.ID && r.Status == "rejected" {
			return nil
		}
	}
	ruleID := ""
	if h.DeltaOp != nil {
		ruleID = h.DeltaOp.RuleID
	}
	recs = append(recs, RSIArchiveRecord{
		ID:                 h.ID,
		RuleID:             ruleID,
		Origin:             h.Origin,
		Ring:               int(h.Ring),
		Title:              h.Title,
		TargetFiles:        h.TargetFiles,
		SourceFingerprints: h.SourceFingerprints,
		DiffSHA256:         h.DiffSHA256,
		AppliedAt:          time.Now().UTC().Format(time.RFC3339),
		RejectionReason:    strings.TrimSpace(reason),
		Status:             "rejected",
	})
	return SaveArchive(vaultDir, recs)
}

// Ablate scans Vault skills and overlays for regressed, model-obsolete, or stale zero-utility
// `<!-- rsi:rule -->` blocks, joining live counters from archive.jsonl (RFC-0029 D7).
// Rules with origin=="user_correction" are exempt from the 30-day zero-hit timer (Q6).
func Ablate(rootDir, vaultDir, currentModel string) ([]RSIHypothesis, error) {
	now := time.Now().UTC()
	today := now.Format("20060102")
	var staged []RSIHypothesis

	arch, _ := LoadArchive(vaultDir)
	archByRule := make(map[string]RSIArchiveRecord)
	for _, rec := range arch {
		if rec.RuleID != "" {
			archByRule[rec.RuleID] = rec
		}
		if rec.ID != "" {
			if _, ok := archByRule[rec.ID]; !ok {
				archByRule[rec.ID] = rec
			}
		}
	}

	for _, absPath := range DiscoverAllSkillFiles("", vaultDir) {
		b, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		rel := targetRelativePath(rootDir, vaultDir, absPath)

		rules := ParseRSIRules(rel, string(b))
		for _, r := range rules {
			helpful := r.Helpful
			harmful := r.Harmful
			origin := r.Origin
			modelScope := r.ModelScope
			if rec, ok := archByRule[r.ID]; ok {
				if rec.HelpfulCount > helpful {
					helpful = rec.HelpfulCount
				}
				if rec.HarmfulCount > harmful {
					harmful = rec.HarmfulCount
				}
				if origin == "" && rec.Origin != "" {
					origin = rec.Origin
				}
				if (modelScope == "" || modelScope == "all") && rec.ModelScope != "" {
					modelScope = rec.ModelScope
				}
			}

			shouldDeprecate := false
			reason := ""
			if harmful > helpful && harmful > 0 {
				shouldDeprecate = true
				reason = fmt.Sprintf("rule %s recorded harmful=%d > helpful=%d", r.ID, harmful, helpful)
			} else if currentModel != "" && modelScope != "" && modelScope != "all" && !strings.EqualFold(modelScope, currentModel) {
				shouldDeprecate = true
				reason = fmt.Sprintf("rule %s was scoped to model %q; ablated for upgraded model %q", r.ID, modelScope, currentModel)
			} else if helpful == 0 && r.Added != "" && origin != "user_correction" {
				if addedTime, err := time.Parse("2006-01-02", r.Added); err == nil && now.Sub(addedTime) > 30*24*time.Hour {
					shouldDeprecate = true
					reason = fmt.Sprintf("rule %s has 0 helpful activations >30d after addition (%s)", r.ID, r.Added)
				}
			}

			if !shouldDeprecate {
				continue
			}

			delta := &ACEDeltaOp{
				Op:      "deprecate_skill_rule",
				RuleID:  r.ID,
				Section: LearnedRulesHeading,
			}
			after, err := ApplyACEDelta(string(b), *delta)
			if err != nil {
				continue
			}
			diff := BuildUnifiedDiff(rel, string(b), after)
			sum := sha256.Sum256([]byte(diff))
			hID := fmt.Sprintf("rsi-%s-ablate-%s", today, r.ID)
			hyp := RSIHypothesis{
				ID:                 hID,
				ObservationIDs:     []string{r.Source},
				Ring:               Ring2SkillTrigger,
				FaultLayer:         FaultWorkerACIOrSkill,
				Title:              fmt.Sprintf("Ablate obsolete skill rule %s from %s", r.ID, filepath.Base(filepath.Dir(absPath))),
				RootCause:          reason,
				EditRationale:      "Reclaim skill prompt budget by removing obsolete or regressed rule.",
				PredictedOutcome:   "Lower prompt overhead with no reliability loss.",
				Origin:             "ablation",
				TargetFiles:        []string{rel},
				DeltaOp:            delta,
				ProposedDiff:       diff,
				DiffSHA256:         hex.EncodeToString(sum[:]),
				ExpectedSavedSteps: 2,
				Status:             "draft",
				Tier:               "personal",
				CreatedAt:          now.Format(time.RFC3339),
			}
			if err := SaveHypothesis(vaultDir, &hyp); err == nil {
				staged = append(staged, hyp)
			}
		}
	}
	return staged, nil
}

// StageNightlyReport summarizes `hq rsi stage-nightly --max=2`.
type StageNightlyReport struct {
	StagedProposalIDs []string `json:"stagedProposalIds,omitempty"`
	StagedRSIIDs      []string `json:"stagedRsiIds,omitempty"`
	NeedsProbeIDs     []string `json:"needsProbeIds,omitempty"`
	FailedVerifyIDs   []string `json:"failedVerifyIds,omitempty"`
	OutboxCardIDs     []string `json:"outboxCardIds,omitempty"`
	AdvisoryCardIDs   []string `json:"advisoryCardIds,omitempty"`
}

// StageNightly verifies and proposes at most maxProposals (default 2) highest-ROI Ring-2
// hypotheses and emits 1-tap Morning Brief outbox cards (RFC-0027) for proposals and advisories.
func StageNightly(rootDir, vaultDir, convID, hqBin string, maxProposals int) (*StageNightlyReport, error) {
	if maxProposals <= 0 {
		maxProposals = 2
	}
	rep := &StageNightlyReport{}
	hyps, err := ListHypotheses(vaultDir)
	if err != nil {
		return nil, err
	}

	var candidates []RSIHypothesis
	for _, h := range hyps {
		if h.Status == "draft" || h.Status == "verified" {
			candidates = append(candidates, h)
		}
	}

	priorityOf := func(h RSIHypothesis) int {
		switch h.Origin {
		case "revert":
			return 3
		case "user_correction":
			return 2
		case "ablation":
			return 1
		default:
			return 0
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		pi, pj := priorityOf(candidates[i]), priorityOf(candidates[j])
		if pi != pj {
			return pi > pj
		}
		if candidates[i].ExpectedSavedSteps != candidates[j].ExpectedSavedSteps {
			return candidates[i].ExpectedSavedSteps > candidates[j].ExpectedSavedSteps
		}
		return candidates[i].CreatedAt > candidates[j].CreatedAt
	})

	now := time.Now().UTC()
	for _, cand := range candidates {
		if len(rep.StagedRSIIDs) >= maxProposals {
			break
		}
		verified, vErr := Verify(rootDir, vaultDir, cand.ID)
		if vErr != nil || verified == nil || verified.Verification == nil || !verified.Verification.AllPassed {
			rep.FailedVerifyIDs = append(rep.FailedVerifyIDs, cand.ID)
			continue
		}
		if verified.Verification.RequiresLiveSafetyProbe && (!verified.SafetyProbePassed || verified.SafetyProbeDiffSHA != verified.DiffSHA256) {
			rep.NeedsProbeIDs = append(rep.NeedsProbeIDs, cand.ID)
			continue
		}

		prop, _, pErr := Propose(rootDir, vaultDir, verified.ID, convID, hqBin)
		if pErr != nil || prop == nil {
			continue
		}
		rep.StagedRSIIDs = append(rep.StagedRSIIDs, verified.ID)
		rep.StagedProposalIDs = append(rep.StagedProposalIDs, prop.ID)

		shaShort := prop.SHA256
		if len(shaShort) > 12 {
			shaShort = shaShort[:12]
		}
		card, _, addErr := outbox.Add(vaultDir, outbox.OutboxItem{
			SourceSkill:  "sleep-consolidate",
			SourceConvID: convID,
			Scope:        "rsi",
			Tier:         outbox.TierBriefRollup,
			Title:        "RSI Proposal: " + verified.Title,
			Summary:      fmt.Sprintf("%s | Target: %s | Proposal: %s", verified.RootCause, strings.Join(verified.TargetFiles, ", "), prop.ID),
			IncidentKey:  "rsi-prop:" + verified.ID,
			ProposalIDs:  []string{prop.ID},
			Decisions: []outbox.DecisionOption{
				{
					ID:          "approve",
					Label:       "Approve Rule",
					Kind:        outbox.ActionApproveProposal,
					ProposalID:  prop.ID,
					ProposalSHA: shaShort,
					Recommended: true,
				},
				{
					ID:         "reject",
					Label:      "Reject & Tombstone",
					Kind:       outbox.ActionRejectProposal,
					ProposalID: prop.ID,
					Reason:     "User rejected RSI rule from Morning Brief",
				},
			},
		}, now)
		if addErr == nil && card.ID != "" {
			rep.OutboxCardIDs = append(rep.OutboxCardIDs, card.ID)
		}
	}

	// Emit up to 2 actionable 1-tap spawn advisories for Ring-1 CLI bugs, script compilations, or Tier-C graduations.
	advisories := ListAdvisories(rootDir, vaultDir)
	for i := 0; i < len(advisories) && i < 2; i++ {
		adv := advisories[i]
		safeSlug := sanitizeAdvisorySlug(adv.SkillOrPkg)
		label := "Spawn Fix Task"
		if adv.Kind == "compile_script_or_route" {
			label = "Spawn Script Compile"
		} else if adv.Kind == "tier_c_graduation" {
			label = "Graduate to Go"
		}
		card, _, addErr := outbox.Add(vaultDir, outbox.OutboxItem{
			SourceSkill:  "sleep-consolidate",
			SourceConvID: convID,
			Scope:        "rsi",
			Tier:         outbox.TierBriefRollup,
			Title:        "RSI Advisory: " + adv.Title,
			Summary:      adv.Summary,
			IncidentKey:  "rsi-adv:" + adv.ID,
			Decisions: []outbox.DecisionOption{
				{
					ID:          "spawn",
					Label:       label,
					Kind:        outbox.ActionSpawnInvestigation,
					SkillSlug:   safeSlug,
					Scope:       "rsi",
					Reason:      adv.SpawnPrompt,
					Recommended: true,
				},
				{
					ID:     "dismiss",
					Label:  "Dismiss",
					Kind:   outbox.ActionDismissNoisy,
					Reason: "User dismissed RSI advisory",
				},
			},
		}, now)
		if addErr == nil && card.ID != "" {
			rep.AdvisoryCardIDs = append(rep.AdvisoryCardIDs, card.ID)
		}
	}

	return rep, nil
}

func sanitizeAdvisorySlug(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" || s == "general" || strings.HasPrefix(s, "internal/") || strings.Contains(s, "/") || strings.Contains(s, ",") {
		return "swe-pulse"
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "swe-pulse"
	}
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// CompactReport summarizes a `hq rsi compact` hygiene & legacy-reset pass.
type CompactReport struct {
	ArchivedDrafts     int  `json:"archivedDrafts"`
	ArchivedCanaries   int  `json:"archivedCanaries"`
	RemovedGeneralFile bool `json:"removedGeneralFile"`
	ObservationsBefore int  `json:"observationsBefore"`
	ObservationsAfter  int  `json:"observationsAfter"`
}

// Compact archives stale orphaned drafts (> maxDraftAge, default 7d), archives ghost canaries
// whose target file/rule no longer exists on disk, removes retired general.md, and deduplicates
// observations.jsonl under normalized v2 fingerprints (RFC-0029 Q4).
func Compact(rootDir, vaultDir string, maxDraftAge time.Duration) (*CompactReport, error) {
	if maxDraftAge <= 0 {
		maxDraftAge = 7 * 24 * time.Hour
	}
	now := time.Now().UTC()
	cutoff := now.Add(-maxDraftAge)
	rep := &CompactReport{}

	// 1. Archive stale draft / failed hypotheses into hypotheses/archived/
	hyps, _ := ListHypotheses(vaultDir)
	archHypDir := filepath.Join(HypothesesDir(vaultDir), "archived")
	for _, h := range hyps {
		if h.Status != "draft" && h.Status != "failed" && h.Status != "archived" {
			continue
		}
		stale := h.Status == "archived" || h.Status == "failed"
		if !stale {
			if ts, err := time.Parse(time.RFC3339, h.CreatedAt); err == nil && ts.Before(cutoff) {
				stale = true
			}
		}
		if !stale {
			continue
		}
		h.Status = "archived"
		src := filepath.Join(HypothesesDir(vaultDir), h.ID+".json")
		dst := filepath.Join(archHypDir, h.ID+".json")
		b, err := json.MarshalIndent(h, "", "  ")
		if err != nil {
			continue
		}
		_ = withRSILock(vaultDir, func() error {
			if err := os.MkdirAll(archHypDir, 0o700); err != nil {
				return err
			}
			if err := store.WriteAtomic(dst, append(b, '\n'), 0o600); err != nil {
				return err
			}
			_ = os.Remove(src)
			return nil
		})
		rep.ArchivedDrafts++
	}

	// 2. Archive ghost canaries whose target files / rule blocks no longer exist on disk
	if recs, err := LoadArchive(vaultDir); err == nil && len(recs) > 0 {
		changed := false
		for i := range recs {
			if recs[i].Status == "canary" && !ruleExistsOnDisk(rootDir, vaultDir, recs[i]) {
				recs[i].Status = "archived"
				rep.ArchivedCanaries++
				changed = true
			}
		}
		if changed {
			_ = SaveArchive(vaultDir, recs)
		}
	}

	// 3. Remove retired general.md overlay if present
	genPath := filepath.Join(vaultDir, "memory", "rsi", "rules", "general.md")
	if _, err := os.Stat(genPath); err == nil {
		if rErr := os.Remove(genPath); rErr == nil {
			rep.RemovedGeneralFile = true
		}
	}

	// 4. Deduplicate observations.jsonl by re-normalizing fingerprints under v2 normalization
	if obsList, err := LoadObservations(vaultDir); err == nil {
		rep.ObservationsBefore = len(obsList)
		byFP := make(map[string]*FrictionEvent, len(obsList))
		var order []string
		for _, ev := range obsList {
			sig := ev.CommandPattern
			if sig == "" {
				sig = ev.ErrorExcerpt
			}
			newFP := computeFingerprint(ev.Category, ev.SkillOrPkg, ev.ToolName, sig)
			ev.Fingerprint = newFP
			if cur, ok := byFP[newFP]; ok {
				cur.OccurrenceCount += ev.OccurrenceCount
				if ev.WastedSteps > cur.WastedSteps {
					cur.WastedSteps = ev.WastedSteps
				}
				for _, cid := range ev.ConversationIDs {
					if cid != "" && !containsString(cur.ConversationIDs, cid) {
						cur.ConversationIDs = append(cur.ConversationIDs, cid)
					}
				}
				if ev.LastSeenAt > cur.LastSeenAt {
					cur.LastSeenAt = ev.LastSeenAt
				}
				if cur.FirstSeenAt == "" || (ev.FirstSeenAt != "" && ev.FirstSeenAt < cur.FirstSeenAt) {
					cur.FirstSeenAt = ev.FirstSeenAt
				}
				if ev.Origin == "user_correction" {
					cur.Origin = "user_correction"
				}
				if ev.Status == "applied" {
					cur.Status = "applied"
				}
				cur.ImpactScore = computeImpactScore(cur.OccurrenceCount, cur.WastedSteps, cur.MeanWastedTokens)
				continue
			}
			evCopy := ev
			if evCopy.FailureClass == "" || evCopy.TargetSubstrate == "" {
				fc, sub := classifyFailureAndSubstrate(evCopy.Category, evCopy.SkillOrPkg, evCopy.ToolName, evCopy.CommandPattern, evCopy.ErrorExcerpt)
				evCopy.FailureClass = fc
				evCopy.TargetSubstrate = sub
			}
			evCopy.ImpactScore = computeImpactScore(evCopy.OccurrenceCount, evCopy.WastedSteps, evCopy.MeanWastedTokens)
			byFP[newFP] = &evCopy
			order = append(order, newFP)
		}
		deduped := make([]FrictionEvent, 0, len(order))
		for _, fp := range order {
			deduped = append(deduped, *byFP[fp])
		}
		rep.ObservationsAfter = len(deduped)
		if len(obsList) > 0 {
			_ = SaveObservations(vaultDir, deduped)
		}
	}

	return rep, nil
}

// StatusSummary summarizes the current state of the 4-stage RSI engine.
type StatusSummary struct {
	TotalObservations int                `json:"totalObservations"`
	OpenObservations  int                `json:"openObservations"`
	TotalHypotheses   int                `json:"totalHypotheses"`
	VerifiedReady     int                `json:"verifiedReady"`
	ActiveCanaries    int                `json:"activeCanaries"`
	PromotedPatches   int                `json:"promotedPatches"`
	RejectedTombstone int                `json:"rejectedTombstones"`
	ActiveSkillRules  int                `json:"activeSkillRules"`
	TotalSkills       int                `json:"totalSkills"`
	AdvisoryCount     int                `json:"advisoryCount"`
	QuotaAllowed      bool               `json:"quotaAllowed"`
	QuotaMessage      string             `json:"quotaMessage,omitempty"`
	Observations      []FrictionEvent    `json:"observations,omitempty"`
	Hypotheses        []RSIHypothesis    `json:"hypotheses,omitempty"`
	Archive           []RSIArchiveRecord `json:"archive,omitempty"`
	Advisories        []RSIAdvisory      `json:"advisories,omitempty"`
}

// GetStatus computes the comprehensive RSI engine status summary.
func GetStatus(rootDir, vaultDir string) StatusSummary {
	obs, _ := LoadObservations(vaultDir)
	hyps, _ := LoadHypothesesSafe(vaultDir)
	arch, _ := LoadArchive(vaultDir)
	advisories := ListAdvisories(rootDir, vaultDir)
	quotaOK, quotaMsg := CheckQuotaCircuitBreaker(vaultDir)

	var openObs int
	for _, o := range obs {
		if o.Status == "open" {
			openObs++
		}
	}
	var verifiedReady int
	for _, h := range hyps {
		if h.Status == "verified" || (h.Verification != nil && h.Verification.AllPassed && h.Status != "approved" && h.Status != "rejected" && h.Status != "archived") {
			verifiedReady++
		}
	}
	var canaries, promoted, rejected int
	for _, a := range arch {
		switch a.Status {
		case "canary":
			canaries++
		case "promoted":
			promoted++
		case "rejected", "reverted":
			rejected++
		}
	}

	skills := DiscoverAllSkillFiles(rootDir, vaultDir)
	activeRules := 0
	for _, sp := range skills {
		if b, err := os.ReadFile(sp); err == nil {
			activeRules += len(ParseRSIRules(sp, string(b)))
		}
	}

	return StatusSummary{
		TotalObservations: len(obs),
		OpenObservations:  openObs,
		TotalHypotheses:   len(hyps),
		VerifiedReady:     verifiedReady,
		ActiveCanaries:    canaries,
		PromotedPatches:   promoted,
		RejectedTombstone: rejected,
		ActiveSkillRules:  activeRules,
		TotalSkills:       len(skills),
		AdvisoryCount:     len(advisories),
		QuotaAllowed:      quotaOK,
		QuotaMessage:      quotaMsg,
		Observations:      obs,
		Hypotheses:        hyps,
		Archive:           arch,
		Advisories:        advisories,
	}
}

func isOverlayTargetList(targets []string) bool {
	for _, t := range targets {
		if strings.HasPrefix(filepath.ToSlash(t), "local/memory/rsi/rules/") {
			return true
		}
	}
	return false
}

// LoadHypothesesSafe returns all hypotheses without error if directory doesn't exist.
func LoadHypothesesSafe(vaultDir string) ([]RSIHypothesis, error) {
	return ListHypotheses(vaultDir)
}
