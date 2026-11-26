package rsi

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/redact"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/transcript"
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
// Single canonical source of truth in internal/extension.
var NonOverridableQuarantineSkills = extension.NonOverridableCoreSkills

// ParsedRSIRule aliases the canonical parser struct in internal/extension.
type ParsedRSIRule = extension.ParsedRSIRule

// ParseRSIRules delegates to internal/extension.ParseRSIRules (single canonical parser).
func ParseRSIRules(skillFile, content string) []ParsedRSIRule {
	return extension.ParseRSIRules(skillFile, content)
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
	EditRationale      string                `json:"editRationale,omitempty"`
	PredictedOutcome   string                `json:"predictedOutcome,omitempty"`
	Origin             string                `json:"origin,omitempty"` // "auto_mined" | "user_correction" | "ablation" | "revert"
	TargetFiles        []string              `json:"targetFiles"`
	DeltaOp            *ACEDeltaOp           `json:"deltaOp,omitempty"`
	ProposedDiff       string                `json:"proposedDiff"`
	DiffSHA256         string                `json:"diffSha256"`
	ReplayTestName     string                `json:"replayTestName,omitempty"`
	ExpectedSavedSteps int                   `json:"expectedSavedSteps"`
	Verification       *VerificationReport   `json:"verification,omitempty"`
	SafetyProbePassed  bool                  `json:"safetyProbePassed,omitempty"`
	SafetyProbeDiffSHA string                `json:"safetyProbeDiffSha256,omitempty"`
	SafetyProbeSummary string                `json:"safetyProbeSummary,omitempty"`
	Status             string                `json:"status"`                // "draft", "verified", "failed", "proposed", "approved", "rejected", "archived"
	Tier               string                `json:"tier,omitempty"`        // "personal" (Tier 1 default) or "upstream" (Tier 2)
	WorktreeDir        string                `json:"worktreeDir,omitempty"` // Isolated worktree path for Tier 2 upstream hypotheses
	Branch             string                `json:"branch,omitempty"`      // Git branch hq/rsi-<id> for Tier 2 upstream hypotheses
	CreatedAt          string                `json:"createdAt"`
}

// RSIAdvisory represents a non-destructive actionable recommendation (Ring-1 CLI fix,
// workflow script compilation, or LATM Tier B->C graduation) surfaced as a 1-tap spawn card.
type RSIAdvisory struct {
	ID          string `json:"id"`
	Kind        string `json:"kind"` // "ring1_cli_triage" | "compile_script_or_route" | "tier_c_graduation"
	SkillOrPkg  string `json:"skillOrPkg"`
	Title       string `json:"title"`
	Summary     string `json:"summary"`
	ImpactScore int64  `json:"impactScore"`
	SpawnPrompt string `json:"spawnPrompt"`
	ObsID       string `json:"observationId,omitempty"`
	RuleID      string `json:"ruleId,omitempty"`
}

// ObservationContext is the bounded, PII-scrubbed, taint-verified evidence packet returned by
// `hq rsi context <obs-id> --json` for grounded semantic synthesis (0 extra files on disk).
type ObservationContext struct {
	Observation        FrictionEvent `json:"observation"`
	TargetSkill        string        `json:"targetSkill"`
	TargetFile         string        `json:"targetFile,omitempty"`
	CurrentTargetText  string        `json:"currentTargetText,omitempty"`
	TranscriptExcerpts []string      `json:"transcriptExcerpts,omitempty"`
	PriorTombstones    []string      `json:"priorTombstones,omitempty"`
	ActiveRuleCount    int           `json:"activeRuleCount"`
	MaxRulesForTarget  int           `json:"maxRulesForTarget"`
}

