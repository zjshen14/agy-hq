package kernel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// ScheduleTrigger represents a scheduled proactive automation from local/triggers.yaml.
type ScheduleTrigger struct {
	ID          string   `json:"id"`
	Cron        string   `json:"cron"`
	Mode        string   `json:"mode,omitempty"` // front_desk_cron | isolated_worker | daemon_gatekeeper
	Model       string   `json:"model"`
	Skill       string   `json:"skill"`
	PrecheckCmd []string `json:"precheckCmd,omitempty"`
	DedupMode   string   `json:"dedupMode,omitempty"` // none | exit_zero_nonempty | stdout_sha256
	Delivery    string   `json:"delivery,omitempty"`  // interrupt | interrupt_on_breach | brief_rollup | ideas_feed | silent_ledger
}

// EffectiveMode returns the normalized execution mode ("front_desk_cron", "isolated_worker", or "daemon_gatekeeper").
func (s ScheduleTrigger) EffectiveMode() string {
	m := strings.ToLower(strings.TrimSpace(s.Mode))
	switch m {
	case "daemon_gatekeeper", "isolated_worker", "front_desk_cron":
		return m
	}
	if len(s.PrecheckCmd) > 0 {
		return "daemon_gatekeeper"
	}
	return "front_desk_cron"
}

// EffectiveDedupMode returns the normalized precheck deduplication mode.
func (s ScheduleTrigger) EffectiveDedupMode() string {
	d := strings.ToLower(strings.TrimSpace(s.DedupMode))
	switch d {
	case "none", "exit_zero_nonempty", "stdout_sha256":
		return d
	}
	if len(s.PrecheckCmd) > 0 {
		return "stdout_sha256"
	}
	return "none"
}

// EffectiveDelivery returns the normalized attention delivery policy for this trigger.
func (s ScheduleTrigger) EffectiveDelivery() string {
	d := strings.ToLower(strings.TrimSpace(s.Delivery))
	switch d {
	case "interrupt", "interrupt_on_breach", "brief_rollup", "ideas_feed", "silent_ledger":
		return d
	}
	return ""
}

// FiveFieldCron strips any leading CRON_TZ=<tz> token and returns a standard 5-field cron expression
// compatible with Antigravity's native `schedule` tool.
func (s ScheduleTrigger) FiveFieldCron() string {
	fields := strings.Fields(strings.TrimSpace(s.Cron))
	if len(fields) > 0 && strings.HasPrefix(strings.ToUpper(fields[0]), "CRON_TZ=") {
		fields = fields[1:]
	}
	return strings.Join(fields, " ")
}

// DefaultPrompt returns the canonical prompt string to pass to Antigravity's `schedule` tool for this trigger.
func (s ScheduleTrigger) DefaultPrompt() string {
	switch s.ID {
	case "morning-brief":
		return "[Auto] Morning Brief: Activate the morning-brief skill to generate today's Morning Brief across calendar events, inbox triage, pending approvals, active worktrees, and investment watchlist."
	case "edgar-close-watch":
		return "[Auto] EDGAR Close Watch: Activate the thesis-check skill to monitor new after-close 8-K, 10-Q, or 10-K filings against investment watchlists."
	case "sleep-consolidate":
		return "[Auto] Nightly Sleep Consolidation: Activate the sleep-consolidate skill to review today's turn journals in local/memory/journal/, update local/MEMORY.md and local/memory/facts.jsonl, and commit private Vault changes."
	default:
		skill := s.Skill
		if skill == "" {
			skill = s.ID
		}
		return fmt.Sprintf("[Auto] %s: Activate the %s skill and execute the scheduled workflow.", s.ID, skill)
	}
}

// InboxRuleTrigger represents a file drop routing rule from local/triggers.yaml.
type InboxRuleTrigger struct {
	Name          string   `json:"name"`
	Extensions    []string `json:"extensions"`
	FilenameRegex string   `json:"filenameRegex,omitempty"`
	Skill         string   `json:"skill"`
}

// TriggersConfig holds the parsed declarative triggers from local/triggers.yaml.
type TriggersConfig struct {
	Schedules  []ScheduleTrigger  `json:"schedules"`
	InboxRules []InboxRuleTrigger `json:"inboxRules"`
}

