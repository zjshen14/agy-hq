package approvals

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	ModeAlways         = "always"
	ModeThirdPartyOnly = "third_party_only"
	ModeAutoOnly       = "auto_only"
	ModeNever          = "never"

	ResolverNone      = ""
	ResolverGitHubPR  = "github_pr"
	ResolverGitRemote = "git_remote"

	// InteractivePreset controls approval friction strictly for interactive owner
	// sessions (top of lattice). Unattended [Auto], subagents, and tainted sessions
	// always enforce full fail-closed rules regardless of interactive_preset.
	InteractivePresetStrict      = "strict"
	InteractivePresetBalanced    = "balanced"
	InteractivePresetLowFriction = "low_friction"

	// InteractiveGateMode controls how gated commands in interactive owner sessions
	// are prompted: "propose" stages a cryptographic hq propose item, whereas
	// "native_force_ask" emits a single native PreToolUse force_ask prompt for
	// non-Ring-0 rules (rsi_apply always stays on propose).
	InteractiveGatePropose        = "propose"
	InteractiveGateNativeForceAsk = "native_force_ask"
)

var (
	reValidRuleID        = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	reGitHubURLRepo      = regexp.MustCompile(`(?i)https?://(?:www\.)?(?:github\.com|gitlab\.com|bitbucket\.org)/([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+?)(?:\.git|/|$|\s|["'])`)
	reGHRepoFlag         = regexp.MustCompile(`(?:^|\s)(?:-R|--repo)(?:=|\s+)["']?([A-Za-z0-9_.-]+(?:/[A-Za-z0-9_.-]+){1,2})["']?(?:\s|$)`)
	reGitPushCmd         = regexp.MustCompile(`(?i)\bgit(?:\s+(?:-[Cc]\s+(?:"[^"]+"|'[^']+'|\S+)|--(?:git-dir|work-tree|namespace|super-prefix|config-env)\s+(?:"[^"]+"|'[^']+'|\S+)|-(?:[Cc]\S+|\w)|--[^\s]+))*\s+push\b`)
	reGitDashC           = regexp.MustCompile(`(?:^|\s)-C(?:\s+(?:"([^"]+)"|'([^']+)'|(\S+))|(\S+))`)
	reUnsafeGitEnvAssign = regexp.MustCompile(`(?i)^(?:GIT_DIR|GIT_WORK_TREE|GIT_COMMON_DIR|GIT_CONFIG|GIT_CONFIG_GLOBAL|GIT_CONFIG_SYSTEM|GIT_CONFIG_PARAMETERS|GIT_CONFIG_COUNT|GIT_SSH|GIT_SSH_COMMAND|GIT_EXEC_PATH|GH_REPO|GH_HOST|GITHUB_HOST)=`)
	reCurlMutFlagPol     = regexp.MustCompile(`(?i)(?:^|\s)(?:-X\s*(?:POST|PUT|PATCH|DELETE)\b|--request(?:=|\s+)(?:POST|PUT|PATCH|DELETE)\b|(?:-d|--data|--data-raw|--data-binary|--data-urlencode|-F|--form|--json|--upload-file|-T)(?:=|\s|$))`)
	reWgetMutFlagPol     = regexp.MustCompile(`(?i)(?:^|\s)(?:--post-data|--post-file|--method(?:=|\s+)(?:POST|PUT|PATCH|DELETE)\b)`)
	reLoopbackOnlyPol    = regexp.MustCompile(`(?i)https?://(?:127\.0\.0\.1|localhost|\[::1\])(?::\d+)?(?:/|\s|"|'|$)`)
	reExtURLHostPol      = regexp.MustCompile(`(?i)https?://([^\s/"':]+)`)
)

// CoreNonDisableablePackRules lists core security rule IDs that companion extension packs
// may tighten or extend, but may never weaken below their built-in mode (only user <vault>/config.yaml may override).
var CoreNonDisableablePackRules = map[string]bool{
	"messaging":            true,
	"release_publish":      true,
	"rsi_apply":            true,
	"workspace_remove":     true,
	"workspace_own":        true,
	"workspace_auto_guard": true,
}

// Rule defines a single uniform outbound approval category in config.yaml or hq-extension.json.
type Rule struct {
	ID             string   `json:"id"`
	Description    string   `json:"description,omitempty"`
	Mode           string   `json:"mode"`
	TargetResolver string   `json:"target_resolver,omitempty"`
	Commands       []string `json:"commands"`
	AutoCategories []string `json:"auto_categories,omitempty"`
	Source         string   `json:"-"`
	compiled       []*regexp.Regexp
}

// ExtensionPackRules carries approval policy contributions from an enabled & trusted companion pack.
type ExtensionPackRules struct {
	PackID     string
	OwnedRepos []string
	Rules      []Rule
}

// RegisteredExtensionApprovalRulesProvider is wired by internal/extension at init time
// to avoid circular package imports between internal/approvals and internal/extension.
var RegisteredExtensionApprovalRulesProvider func(vaultDir string) []ExtensionPackRules

// PolicyConfig represents the resolved outbound approval policy for a Vault.
type PolicyConfig struct {
	OwnedRepos          []string
	AskTTL              time.Duration
	ProposalTTL         time.Duration
	RejectNoopCommands  bool
	InteractivePreset   string
	InteractiveGateMode string
	HostPresetBridge    bool
	Rules               []Rule
}

// EvalDecision describes the outcome of evaluating a shell command against PolicyConfig.
type EvalDecision struct {
	Gated      bool
	RuleID     string
	Mode       string
	TargetRepo string
	Reason     string
}

// DefaultRules returns the built-in outbound approval rule table (RFC-0021 §4.1).
func DefaultRules() []Rule {
	rules := []Rule{
		{
			ID:          "messaging",
			Description: "Outbound email, chat, and direct external messaging",
			Mode:        ModeAlways,
			Commands: []string{
				`\bgws\s+(gmail|chat)\s+(send|create|post|patch|update|delete)\b`,
				`(?:^|[;&|]\s*)(?:[A-Za-z_][A-Za-z0-9_]*=\S+\s+)*(?:["']?(?:\S+/)?language_server["']?\s+plugin-cli\s+)?["']?(?:\S+/)?gmail(?:-[0-9]+)?["']?\s+[^;&|\n]*\b(?:mutate\s+(?:send-message|send-draft|send-with-attachment|send|reply|forward)|send-message|send-draft)\b`,
				`\bhq\s+gmail\s+(?:send|reply|forward|send-draft)\b`,
				`\bhq\s+gchat\s+(?:send|reply|forward|send-draft)\b`,
				`\bchat\.googleapis\.com/v1/spaces/[^\s]*messages\b`,
			},
			AutoCategories: []string{"external_comms"},
			Source:         "builtin",
		},
		{
			ID:          "release_publish",
			Description: "Public releases, package registries, and production deployments",
			Mode:        ModeAlways,
			Commands: []string{
				`\b(?:gh\s+release\s+create|npm\s+publish|pnpm\s+publish|yarn\s+publish|cargo\s+publish|twine\s+upload|gem\s+push|docker\s+push|helm\s+push|vercel\s+(?:--prod|deploy\s+--prod)|fly\s+deploy|netlify\s+deploy\s+--prod)\b`,
			},
			AutoCategories: []string{"pkg_publish", "prod_deploy"},
			Source:         "builtin",
		},
		{
			ID:             "pr_merge",
			Description:    "Pull request merges (third-party always gated; owned repos auto-only)",
			Mode:           ModeThirdPartyOnly,
			TargetResolver: ResolverGitHubPR,
			Commands: []string{
				`\bgh\s+pr\s+merge\b`,
			},
			AutoCategories: []string{"forge_mutate"},
			Source:         "builtin",
		},
		{
			ID:             "forge_git",
			Description:    "Git pushes/tags and owned-repo issue/PR management",
			Mode:           ModeAutoOnly,
			TargetResolver: ResolverGitRemote,
			Commands: []string{
				`\bgit(?:\s+(?:-[Cc]\s+(?:"[^"]+"|'[^']+'|\S+)|--(?:git-dir|work-tree|namespace|super-prefix|config-env)\s+(?:"[^"]+"|'[^']+'|\S+)|-(?:[Cc]\S+|\w)|--[^\s]+))*\s+push\b`,
				`\bgh\s+(?:(?:pr|issue)\s+(?:create|close|reopen|comment|edit|ready|review)|repo\s+(?:delete|rename)|api\b[^\n]*(?:-X\s*(?:POST|PUT|PATCH|DELETE)|--method(?:=|\s+)(?:POST|PUT|PATCH|DELETE)|(?:\s|^)-[fF]\s|--field\b|--raw-field\b))`,
				`\b(?:terraform\s+(?:apply|destroy)|kubectl\s+(?:apply|delete|patch))\b`,
			},
			AutoCategories: []string{"forge_mutate"},
			Source:         "builtin",
		},
		{
			ID:          "rsi_apply",
			Description: "Ring-0 self-modification, trust pins, & local console approval API",
			Mode:        ModeAlways,
			Commands: []string{
				`\bhq\s+(?:rsi\s+apply|connector\s+apply-offer|extension\s+apply-trust|channel\s+apply-config|delegate\s+(?:apply-policy|apply-reply))\b`,
				`(?:https?://[^\s"']*|127\.0\.0\.1:\d+|localhost:\d+)/api/(?:approve|reject)\b`,
			},
			Source: "builtin",
		},
		{
			// RFC-0025 §10.1 / §16.9: `hq workspace remove <key> --force` deletes an hq-owned provider workspace even
			// when it is dirty or holds open changes. It runs only through `hq propose` + human `hq approve`.
			ID:          "workspace_remove",
			Description: "Force-removing an hq-owned workspace that may hold uncommitted work or open changes",
			Mode:        ModeAlways,
			Commands: []string{
				`(?:\bhq\S*|\bcmd/hq/?)\s+workspace\s+remove\b[^;&|\n]*?[\s"']-{1,}force\b`,
				`\bexec\s+-a\s+\S+[^;&|\n]*\bworkspace\s+remove\b[^;&|\n]*?[\s"']-{1,}force\b`,
			},
			Source: "builtin",
		},
		{
			// RFC-0025 §7.1: promoting an attached workspace to owned makes it removable by hq.
			ID:          "workspace_own",
			Description: "Promoting an attached workspace to hq-owned (makes it removable by hq)",
			Mode:        ModeAlways,
			Commands: []string{
				`(?:\bhq\S*|\bcmd/hq/?)\s+workspace\s+own\b`,
				`\bexec\s+-a\s+\S+[^;&|\n]*\bworkspace\s+own\b`,
			},
			Source: "builtin",
		},
		{
			// RFC-0025 §5.3: unattended [Auto] workers may never create, remove, prune, spawn --workspace,
			// or archive workspaces directly (enforced in PreToolUse so env -u ANTIGRAVITY_CONVERSATION_ID cannot bypass).
			ID:          "workspace_auto_guard",
			Description: "Unattended [Auto] workspace mutations (create, remove, prune, spawn --workspace, archive)",
			Mode:        ModeAutoOnly,
			Commands: []string{
				`(?:\bhq\S*|\bcmd/hq/?)\s+workspace\s+(?:create|remove|own|prune)\b`,
				`(?:\bhq\S*|\bcmd/hq/?)\s+(?:spawn|subagent)\b[^;&|\n]*?[\s"']-{1,}workspace(?:=|\s|$)`,
				`(?:\bhq\S*|\bcmd/hq/?)\s+archive\b`,
				`\bexec\s+-a\s+\S+[^;&|\n]*\b(?:workspace\s+(?:create|remove|own|prune)|archive)\b`,
			},
			AutoCategories: []string{"workspace_mutate"},
			Source:         "builtin",
		},
	}
	for i := range rules {
		_ = rules[i].compile()
	}
	return rules
}

