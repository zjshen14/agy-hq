package kernel

import (
	"encoding/xml"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestParseTriggersFile(t *testing.T) {
	yamlContent := `# Declarative Proactive Triggers & Inbox Routing Rules
schedules:
  - id: morning-brief
    cron: "CRON_TZ=America/Los_Angeles 30 7 * * *"
    model: flash
    skill: morning-brief
  - id: edgar-close-watch
    cron: "CRON_TZ=America/Los_Angeles 30 13 * * 1-5"
    model: flash
    precheck_cmd: ["./bin/hq", "status", "--quiet"]
    skill: thesis-check
  - id: sleep-consolidate
    cron: "CRON_TZ=America/Los_Angeles 30 23 * * *"
    model: flash
    skill: sleep-consolidate

inbox_rules:
  - name: bank-statements
    extensions: [".csv", ".ofx", ".qfx"]
    skill: ledger-ingest
  - name: tax-forms
    extensions: [".pdf"]
    filename_regex: "(?i)(w-?2|1099|1098|k-?1|tax)"
    skill: tax-prep
  - name: sec-filings
    extensions: [".pdf", ".htm", ".html"]
    filename_regex: "(?i)(10-[kq]|8-k|earnings)"
    skill: thesis-check
`
	cfg, err := ParseTriggersContent(yamlContent)
	if err != nil {
		t.Fatalf("ParseTriggersContent failed: %v", err)
	}

	if len(cfg.Schedules) != 3 {
		t.Fatalf("expected 3 schedules, got %d", len(cfg.Schedules))
	}
	if cfg.Schedules[0].ID != "morning-brief" || cfg.Schedules[0].Skill != "morning-brief" {
		t.Errorf("unexpected schedule 0: %+v", cfg.Schedules[0])
	}
	if cfg.Schedules[1].ID != "edgar-close-watch" || len(cfg.Schedules[1].PrecheckCmd) != 3 {
		t.Errorf("unexpected schedule 1 precheck: %+v", cfg.Schedules[1])
	}
	if cfg.Schedules[2].ID != "sleep-consolidate" {
		t.Errorf("unexpected schedule 2: %+v", cfg.Schedules[2])
	}

	if len(cfg.InboxRules) != 3 {
		t.Fatalf("expected 3 inbox rules, got %d", len(cfg.InboxRules))
	}
	if cfg.InboxRules[0].Name != "bank-statements" || len(cfg.InboxRules[0].Extensions) != 3 {
		t.Errorf("unexpected rule 0: %+v", cfg.InboxRules[0])
	}
	if cfg.InboxRules[1].Name != "tax-forms" || cfg.InboxRules[1].FilenameRegex == "" {
		t.Errorf("unexpected rule 1: %+v", cfg.InboxRules[1])
	}

	// Test non-existent file handling
	missingCfg, _ := ParseTriggersFile(filepath.Join(t.TempDir(), "non-existent.yaml"))
	if missingCfg != nil {
		t.Fatalf("expected nil config on missing file")
	}
}

func TestGenerateSystemdService(t *testing.T) {
	unit := GenerateSystemdService("/usr/local/bin/hq", "/home/user/work", "/home/user/work/local")

	if !strings.Contains(unit, "[Unit]") || !strings.Contains(unit, "[Service]") || !strings.Contains(unit, "[Install]") {
		t.Fatalf("systemd service missing sections:\n%s", unit)
	}
	if !strings.Contains(unit, "ExecStart=/usr/local/bin/hq daemon") {
		t.Errorf("systemd service missing ExecStart:\n%s", unit)
	}
	if !strings.Contains(unit, "WorkingDirectory=/home/user/work") {
		t.Errorf("systemd service missing WorkingDirectory:\n%s", unit)
	}
	if !strings.Contains(unit, "Environment=HQ_ROOT_DIR=/home/user/work") {
		t.Errorf("systemd service missing HQ_ROOT_DIR:\n%s", unit)
	}
	if !strings.Contains(unit, "Environment=HQ_VAULT_DIR=/home/user/work/local") {
		t.Errorf("systemd service missing HQ_VAULT_DIR:\n%s", unit)
	}
	if !strings.Contains(unit, "WantedBy=default.target") {
		t.Errorf("systemd service missing WantedBy:\n%s", unit)
	}
}

func TestGenerateLaunchdPlist(t *testing.T) {
	plist := GenerateLaunchdPlist("/usr/local/bin/hq", "/home/user/work", "/home/user/work/local")

	// Validate valid XML structure
	var parsed struct {
		XMLName xml.Name `xml:"plist"`
		Dict    struct {
			Key []string `xml:"key"`
		} `xml:"dict"`
	}
	if err := xml.Unmarshal([]byte(plist), &parsed); err != nil {
		t.Fatalf("launchd plist is not valid XML: %v\n%s", err, plist)
	}

	if !strings.Contains(plist, "com.antigravity.agy-hq") {
		t.Errorf("plist missing label com.antigravity.agy-hq")
	}
	if !strings.Contains(plist, "<string>/usr/local/bin/hq</string>") {
		t.Errorf("plist missing binary path")
	}
	if !strings.Contains(plist, "<key>RunAtLoad</key>") {
		t.Errorf("plist missing RunAtLoad")
	}
}

func TestDaemonStateAndInfo(t *testing.T) {
	tmpDir := t.TempDir()
	vaultDir := filepath.Join(tmpDir, "local")
	_ = os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755)

	triggersContent := `schedules:
  - id: test-job
    cron: "0 * * * *"
    skill: test
`
	_ = os.WriteFile(filepath.Join(vaultDir, "triggers.yaml"), []byte(triggersContent), 0o644)

	// Before daemon starts
	infoBefore := GetDaemonInfo(vaultDir)
	if infoBefore.Running {
		t.Fatalf("expected daemon to be inactive initially")
	}
	if infoBefore.SchedulesCount != 1 {
		t.Errorf("expected 1 schedule declared, got %d", infoBefore.SchedulesCount)
	}
	statusInactive := FormatDaemonStatus(infoBefore)
	if !strings.Contains(statusInactive, "INACTIVE") {
		t.Errorf("expected INACTIVE in status, got:\n%s", statusInactive)
	}

	// Start daemon
	if err := RecordDaemonStart(vaultDir, "127.0.0.1:4242"); err != nil {
		t.Fatalf("RecordDaemonStart failed: %v", err)
	}

	// Verify status while running
	infoRunning := GetDaemonInfo(vaultDir)
	if !infoRunning.Running {
		t.Fatalf("expected daemon to be reported as running")
	}
	if infoRunning.PID != os.Getpid() {
		t.Errorf("expected PID %d, got %d", os.Getpid(), infoRunning.PID)
	}
	if infoRunning.Addr != "127.0.0.1:4242" {
		t.Errorf("expected addr 127.0.0.1:4242, got %s", infoRunning.Addr)
	}
	statusRunning := FormatDaemonStatus(infoRunning)
	if !strings.Contains(statusRunning, "RUNNING") || !strings.Contains(statusRunning, "PID:") {
		t.Errorf("expected RUNNING and PID in status, got:\n%s", statusRunning)
	}

	// Second start by another process should fail if alive
	// Note: same PID doesn't block itself, but let's test stop
	if err := RecordDaemonStop(vaultDir); err != nil {
		t.Fatalf("RecordDaemonStop failed: %v", err)
	}

	infoStopped := GetDaemonInfo(vaultDir)
	if infoStopped.Running {
		t.Fatalf("expected daemon to be inactive after stop")
	}
}

