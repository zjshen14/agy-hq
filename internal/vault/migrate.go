package vault

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// CurrentSchemaVersion is the monotonic schema version for the private local/ Vault.
const CurrentSchemaVersion = 3

// SchemaState is persisted at local/memory/schema_version.json.
type SchemaState struct {
	Version           int      `json:"version"`
	UpdatedAt         string   `json:"updatedAt"`
	AppliedMigrations []string `json:"appliedMigrations"`
}

// MigrationReport summarizes what vault.Migrate changed.
type MigrationReport struct {
	PreviousVersion   int      `json:"previousVersion"`
	CurrentVersion    int      `json:"currentVersion"`
	SnapshotCommitted bool     `json:"snapshotCommitted"`
	CreatedItems      []string `json:"createdItems,omitempty"`
	AppliedSteps      []string `json:"appliedSteps,omitempty"`
	AddedConfigKeys   []string `json:"addedConfigKeys,omitempty"`
}

func schemaFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "schema_version.json")
}

// LoadSchemaState reads local/memory/schema_version.json (defaults to version 0 if missing).
func LoadSchemaState(vaultDir string) SchemaState {
	var st SchemaState
	data, err := os.ReadFile(schemaFilePath(vaultDir))
	if err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

// SnapshotVault commits any uncommitted changes in local/.git as a restore point before migration/upgrade.
func SnapshotVault(vaultDir, reason string) bool {
	gitDir := filepath.Join(vaultDir, ".git")
	if _, err := os.Stat(gitDir); err != nil {
		return false
	}
	statusOut, err := exec.Command("git", "-C", vaultDir, "status", "--porcelain").Output()
	if err != nil || len(strings.TrimSpace(string(statusOut))) == 0 {
		return false
	}
	_ = exec.Command("git", "-C", vaultDir, "add", "-A").Run()
	msg := fmt.Sprintf("chore(vault): pre-upgrade snapshot (%s)", reason)
	if err := exec.Command("git", "-C", vaultDir, "commit", "-m", msg).Run(); err != nil {
		return false
	}
	return true
}

// Migrate performs idempotent, non-destructive schema and template migrations on local/.
func Migrate(vaultDir, templateDir string) (*MigrationReport, error) {
	prev := LoadSchemaState(vaultDir)
	report := &MigrationReport{
		PreviousVersion: prev.Version,
		CurrentVersion:  CurrentSchemaVersion,
	}

	// 1. If the Vault already exists and has uncommitted changes, create a restore-point commit in local/.git
	if prev.Version < CurrentSchemaVersion {
		report.SnapshotCommitted = SnapshotVault(vaultDir, fmt.Sprintf("v%d->v%d", prev.Version, CurrentSchemaVersion))
	}

	// 2. Run Init to scaffold any new directories, missing template files, or .git/hooks/pre-commit
	created, err := Init(vaultDir, templateDir)
	if err != nil {
		return report, err
	}
	report.CreatedItems = created

	appliedSet := make(map[string]bool)
	for _, m := range prev.AppliedMigrations {
		appliedSet[m] = true
	}
	appliedList := append([]string(nil), prev.AppliedMigrations...)
	recordStep := func(id, desc string) {
		if !appliedSet[id] {
			appliedSet[id] = true
			appliedList = append(appliedList, id)
		}
		report.AppliedSteps = append(report.AppliedSteps, desc)
	}

	// Migration v1 -> v2: Additive .gitignore merge & notes/briefs directory
	if prev.Version < 2 {
		mergedLines := mergeGitignoreAdditive(
			filepath.Join(templateDir, "gitignore.tmpl"),
			filepath.Join(vaultDir, ".gitignore"),
		)
		if mergedLines > 0 {
			recordStep("v2_gitignore_merge", fmt.Sprintf("Merged %d new ignore rules into local/.gitignore", mergedLines))
		} else {
			recordStep("v2_gitignore_merge", "Verified local/.gitignore rules and local/notes/briefs directory")
		}
	}

	// Migration v2 -> v3: Normalize tasks.json titles to [Task:<scope>] convention & remove legacy hq_console_mobile.md
	if prev.Version < 3 {
		normalized := normalizeLegacyTaskTitles(vaultDir)
		removedLegacy := removeLegacyMobileConsole(vaultDir)
		recordStep("v3_task_titles_and_console", fmt.Sprintf("Normalized %d task title(s) to [Task:<scope>] convention; cleaned legacy mobile console=%v", normalized, removedLegacy))
	}

	// 3. Additive top-level key synchronization for config.yaml and triggers.yaml
	for _, fname := range []string{"config.yaml", "triggers.yaml"} {
		added := mergeMissingTopLevelYAMLBlocks(
			filepath.Join(templateDir, fname),
			filepath.Join(vaultDir, fname),
		)
		for _, k := range added {
			report.AddedConfigKeys = append(report.AddedConfigKeys, fname+":"+k)
		}
	}

	// 4. Persist updated schema_version.json
	newState := SchemaState{
		Version:           CurrentSchemaVersion,
		UpdatedAt:         time.Now().Format(time.RFC3339),
		AppliedMigrations: appliedList,
	}
	if b, err := json.MarshalIndent(newState, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
		_ = os.WriteFile(schemaFilePath(vaultDir), b, 0o644)
	}

	return report, nil
}

func mergeGitignoreAdditive(tmplPath, dstPath string) int {
	tmplBytes, err1 := os.ReadFile(tmplPath)
	dstBytes, err2 := os.ReadFile(dstPath)
	if err1 != nil || err2 != nil {
		return 0
	}
	existing := make(map[string]bool)
	for _, line := range strings.Split(string(dstBytes), "\n") {
		existing[strings.TrimSpace(line)] = true
	}
	var toAppend []string
	for _, line := range strings.Split(string(tmplBytes), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if !existing[trimmed] {
			toAppend = append(toAppend, trimmed)
			existing[trimmed] = true
		}
	}
	if len(toAppend) == 0 {
		return 0
	}
	updated := strings.TrimRight(string(dstBytes), "\n") + "\n" + strings.Join(toAppend, "\n") + "\n"
	_ = os.WriteFile(dstPath, []byte(updated), 0o644)
	return len(toAppend)
}

func normalizeLegacyTaskTitles(vaultDir string) int {
	tasksPath := filepath.Join(vaultDir, "memory", "tasks.json")
	data, err := os.ReadFile(tasksPath)
	if err != nil {
		return 0
	}
	var raw []map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0
	}
	count := 0
	for _, item := range raw {
		title, _ := item["title"].(string)
		slug, _ := item["taskSlug"].(string)
		if title != "" && !strings.HasPrefix(title, "[Task:") && !strings.HasPrefix(title, "[HQ]") {
			scope := "general"
			if parts := strings.SplitN(slug, "-", 2); len(parts) > 0 && parts[0] != "" {
				scope = parts[0]
			}
			item["title"] = fmt.Sprintf("[Task:%s] %s", scope, title)
			count++
		}
	}
	if count > 0 {
		if b, err := json.MarshalIndent(raw, "", "  "); err == nil {
			_ = os.WriteFile(tasksPath, b, 0o644)
		}
	}
	return count
}