// DefaultPolicyConfig returns the compile-time default PolicyConfig.
func DefaultPolicyConfig() PolicyConfig {
	return PolicyConfig{
		OwnedRepos:          nil,
		AskTTL:              DefaultAskTTL,
		ProposalTTL:         DefaultProposalTTL,
		RejectNoopCommands:  true,
		InteractivePreset:   InteractivePresetBalanced,
		InteractiveGateMode: InteractiveGatePropose,
		Rules:               DefaultRules(),
	}
}

func isValidInteractivePreset(p string) bool {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case InteractivePresetStrict, InteractivePresetBalanced, InteractivePresetLowFriction:
		return true
	default:
		return false
	}
}

func isValidInteractiveGateMode(m string) bool {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case InteractiveGatePropose, InteractiveGateNativeForceAsk:
		return true
	default:
		return false
	}
}

func isValidMode(m string) bool {
	switch strings.ToLower(strings.TrimSpace(m)) {
	case ModeAlways, ModeThirdPartyOnly, ModeAutoOnly, ModeNever:
		return true
	default:
		return false
	}
}

func isValidTargetResolver(r string) bool {
	switch strings.ToLower(strings.TrimSpace(r)) {
	case ResolverNone, ResolverGitHubPR, ResolverGitRemote:
		return true
	default:
		return false
	}
}

func (r *Rule) compile() error {
	r.ID = strings.TrimSpace(r.ID)
	r.Mode = strings.ToLower(strings.TrimSpace(r.Mode))
	r.TargetResolver = strings.ToLower(strings.TrimSpace(r.TargetResolver))
	r.compiled = nil
	for _, pat := range r.Commands {
		p := strings.TrimSpace(pat)
		if p == "" {
			continue
		}
		if !strings.HasPrefix(p, "(?i)") {
			p = "(?i)" + p
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return fmt.Errorf("rule %q invalid command regex %q: %w", r.ID, pat, err)
		}
		r.compiled = append(r.compiled, re)
	}
	return nil
}

// ValidateRule checks schema & regex validity for a single Rule.
// allowEmptyCommands permits user config overrides that only change `mode:` on a built-in rule ID.
func ValidateRule(r Rule, allowEmptyCommands bool) error {
	id := strings.TrimSpace(r.ID)
	if !reValidRuleID.MatchString(id) {
		return fmt.Errorf("invalid approval rule id %q (expected lowercase alphanumeric, '-' or '_')", r.ID)
	}
	if !isValidMode(r.Mode) {
		return fmt.Errorf("rule %q invalid mode %q (expected always, third_party_only, auto_only, or never)", id, r.Mode)
	}
	if !isValidTargetResolver(r.TargetResolver) {
		return fmt.Errorf("rule %q invalid target_resolver %q (expected github_pr or git_remote)", id, r.TargetResolver)
	}
	if !allowEmptyCommands && len(r.Commands) == 0 && strings.ToLower(strings.TrimSpace(r.Mode)) != ModeNever {
		return fmt.Errorf("rule %q requires at least one regex in commands", id)
	}
	tmp := r
	return tmp.compile()
}

type parsedVaultApprovals struct {
	Present                bool
	HasOwnedRepos          bool
	OwnedRepos             []string
	AskTTLRaw              string
	ProposalTTLRaw         string
	HasRejectNoopCommands  bool
	RejectNoopCommands     bool
	InteractivePresetRaw   string
	InteractiveGateModeRaw string
	HasHostPresetBridge    bool
	HostPresetBridge       bool
	Rules                  []Rule
}

// stripYAMLCommentQuoteAware strips trailing `# ...` comments outside single/double quotes.
func stripYAMLCommentQuoteAware(s string) string {
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				if i+1 < len(s) && s[i+1] == '\'' {
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case '#':
			if i == 0 || s[i-1] == ' ' || s[i-1] == '\t' {
				return strings.TrimRight(s[:i], " \t")
			}
		}
	}
	return strings.TrimSpace(s)
}

func unquoteYAMLScalar(raw string) string {
	s := strings.TrimSpace(raw)
	if len(s) >= 2 && s[0] == '\'' && s[len(s)-1] == '\'' {
		return strings.ReplaceAll(s[1:len(s)-1], "''", "'")
	}
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		inner := s[1 : len(s)-1]
		var b strings.Builder
		for i := 0; i < len(inner); i++ {
			if inner[i] == '\\' && i+1 < len(inner) {
				i++
				switch inner[i] {
				case 'n':
					b.WriteByte('\n')
				case 't':
					b.WriteByte('\t')
				case 'r':
					b.WriteByte('\r')
				case '"', '\\', '/':
					b.WriteByte(inner[i])
				default:
					// Preserve regex escapes such as \b, \s, \d if someone wrote them inside double quotes
					b.WriteByte('\\')
					b.WriteByte(inner[i])
				}
				continue
			}
			b.WriteByte(inner[i])
		}
		return b.String()
	}
	return s
}

