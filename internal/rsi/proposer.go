package rsi

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/vault"
)

// RSIArchiveRecord tracks an applied RSI mutation through its frequency-adaptive post-apply canary window.
type RSIArchiveRecord struct {
	ID                 string   `json:"id"`
	ProposalID         string   `json:"proposalId,omitempty"`
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
	Status             string   `json:"status"` // "canary", "promoted", "reverted"
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

// SaveArchive writes the canonical RSIArchiveRecord slice to local/memory/rsi/archive.jsonl.
func SaveArchive(vaultDir string, recs []RSIArchiveRecord) error {
	p := ArchivePath(vaultDir)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	var sb strings.Builder
	for _, r := range recs {
		b, err := json.Marshal(r)
		if err != nil {
			continue
		}
		sb.Write(b)
		sb.WriteByte('\n')
	}
	return os.WriteFile(p, []byte(sb.String()), 0o644)
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

func updateCanaryRecords(vaultDir string, cleanSkills map[string]int, observedFPs map[string]bool) error {
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
		recurred := false
		for _, fp := range r.SourceFingerprints {
			if observedFPs[fp] {
				recurred = true
				break
			}
		}
		if recurred {
			r.SessionsObserved++
			r.HarmfulCount++
			changed = true
		} else {
			for _, tf := range r.TargetFiles {
				for sk, count := range cleanSkills {
					if count > 0 && strings.Contains(tf, sk) {
						r.SessionsObserved++
						r.HelpfulCount++
						changed = true
						break
					}
				}
			}
		}
		if r.TargetCanaryRuns <= 0 {
			r.TargetCanaryRuns = 3
		}
		if r.SessionsObserved >= r.TargetCanaryRuns && r.HarmfulCount == 0 {
			r.Status = "promoted"
			changed = true
		}
	}
	if changed {
		return SaveArchive(vaultDir, recs)
	}
	return nil
}

// Propose stages a verified RSIHypothesis into local/memory/pending/<propId>.json via approvals.CreateWithMetadata.
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
	}

	summary := fmt.Sprintf("[%s] %s (rsi:%s · diff:%s)", h.Ring.Label(), h.Title, h.ID, shaShort)
	cmdStr := fmt.Sprintf("%s rsi apply %s --expect-diff-sha256=%s", hqBin, h.ID, shaShort)

	diffPreview := strings.TrimSpace(h.ProposedDiff)
	if len(diffPreview) > 800 {
		diffPreview = diffPreview[:800] + "\n..."
	}

	gateSummary := fmt.Sprintf("Gate 1 (TCB/AST): PASS | Gate 2 (Counterfactual): PASS (%s) | Gate 3 (Tests/PII): PASS | Gate 4 (Budget): PASS", h.Verification.CounterfactualBasline)

	meta := map[string]string{
		"rsiId":                h.ID,
		"ring":                 fmt.Sprintf("%d", int(h.Ring)),
		"ringLabel":            h.Ring.Label(),
		"title":                h.Title,
		"faultLayer":           string(h.FaultLayer),
		"targetFiles":          strings.Join(h.TargetFiles, ", "),
		"gateSummary":          gateSummary,
		"diffSha256":           h.DiffSHA256,
		"canonicalDiffPreview": diffPreview,
	}

	prop, err := approvals.CreateWithMetadata(vaultDir, actionType, summary, cmdStr, rootDir, convID, meta)
	if err != nil {
		return nil, h, err
	}

	h.Status = "proposed"
	_ = SaveHypothesis(vaultDir, h)
	return &prop, h, nil
}

// Apply deterministically applies an approved RSIHypothesis after verifying its DiffSHA256,
// taking a pre-apply Vault git snapshot, and recording a canary entry in local/memory/rsi/archive.jsonl.
func Apply(rootDir, vaultDir, rsiID, expectDiffSHA string) (string, error) {
	h, err := LoadHypothesis(vaultDir, rsiID)
	if err != nil {
		return "", fmt.Errorf("load hypothesis %s: %w", rsiID, err)
	}
	if h.Verification == nil || !h.Verification.AllPassed {
		return "", fmt.Errorf("cannot apply unverified hypothesis %s", h.ID)
	}

	actualSum := sha256.Sum256([]byte(h.ProposedDiff))
	actualHex := hex.EncodeToString(actualSum[:])
	if actualHex != h.DiffSHA256 {
		return "", fmt.Errorf("diff integrity mismatch for %s", h.ID)
	}
	expect := strings.ToLower(strings.TrimSpace(expectDiffSHA))
	if expect != "" && !strings.HasPrefix(strings.ToLower(h.DiffSHA256), expect) {
		return "", fmt.Errorf("expect-diff-sha256 mismatch: expected prefix %q, got %q", expect, h.DiffSHA256[:12])
	}

	// Take Dual-Repo Pre-Apply Snapshot in local/.git
	snapshotTaken := vault.SnapshotVault(vaultDir, "pre-rsi-apply-"+h.ID)
	snapRef := "clean"
	if snapshotTaken {
		snapRef = "pre-rsi-apply-" + h.ID
	}

	if h.DeltaOp != nil {
		for _, tf := range h.TargetFiles {
			absPath := tf
			if !filepath.IsAbs(absPath) {
				absPath = filepath.Join(rootDir, filepath.FromSlash(tf))
			}
			rawBytes, err := os.ReadFile(absPath)
			if err != nil {
				return "", fmt.Errorf("read target %s: %w", tf, err)
			}
			after, err := ApplyACEDelta(string(rawBytes), *h.DeltaOp)
			if err != nil {
				return "", fmt.Errorf("apply ACE delta to %s: %w", tf, err)
			}
			if err := os.WriteFile(absPath, []byte(after), 0o644); err != nil {
				return "", fmt.Errorf("write target %s: %w", tf, err)
			}
		}
	}

	h.Status = "approved"
	_ = SaveHypothesis(vaultDir, h)

	// Mark linked observations as applied
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

	// Record canary entry in archive.jsonl
	recs, _ := LoadArchive(vaultDir)
	canaryRuns := targetCanaryRunsForTargets(h.TargetFiles)
	recs = append(recs, RSIArchiveRecord{
		ID:                 h.ID,
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
		Status:             "canary",
	})
	_ = SaveArchive(vaultDir, recs)

	return fmt.Sprintf("Applied RSI patch %s to %s (canary window: %d runs, diff sha256: %s)\n",
		h.ID, strings.Join(h.TargetFiles, ", "), canaryRuns, h.DiffSHA256[:12]), nil
}

