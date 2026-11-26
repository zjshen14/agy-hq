package rsi

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/approvals"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/vault"
)

// OverlayMigrationReport summarizes migration of inline tracked RSI rules into personal Vault overlays.
type OverlayMigrationReport struct {
	ExtractedRules int      `json:"extractedRules"`
	CleanedFiles   []string `json:"cleanedFiles,omitempty"`
	UpdatedTargets []string `json:"updatedTargets,omitempty"`
}

func sanitizeBranchToken(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(s)) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	res := strings.Trim(b.String(), "-")
	if len(res) > 32 {
		res = res[:32]
	}
	if res == "" {
		res = "patch"
	}
	return res
}

func ensureUpstreamRSIWorktree(rootDir, vaultDir, token string) (string, string, error) {
	safe := sanitizeBranchToken(token)
	wtName := "headquarter-rsi-" + safe
	branch := "hq/rsi-" + safe
	wtDir := filepath.Join(vaultDir, "worktrees", wtName)

	if st, err := os.Stat(filepath.Join(wtDir, ".git")); err == nil && st != nil {
		return wtDir, branch, nil
	}
	if err := os.MkdirAll(filepath.Join(vaultDir, "worktrees"), 0o755); err != nil {
		return "", "", fmt.Errorf("mkdir worktrees: %w", err)
	}
	cmd := vault.SanitizedGitCommand(rootDir, "worktree", "add", "-B", branch, wtDir, "HEAD")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", "", fmt.Errorf("create upstream RSI worktree %s (%s): %w\n%s", wtDir, branch, err, strings.TrimSpace(string(out)))
	}
	return wtDir, branch, nil
}

// MigrateOverlays extracts any legacy `<!-- rsi:rule -->` blocks embedded inside tracked
// Core OS or template SKILL.md files and moves them into the user's private Vault
// (`<vault>/skills/<slug>/SKILL.md` or `<vault>/memory/rsi/rules/<slug>.md`), leaving
// tracked repository SKILL.md files completely clean. Idempotent on repeat runs.
func MigrateOverlays(rootDir, vaultDir string) (*OverlayMigrationReport, error) {
	rep := &OverlayMigrationReport{}
	var candidates []string
	for _, base := range []string{
		filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills"),
		filepath.Join(rootDir, "templates", "skills"),
	} {
		entries, err := os.ReadDir(base)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			sp := filepath.Join(base, e.Name(), "SKILL.md")
			if _, err := os.Stat(sp); err == nil {
				candidates = append(candidates, sp)
			}
		}
	}

	seenTargets := make(map[string]bool)
	for _, absSkill := range candidates {
		rawBytes, err := os.ReadFile(absSkill)
		if err != nil {
			continue
		}
		cur := string(rawBytes)
		relSkill, rErr := filepath.Rel(rootDir, absSkill)
		if rErr != nil {
			relSkill = absSkill
		}
		relSkill = filepath.ToSlash(relSkill)

		rules := ParseRSIRules(relSkill, cur)
		if len(rules) == 0 {
			continue
		}

		skillSlug := filepath.Base(filepath.Dir(absSkill))
		updatedContent := cur
		extractedAny := false

		for _, r := range rules {
			block, ok := extension.ExtractRSIRuleBlock(cur, r.ID)
			if !ok {
				continue
			}
			destSlug := skillSlug
			if !NonOverridableQuarantineSkills[destSlug] && destSlug != "" && destSlug != "general" {
				relTgt, absTgt, _, tErr := ResolveTier1Target(vaultDir, destSlug)
				if tErr == nil {
					beforeTgt, cErr := EnsureOverlaySkeletonContent(absTgt, relTgt)
					if cErr == nil && !strings.Contains(beforeTgt, fmt.Sprintf(`<!-- rsi:rule id=%q`, r.ID)) {
						afterTgt, aErr := ApplyACEDelta(beforeTgt, ACEDeltaOp{
							Op:         "add_skill_rule",
							RuleID:     r.ID,
							Section:    LearnedRulesHeading,
							Content:    block,
							ModelScope: r.ModelScope,
						})
						if aErr == nil {
							if err := os.MkdirAll(filepath.Dir(absTgt), 0o755); err == nil {
								if err := os.WriteFile(absTgt, []byte(afterTgt), 0o644); err == nil {
									rep.ExtractedRules++
									if !seenTargets[relTgt] {
										seenTargets[relTgt] = true
										rep.UpdatedTargets = append(rep.UpdatedTargets, relTgt)
									}
								}
							}
						}
					}
				}
			}

			if stripped, dErr := ApplyACEDelta(updatedContent, ACEDeltaOp{
				Op:     "deprecate_skill_rule",
				RuleID: r.ID,
			}); dErr == nil {
				updatedContent = stripped
				extractedAny = true
			}
		}

		if extractedAny {
			if len(ParseRSIRules(relSkill, updatedContent)) == 0 {
				updatedContent = strings.ReplaceAll(updatedContent, "\n"+LearnedRulesHeading+"\n", "\n")
				updatedContent = strings.ReplaceAll(updatedContent, "## Learned Operational Rules (RSI Managed)", "")
			}
			updatedContent = strings.TrimRight(updatedContent, " \t\n") + "\n"
			if err := os.WriteFile(absSkill, []byte(updatedContent), 0o644); err == nil {
				rep.CleanedFiles = append(rep.CleanedFiles, relSkill)
			}
		}
	}

	return rep, nil
}

