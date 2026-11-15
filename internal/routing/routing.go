package routing

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zjshen14/agy-hq/internal/extension"
)

var (
	reDeepHighOnlyTask = regexp.MustCompile(`(?i)\b(architect|architecture|design|security|audit|race\s+condition|root\s+cause|falsif|kc-?1|breach|rfc)\b`)
	reDeepTask         = regexp.MustCompile(`(?i)\b(architect|architecture|design|security|audit|race\s+condition|root\s+cause|thesis|falsif|kc-?1|breach|rfc|complex|deep)\b`)
	reDeepMediumTask   = regexp.MustCompile(`(?i)\b(sleep[- ]?consolidat\w*|consolidat(?:e|ion)|dreaming|research\s+memo|deep\s+research)\b`)
	reHeavySweepTask   = regexp.MustCompile(`(?i)\b(exhaustive|multi[- ]?file\s+refactor\w*|hard\s+e2e|e2e\s+debug\w*|test\s+debug\w*|sweep)\b`)
	reRoutineTask      = regexp.MustCompile(`(?i)\b(morning[- ]?brief|edgar[- ]?close[- ]?watch|close\s+watch|filing\s+extract\w*|multi[- ]?source\s+summar\w*|brief)\b`)
	reQuickTask        = regexp.MustCompile(`(?i)\b(triage|status|summary|summarize|list|format|lint|typo|quick|hiive|poll\w*|lookup)\b`)
	rePromptModelHint  = regexp.MustCompile(`(?i)\b(?:use|with|on)\s+(opus|sonnet|carbon|argon|hydrogen|flash_lite|flash-lite|flash)\b`)
)

// TierTarget holds the concrete model ID and subagent coarse alias for a logical tier.
type TierTarget struct {
	Model         string `json:"model"`
	SubagentModel string `json:"subagent_model,omitempty"`
}

// VaultRoutingConfig holds parsed routing overrides from <vault>/config.yaml.
type VaultRoutingConfig struct {
	FrontDeskModel string                `json:"front_desk_model,omitempty"`
	FallbackOn429  string                `json:"fallback_on_429,omitempty"`
	Tiers          map[string]TierTarget `json:"tiers,omitempty"`
}

// Request specifies inputs to Vault-aware tier/model resolution.
type Request struct {
	Prompt        string
	ExplicitModel string
	ExplicitDepth string
	ExplicitTier  string
}

// Result contains the resolved model ID, canonical observability tier (quick|standard|deep),
// logical routing tier (quick|routine|standard|heavy|deep_routine|deep), subagent alias, and resolution source.
type Result struct {
	ModelID       string `json:"model_id"`
	CanonicalTier string `json:"canonical_tier"`
	LogicalTier   string `json:"logical_tier"`
	SubagentModel string `json:"subagent_model"`
	FallbackModel string `json:"fallback_model,omitempty"`
	Source        string `json:"source"` // "explicit" | "vault" | "pack:<name>" | "builtin"
}

// NormalizeLogicalTier normalizes tier aliases to canonical 6-tier names:
// quick, routine, standard, heavy, deep_routine, deep.
func NormalizeLogicalTier(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "quick":
		return "quick"
	case "routine", "synthesis", "medium", "standard-medium":
		return "routine"
	case "heavy", "sweep", "xhigh", "standard-xhigh":
		return "heavy"
	case "deep-medium", "deep_medium", "deep-routine", "deep_routine", "consolidation", "research":
		return "deep_routine"
	case "deep":
		return "deep"
	case "standard":
		return "standard"
	default:
		return ""
	}
}

func canonicalBucketForLogical(logical string) string {
	switch logical {
	case "quick":
		return "quick"
	case "deep_routine", "deep":
		return "deep"
	default:
		return "standard"
	}
}

