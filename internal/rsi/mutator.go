package rsi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// MutationRing defines the 4-Ring Mutability Lattice from RFC-0010.
type MutationRing int

const (
	Ring0ImmutableTCB MutationRing = 0 // Immutable: approvals, tcb.go, verifier.go, .gitignore, .git/hooks
	Ring1KernelCode   MutationRing = 1 // Approval-bound: internal/*, cmd/hq/*, AGENTS.md, hooks.json
	Ring2SkillTrigger MutationRing = 2 // Approval-bound: .agents/plugins/agy-hq/skills/*/SKILL.md, triggers.yaml
	Ring3MemoryWiki   MutationRing = 3 // Auto-consolidated: local/MEMORY.md, local/memory/topics/*.md, facts.jsonl
)

func (r MutationRing) Label() string {
	switch r {
	case Ring0ImmutableTCB:
		return "Ring 0 (Immutable TCB)"
	case Ring1KernelCode:
		return "Ring 1 (Kernel / Hook Code)"
	case Ring2SkillTrigger:
		return "Ring 2 (Skill / Trigger Delta)"
	case Ring3MemoryWiki:
		return "Ring 3 (Memory / Wiki)"
	default:
		return fmt.Sprintf("Ring %d", int(r))
	}
}

// ACEDeltaOp represents an Agentic Context Engineering (ACE) structured delta operation.
type ACEDeltaOp struct {
	Op         string `json:"op"`                   // "add_skill_rule", "patch_skill_rule", "deprecate_skill_rule"
	RuleID     string `json:"ruleId"`               // e.g. "rsi-rule-gchat-01"
	Section    string `json:"section"`              // e.g. "## Learned Operational Rules (RSI Managed)"
	Content    string `json:"content"`              // Markdown bullet with HTML metadata comment
	ModelScope string `json:"modelScope,omitempty"` // e.g. "gemini-3.8-flash-high" or "all"
}

// RSIHypothesis is a candidate self-improvement mutation persisted in local/memory/rsi/hypotheses/<id>.json.
type RSIHypothesis struct {
	ID                 string                `json:"id"`
	ObservationIDs     []string              `json:"observationIds"`
	SourceFingerprints []string              `json:"sourceFingerprints"`
	Ring               MutationRing          `json:"ring"`
	FaultLayer         AttributionFaultLayer `json:"faultLayer"`
	Title              string                `json:"title"`
	RootCause          string                `json:"rootCause"`
	TargetFiles        []string              `json:"targetFiles"`
	DeltaOp            *ACEDeltaOp           `json:"deltaOp,omitempty"`
	ProposedDiff       string                `json:"proposedDiff"`
	DiffSHA256         string                `json:"diffSha256"`
	ReplayTestName     string                `json:"replayTestName,omitempty"`
	ExpectedSavedSteps int                   `json:"expectedSavedSteps"`
	Verification       *VerificationReport   `json:"verification,omitempty"`
	Status             string                `json:"status"` // "draft", "verified", "failed", "proposed", "approved", "rejected"
	CreatedAt          string                `json:"createdAt"`
}

// HypothesesDir returns local/memory/rsi/hypotheses.
func HypothesesDir(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "rsi", "hypotheses")
}

// SaveHypothesis writes an RSIHypothesis to local/memory/rsi/hypotheses/<id>.json.
func SaveHypothesis(vaultDir string, h *RSIHypothesis) error {
	dir := HypothesesDir(vaultDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if h.DiffSHA256 == "" && h.ProposedDiff != "" {
		sum := sha256.Sum256([]byte(h.ProposedDiff))
		h.DiffSHA256 = hex.EncodeToString(sum[:])
	}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, h.ID+".json"), b, 0o644)
}