var reWorkstationPath = regexp.MustCompile(`(/Users/|/home/|C:\\Users\\)[A-Za-z0-9._\-]+`)

func checkRuleCleanForUpstream(rootDir, vaultDir, ruleBlock string) error {
	if reSSN.MatchString(ruleBlock) {
		return fmt.Errorf("contains SSN pattern")
	}
	if reWorkstationPath.MatchString(ruleBlock) {
		return fmt.Errorf("contains workstation home path")
	}
	if home, err := os.UserHomeDir(); err == nil && len(home) > 5 && strings.Contains(ruleBlock, home) {
		return fmt.Errorf("contains local HOME directory path")
	}
	if len(vaultDir) > 5 && strings.Contains(ruleBlock, vaultDir) {
		return fmt.Errorf("contains local Vault absolute path")
	}
	if len(rootDir) > 5 && strings.Contains(ruleBlock, rootDir) {
		return fmt.Errorf("contains local checkout absolute path")
	}
	lower := strings.ToLower(ruleBlock)
	for _, priv := range []string{"local/private/", "local/memory/", "local/notes/", "ghp_", "ya29.", "sk-", "aiza"} {
		if strings.Contains(lower, priv) {
			return fmt.Errorf("contains private Vault or credential marker %q", priv)
		}
	}
	for _, email := range reEmail.FindAllString(ruleBlock, -1) {
		le := strings.ToLower(email)
		if strings.HasSuffix(le, "@example.com") ||
			strings.HasSuffix(le, "@partner.io") ||
			strings.HasSuffix(le, "@localhost") ||
			le == "user@gmail.com" ||
			le == "you@gmail.com" ||
			le == "me@gmail.com" {
			continue
		}
		return fmt.Errorf("contains personal email address %q", email)
	}
	return nil
}

