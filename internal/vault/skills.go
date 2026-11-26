package vault

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zjshen14/agy-hq/internal/store"
)

// CoreOSSkillSlugs lists the 3 channel-agnostic OS lifecycle skills shipped in the core plugin bundle.
var CoreOSSkillSlugs = []string{
	"inbox-triage",
	"morning-brief",
	"sleep-consolidate",
}

// StarterSkillSlugs lists opt-in starter skill templates embedded in templates/skills/.
var StarterSkillSlugs = []string{
	"gchat",
	"swe-pulse",
	"ledger-ingest",
	"tax-prep",
	"thesis-check",
	"filing-diff",
}

// SkillInfo describes an installed or available skill across Core OS and Vault User Space.
type SkillInfo struct {
	Name           string `json:"name"`
	Tier           string `json:"tier"` // "core", "vault-starter", "vault-custom", "starter-available"
	Installed      bool   `json:"installed"`
	Path           string `json:"path,omitempty"`
	OverlayPath    string `json:"overlayPath,omitempty"`
	Description    string `json:"description,omitempty"`
	Modified       bool   `json:"modified,omitempty"`
	ActiveRSIRules int    `json:"activeRsiRules"`
}

// IsCoreOSSkill reports whether slug is one of the 3 immutable Core OS lifecycle skills.
func IsCoreOSSkill(slug string) bool {
	s := strings.TrimSpace(slug)
	for _, c := range CoreOSSkillSlugs {
		if s == c {
			return true
		}
	}
	return false
}

// IsStarterSkill reports whether slug is one of the opt-in starter templates in templates/skills/.
func IsStarterSkill(slug string) bool {
	s := strings.TrimSpace(slug)
	for _, st := range StarterSkillSlugs {
		if s == st {
			return true
		}
	}
	return false
}

var validSlugRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