type daemonState struct {
	PID       int    `json:"pid"`
	Addr      string `json:"addr"`
	StartedAt string `json:"startedAt"`
}

// DaemonInfo holds the running state, PID, uptime, and declared triggers of the background daemon.
type DaemonInfo struct {
	Running         bool               `json:"running"`
	PID             int                `json:"pid,omitempty"`
	Addr            string             `json:"addr,omitempty"`
	StartedAt       string             `json:"startedAt,omitempty"`
	UptimeSeconds   int64              `json:"uptimeSeconds,omitempty"`
	UptimeStr       string             `json:"uptime,omitempty"`
	SchedulesCount  int                `json:"schedulesCount"`
	InboxRulesCount int                `json:"inboxRulesCount"`
	Schedules       []ScheduleTrigger  `json:"schedules,omitempty"`
	InboxRules      []InboxRuleTrigger `json:"inboxRules,omitempty"`
}

func daemonStatePath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "daemon.json")
}

func cleanVal(s string) string {
	s = strings.TrimSpace(s)
	if (strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`)) ||
		(strings.HasPrefix(s, `'`) && strings.HasSuffix(s, `'`)) {
		return s[1 : len(s)-1]
	}
	return s
}

func parseStringList(s string) []string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		s = s[1 : len(s)-1]
	}
	parts := strings.Split(s, ",")
	var result []string
	for _, p := range parts {
		cleaned := cleanVal(strings.TrimSpace(p))
		if cleaned != "" {
			result = append(result, cleaned)
		}
	}
	return result
}

func parseScheduleField(s *ScheduleTrigger, line string) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return
	}
	key := strings.TrimSpace(line[:idx])
	val := strings.TrimSpace(line[idx+1:])

	switch key {
	case "id":
		s.ID = cleanVal(val)
	case "cron":
		s.Cron = cleanVal(val)
	case "mode":
		s.Mode = cleanVal(val)
	case "model":
		s.Model = cleanVal(val)
	case "skill":
		s.Skill = cleanVal(val)
	case "precheck_cmd":
		s.PrecheckCmd = parseStringList(val)
	case "dedup_mode":
		s.DedupMode = cleanVal(val)
	case "delivery":
		s.Delivery = cleanVal(val)
	}
}

func parseInboxRuleField(r *InboxRuleTrigger, line string) {
	idx := strings.Index(line, ":")
	if idx < 0 {
		return
	}
	key := strings.TrimSpace(line[:idx])
	val := strings.TrimSpace(line[idx+1:])

	switch key {
	case "name":
		r.Name = cleanVal(val)
	case "skill":
		r.Skill = cleanVal(val)
	case "filename_regex":
		r.FilenameRegex = cleanVal(val)
	case "extensions":
		r.Extensions = parseStringList(val)
	}
}

// ParseTriggersContent parses YAML triggers declarations into TriggersConfig.
func ParseTriggersContent(content string) (*TriggersConfig, error) {
	cfg := &TriggersConfig{
		Schedules:  make([]ScheduleTrigger, 0),
		InboxRules: make([]InboxRuleTrigger, 0),
	}

	scanner := bufio.NewScanner(strings.NewReader(content))
	section := ""

	var curSchedule *ScheduleTrigger
	var curRule *InboxRuleTrigger

	flushSchedule := func() {
		if curSchedule != nil {
			cfg.Schedules = append(cfg.Schedules, *curSchedule)
			curSchedule = nil
		}
	}
	flushRule := func() {
		if curRule != nil {
			cfg.InboxRules = append(cfg.InboxRules, *curRule)
			curRule = nil
		}
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}

		// Check for top-level sections (no leading whitespace)
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' && line[0] != '-' {
			if strings.HasPrefix(trimmed, "schedules:") {
				flushSchedule()
				flushRule()
				section = "schedules"
				continue
			} else if strings.HasPrefix(trimmed, "inbox_rules:") {
				flushSchedule()
				flushRule()
				section = "inbox_rules"
				continue
			} else {
				flushSchedule()
				flushRule()
				section = ""
				continue
			}
		}

		if section == "schedules" {
			if strings.HasPrefix(trimmed, "- ") {
				flushSchedule()
				curSchedule = &ScheduleTrigger{}
				trimmed = strings.TrimPrefix(trimmed, "- ")
			}
			if curSchedule != nil {
				parseScheduleField(curSchedule, trimmed)
			}
		} else if section == "inbox_rules" {
			if strings.HasPrefix(trimmed, "- ") {
				flushRule()
				curRule = &InboxRuleTrigger{}
				trimmed = strings.TrimPrefix(trimmed, "- ")
			}
			if curRule != nil {
				parseInboxRuleField(curRule, trimmed)
			}
		}
	}
	flushSchedule()
	flushRule()

	return cfg, nil
}

// ParseTriggersFile reads and parses a triggers YAML file.
func ParseTriggersFile(path string) (*TriggersConfig, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseTriggersContent(string(data))
}

// LoadTriggers loads triggers from local/triggers.yaml in vaultDir.
func LoadTriggers(vaultDir string) (*TriggersConfig, error) {
	triggersPath := filepath.Join(vaultDir, "triggers.yaml")
	cfg, err := ParseTriggersFile(triggersPath)
	if err != nil {
		return &TriggersConfig{
			Schedules:  make([]ScheduleTrigger, 0),
			InboxRules: make([]InboxRuleTrigger, 0),
		}, err
	}
	return cfg, nil
}

func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil
}

func isDaemonProcess(pid int) bool {
	if pid <= 0 || !isProcessAlive(pid) {
		return false
	}
	if runtime.GOOS == "linux" {
		if data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); err == nil {
			cmdline := strings.ReplaceAll(string(data), "\x00", " ")
			return strings.Contains(cmdline, "daemon")
		}
	}
	if out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").Output(); err == nil {
		cmdline := string(out)
		return strings.Contains(cmdline, "daemon")
	}
	return true
}

func checkPort(addr string) bool {
	conn, err := net.DialTimeout("tcp", addr, 150*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func checkHTTPHealth(addr, vaultDir string) bool {
	client := http.Client{
		Timeout: 250 * time.Millisecond,
	}
	resp, err := client.Get(fmt.Sprintf("http://%s/api/state", addr))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false
	}
	var state struct {
		VaultDir string `json:"vaultDir"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err == nil && state.VaultDir != "" {
		if vaultDir == "" {
			return true
		}
		return filepath.Clean(state.VaultDir) == filepath.Clean(vaultDir)
	}
	return false
}

func findDaemonPID(addr string) int {
	// Try pgrep -f "hq daemon"
	if out, err := exec.Command("pgrep", "-f", "hq daemon").Output(); err == nil {
		for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid != os.Getpid() && pid > 0 {
				return pid
			}
		}
	}
	// Try lsof -ti :<port>
	_, port, err := net.SplitHostPort(addr)
	if err == nil && port != "" {
		if out, err := exec.Command("lsof", "-ti", ":"+port).Output(); err == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if pid, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && pid != os.Getpid() && pid > 0 {
					return pid
				}
			}
		}
	}
	return 0
}

