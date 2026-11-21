package delegates

import (
	"bytes"
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
	"text/template"
	"text/template/parse"
	"time"
	"unicode/utf8"

	"github.com/zjshen14/agy-hq/internal/authz"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/taint"
	"github.com/zjshen14/agy-hq/internal/vault"
)

const (
	PolicySchemaVersion            = 1
	DefaultGlobalMaxRepliesPerHour = 20
	DefaultPolicyMaxRepliesPerHour = 10
	DefaultMaxBodyRunes            = 1800
	DefaultMaxCmdRunsPerRequest    = 5
	MaxCmdRunsPerRequestCeiling    = 15
	DefaultMaxEscalationsPerHour   = 10
	DefaultGrantTTLSeconds         = 900 // 15 minutes
	MaxGrantTTLSeconds             = 3600
	DefaultCommandTimeoutMs        = 8000
	MaxCommandTimeoutMs            = 30000
	MaxRegexArgRunes               = 64
	TrustSourceDelegates           = "delegates"
)

var (
	reValidSlugID      = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	reValidArgName     = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]{0,47}$`)
	reValidFlagName    = regexp.MustCompile(`^-{1,2}[a-zA-Z0-9][a-zA-Z0-9._-]{0,47}$`)
	reValidCallerEmail = regexp.MustCompile(`^[a-z0-9._%+\-]+@[a-z0-9.\-]+\.[a-z]{2,}$`)
	reValidCallerID    = regexp.MustCompile(`^[a-z0-9._:/@-]{3,128}$`)
	reValidEnvKey      = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`)
	reNumericOnlyRegex = regexp.MustCompile(`^\^(\[0-9\]|\\d)([+*?]|\{[0-9,]+\})?\$$`)
	reValidSourceSpec  = regexp.MustCompile(`^transport:[a-z0-9][a-z0-9-]{1,62}/[a-z0-9][a-z0-9-]{1,62}$`)
	reValidSkillGrant  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}/[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	reValidTaskGrant   = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,79}$`)
	reValidWSGrant     = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{2,199}$`)
)

// StringOrLines decodes either a JSON string or an array of strings joined with "\n".
type StringOrLines string

func (s *StringOrLines) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		*s = ""
		return nil
	}
	if trimmed[0] == '"' {
		var single string
		if err := json.Unmarshal(trimmed, &single); err != nil {
			return err
		}
		*s = StringOrLines(single)
		return nil
	}
	if trimmed[0] == '[' {
		var lines []string
		if err := json.Unmarshal(trimmed, &lines); err != nil {
			return err
		}
		*s = StringOrLines(strings.Join(lines, "\n"))
		return nil
	}
	return fmt.Errorf("expected string or array of strings")
}

// Config is the sovereign JSONC policy document stored at <vault>/private/delegates/policy.json (RFC-0026 §6.2).
type Config struct {
	SchemaVersion int                    `json:"schema_version"`
	Defaults      DefaultsConfig         `json:"defaults,omitempty"`
	CallerGroups  map[string][]string    `json:"caller_groups,omitempty"`
	Commands      map[string]CommandSpec `json:"commands,omitempty"`
	Policies      []PolicySpec           `json:"policies,omitempty"`
	Snapshots     []SnapshotSpec         `json:"snapshots,omitempty"`
}

// DefaultsConfig defines global defaults and Ring-0 egress guards shared across all policies.
type DefaultsConfig struct {
	Sources                  []string `json:"sources,omitempty"`
	GlobalMaxRepliesPerHour  int      `json:"global_max_replies_per_hour,omitempty"`
	AuditTier                string   `json:"audit_tier,omitempty"`
	BlockedPatterns          []string `json:"blocked_patterns,omitempty"`
	SensitiveTerms           []string `json:"sensitive_terms,omitempty"`
	MaxCommandRunsPerRequest int      `json:"max_command_runs_per_request,omitempty"`
	MaxEscalationsPerHour    int      `json:"max_escalations_per_hour,omitempty"`
}

// CommandArgSpec defines a strictly typed parameter for a pinned command, nudge, or structured egress template.
type CommandArgSpec struct {
	Name     string   `json:"name"`
	Flag     string   `json:"flag,omitempty"`
	Type     string   `json:"type"` // "int" | "enum" | "regex"
	Min      *int     `json:"min,omitempty"`
	Max      *int     `json:"max,omitempty"`
	Values   []string `json:"values,omitempty"`
	Pattern  string   `json:"pattern,omitempty"`
	Default  string   `json:"default,omitempty"`
	Required bool     `json:"required,omitempty"`
}

// CommandSpec defines a top-level SHA-256-pinned non-shell read-only command.
type CommandSpec struct {
	Description string           `json:"description,omitempty"`
	Command     []string         `json:"command"`
	SourcePack  string           `json:"source_pack,omitempty"`
	EnvAllow    []string         `json:"env_allow,omitempty"`
	TimeoutMs   int              `json:"timeout_ms,omitempty"`
	Args        []CommandArgSpec `json:"args,omitempty"`
}