func removeLegacyMobileConsole(vaultDir string) bool {
	removed := false
	if b, err := os.ReadFile(filepath.Join(vaultDir, "memory", "active_artifact_dir.txt")); err == nil {
		artDir := strings.TrimSpace(string(b))
		if artDir != "" {
			legacyPath := filepath.Join(artDir, "hq_console_mobile.md")
			if err := os.Remove(legacyPath); err == nil {
				removed = true
			}
		}
	}
	return removed
}

// mergeMissingTopLevelYAMLBlocks checks if template YAML has top-level keys ("key:")
// that are absent from the user's local YAML, and appends their block non-destructively.
func mergeMissingTopLevelYAMLBlocks(tmplPath, dstPath string) []string {
	tmplBytes, err1 := os.ReadFile(tmplPath)
	dstBytes, err2 := os.ReadFile(dstPath)
	if err1 != nil || err2 != nil {
		return nil
	}
	dstKeys := parseTopLevelYAMLKeys(string(dstBytes))
	blocks := splitTopLevelYAMLBlocks(string(tmplBytes))

	var addedKeys []string
	var appendedBlocks []string
	for _, blk := range blocks {
		if blk.key != "" && !dstKeys[blk.key] {
			addedKeys = append(addedKeys, blk.key)
			appendedBlocks = append(appendedBlocks, blk.content)
			dstKeys[blk.key] = true
		}
	}
	if len(appendedBlocks) > 0 {
		updated := strings.TrimRight(string(dstBytes), "\n") +
			"\n\n# --- Added by hq migrate ---\n" +
			strings.Join(appendedBlocks, "\n") + "\n"
		_ = os.WriteFile(dstPath, []byte(updated), 0o644)
	}
	return addedKeys
}

type yamlBlock struct {
	key     string
	content string
}

func parseTopLevelYAMLKeys(content string) map[string]bool {
	keys := make(map[string]bool)
	for _, line := range strings.Split(content, "\n") {
		if len(line) == 0 || line[0] == ' ' || line[0] == '\t' || line[0] == '#' || line[0] == '-' {
			continue
		}
		if idx := strings.Index(line, ":"); idx > 0 {
			keys[strings.TrimSpace(line[:idx])] = true
		}
	}
	return keys
}

func splitTopLevelYAMLBlocks(content string) []yamlBlock {
	var blocks []yamlBlock
	var curKey string
	var curLines []string

	flush := func() {
		if curKey != "" && len(curLines) > 0 {
			blocks = append(blocks, yamlBlock{
				key:     curKey,
				content: strings.TrimRight(strings.Join(curLines, "\n"), "\n"),
			})
		}
	}

	for _, line := range strings.Split(content, "\n") {
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '#' && line[0] != '-' {
			if idx := strings.Index(line, ":"); idx > 0 {
				flush()
				curKey = strings.TrimSpace(line[:idx])
				curLines = []string{line}
				continue
			}
		}
		if curKey != "" {
			curLines = append(curLines, line)
		}
	}
	flush()
	return blocks
}