func formatDuration(d time.Duration) string {
	d = d.Round(time.Second)
	h := d / time.Hour
	d -= h * time.Hour
	m := d / time.Minute
	d -= m * time.Minute
	s := d / time.Second
	if h > 0 {
		return fmt.Sprintf("%dh %dm %ds", h, m, s)
	}
	if m > 0 {
		return fmt.Sprintf("%dm %ds", m, s)
	}
	return fmt.Sprintf("%ds", s)
}

// RecordDaemonStart registers the running daemon in local/memory/daemon.json.
func RecordDaemonStart(vaultDir, addr string) error {
	p := daemonStatePath(vaultDir)
	if data, err := os.ReadFile(p); err == nil {
		var existing daemonState
		if json.Unmarshal(data, &existing) == nil && existing.PID > 0 && isProcessAlive(existing.PID) && existing.PID != os.Getpid() {
			return fmt.Errorf("daemon already running with PID %d", existing.PID)
		}
	}
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		return err
	}
	st := daemonState{
		PID:       os.Getpid(),
		Addr:      addr,
		StartedAt: time.Now().Format(time.RFC3339),
	}
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o644)
}

// RecordDaemonStop cleans up local/memory/daemon.json upon daemon shutdown.
func RecordDaemonStop(vaultDir string) error {
	p := daemonStatePath(vaultDir)
	data, err := os.ReadFile(p)
	if err != nil {
		return nil
	}
	var existing daemonState
	if json.Unmarshal(data, &existing) == nil {
		if existing.PID == os.Getpid() || !isProcessAlive(existing.PID) {
			_ = os.Remove(p)
		}
	} else {
		_ = os.Remove(p)
	}
	return nil
}