func parseYAMLInlineBracketList(raw string) ([]string, bool) {
	s := strings.TrimSpace(raw)
	if !strings.HasPrefix(s, "[") || !strings.HasSuffix(s, "]") {
		return nil, false
	}
	inner := strings.TrimSpace(s[1 : len(s)-1])
	if inner == "" {
		return []string{}, true
	}
	var out []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(inner); i++ {
		ch := inner[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if inSingle {
			cur.WriteByte(ch)
			if ch == '\'' {
				if i+1 < len(inner) && inner[i+1] == '\'' {
					cur.WriteByte('\'')
					i++
					continue
				}
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				cur.WriteByte(ch)
				escaped = true
				continue
			}
			cur.WriteByte(ch)
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
			cur.WriteByte(ch)
		case '"':
			inDouble = true
			cur.WriteByte(ch)
		case ',':
			item := unquoteYAMLScalar(cur.String())
			if item != "" {
				out = append(out, item)
			}
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if last := unquoteYAMLScalar(cur.String()); last != "" {
		out = append(out, last)
	}
	return out, true
}

func splitYAMLKeyValueQuoteAware(line string) (string, string, bool) {
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(line); i++ {
		ch := line[i]
		if escaped {
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
				continue
			}
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\'':
			inSingle = true
		case '"':
			inDouble = true
		case ':':
			if i+1 == len(line) || line[i+1] == ' ' || line[i+1] == '\t' {
				return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
			}
		}
	}
	return "", "", false
}

func parseApprovalsConfigYAML(raw string) (parsedVaultApprovals, error) {
	var out parsedVaultApprovals
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")

	inApprovals := false
	sectionMode := ""   // "", "owned_repos", "rules"
	ruleListField := "" // "", "commands", "auto_categories"
	ruleIndent := -1
	var curRule *Rule

	flushRule := func() {
		if curRule != nil {
			out.Rules = append(out.Rules, *curRule)
			curRule = nil
		}
		ruleListField = ""
	}

	applyRuleKV := func(r *Rule, key, val string) error {
		ruleListField = ""
		switch key {
		case "id":
			r.ID = unquoteYAMLScalar(val)
		case "description":
			r.Description = unquoteYAMLScalar(val)
		case "mode":
			r.Mode = unquoteYAMLScalar(val)
		case "target_resolver":
			r.TargetResolver = unquoteYAMLScalar(val)
		case "commands":
			if val == "" {
				ruleListField = "commands"
			} else if arr, ok := parseYAMLInlineBracketList(val); ok {
				r.Commands = append(r.Commands, arr...)
			} else {
				return fmt.Errorf("approvals.rules[%q].commands must be a YAML list", r.ID)
			}
		case "auto_categories":
			if val == "" {
				ruleListField = "auto_categories"
			} else if arr, ok := parseYAMLInlineBracketList(val); ok {
				r.AutoCategories = append(r.AutoCategories, arr...)
			} else {
				return fmt.Errorf("approvals.rules[%q].auto_categories must be a YAML list", r.ID)
			}
		default:
			return fmt.Errorf("unknown field %q in approvals.rules", key)
		}
		return nil
	}

	for lineIdx, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " \t"))
		clean := stripYAMLCommentQuoteAware(trimmed)
		if clean == "" {
			continue
		}

		if indent == 0 {
			flushRule()
			sectionMode = ""
			if clean == "approvals:" {
				inApprovals = true
				out.Present = true
			} else {
				inApprovals = false
			}
			continue
		}
		if !inApprovals {
			continue
		}

		// Direct children of `approvals:` (typical indent == 2) when not inside a deeper list
		if sectionMode == "owned_repos" && strings.HasPrefix(clean, "- ") {
			item := unquoteYAMLScalar(strings.TrimSpace(strings.TrimPrefix(clean, "- ")))
			if item != "" {
				out.OwnedRepos = append(out.OwnedRepos, item)
			}
			continue
		}

		if sectionMode == "rules" {
			if strings.HasPrefix(clean, "- ") {
				rest := strings.TrimSpace(strings.TrimPrefix(clean, "- "))
				if curRule != nil && ruleListField != "" && (ruleIndent < 0 || indent > ruleIndent) {
					if _, _, isKV := splitYAMLKeyValueQuoteAware(rest); !isKV {
						val := unquoteYAMLScalar(rest)
						if val != "" {
							if ruleListField == "commands" {
								curRule.Commands = append(curRule.Commands, val)
							} else if ruleListField == "auto_categories" {
								curRule.AutoCategories = append(curRule.AutoCategories, val)
							}
						}
						continue
					}
				}
				flushRule()
				ruleIndent = indent
				curRule = &Rule{Source: "vault"}
				if k, v, ok := splitYAMLKeyValueQuoteAware(rest); ok {
					if err := applyRuleKV(curRule, k, v); err != nil {
						return out, fmt.Errorf("config.yaml:%d: %w", lineIdx+1, err)
					}
				} else {
					return out, fmt.Errorf("config.yaml:%d: expected rule mapping entry under approvals.rules", lineIdx+1)
				}
				continue
			}

			if curRule != nil && (ruleIndent < 0 || indent > ruleIndent) {
				if k, v, ok := splitYAMLKeyValueQuoteAware(clean); ok {
					if err := applyRuleKV(curRule, k, v); err != nil {
						return out, fmt.Errorf("config.yaml:%d: %w", lineIdx+1, err)
					}
					continue
				}
			}
		}

		// Top-level keys inside `approvals:`
		if k, v, ok := splitYAMLKeyValueQuoteAware(clean); ok {
			flushRule()
			sectionMode = ""
			ruleIndent = -1
			switch k {
			case "owned_repos":
				out.HasOwnedRepos = true
				if v == "" {
					sectionMode = "owned_repos"
				} else if arr, isArr := parseYAMLInlineBracketList(v); isArr {
					out.OwnedRepos = append([]string(nil), arr...)
				} else {
					return out, fmt.Errorf("config.yaml:%d: approvals.owned_repos must be a YAML list", lineIdx+1)
				}
			case "ask_ttl":
				out.AskTTLRaw = unquoteYAMLScalar(v)
			case "proposal_ttl":
				out.ProposalTTLRaw = unquoteYAMLScalar(v)
			case "interactive_preset":
				out.InteractivePresetRaw = strings.ToLower(strings.TrimSpace(unquoteYAMLScalar(v)))
			case "interactive_gate_mode":
				out.InteractiveGateModeRaw = strings.ToLower(strings.TrimSpace(unquoteYAMLScalar(v)))
			case "host_preset_bridge":
				uv := strings.ToLower(unquoteYAMLScalar(v))
				switch uv {
				case "true", "yes", "1":
					out.HasHostPresetBridge = true
					out.HostPresetBridge = true
				case "false", "no", "0":
					out.HasHostPresetBridge = true
					out.HostPresetBridge = false
				default:
					return out, fmt.Errorf("config.yaml:%d: invalid boolean %q for %s", lineIdx+1, v, k)
				}
			case "reject_noop_commands", "reject_echo_guard":
				uv := strings.ToLower(unquoteYAMLScalar(v))
				switch uv {
				case "true", "yes", "1":
					out.HasRejectNoopCommands = true
					out.RejectNoopCommands = true
				case "false", "no", "0":
					out.HasRejectNoopCommands = true
					out.RejectNoopCommands = false
				default:
					return out, fmt.Errorf("config.yaml:%d: invalid boolean %q for %s", lineIdx+1, v, k)
				}
			case "rules":
				if v == "" {
					sectionMode = "rules"
				} else if arr, isArr := parseYAMLInlineBracketList(v); isArr && len(arr) == 0 {
					// `rules: []` leaves built-in rules untouched unless overridden
				} else {
					return out, fmt.Errorf("config.yaml:%d: approvals.rules must be a list of rule objects", lineIdx+1)
				}
			default:
				return out, fmt.Errorf("config.yaml:%d: unknown key %q in approvals section", lineIdx+1, k)
			}
		}
	}
	flushRule()
	return out, nil
}

// ValidateConfigYAML validates the `approvals:` section of a raw config.yaml document before persistence.
func ValidateConfigYAML(raw string) ([]string, error) {
	_, warnings, err := buildPolicyFromSources(nil, raw)
	return warnings, err
}

func mergeRuleIntoSlice(rules []Rule, incoming Rule, source string) ([]Rule, error) {
	incoming.ID = strings.TrimSpace(incoming.ID)
	incoming.Source = source
	idx := -1
	for i := range rules {
		if rules[i].ID == incoming.ID {
			idx = i
			break
		}
	}
	if idx >= 0 {
		merged := rules[idx]
		merged.Source = source
		if strings.TrimSpace(incoming.Description) != "" {
			merged.Description = strings.TrimSpace(incoming.Description)
		}
		if strings.TrimSpace(incoming.Mode) != "" {
			merged.Mode = strings.ToLower(strings.TrimSpace(incoming.Mode))
		}
		if strings.TrimSpace(incoming.TargetResolver) != "" {
			merged.TargetResolver = strings.ToLower(strings.TrimSpace(incoming.TargetResolver))
		}
		if len(incoming.Commands) > 0 {
			if CoreNonDisableablePackRules[merged.ID] {
				// Preserve built-in Ring-0 command patterns across upgrades even when pre-existing Vault config.yaml lists older patterns.
				merged.Commands = appendUniqueStrings(append([]string(nil), merged.Commands...), incoming.Commands...)
			} else {
				merged.Commands = append([]string(nil), incoming.Commands...)
			}
		}
		if len(incoming.AutoCategories) > 0 {
			merged.AutoCategories = append([]string(nil), incoming.AutoCategories...)
		}
		if err := ValidateRule(merged, false); err != nil {
			return nil, err
		}
		if err := merged.compile(); err != nil {
			return nil, err
		}
		rules[idx] = merged
		return rules, nil
	}

	if err := ValidateRule(incoming, false); err != nil {
		return nil, err
	}
	if err := incoming.compile(); err != nil {
		return nil, err
	}
	return append(rules, incoming), nil
}

func appendUniqueStrings(dst []string, items ...string) []string {
	seen := make(map[string]bool, len(dst))
	for _, d := range dst {
		seen[strings.ToLower(strings.TrimSpace(d))] = true
	}
	for _, it := range items {
		clean := strings.TrimSpace(it)
		key := strings.ToLower(clean)
		if clean != "" && !seen[key] {
			seen[key] = true
			dst = append(dst, clean)
		}
	}
	return dst
}

func buildPolicyFromSources(packs []ExtensionPackRules, rawConfigYAML string) (PolicyConfig, []string, error) {
	pol := DefaultPolicyConfig()
	var warnings []string

	// 1. Merge enabled & trusted companion extension packs
	for _, pk := range packs {
		pol.OwnedRepos = appendUniqueStrings(pol.OwnedRepos, pk.OwnedRepos...)
		for _, r := range pk.Rules {
			modeLower := strings.ToLower(strings.TrimSpace(r.Mode))
			if CoreNonDisableablePackRules[strings.TrimSpace(r.ID)] && modeLower != "" && modeLower != ModeAlways {
				warnings = append(warnings, fmt.Sprintf("pack %q attempted to weaken core rule %q to %q (ignored; configure Vault config.yaml instead)", pk.PackID, r.ID, r.Mode))
				continue
			}
			merged, err := mergeRuleIntoSlice(pol.Rules, r, "pack:"+pk.PackID)
			if err != nil {
				warnings = append(warnings, fmt.Sprintf("pack %q invalid approval rule %q: %v", pk.PackID, r.ID, err))
				continue
			}
			pol.Rules = merged
		}
	}

	// 2. Merge user <vault>/config.yaml (highest precedence)
	if strings.TrimSpace(rawConfigYAML) != "" {
		parsed, err := parseApprovalsConfigYAML(rawConfigYAML)
		if err != nil {
			return pol, warnings, err
		}
		if parsed.Present {
			if parsed.HasOwnedRepos {
				pol.OwnedRepos = nil
				pol.OwnedRepos = appendUniqueStrings(pol.OwnedRepos, parsed.OwnedRepos...)
			}
			if parsed.AskTTLRaw != "" {
				d, dErr := time.ParseDuration(parsed.AskTTLRaw)
				if dErr != nil || d <= 0 {
					return pol, warnings, fmt.Errorf("invalid approvals.ask_ttl %q (expected positive Go duration like 24h)", parsed.AskTTLRaw)
				}
				pol.AskTTL = d
			}
			if parsed.ProposalTTLRaw != "" {
				d, dErr := time.ParseDuration(parsed.ProposalTTLRaw)
				if dErr != nil || d <= 0 {
					return pol, warnings, fmt.Errorf("invalid approvals.proposal_ttl %q (expected positive Go duration like 24h)", parsed.ProposalTTLRaw)
				}
				pol.ProposalTTL = d
			}
			if parsed.HasRejectNoopCommands {
				pol.RejectNoopCommands = parsed.RejectNoopCommands
			}
			if parsed.InteractivePresetRaw != "" {
				if !isValidInteractivePreset(parsed.InteractivePresetRaw) {
					return pol, warnings, fmt.Errorf("invalid approvals.interactive_preset %q (expected strict, balanced, or low_friction)", parsed.InteractivePresetRaw)
				}
				pol.InteractivePreset = parsed.InteractivePresetRaw
			}
			if parsed.InteractiveGateModeRaw != "" {
				if !isValidInteractiveGateMode(parsed.InteractiveGateModeRaw) {
					return pol, warnings, fmt.Errorf("invalid approvals.interactive_gate_mode %q (expected propose or native_force_ask)", parsed.InteractiveGateModeRaw)
				}
				pol.InteractiveGateMode = parsed.InteractiveGateModeRaw
			}
			if parsed.HasHostPresetBridge {
				pol.HostPresetBridge = parsed.HostPresetBridge
			}

			seenVaultIDs := make(map[string]bool)
			for _, r := range parsed.Rules {
				rid := strings.TrimSpace(r.ID)
				if seenVaultIDs[rid] {
					return pol, warnings, fmt.Errorf("duplicate rule id %q in approvals.rules", rid)
				}
				seenVaultIDs[rid] = true
				merged, mErr := mergeRuleIntoSlice(pol.Rules, r, "vault")
				if mErr != nil {
					return pol, warnings, mErr
				}
				pol.Rules = merged
			}
		}
	}

	hasThirdPartyRule := false
	for _, r := range pol.Rules {
		if r.Mode == ModeThirdPartyOnly {
			hasThirdPartyRule = true
		}
		if CoreNonDisableablePackRules[r.ID] && r.Mode == ModeNever {
			warnings = append(warnings, fmt.Sprintf("core security rule %q is set to mode=never in config.yaml", r.ID))
		}
	}
	if hasThirdPartyRule && len(pol.OwnedRepos) == 0 {
		warnings = append(warnings, "third_party_only rule active with empty approvals.owned_repos (all matching repos fail-closed to gated until owned_repos is configured)")
	}
	return pol, warnings, nil
}