// LoadHypothesis loads a single RSIHypothesis by ID from local/memory/rsi/hypotheses/<id>.json.
func LoadHypothesis(vaultDir, id string) (*RSIHypothesis, error) {
	id = strings.TrimSuffix(strings.TrimSpace(id), ".json")
	b, err := os.ReadFile(filepath.Join(HypothesesDir(vaultDir), id+".json"))
	if err != nil {
		return nil, err
	}
	var h RSIHypothesis
	if err := json.Unmarshal(b, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// ListHypotheses returns all RSIHypothesis records sorted by ID.
func ListHypotheses(vaultDir string) ([]RSIHypothesis, error) {
	dir := HypothesesDir(vaultDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	var out []RSIHypothesis
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		if h, err := LoadHypothesis(vaultDir, strings.TrimSuffix(e.Name(), ".json")); err == nil {
			out = append(out, *h)
		}
	}
	return out, nil
}

// CheckQuotaCircuitBreaker checks local/memory/quota_state.json and returns an error if rolling 5h/daily
// usage exceeds 75% (unless force is true).
func CheckQuotaCircuitBreaker(vaultDir string) (bool, string) {
	quotaFile := filepath.Join(vaultDir, "memory", "quota_state.json")
	b, err := os.ReadFile(quotaFile)
	if err != nil {
		return true, ""
	}
	var q struct {
		SpawnsToday int     `json:"spawnsToday"`
		DailyLimit  int     `json:"dailyLimit"`
		UsageRatio  float64 `json:"usageRatio"`
	}
	if json.Unmarshal(b, &q) != nil {
		return true, ""
	}
	ratio := q.UsageRatio
	if ratio == 0 && q.DailyLimit > 0 {
		ratio = float64(q.SpawnsToday) / float64(q.DailyLimit)
	}
	if ratio >= 0.75 {
		return false, fmt.Sprintf("RSI Quota Circuit Breaker active: rolling quota utilization is %.0f%% (>= 75%% threshold); deferring non-urgent RSI synthesis to preserve interactive user capacity", ratio*100)
	}
	return true, ""
}

// ResolveSkillFile locates the SKILL.md path for a skill slug in rootDir (checking .agents/plugins/agy-hq/skills/ first, then .agents/skills/).
func ResolveSkillFile(rootDir, skillSlug string) (string, string) {
	relPlugin := filepath.ToSlash(filepath.Join(".agents", "plugins", "agy-hq", "skills", skillSlug, "SKILL.md"))
	absPlugin := filepath.Join(rootDir, filepath.FromSlash(relPlugin))
	if _, err := os.Stat(absPlugin); err == nil {
		return relPlugin, absPlugin
	}
	relLegacy := filepath.ToSlash(filepath.Join(".agents", "skills", skillSlug, "SKILL.md"))
	absLegacy := filepath.Join(rootDir, filepath.FromSlash(relLegacy))
	if _, err := os.Stat(absLegacy); err == nil {
		return relLegacy, absLegacy
	}
	return relPlugin, absPlugin
}

// DiscoverAllSkillFiles returns all relative and absolute SKILL.md paths in rootDir.
func DiscoverAllSkillFiles(rootDir string) []string {
	var paths []string
	seen := make(map[string]bool)
	for _, base := range []string{
		filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills"),
		filepath.Join(rootDir, ".agents", "skills"),
	} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(base, e.Name(), "SKILL.md")
			if _, err := os.Stat(p); err == nil && !seen[p] {
				seen[p] = true
				paths = append(paths, p)
			}
		}
	}
	sort.Strings(paths)
	return paths
}

const LearnedRulesHeading = "## Learned Operational Rules (RSI Managed)"