// Ablate scans SKILL.md files for model-scoped or stale zero-utility `<!-- rsi:rule -->` blocks
// and stages deprecation hypotheses so upgraded models are never weighed down by obsolete scaffolding.
func Ablate(rootDir, vaultDir, currentModel string) ([]RSIHypothesis, error) {
	now := time.Now().UTC()
	today := now.Format("20060102")
	var staged []RSIHypothesis

	for _, absPath := range DiscoverAllSkillFiles(rootDir) {
		b, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(rootDir, absPath)
		if err != nil {
			rel = absPath
		}
		rel = filepath.ToSlash(rel)

		rules := ParseRSIRules(rel, string(b))
		for _, r := range rules {
			shouldDeprecate := false
			reason := ""
			if r.Harmful > r.Helpful && r.Harmful > 0 {
				shouldDeprecate = true
				reason = fmt.Sprintf("rule %s recorded harmful=%d > helpful=%d", r.ID, r.Harmful, r.Helpful)
			} else if currentModel != "" && r.ModelScope != "" && r.ModelScope != "all" && !strings.EqualFold(r.ModelScope, currentModel) {
				shouldDeprecate = true
				reason = fmt.Sprintf("rule %s was scoped to model %q; ablated for upgraded model %q", r.ID, r.ModelScope, currentModel)
			} else if r.Helpful == 0 && r.Added != "" {
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
				TargetFiles:        []string{rel},
				DeltaOp:            delta,
				ProposedDiff:       diff,
				DiffSHA256:         hex.EncodeToString(sum[:]),
				ExpectedSavedSteps: 2,
				Status:             "draft",
				CreatedAt:          now.Format(time.RFC3339),
			}
			if err := SaveHypothesis(vaultDir, &hyp); err == nil {
				staged = append(staged, hyp)
			}
		}
	}
	return staged, nil
}

// StatusSummary summarizes the current state of the 4-stage RSI engine.
type StatusSummary struct {
	TotalObservations int                `json:"totalObservations"`
	OpenObservations  int                `json:"openObservations"`
	TotalHypotheses   int                `json:"totalHypotheses"`
	VerifiedReady     int                `json:"verifiedReady"`
	ActiveCanaries    int                `json:"activeCanaries"`
	PromotedPatches   int                `json:"promotedPatches"`
	ActiveSkillRules  int                `json:"activeSkillRules"`
	TotalSkills       int                `json:"totalSkills"`
	QuotaAllowed      bool               `json:"quotaAllowed"`
	QuotaMessage      string             `json:"quotaMessage,omitempty"`
	Observations      []FrictionEvent    `json:"observations,omitempty"`
	Hypotheses        []RSIHypothesis    `json:"hypotheses,omitempty"`
	Archive           []RSIArchiveRecord `json:"archive,omitempty"`
}

// GetStatus computes the comprehensive RSI engine status summary.
func GetStatus(rootDir, vaultDir string) StatusSummary {
	obs, _ := LoadObservations(vaultDir)
	hyps, _ := LoadHypothesesSafe(vaultDir)
	arch, _ := LoadArchive(vaultDir)
	quotaOK, quotaMsg := CheckQuotaCircuitBreaker(vaultDir)

	var openObs int
	for _, o := range obs {
		if o.Status == "open" {
			openObs++
		}
	}
	var verifiedReady int
	for _, h := range hyps {
		if h.Status == "verified" || (h.Verification != nil && h.Verification.AllPassed) {
			verifiedReady++
		}
	}
	var canaries, promoted int
	for _, a := range arch {
		switch a.Status {
		case "canary":
			canaries++
		case "promoted":
			promoted++
		}
	}

	skills := DiscoverAllSkillFiles(rootDir)
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
		ActiveSkillRules:  activeRules,
		TotalSkills:       len(skills),
		QuotaAllowed:      quotaOK,
		QuotaMessage:      quotaMsg,
		Observations:      obs,
		Hypotheses:        hyps,
		Archive:           arch,
	}
}

// LoadHypothesesSafe returns all hypotheses without error if directory doesn't exist.
func LoadHypothesesSafe(vaultDir string) ([]RSIHypothesis, error) {
	return ListHypotheses(vaultDir)
}