// MatchSpec defines how an incoming peer request matches a policy.
type MatchSpec struct {
	Callers         []string `json:"callers"`
	Sources         []string `json:"sources,omitempty"`
	Regex           string   `json:"regex,omitempty"`
	UrgencyKeywords []string `json:"urgency_keywords,omitempty"`
	Keywords        []string `json:"keywords,omitempty"`
}

// NudgeSpec defines a CaMeL parameterized task action:
//   - "queue_only": appends a static priority item to memory/delegates_queues/<slug>.jsonl (Zone 2 on next interactive turn).
//   - "spawn_sandboxed_worker": spawns an isolated unattended_auto worker (OS-sandboxed + NI-3 auto_propose_only)
//     from a 100% static Go template to execute investigative/drafting work autonomously and stage any external action.
type NudgeSpec struct {
	ID                         string           `json:"id"`
	DeliveryMode               string           `json:"delivery_mode"` // "queue_only" | "spawn_sandboxed_worker"
	TargetTaskSlug             string           `json:"target_task_slug,omitempty"`
	TargetTaskScope            string           `json:"target_task_scope,omitempty"`
	WorkerSkill                string           `json:"worker_skill,omitempty"`
	WorkerTier                 string           `json:"worker_tier,omitempty"` // "quick" | "routine" | "standard" | "heavy" | "deep"
	CooldownSeconds            int              `json:"cooldown_seconds,omitempty"`
	MaxNudgesPerHour           int              `json:"max_nudges_per_hour,omitempty"`
	Params                     []CommandArgSpec `json:"params,omitempty"`
	PreflightCommand           string           `json:"preflight_command,omitempty"`
	OnPreflightFail            string           `json:"on_preflight_fail,omitempty"` // "escalate" | "reply_uniform"
	PreflightFailReplyTemplate StringOrLines    `json:"preflight_fail_reply_template,omitempty"`
	Template                   StringOrLines    `json:"template"`
	AckReplyTemplate           StringOrLines    `json:"ack_reply_template,omitempty"`
}

// GrantsSpec enumerates explicit non-wildcard resources granted to a scoped delegate policy.
type GrantsSpec struct {
	Tasks      []string    `json:"tasks,omitempty"`
	Workspaces []string    `json:"workspaces,omitempty"`
	Skills     []string    `json:"skills,omitempty"`
	Commands   []string    `json:"commands,omitempty"`
	Nudges     []NudgeSpec `json:"nudges,omitempty"`
}

// EgressConfig governs how a scoped delegate policy replies or escalates.
type EgressConfig struct {
	Mode                 string           `json:"mode"`                // "none" | "escalate" | "stage_approval" | "auto_reply" | "structured_template"
	FailMode             string           `json:"fail_mode,omitempty"` // "stage_approval" | "escalate"
	MaxRepliesPerHour    int              `json:"max_replies_per_hour,omitempty"`
	MaxBodyRunes         int              `json:"max_body_runes,omitempty"`
	BlockedPatterns      []string         `json:"blocked_patterns,omitempty"`
	StructuredTemplate   StringOrLines    `json:"structured_template,omitempty"`
	StructuredFields     []CommandArgSpec `json:"structured_fields,omitempty"`
	EscalatedAckTemplate StringOrLines    `json:"escalated_ack_template,omitempty"`
}

// PolicySpec defines a single orthogonal scoped delegate policy.
type PolicySpec struct {
	ID                       string       `json:"id"`
	Enabled                  *bool        `json:"enabled,omitempty"`
	Description              string       `json:"description,omitempty"`
	ScheduleWindow           string       `json:"schedule_window,omitempty"`
	MaxCommandRunsPerRequest int          `json:"max_command_runs_per_request,omitempty"`
	GrantTTLSeconds          int          `json:"grant_ttl_seconds,omitempty"`
	Match                    MatchSpec    `json:"match"`
	Grants                   GrantsSpec   `json:"grants,omitempty"`
	Egress                   EgressConfig `json:"egress"`
}

// IsEnabled reports whether the policy is active (defaults to true when omitted).
func (p PolicySpec) IsEnabled() bool {
	if p.Enabled == nil {
		return true
	}
	return *p.Enabled
}

// SnapshotCommandSpec defines a fixed-argument command executed during 0-LLM snapshot publishing.
type SnapshotCommandSpec struct {
	Command   string            `json:"command"`
	FixedArgs map[string]string `json:"fixed_args,omitempty"`
}

// SnapshotSpec configures periodic 0-LLM-token offline status snapshots via transport.publish_snapshot.
type SnapshotSpec struct {
	ID              string                `json:"id"`
	Enabled         *bool                 `json:"enabled,omitempty"`
	Source          string                `json:"source"`
	IntervalMinutes int                   `json:"interval_minutes"`
	Tasks           []string              `json:"tasks,omitempty"`
	Workspaces      []string              `json:"workspaces,omitempty"`
	Commands        []SnapshotCommandSpec `json:"commands,omitempty"`
}

// IsEnabled reports whether the snapshot is active (defaults to true when omitted).
func (s SnapshotSpec) IsEnabled() bool {
	if s.Enabled == nil {
		return true
	}
	return *s.Enabled
}