// ApplyACEDelta applies an ACEDeltaOp (add_skill_rule, patch_skill_rule, deprecate_skill_rule)
// to a SKILL.md document without wholesale rewrites.
func ApplyACEDelta(skillContent string, op ACEDeltaOp) (string, error) {
	if strings.TrimSpace(op.RuleID) == "" {
		return "", fmt.Errorf("ACE delta ruleId is required")
	}
	section := strings.TrimSpace(op.Section)
	if section == "" {
		section = LearnedRulesHeading
	}

	ruleMarker := fmt.Sprintf(`<!-- rsi:rule id=%q`, op.RuleID)

	switch op.Op {
	case "add_skill_rule":
		if strings.Contains(skillContent, ruleMarker) {
			return ApplyACEDelta(skillContent, ACEDeltaOp{
				Op:         "patch_skill_rule",
				RuleID:     op.RuleID,
				Section:    section,
				Content:    op.Content,
				ModelScope: op.ModelScope,
			})
		}
		block := strings.TrimSpace(op.Content)
		if !strings.Contains(block, section) {
			if strings.Contains(skillContent, section) {
				idx := strings.Index(skillContent, section)
				insertPos := idx + len(section)
				rest := skillContent[insertPos:]
				return skillContent[:insertPos] + "\n\n" + block + rest, nil
			}
			trimmed := strings.TrimRight(skillContent, "\n")
			return trimmed + "\n\n" + section + "\n\n" + block + "\n", nil
		}
		trimmed := strings.TrimRight(skillContent, "\n")
		return trimmed + "\n\n" + block + "\n", nil

	case "patch_skill_rule":
		updated, replaced := replaceOrRemoveRSIRuleBlock(skillContent, op.RuleID, strings.TrimSpace(op.Content))
		if !replaced {
			return "", fmt.Errorf("rule %q not found for patch_skill_rule", op.RuleID)
		}
		return updated, nil

	case "deprecate_skill_rule":
		updated, removed := replaceOrRemoveRSIRuleBlock(skillContent, op.RuleID, "")
		if !removed {
			return "", fmt.Errorf("rule %q not found for deprecate_skill_rule", op.RuleID)
		}
		return updated, nil

	default:
		return "", fmt.Errorf("unsupported ACEDeltaOp %q", op.Op)
	}
}

func replaceOrRemoveRSIRuleBlock(content, ruleID, replacement string) (string, bool) {
	lines := strings.Split(content, "\n")
	marker := fmt.Sprintf(`<!-- rsi:rule id=%q`, ruleID)
	var out []string
	found := false
	i := 0
	for i < len(lines) {
		if strings.Contains(lines[i], marker) {
			found = true
			if replacement != "" {
				out = append(out, strings.Split(replacement, "\n")...)
			}
			i++
			// Skip contiguous bullet/continuation lines belonging to this rule
			for i < len(lines) {
				trimmed := strings.TrimSpace(lines[i])
				if trimmed == "" || strings.HasPrefix(trimmed, "<!-- rsi:rule") || strings.HasPrefix(trimmed, "## ") {
					if trimmed == "" && replacement == "" {
						i++
					}
					break
				}
				i++
			}
			continue
		}
		out = append(out, lines[i])
		i++
	}
	return strings.Join(out, "\n"), found
}

// BuildUnifiedDiff produces a compact unified diff preview between before and after for relPath.
func BuildUnifiedDiff(relPath, before, after string) string {
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("--- a/%s\n+++ b/%s\n", filepath.ToSlash(relPath), filepath.ToSlash(relPath)))
	beforeLines := strings.Split(strings.TrimRight(before, "\n"), "\n")
	afterLines := strings.Split(strings.TrimRight(after, "\n"), "\n")

	beforeSet := make(map[string]int, len(beforeLines))
	for _, l := range beforeLines {
		beforeSet[l]++
	}
	afterSet := make(map[string]int, len(afterLines))
	for _, l := range afterLines {
		afterSet[l]++
	}

	sb.WriteString(fmt.Sprintf("@@ -%d,%d +%d,%d @@\n", 1, len(beforeLines), 1, len(afterLines)))
	for _, l := range beforeLines {
		if afterSet[l] == 0 {
			sb.WriteString("-" + l + "\n")
		} else {
			afterSet[l]--
		}
	}
	for _, l := range afterLines {
		if beforeSet[l] == 0 {
			sb.WriteString("+" + l + "\n")
		} else {
			beforeSet[l]--
		}
	}
	return sb.String()
}