// HypothesisInput is the structured JSON schema accepted by `hq rsi hypothesize --from-json`.
type HypothesisInput struct {
	ObservationID    string `json:"observationId"`
	SkillSlug        string `json:"skillSlug"`
	RuleID           string `json:"ruleId,omitempty"`
	Title            string `json:"title"`
	RootCause        string `json:"rootCause"`
	EditRationale    string `json:"editRationale"`
	PredictedOutcome string `json:"predictedOutcome"`
	RuleText         string `json:"ruleText"`
	ModelScope       string `json:"modelScope,omitempty"`
	Origin           string `json:"origin,omitempty"`
}

// HypothesesDir returns local/memory/rsi/hypotheses.
func HypothesesDir(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "rsi", "hypotheses")
}

// SaveHypothesis atomically writes an RSIHypothesis to local/memory/rsi/hypotheses/<id>.json
// under store.RankMemory with 0600 permissions.
func SaveHypothesis(vaultDir string, h *RSIHypothesis) error {
	dir := HypothesesDir(vaultDir)
	if h.DiffSHA256 == "" && h.ProposedDiff != "" {
		sum := sha256.Sum256([]byte(h.ProposedDiff))
		h.DiffSHA256 = hex.EncodeToString(sum[:])
	}
	b, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return withRSILock(vaultDir, func() error {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		return store.WriteAtomic(filepath.Join(dir, h.ID+".json"), b, 0o600)
	})
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

// CheckQuotaCircuitBreaker checks local/memory/quota_state.json and returns false if rolling 5h/daily
// usage exceeds 75%.
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
		if slug == "" || slug == "general" {
			return "", fmt.Errorf("general.md overlay is retired; specify a named skill slug")
		}
		return OverlaySkeleton(slug), nil
	}
	return "", os.ErrNotExist
}

