package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/store"
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

func writeJSON(w io.Writer, v interface{}) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func printJSON(v interface{}) {
	writeJSON(os.Stdout, v)
}

func printJSONUnescaped(v interface{}) {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func dieIfErr(context string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", context, err)
		os.Exit(1)
	}
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}

func newJSONFlagSet(name string) (*flag.FlagSet, *bool) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	asJSON := fs.Bool("json", false, "Output JSON")
	return fs, asJSON
}

func splitCSV(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if s := strings.TrimSpace(part); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func parseKVFlags(dst map[string]string, items []string, strict bool) (map[string]string, error) {
	if dst == nil {
		dst = make(map[string]string, len(items))
	}
	for _, raw := range items {
		k, v, ok := strings.Cut(raw, "=")
		k = strings.TrimSpace(k)
		if !ok || k == "" {
			if strict {
				return nil, fmt.Errorf("expected key=value pair, got %q", raw)
			}
			continue
		}
		dst[k] = v
	}
	return dst, nil
}

type flexibleArgs struct {
	pos   []string
	vals  map[string]string
	bools map[string]bool
}

func (a flexibleArgs) val(name string) string { return strings.TrimSpace(a.vals[name]) }

// parseFlexibleArgs accepts flags anywhere (before or after positionals) as --name=value, --name value, or
// -name; bool flags also accept --name=true|false.
func parseFlexibleArgs(args []string, valueFlags, boolFlags []string) (flexibleArgs, error) {
	isVal := make(map[string]bool, len(valueFlags))
	for _, f := range valueFlags {
		isVal[f] = true
	}
	isBool := make(map[string]bool, len(boolFlags))
	for _, f := range boolFlags {
		isBool[f] = true
	}
	out := flexibleArgs{vals: make(map[string]string), bools: make(map[string]bool)}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			out.pos = append(out.pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			out.pos = append(out.pos, a)
			continue
		}
		if strings.HasPrefix(a, "---") {
			return out, fmt.Errorf("invalid flag %s", a)
		}
		var name string
		if strings.HasPrefix(a, "--") {
			name = a[2:]
		} else {
			name = a[1:]
		}
		if name == "" || strings.HasPrefix(name, "-") {
			return out, fmt.Errorf("invalid flag %s", a)
		}
		val, hasVal := "", false
		if k, v, ok := strings.Cut(name, "="); ok {
			name, val, hasVal = k, v, true
		}
		switch {
		case isVal[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return out, fmt.Errorf("flag --%s needs a value", name)
				}
				i++
				val = args[i]
			}
			out.vals[name] = val
		case isBool[name]:
			b := true
			if hasVal {
				parsed, err := strconv.ParseBool(val)
				if err != nil {
					return out, fmt.Errorf("flag --%s expects true or false", name)
				}
				b = parsed
			}
			out.bools[name] = b
		default:
			return out, fmt.Errorf("unknown flag %s", a)
		}
	}
	return out, nil
}

func shortPrefix(s string, n int) string {
	if n <= 0 {
		return ""
	}
	if len(s) <= n {
		return s
	}
	for i := 0; i < n; i++ {
		if s[i] >= utf8.RuneSelf {
			return store.TruncateRunesWithSuffix(s, n, "")
		}
	}
	return s[:n]
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
	return store.TruncateRunes(strings.TrimSpace(s), maxLen)
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
	if exe, err := os.Executable(); err == nil && hostpaths.IsTempPath(exe) {
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