// Hypothesize executes Stage 2 (Bounded Hypothesis & Mutation Synthesis), converting high-ROI
// open FrictionEvents into structured RSIHypothesis specifications in local/memory/rsi/hypotheses/.
func Hypothesize(rootDir, vaultDir string, targetObsID string, force bool) ([]RSIHypothesis, error) {
	if ok, reason := CheckQuotaCircuitBreaker(vaultDir); !ok && !force {
		return nil, fmt.Errorf("%s", reason)
	}

	observations, err := LoadObservations(vaultDir)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	today := now.Format("20060102")
	var created []RSIHypothesis
	modifiedObs := false

	for i := range observations {
		ev := &observations[i]
		if targetObsID != "" && ev.ID != targetObsID && ev.Fingerprint != targetObsID {
			continue
		}
		if targetObsID == "" && ev.Status != "open" {
			continue
		}

		expectedSaved := ev.OccurrenceCount * ev.WastedSteps
		highPriority := ev.Category == FrictionUserIntervention || ev.Category == FrictionDraftEdited
		if !force && targetObsID == "" && !highPriority && expectedSaved < 4 {
			continue
		}

		skillSlug := ev.SkillOrPkg
		if strings.HasPrefix(skillSlug, "internal/") || skillSlug == "" {
			skillSlug = "swe-pulse"
		}
		relSkill, absSkill := ResolveSkillFile(rootDir, skillSlug)
		skillBytes, err := os.ReadFile(absSkill)
		if err != nil {
			// Fallback to swe-pulse if the inferred skill directory does not exist
			skillSlug = "swe-pulse"
			relSkill, absSkill = ResolveSkillFile(rootDir, skillSlug)
			skillBytes, _ = os.ReadFile(absSkill)
		}

		ruleID := fmt.Sprintf("rsi-rule-%s-%s", skillSlug, ev.Fingerprint[:6])
		modelScope := ev.ModelName
		if modelScope == "" {
			modelScope = "all"
		}

		guidance := ev.RecoveryAction
		if strings.TrimSpace(guidance) == "" {
			guidance = fmt.Sprintf("Avoid repeating `%s` error (%s); verify flags and pre-conditions before invoking `%s`.", ev.CommandPattern, ev.ErrorExcerpt, ev.ToolName)
		}
		if skillSlug == "thesis-check" || skillSlug == "filing-diff" {
			if !strings.Contains(strings.ToLower(guidance), "kill criteria") && !strings.Contains(strings.ToLower(guidance), "falsif") {
				guidance += " Always evaluate disconfirming evidence, bear case risks, and explicit kill criteria falsification."
			}
		}

		ruleBlock := fmt.Sprintf(
			"<!-- rsi:rule id=%q source=%q model_scope=%q added=%q helpful=\"0\" harmful=\"0\" -->\n- **[Learned Rule · %s]:** When using `%s` in `%s`, %s *(Observed %dx; prevents: %s)*",
			ruleID,
			ev.ID,
			modelScope,
			now.Format("2006-01-02"),
			ev.FaultLayer,
			ev.ToolName,
			skillSlug,
			guidance,
			ev.OccurrenceCount,
			ev.ErrorExcerpt,
		)

		delta := &ACEDeltaOp{
			Op:         "add_skill_rule",
			RuleID:     ruleID,
			Section:    LearnedRulesHeading,
			Content:    ruleBlock,
			ModelScope: modelScope,
		}

		afterContent, err := ApplyACEDelta(string(skillBytes), *delta)
		if err != nil {
			continue
		}
		diff := BuildUnifiedDiff(relSkill, string(skillBytes), afterContent)
		sum := sha256.Sum256([]byte(diff))
		diffSHA := hex.EncodeToString(sum[:])

		hID := fmt.Sprintf("rsi-%s-%s", today, ev.Fingerprint[:6])
		hyp := RSIHypothesis{
			ID:                 hID,
			ObservationIDs:     []string{ev.ID},
			SourceFingerprints: []string{ev.Fingerprint},
			Ring:               Ring2SkillTrigger,
			FaultLayer:         ev.FaultLayer,
			Title:              fmt.Sprintf("Add learned %s rule %s (%s)", skillSlug, ruleID, ev.Category),
			RootCause:          fmt.Sprintf("Ground-truth telemetry recorded %d occurrence(s) of %s in %s (%s)", ev.OccurrenceCount, ev.Category, skillSlug, ev.ErrorExcerpt),
			TargetFiles:        []string{relSkill},
			DeltaOp:            delta,
			ProposedDiff:       diff,
			DiffSHA256:         diffSHA,
			ExpectedSavedSteps: expectedSaved,
			Status:             "draft",
			CreatedAt:          now.Format(time.RFC3339),
		}

		if err := SaveHypothesis(vaultDir, &hyp); err != nil {
			return created, err
		}
		ev.Status = "hypothesized"
		modifiedObs = true
		created = append(created, hyp)
	}

	if modifiedObs {
		_ = SaveObservations(vaultDir, observations)
	}

	// Check for LATM Tier B -> Tier C Skill-to-Code Promotion candidates (helpful >= 5)
	if promos := findTierCPromotionCandidates(rootDir, vaultDir, today, now); len(promos) > 0 {
		created = append(created, promos...)
	}

	return created, nil
}