// ResolveTier1Target selects the Tier 1 personal mutation path for a named skillSlug inside vaultDir:
// - Tier 1A: local/skills/<slug>/SKILL.md when installed in Vault user space (and not a Core OS skill)
// - Tier 1B: local/memory/rsi/rules/<slug>.md for Core OS skills or plugin skills without a Vault SKILL.md
// Quarantine skills and the retired "general" / internal/* catch-all are unconditionally rejected.
func ResolveTier1Target(vaultDir, skillSlug string) (relTarget string, absTarget string, isOverlay bool, err error) {
	slug := strings.TrimSpace(skillSlug)
	if slug == "" || slug == "general" || strings.HasPrefix(slug, "internal/") {
		return "", "", false, fmt.Errorf("general.md overlay is retired; RSI rules must target a named skill slug (got %q; store cross-cutting preferences in Ring-3 memory)", skillSlug)
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

// DiscoverAllSkillFiles returns all SKILL.md and named overlay rule markdown paths across rootDir and optional vaultDir.
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
				if !re.IsDir() && strings.HasSuffix(re.Name(), ".md") && re.Name() != "general.md" {
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
// to a SKILL.md or overlay document without wholesale rewrites.
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

func activeTombstoneFingerprints(vaultDir string) map[string]bool {
	out := make(map[string]bool)
	recs, err := LoadArchive(vaultDir)
	if err != nil {
		return out
	}
	for _, r := range recs {
		if r.Status == "rejected" || r.Status == "reverted" {
			for _, fp := range r.SourceFingerprints {
				if fp != "" {
					out[fp] = true
				}
			}
		}
	}
	return out
}

// isCleanSingleLineFlagRepair returns true only when cmdPat and recovery share the same leading binary,
// recovery is a concise single-line shell invocation (<= 140 chars), and contains no heredocs or chat prose.
func isCleanSingleLineFlagRepair(cmdPat, recovery string) bool {
	c := strings.TrimSpace(cmdPat)
	r := strings.TrimSpace(recovery)
	if c == "" || r == "" || len(r) > 140 || strings.Contains(r, "\n") || strings.Contains(r, "<<") {
		return false
	}
	lowerR := strings.ToLower(r)
	for _, bad := range []string{"dispatched ", "worker ", "launched ", "subagent ", "i will ", "let me "} {
		if strings.HasPrefix(lowerR, bad) {
			return false
		}
	}
	cf := strings.Fields(c)
	rf := strings.Fields(r)
	if len(cf) == 0 || len(rf) == 0 {
		return false
	}
	return filepath.Base(cf[0]) == filepath.Base(rf[0]) && c != r
}

// HypothesizeWithOptions synthesizes either Tier 1 personal Vault mutations (default)
// or Tier 2 upstream contributor PR mutations in an isolated git worktree (`--upstream`).
// Per RFC-0029, 0-token Go template synthesis is restricted strictly to high-precision
// categories (proactive noise, user corrections, draft edits, or clean single-line CLI flag repairs)
// on named skills with distinctConvs >= 2 (or >= 1 for explicit user corrections).
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
	tombstones := activeTombstoneFingerprints(vaultDir)

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
		if !opts.Force && tombstones[ev.Fingerprint] {
			if targetObsID != "" {
				return created, fmt.Errorf("observation %s matches a rejected/reverted RSI tombstone (pass --force to override)", ev.ID)
			}
			continue
		}

		// Require Ring-2 substrate unless explicitly forced/targeted
		if targetObsID == "" && !opts.Force && ev.TargetSubstrate != "" && ev.TargetSubstrate != "ring2_skill_or_overlay" {
			continue
		}

		isUserCorrection := ev.Origin == "user_correction" || ev.Category == FrictionUserIntervention || ev.Category == FrictionDraftEdited
		distinctConvs := len(ev.ConversationIDs)
		if distinctConvs == 0 && ev.OccurrenceCount > 0 {
			distinctConvs = ev.OccurrenceCount
		}
		if !opts.Force && targetObsID == "" {
			if isUserCorrection {
				if distinctConvs < 1 {
					continue
				}
			} else if ev.Category == FrictionProactiveNoise {
				// Proactive noise already has high statistical thresholds (>=4..5 resolved items)
			} else {
				if distinctConvs < 2 {
					continue
				}
				// For error_recovery_chain, require clean single-line CLI flag repair for 0-token Go template;
				// noisy/complex traces should use `hq rsi context` + `hq rsi hypothesize --from-json`.
				if ev.Category == FrictionErrorRecovery && !isCleanSingleLineFlagRepair(ev.CommandPattern, ev.RecoveryAction) {
					continue
				}
			}
		}

		skillSlug := strings.TrimSpace(ev.SkillOrPkg)
		if skillSlug == "" || skillSlug == "general" || strings.HasPrefix(skillSlug, "internal/") {
			if targetObsID != "" {
				return created, fmt.Errorf("observation %s has unattributable skill %q; general.md overlay is retired (use Ring-3 memory or specify a named skill via --from-json)", ev.ID, skillSlug)
			}
			continue
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
			if strings.HasPrefix(relUp, "local/") {
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
		origin := ev.Origin
		if origin == "" {
			origin = "auto_mined"
		}

		guidance := sanitizePIIForTrackedSkill(rootDir, ev.RecoveryAction)
		cleanExcerpt := sanitizePIIForTrackedSkill(rootDir, ev.ErrorExcerpt)
		cleanCmdPat := sanitizePIIForTrackedSkill(rootDir, ev.CommandPattern)
		if strings.TrimSpace(guidance) == "" {
			guidance = fmt.Sprintf("Verify exact flags and pre-conditions for `%s` before invoking `%s` to prevent `%s`.", cleanCmdPat, ev.ToolName, cleanExcerpt)
		}
		if skillSlug == "thesis-check" || skillSlug == "filing-diff" {
			if !strings.Contains(strings.ToLower(guidance), "kill criteria") && !strings.Contains(strings.ToLower(guidance), "falsif") {
				guidance += " Always evaluate disconfirming evidence, bear case risks, and explicit kill criteria falsification."
			}
		}

		originAttr := ""
		if origin == "user_correction" {
			originAttr = ` origin="user_correction"`
		}
		ruleBlock := fmt.Sprintf(
			"<!-- rsi:rule id=%q source=%q model_scope=%q added=%q helpful=\"0\" harmful=\"0\"%s -->\n- **[Learned Rule · %s]:** When using `%s` in `%s`, %s *(Observed %dx; prevents: %s)*",
			ruleID,
			ev.ID,
			modelScope,
			now.Format("2006-01-02"),
			originAttr,
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

		expectedSaved := ev.OccurrenceCount * ev.WastedSteps
		if expectedSaved <= 0 {
			expectedSaved = 2
		}
		hID := fmt.Sprintf("rsi-%s-%s", today, ev.Fingerprint[:6])
		hyp := RSIHypothesis{
			ID:                 hID,
			ObservationIDs:     []string{ev.ID},
			SourceFingerprints: []string{ev.Fingerprint},
			Ring:               Ring2SkillTrigger,
			FaultLayer:         ev.FaultLayer,
			Title:              fmt.Sprintf("Add learned %s rule %s (%s)", skillSlug, ruleID, ev.Category),
			RootCause:          fmt.Sprintf("Ground-truth telemetry recorded %d occurrence(s) of %s in %s (%s)", ev.OccurrenceCount, ev.Category, skillSlug, ev.ErrorExcerpt),
			EditRationale:      fmt.Sprintf("Targeted %s rule directly addresses observed %s failure pattern.", skillSlug, ev.Category),
			PredictedOutcome:   fmt.Sprintf("Eliminate recurring %s failures in %s without regressing adjacent runs.", ev.Category, skillSlug),
			Origin:             origin,
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

	return created, nil
}

// HypothesizeFromJSON builds and saves a structured Ring-2 RSIHypothesis from LLM-authored JSON input
// (`hq rsi hypothesize --from-json`), validating target safety and formatting the canonical ACE rule block.
func HypothesizeFromJSON(rootDir, vaultDir string, spec HypothesisInput) (*RSIHypothesis, error) {
	skillSlug := strings.TrimSpace(spec.SkillSlug)
	ruleText := strings.TrimSpace(spec.RuleText)
	if skillSlug == "" || skillSlug == "general" {
		return nil, fmt.Errorf("skillSlug must be a named skill (general.md is retired)")
	}
	if ruleText == "" {
		return nil, fmt.Errorf("ruleText is required")
	}

	var matchedObs *FrictionEvent
	observations, _ := LoadObservations(vaultDir)
	for i := range observations {
		if spec.ObservationID != "" && (observations[i].ID == spec.ObservationID || observations[i].Fingerprint == spec.ObservationID) {
			if observations[i].Tainted {
				return nil, fmt.Errorf("cannot synthesize hypothesis from tainted observation %s", observations[i].ID)
			}
			matchedObs = &observations[i]
			break
		}
	}

	relTarget, absTarget, _, err := ResolveTier1Target(vaultDir, skillSlug)
	if err != nil {
		return nil, err
	}
	beforeContent, err := EnsureOverlaySkeletonContent(absTarget, relTarget)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	today := now.Format("20060102")
	fpToken := ""
	var obsIDs, fps []string
	fault := FaultWorkerACIOrSkill
	origin := strings.TrimSpace(spec.Origin)
	expectedSaved := 2

	if matchedObs != nil {
		fpToken = matchedObs.Fingerprint
		if len(fpToken) > 6 {
			fpToken = fpToken[:6]
		}
		obsIDs = []string{matchedObs.ID}
		fps = []string{matchedObs.Fingerprint}
		fault = matchedObs.FaultLayer
		if origin == "" {
			origin = matchedObs.Origin
		}
		if s := matchedObs.OccurrenceCount * matchedObs.WastedSteps; s > expectedSaved {
			expectedSaved = s
		}
	} else {
		sum := sha256.Sum256([]byte(skillSlug + "|" + ruleText))
		fpToken = hex.EncodeToString(sum[:])[:6]
		if spec.ObservationID != "" {
			obsIDs = []string{spec.ObservationID}
		}
		fps = []string{hex.EncodeToString(sum[:])[:16]}
	}
	if origin == "" {
		origin = "auto_mined"
	}

	ruleID := strings.TrimSpace(spec.RuleID)
	if ruleID == "" {
		ruleID = fmt.Sprintf("rsi-rule-%s-%s", skillSlug, fpToken)
	}
	modelScope := strings.TrimSpace(spec.ModelScope)
	if modelScope == "" {
		modelScope = "all"
	}
	srcAttr := "semantic"
	if len(obsIDs) > 0 {
		srcAttr = obsIDs[0]
	}
	originAttr := ""
	if origin == "user_correction" {
		originAttr = ` origin="user_correction"`
	}

	cleanRule := sanitizePIIForTrackedSkill(rootDir, ruleText)
	if !strings.HasPrefix(cleanRule, "- ") {
		cleanRule = "- " + cleanRule
	}
	ruleBlock := fmt.Sprintf(
		"<!-- rsi:rule id=%q source=%q model_scope=%q added=%q helpful=\"0\" harmful=\"0\"%s -->\n%s",
		ruleID, srcAttr, modelScope, now.Format("2006-01-02"), originAttr, cleanRule,
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
		return nil, err
	}
	diff := BuildUnifiedDiff(relTarget, beforeContent, afterContent)
	sum := sha256.Sum256([]byte(diff))
	diffSHA := hex.EncodeToString(sum[:])

	title := strings.TrimSpace(spec.Title)
	if title == "" {
		title = fmt.Sprintf("Add learned %s rule %s", skillSlug, ruleID)
	}
	rootCause := strings.TrimSpace(spec.RootCause)
	if rootCause == "" && matchedObs != nil {
		rootCause = matchedObs.ErrorExcerpt
	}

	h := &RSIHypothesis{
		ID:                 fmt.Sprintf("rsi-%s-%s", today, fpToken),
		ObservationIDs:     obsIDs,
		SourceFingerprints: fps,
		Ring:               Ring2SkillTrigger,
		FaultLayer:         fault,
		Title:              title,
		RootCause:          rootCause,
		EditRationale:      strings.TrimSpace(spec.EditRationale),
		PredictedOutcome:   strings.TrimSpace(spec.PredictedOutcome),
		Origin:             origin,
		TargetFiles:        []string{relTarget},
		DeltaOp:            delta,
		ProposedDiff:       diff,
		DiffSHA256:         diffSHA,
		ExpectedSavedSteps: expectedSaved,
		Status:             "draft",
		Tier:               "personal",
		CreatedAt:          now.Format(time.RFC3339),
	}
	if err := SaveHypothesis(vaultDir, h); err != nil {
		return nil, err
	}
	if matchedObs != nil {
		matchedObs.Status = "hypothesized"
		_ = SaveObservations(vaultDir, observations)
	}
	return h, nil
}

// BuildObservationContext constructs a bounded, PII-scrubbed, taint-verified evidence packet
// for semantic synthesis (`hq rsi context <obs-id> --json`) without writing any extra files.
func BuildObservationContext(rootDir, vaultDir, obsID string) (*ObservationContext, error) {
	obsList, err := LoadObservations(vaultDir)
	if err != nil {
		return nil, err
	}
	var target *FrictionEvent
	for i := range obsList {
		if obsList[i].ID == obsID || obsList[i].Fingerprint == obsID {
			ev := obsList[i]
			target = &ev
			break
		}
	}
	if target == nil {
		return nil, fmt.Errorf("observation %q not found", obsID)
	}
	if target.Tainted {
		return nil, fmt.Errorf("refusing context packet for tainted observation %s", target.ID)
	}

	ctx := &ObservationContext{
		Observation:       *target,
		TargetSkill:       target.SkillOrPkg,
		MaxRulesForTarget: MaxRulesPerOverlay,
	}

	if rel, abs, isOverlay, tErr := ResolveTier1Target(vaultDir, target.SkillOrPkg); tErr == nil {
		ctx.TargetFile = rel
		if !isOverlay {
			ctx.MaxRulesForTarget = MaxActiveRSIRules
		}
		if b, rErr := os.ReadFile(abs); rErr == nil {
			content := string(b)
			ctx.ActiveRuleCount = len(ParseRSIRules(rel, content))
			if len(content) > 4000 {
				content = content[:4000] + "\n... (truncated)"
			}
			ctx.CurrentTargetText = redact.ScrubForUntrustedOutput(vaultDir, content)
		}
	}

	// Collect up to 2 untainted ±5-step transcript excerpts matching target.ConversationIDs
	convSet := make(map[string]bool, len(target.ConversationIDs))
	for _, cid := range target.ConversationIDs {
		convSet[cid] = true
	}
	seenConv := make(map[string]bool)
	_ = vault.WalkJournals(vaultDir, time.Time{}, false, func(_ string, rec vault.JournalRecord) bool {
		if len(ctx.TranscriptExcerpts) >= 2 {
			return false
		}
		if !convSet[rec.ConversationID] || seenConv[rec.ConversationID] || rec.TranscriptPath == "" {
			return true
		}
		if rec.TaintedSession {
			return true
		}
		if _, tainted := taint.IsTainted(vaultDir, rec.ConversationID); tainted {
			return true
		}
		steps, trTainted, pErr := transcript.ParseSteps(rec.TranscriptPath, rec.StepStartIdx)
		if pErr != nil || trTainted || len(steps) == 0 {
			return true
		}
		seenConv[rec.ConversationID] = true
		failIdx := -1
		for i, s := range steps {
			if s.IsError || s.IsDenial {
				failIdx = i
				break
			}
		}
		if failIdx < 0 {
			failIdx = 0
		}
		lo := failIdx - 5
		if lo < 0 {
			lo = 0
		}
		hi := failIdx + 5
		if hi >= len(steps) {
			hi = len(steps) - 1
		}
		var sb strings.Builder
		sb.WriteString(fmt.Sprintf("[conv=%s steps=%d..%d]\n", rec.ConversationID, steps[lo].StepIndex, steps[hi].StepIndex))
		for k := lo; k <= hi; k++ {
			st := steps[k]
			tag := st.Type
			if st.IsDenial {
				tag = "PRETOOL_DENY"
			} else if st.IsError {
				tag = "ERROR"
			}
			line := st.CmdLine
			if line == "" {
				line = transcript.FirstLineExcerpt(st.Content, 160)
			}
			sb.WriteString(fmt.Sprintf("  step %d (%s/%s): %s\n", st.StepIndex, tag, st.ToolName, sanitizePIIForTrackedSkill(rootDir, redact.ScrubForUntrustedOutput(vaultDir, line))))
		}
		ctx.TranscriptExcerpts = append(ctx.TranscriptExcerpts, strings.TrimSpace(sb.String()))
		return true
	})

	// Attach prior rejection/revert tombstones for the same skill so synthesis never repeats rejected edits
	if arch, aErr := LoadArchive(vaultDir); aErr == nil {
		for _, r := range arch {
			if r.Status != "rejected" && r.Status != "reverted" {
				continue
			}
			if target.SkillOrPkg != "" && strings.Contains(strings.ToLower(strings.Join(r.TargetFiles, " ")+" "+r.Title), strings.ToLower(target.SkillOrPkg)) {
				msg := fmt.Sprintf("[%s] %s", r.Status, r.Title)
				if r.RejectionReason != "" {
					msg += " (" + r.RejectionReason + ")"
				}
				ctx.PriorTombstones = append(ctx.PriorTombstones, msg)
			}
		}
	}

	return ctx, nil
}

// ListAdvisories returns non-destructive actionable advisories for Ring-1 CLI bugs,
// high-cost workflow script compilation, and LATM Tier B->C graduations (helpful >= 5).
func ListAdvisories(rootDir, vaultDir string) []RSIAdvisory {
	var out []RSIAdvisory
	tombstones := activeTombstoneFingerprints(vaultDir)

	if obsList, err := LoadObservations(vaultDir); err == nil {
		for _, ev := range obsList {
			if ev.Status != "open" || ev.Tainted || tombstones[ev.Fingerprint] {
				continue
			}
			distinctConvs := len(ev.ConversationIDs)
			if distinctConvs < 2 && ev.OccurrenceCount < 2 {
				continue
			}
			switch ev.TargetSubstrate {
			case "ring1_cli_triage":
				out = append(out, RSIAdvisory{
					ID:          "adv-cli-" + ev.Fingerprint[:6],
					Kind:        "ring1_cli_triage",
					SkillOrPkg:  ev.SkillOrPkg,
					Title:       fmt.Sprintf("Fix recurring CLI friction in %s (%dx)", ev.SkillOrPkg, ev.OccurrenceCount),
					Summary:     fmt.Sprintf("%s (impact=%d, %d convs)", ev.ErrorExcerpt, ev.ImpactScore, distinctConvs),
					ImpactScore: ev.ImpactScore,
					ObsID:       ev.ID,
					SpawnPrompt: fmt.Sprintf("Investigate and fix recurring CLI observation %s (`hq rsi context %s --json`): %s", ev.ID, ev.ID, ev.ErrorExcerpt),
				})
			case "compile_script_or_route":
				out = append(out, RSIAdvisory{
					ID:          "adv-compile-" + ev.Fingerprint[:6],
					Kind:        "compile_script_or_route",
					SkillOrPkg:  ev.SkillOrPkg,
					Title:       fmt.Sprintf("Compile high-cost %s workflow (%dx)", ev.SkillOrPkg, ev.OccurrenceCount),
					Summary:     fmt.Sprintf("%s (impact=%d, %d convs)", ev.ErrorExcerpt, ev.ImpactScore, distinctConvs),
					ImpactScore: ev.ImpactScore,
					ObsID:       ev.ID,
					SpawnPrompt: fmt.Sprintf("Audit observation %s (`hq rsi context %s --json`) and compile the repetitive multi-step workflow into a deterministic script or tight skill route.", ev.ID, ev.ID),
				})
			}
		}
	}

	// Non-destructive LATM Tier B->C graduation advisories from archive.jsonl (replaces destructive D11a deprecation).
	if arch, err := LoadArchive(vaultDir); err == nil {
		for _, r := range arch {
			if (r.Status == "promoted" || r.Status == "canary") && r.HelpfulCount >= 5 && r.HarmfulCount == 0 {
				ruleID := r.RuleID
				if ruleID == "" {
					ruleID = r.ID
				}
				out = append(out, RSIAdvisory{
					ID:          "adv-grad-" + sanitizeBranchToken(ruleID),
					Kind:        "tier_c_graduation",
					SkillOrPkg:  strings.Join(r.TargetFiles, ","),
					Title:       fmt.Sprintf("Graduate proven RSI rule %s (helpful=%d) to deterministic Go check", ruleID, r.HelpfulCount),
					Summary:     fmt.Sprintf("Rule %s achieved %d verified helpful activations with 0 regressions; consider compiling into deterministic Go.", ruleID, r.HelpfulCount),
					ImpactScore: int64(r.HelpfulCount * 200),
					RuleID:      ruleID,
					SpawnPrompt: fmt.Sprintf("Implement a deterministic Go check for proven RSI rule %s (%s), add unit tests, and then deprecate the markdown rule cleanly.", ruleID, r.Title),
				})
			}
		}
	}

	sort.Slice(out, func(i, j int) bool {
		if out[i].ImpactScore != out[j].ImpactScore {
			return out[i].ImpactScore > out[j].ImpactScore
		}
		return out[i].ID < out[j].ID
	})
	return out
}

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