func inferSubagentAlias(modelID, logicalTier string) string {
	m := strings.ToLower(strings.TrimSpace(modelID))
	switch {
	case strings.Contains(m, "carbon") || strings.Contains(m, "argon") || strings.Contains(m, "opus") || strings.Contains(m, "sonnet") || strings.Contains(m, "-pro"):
		return "pro"
	case strings.HasSuffix(m, "-low") && (strings.Contains(m, "hydrogen") || strings.Contains(m, "flash")):
		return "flash_lite"
	case logicalTier == "quick":
		return "flash_lite"
	case logicalTier == "deep" || logicalTier == "deep_routine":
		return "pro"
	default:
		return "flash"
	}
}

func inferLogicalTierFromPrompt(prompt string) string {
	switch {
	case reDeepMediumTask.MatchString(prompt) && !reDeepHighOnlyTask.MatchString(prompt):
		return "deep_routine"
	case reHeavySweepTask.MatchString(prompt) && !reDeepHighOnlyTask.MatchString(prompt):
		return "heavy"
	case reRoutineTask.MatchString(prompt) && !reDeepHighOnlyTask.MatchString(prompt):
		return "routine"
	case reDeepTask.MatchString(prompt):
		return "deep"
	case reQuickTask.MatchString(prompt):
		return "quick"
	default:
		return "standard"
	}
}

// ResolveBuiltin performs 100% pure, deterministic model and tier resolution with zero disk reads.
// It backs kernel.ResolveModel directly to preserve CI Gate H3 and CUJ4 purity.
func ResolveBuiltin(prompt, explicitModel, explicitDepth, explicitTier string) (modelID, canonicalTier, subagentModel string) {
	depth := strings.ToLower(strings.TrimSpace(explicitDepth))
	model := strings.ToLower(strings.TrimSpace(explicitModel))
	tier := strings.ToLower(strings.TrimSpace(explicitTier))

	if model == "" {
		if m := rePromptModelHint.FindStringSubmatch(prompt); len(m) == 2 {
			model = strings.ToLower(m[1])
		}
	}

	if tier == "" {
		switch inferLogicalTierFromPrompt(prompt) {
		case "deep_routine":
			tier = "deep-medium"
		case "heavy":
			tier = "heavy"
		case "routine":
			tier = "routine"
		case "deep":
			tier = "deep"
		case "quick":
			tier = "quick"
		default:
			tier = "standard"
		}
	}

	inferredDepth := ""
	switch tier {
	case "quick":
		inferredDepth = "low"
		if model == "" {
			model = "gemini-4-hydrogen"
		}
		subagentModel = "flash_lite"
	case "routine", "synthesis", "medium", "standard-medium":
		tier = "standard"
		inferredDepth = "medium"
		if model == "" {
			model = "gemini-4-hydrogen"
		}
		subagentModel = "flash"
	case "heavy", "sweep", "xhigh", "standard-xhigh":
		tier = "standard"
		inferredDepth = "xhigh"
		if model == "" {
			model = "gemini-4-hydrogen"
		}
		subagentModel = "flash"
	case "deep-medium", "deep_medium", "deep-routine", "deep_routine", "consolidation", "research":
		tier = "deep"
		inferredDepth = "medium"
		if model == "" {
			model = "gemini-4-carbon"
		}
		subagentModel = "pro"
	case "deep":
		inferredDepth = "high"
		if model == "" {
			model = "gemini-4-carbon"
		}
		subagentModel = "pro"
	default:
		tier = "standard"
		inferredDepth = "high"
		if model == "" {
			model = "gemini-4-hydrogen"
		}
		subagentModel = "flash"
	}
	if depth == "" {
		depth = inferredDepth
	}

	switch model {
	case "hydrogen":
		model = "gemini-4-hydrogen"
		subagentModel = "flash"
	case "flash":
		model = "gemini-4-hydrogen"
		subagentModel = "flash"
	case "flash_lite", "flash-lite":
		model = "gemini-4-hydrogen"
		depth = "low"
		subagentModel = "flash_lite"
	case "carbon":
		model = "gemini-4-carbon"
		subagentModel = "pro"
	case "argon", "pro":
		model = "gemini-4-carbon"
		subagentModel = "pro"
	case "sonnet", "claude-sonnet":
		model = "claude-sonnet-5-5"
		subagentModel = "pro"
	case "opus", "claude-opus":
		model = "claude-opus-5-5"
		subagentModel = "pro"
	}

	if strings.HasSuffix(model, "-low") || strings.HasSuffix(model, "-medium") || strings.HasSuffix(model, "-high") || strings.HasSuffix(model, "-xhigh") {
		if strings.HasPrefix(model, "gemini-4-carbon-") || strings.HasPrefix(model, "gemini-4-argon-") ||
			strings.HasPrefix(model, "claude-opus-") || strings.HasPrefix(model, "claude-sonnet-") {
			subagentModel = "pro"
			if strings.TrimSpace(explicitTier) == "" && (strings.HasPrefix(model, "gemini-4-carbon-") || strings.HasPrefix(model, "gemini-4-argon-")) {
				tier = "deep"
			}
		} else if strings.HasPrefix(model, "gemini-4-hydrogen-low") || strings.HasPrefix(model, "gemini-3.8-flash-low") {
			subagentModel = "flash_lite"
			if strings.TrimSpace(explicitTier) == "" {
				tier = "quick"
			}
		} else if strings.HasPrefix(model, "gemini-4-hydrogen-") || strings.HasPrefix(model, "gemini-3.8-flash-") {
			subagentModel = "flash"
			if strings.TrimSpace(explicitTier) == "" {
				tier = "standard"
			}
		}
		return model, tier, subagentModel
	}
	return fmt.Sprintf("%s-%s", model, depth), tier, subagentModel
}

