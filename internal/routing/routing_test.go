package routing

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/zjshen14/agy-hq/internal/extension"
)

func TestResolvePrecedenceVaultPackAndBuiltin(t *testing.T) {
	vaultDir := t.TempDir()

	// 1. Builtin fallback when no config.yaml exists
	r0 := Resolve(vaultDir, Request{Prompt: "Morning brief and multi-source summary"})
	if r0.ModelID != "gemini-4-hydrogen-medium" || r0.LogicalTier != "routine" || r0.Source != "builtin" {
		t.Fatalf("unexpected builtin resolution: %+v", r0)
	}

	// 2. Pack routing_defaults (only when use_pack_routing=true)
	err := extension.UpdateVaultState(context.Background(), vaultDir, func(st *extension.VaultState) error {
		st.Packs["acme-workspace"] = extension.PackActivation{
			Enabled:        true,
			UsePackRouting: true,
			RoutingTiers: map[string]string{
				"routine":      "gemini-3.8-flash-medium",
				"deep_routine": "claude-sonnet-5-5-medium",
			},
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	r1 := Resolve(vaultDir, Request{ExplicitTier: "deep-medium"})
	if r1.ModelID != "claude-sonnet-5-5-medium" || r1.CanonicalTier != "deep" || r1.SubagentModel != "pro" || r1.Source != "pack:acme-workspace" {
		t.Fatalf("unexpected pack resolution: %+v", r1)
	}

	// 3. Vault config.yaml overrides pack routing_defaults
	cfgYAML := `routing:
  front_desk_model: gemini-3.8-flash-medium
  tiers:
    quick: gemini-3.8-flash-low
    routine:
      model: gemini-3.8-flash-medium
      subagent_model: flash
    deep_routine:
      model: gemini-4-argon-medium
      subagent_model: pro
  fallback_on_429: gemini-3.8-flash-low
`
	if err := os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(cfgYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	r2 := Resolve(vaultDir, Request{ExplicitTier: "deep-medium"})
	if r2.ModelID != "gemini-4-argon-medium" || r2.Source != "vault" || r2.FallbackModel != "gemini-3.8-flash-low" {
		t.Fatalf("unexpected vault resolution: %+v", r2)
	}
	rQuick := Resolve(vaultDir, Request{ExplicitTier: "quick"})
	if rQuick.ModelID != "gemini-3.8-flash-low" || rQuick.SubagentModel != "flash_lite" {
		t.Fatalf("unexpected inline quick tier resolution: %+v", rQuick)
	}

	// 4. Explicit caller model overrides Vault config.yaml
	r3 := Resolve(vaultDir, Request{Prompt: "Use opus to audit concurrency", ExplicitTier: "deep"})
	if r3.ModelID != "claude-opus-5-5-high" || r3.Source != "explicit" {
		t.Fatalf("unexpected explicit override resolution: %+v", r3)
	}

	// 5. Coarse alias (`flash_lite`, `flash`, `pro`) honors customized Vault/Pack tier overrides,
	//    and suffixless custom explicit model IDs are preserved verbatim without appending `-high`.
	rCoarseQuick := Resolve(vaultDir, Request{ExplicitModel: "flash_lite"})
	if rCoarseQuick.ModelID != "gemini-3.8-flash-low" || rCoarseQuick.Source != "vault" {
		t.Fatalf("expected flash_lite alias to honor customized vault quick tier, got: %+v", rCoarseQuick)
	}
	rCustomVerbatim := Resolve(vaultDir, Request{ExplicitModel: "acme-custom-fast-v2"})
	if rCustomVerbatim.ModelID != "acme-custom-fast-v2" {
		t.Fatalf("expected suffixless custom model ID preserved verbatim, got: %+v", rCustomVerbatim)
	}
	cands := CustomFallbackCandidates(vaultDir, "retired-low-model")
	if len(cands) == 0 || cands[0] != "gemini-3.8-flash-low" {
		t.Fatalf("expected CustomFallbackCandidates to surface customized vault quick model first, got: %v", cands)
	}
}

func TestNeonDefaultsAndExplicitResolution(t *testing.T) {
	vaultDir := t.TempDir()

	tierCases := []struct {
		tier      string
		wantModel string
		wantCanon string
		wantSub   string
	}{
		{"quick", "gemini-4-hydrogen-low", "quick", "flash_lite"},
		{"routine", "gemini-4-hydrogen-medium", "standard", "flash"},
		{"standard", "gemini-4-neon-medium", "standard", "flash"},
		{"heavy", "gemini-4-neon-high", "standard", "flash"},
		{"deep_routine", "gemini-4-neon-high", "deep", "pro"},
		{"deep-medium", "gemini-4-neon-high", "deep", "pro"},
		{"deep", "gemini-4-carbon-high", "deep", "pro"},
	}
	for _, tc := range tierCases {
		res := Resolve(vaultDir, Request{ExplicitTier: tc.tier})
		if res.ModelID != tc.wantModel || res.CanonicalTier != tc.wantCanon || res.SubagentModel != tc.wantSub {
			t.Fatalf("Resolve(tier=%q) = %+v, want model=%q tier=%q sub=%q", tc.tier, res, tc.wantModel, tc.wantCanon, tc.wantSub)
		}
	}

	explicitCases := []struct {
		req       Request
		wantModel string
		wantCanon string
		wantSub   string
	}{
		{Request{ExplicitModel: "neon"}, "gemini-4-neon-medium", "standard", "flash"},
		{Request{ExplicitModel: "neon", ExplicitDepth: "low"}, "gemini-4-neon-low", "standard", "flash_lite"},
		{Request{ExplicitModel: "neon", ExplicitDepth: "medium"}, "gemini-4-neon-medium", "standard", "flash"},
		{Request{ExplicitModel: "neon", ExplicitDepth: "high"}, "gemini-4-neon-high", "standard", "flash"},
		{Request{ExplicitModel: "neon", ExplicitDepth: "xhigh"}, "gemini-4-neon-xhigh", "standard", "flash"},
		{Request{ExplicitModel: "neon", ExplicitTier: "deep"}, "gemini-4-neon-high", "deep", "pro"},
		{Request{ExplicitModel: "gemini-4-neon-low"}, "gemini-4-neon-low", "quick", "flash_lite"},
		{Request{ExplicitModel: "gemini-4-neon-medium"}, "gemini-4-neon-medium", "standard", "flash"},
		{Request{ExplicitModel: "gemini-4-neon-high"}, "gemini-4-neon-high", "standard", "flash"},
		{Request{ExplicitModel: "gemini-4-neon-xhigh"}, "gemini-4-neon-xhigh", "standard", "flash"},
		{Request{Prompt: "Use neon to debug the parser"}, "gemini-4-neon-medium", "standard", "flash"},
		{Request{Prompt: "Use neon to consolidate memory", ExplicitTier: "deep-medium"}, "gemini-4-neon-high", "deep", "pro"},
	}
	for _, tc := range explicitCases {
		res := Resolve(vaultDir, tc.req)
		if res.ModelID != tc.wantModel || res.CanonicalTier != tc.wantCanon || res.SubagentModel != tc.wantSub {
			t.Fatalf("Resolve(%+v) = %+v, want model=%q tier=%q sub=%q", tc.req, res, tc.wantModel, tc.wantCanon, tc.wantSub)
		}
	}

	// Unmodified template config.yaml defaults match builtinDefaultModelForLogical so pack routing still applies.
	tplCfg := `routing:
  front_desk_model: gemini-4-hydrogen-medium
  tiers:
    quick:
      model: gemini-4-hydrogen-low
      subagent_model: flash_lite
    routine:
      model: gemini-4-hydrogen-medium
      subagent_model: flash
    standard:
      model: gemini-4-neon-medium
      subagent_model: flash
    heavy:
      model: gemini-4-neon-high
      subagent_model: flash
    deep_routine:
      model: gemini-4-neon-high
      subagent_model: pro
    deep:
      model: gemini-4-carbon-high
      subagent_model: pro
  fallback_on_429: gemini-4-hydrogen-medium
`
	if err := os.WriteFile(filepath.Join(vaultDir, "config.yaml"), []byte(tplCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := extension.UpdateVaultState(context.Background(), vaultDir, func(st *extension.VaultState) error {
		st.Packs["neon-pack"] = extension.PackActivation{
			Enabled:        true,
			UsePackRouting: true,
			RoutingTiers: map[string]string{
				"standard": "gemini-4-neon-high",
			},
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	rPack := Resolve(vaultDir, Request{ExplicitTier: "standard"})
	if rPack.ModelID != "gemini-4-neon-high" || rPack.Source != "pack:neon-pack" {
		t.Fatalf("expected pack override over unmodified template defaults, got %+v", rPack)
	}
}