func validateSkillSlug(slug string) error {
	s := strings.TrimSpace(slug)
	if s == "" || strings.Contains(s, "/") || strings.Contains(s, `\`) || strings.Contains(s, "..") {
		return fmt.Errorf("invalid skill slug %q", slug)
	}
	if !validSlugRe.MatchString(s) {
		return fmt.Errorf("invalid skill slug %q (expected lowercase alphanumeric, hyphen, or underscore)", slug)
	}
	return nil
}

// VaultSkillsDir returns <vault>/skills.
func VaultSkillsDir(vaultDir string) string {
	return filepath.Join(vaultDir, "skills")
}

func baselinesFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "rsi", "skill_baselines.json")
}

func loadSkillBaselines(vaultDir string) map[string]string {
	out := make(map[string]string)
	b, err := os.ReadFile(baselinesFilePath(vaultDir))
	if err != nil {
		return out
	}
	_ = json.Unmarshal(b, &out)
	if out == nil {
		return make(map[string]string)
	}
	return out
}

// IsInstalledSkill reports whether slug is either a Core OS skill or installed under <vault>/skills/<slug>/SKILL.md.
func IsInstalledSkill(vaultDir, slug string) bool {
	s := strings.TrimSpace(slug)
	if s == "" {
		return false
	}
	if IsCoreOSSkill(s) {
		return true
	}
	if strings.TrimSpace(vaultDir) == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(VaultSkillsDir(vaultDir), s, "SKILL.md"))
	return err == nil
}

func saveSkillBaselines(vaultDir string, baselines map[string]string) error {
	path := baselinesFilePath(vaultDir)
	b, err := json.MarshalIndent(baselines, "", "  ")
	if err != nil {
		return err
	}
	return withMemoryLock(vaultDir, func() error {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		return store.WriteAtomic(path, append(b, '\n'), 0o600)
	})
}

func sha256HexTrimmed(s string) string {
	norm := strings.ReplaceAll(strings.TrimSpace(s), "\r\n", "\n")
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])
}

var (
	reRSIHeadingLine = regexp.MustCompile(`(?m)^##\s+Learned Operational Rules\b.*$`)
	reRSIRuleLine    = regexp.MustCompile(`(?m)^<!--\s*rsi:rule\s+[^>]+-->`)
)

// SplitSkillRSIRules separates a SKILL.md document into its base markdown content
// (everything before the RSI-managed rules heading or first rsi:rule comment),
// the trailing RSI section (if any), and the count of <!-- rsi:rule --> blocks.
func SplitSkillRSIRules(content string) (baseContent string, rsiSection string, ruleCount int) {
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	ruleCount = len(reRSIRuleLine.FindAllStringIndex(norm, -1))

	splitIdx := -1
	if loc := reRSIHeadingLine.FindStringIndex(norm); loc != nil {
		splitIdx = loc[0]
	}
	if rLoc := reRSIRuleLine.FindStringIndex(norm); rLoc != nil {
		if splitIdx < 0 || rLoc[0] < splitIdx {
			// Also capture an immediately preceding RSI heading if present, otherwise split at rule comment
			splitIdx = rLoc[0]
		}
	}
	if splitIdx < 0 {
		return strings.TrimRight(norm, "\n") + "\n", "", ruleCount
	}
	base := strings.TrimRight(norm[:splitIdx], " \t\n")
	if base != "" {
		base += "\n"
	}
	sec := strings.TrimSpace(norm[splitIdx:])
	if sec != "" {
		sec += "\n"
	}
	return base, sec, ruleCount
}

func combineBaseAndRSISection(baseContent, rsiSection string) string {
	b := strings.TrimRight(strings.ReplaceAll(baseContent, "\r\n", "\n"), "\n") + "\n"
	if strings.TrimSpace(rsiSection) == "" {
		return b
	}
	return b + "\n" + strings.TrimSpace(rsiSection) + "\n"
}

// AddStarterSkill copies a starter skill template from templates/skills/<slug>/SKILL.md
// into <vault>/skills/<slug>/SKILL.md. When force is true, any appended RSI rule blocks
// are preserved while refreshing the base template markdown.
func AddStarterSkill(vaultDir, templateDir, slug string, force bool) (changed bool, targetPath string, err error) {
	slug = strings.TrimSpace(slug)
	if !IsStarterSkill(slug) {
		return false, "", fmt.Errorf("unknown starter skill %q (available starters: %s)", slug, strings.Join(StarterSkillSlugs, ", "))
	}
	tmplRel := filepath.ToSlash(filepath.Join("skills", slug, "SKILL.md"))
	tmplBytes, err := ReadTemplateFile(templateDir, tmplRel)
	if err != nil {
		return false, "", fmt.Errorf("read starter skill template %s: %w", slug, err)
	}
	tmplBase, _, _ := SplitSkillRSIRules(string(tmplBytes))
	tmplHash := sha256HexTrimmed(tmplBase)

	dstPath := filepath.Join(VaultSkillsDir(vaultDir), slug, "SKILL.md")
	baselines := loadSkillBaselines(vaultDir)

	if existingBytes, rErr := os.ReadFile(dstPath); rErr == nil {
		curBase, curRSISec, _ := SplitSkillRSIRules(string(existingBytes))
		curHash := sha256HexTrimmed(curBase)
		if !force {
			if baselines[slug] == "" && curHash == tmplHash {
				baselines[slug] = tmplHash
				_ = saveSkillBaselines(vaultDir, baselines)
			}
			return false, dstPath, nil
		}
		out := combineBaseAndRSISection(tmplBase, curRSISec)
		if err := os.WriteFile(dstPath, []byte(out), 0o644); err != nil {
			return false, "", err
		}
		baselines[slug] = tmplHash
		_ = saveSkillBaselines(vaultDir, baselines)
		return true, dstPath, nil
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0o755); err != nil {
		return false, "", err
	}
	if err := os.WriteFile(dstPath, tmplBytes, 0o644); err != nil {
		return false, "", err
	}
	baselines[slug] = tmplHash
	_ = saveSkillBaselines(vaultDir, baselines)
	return true, dstPath, nil
}

// SyncStarterSkills performs dpkg-style safe template updates across all starter skills
// already installed in <vault>/skills/. If the user has not hand-edited the base prose
// relative to the recorded template SHA-256 baseline, the base markdown is refreshed while
// preserving any active <!-- rsi:rule --> blocks. Edited starter skills are left untouched.
func SyncStarterSkills(vaultDir, templateDir string) (updated []string, preservedEdited []string, err error) {
	skillsDir := VaultSkillsDir(vaultDir)
	entries, rErr := os.ReadDir(skillsDir)
	if rErr != nil {
		if os.IsNotExist(rErr) {
			return nil, nil, nil
		}
		return nil, nil, rErr
	}

	baselines := loadSkillBaselines(vaultDir)
	baselinesDirty := false

	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		slug := e.Name()
		if !IsStarterSkill(slug) {
			continue
		}
		dstPath := filepath.Join(skillsDir, slug, "SKILL.md")
		curBytes, err := os.ReadFile(dstPath)
		if err != nil {
			continue
		}
		tmplRel := filepath.ToSlash(filepath.Join("skills", slug, "SKILL.md"))
		tmplBytes, err := ReadTemplateFile(templateDir, tmplRel)
		if err != nil {
			continue
		}

		curBase, curRSISec, _ := SplitSkillRSIRules(string(curBytes))
		tmplBase, _, _ := SplitSkillRSIRules(string(tmplBytes))
		curHash := sha256HexTrimmed(curBase)
		tmplHash := sha256HexTrimmed(tmplBase)

		if curHash == tmplHash {
			if baselines[slug] != tmplHash {
				baselines[slug] = tmplHash
				baselinesDirty = true
			}
			continue
		}

		recordedBaseHash := baselines[slug]
		if recordedBaseHash != "" && curHash == recordedBaseHash {
			out := combineBaseAndRSISection(tmplBase, curRSISec)
			if wErr := os.WriteFile(dstPath, []byte(out), 0o644); wErr != nil {
				return updated, preservedEdited, wErr
			}
			baselines[slug] = tmplHash
			baselinesDirty = true
			updated = append(updated, slug)
		} else {
			preservedEdited = append(preservedEdited, slug)
		}
	}

	if baselinesDirty {
		_ = saveSkillBaselines(vaultDir, baselines)
	}
	sort.Strings(updated)
	sort.Strings(preservedEdited)
	return updated, preservedEdited, nil
}