// ExportUpstream graduates a clean personal RSI rule (or hypothesis) from Vault space into
// an isolated Tier 2 worktree branch (`hq/rsi-<id>`) in an agy-hq checkout and stages `gh pr create`
// via `approvals.CreateWithMetadata`. Rules containing PII, secrets, or local Vault references are refused.
func ExportUpstream(rootDir, vaultDir, id, convID, hqBin string) (*approvals.Proposal, *RSIHypothesis, error) {
	if !vault.IsAgyHQRepo(rootDir) {
		return nil, nil, fmt.Errorf("hq rsi export --upstream requires an agy-hq repository checkout")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, nil, fmt.Errorf("rule ID or RSI hypothesis ID is required")
	}

	var ruleID string
	var ruleBlock string
	var modelScope string
	var skillSlug string
	var obsIDs []string

	// 1. Search Vault overlays and Vault skills for a matching rule ID
	for _, absPath := range DiscoverAllSkillFiles("", vaultDir) {
		b, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		content := string(b)
		for _, r := range ParseRSIRules(absPath, content) {
			if r.ID == id {
				if blk, ok := extension.ExtractRSIRuleBlock(content, r.ID); ok {
					ruleID = r.ID
					ruleBlock = blk
					modelScope = r.ModelScope
					if strings.HasSuffix(absPath, "SKILL.md") {
						skillSlug = filepath.Base(filepath.Dir(absPath))
					} else {
						skillSlug = strings.TrimSuffix(filepath.Base(absPath), ".md")
					}
					if r.Source != "" {
						obsIDs = []string{r.Source}
					}
					break
				}
			}
		}
		if ruleID != "" {
			break
		}
	}

	// 2. Fallback: load hypothesis by ID
	if ruleID == "" {
		h, err := LoadHypothesis(vaultDir, id)
		if err != nil || h.DeltaOp == nil {
			return nil, nil, fmt.Errorf("no active Vault RSI rule or hypothesis found matching %q", id)
		}
		ruleID = h.DeltaOp.RuleID
		ruleBlock = h.DeltaOp.Content
		modelScope = h.DeltaOp.ModelScope
		obsIDs = h.ObservationIDs
		if len(h.TargetFiles) > 0 {
			tf := filepath.ToSlash(h.TargetFiles[0])
			if strings.HasSuffix(tf, "SKILL.md") {
				skillSlug = filepath.Base(filepath.Dir(tf))
			} else {
				skillSlug = strings.TrimSuffix(filepath.Base(tf), ".md")
			}
		}
	}

	if skillSlug == "" || skillSlug == "general" {
		return nil, nil, fmt.Errorf("rule %q belongs to generic overlay %q which has no direct upstream Core/starter skill target", ruleID, skillSlug)
	}
	if NonOverridableQuarantineSkills[skillSlug] {
		return nil, nil, fmt.Errorf("quarantine skill %q cannot be exported via upstream RSI", skillSlug)
	}
	if err := checkRuleCleanForUpstream(rootDir, vaultDir, ruleBlock); err != nil {
		return nil, nil, fmt.Errorf("refusing upstream export of %q: %w", ruleID, err)
	}

	var relUpstream string
	if coreCand := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", skillSlug, "SKILL.md"); fileExists(coreCand) {
		relUpstream = filepath.ToSlash(filepath.Join(".agents", "plugins", "agy-hq", "skills", skillSlug, "SKILL.md"))
	} else if tmplCand := filepath.Join(rootDir, "templates", "skills", skillSlug, "SKILL.md"); fileExists(tmplCand) {
		relUpstream = filepath.ToSlash(filepath.Join("templates", "skills", skillSlug, "SKILL.md"))
	} else {
		return nil, nil, fmt.Errorf("skill %q is a custom Vault skill with no upstream Core/template target in %s", skillSlug, rootDir)
	}

	wtDir, branch, err := ensureUpstreamRSIWorktree(rootDir, vaultDir, ruleID)
	if err != nil {
		return nil, nil, err
	}
	wtTarget := filepath.Join(wtDir, filepath.FromSlash(relUpstream))
	beforeBytes, err := os.ReadFile(wtTarget)
	if err != nil {
		return nil, nil, fmt.Errorf("read upstream skill in worktree %s: %w", wtTarget, err)
	}
	delta := &ACEDeltaOp{
		Op:         "add_skill_rule",
		RuleID:     ruleID,
		Section:    LearnedRulesHeading,
		Content:    ruleBlock,
		ModelScope: modelScope,
	}
	cleanProse := stripRSIRuleComments(ruleBlock)
	if cleanProse == "" {
		return nil, nil, fmt.Errorf("rule %q contains no exportable prose content", ruleID)
	}
	afterContent := strings.TrimRight(string(beforeBytes), "\n")
	if !strings.Contains(afterContent, cleanProse) {
		afterContent += "\n" + cleanProse
	}
	afterContent += "\n"
	if err := os.WriteFile(wtTarget, []byte(afterContent), 0o644); err != nil {
		return nil, nil, fmt.Errorf("write upstream worktree file %s: %w", wtTarget, err)
	}

	diff := BuildUnifiedDiff(relUpstream, string(beforeBytes), afterContent)
	sum := sha256.Sum256([]byte(diff))
	diffHex := hex.EncodeToString(sum[:])

	_ = vault.SanitizedGitCommand(wtDir, "add", relUpstream).Run()
	commitMsg := fmt.Sprintf("chore(rsi): export sanitized rule %s for %s", ruleID, skillSlug)
	_ = vault.SanitizedGitCommand(wtDir, "commit", "-m", commitMsg).Run()

	now := time.Now().UTC()
	hID := fmt.Sprintf("rsi-%s-export-%s", now.Format("20060102"), sanitizeBranchToken(ruleID))
	hyp := RSIHypothesis{
		ID:                 hID,
		ObservationIDs:     obsIDs,
		Ring:               Ring2SkillTrigger,
		FaultLayer:         FaultWorkerACIOrSkill,
		Title:              fmt.Sprintf("Export sanitized %s rule %s upstream (%s)", skillSlug, ruleID, branch),
		RootCause:          fmt.Sprintf("Curated Tier 1 Vault rule %s passed PII/boundary checks for Tier 2 upstream PR export", ruleID),
		TargetFiles:        []string{relUpstream},
		DeltaOp:            delta,
		ProposedDiff:       diff,
		DiffSHA256:         diffHex,
		ExpectedSavedSteps: 2,
		Status:             "proposed",
		Tier:               "upstream",
		WorktreeDir:        wtDir,
		Branch:             branch,
		CreatedAt:          now.Format(time.RFC3339),
	}
	_ = SaveHypothesis(vaultDir, &hyp)

	if hqBin == "" {
		hqBin = "hq"
	}
	_ = hqBin
	prTitle := fmt.Sprintf("chore(rsi): upstream %s rule (%s)", skillSlug, ruleID)
	prBody := fmt.Sprintf("Upstream export of verified Vault RSI rule `%s` to `%s` on branch `%s`.", ruleID, relUpstream, branch)
	cmdStr := fmt.Sprintf("git push -u origin %s && gh pr create --title %q --body %q", branch, prTitle, prBody)
	summary := fmt.Sprintf("[Tier 2 Upstream RSI] Push %s & open PR for %s (%s)", branch, relUpstream, ruleID)

	diffPreview := strings.TrimSpace(diff)
	if len(diffPreview) > 800 {
		diffPreview = diffPreview[:800] + "\n..."
	}
	prop, err := approvals.CreateWithMetadata(vaultDir, "pr_create", summary, cmdStr, wtDir, convID, map[string]string{
		"rsiId":                hyp.ID,
		"tier":                 "upstream",
		"branch":               branch,
		"worktreeDir":          wtDir,
		"targetFiles":          relUpstream,
		"diffSha256":           diffHex,
		"canonicalDiffPreview": diffPreview,
	})
	if err != nil {
		return nil, &hyp, err
	}
	return &prop, &hyp, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func stripRSIRuleComments(block string) string {
	var kept []string
	for _, line := range strings.Split(strings.ReplaceAll(block, "\r\n", "\n"), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "<!-- rsi:rule") || strings.HasPrefix(trimmed, "<!-- /rsi:rule") ||
			trimmed == "## Learned Operational Rules (RSI Managed)" || trimmed == "" {
			continue
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