// TemplateData is the strictly restricted data struct passed to Go text/template rendering.
// Note: raw peer Body and Subject fields DO NOT EXIST on this struct by design (CaMeL invariant).
type TemplateData struct {
	CallerID  string
	PolicyID  string
	RequestID string
	Params    map[string]string
}

// ValidateOptions tunes static validation of a delegate Config.
type ValidateOptions struct {
	CheckBinaryPins bool
}

// PolicyFilePath returns <vault>/private/delegates/policy.json.
func PolicyFilePath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "delegates", "policy.json")
}

// StripJSONC strips // line comments, /* block comments */, and trailing commas before } or ]
// while preserving string literals verbatim (pure Go stdlib, quote-aware).
func StripJSONC(raw []byte) []byte {
	var clean bytes.Buffer
	clean.Grow(len(raw))
	inStr := false
	escaped := false
	n := len(raw)

	for i := 0; i < n; i++ {
		c := raw[i]
		if inStr {
			clean.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			clean.WriteByte(c)
			continue
		}
		if c == '/' && i+1 < n {
			if raw[i+1] == '/' {
				i += 2
				for i < n && raw[i] != '\n' {
					i++
				}
				if i < n {
					clean.WriteByte('\n')
				}
				continue
			}
			if raw[i+1] == '*' {
				i += 2
				for i+1 < n && !(raw[i] == '*' && raw[i+1] == '/') {
					if raw[i] == '\n' {
						clean.WriteByte('\n')
					}
					i++
				}
				i++ // consume '/'
				continue
			}
		}
		clean.WriteByte(c)
	}

	// Second pass: remove trailing commas before '}' or ']' outside string literals.
	src := clean.Bytes()
	var out bytes.Buffer
	out.Grow(len(src))
	inStr = false
	escaped = false
	for i := 0; i < len(src); i++ {
		c := src[i]
		if inStr {
			out.WriteByte(c)
			if escaped {
				escaped = false
			} else if c == '\\' {
				escaped = true
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		if c == '"' {
			inStr = true
			out.WriteByte(c)
			continue
		}
		if c == ',' {
			j := i + 1
			for j < len(src) && (src[j] == ' ' || src[j] == '\t' || src[j] == '\n' || src[j] == '\r') {
				j++
			}
			if j < len(src) && (src[j] == '}' || src[j] == ']') {
				continue
			}
		}
		out.WriteByte(c)
	}
	return out.Bytes()
}

// ParseConfig parses raw JSONC into a Config with DisallowUnknownFields().
func ParseConfig(raw []byte) (Config, error) {
	stripped := StripJSONC(raw)
	var cfg Config
	dec := json.NewDecoder(bytes.NewReader(stripped))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse delegate policy JSONC: %w", err)
	}
	var trailing interface{}
	if err := dec.Decode(&trailing); err == nil {
		return Config{}, fmt.Errorf("parse delegate policy JSONC: unexpected trailing JSON value")
	}
	return cfg, nil
}

// LoadConfig reads and parses <vault>/private/delegates/policy.json along with its SHA-256 hex digest.
func LoadConfig(vaultDir string) (Config, string, bool, error) {
	p := PolicyFilePath(vaultDir)
	st, err := os.Lstat(p)
	if err != nil {
		if os.IsNotExist(err) {
			return Config{}, "", false, nil
		}
		return Config{}, "", false, err
	}
	if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
		return Config{}, "", false, fmt.Errorf("refusing symlinked or non-regular delegate policy %s", p)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return Config{}, "", false, err
	}
	sum := sha256.Sum256(raw)
	cfg, err := ParseConfig(raw)
	if err != nil {
		return Config{}, hex.EncodeToString(sum[:]), true, err
	}
	return cfg, hex.EncodeToString(sum[:]), true, nil
}

// NormalizeCallerID canonicalizes and validates an ASCII caller identifier (email or handle).
func NormalizeCallerID(raw string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(raw))
	if s == "" {
		return "", fmt.Errorf("empty caller_id")
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return "", fmt.Errorf("caller_id %q contains non-ASCII or whitespace/control characters", raw)
		}
	}
	if strings.Contains(s, "@") {
		if !reValidCallerEmail.MatchString(s) && !reValidCallerID.MatchString(s) {
			return "", fmt.Errorf("invalid caller_id %q", raw)
		}
		return s, nil
	}
	if !reValidCallerID.MatchString(s) {
		return "", fmt.Errorf("invalid caller_id %q", raw)
	}
	return s, nil
}

// ExpandPolicyCallers resolves literal callers and local @group references into a canonical set.
func ExpandPolicyCallers(cfg Config, pol PolicySpec) map[string]bool {
	out := make(map[string]bool)
	for _, entry := range pol.Match.Callers {
		e := strings.TrimSpace(entry)
		if e == "" {
			continue
		}
		if strings.HasPrefix(e, "@") {
			grp := strings.TrimPrefix(e, "@")
			for _, member := range cfg.CallerGroups[grp] {
				if norm, err := NormalizeCallerID(member); err == nil {
					out[norm] = true
				}
			}
			continue
		}
		if norm, err := NormalizeCallerID(e); err == nil {
			out[norm] = true
		}
	}
	return out
}