// RemoveVaultSkill safely snapshots the Vault and removes a user-space skill directory from <vault>/skills/<slug>.
func RemoveVaultSkill(vaultDir, slug string) error {
	slug = strings.TrimSpace(slug)
	if IsCoreOSSkill(slug) {
		return fmt.Errorf("cannot remove Core OS skill %q from vault user space", slug)
	}
	if err := validateSkillSlug(slug); err != nil {
		return err
	}
	targetDir := filepath.Join(VaultSkillsDir(vaultDir), slug)
	if _, err := os.Stat(targetDir); os.IsNotExist(err) {
		return fmt.Errorf("vault skill %q is not installed at %s", slug, targetDir)
	}
	SnapshotVault(vaultDir, "remove-skill-"+slug)
	if err := os.RemoveAll(targetDir); err != nil {
		return err
	}
	baselines := loadSkillBaselines(vaultDir)
	if _, ok := baselines[slug]; ok {
		delete(baselines, slug)
		_ = saveSkillBaselines(vaultDir, baselines)
	}
	return nil
}

// ListSkills returns Core OS skills, installed Vault skills (starter & custom),
// and un-installed starter templates.
func ListSkills(rootDir, vaultDir, templateDir string) ([]SkillInfo, error) {
	var out []SkillInfo
	baselines := loadSkillBaselines(vaultDir)

	// 1. Core OS lifecycle skills (.agents/plugins/agy-hq/skills/)
	for _, slug := range CoreOSSkillSlugs {
		var corePath string
		if rootDir != "" {
			cand := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", slug, "SKILL.md")
			if _, err := os.Stat(cand); err == nil {
				corePath = cand
			}
		}
		var desc string
		if corePath != "" {
			if b, err := os.ReadFile(corePath); err == nil {
				desc = extractSkillDescription(string(b))
			}
		}
		overlayRules, dispOverlay := readSkillOverlayInfo(vaultDir, slug)
		out = append(out, SkillInfo{
			Name:           slug,
			Tier:           "core",
			Installed:      true,
			Path:           corePath,
			OverlayPath:    dispOverlay,
			Description:    desc,
			ActiveRSIRules: overlayRules,
		})
	}

	// 2. Installed Vault User-Space skills (<vault>/skills/*/SKILL.md)
	installedSet := make(map[string]bool)
	skillsDir := VaultSkillsDir(vaultDir)
	if entries, err := os.ReadDir(skillsDir); err == nil {
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			slug := e.Name()
			skillPath := filepath.Join(skillsDir, slug, "SKILL.md")
			b, rErr := os.ReadFile(skillPath)
			if rErr != nil {
				continue
			}
			installedSet[slug] = true
			baseContent, _, inlineRules := SplitSkillRSIRules(string(b))
			desc := extractSkillDescription(string(b))

			overlayRules, dispOverlay := readSkillOverlayInfo(vaultDir, slug)

			tier := "vault-custom"
			modified := false
			if IsStarterSkill(slug) {
				tier = "vault-starter"
				if baseHash := baselines[slug]; baseHash != "" {
					modified = sha256HexTrimmed(baseContent) != baseHash
				} else if tmplBytes, tErr := ReadTemplateFile(templateDir, filepath.ToSlash(filepath.Join("skills", slug, "SKILL.md"))); tErr == nil {
					tmplBase, _, _ := SplitSkillRSIRules(string(tmplBytes))
					modified = sha256HexTrimmed(baseContent) != sha256HexTrimmed(tmplBase)
				}
			}

			out = append(out, SkillInfo{
				Name:           slug,
				Tier:           tier,
				Installed:      true,
				Path:           skillPath,
				OverlayPath:    dispOverlay,
				Description:    desc,
				Modified:       modified,
				ActiveRSIRules: inlineRules + overlayRules,
			})
		}
	}

	// 3. Available opt-in starter templates not yet installed in Vault
	for _, slug := range StarterSkillSlugs {
		if installedSet[slug] {
			continue
		}
		var desc string
		if tmplBytes, err := ReadTemplateFile(templateDir, filepath.ToSlash(filepath.Join("skills", slug, "SKILL.md"))); err == nil {
			desc = extractSkillDescription(string(tmplBytes))
		}
		out = append(out, SkillInfo{
			Name:        slug,
			Tier:        "starter-available",
			Installed:   false,
			Description: desc,
		})
	}

	return out, nil
}

