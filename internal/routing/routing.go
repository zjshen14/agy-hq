package routing

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/yamlmini"
)

var (
	reDeepHighOnlyTask = regexp.MustCompile(`(?i)\b(architect|architecture|design|security|audit|race\s+condition|root\s+cause|falsif|kc-?1|breach|rfc)\b`)
	reDeepTask         = regexp.MustCompile(`(?i)\b(architect|architecture|design|security|audit|race\s+condition|root\s+cause|thesis|falsif|kc-?1|breach|rfc|complex|deep)\b`)
	reDeepMediumTask   = regexp.MustCompile(`(?i)\b(sleep[- ]?consolidat\w*|consolidat(?:e|ion)|dreaming|research\s+memo|deep\s+research)\b`)
	reHeavySweepTask   = regexp.MustCompile(`(?i)\b(exhaustive|multi[- ]?file\s+refactor\w*|hard\s+e2e|e2e\s+debug\w*|test\s+debug\w*|sweep)\b`)
	reRoutineTask      = regexp.MustCompile(`(?i)\b(morning[- ]?brief|edgar[- ]?close[- ]?watch|close\s+watch|filing\s+extract\w*|multi[- ]?source\s+summar\w*|brief)\b`)
	reQuickTask        = regexp.MustCompile(`(?i)\b(triage|status|summary|summarize|list|format|lint|typo|quick|hiive|poll\w*|lookup)\b`)
	rePromptModelHint  = regexp.MustCompile(`(?i)\b(?:use|with|on)\s+(opus|sonnet|carbon|argon|hydrogen|neon|flash_lite|flash-lite|flash)\b`)
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
	case strings.HasSuffix(m, "-low") && (strings.Contains(m, "hydrogen") || strings.Contains(m, "neon") || strings.Contains(m, "flash")):
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
		tier = inferLogicalTierFromPrompt(prompt)
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
		inferredDepth = "high"
		if model == "" {
			model = "gemini-4-neon"
		} else if model == "hydrogen" || model == "gemini-4-hydrogen" {
			inferredDepth = "xhigh"
		}
		subagentModel = "flash"
	case "deep-medium", "deep_medium", "deep-routine", "deep_routine", "consolidation", "research":
		tier = "deep"
		inferredDepth = "high"
		if model == "" {
			model = "gemini-4-neon"
		} else if model != "neon" && model != "gemini-4-neon" {
			inferredDepth = "medium"
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
		inferredDepth = "medium"
		if model == "" {
			model = "gemini-4-neon"
		} else if model != "neon" && model != "gemini-4-neon" {
			inferredDepth = "high"
		}
		subagentModel = "flash"
	}
	if depth == "" {
		depth = inferredDepth
	}

	isMultiDepthBase := model == "gemini-4-hydrogen" || model == "gemini-4-carbon" || model == "gemini-4-neon"
	switch model {
	case "neon", "gemini-4-neon":
		model = "gemini-4-neon"
		if tier == "deep" {
			subagentModel = "pro"
		} else if depth == "low" || tier == "quick" {
			subagentModel = "flash_lite"
		} else {
			subagentModel = "flash"
		}
		isMultiDepthBase = true
	case "hydrogen":
		model = "gemini-4-hydrogen"
		subagentModel = "flash"
		isMultiDepthBase = true
	case "flash":
		model = "gemini-4-hydrogen"
		subagentModel = "flash"
		isMultiDepthBase = true
	case "flash_lite", "flash-lite":
		model = "gemini-4-hydrogen"
		depth = "low"
		subagentModel = "flash_lite"
		isMultiDepthBase = true
	case "carbon":
		model = "gemini-4-carbon"
		subagentModel = "pro"
		isMultiDepthBase = true
	case "argon", "pro":
		model = "gemini-4-carbon"
		subagentModel = "pro"
		isMultiDepthBase = true
	case "sonnet", "claude-sonnet":
		model = "claude-sonnet-5-5"
		subagentModel = "pro"
		isMultiDepthBase = true
	case "opus", "claude-opus":
		model = "claude-opus-5-5"
		subagentModel = "pro"
		isMultiDepthBase = true
	case "gemini-4-argon", "claude-sonnet-5-5", "claude-opus-5-5", "gemini-3.8-flash":
		isMultiDepthBase = true
	}

	if strings.HasSuffix(model, "-low") || strings.HasSuffix(model, "-medium") || strings.HasSuffix(model, "-high") || strings.HasSuffix(model, "-xhigh") {
		if strings.HasPrefix(model, "gemini-4-carbon-") || strings.HasPrefix(model, "gemini-4-argon-") ||
			strings.HasPrefix(model, "claude-opus-") || strings.HasPrefix(model, "claude-sonnet-") {
			subagentModel = "pro"
			if strings.TrimSpace(explicitTier) == "" && (strings.HasPrefix(model, "gemini-4-carbon-") || strings.HasPrefix(model, "gemini-4-argon-")) {
				tier = "deep"
			}
		} else if strings.HasPrefix(model, "gemini-4-neon-") {
			if strings.HasSuffix(model, "-low") {
				subagentModel = "flash_lite"
				if strings.TrimSpace(explicitTier) == "" {
					tier = "quick"
				}
			} else if tier == "deep" {
				subagentModel = "pro"
			} else {
				subagentModel = "flash"
				if strings.TrimSpace(explicitTier) == "" {
					tier = "standard"
				}
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
	if !isMultiDepthBase && strings.TrimSpace(explicitDepth) == "" {
		subagentModel = inferSubagentAlias(model, tier)
		return model, tier, subagentModel
	}
	return fmt.Sprintf("%s-%s", model, depth), tier, subagentModel
}

// ParseVaultRoutingContent parses the `routing:` section of config.yaml (supporting both nested `{model, subagent_model}` and inline `<tier>: <model>`).
func ParseVaultRoutingContent(content string) VaultRoutingConfig {
	out := VaultRoutingConfig{
		Tiers: make(map[string]TierTarget),
	}
	inRouting := false
	inTiers := false
	routingIndent := 0
	tiersIndent := 0
	activeTier := ""
	tierEntryIndent := 0

	for _, raw := range yamlmini.Lines(content) {
		clean := yamlmini.StripComment(raw)
		if clean == "" {
			continue
		}
		ind := yamlmini.LeadingIndent(raw)
		if !inRouting {
			if ind == 0 && strings.HasPrefix(clean, "routing:") {
				inRouting = true
				routingIndent = 0
			}
			continue
		}
		if ind <= routingIndent && !strings.HasPrefix(clean, "routing:") {
			break
		}
		if inTiers && ind <= tiersIndent {
			inTiers = false
			activeTier = ""
		}

		key, rawVal, ok := yamlmini.SplitKV(clean)
		if !ok {
			continue
		}
		val := yamlmini.UnquoteScalar(rawVal)

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

func builtinDefaultModelForLogical(logical string) string {
	switch logical {
	case "quick":
		return "gemini-4-hydrogen-low"
	case "routine":
		return "gemini-4-hydrogen-medium"
	case "heavy":
		return "gemini-4-neon-high"
	case "deep_routine":
		return "gemini-4-neon-high"
	case "deep":
		return "gemini-4-carbon-high"
	default:
		return "gemini-4-neon-medium"
	}
}

type optedInPackRouting struct {
	name  string
	tiers map[string]string
}

func loadOptedInPackRoutings(vaultDir string) []optedInPackRouting {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	extSt, err := extension.LoadVaultState(vaultDir)
	if err != nil {
		return nil
	}
	var out []optedInPackRouting
	for _, packName := range extSt.OrderedEnabledPacks() {
		act := extSt.Packs[packName]
		if !act.Enabled || !act.UsePackRouting || len(act.RoutingTiers) == 0 {
			continue
		}
		if strings.TrimSpace(act.Path) != "" {
			if _, vErr := extension.VerifyTrustedFileReadOnly(vaultDir, "pack:"+packName, filepath.Join(act.Path, "hq-extension.json")); vErr != nil {
				continue
			}
		}
		out = append(out, optedInPackRouting{name: packName, tiers: act.RoutingTiers})
	}
	return out
}

func lookupPackRoutingModel(packs []optedInPackRouting, logical string) (string, string) {
	for _, p := range packs {
		if rawModel := strings.TrimSpace(p.tiers[logical]); rawModel != "" {
			return rawModel, p.name
		}
	}
	return "", ""
}

func buildVaultTierResult(target TierTarget, logical, explicitDepth, fallback string) Result {
	m := applyExplicitDepth(strings.TrimSpace(target.Model), explicitDepth)
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

func buildPackTierResult(packModel, packName, logical, explicitDepth, fallback string) Result {
	m := applyExplicitDepth(packModel, explicitDepth)
	return Result{
		ModelID:       m,
		CanonicalTier: canonicalBucketForLogical(logical),
		LogicalTier:   logical,
		SubagentModel: inferSubagentAlias(m, logical),
		FallbackModel: fallback,
		Source:        "pack:" + packName,
	}
}

func coarseAliasLogicalTier(explicitModel, explicitTier string) (string, bool) {
	normT := NormalizeLogicalTier(explicitTier)
	switch strings.ToLower(strings.TrimSpace(explicitModel)) {
	case "flash_lite", "flash-lite":
		return "quick", true
	case "flash":
		if normT == "routine" || normT == "heavy" || normT == "standard" {
			return normT, true
		}
		return "standard", true
	case "pro":
		if normT == "deep_routine" || normT == "deep" {
			return normT, true
		}
		return "deep", true
	default:
		return "", false
	}
}

// CustomFallbackCandidates returns ordered customized model IDs configured in <vault>/config.yaml
// and opted-in trusted extension packs (<vault>/extensions.json) for requestedModel so live
// Language Server fallback prefers user/pack overrides ahead of built-in defaults.
func CustomFallbackCandidates(vaultDir, requestedModel string) []string {
	if strings.TrimSpace(vaultDir) == "" {
		return nil
	}
	vaultCfg, hasVaultCfg := LoadVaultRouting(vaultDir)
	reqNorm := strings.ToLower(strings.TrimSpace(requestedModel))

	var tierOrder []string
	switch {
	case strings.Contains(reqNorm, "carbon") || strings.Contains(reqNorm, "argon") ||
		strings.Contains(reqNorm, "opus") || strings.Contains(reqNorm, "sonnet") ||
		strings.Contains(reqNorm, "pro"):
		tierOrder = []string{"deep", "deep_routine", "standard", "routine", "quick"}
	case strings.Contains(reqNorm, "lite") || strings.Contains(reqNorm, "-low") || strings.Contains(reqNorm, "flash"):
		tierOrder = []string{"quick", "routine", "standard"}
	default:
		tierOrder = []string{"standard", "routine", "quick", "deep_routine"}
	}

	seen := make(map[string]bool)
	var out []string
	add := func(cand string) {
		c := strings.TrimSpace(cand)
		if c == "" || strings.EqualFold(c, reqNorm) {
			return
		}
		k := strings.ToLower(c)
		if seen[k] {
			return
		}
		seen[k] = true
		out = append(out, c)
	}

	packs := loadOptedInPackRoutings(vaultDir)
	for _, t := range tierOrder {
		packM, _ := lookupPackRoutingModel(packs, t)
		if hasVaultCfg {
			if target, ok := vaultCfg.Tiers[t]; ok && strings.TrimSpace(target.Model) != "" {
				rawV := strings.TrimSpace(target.Model)
				if rawV != builtinDefaultModelForLogical(t) {
					add(rawV)
				}
			}
		}
		if packM != "" && packM != builtinDefaultModelForLogical(t) {
			add(packM)
		}
	}
	if hasVaultCfg {
		if fb := strings.TrimSpace(vaultCfg.FallbackOn429); fb != "" && fb != builtinDefaultModelForLogical("routine") {
			add(fb)
		}
		if fd := strings.TrimSpace(vaultCfg.FrontDeskModel); fd != "" && fd != builtinDefaultModelForLogical("routine") {
			add(fd)
		}
	}
	return out
}

// Resolve computes the effective model and tier honoring:
// 1. Explicit caller model / natural-language prompt model hint (consulting customized Vault/Pack tiers first on coarse aliases flash_lite/flash/pro)
// 2. Customized <vault>/config.yaml routing.tiers.<logicalTier>
// 3. Vault-opted-in & trusted extension pack routing_defaults (<vault>/extensions.json)
// 4. Unmodified template <vault>/config.yaml or built-in ResolveBuiltin fallback
func Resolve(vaultDir string, req Request) Result {
	explicitM := strings.TrimSpace(req.ExplicitModel)
	hasHint := explicitM != "" || rePromptModelHint.MatchString(req.Prompt)

	logical := NormalizeLogicalTier(req.ExplicitTier)
	if logical == "" {
		logical = inferLogicalTierFromPrompt(req.Prompt)
	}

	vaultCfg, hasVaultCfg := LoadVaultRouting(vaultDir)
	fallback := vaultCfg.FallbackOn429
	packs := loadOptedInPackRoutings(vaultDir)

	if coarseLogical, isCoarse := coarseAliasLogicalTier(explicitM, req.ExplicitTier); isCoarse {
		packCoarse, packCoarseName := lookupPackRoutingModel(packs, coarseLogical)
		if hasVaultCfg {
			if target, ok := vaultCfg.Tiers[coarseLogical]; ok && strings.TrimSpace(target.Model) != "" {
				if strings.TrimSpace(target.Model) != builtinDefaultModelForLogical(coarseLogical) {
					return buildVaultTierResult(target, coarseLogical, req.ExplicitDepth, fallback)
				}
			}
		}
		if packCoarse != "" {
			return buildPackTierResult(packCoarse, packCoarseName, coarseLogical, req.ExplicitDepth, fallback)
		}
	}

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

	packModel, packName := lookupPackRoutingModel(packs, logical)

	if hasVaultCfg {
		if target, ok := vaultCfg.Tiers[logical]; ok && strings.TrimSpace(target.Model) != "" {
			rawVaultModel := strings.TrimSpace(target.Model)
			// If user explicitly opted into pack routing (`--use-pack-routing`) and config.yaml still holds
			// the unmodified template default for this tier, honor the opted-in pack override.
			if packModel == "" || rawVaultModel != builtinDefaultModelForLogical(logical) {
				return buildVaultTierResult(target, logical, req.ExplicitDepth, fallback)
			}
		}
	}

	if packModel != "" {
		return buildPackTierResult(packModel, packName, logical, req.ExplicitDepth, fallback)
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