// ValidateTypedArgValue validates rawVal against spec (int, enum, or anchored regex <= 64 runes, no leading '-').
func ValidateTypedArgValue(spec CommandArgSpec, rawVal string) (string, error) {
	val := strings.TrimSpace(rawVal)
	if val == "" {
		if spec.Default != "" {
			val = spec.Default
		} else if spec.Required {
			return "", fmt.Errorf("missing required parameter %q", spec.Name)
		} else {
			return "", nil
		}
	}
	for _, r := range val {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("parameter %q contains control characters", spec.Name)
		}
	}
	switch spec.Type {
	case "int":
		n, err := strconv.Atoi(val)
		if err != nil {
			return "", fmt.Errorf("parameter %q expects int, got %q", spec.Name, val)
		}
		if spec.Min != nil && n < *spec.Min {
			return "", fmt.Errorf("parameter %q=%d below min %d", spec.Name, n, *spec.Min)
		}
		if spec.Max != nil && n > *spec.Max {
			return "", fmt.Errorf("parameter %q=%d exceeds max %d", spec.Name, n, *spec.Max)
		}
		return strconv.Itoa(n), nil
	case "enum":
		for _, allowed := range spec.Values {
			if val == allowed {
				return val, nil
			}
		}
		return "", fmt.Errorf("parameter %q=%q not in allowed enum %v", spec.Name, val, spec.Values)
	case "regex":
		if strings.HasPrefix(val, "-") {
			return "", fmt.Errorf("parameter %q value %q cannot start with '-' (anti-flag-injection)", spec.Name, val)
		}
		if utf8.RuneCountInString(val) > MaxRegexArgRunes {
			return "", fmt.Errorf("parameter %q exceeds max length %d runes", spec.Name, MaxRegexArgRunes)
		}
		re, err := regexp.Compile(spec.Pattern)
		if err != nil {
			return "", fmt.Errorf("parameter %q has invalid pattern: %w", spec.Name, err)
		}
		if !re.MatchString(val) {
			return "", fmt.Errorf("parameter %q=%q does not match pattern %q", spec.Name, val, spec.Pattern)
		}
		return val, nil
	default:
		return "", fmt.Errorf("unsupported parameter type %q for %q", spec.Type, spec.Name)
	}
}

// ValidateSafeTemplate parses a Go text/template with an empty FuncMap and verifies via AST inspection
// that ONLY .CallerID, .PolicyID, .RequestID, and .Params.<declaredParam> are referenced.
func ValidateSafeTemplate(label, tmplStr string, allowedParams map[string]bool) error {
	if strings.TrimSpace(tmplStr) == "" {
		return nil
	}
	t, err := template.New(label).Option("missingkey=error").Funcs(template.FuncMap{}).Parse(tmplStr)
	if err != nil {
		return fmt.Errorf("%s invalid Go template: %w", label, err)
	}
	if t.Tree == nil || t.Tree.Root == nil {
		return nil
	}
	var walk func(n parse.Node) error
	walk = func(n parse.Node) error {
		if n == nil {
			return nil
		}
		switch node := n.(type) {
		case *parse.ListNode:
			if node == nil {
				return nil
			}
			for _, child := range node.Nodes {
				if err := walk(child); err != nil {
					return err
				}
			}
		case *parse.TextNode, *parse.CommentNode:
			return nil
		case *parse.ActionNode:
			return walk(node.Pipe)
		case *parse.IfNode:
			if err := walk(node.Pipe); err != nil {
				return err
			}
			if err := walk(node.List); err != nil {
				return err
			}
			return walk(node.ElseList)
		case *parse.PipeNode:
			if node == nil {
				return nil
			}
			if len(node.Decl) > 0 {
				return fmt.Errorf("%s template variable declarations are forbidden", label)
			}
			if len(node.Cmds) != 1 {
				return fmt.Errorf("%s template pipelines are forbidden", label)
			}
			return walk(node.Cmds[0])
		case *parse.CommandNode:
			if len(node.Args) != 1 {
				return fmt.Errorf("%s template function calls/arguments are forbidden", label)
			}
			return walk(node.Args[0])
		case *parse.FieldNode:
			if len(node.Ident) == 1 {
				switch node.Ident[0] {
				case "CallerID", "PolicyID", "RequestID":
					return nil
				default:
					return fmt.Errorf("%s template field .%s is not allowed (only .CallerID, .PolicyID, .RequestID, .Params.<name>)", label, node.Ident[0])
				}
			}
			if len(node.Ident) == 2 && node.Ident[0] == "Params" {
				if !allowedParams[node.Ident[1]] {
					return fmt.Errorf("%s template references undeclared param .Params.%s", label, node.Ident[1])
				}
				return nil
			}
			return fmt.Errorf("%s template field .%s is not allowed", label, strings.Join(node.Ident, "."))
		default:
			return fmt.Errorf("%s template construct %T is forbidden", label, n)
		}
		return nil
	}
	return walk(t.Tree.Root)
}