// IsDaemonActiveFast checks local/memory/daemon.json without making network dials so SessionStart
// and GetUnarmedTriggers can determine in <1ms whether daemon_gatekeeper schedules are actively managed.
func IsDaemonActiveFast(vaultDir string) bool {
	data, err := os.ReadFile(daemonStatePath(vaultDir))
	if err != nil {
		return false
	}
	var st daemonState
	if json.Unmarshal(data, &st) != nil || st.PID <= 0 {
		return false
	}
	return isProcessAlive(st.PID)
}

// GetDaemonInfo returns the status, running state, PID, uptime, and declared triggers.
func GetDaemonInfo(vaultDir string) DaemonInfo {
	triggers, _ := LoadTriggers(vaultDir)
	info := DaemonInfo{
		Running:         false,
		SchedulesCount:  len(triggers.Schedules),
		InboxRulesCount: len(triggers.InboxRules),
		Schedules:       triggers.Schedules,
		InboxRules:      triggers.InboxRules,
	}

	p := daemonStatePath(vaultDir)
	data, err := os.ReadFile(p)
	if err == nil {
		var st daemonState
		if json.Unmarshal(data, &st) == nil && st.PID > 0 {
			if isProcessAlive(st.PID) {
				info.Running = true
				info.PID = st.PID
				info.Addr = st.Addr
				info.StartedAt = st.StartedAt
				if t, err := time.Parse(time.RFC3339, st.StartedAt); err == nil {
					dur := time.Since(t)
					info.UptimeSeconds = int64(dur.Seconds())
					info.UptimeStr = formatDuration(dur)
				}
				return info
			}
			// Process is dead, clean up stale file
			_ = os.Remove(p)
		}
	}

	// Fallback: check if standard daemon port is listening and serving this vaultDir
	defaultAddr := "127.0.0.1:4242"
	if port := os.Getenv("PORT"); port != "" {
		defaultAddr = "127.0.0.1:" + port
	}
	if checkHTTPHealth(defaultAddr, vaultDir) {
		info.Running = true
		info.Addr = defaultAddr
		pid := findDaemonPID(defaultAddr)
		if pid > 0 {
			info.PID = pid
		}
		return info
	}
	if defaultAddr != "127.0.0.1:4747" && checkHTTPHealth("127.0.0.1:4747", vaultDir) {
		info.Running = true
		info.Addr = "127.0.0.1:4747"
		pid := findDaemonPID("127.0.0.1:4747")
		if pid > 0 {
			info.PID = pid
		}
		return info
	}

	return info
}

// FormatDaemonStatus formats DaemonInfo into a human-readable CLI status string.
func FormatDaemonStatus(info DaemonInfo) string {
	var sb strings.Builder
	if info.Running {
		sb.WriteString("agy-hq Daemon Status: RUNNING\n")
		if info.PID > 0 {
			sb.WriteString(fmt.Sprintf("PID:               %d\n", info.PID))
		}
		if info.Addr != "" {
			sb.WriteString(fmt.Sprintf("Listen Address:    http://%s\n", info.Addr))
		}
		if info.UptimeStr != "" {
			sb.WriteString(fmt.Sprintf("Uptime:            %s (started at %s)\n", info.UptimeStr, info.StartedAt))
		}
	} else {
		sb.WriteString("agy-hq Daemon Status: INACTIVE (stopped)\n")
	}

	sb.WriteString(fmt.Sprintf("Declared Triggers: %d schedules, %d inbox rules\n", info.SchedulesCount, info.InboxRulesCount))
	if len(info.Schedules) > 0 {
		sb.WriteString("  Schedules:\n")
		for _, s := range info.Schedules {
			sb.WriteString(fmt.Sprintf("    - %s (cron: %q, model: %s, skill: %s)\n", s.ID, s.Cron, s.Model, s.Skill))
		}
	}
	if len(info.InboxRules) > 0 {
		sb.WriteString("  Inbox Rules:\n")
		for _, r := range info.InboxRules {
			details := []string{fmt.Sprintf("extensions: [%s]", strings.Join(r.Extensions, ", "))}
			if r.FilenameRegex != "" {
				details = append(details, fmt.Sprintf("regex: %q", r.FilenameRegex))
			}
			details = append(details, fmt.Sprintf("skill: %s", r.Skill))
			sb.WriteString(fmt.Sprintf("    - %s (%s)\n", r.Name, strings.Join(details, ", ")))
		}
	}
	if !info.Running {
		sb.WriteString("\nRun './bin/hq daemon install' to enable the background service, or './bin/hq daemon' to run in foreground.\n")
	}
	return sb.String()
}

