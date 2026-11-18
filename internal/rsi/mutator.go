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

	"github.com/zjshen14/agy-hq/internal/vault"
)

// MutationRing defines the 4-Ring Mutability Lattice from RFC-0010.
type MutationRing int

const (
	Ring0ImmutableTCB MutationRing = 0 // Immutable: approvals, tcb.go, verifier.go, .gitignore, .git/hooks
	Ring1KernelCode   MutationRing = 1 // Approval-bound: internal/*, cmd/hq/*, AGENTS.md, hooks.json
	Ring2SkillTrigger MutationRing = 2 // Approval-bound: Vault skills/overlays (Tier 1) & opt-in --upstream skills (Tier 2)
	Ring3MemoryWiki   MutationRing = 3 // Auto-consolidated: local/MEMORY.md, local/memory/topics/*.md, facts.jsonl
)

// NonOverridableQuarantineSkills cannot be overridden or mutated via personal RSI overlays.
var NonOverridableQuarantineSkills = map[string]bool{
	"inbox-triage":              true,
	"quarantined-digest-reader": true,
}

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
	ModelScope string `json:"modelScope,omitempty"` // e.g. "gemini-4-hydrogen-high" or "all"
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
	Status             string                `json:"status"`                // "draft", "verified", "failed", "proposed", "approved", "rejected"
	Tier               string                `json:"tier,omitempty"`        // "personal" (Tier 1 default) or "upstream" (Tier 2)
	WorktreeDir        string                `json:"worktreeDir,omitempty"` // Isolated worktree path for Tier 2 upstream hypotheses
	Branch             string                `json:"branch,omitempty"`      // Git branch hq/rsi-<id> for Tier 2 upstream hypotheses
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

// ResolveHypothesisTargetPath maps a hypothesis target path ("local/skills/...", "local/memory/rsi/rules/...",
// or repository/worktree relative path) to an absolute path while strictly enforcing containment inside vaultDir or rootDir/WorktreeDir.
func ResolveHypothesisTargetPath(rootDir, vaultDir string, h *RSIHypothesis, tf string) (string, error) {
	if strings.TrimSpace(tf) == "" || filepath.IsAbs(tf) {
		return "", fmt.Errorf("invalid target file path %q", tf)
	}
	slash := filepath.ToSlash(filepath.Clean(tf))
	slash = strings.TrimPrefix(slash, "./")
	if slash == "." || slash == ".." || strings.HasPrefix(slash, "../") {
		return "", fmt.Errorf("target path %q escapes root", tf)
	}

	if strings.HasPrefix(slash, "local/") {
		cleanVault, err := filepath.Abs(vaultDir)
		if err != nil {
			return "", fmt.Errorf("resolve vaultDir: %w", err)
		}
		if evalVault, err := filepath.EvalSymlinks(cleanVault); err == nil {
			cleanVault = evalVault
		}
		sub := strings.TrimPrefix(slash, "local/")
		absTarget := filepath.Clean(filepath.Join(cleanVault, filepath.FromSlash(sub)))
		if evalTarget, err := filepath.EvalSymlinks(absTarget); err == nil {
			absTarget = evalTarget
		}
		rel, err := filepath.Rel(cleanVault, absTarget)
		if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
			return "", fmt.Errorf("target file %q escapes Vault root", tf)
		}
		return absTarget, nil
	}

	baseRoot := rootDir
	if h != nil && h.Tier == "upstream" && strings.TrimSpace(h.WorktreeDir) != "" {
		baseRoot = h.WorktreeDir
	}
	cleanRoot, err := filepath.Abs(baseRoot)
	if err != nil {
		return "", fmt.Errorf("resolve rootDir: %w", err)
	}
	if evalRoot, err := filepath.EvalSymlinks(cleanRoot); err == nil {
		cleanRoot = evalRoot
	}
	absTarget := filepath.Clean(filepath.Join(cleanRoot, filepath.FromSlash(slash)))
	if evalTarget, err := filepath.EvalSymlinks(absTarget); err == nil {
		absTarget = evalTarget
	}
	rel, err := filepath.Rel(cleanRoot, absTarget)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("target file %q escapes repository root", tf)
	}
	return absTarget, nil
}