// RenderSafeTemplate executes a validated static Go template with an empty FuncMap and TemplateData.
func RenderSafeTemplate(tmplStr string, data TemplateData) (string, error) {
	if strings.TrimSpace(tmplStr) == "" {
		return "", nil
	}
	t, err := template.New("delegate").Option("missingkey=error").Funcs(template.FuncMap{}).Parse(tmplStr)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

// CompilePolicyGrant builds the SessionGrant for pol + (callerID, reqID, now) using authz.CompileDelegateVerbs.
func CompilePolicyGrant(pol PolicySpec, grantID, callerID, reqID string, now time.Time) taint.SessionGrant {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	ttl := pol.GrantTTLSeconds
	if ttl <= 0 {
		ttl = DefaultGrantTTLSeconds
	}
	if ttl > MaxGrantTTLSeconds {
		ttl = MaxGrantTTLSeconds
	}
	nudgeIDs := make([]string, 0, len(pol.Grants.Nudges))
	for _, n := range pol.Grants.Nudges {
		nudgeIDs = append(nudgeIDs, n.ID)
	}
	g := taint.SessionGrant{
		GrantID:    grantID,
		Profile:    string(authz.ProfileScopedDelegate),
		PolicyID:   pol.ID,
		CallerID:   callerID,
		RequestID:  reqID,
		ExpiresAt:  now.UTC().Add(time.Duration(ttl) * time.Second).Format(time.RFC3339),
		Tasks:      append([]string(nil), pol.Grants.Tasks...),
		Workspaces: append([]string(nil), pol.Grants.Workspaces...),
		Skills:     append([]string(nil), pol.Grants.Skills...),
		Commands:   append([]string(nil), pol.Grants.Commands...),
		NudgeIDs:   nudgeIDs,
		EgressMode: pol.Egress.Mode,
	}
	g.AllowedHQVerbs = authz.CompileDelegateVerbs(g)
	return g
}

func validateArgSpec(ctxLabel string, arg CommandArgSpec) error {
	if !reValidArgName.MatchString(arg.Name) {
		return fmt.Errorf("%s invalid arg name %q", ctxLabel, arg.Name)
	}
	if arg.Flag != "" && !reValidFlagName.MatchString(arg.Flag) {
		return fmt.Errorf("%s arg %q invalid flag %q", ctxLabel, arg.Name, arg.Flag)
	}
	switch arg.Type {
	case "int":
		if arg.Min != nil && arg.Max != nil && *arg.Min > *arg.Max {
			return fmt.Errorf("%s arg %q min %d > max %d", ctxLabel, arg.Name, *arg.Min, *arg.Max)
		}
		if arg.Default != "" {
			if _, err := ValidateTypedArgValue(arg, arg.Default); err != nil {
				return fmt.Errorf("%s arg %q invalid default: %w", ctxLabel, arg.Name, err)
			}
		}
	case "enum":
		if len(arg.Values) == 0 {
			return fmt.Errorf("%s enum arg %q requires non-empty values", ctxLabel, arg.Name)
		}
		for _, v := range arg.Values {
			if strings.TrimSpace(v) == "" || strings.HasPrefix(v, "-") {
				return fmt.Errorf("%s enum arg %q has invalid value %q", ctxLabel, arg.Name, v)
			}
		}
		if arg.Default != "" {
			if _, err := ValidateTypedArgValue(arg, arg.Default); err != nil {
				return fmt.Errorf("%s arg %q invalid default: %w", ctxLabel, arg.Name, err)
			}
		}
	case "regex":
		p := strings.TrimSpace(arg.Pattern)
		if !strings.HasPrefix(p, "^") || !strings.HasSuffix(p, "$") {
			return fmt.Errorf("%s regex arg %q pattern %q must be anchored with ^...$", ctxLabel, arg.Name, arg.Pattern)
		}
		if _, err := regexp.Compile(p); err != nil {
			return fmt.Errorf("%s regex arg %q invalid pattern: %w", ctxLabel, arg.Name, err)
		}
		if arg.Default != "" {
			if _, err := ValidateTypedArgValue(arg, arg.Default); err != nil {
				return fmt.Errorf("%s arg %q invalid default: %w", ctxLabel, arg.Name, err)
			}
		}
	default:
		return fmt.Errorf("%s arg %q unsupported type %q (expected int, enum, or regex)", ctxLabel, arg.Name, arg.Type)
	}
	return nil
}

// ValidateConfig performs strict static validation of a delegate Config (RFC-0026 §6.3),
// returning non-fatal operator warnings alongside any hard validation error.
func ValidateConfig(vaultDir string, cfg Config, opts ValidateOptions) ([]string, error) {
	if cfg.SchemaVersion != PolicySchemaVersion {
		return nil, fmt.Errorf("unsupported schema_version %d (expected %d)", cfg.SchemaVersion, PolicySchemaVersion)
	}
	var warnings []string

	// Validate defaults
	for _, src := range cfg.Defaults.Sources {
		if !reValidSourceSpec.MatchString(strings.TrimSpace(src)) {
			return nil, fmt.Errorf("defaults.sources invalid entry %q (expected transport:<pack>/<id>)", src)
		}
	}
	for _, pat := range cfg.Defaults.BlockedPatterns {
		if _, err := regexp.Compile(pat); err != nil {
			return nil, fmt.Errorf("defaults.blocked_patterns invalid regex %q: %w", pat, err)
		}
	}
	if cfg.Defaults.MaxCommandRunsPerRequest < 0 || cfg.Defaults.MaxCommandRunsPerRequest > MaxCmdRunsPerRequestCeiling {
		return nil, fmt.Errorf("defaults.max_command_runs_per_request=%d out of range [0, %d]", cfg.Defaults.MaxCommandRunsPerRequest, MaxCmdRunsPerRequestCeiling)
	}

	// Validate caller_groups
	for grp, members := range cfg.CallerGroups {
		if !reValidSlugID.MatchString(grp) {
			return nil, fmt.Errorf("caller_groups invalid group name %q", grp)
		}
		if len(members) == 0 {
			return nil, fmt.Errorf("caller_groups.%s must not be empty", grp)
		}
		for _, m := range members {
			if _, err := NormalizeCallerID(m); err != nil {
				return nil, fmt.Errorf("caller_groups.%s: %w", grp, err)
			}
		}
	}

	// Validate commands
	for cmdID, cmd := range cfg.Commands {
		if !reValidSlugID.MatchString(cmdID) {
			return nil, fmt.Errorf("commands invalid id %q", cmdID)
		}
		if len(cmd.Command) == 0 || strings.TrimSpace(cmd.Command[0]) == "" {
			return nil, fmt.Errorf("commands.%s requires non-empty command array", cmdID)
		}
		if extension.IsBlockedComponentCommand0(cmd.Command[0]) {
			return nil, fmt.Errorf("commands.%s command[0] %q cannot be a shell, interpreter, or hq itself", cmdID, cmd.Command[0])
		}
		if cmd.TimeoutMs < 0 || cmd.TimeoutMs > MaxCommandTimeoutMs {
			return nil, fmt.Errorf("commands.%s timeout_ms=%d out of range [0, %d]", cmdID, cmd.TimeoutMs, MaxCommandTimeoutMs)
		}
		for _, ek := range cmd.EnvAllow {
			if !reValidEnvKey.MatchString(strings.TrimSpace(ek)) {
				return nil, fmt.Errorf("commands.%s invalid env_allow %q", cmdID, ek)
			}
		}
		seenArgs := make(map[string]bool)
		for _, a := range cmd.Args {
			if seenArgs[a.Name] {
				return nil, fmt.Errorf("commands.%s duplicate arg %q", cmdID, a.Name)
			}
			seenArgs[a.Name] = true
			if err := validateArgSpec("commands."+cmdID, a); err != nil {
				return nil, err
			}
		}
		if opts.CheckBinaryPins && strings.TrimSpace(vaultDir) != "" {
			trustSrc := TrustSourceDelegates
			if strings.TrimSpace(cmd.SourcePack) != "" {
				trustSrc = "pack:" + strings.TrimSpace(cmd.SourcePack)
			}
			if _, err := extension.VerifyTrustedBinaryReadOnly(vaultDir, trustSrc, cmd.Command[0]); err != nil {
				return nil, fmt.Errorf("commands.%s binary pin check failed (%s): %w", cmdID, trustSrc, err)
			}
		}
	}

	// Validate policies
	seenPolicyIDs := make(map[string]bool)
	for _, pol := range cfg.Policies {
		if !reValidSlugID.MatchString(pol.ID) {
			return nil, fmt.Errorf("policies invalid id %q", pol.ID)
		}
		if seenPolicyIDs[pol.ID] {
			return nil, fmt.Errorf("duplicate policy id %q", pol.ID)
		}
		seenPolicyIDs[pol.ID] = true

		if len(pol.Match.Callers) == 0 {
			return nil, fmt.Errorf("policy %q match.callers must not be empty", pol.ID)
		}
		for _, c := range pol.Match.Callers {
			c = strings.TrimSpace(c)
			if strings.HasPrefix(c, "@") {
				grp := strings.TrimPrefix(c, "@")
				if _, ok := cfg.CallerGroups[grp]; !ok {
					return nil, fmt.Errorf("policy %q references unknown caller group %q", pol.ID, c)
				}
			} else if _, err := NormalizeCallerID(c); err != nil {
				return nil, fmt.Errorf("policy %q match.callers: %w", pol.ID, err)
			}
		}
		for _, src := range pol.Match.Sources {
			if !reValidSourceSpec.MatchString(strings.TrimSpace(src)) {
				return nil, fmt.Errorf("policy %q match.sources invalid entry %q", pol.ID, src)
			}
		}
		if pol.Match.Regex != "" {
			if _, err := regexp.Compile(pol.Match.Regex); err != nil {
				return nil, fmt.Errorf("policy %q match.regex invalid: %w", pol.ID, err)
			}
		}
		if pol.MaxCommandRunsPerRequest < 0 || pol.MaxCommandRunsPerRequest > MaxCmdRunsPerRequestCeiling {
			return nil, fmt.Errorf("policy %q max_command_runs_per_request=%d out of range [0, %d]", pol.ID, pol.MaxCommandRunsPerRequest, MaxCmdRunsPerRequestCeiling)
		}

		// Zero wildcards on grants.tasks & grants.workspaces
		for _, slug := range pol.Grants.Tasks {
			if strings.ContainsAny(slug, "*?[") || !reValidTaskGrant.MatchString(slug) {
				return nil, fmt.Errorf("policy %q grants.tasks entry %q invalid (wildcards forbidden)", pol.ID, slug)
			}
			if strings.TrimSpace(vaultDir) != "" {
				if _, _, hasPub, _ := vault.LoadPublicStatus(vaultDir, slug); !hasPub {
					warnings = append(warnings, fmt.Sprintf("policy %q grants task %q which has no curated Public Status yet (run `hq progress save --task=%s --public-status=\"...\"`)", pol.ID, slug, slug))
				}
			}
		}
		for _, ws := range pol.Grants.Workspaces {
			if strings.ContainsAny(ws, "*?[") || !reValidWSGrant.MatchString(ws) {
				return nil, fmt.Errorf("policy %q grants.workspaces entry %q invalid (wildcards forbidden)", pol.ID, ws)
			}
		}
		for _, sk := range pol.Grants.Skills {
			if strings.ContainsAny(sk, "*?[") || !reValidSkillGrant.MatchString(sk) {
				return nil, fmt.Errorf("policy %q grants.skills entry %q invalid (expected <pack>/<skill>)", pol.ID, sk)
			}
		}
		hasReads := len(pol.Grants.Tasks) > 0 || len(pol.Grants.Workspaces) > 0
		for _, cid := range pol.Grants.Commands {
			cspec, ok := cfg.Commands[cid]
			if !ok {
				return nil, fmt.Errorf("policy %q grants.commands references unknown command %q", pol.ID, cid)
			}
			if hasReads {
				for _, a := range cspec.Args {
					if a.Type == "regex" && !reNumericOnlyRegex.MatchString(strings.TrimSpace(a.Pattern)) {
						warnings = append(warnings, fmt.Sprintf("policy %q combines task/workspace reads with non-numeric regex command arg %s.%s (prefer enum or numeric regex to minimize covert channel bandwidth)", pol.ID, cid, a.Name))
					}
				}
			}
		}

		// Validate nudges
		seenNudges := make(map[string]bool)
		for _, n := range pol.Grants.Nudges {
			if !reValidSlugID.MatchString(n.ID) {
				return nil, fmt.Errorf("policy %q invalid nudge id %q", pol.ID, n.ID)
			}
			if seenNudges[n.ID] {
				return nil, fmt.Errorf("policy %q duplicate nudge id %q", pol.ID, n.ID)
			}
			seenNudges[n.ID] = true
			dm := strings.TrimSpace(n.DeliveryMode)
			switch dm {
			case "queue_only":
				if strings.TrimSpace(n.TargetTaskSlug) == "" && strings.TrimSpace(n.TargetTaskScope) == "" {
					return nil, fmt.Errorf("policy %q nudge %q (queue_only) requires target_task_slug or target_task_scope", pol.ID, n.ID)
				}
			case "spawn_sandboxed_worker":
				if ws := strings.TrimSpace(n.WorkerSkill); ws != "" && !reValidSlugID.MatchString(ws) {
					return nil, fmt.Errorf("policy %q nudge %q invalid worker_skill %q", pol.ID, n.ID, n.WorkerSkill)
				}
				switch strings.ToLower(strings.TrimSpace(n.WorkerTier)) {
				case "", "quick", "routine", "standard", "heavy", "deep":
				default:
					return nil, fmt.Errorf("policy %q nudge %q invalid worker_tier %q (expected quick, routine, standard, heavy, or deep)", pol.ID, n.ID, n.WorkerTier)
				}
				if strings.TrimSpace(n.PreflightCommand) == "" {
					warnings = append(warnings, fmt.Sprintf("policy %q nudge %q uses spawn_sandboxed_worker without preflight_command (recommended to verify forwarded objects before spawning background workers)", pol.ID, n.ID))
				}
			default:
				return nil, fmt.Errorf("policy %q nudge %q delivery_mode=%q unsupported (expected \"queue_only\" or \"spawn_sandboxed_worker\")", pol.ID, n.ID, n.DeliveryMode)
			}
			paramSet := make(map[string]bool)
			for _, p := range n.Params {
				if paramSet[p.Name] {
					return nil, fmt.Errorf("policy %q nudge %q duplicate param %q", pol.ID, n.ID, p.Name)
				}
				paramSet[p.Name] = true
				if err := validateArgSpec("policy."+pol.ID+".nudge."+n.ID, p); err != nil {
					return nil, err
				}
			}
			if n.PreflightCommand != "" {
				if _, ok := cfg.Commands[n.PreflightCommand]; !ok {
					return nil, fmt.Errorf("policy %q nudge %q references unknown preflight_command %q", pol.ID, n.ID, n.PreflightCommand)
				}
			}
			if n.OnPreflightFail != "" && n.OnPreflightFail != "escalate" && n.OnPreflightFail != "reply_uniform" {
				return nil, fmt.Errorf("policy %q nudge %q invalid on_preflight_fail %q (expected escalate or reply_uniform)", pol.ID, n.ID, n.OnPreflightFail)
			}
			if strings.TrimSpace(string(n.Template)) == "" {
				return nil, fmt.Errorf("policy %q nudge %q requires non-empty static template", pol.ID, n.ID)
			}
			if err := ValidateSafeTemplate("nudge.template", string(n.Template), paramSet); err != nil {
				return nil, fmt.Errorf("policy %q nudge %q: %w", pol.ID, n.ID, err)
			}
			if err := ValidateSafeTemplate("nudge.ack_reply_template", string(n.AckReplyTemplate), paramSet); err != nil {
				return nil, fmt.Errorf("policy %q nudge %q: %w", pol.ID, n.ID, err)
			}
			if err := ValidateSafeTemplate("nudge.preflight_fail_reply_template", string(n.PreflightFailReplyTemplate), map[string]bool{}); err != nil {
				return nil, fmt.Errorf("policy %q nudge %q: %w", pol.ID, n.ID, err)
			}
		}

		// Validate egress
		switch pol.Egress.Mode {
		case "none", "escalate", "stage_approval", "auto_reply", "structured_template":
		default:
			return nil, fmt.Errorf("policy %q invalid egress.mode %q", pol.ID, pol.Egress.Mode)
		}
		if pol.Egress.FailMode != "" && pol.Egress.FailMode != "stage_approval" && pol.Egress.FailMode != "escalate" {
			return nil, fmt.Errorf("policy %q invalid egress.fail_mode %q", pol.ID, pol.Egress.FailMode)
		}
		for _, pat := range pol.Egress.BlockedPatterns {
			if _, err := regexp.Compile(pat); err != nil {
				return nil, fmt.Errorf("policy %q egress.blocked_patterns invalid regex %q: %w", pol.ID, pat, err)
			}
		}
		if pol.Egress.Mode == "structured_template" {
			if strings.TrimSpace(string(pol.Egress.StructuredTemplate)) == "" {
				return nil, fmt.Errorf("policy %q structured_template mode requires egress.structured_template", pol.ID)
			}
			fieldSet := make(map[string]bool)
			for _, f := range pol.Egress.StructuredFields {
				if fieldSet[f.Name] {
					return nil, fmt.Errorf("policy %q duplicate structured_fields %q", pol.ID, f.Name)
				}
				fieldSet[f.Name] = true
				if err := validateArgSpec("policy."+pol.ID+".egress", f); err != nil {
					return nil, err
				}
			}
			if err := ValidateSafeTemplate("egress.structured_template", string(pol.Egress.StructuredTemplate), fieldSet); err != nil {
				return nil, fmt.Errorf("policy %q: %w", pol.ID, err)
			}
		}
		if err := ValidateSafeTemplate("egress.escalated_ack_template", string(pol.Egress.EscalatedAckTemplate), map[string]bool{}); err != nil {
			return nil, fmt.Errorf("policy %q: %w", pol.ID, err)
		}

		// Compile and validate lattice invariants NI-1..NI-4 via internal/authz!
		sampleGrant := CompilePolicyGrant(pol, "validate-grant", "validate@example.com", "validate-req", time.Now().UTC())
		compiledPol := authz.BuildScopedDelegate(taint.Record{
			SchemaVersion: taint.SchemaVersion,
			Source:        "validate:" + pol.ID,
			Grant:         &sampleGrant,
		})
		if err := authz.ValidateProfile(compiledPol); err != nil {
			return nil, fmt.Errorf("policy %q violates authz lattice invariant: %w", pol.ID, err)
		}
	}

	// Validate snapshots
	seenSnaps := make(map[string]bool)
	for _, snap := range cfg.Snapshots {
		if !reValidSlugID.MatchString(snap.ID) {
			return nil, fmt.Errorf("snapshots invalid id %q", snap.ID)
		}
		if seenSnaps[snap.ID] {
			return nil, fmt.Errorf("duplicate snapshot id %q", snap.ID)
		}
		seenSnaps[snap.ID] = true
		if !reValidSourceSpec.MatchString(strings.TrimSpace(snap.Source)) {
			return nil, fmt.Errorf("snapshot %q invalid source %q", snap.ID, snap.Source)
		}
		if snap.IntervalMinutes < 5 {
			return nil, fmt.Errorf("snapshot %q interval_minutes=%d must be >= 5", snap.ID, snap.IntervalMinutes)
		}
		for _, slug := range snap.Tasks {
			if strings.ContainsAny(slug, "*?[") || !reValidTaskGrant.MatchString(slug) {
				return nil, fmt.Errorf("snapshot %q invalid task %q", snap.ID, slug)
			}
		}
		for _, ws := range snap.Workspaces {
			if strings.ContainsAny(ws, "*?[") || !reValidWSGrant.MatchString(ws) {
				return nil, fmt.Errorf("snapshot %q invalid workspace %q", snap.ID, ws)
			}
		}
		for _, sc := range snap.Commands {
			cspec, ok := cfg.Commands[sc.Command]
			if !ok {
				return nil, fmt.Errorf("snapshot %q references unknown command %q", snap.ID, sc.Command)
			}
			for _, a := range cspec.Args {
				if _, err := ValidateTypedArgValue(a, sc.FixedArgs[a.Name]); err != nil {
					return nil, fmt.Errorf("snapshot %q command %q fixed_args: %w", snap.ID, sc.Command, err)
				}
			}
		}
	}

	sort.Strings(warnings)
	return warnings, nil
}