func getSystemdUserDir() string {
	if d := os.Getenv("HQ_SYSTEMD_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "systemd", "user")
}

func getLaunchdAgentsDir() string {
	if d := os.Getenv("HQ_LAUNCHD_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Library", "LaunchAgents")
}

func commandExists(bin string) bool {
	_, err := exec.LookPath(bin)
	return err == nil
}

// GenerateSystemdService creates valid systemd unit configuration for agy-hq daemon.
func GenerateSystemdService(binPath, rootDir, vaultDir string) string {
	home, _ := os.UserHomeDir()
	envPath := fmt.Sprintf("/opt/homebrew/bin:/usr/local/go/bin:%s/.local/go/bin:%s/.local/bin:/usr/local/bin:/usr/bin:/bin", home, home)
	if p := os.Getenv("PATH"); p != "" {
		envPath = p
	}

	return fmt.Sprintf(`[Unit]
Description=agy-hq Headquarter Daemon & Console
Documentation=https://github.com/zjshen14/agy-hq
After=network.target

[Service]
Type=simple
WorkingDirectory=%s
ExecStart=%s daemon
Restart=always
RestartSec=5
Environment=HQ_ROOT_DIR=%s
Environment=HQ_VAULT_DIR=%s
Environment=PATH=%s

[Install]
WantedBy=default.target
`, rootDir, binPath, rootDir, vaultDir, envPath)
}

// GenerateLaunchdPlist creates valid launchd plist configuration for agy-hq daemon on macOS.
func GenerateLaunchdPlist(binPath, rootDir, vaultDir string) string {
	home, _ := os.UserHomeDir()
	envPath := fmt.Sprintf("/opt/homebrew/bin:/usr/local/go/bin:%s/.local/go/bin:%s/.local/bin:/usr/local/bin:/usr/bin:/bin", home, home)
	if p := os.Getenv("PATH"); p != "" {
		envPath = p
	}
	logPath := filepath.Join(vaultDir, "memory", "daemon.log")

	return fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>com.antigravity.agy-hq</string>
    <key>ProgramArguments</key>
    <array>
        <string>%s</string>
        <string>daemon</string>
    </array>
    <key>WorkingDirectory</key>
    <string>%s</string>
    <key>RunAtLoad</key>
    <true/>
    <key>KeepAlive</key>
    <true/>
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>%s</string>
        <key>HQ_ROOT_DIR</key>
        <string>%s</string>
        <key>HQ_VAULT_DIR</key>
        <string>%s</string>
    </dict>
    <key>StandardOutPath</key>
    <string>%s</string>
    <key>StandardErrorPath</key>
    <string>%s</string>
</dict>
</plist>
`, binPath, rootDir, envPath, rootDir, vaultDir, logPath, logPath)
}

// InstallDaemon writes systemd/launchd configs and enables the background service.
func InstallDaemon(rootDir, vaultDir string) (string, error) {
	binPath := filepath.Join(rootDir, "bin", "hq")
	if _, err := os.Stat(binPath); err != nil {
		if exe, err := os.Executable(); err == nil && !strings.HasPrefix(exe, os.TempDir()) {
			binPath = exe
		}
	}

	switch runtime.GOOS {
	case "linux":
		sysDir := getSystemdUserDir()
		if err := os.MkdirAll(sysDir, 0o755); err != nil {
			return "", fmt.Errorf("create systemd user directory: %w", err)
		}
		servicePath := filepath.Join(sysDir, "agy-hq.service")
		serviceContent := GenerateSystemdService(binPath, rootDir, vaultDir)
		if err := os.WriteFile(servicePath, []byte(serviceContent), 0o644); err != nil {
			return "", fmt.Errorf("write systemd service file: %w", err)
		}

		if os.Getenv("HQ_SKIP_SYSTEMCTL") != "1" && commandExists("systemctl") {
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
			if out, err := exec.Command("systemctl", "--user", "enable", "--now", "agy-hq.service").CombinedOutput(); err != nil {
				return fmt.Sprintf("Installed %s (warning: systemctl --user enable failed: %v: %s)", servicePath, err, strings.TrimSpace(string(out))), nil
			}
			return fmt.Sprintf("Installed and enabled systemd user service: %s\nDaemon is now running via systemctl --user.", servicePath), nil
		}
		return fmt.Sprintf("Wrote systemd user service to %s (systemctl auto-start skipped)", servicePath), nil

	case "darwin":
		launchDir := getLaunchdAgentsDir()
		if err := os.MkdirAll(launchDir, 0o755); err != nil {
			return "", fmt.Errorf("create LaunchAgents directory: %w", err)
		}
		plistPath := filepath.Join(launchDir, "com.antigravity.agy-hq.plist")
		plistContent := GenerateLaunchdPlist(binPath, rootDir, vaultDir)
		if err := os.WriteFile(plistPath, []byte(plistContent), 0o644); err != nil {
			return "", fmt.Errorf("write launchd plist file: %w", err)
		}

		if os.Getenv("HQ_SKIP_LAUNCHCTL") != "1" && commandExists("launchctl") {
			_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()
			if out, err := exec.Command("launchctl", "load", "-w", plistPath).CombinedOutput(); err != nil {
				return fmt.Sprintf("Installed %s (warning: launchctl load failed: %v: %s)", plistPath, err, strings.TrimSpace(string(out))), nil
			}
			return fmt.Sprintf("Installed and loaded launchd agent: %s\nDaemon is now running via launchctl.", plistPath), nil
		}
		return fmt.Sprintf("Wrote launchd agent plist to %s (launchctl auto-load skipped)", plistPath), nil

	default:
		return "", fmt.Errorf("unsupported operating system %q for daemon service management", runtime.GOOS)
	}
}

// UninstallDaemon stops and removes the installed systemd/launchd service.
func UninstallDaemon(vaultDir string) (string, error) {
	var msgs []string

	// Stop any running daemon recorded in memory/daemon.json
	statePath := daemonStatePath(vaultDir)
	if data, err := os.ReadFile(statePath); err == nil {
		var st daemonState
		if json.Unmarshal(data, &st) == nil && st.PID > 0 {
			if isDaemonProcess(st.PID) {
				if proc, err := os.FindProcess(st.PID); err == nil {
					_ = proc.Signal(syscall.SIGTERM)
					msgs = append(msgs, fmt.Sprintf("Stopped running daemon process (PID %d)", st.PID))
				}
			}
		}
		_ = os.Remove(statePath)
	}

	switch runtime.GOOS {
	case "linux":
		sysDir := getSystemdUserDir()
		servicePath := filepath.Join(sysDir, "agy-hq.service")
		if os.Getenv("HQ_SKIP_SYSTEMCTL") != "1" && commandExists("systemctl") {
			_ = exec.Command("systemctl", "--user", "disable", "--now", "agy-hq.service").Run()
			_ = exec.Command("systemctl", "--user", "daemon-reload").Run()
			msgs = append(msgs, "Disabled and stopped agy-hq.service via systemctl --user")
		}
		if err := os.Remove(servicePath); err == nil {
			msgs = append(msgs, fmt.Sprintf("Removed %s", servicePath))
		}
	case "darwin":
		launchDir := getLaunchdAgentsDir()
		plistPath := filepath.Join(launchDir, "com.antigravity.agy-hq.plist")
		if os.Getenv("HQ_SKIP_LAUNCHCTL") != "1" && commandExists("launchctl") {
			_ = exec.Command("launchctl", "unload", "-w", plistPath).Run()
			msgs = append(msgs, "Unloaded com.antigravity.agy-hq via launchctl")
		}
		if err := os.Remove(plistPath); err == nil {
			msgs = append(msgs, fmt.Sprintf("Removed %s", plistPath))
		}
	}

	if len(msgs) == 0 {
		return "No installed daemon service found to remove.", nil
	}
	return strings.Join(msgs, "\n"), nil
}