func cleanYAMLVal(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, " #"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

func leadingIndent(line string) int {
	n := 0
	for _, ch := range line {
		if ch == ' ' {
			n++
		} else if ch == '\t' {
			n += 2
		} else {
			break
		}
	}
	return n
}

// ParseVaultRoutingContent parses the `routing:` section of config.yaml (supporting both nested `{model, subagent_model}` and inline `<tier>: <model>`).
func ParseVaultRoutingContent(content string) VaultRoutingConfig {
	out := VaultRoutingConfig{
		Tiers: make(map[string]TierTarget),
	}
	scanner := bufio.NewScanner(strings.NewReader(content))
	inRouting := false
	inTiers := false
	routingIndent := 0
	tiersIndent := 0
	activeTier := ""
	tierEntryIndent := 0

	for scanner.Scan() {
		raw := scanner.Text()
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		ind := leadingIndent(raw)
		if !inRouting {
			if ind == 0 && strings.HasPrefix(trimmed, "routing:") {
				inRouting = true
				routingIndent = 0
			}
			continue
		}
		if ind <= routingIndent && !strings.HasPrefix(trimmed, "routing:") {
			break
		}
		if inTiers && ind <= tiersIndent {
			inTiers = false
			activeTier = ""
		}

		colon := strings.Index(trimmed, ":")
		if colon < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:colon])
		val := cleanYAMLVal(trimmed[colon+1:])

		if !inTiers {
			switch key {
			case "front_desk_model":
				out.FrontDeskModel = val
			case "fallback_on_429":
				out.FallbackOn429 = val
			case "tiers":
				inTiers = true
				tiersIndent = ind
			}
			continue
		}

		if activeTier != "" && ind > tierEntryIndent {
			cur := out.Tiers[activeTier]
			switch key {
			case "model":
				cur.Model = val
			case "subagent_model":
				cur.SubagentModel = val
			}
			out.Tiers[activeTier] = cur
			continue
		}

		norm := NormalizeLogicalTier(key)
		if norm == "" {
			activeTier = ""
			continue
		}
		activeTier = norm
		tierEntryIndent = ind
		if val != "" {
			out.Tiers[norm] = TierTarget{
				Model:         val,
				SubagentModel: inferSubagentAlias(val, norm),
			}
		} else if _, exists := out.Tiers[norm]; !exists {
			out.Tiers[norm] = TierTarget{}
		}
	}
	return out
}