func TestInstallAndUninstallDaemon_Mock(t *testing.T) {
	tmpDir := t.TempDir()
	rootDir := filepath.Join(tmpDir, "repo")
	vaultDir := filepath.Join(rootDir, "local")
	sysDir := filepath.Join(tmpDir, "systemd", "user")
	launchDir := filepath.Join(tmpDir, "LaunchAgents")
	_ = os.MkdirAll(filepath.Join(rootDir, "bin"), 0o755)
	_ = os.MkdirAll(vaultDir, 0o755)
	_ = os.WriteFile(filepath.Join(rootDir, "bin", "hq"), []byte("#!/bin/sh\n"), 0o755)

	t.Setenv("HQ_SYSTEMD_DIR", sysDir)
	t.Setenv("HQ_LAUNCHD_DIR", launchDir)
	t.Setenv("HQ_SKIP_SYSTEMCTL", "1")
	t.Setenv("HQ_SKIP_LAUNCHCTL", "1")

	installMsg, err := InstallDaemon(rootDir, vaultDir)
	if err != nil {
		t.Fatalf("InstallDaemon failed: %v", err)
	}
	if !strings.Contains(installMsg, "Wrote") && !strings.Contains(installMsg, "Installed") {
		t.Errorf("unexpected install msg: %s", installMsg)
	}

	// Verify file was written
	var expectedPath string
	if runtime.GOOS == "darwin" {
		expectedPath = filepath.Join(launchDir, "com.antigravity.agy-hq.plist")
	} else {
		expectedPath = filepath.Join(sysDir, "agy-hq.service")
	}
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected service file to exist at %s: %v", expectedPath, err)
	}

	// Uninstall
	uninstallMsg, err := UninstallDaemon(vaultDir)
	if err != nil {
		t.Fatalf("UninstallDaemon failed: %v", err)
	}
	if !strings.Contains(uninstallMsg, "Removed") && !strings.Contains(uninstallMsg, "Disabled") {
		t.Errorf("unexpected uninstall msg: %s", uninstallMsg)
	}
	if _, err := os.Stat(expectedPath); !os.IsNotExist(err) {
		t.Fatalf("expected service file %s to be removed after uninstall", expectedPath)
	}
}