// OverlaySkeleton returns the minimal markdown header for a new personal RSI overlay rule file.
func OverlaySkeleton(skillSlug string) string {
	slug := strings.TrimSpace(skillSlug)
	if slug == "" {
		slug = "general"
	}
	return fmt.Sprintf("# Personal RSI Overlay: %s\n", slug)
}

// EnsureOverlaySkeletonContent reads absPath if present on disk; if missing and relTarget
// is a personal RSI overlay path (local/memory/rsi/rules/<slug>.md), returns its skeleton header.
func EnsureOverlaySkeletonContent(absPath, relTarget string) (string, error) {
	if b, err := os.ReadFile(absPath); err == nil {
		return string(b), nil
	} else if !os.IsNotExist(err) {
		return "", err
	}
	slash := filepath.ToSlash(relTarget)
	if strings.HasPrefix(slash, "local/memory/rsi/rules/") {
		slug := strings.TrimSuffix(filepath.Base(slash), ".md")
		return OverlaySkeleton(slug), nil
	}
	return "", os.ErrNotExist
}

// ResolveTier1Target selects the Tier 1 personal mutation path for skillSlug inside vaultDir:
// - Tier 1A: local/skills/<slug>/SKILL.md when installed in Vault user space (and not a Core OS skill)
// - Tier 1B: local/memory/rsi/rules/<slug>.md for Core OS skills, "general", or uninstalled templates
// Quarantine skills (inbox-triage, quarantined-digest-reader) are unconditionally rejected.
func ResolveTier1Target(vaultDir, skillSlug string) (relTarget string, absTarget string, isOverlay bool, err error) {
	slug := strings.TrimSpace(skillSlug)
	if slug == "" || strings.HasPrefix(slug, "internal/") {
		slug = "general"
	}
	if NonOverridableQuarantineSkills[slug] {
		return "", "", false, fmt.Errorf("quarantine skill %q is non-overridable", slug)
	}
	if !vault.IsCoreOSSkill(slug) {
		vaultSkillAbs := filepath.Join(vaultDir, "skills", slug, "SKILL.md")
		if _, sErr := os.Stat(vaultSkillAbs); sErr == nil {
			return filepath.ToSlash(filepath.Join("local", "skills", slug, "SKILL.md")), vaultSkillAbs, false, nil
		}
	}
	relOverlay := filepath.ToSlash(filepath.Join("local", "memory", "rsi", "rules", slug+".md"))
	absOverlay := filepath.Join(vaultDir, "memory", "rsi", "rules", slug+".md")
	return relOverlay, absOverlay, true, nil
}

// ResolveSkillFile locates the SKILL.md path for a skill slug (checking Vault skills if vaultDir
// is supplied, then .agents/plugins/agy-hq/skills/, then templates/skills/, then .agents/skills/).
func ResolveSkillFile(rootDir, skillSlug string, vaultDir ...string) (string, string) {
	if len(vaultDir) > 0 && strings.TrimSpace(vaultDir[0]) != "" {
		absVaultSkill := filepath.Join(vaultDir[0], "skills", skillSlug, "SKILL.md")
		if _, err := os.Stat(absVaultSkill); err == nil {
			return filepath.ToSlash(filepath.Join("local", "skills", skillSlug, "SKILL.md")), absVaultSkill
		}
	}
	relPlugin := filepath.ToSlash(filepath.Join(".agents", "plugins", "agy-hq", "skills", skillSlug, "SKILL.md"))
	absPlugin := filepath.Join(rootDir, filepath.FromSlash(relPlugin))
	if _, err := os.Stat(absPlugin); err == nil {
		return relPlugin, absPlugin
	}
	relTemplate := filepath.ToSlash(filepath.Join("templates", "skills", skillSlug, "SKILL.md"))
	absTemplate := filepath.Join(rootDir, filepath.FromSlash(relTemplate))
	if _, err := os.Stat(absTemplate); err == nil {
		return relTemplate, absTemplate
	}
	relLegacy := filepath.ToSlash(filepath.Join(".agents", "skills", skillSlug, "SKILL.md"))
	absLegacy := filepath.Join(rootDir, filepath.FromSlash(relLegacy))
	if _, err := os.Stat(absLegacy); err == nil {
		return relLegacy, absLegacy
	}
	return relPlugin, absPlugin
}