var (
	reRSIRuleComment = regexp.MustCompile(`<!--\s*rsi:rule\s+([^>]+)-->`)
	reRuleKV         = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

// ParsedRSIRule represents a parsed `<!-- rsi:rule ... -->` metadata block in a SKILL.md file.
type ParsedRSIRule struct {
	ID         string
	Source     string
	ModelScope string
	Added      string
	Helpful    int
	Harmful    int
	SkillFile  string
}

// ParseRSIRules extracts all `<!-- rsi:rule ... -->` blocks from a SKILL.md string.
func ParseRSIRules(skillFile, content string) []ParsedRSIRule {
	matches := reRSIRuleComment.FindAllStringSubmatch(content, -1)
	var out []ParsedRSIRule
	for _, m := range matches {
		attrs := m[1]
		kvs := reRuleKV.FindAllStringSubmatch(attrs, -1)
		r := ParsedRSIRule{SkillFile: skillFile}
		for _, kv := range kvs {
			switch kv[1] {
			case "id":
				r.ID = kv[2]
			case "source":
				r.Source = kv[2]
			case "model_scope":
				r.ModelScope = kv[2]
			case "added":
				r.Added = kv[2]
			case "helpful":
				r.Helpful, _ = strconv.Atoi(kv[2])
			case "harmful":
				r.Harmful, _ = strconv.Atoi(kv[2])
			}
		}
		if r.ID != "" {
			out = append(out, r)
		}
	}
	return out
}

func findTierCPromotionCandidates(rootDir, vaultDir, today string, now time.Time) []RSIHypothesis {
	var promos []RSIHypothesis
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
		for _, r := range ParseRSIRules(rel, string(b)) {
			if r.Helpful >= 5 && r.Harmful == 0 {
				hID := fmt.Sprintf("rsi-%s-promote-%s", today, r.ID)
				if _, err := LoadHypothesis(vaultDir, hID); err == nil {
					continue
				}
				after, err := ApplyACEDelta(string(b), ACEDeltaOp{
					Op:     "deprecate_skill_rule",
					RuleID: r.ID,
				})
				if err != nil {
					continue
				}
				diff := BuildUnifiedDiff(rel, string(b), after)
				sum := sha256.Sum256([]byte(diff))
				hyp := RSIHypothesis{
					ID:                 hID,
					ObservationIDs:     []string{r.Source},
					Ring:               Ring2SkillTrigger,
					FaultLayer:         FaultWorkerACIOrSkill,
					Title:              fmt.Sprintf("[LATM Tier B->C Promotion] Graduate proven skill rule %s (helpful=%d) to deterministic Go check", r.ID, r.Helpful),
					RootCause:          fmt.Sprintf("Skill rule %s has been verified helpful %d times with 0 regressions; ready to graduate to 0-token Go code and reclaim SKILL.md token budget", r.ID, r.Helpful),
					TargetFiles:        []string{rel},
					DeltaOp:            &ACEDeltaOp{Op: "deprecate_skill_rule", RuleID: r.ID, Section: LearnedRulesHeading},
					ProposedDiff:       diff,
					DiffSHA256:         hex.EncodeToString(sum[:]),
					ExpectedSavedSteps: r.Helpful * 2,
					Status:             "draft",
					CreatedAt:          now.Format(time.RFC3339),
				}
				_ = SaveHypothesis(vaultDir, &hyp)
				promos = append(promos, hyp)
			}
		}
	}
	return promos
}
