package vault

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVaultSkills_AddListRemoveAndSyncStarterSkills(t *testing.T) {
	vaultDir := t.TempDir()
	rootDir := t.TempDir()

	// Seed minimal Core OS skills in synthetic rootDir and clean Vault via Init.
	for _, core := range CoreOSSkillSlugs {
		coreDir := filepath.Join(rootDir, ".agents", "plugins", "agy-hq", "skills", core)
		_ = os.MkdirAll(coreDir, 0o755)
		_ = os.WriteFile(filepath.Join(coreDir, "SKILL.md"),
			[]byte("---\nname: "+core+"\ndescription: Core OS "+core+"\n---\n"), 0o644)
	}
	if _, err := Init(vaultDir, ""); err != nil {
		t.Fatalf("Init failed: %v", err)
	}

	// 1. Fresh Vault must be lean (0 starters auto-installed)
	if _, err := os.Stat(filepath.Join(vaultDir, "skills", "ledger-ingest", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("expected fresh Vault Init to be lean without ledger-ingest")
	}

	// 2. Add starter skill from embedded templates.FS (templateDir == "")
	changed, dstPath, err := AddStarterSkill(vaultDir, "", "ledger-ingest", false)
	if err != nil || !changed {
		t.Fatalf("AddStarterSkill(ledger-ingest) failed: changed=%v err=%v", changed, err)
	}
	if _, statErr := os.Stat(dstPath); statErr != nil {
		t.Fatalf("expected ledger-ingest SKILL.md at %s: %v", dstPath, statErr)
	}
	changedAgain, _, err := AddStarterSkill(vaultDir, "", "ledger-ingest", false)
	if err != nil || changedAgain {
		t.Fatalf("expected second non-forced AddStarterSkill to be a no-op, got changed=%v err=%v", changedAgain, err)
	}

	// 3. Add a custom user-authored skill (living-budget) with helper script
	customDir := filepath.Join(vaultDir, "skills", "living-budget")
	_ = os.MkdirAll(filepath.Join(customDir, "scripts"), 0o755)
	_ = os.WriteFile(filepath.Join(customDir, "SKILL.md"),
		[]byte("---\nname: living-budget\ndescription: Monthly living expense budget helper\n---\n\n# Living Budget\n"), 0o644)

	// 4. ListSkills reports Core (3), installed vault-starter, custom vault skill, and remaining available starters
	infos, err := ListSkills(rootDir, vaultDir, "")
	if err != nil {
		t.Fatalf("ListSkills failed: %v", err)
	}
	tiers := make(map[string]string)
	for _, info := range infos {
		tiers[info.Name] = info.Tier
	}
	if tiers["morning-brief"] != "core" || tiers["ledger-ingest"] != "vault-starter" ||
		tiers["living-budget"] != "vault-custom" || tiers["swe-pulse"] != "starter-available" {
		t.Fatalf("unexpected ListSkills tiers: %+v", tiers)
	}

	// 5. RemoveVaultSkill blocks Core OS skills and removes custom user skills
	for _, core := range CoreOSSkillSlugs {
		if err := RemoveVaultSkill(vaultDir, core); err == nil {
			t.Fatalf("expected RemoveVaultSkill(%q) to refuse removing Core OS skill", core)
		}
	}
	if err := RemoveVaultSkill(vaultDir, "living-budget"); err != nil {
		t.Fatalf("RemoveVaultSkill(living-budget) failed: %v", err)
	}
	if _, err := os.Stat(customDir); !os.IsNotExist(err) {
		t.Fatalf("expected living-budget directory to be removed")
	}

	// 6. dpkg-style SyncStarterSkills:
	//    Case A: Unmodified starter prose + appended <!-- rsi:rule --> block -> updates template prose AND preserves RSI rules!
	rsiSection := "\n\n## Learned Operational Rules (RSI Managed)\n\n<!-- rsi:rule id=\"rsi-rule-ledger-01\" source=\"obs-1\" model_scope=\"all\" added=\"2026-11-15\" helpful=\"1\" harmful=\"0\" -->\n- **[Learned Rule]:** Auto-tag coffee shops.\n<!-- /rsi:rule -->\n"
	origBytes, _ := os.ReadFile(dstPath)
	_ = os.WriteFile(dstPath, []byte(strings.TrimRight(string(origBytes), "\n")+rsiSection), 0o644)

	fakeTmplDir := t.TempDir()
	fakeLedgerDir := filepath.Join(fakeTmplDir, "skills", "ledger-ingest")
	_ = os.MkdirAll(fakeLedgerDir, 0o755)
	_ = os.WriteFile(filepath.Join(fakeLedgerDir, "SKILL.md"),
		[]byte("---\nname: ledger-ingest\ndescription: Upgraded v2 ledger ingest\n---\n\n# Ledger Ingest v2\nUpdated instructions.\n"), 0o644)

	updated, preserved, err := SyncStarterSkills(vaultDir, fakeTmplDir)
	if err != nil {
		t.Fatalf("SyncStarterSkills Case A failed: %v", err)
	}
	if len(updated) != 1 || updated[0] != "ledger-ingest" || len(preserved) != 0 {
		t.Fatalf("expected ledger-ingest updated and 0 preservedEdited, got updated=%v preserved=%v", updated, preserved)
	}
	afterSyncBytes, _ := os.ReadFile(dstPath)
	afterSync := string(afterSyncBytes)
	if !strings.Contains(afterSync, "Upgraded v2 ledger ingest") || !strings.Contains(afterSync, "rsi-rule-ledger-01") {
		t.Fatalf("expected SyncStarterSkills to upgrade base prose AND keep RSI rule, got:\n%s", afterSync)
	}

	//    Case B: User manually customizes base prose -> subsequent SyncStarterSkills preserves user edit
	customV2 := strings.ReplaceAll(afterSync, "Updated instructions.", "My bespoke family budget rules.")
	_ = os.WriteFile(dstPath, []byte(customV2), 0o644)
	_ = os.WriteFile(filepath.Join(fakeLedgerDir, "SKILL.md"),
		[]byte("---\nname: ledger-ingest\ndescription: Upgraded v3\n---\n\n# Ledger Ingest v3\n"), 0o644)

	updated2, preserved2, err := SyncStarterSkills(vaultDir, fakeTmplDir)
	if err != nil {
		t.Fatalf("SyncStarterSkills Case B failed: %v", err)
	}
	if len(updated2) != 0 || len(preserved2) != 1 || preserved2[0] != "ledger-ingest" {
		t.Fatalf("expected user-edited ledger-ingest to be preserved, got updated=%v preserved=%v", updated2, preserved2)
	}
	finalBytes, _ := os.ReadFile(dstPath)
	if !strings.Contains(string(finalBytes), "My bespoke family budget rules.") {
		t.Fatalf("expected user customization to remain intact, got:\n%s", string(finalBytes))
	}
}

func TestMigrateV8ToV9_SeedsOnlyActiveExistingStarters(t *testing.T) {
	vaultDir := t.TempDir()
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)
	_ = os.MkdirAll(filepath.Join(vaultDir, "private", "ledger"), 0o755)
	_ = os.MkdirAll(filepath.Join(vaultDir, "private", "gchat"), 0o755)
	_ = os.WriteFile(filepath.Join(vaultDir, "private", "ledger", "main.bean"), []byte("; empty\n"), 0o600)
	_ = os.WriteFile(filepath.Join(vaultDir, "private", "gchat", "accounts.json"), []byte(`{}`), 0o600)

	// Mark Vault as pre-v9 schema version 8
	sBytes, _ := json.MarshalIndent(SchemaState{Version: 8, UpdatedAt: "2026-11-14T00:00:00Z"}, "", "  ")
	_ = os.WriteFile(schemaFilePath(vaultDir), sBytes, 0o644)

	rep, err := Migrate(vaultDir, "")
	if err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}
	if rep.PreviousVersion != 8 || rep.CurrentVersion != CurrentSchemaVersion {
		t.Fatalf("unexpected migration versions: %+v", rep)
	}

	// Active domains (ledger, gchat) should be seeded; inactive domain (filing-diff) should NOT
	if _, err := os.Stat(filepath.Join(vaultDir, "skills", "ledger-ingest", "SKILL.md")); err != nil {
		t.Fatalf("expected active ledger-ingest starter to be seeded on v8->v9 migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "skills", "gchat", "SKILL.md")); err != nil {
		t.Fatalf("expected active gchat starter to be seeded on v8->v9 migration: %v", err)
	}
	if _, err := os.Stat(filepath.Join(vaultDir, "skills", "filing-diff", "SKILL.md")); !os.IsNotExist(err) {
		t.Fatalf("expected inactive filing-diff NOT to be seeded on v8->v9 migration")
	}
}