// DiscoverAllSkillFiles returns all SKILL.md and overlay rule markdown paths across rootDir and optional vaultDir.
func DiscoverAllSkillFiles(rootDir string, vaultDir ...string) []string {
	var paths []string
	seen := make(map[string]bool)
	addFile := func(p string) {
		clean := filepath.Clean(p)
		if !seen[clean] {
			if _, err := os.Stat(clean); err == nil {
				seen[clean] = true
				paths = append(paths, clean)
			}
		}
	}

	var bases []string
	if len(vaultDir) > 0 && strings.TrimSpace(vaultDir[0]) != "" {
		v := vaultDir[0]
		bases = append(bases, filepath.Join(v, "skills"))
		rulesDir := filepath.Join(v, "memory", "rsi", "rules")
		if rEntries, rErr := os.ReadDir(rulesDir); rErr == nil {
			for _, re := range rEntries {
				if !re.IsDir() && strings.HasSuffix(re.Name(), ".md") {
					addFile(filepath.Join(rulesDir, re.Name()))
				}
			}
		}
	}
	if rootDir != "" {
		bases = append(bases,
			filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills"),
			filepath.Join(rootDir, ".agents", "skills"),
		)
	}

	for _, base := range bases {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			addFile(filepath.Join(base, e.Name(), "SKILL.md"))
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

// HypothesizeOptions controls Tier 1 (default personal Vault) vs Tier 2 (--upstream worktree) synthesis.
type HypothesizeOptions struct {
	Force    bool
	Upstream bool
}

// HypothesizeWithOptions synthesizes either Tier 1 personal Vault mutations (default)
// or Tier 2 upstream contributor PR mutations in an isolated git worktree (`--upstream`).
func HypothesizeWithOptions(rootDir, vaultDir string, targetObsID string, opts HypothesizeOptions) ([]RSIHypothesis, error) {
	if ok, reason := CheckQuotaCircuitBreaker(vaultDir); !ok && !opts.Force {
		return nil, fmt.Errorf("%s", reason)
	}
	if opts.Upstream && !vault.IsAgyHQRepo(rootDir) {
		return nil, fmt.Errorf("--upstream requires running inside an agy-hq repository checkout")
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
		if targetObsID == "" && ev.Tainted {
			continue
		}

		expectedSaved := ev.OccurrenceCount * ev.WastedSteps
		highPriority := ev.Category == FrictionUserIntervention || ev.Category == FrictionDraftEdited
		if !opts.Force && targetObsID == "" && !highPriority && expectedSaved < 4 {
			continue
		}

		skillSlug := strings.TrimSpace(ev.SkillOrPkg)
		if strings.HasPrefix(skillSlug, "internal/") || skillSlug == "" {
			skillSlug = "general"
		}
		if NonOverridableQuarantineSkills[skillSlug] {
			if targetObsID != "" {
				return created, fmt.Errorf("quarantine skill %q is non-overridable; cannot synthesize RSI overlay", skillSlug)
			}
			continue
		}

		var relTarget string
		var absTarget string
		var beforeContent string
		var tier string
		var worktreeDir string
		var branchName string

		if opts.Upstream {
			tier = "upstream"
			relUp, _ := ResolveSkillFile(rootDir, skillSlug)
			if strings.HasPrefix(relUp, "local/") || skillSlug == "general" {
				if targetObsID != "" {
					return created, fmt.Errorf("skill %q has no tracked upstream Core or starter template SKILL.md target", skillSlug)
				}
				continue
			}
			if _, sErr := os.Stat(filepath.Join(rootDir, filepath.FromSlash(relUp))); sErr != nil {
				if targetObsID != "" {
					return created, fmt.Errorf("upstream target %s not found in %s", relUp, rootDir)
				}
				continue
			}
			wtDir, br, wErr := ensureUpstreamRSIWorktree(rootDir, vaultDir, ev.Fingerprint[:6])
			if wErr != nil {
				return created, wErr
			}
			worktreeDir = wtDir
			branchName = br
			relTarget = relUp
			absTarget = filepath.Join(wtDir, filepath.FromSlash(relUp))
			rawBytes, rErr := os.ReadFile(absTarget)
			if rErr != nil {
				return created, fmt.Errorf("read upstream target %s in worktree: %w", relUp, rErr)
			}
			beforeContent = string(rawBytes)
		} else {
			tier = "personal"
			rRel, rAbs, _, rErr := ResolveTier1Target(vaultDir, skillSlug)
			if rErr != nil {
				continue
			}
			relTarget = rRel
			absTarget = rAbs
			c, cErr := EnsureOverlaySkeletonContent(absTarget, relTarget)
			if cErr != nil {
				continue
			}
			beforeContent = c
		}

		ruleID := fmt.Sprintf("rsi-rule-%s-%s", skillSlug, ev.Fingerprint[:6])
		modelScope := ev.ModelName
		if modelScope == "" {
			modelScope = "all"
		}

		guidance := sanitizePIIForTrackedSkill(rootDir, ev.RecoveryAction)
		cleanExcerpt := sanitizePIIForTrackedSkill(rootDir, ev.ErrorExcerpt)
		cleanCmdPat := sanitizePIIForTrackedSkill(rootDir, ev.CommandPattern)
		if strings.TrimSpace(guidance) == "" {
			guidance = fmt.Sprintf("Avoid repeating `%s` error (%s); verify flags and pre-conditions before invoking `%s`.", cleanCmdPat, cleanExcerpt, ev.ToolName)
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
			cleanExcerpt,
		)

		delta := &ACEDeltaOp{
			Op:         "add_skill_rule",
			RuleID:     ruleID,
			Section:    LearnedRulesHeading,
			Content:    ruleBlock,
			ModelScope: modelScope,
		}

		afterContent, err := ApplyACEDelta(beforeContent, *delta)
		if err != nil {
			continue
		}
		diff := BuildUnifiedDiff(relTarget, beforeContent, afterContent)
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
			TargetFiles:        []string{relTarget},
			DeltaOp:            delta,
			ProposedDiff:       diff,
			DiffSHA256:         diffSHA,
			ExpectedSavedSteps: expectedSaved,
			Status:             "draft",
			Tier:               tier,
			WorktreeDir:        worktreeDir,
			Branch:             branchName,
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
	if !opts.Upstream {
		if promos := findTierCPromotionCandidates(rootDir, vaultDir, today, now); len(promos) > 0 {
			created = append(created, promos...)
		}
	}

	return created, nil
}

var (
	reRSIRuleComment = regexp.MustCompile(`<!--\s*rsi:rule\s+([^>]+)-->`)
	reRuleKV         = regexp.MustCompile(`(\w+)="([^"]*)"`)
)

func sanitizePIIForTrackedSkill(rootDir, s string) string {
	if rootDir != "" && len(rootDir) > 5 {
		s = strings.ReplaceAll(s, rootDir, "<repo-root>")
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" && len(home) > 5 {
		s = strings.ReplaceAll(s, home, "<home>")
	}
	s = reEmail.ReplaceAllStringFunc(s, func(m string) string {
		lower := strings.ToLower(m)
		if strings.HasSuffix(lower, "@example.com") || lower == "user@gmail.com" || lower == "you@gmail.com" {
			return m
		}
		return "user@gmail.com"
	})
	return s
}

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

func targetRelativePath(rootDir, vaultDir, absPath string) string {
	if vaultDir != "" {
		cleanV := filepath.Clean(vaultDir)
		if rel, err := filepath.Rel(cleanV, absPath); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(filepath.Join("local", rel))
		}
	}
	if rootDir != "" {
		cleanR := filepath.Clean(rootDir)
		if rel, err := filepath.Rel(cleanR, absPath); err == nil && rel != "." && !strings.HasPrefix(rel, "..") {
			return filepath.ToSlash(rel)
		}
	}
	return filepath.ToSlash(absPath)
}

func findTierCPromotionCandidates(rootDir, vaultDir, today string, now time.Time) []RSIHypothesis {
	var promos []RSIHypothesis
	for _, absPath := range DiscoverAllSkillFiles("", vaultDir) {
		b, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		rel := targetRelativePath(rootDir, vaultDir, absPath)
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
