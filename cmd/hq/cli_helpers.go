package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/vault"
)

type repeatFlag []string

func (rf *repeatFlag) String() string { return strings.Join(*rf, ",") }
func (rf *repeatFlag) Set(val string) error {
	*rf = append(*rf, val)
	return nil
}

func splitSubcommand(args []string, defaultSub string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return defaultSub, args
}

func splitFirstPositionalAndFlags(args []string) (string, []string) {
	var pos string
	var flagArgs []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") && pos == "" {
			pos = a
		} else {
			flagArgs = append(flagArgs, a)
		}
	}
	return pos, flagArgs
}

func splitPositionalAndFlags(args []string) ([]string, []string) {
	var pos, fl []string
	for _, a := range args {
		if !strings.HasPrefix(a, "-") {
			pos = append(pos, a)
		} else {
			fl = append(fl, a)
		}
	}
	return pos, fl
}

func printJSON(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func printJSONUnescaped(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func shortPrefix(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func parseFlexibleDuration(raw string) (time.Duration, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if strings.HasSuffix(s, "d") {
		days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("invalid day duration %q", raw)
		}
		return time.Duration(days) * 24 * time.Hour, nil
	}
	return time.ParseDuration(s)
}

func parseSinceFlag(raw string, now time.Time) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return time.Time{}, nil
	}
	if d, err := parseFlexibleDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if ts, err := time.Parse(time.RFC3339, s); err == nil {
		return ts.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid --since value %q (expected duration like 24h/7d or RFC3339)", s)
}

func parseOptionalNowFlag(raw string) time.Time {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return time.Now()
	}
	if t, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return t
	}
	return time.Now()
}

func checkCallerTaintedEnv(vaultDir string) (taint.Record, bool) {
	convID := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	if convID == "" {
		return taint.Record{}, false
	}
	return taint.Check(vaultDir, convID)
}

func denyIfCallerTainted(vaultDir, verb string) {
	if rec, ok := checkCallerTaintedEnv(vaultDir); ok {
		fmt.Fprintf(os.Stderr, "hq %s blocked: conversation %s is quarantined (%s)\n", verb, rec.ConvID, rec.Source)
		os.Exit(1)
	}
}

func truncateDisplay(s string, maxLen int) string {
	s = strings.TrimSpace(s)
	if len(s) <= maxLen {
		return s
	}
	if maxLen <= 3 {
		return s[:maxLen]
	}
	return s[:maxLen-3] + "..."
}

func resolveTemplateDir(rootDir string) string {
	if vault.IsAgyHQRepo(rootDir) || strings.TrimSpace(os.Getenv("HQ_ROOT_DIR")) != "" {
		dir := filepath.Join(rootDir, "templates")
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir
		}
	}
	return ""
}

func resolveTaskScope(scopeOverride, repoSpec, taskName string) string {
	scope := "general"
	if scopeOverride != "" {
		scope = scopeOverride
	} else if repoSpec != "" {
		scope = filepath.Base(strings.TrimSuffix(repoSpec, ".git"))
	} else if parts := strings.SplitN(taskName, "-", 2); len(parts) > 0 && parts[0] != "" {
		scope = parts[0]
	}
	if scope == "investment" {
		scope = "investing"
	}
	return scope
}

func resolveDefaultWorkDir(rootDir string) string {
	if vault.IsAgyHQRepo(rootDir) {
		return rootDir
	}
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		return cwd
	}
	return rootDir
}

func resolveProposalHQBin(rootDir string) string {
	if exe, err := os.Executable(); err == nil && vault.IsTempPath(exe) {
		return exe
	}
	if vault.IsAgyHQRepo(rootDir) {
		if st, err := os.Stat(filepath.Join(rootDir, "bin", "hq")); err == nil && !st.IsDir() {
			return "./bin/hq"
		}
	}
	if _, err := exec.LookPath("hq"); err == nil {
		return "hq"
	}
	if exe, err := os.Executable(); err == nil && strings.TrimSpace(exe) != "" {
		return exe
	}
	return "hq"
}

func displayHQCmd(rootDir string) string {
	if vault.IsAgyHQRepo(rootDir) {
		if st, err := os.Stat(filepath.Join(rootDir, "bin", "hq")); err == nil && !st.IsDir() {
			return "./bin/hq"
		}
	}
	return "hq"
}