// LoadPolicy resolves the active PolicyConfig for vaultDir (Built-ins -> Trusted Packs -> Vault config.yaml).
// On corrupt Vault YAML, LoadPolicy fails closed by returning built-in + pack defaults alongside the parse error.
func LoadPolicy(vaultDir string) (PolicyConfig, []string, error) {
	var packs []ExtensionPackRules
	if RegisteredExtensionApprovalRulesProvider != nil && strings.TrimSpace(vaultDir) != "" {
		packs = RegisteredExtensionApprovalRulesProvider(vaultDir)
	}
	rawYAML := ""
	if strings.TrimSpace(vaultDir) != "" {
		if b, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml")); err == nil {
			rawYAML = string(b)
		}
	}
	pol, warnings, err := buildPolicyFromSources(packs, rawYAML)
	if err != nil {
		fallback, _, _ := buildPolicyFromSources(packs, "")
		return fallback, warnings, err
	}
	return pol, warnings, nil
}

func loadPolicyAskTTL(vaultDir string) time.Duration {
	pol, _, _ := LoadPolicy(vaultDir)
	if pol.AskTTL > 0 {
		return pol.AskTTL
	}
	return DefaultAskTTL
}

func loadPolicyProposalTTL(vaultDir string) time.Duration {
	pol, _, _ := LoadPolicy(vaultDir)
	if pol.ProposalTTL > 0 {
		return pol.ProposalTTL
	}
	return DefaultProposalTTL
}

func groupRuleIDsByMode(rules []Rule, backtick bool) (alwaysIDs, tpIDs, autoIDs []string) {
	for _, r := range rules {
		id := r.ID
		if backtick {
			id = "`" + id + "`"
		}
		switch r.Mode {
		case ModeAlways:
			alwaysIDs = append(alwaysIDs, id)
		case ModeThirdPartyOnly:
			tpIDs = append(tpIDs, id)
		case ModeAutoOnly:
			autoIDs = append(autoIDs, id)
		}
	}
	return alwaysIDs, tpIDs, autoIDs
}

// DoctorCheck validates <vault>/config.yaml and pack approval rules for `hq doctor`.
func DoctorCheck(vaultDir string) (status string, detail string) {
	pol, warnings, err := LoadPolicy(vaultDir)
	if err != nil {
		return "FAIL", fmt.Sprintf("Invalid approvals policy in config.yaml: %v", err)
	}
	alwaysIDs, tpIDs, autoIDs := groupRuleIDsByMode(pol.Rules, false)
	summary := fmt.Sprintf("%d rules active (always=[%s], third_party=[%s], auto_only=[%s], owned_repos=%d, ask_ttl=%s)",
		len(pol.Rules),
		strings.Join(alwaysIDs, ","),
		strings.Join(tpIDs, ","),
		strings.Join(autoIDs, ","),
		len(pol.OwnedRepos),
		pol.AskTTL,
	)
	if len(warnings) > 0 {
		// Empty owned_repos on default install is informational (PASS with note), while disabling core rules is WARN
		hasWarn := false
		for _, w := range warnings {
			if strings.Contains(w, "mode=never") || strings.Contains(w, "invalid approval rule") || strings.Contains(w, "attempted to weaken") {
				hasWarn = true
				break
			}
		}
		if hasWarn {
			return "WARN", summary + " · " + strings.Join(warnings, "; ")
		}
	}
	return "PASS", summary
}

// FormatActivePolicySummary renders a compact SessionStart section so LLM instructions always match PreToolUse enforcement.
func FormatActivePolicySummary(pol PolicyConfig) string {
	alwaysList, tpList, autoList := groupRuleIDsByMode(pol.Rules, true)
	ownedNote := "none configured — fails closed to gated"
	if len(pol.OwnedRepos) > 0 {
		ownedNote = strings.Join(pol.OwnedRepos, ", ")
	}
	var lines []string
	lines = append(lines, "## Active Outbound Approval Policy (`config.yaml`)")
	if len(alwaysList) > 0 {
		lines = append(lines, "- **Always Gated (`hq propose` + `ask_question`):** "+strings.Join(alwaysList, ", "))
	}
	if len(tpList) > 0 {
		lines = append(lines, fmt.Sprintf("- **Third-Party Only (`owned_repos`: `%s`):** %s", ownedNote, strings.Join(tpList, ", ")))
	}
	if len(autoList) > 0 {
		lines = append(lines, "- **Interactive-Exempt (`[Auto]`/`tainted` Gated Only):** "+strings.Join(autoList, ", "))
	}
	lines = append(lines, "- **Browser / GUI Actions (No Deterministic CLI):** Use markdown blockquote preview + native `ask_question` directly (never stage dummy `echo`/`true` commands in `hq propose`).")
	return strings.Join(lines, "\n")
}

// ParseOwnerRepoFromRemoteURL extracts normalized `owner/repo` from HTTPS or SSH git remote URLs.
func ParseOwnerRepoFromRemoteURL(rawURL string) string {
	u := strings.TrimSpace(rawURL)
	if u == "" {
		return ""
	}
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, ".git")

	// SCP-style SSH URL: git@example.com:owner/repo
	if !strings.Contains(u, "://") {
		if colon := strings.IndexByte(u, ':'); colon > 0 && colon+1 < len(u) {
			pathPart := strings.TrimPrefix(u[colon+1:], "/")
			parts := strings.Split(pathPart, "/")
			if len(parts) >= 2 && parts[len(parts)-2] != "" && parts[len(parts)-1] != "" {
				return parts[len(parts)-2] + "/" + parts[len(parts)-1]
			}
		}
	}

	// Scheme URL: https://example.com/owner/repo or ssh://git@example.com/owner/repo
	if idx := strings.Index(u, "://"); idx >= 0 {
		rest := u[idx+3:]
		if slash := strings.IndexByte(rest, '/'); slash >= 0 && slash+1 < len(rest) {
			pathPart := strings.Trim(rest[slash+1:], "/")
			parts := strings.Split(pathPart, "/")
			if len(parts) >= 2 && parts[0] != "" && parts[1] != "" {
				return parts[0] + "/" + parts[1]
			}
		}
	}
	return ""
}

func sanitizedApprovalGitEnv() []string {
	raw := os.Environ()
	out := make([]string, 0, len(raw)+1)
	for _, kv := range raw {
		if strings.HasPrefix(kv, "GIT_DIR=") ||
			strings.HasPrefix(kv, "GIT_WORK_TREE=") ||
			strings.HasPrefix(kv, "GIT_INDEX_FILE=") ||
			strings.HasPrefix(kv, "GIT_OBJECT_DIRECTORY=") ||
			strings.HasPrefix(kv, "GIT_ALTERNATE_OBJECT_DIRECTORIES=") ||
			strings.HasPrefix(kv, "GIT_COMMON_DIR=") ||
			strings.HasPrefix(kv, "GIT_PREFIX=") {
			continue
		}
		out = append(out, kv)
	}
	out = append(out, "GIT_TERMINAL_PROMPT=0")
	return out
}