func readSkillOverlayInfo(vaultDir, slug string) (int, string) {
	overlayPath := filepath.Join(vaultDir, "memory", "rsi", "rules", slug+".md")
	b, err := os.ReadFile(overlayPath)
	if err != nil {
		return 0, ""
	}
	_, _, rules := SplitSkillRSIRules(string(b))
	if rules > 0 {
		return rules, overlayPath
	}
	return 0, ""
}

func extractSkillDescription(content string) string {
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(norm, "---\n") {
		return ""
	}
	end := strings.Index(norm[4:], "\n---")
	if end < 0 {
		return ""
	}
	fm := norm[4 : 4+end]
	lines := strings.Split(fm, "\n")
	for i := 0; i < len(lines); i++ {
		trimmed := strings.TrimSpace(lines[i])
		if strings.HasPrefix(trimmed, "description:") {
			val := strings.TrimSpace(strings.TrimPrefix(trimmed, "description:"))
			if val == ">-" || val == ">" || val == "|" || val == "|-" {
				var parts []string
				for j := i + 1; j < len(lines); j++ {
					if !strings.HasPrefix(lines[j], " ") && !strings.HasPrefix(lines[j], "\t") {
						break
					}
					parts = append(parts, strings.TrimSpace(lines[j]))
				}
				return strings.Trim(strings.Join(parts, " "), `"'`)
			}
			return strings.Trim(val, `"'`)
		}
	}
	return ""
}