// LoadVaultRouting reads <vault>/config.yaml and extracts routing overrides if present.
func LoadVaultRouting(vaultDir string) (VaultRoutingConfig, bool) {
	if strings.TrimSpace(vaultDir) == "" {
		return VaultRoutingConfig{}, false
	}
	data, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml"))
	if err != nil {
		return VaultRoutingConfig{}, false
	}
	cfg := ParseVaultRoutingContent(string(data))
	return cfg, len(cfg.Tiers) > 0 || cfg.FrontDeskModel != "" || cfg.FallbackOn429 != ""
}

func applyExplicitDepth(modelID, explicitDepth string) string {
	d := strings.ToLower(strings.TrimSpace(explicitDepth))
	if d == "" {
		return modelID
	}
	for _, suf := range []string{"-low", "-medium", "-high", "-xhigh"} {
		if strings.HasSuffix(modelID, suf) {
			return strings.TrimSuffix(modelID, suf) + "-" + d
		}
	}
	return modelID + "-" + d
}

// Resolve computes the effective model and tier honoring:
// 1. Explicit caller model / natural-language prompt model hint
// 2. <vault>/config.yaml routing.tiers.<logicalTier>
// 3. Vault-opted-in extension pack routing_defaults (<vault>/extensions.json)
// 4. Built-in ResolveBuiltin fallback
func Resolve(vaultDir string, req Request) Result {
	explicitM := strings.TrimSpace(req.ExplicitModel)
	hasHint := explicitM != "" || rePromptModelHint.MatchString(req.Prompt)

	logical := NormalizeLogicalTier(req.ExplicitTier)
	if logical == "" {
		logical = inferLogicalTierFromPrompt(req.Prompt)
	}

	vaultCfg, hasVaultCfg := LoadVaultRouting(vaultDir)
	fallback := vaultCfg.FallbackOn429

	if hasHint {
		m, cTier, sub := ResolveBuiltin(req.Prompt, req.ExplicitModel, req.ExplicitDepth, req.ExplicitTier)
		return Result{
			ModelID:       m,
			CanonicalTier: cTier,
			LogicalTier:   logical,
			SubagentModel: sub,
			FallbackModel: fallback,
			Source:        "explicit",
		}
	}

	if hasVaultCfg {
		if target, ok := vaultCfg.Tiers[logical]; ok && strings.TrimSpace(target.Model) != "" {
			m := applyExplicitDepth(strings.TrimSpace(target.Model), req.ExplicitDepth)
			sub := strings.TrimSpace(target.SubagentModel)
			if sub == "" {
				sub = inferSubagentAlias(m, logical)
			}
			return Result{
				ModelID:       m,
				CanonicalTier: canonicalBucketForLogical(logical),
				LogicalTier:   logical,
				SubagentModel: sub,
				FallbackModel: fallback,
				Source:        "vault",
			}
		}
	}

	if strings.TrimSpace(vaultDir) != "" {
		if extSt, err := extension.LoadVaultState(vaultDir); err == nil {
			for packName, act := range extSt.Packs {
				if !act.Enabled || !act.UsePackRouting || len(act.RoutingTiers) == 0 {
					continue
				}
				if rawModel := strings.TrimSpace(act.RoutingTiers[logical]); rawModel != "" {
					m := applyExplicitDepth(rawModel, req.ExplicitDepth)
					return Result{
						ModelID:       m,
						CanonicalTier: canonicalBucketForLogical(logical),
						LogicalTier:   logical,
						SubagentModel: inferSubagentAlias(m, logical),
						FallbackModel: fallback,
						Source:        "pack:" + packName,
					}
				}
			}
		}
	}

	m, cTier, sub := ResolveBuiltin(req.Prompt, "", req.ExplicitDepth, req.ExplicitTier)
	return Result{
		ModelID:       m,
		CanonicalTier: cTier,
		LogicalTier:   logical,
		SubagentModel: sub,
		FallbackModel: fallback,
		Source:        "builtin",
	}
}