func runApprovalGitCmd(dir string, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	fullArgs := append([]string{"-C", dir}, args...)
	cmd := exec.CommandContext(ctx, "git", fullArgs...)
	cmd.Env = sanitizedApprovalGitEnv()
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func gitRemoteOwnerRepo(workDir, remoteName string) string {
	if strings.TrimSpace(remoteName) == "" {
		remoteName = "origin"
	}
	dir := strings.TrimSpace(workDir)
	if dir == "" {
		if cwd, err := os.Getwd(); err == nil {
			dir = cwd
		}
	}
	if dir == "" {
		return ""
	}
	out, err := runApprovalGitCmd(dir, 50*time.Millisecond, "remote", "get-url", remoteName)
	if err != nil {
		return ""
	}
	return ParseOwnerRepoFromRemoteURL(out)
}

func expandLeadingTilde(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil && home != "" {
			if p == "~" {
				return home
			}
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

func extractLeadingCDDir(cmdLine, fallbackDir string) string {
	s := strings.TrimSpace(cmdLine)
	if !strings.HasPrefix(s, "cd ") {
		return fallbackDir
	}
	for _, sep := range []string{"&&", ";"} {
		if idx := strings.Index(s, sep); idx > 3 {
			rawDir := strings.TrimSpace(strings.TrimPrefix(s[:idx], "cd "))
			rawDir = expandLeadingTilde(strings.Trim(rawDir, `"'`))
			if rawDir != "" {
				if filepath.IsAbs(rawDir) {
					return rawDir
				}
				if fallbackDir != "" {
					return filepath.Join(fallbackDir, rawDir)
				}
			}
			break
		}
	}
	return fallbackDir
}

func applyGitDashCDirs(gitPrefix, baseDir string) string {
	dir := strings.TrimSpace(baseDir)
	for _, m := range reGitDashC.FindAllStringSubmatch(gitPrefix, -1) {
		raw := ""
		for k := 1; k < len(m); k++ {
			if m[k] != "" {
				raw = m[k]
				break
			}
		}
		raw = expandLeadingTilde(strings.TrimSpace(raw))
		if raw == "" {
			continue
		}
		if filepath.IsAbs(raw) {
			dir = raw
		} else if dir != "" {
			dir = filepath.Join(dir, raw)
		} else {
			dir = raw
		}
	}
	return dir
}

func gitPushFlagTakesSeparateArg(tok string) bool {
	if strings.Contains(tok, "=") {
		return false
	}
	switch tok {
	case "-o", "--push-option", "--receive-pack", "--exec":
		return true
	default:
		return false
	}
}

func isShellRedirToken(tok string) bool {
	return strings.Contains(tok, ">") || strings.Contains(tok, "<")
}

// ResolveRepoTarget extracts `owner/repo` from a command line and/or local git remote without network calls.
func ResolveRepoTarget(resolver, cmdLine, workDir string) string {
	effDir := extractLeadingCDDir(cmdLine, workDir)

	// 1. Explicit -R / --repo flag (`gh pr merge 42 -R owner/repo` or `HOST/owner/repo`)
	if m := reGHRepoFlag.FindStringSubmatch(cmdLine); len(m) >= 2 {
		parts := strings.Split(strings.Trim(m[1], "/"), "/")
		if len(parts) >= 2 {
			return parts[len(parts)-2] + "/" + parts[len(parts)-1]
		}
	}

	// 2. Explicit forge URL (`https://github.com/owner/repo/pull/42`)
	if m := reGitHubURLRepo.FindStringSubmatch(cmdLine); len(m) >= 2 {
		return strings.TrimSuffix(m[1], ".git")
	}

	// 3. `git [flags] push <remote-or-url>` positional inspection when using git_remote
	if resolver == ResolverGitRemote {
		if loc := reGitPushCmd.FindStringIndex(cmdLine); loc != nil {
			effDir = applyGitDashCDirs(cmdLine[loc[0]:loc[1]], effDir)
			rest := strings.TrimSpace(cmdLine[loc[1]:])
			fields := strings.Fields(rest)
			remoteTok := ""
			afterDoubleDash := false
			for i := 0; i < len(fields); i++ {
				tok := strings.Trim(fields[i], `"'`)
				if tok == "" || tok == "&&" || tok == ";" || tok == "|" || tok == "||" {
					break
				}
				if isShellRedirToken(tok) {
					if (strings.HasSuffix(tok, ">") || strings.HasSuffix(tok, "<")) && i+1 < len(fields) {
						i++
					}
					continue
				}
				if !afterDoubleDash {
					if tok == "--" {
						afterDoubleDash = true
						continue
					}
					if strings.HasPrefix(tok, "--repo=") {
						remoteTok = strings.Trim(strings.TrimPrefix(tok, "--repo="), `"'`)
						break
					}
					if tok == "--repo" && i+1 < len(fields) {
						remoteTok = strings.Trim(fields[i+1], `"'`)
						break
					}
					if strings.HasPrefix(tok, "-") {
						if gitPushFlagTakesSeparateArg(tok) && i+1 < len(fields) {
							i++
						}
						continue
					}
				}
				remoteTok = tok
				break
			}
			if remoteTok != "" {
				if parsed := ParseOwnerRepoFromRemoteURL(remoteTok); parsed != "" {
					return parsed
				}
				if !strings.Contains(remoteTok, ":") && !strings.Contains(remoteTok, "/") {
					return gitRemoteOwnerRepo(effDir, remoteTok)
				}
				return ""
			}
		}
	}

	// 4. Local git origin remote fallback
	return gitRemoteOwnerRepo(effDir, "origin")
}

// MatchesOwnedRepos returns true iff ownerRepo matches at least one `owner/repo` glob in ownedGlobs.
// Empty ownerRepo or empty ownedGlobs fails closed (`false`).
func MatchesOwnedRepos(ownedGlobs []string, ownerRepo string) bool {
	target := strings.ToLower(strings.Trim(strings.TrimSpace(ownerRepo), "/"))
	tParts := strings.Split(target, "/")
	if len(tParts) != 2 || tParts[0] == "" || tParts[1] == "" || len(ownedGlobs) == 0 {
		return false
	}
	for _, rawGlob := range ownedGlobs {
		g := strings.ToLower(strings.Trim(strings.TrimSpace(rawGlob), "/"))
		if g == "" {
			continue
		}
		if g == "*/*" || g == "*" {
			return true
		}
		gParts := strings.Split(g, "/")
		if len(gParts) != 2 {
			continue
		}
		okOwner, err1 := path.Match(gParts[0], tParts[0])
		okRepo, err2 := path.Match(gParts[1], tParts[1])
		if err1 == nil && err2 == nil && okOwner && okRepo {
			return true
		}
	}
	return false
}

func hasNonLoopbackExternalURL(cmdLine string) bool {
	matches := reExtURLHostPol.FindAllStringSubmatch(cmdLine, -1)
	if len(matches) == 0 {
		return !reLoopbackOnlyPol.MatchString(cmdLine)
	}
	for _, m := range matches {
		host := strings.ToLower(strings.TrimSpace(m[1]))
		if host != "127.0.0.1" && host != "localhost" && host != "[::1]" {
			return true
		}
	}
	return false
}

// stripShellLineContinuationsAndIFS removes bash backslash-newline line continuations outside single quotes
// and expands unquoted ${IFS}/$IFS word separators to spaces so obfuscations like `work\\\nspace` or
// `gh${IFS}pr${IFS}merge` normalize cleanly before rule matching.
func stripShellLineContinuationsAndIFS(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if inSingle {
			b.WriteByte(ch)
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if ch == '\\' {
			if i+1 < len(s) && s[i+1] == '\n' {
				i++
				continue
			}
			if i+2 < len(s) && s[i+1] == '\r' && s[i+2] == '\n' {
				i += 2
				continue
			}
			b.WriteByte(ch)
			if i+1 < len(s) {
				i++
				b.WriteByte(s[i])
			}
			continue
		}
		if inDouble {
			b.WriteByte(ch)
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		if ch == '\'' {
			inSingle = true
			b.WriteByte(ch)
			continue
		}
		if ch == '"' {
			inDouble = true
			b.WriteByte(ch)
			continue
		}
		if ch == '$' {
			if strings.HasPrefix(s[i:], "${IFS}") {
				b.WriteByte(' ')
				i += len("${IFS}") - 1
				continue
			}
			if strings.HasPrefix(s[i:], "$IFS") {
				end := i + len("$IFS")
				if end >= len(s) || !((s[end] >= 'A' && s[end] <= 'Z') || (s[end] >= 'a' && s[end] <= 'z') || (s[end] >= '0' && s[end] <= '9') || s[end] == '_') {
					b.WriteByte(' ')
					i = end - 1
					continue
				}
			}
			// Do not enter raw single-quote state for bash ANSI-C quoting $'...': let shellTokensSimple decode escapes.
			if i+1 < len(s) && s[i+1] == '\'' {
				b.WriteByte('$')
				b.WriteByte('\'')
				i += 2
				for ; i < len(s); i++ {
					c2 := s[i]
					b.WriteByte(c2)
					if c2 == '\\' && i+1 < len(s) {
						i++
						b.WriteByte(s[i])
						continue
					}
					if c2 == '\'' {
						break
					}
				}
				continue
			}
		}
		b.WriteByte(ch)
	}
	return b.String()
}

// normalizeCommandForRuleMatch splits a shell command line into quote-aware segments (breaking on unquoted
// separators, process substitutions <(...)/(>...), and command substitutions, including $(...) and backticks
// inside double quotes) and dequotes each segment's tokens with shellTokensSimple (including bash $'...'
// ANSI-C quoting, $"..." locale quoting, \\\n continuations, and ${IFS} separators).
func normalizeCommandForRuleMatch(cmdLine string) string {
	cmdLine = stripShellLineContinuationsAndIFS(cmdLine)
	var segs []string
	var cur strings.Builder
	flush := func() {
		s := strings.TrimSpace(cur.String())
		cur.Reset()
		if s == "" {
			return
		}
		toks := shellTokensSimple(s)
		if len(toks) > 0 {
			segs = append(segs, strings.Join(toks, " "))
		}
	}
	inSingle := false
	inANSISingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(cmdLine); i++ {
		ch := cmdLine[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if inANSISingle {
			cur.WriteByte(ch)
			if ch == '\\' {
				escaped = true
			} else if ch == '\'' {
				inANSISingle = false
			}
			continue
		}
		if inSingle {
			cur.WriteByte(ch)
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				cur.WriteByte(ch)
				escaped = true
				continue
			}
			if ch == '"' {
				cur.WriteByte(ch)
				inDouble = false
				continue
			}
			if ch == '`' {
				flush()
				continue
			}
			if ch == '$' && i+1 < len(cmdLine) && cmdLine[i+1] == '(' {
				flush()
				i++
				continue
			}
			cur.WriteByte(ch)
			continue
		}
		switch ch {
		case '\\':
			cur.WriteByte(ch)
			escaped = true
		case '\'':
			inSingle = true
			cur.WriteByte(ch)
		case '"':
			inDouble = true
			cur.WriteByte(ch)
		case ';', '\n', '|', '&', '(', ')', '`':
			flush()
		case '<', '>':
			if i+1 < len(cmdLine) && cmdLine[i+1] == '(' {
				flush()
				i++
				continue
			}
			cur.WriteByte(ch)
		case '$':
			if i+1 < len(cmdLine) {
				if cmdLine[i+1] == '(' {
					flush()
					i++
					continue
				}
				if cmdLine[i+1] == '\'' {
					inANSISingle = true
					cur.WriteByte('$')
					cur.WriteByte('\'')
					i++
					continue
				}
			}
			cur.WriteByte(ch)
		default:
			cur.WriteByte(ch)
		}
	}
	flush()
	return strings.Join(segs, " ; ")
}

var pipelineExecOrShellBinaries = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true,
	"fish": true, "csh": true, "tcsh": true, "ash": true,
	"eval": true, "exec": true, "source": true, ".": true,
	"xargs": true, "env": true, "nohup": true, "timeout": true,
	"sudo": true, "su": true, "doas": true,
	"python": true, "python3": true, "python2": true,
	"perl": true, "ruby": true, "node": true, "deno": true, "bun": true,
	"awk": true, "gawk": true, "mawk": true, "nawk": true,
	"sed": true, "gsed": true, "lua": true, "php": true, "tclsh": true,
}

// splitStrictTopLevelStatements splits cmdLine on unquoted top-level ';', '\n', '&&', and '||'.
// It fails closed (returns nil, false) on command/process substitutions, ANSI-C/locale quoting,
// line continuations, subshells/braces, unclosed quotes/escapes, or background '&'.
func splitStrictTopLevelStatements(cmdLine string) ([]string, bool) {
	if strings.TrimSpace(cmdLine) == "" {
		return nil, false
	}
	for i := 0; i < len(cmdLine); i++ {
		ch := cmdLine[i]
		if ch == '`' || ch == '\r' {
			return nil, false
		}
		if ch == '\\' && i+1 < len(cmdLine) && (cmdLine[i+1] == '\n' || cmdLine[i+1] == '\r') {
			return nil, false
		}
		if ch == '$' && i+1 < len(cmdLine) && (cmdLine[i+1] == '(' || cmdLine[i+1] == '\'' || cmdLine[i+1] == '"') {
			return nil, false
		}
		if (ch == '<' || ch == '>') && i+1 < len(cmdLine) && cmdLine[i+1] == '(' {
			return nil, false
		}
	}

	var stmts []string
	var cur strings.Builder
	inSingle, inDouble, escaped := false, false, false
	for i := 0; i < len(cmdLine); i++ {
		ch := cmdLine[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if inSingle {
			cur.WriteByte(ch)
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				cur.WriteByte(ch)
				escaped = true
				continue
			}
			cur.WriteByte(ch)
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\\':
			cur.WriteByte(ch)
			escaped = true
		case '\'':
			inSingle = true
			cur.WriteByte(ch)
		case '"':
			inDouble = true
			cur.WriteByte(ch)
		case '(', ')', '{', '}':
			return nil, false
		case ';', '\n':
			if s := strings.TrimSpace(cur.String()); s != "" {
				stmts = append(stmts, s)
			}
			cur.Reset()
		case '&':
			if i+1 < len(cmdLine) && cmdLine[i+1] == '&' {
				if s := strings.TrimSpace(cur.String()); s != "" {
					stmts = append(stmts, s)
				}
				cur.Reset()
				i++
				continue
			}
			// Allow fd dup like 2>&1 only if preceded by '>'; otherwise fail closed on background '&'.
			if i > 0 && cmdLine[i-1] == '>' {
				cur.WriteByte(ch)
				continue
			}
			return nil, false
		case '|':
			if i+1 < len(cmdLine) && cmdLine[i+1] == '|' {
				if s := strings.TrimSpace(cur.String()); s != "" {
					stmts = append(stmts, s)
				}
				cur.Reset()
				i++
				continue
			}
			cur.WriteByte(ch)
		default:
			cur.WriteByte(ch)
		}
	}
	if inSingle || inDouble || escaped {
		return nil, false
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		stmts = append(stmts, s)
	}
	if len(stmts) == 0 {
		return nil, false
	}
	return stmts, true
}

// filterReadOnlySearchSegments removes strictly read-only text-search pipeline stages
// (grep/egrep/fgrep/rg/ag/ack and `git grep`) so searching for a gated command string does not
// false-trigger approval rules. It fails closed (returns cmdLine, false) on any shell expansion,
// quoting trick, subshell, output redirection, exec flag (`rg --pre`), or pipe to a shell/interpreter.
func filterReadOnlySearchSegments(cmdLine string) (string, bool) {
	stmts, ok := splitStrictTopLevelStatements(cmdLine)
	if !ok {
		return cmdLine, false
	}

	var keptStmts []string
	strippedAny := false
	for _, stmt := range stmts {
		stages, hasOutputRedir := splitPipelineStagesStrict(stmt)
		if len(stages) == 0 || hasOutputRedir {
			keptStmts = append(keptStmts, stmt)
			continue
		}
		// If any stage invokes a shell, interpreter, or command runner, keep the entire pipeline fail-closed.
		hasDangerousConsumer := false
		parsedStages := make([][]string, len(stages))
		for idx, st := range stages {
			words, ok := tokenizeSimpleShellWords(st)
			if !ok || len(words) == 0 {
				hasDangerousConsumer = true
				break
			}
			parsedStages[idx] = words
			bin := firstNonAssignCommandWord(words)
			if bin == "" || strings.ContainsAny(bin, "$*?") {
				hasDangerousConsumer = true
				break
			}
			base := strings.ToLower(filepath.Base(bin))
			if pipelineExecOrShellBinaries[base] || strings.HasPrefix(base, "python") {
				hasDangerousConsumer = true
				break
			}
		}
		if hasDangerousConsumer {
			keptStmts = append(keptStmts, stmt)
			continue
		}

		var keptStages []string
		for idx, st := range stages {
			if isPureReadOnlySearchCommand(parsedStages[idx]) {
				strippedAny = true
				continue
			}
			keptStages = append(keptStages, st)
		}
		if len(keptStages) > 0 {
			keptStmts = append(keptStmts, strings.Join(keptStages, " | "))
		}
	}
	if !strippedAny {
		return cmdLine, false
	}
	return strings.Join(keptStmts, " ; "), true
}

func splitPipelineStagesStrict(stmt string) ([]string, bool) {
	var stages []string
	var cur strings.Builder
	inSingle, inDouble, escaped := false, false, false
	hasOutputRedir := false
	for i := 0; i < len(stmt); i++ {
		ch := stmt[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if inSingle {
			cur.WriteByte(ch)
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				cur.WriteByte(ch)
				escaped = true
				continue
			}
			cur.WriteByte(ch)
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\\':
			cur.WriteByte(ch)
			escaped = true
		case '\'':
			inSingle = true
			cur.WriteByte(ch)
		case '"':
			inDouble = true
			cur.WriteByte(ch)
		case '>':
			// Allow 2>&1 / >&2 / 2>/dev/null; treat any other '>' as a file-writing redirection.
			rest := strings.TrimSpace(stmt[i+1:])
			if !(strings.HasPrefix(rest, "&1") || strings.HasPrefix(rest, "&2") || strings.HasPrefix(rest, "/dev/null")) {
				hasOutputRedir = true
			}
			cur.WriteByte(ch)
		case '|':
			if s := strings.TrimSpace(cur.String()); s != "" {
				stages = append(stages, s)
			}
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if s := strings.TrimSpace(cur.String()); s != "" {
		stages = append(stages, s)
	}
	return stages, hasOutputRedir
}

func tokenizeSimpleShellWords(s string) ([]string, bool) {
	return TokenizeShellWords(s, false)
}

// StripLeadingEnv removes leading `VAR=val` assignments (and optionally bare `env` words) from toks.
func StripLeadingEnv(toks []string, stripEnvCmd bool) []string {
	i := 0
	for i < len(toks) {
		if stripEnvCmd && toks[i] == "env" {
			i++
			continue
		}
		if isEnvAssignToken(toks[i]) {
			i++
			continue
		}
		break
	}
	return toks[i:]
}

func firstNonAssignCommandWord(words []string) string {
	rest := StripLeadingEnv(words, false)
	if len(rest) == 0 {
		return ""
	}
	return rest[0]
}

func isEnvAssignToken(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	name := tok[:eq]
	for i := 0; i < len(name); i++ {
		c := name[i]
		if i == 0 {
			if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || c == '_') {
				return false
			}
		} else {
			if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
				return false
			}
		}
	}
	return true
}

func hasDisallowedPreDoubleDashFlag(args []string, exactOrPrefix ...string) bool {
	for _, a := range args {
		if a == "--" {
			return false
		}
		for _, flag := range exactOrPrefix {
			if a == flag || (strings.HasPrefix(flag, "--") && strings.HasPrefix(a, flag+"=")) {
				return true
			}
		}
	}
	return false
}

func isPureReadOnlySearchCommand(words []string) bool {
	words = StripLeadingEnv(words, false)
	if len(words) == 0 {
		return false
	}
	bin := words[0]
	if strings.ContainsAny(bin, "$*?") {
		return false
	}
	base := strings.ToLower(filepath.Base(bin))
	rest := words[1:]
	switch base {
	case "grep", "egrep", "fgrep":
		return true
	case "rg":
		// ripgrep --pre executes arbitrary preprocessor binaries; fail closed if present.
		return !hasDisallowedPreDoubleDashFlag(rest, "--pre", "--pre-glob")
	case "ag", "ack":
		return !hasDisallowedPreDoubleDashFlag(rest, "--pager")
	case "git":
		// Only allow safe non-executing pre-flags (-C <dir>, --no-pager) before `grep`,
		// and reject pager/diff-driver flags (-O / --open-files-in-pager / --ext-diff / --textconv).
		j := 0
		for j < len(rest) {
			a := rest[j]
			if a == "-C" {
				j += 2
				continue
			}
			if strings.HasPrefix(a, "-C") || a == "--no-pager" {
				j++
				continue
			}
			break
		}
		if j >= len(rest) || rest[j] != "grep" {
			return false
		}
		return !hasDisallowedPreDoubleDashFlag(rest[j+1:], "-O", "--open-files-in-pager", "--ext-diff", "--textconv")
	default:
		return false
	}
}

func isSafeOwnedGitConfigOverride(kv string) bool {
	if strings.ContainsAny(kv, "$`") {
		return false
	}
	low := strings.ToLower(strings.TrimSpace(kv))
	for _, banned := range []string{
		"remote.", "insteadof", "pushinsteadof", "sshcommand",
		"core.hookspath", "core.fsmonitor", "credential.", "include.", "includeif.",
	} {
		if strings.Contains(low, banned) {
			return false
		}
	}
	return true
}

func extractOwnerRepoFromFlagVal(raw string) string {
	cleaned := strings.Trim(strings.TrimSuffix(strings.TrimSpace(raw), ".git"), "/")
	parts := strings.Split(cleaned, "/")
	if len(parts) < 2 {
		return ""
	}
	return parts[len(parts)-2] + "/" + parts[len(parts)-1]
}

func ruleMatchesText(r *Rule, text string) bool {
	if r == nil {
		return false
	}
	norm := normalizeCommandForRuleMatch(text)
	for _, re := range r.compiled {
		if re.MatchString(text) || (norm != "" && re.MatchString(norm)) {
			return true
		}
	}
	return false
}

func isSafeOwnedRepoForgeStage(pol PolicyConfig, stage string, curDir string) bool {
	words, ok := tokenizeSimpleShellWords(stage)
	if !ok || len(words) == 0 {
		return false
	}
	for _, tok := range words {
		if !isEnvAssignToken(tok) {
			break
		}
		if reUnsafeGitEnvAssign.MatchString(tok) {
			return false
		}
	}
	rest := StripLeadingEnv(words, false)
	if len(rest) < 2 {
		return false
	}
	bin := rest[0]
	if strings.ContainsAny(bin, "$*?") {
		return false
	}
	base := strings.ToLower(filepath.Base(bin))

	// Every explicit forge URL in the stage must resolve to an owned repo.
	for _, m := range reGitHubURLRepo.FindAllStringSubmatch(stage, -1) {
		if len(m) >= 2 && !MatchesOwnedRepos(pol.OwnedRepos, strings.TrimSuffix(m[1], ".git")) {
			return false
		}
	}

	switch base {
	case "git":
		stageDir := strings.TrimSpace(curDir)
		j := 1
		for j < len(rest) {
			a := rest[j]
			if !strings.HasPrefix(a, "-") {
				break
			}
			switch {
			case a == "-C":
				if j+1 >= len(rest) {
					return false
				}
				d := rest[j+1]
				if strings.ContainsAny(d, "$*?") {
					return false
				}
				d = expandLeadingTilde(strings.TrimSpace(d))
				if filepath.IsAbs(d) {
					stageDir = d
				} else if stageDir != "" {
					stageDir = filepath.Join(stageDir, d)
				} else {
					stageDir = d
				}
				j += 2
			case strings.HasPrefix(a, "-C") && len(a) > 2:
				d := a[2:]
				if strings.ContainsAny(d, "$*?") {
					return false
				}
				d = expandLeadingTilde(strings.TrimSpace(d))
				if filepath.IsAbs(d) {
					stageDir = d
				} else if stageDir != "" {
					stageDir = filepath.Join(stageDir, d)
				} else {
					stageDir = d
				}
				j++
			case a == "-c":
				if j+1 >= len(rest) || !isSafeOwnedGitConfigOverride(rest[j+1]) {
					return false
				}
				j += 2
			case strings.HasPrefix(a, "-c") && len(a) > 2:
				if !isSafeOwnedGitConfigOverride(a[2:]) {
					return false
				}
				j++
			case a == "--no-pager":
				j++
			default:
				return false
			}
		}
		if j >= len(rest) {
			return false
		}
		sub := strings.ToLower(rest[j])
		if sub == "commit" {
			return MatchesOwnedRepos(pol.OwnedRepos, gitRemoteOwnerRepo(stageDir, "origin"))
		}
		if sub != "push" {
			return false
		}
		pushArgs := rest[j+1:]
		afterDoubleDash := false
		for k := 0; k < len(pushArgs); k++ {
			arg := pushArgs[k]
			if isShellRedirToken(arg) {
				if (strings.HasSuffix(arg, ">") || strings.HasSuffix(arg, "<")) && k+1 < len(pushArgs) {
					k++
				}
				continue
			}
			if !afterDoubleDash {
				if arg == "--" {
					afterDoubleDash = true
					continue
				}
				if strings.HasPrefix(arg, "--repo=") {
					if strings.Contains(arg, "$") {
						return false
					}
					continue
				}
				if arg == "--repo" {
					if k+1 >= len(pushArgs) || strings.Contains(pushArgs[k+1], "$") {
						return false
					}
					k++
					continue
				}
				if strings.HasPrefix(arg, "-") {
					if gitPushFlagTakesSeparateArg(arg) {
						k++
					}
					continue
				}
			}
			if strings.Contains(arg, "$") {
				return false
			}
			break
		}
		target := ResolveRepoTarget(ResolverGitRemote, stage, curDir)
		return MatchesOwnedRepos(pol.OwnedRepos, target)

	case "gh":
		if len(rest) < 3 {
			return false
		}
		sub1 := strings.ToLower(rest[1])
		sub2 := strings.ToLower(rest[2])
		if sub1 != "pr" && sub1 != "issue" {
			return false
		}
		switch sub2 {
		case "create", "close", "reopen", "comment", "edit", "ready", "review":
		default:
			return false
		}
		for k := 3; k < len(rest); k++ {
			arg := rest[k]
			switch {
			case arg == "-R" || arg == "--repo":
				if k+1 >= len(rest) || strings.Contains(rest[k+1], "$") {
					return false
				}
				if !MatchesOwnedRepos(pol.OwnedRepos, extractOwnerRepoFromFlagVal(rest[k+1])) {
					return false
				}
				k++
			case strings.HasPrefix(arg, "--repo="):
				val := strings.TrimPrefix(arg, "--repo=")
				if strings.Contains(val, "$") || !MatchesOwnedRepos(pol.OwnedRepos, extractOwnerRepoFromFlagVal(val)) {
					return false
				}
			case strings.HasPrefix(arg, "-R") && len(arg) > 2:
				val := strings.TrimPrefix(arg[2:], "=")
				if strings.Contains(val, "$") || !MatchesOwnedRepos(pol.OwnedRepos, extractOwnerRepoFromFlagVal(val)) {
					return false
				}
			}
		}
		target := ResolveRepoTarget(ResolverGitRemote, stage, curDir)
		return MatchesOwnedRepos(pol.OwnedRepos, target)

	default:
		return false
	}
}

// isSafeOwnedRepoGitForgeSubcommand returns true iff every forge_git-matching stage in evalLine
// is a direct `git push` (or local `git commit` mentioning `git push`) or safe `gh pr|issue`
// management command targeting a repository explicitly matched by pol.OwnedRepos.
// It fails closed when pol.OwnedRepos is empty, on any shell substitution/subshell/wrapper,
// on terraform/kubectl/gh repo delete|rename/gh api, or when any target resolves outside owned_repos.
func isSafeOwnedRepoGitForgeSubcommand(pol PolicyConfig, forgeRule *Rule, evalLine string, workDir string) bool {
	if len(pol.OwnedRepos) == 0 || forgeRule == nil {
		return false
	}
	stmts, ok := splitStrictTopLevelStatements(evalLine)
	if !ok {
		return false
	}
	curDir := strings.TrimSpace(workDir)
	matchedAnyForge := false
	for _, stmt := range stmts {
		stages, _ := splitPipelineStagesStrict(stmt)
		if len(stages) == 0 {
			return false
		}
		if len(stages) == 1 {
			if words, wok := tokenizeSimpleShellWords(stages[0]); wok && len(words) > 0 {
				cmdWord := strings.ToLower(firstNonAssignCommandWord(words))
				if cmdWord == "pushd" || cmdWord == "popd" {
					return false
				}
				if cmdWord == "cd" {
					rest := StripLeadingEnv(words, false)
					if len(rest) != 2 || rest[1] == "-" || strings.ContainsAny(rest[1], "$*?") {
						return false
					}
					d := expandLeadingTilde(strings.TrimSpace(rest[1]))
					if filepath.IsAbs(d) {
						curDir = d
					} else if curDir != "" {
						curDir = filepath.Join(curDir, d)
					} else {
						curDir = d
					}
					continue
				}
			}
		}
		for _, stage := range stages {
			if !ruleMatchesText(forgeRule, stage) {
				continue
			}
			if !isSafeOwnedRepoForgeStage(pol, stage, curDir) {
				return false
			}
			matchedAnyForge = true
		}
	}
	return matchedAnyForge
}

// EvaluateCommand checks checkLine against the resolved PolicyConfig and session autonomy context.
func EvaluateCommand(pol PolicyConfig, checkLine string, isAutoOrTainted bool, workDir string) EvalDecision {
	return EvaluateCommandForSession(pol, checkLine, isAutoOrTainted, false, workDir)
}

// EvaluateCommandForSession checks checkLine against the resolved PolicyConfig and session autonomy
// context, optionally allowing untainted interactive subagents to run safe owned-repo `forge_git`
// commands (`git push`, `gh pr|issue`) when `approvals.owned_repos` matches every resolved target.
func EvaluateCommandForSession(pol PolicyConfig, checkLine string, isAutoOrTainted bool, isUntaintedInteractiveSubagent bool, workDir string) EvalDecision {
	evalLine, stripped := filterReadOnlySearchSegments(checkLine)
	if stripped && strings.TrimSpace(evalLine) == "" {
		return EvalDecision{Gated: false}
	}
	normLine := normalizeCommandForRuleMatch(evalLine)
	for i := range pol.Rules {
		r := &pol.Rules[i]
		if r.Mode == ModeNever {
			continue
		}
		if len(r.compiled) == 0 && len(r.Commands) > 0 {
			_ = r.compile()
		}
		matched := false
		for _, re := range r.compiled {
			if re.MatchString(evalLine) || (normLine != "" && re.MatchString(normLine)) {
				matched = true
				break
			}
		}
		if !matched {
			continue
		}

		effectiveMode := r.Mode
		if !isAutoOrTainted {
			switch pol.InteractivePreset {
			case InteractivePresetLowFriction:
				if effectiveMode == ModeAlways && isRelaxedByLowFrictionPreset(r) {
					effectiveMode = ModeAutoOnly
				}
			case InteractivePresetStrict:
				if effectiveMode == ModeAutoOnly && r.ID != "workspace_auto_guard" {
					effectiveMode = ModeAlways
				}
			}
		}

		switch effectiveMode {
		case ModeAlways:
			return EvalDecision{
				Gated:  true,
				RuleID: r.ID,
				Mode:   ModeAlways,
				Reason: r.Description,
			}
		case ModeAutoOnly:
			if isAutoOrTainted {
				if isUntaintedInteractiveSubagent &&
					r.ID == "forge_git" &&
					pol.InteractivePreset != InteractivePresetStrict &&
					isSafeOwnedRepoGitForgeSubcommand(pol, r, evalLine, workDir) {
					continue
				}
				return EvalDecision{
					Gated:  true,
					RuleID: r.ID,
					Mode:   ModeAutoOnly,
					Reason: r.Description,
				}
			}
		case ModeThirdPartyOnly:
			if isAutoOrTainted {
				return EvalDecision{
					Gated:  true,
					RuleID: r.ID,
					Mode:   ModeThirdPartyOnly,
					Reason: r.Description,
				}
			}
			targetRepo := ResolveRepoTarget(r.TargetResolver, evalLine, workDir)
			if !MatchesOwnedRepos(pol.OwnedRepos, targetRepo) {
				return EvalDecision{
					Gated:      true,
					RuleID:     r.ID,
					Mode:       ModeThirdPartyOnly,
					TargetRepo: targetRepo,
					Reason:     r.Description,
				}
			}
		}
	}

	// Built-in unattended [Auto] mutating external HTTP safety net
	if isAutoOrTainted {
		lower := strings.ToLower(evalLine)
		if (strings.Contains(lower, "curl") && reCurlMutFlagPol.MatchString(evalLine) && hasNonLoopbackExternalURL(evalLine)) ||
			(strings.Contains(lower, "wget") && reWgetMutFlagPol.MatchString(evalLine) && hasNonLoopbackExternalURL(evalLine)) {
			return EvalDecision{
				Gated:  true,
				RuleID: "mutating_external_http",
				Mode:   ModeAutoOnly,
				Reason: "Unattended external mutating HTTP request",
			}
		}
	}

	return EvalDecision{Gated: false}
}

// isRelaxedByLowFrictionPreset returns true if r should relax from ModeAlways
// to ModeAutoOnly strictly inside owner interactive sessions under
// approvals.interactive_preset: low_friction (RFC-0026 §3.6).
// Core irreversible boundaries (messaging, release_publish, pr_merge, rsi_apply)
// and explicit vault overrides remain gated.
func isRelaxedByLowFrictionPreset(r *Rule) bool {
	if r == nil || r.Source == "vault" {
		return false
	}
	if r.ID == "workspace_own" || r.ID == "workspace_remove" {
		return true
	}
	if strings.HasPrefix(r.ID, "wsd-") {
		return true
	}
	if strings.HasPrefix(r.Source, "pack:") && !CoreNonDisableablePackRules[r.ID] {
		return true
	}
	return false
}

func isNoopBinaryToken(bin string) bool {
	base := strings.ToLower(filepath.Base(strings.Trim(strings.TrimSpace(bin), `"'`)))
	switch base {
	case "echo", "printf", "true", ":":
		return true
	default:
		return false
	}
}

func splitShellSegmentsQuoteAware(s string) (segments []string, hasRedirectOrPipe bool) {
	inSingle := false
	inDouble := false
	escaped := false
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		if escaped {
			cur.WriteByte(ch)
			escaped = false
			continue
		}
		if inSingle {
			cur.WriteByte(ch)
			if ch == '\'' {
				inSingle = false
			}
			continue
		}
		if inDouble {
			if ch == '\\' {
				cur.WriteByte(ch)
				escaped = true
				continue
			}
			cur.WriteByte(ch)
			if ch == '"' {
				inDouble = false
			}
			continue
		}
		switch ch {
		case '\\':
			cur.WriteByte(ch)
			escaped = true
		case '\'':
			inSingle = true
			cur.WriteByte(ch)
		case '"':
			inDouble = true
			cur.WriteByte(ch)
		case '>':
			hasRedirectOrPipe = true
			cur.WriteByte(ch)
		case '|':
			if i+1 < len(s) && s[i+1] == '|' {
				if seg := strings.TrimSpace(cur.String()); seg != "" {
					segments = append(segments, seg)
				}
				cur.Reset()
				i++
				continue
			}
			hasRedirectOrPipe = true
			cur.WriteByte(ch)
		case '&':
			if i+1 < len(s) && s[i+1] == '&' {
				if seg := strings.TrimSpace(cur.String()); seg != "" {
					segments = append(segments, seg)
				}
				cur.Reset()
				i++
				continue
			}
			cur.WriteByte(ch)
		case ';', '\n':
			if seg := strings.TrimSpace(cur.String()); seg != "" {
				segments = append(segments, seg)
			}
			cur.Reset()
		default:
			cur.WriteByte(ch)
		}
	}
	if seg := strings.TrimSpace(cur.String()); seg != "" {
		segments = append(segments, seg)
	}
	return segments, hasRedirectOrPipe
}

func decodeANSICQuote(seg string, start int) (string, int) {
	var b strings.Builder
	for i := start; i < len(seg); i++ {
		ch := seg[i]
		if ch == '\'' {
			return b.String(), i
		}
		if ch != '\\' || i+1 >= len(seg) {
			b.WriteByte(ch)
			continue
		}
		i++
		esc := seg[i]
		switch esc {
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'e', 'E':
			b.WriteByte(0x1b)
		case 'f':
			b.WriteByte('\f')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'v':
			b.WriteByte('\v')
		case '\\', '\'', '"', '?':
			b.WriteByte(esc)
		case 'x':
			j := i + 1
			for j < len(seg) && j < i+3 && isHexDigit(seg[j]) {
				j++
			}
			if j > i+1 {
				if v, err := strconv.ParseUint(seg[i+1:j], 16, 8); err == nil {
					b.WriteByte(byte(v))
					i = j - 1
					continue
				}
			}
			b.WriteByte('x')
		case 'u', 'U':
			maxDig := 4
			if esc == 'U' {
				maxDig = 8
			}
			j := i + 1
			for j < len(seg) && j < i+1+maxDig && isHexDigit(seg[j]) {
				j++
			}
			if j > i+1 {
				if v, err := strconv.ParseUint(seg[i+1:j], 16, 32); err == nil {
					b.WriteRune(rune(v))
					i = j - 1
					continue
				}
			}
			b.WriteByte(esc)
		default:
			if esc >= '0' && esc <= '7' {
				j := i + 1
				for j < len(seg) && j < i+3 && seg[j] >= '0' && seg[j] <= '7' {
					j++
				}
				if v, err := strconv.ParseUint(seg[i:j], 8, 8); err == nil {
					b.WriteByte(byte(v))
					i = j - 1
					continue
				}
			}
			b.WriteByte('\\')
			b.WriteByte(esc)
		}
	}
	return b.String(), len(seg) - 1
}

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// TokenizeShellWords splits seg on unquoted whitespace while decoding single/double quotes and
// backslash escapes. When decodeANSIC is true, bash ANSI-C ($'...') and locale ($"...") quoting
// are also decoded. The closed boolean reports whether all quotes and escapes were cleanly closed.
func TokenizeShellWords(seg string, decodeANSIC bool) ([]string, bool) {
	var out []string
	var cur strings.Builder
	inToken := false
	inSingle := false
	inDouble := false
	escaped := false
	for i := 0; i < len(seg); i++ {
		ch := seg[i]
		if escaped {
			cur.WriteByte(ch)
			inToken = true
			escaped = false
			continue
		}
		if inSingle {
			if ch == '\'' {
				inSingle = false
			} else {
				cur.WriteByte(ch)
			}
			inToken = true
			continue
		}
		if inDouble {
			if ch == '\\' {
				escaped = true
			} else if ch == '"' {
				inDouble = false
			} else {
				cur.WriteByte(ch)
			}
			inToken = true
			continue
		}
		switch ch {
		case ' ', '\t':
			if inToken {
				out = append(out, cur.String())
				cur.Reset()
				inToken = false
			}
		case '\\':
			escaped = true
			inToken = true
		case '\'':
			inSingle = true
			inToken = true
		case '"':
			inDouble = true
			inToken = true
		case '$':
			if decodeANSIC && i+1 < len(seg) {
				if seg[i+1] == '\'' {
					decoded, endIdx := decodeANSICQuote(seg, i+2)
					cur.WriteString(decoded)
					i = endIdx
					inToken = true
					continue
				}
				if seg[i+1] == '"' {
					inDouble = true
					inToken = true
					i++
					continue
				}
			}
			cur.WriteByte(ch)
			inToken = true
		default:
			cur.WriteByte(ch)
			inToken = true
		}
	}
	if inToken {
		out = append(out, cur.String())
	}
	return out, !(inSingle || inDouble || escaped)
}

func shellTokensSimple(seg string) []string {
	words, _ := TokenizeShellWords(seg, true)
	return words
}

func isSegmentNoop(seg string, depth int) bool {
	if depth > 4 {
		return false
	}
	s := strings.TrimSpace(seg)
	if s == "" || strings.HasPrefix(s, "#") {
		return true
	}
	toks := StripLeadingEnv(shellTokensSimple(s), true)
	if len(toks) == 0 {
		return true
	}
	base := strings.ToLower(filepath.Base(toks[0]))
	if (base == "sh" || base == "bash" || base == "zsh") && len(toks) >= 3 && toks[1] == "-c" {
		return ValidateDeterministicCommand(toks[2], nil) != nil
	}
	return isNoopBinaryToken(toks[0])
}

// ValidateDeterministicCommand rejects dummy no-op placeholder commands (`echo`, `printf`, `true`, `:`)
// staged via `hq propose` when there is no file redirection or pipeline (RFC-0021 §4.2, F2).
func ValidateDeterministicCommand(cmdStr string, argv []string) error {
	noopErr := fmt.Errorf("no-op placeholder command (echo/printf/true/:) rejected: `hq propose` requires a real deterministic CLI command executed by Go in 0 LLM tokens. For interactive browser/GUI actions, present a markdown blockquote preview and call native `ask_question` directly; for unattended [Auto] tasks, escalate via `hq outbox add --tier=interrupt`")

	if len(argv) > 0 {
		toks := argv
		for len(toks) > 0 && toks[0] == "env" {
			toks = toks[1:]
		}
		if len(toks) == 0 || strings.TrimSpace(toks[0]) == "" {
			return noopErr
		}
		base := strings.ToLower(filepath.Base(toks[0]))
		if (base == "sh" || base == "bash" || base == "zsh") && len(toks) >= 3 && toks[1] == "-c" {
			return ValidateDeterministicCommand(toks[2], nil)
		}
		if isNoopBinaryToken(toks[0]) {
			return noopErr
		}
		return nil
	}

	trimmed := strings.TrimSpace(cmdStr)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return noopErr
	}
	segs, hasRedirectOrPipe := splitShellSegmentsQuoteAware(trimmed)
	if hasRedirectOrPipe {
		return nil
	}
	if len(segs) == 0 {
		return noopErr
	}
	allNoop := true
	for _, seg := range segs {
		if !isSegmentNoop(seg, 0) {
			allNoop = false
			break
		}
	}
	if allNoop {
		return noopErr
	}
	return nil
}
