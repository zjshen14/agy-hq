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
