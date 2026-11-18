package extension

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBridgePackFiles(t *testing.T, dir string, pluginJSON, extJSON string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if pluginJSON != "" {
		if err := os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(pluginJSON), 0o644); err != nil {
			t.Fatalf("write plugin.json: %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "hq-extension.json"), []byte(extJSON), 0o644); err != nil {
		t.Fatalf("write hq-extension.json: %v", err)
	}
}

func TestLintPackChannelBridgesValidation(t *testing.T) {
	t.Run("valid compiled channel bridge passes", func(t *testing.T) {
		packDir := filepath.Join(t.TempDir(), "acme-messaging")
		writeBridgePackFiles(t, packDir,
			`{"name":"acme-messaging","version":"1.0.0"}`,
			`{
  "schema_version": 1,
  "id": "acme-messaging",
  "version": "1.0.0",
  "components": {
    "channel_bridges": [
      {
        "id": "desk-bridge",
        "protocol": "hq-channel-bridge/v1",
        "command": ["./bin/acme-chat-bridge", "serve"],
        "env_allow": ["ACME_BRIDGE_STATE_DIR"],
        "timeout_ms": 5000,
        "max_body_runes": 2000,
        "capabilities": {
          "desk_binding": true,
          "item_threads": true,
          "task_digests": true,
          "callback_buttons": true
        }
      }
    ]
  }
}`,
		)
		if _, err := LintPack(packDir); err != nil {
			t.Fatalf("expected valid channel_bridges pack to pass LintPack, got: %v", err)
		}
	})

	cases := []struct {
		name    string
		extJSON string
		wantSub string
	}{
		{
			name: "shell wrapper rejected",
			extJSON: `{
  "schema_version": 1, "id": "acme-messaging", "version": "1.0.0",
  "components": { "channel_bridges": [{ "id": "desk-bridge", "protocol": "hq-channel-bridge/v1", "command": ["/bin/sh", "-c", "echo ok"] }] }
}`,
			wantSub: "shell or generic interpreter wrapper",
		},
		{
			name: "python wrapper rejected",
			extJSON: `{
  "schema_version": 1, "id": "acme-messaging", "version": "1.0.0",
  "components": { "channel_bridges": [{ "id": "desk-bridge", "protocol": "hq-channel-bridge/v1", "command": ["python3.12", "bridge.py"] }] }
}`,
			wantSub: "shell or generic interpreter wrapper",
		},
		{
			name: "blocked ANTIGRAVITY_CSRF_TOKEN env_allow rejected",
			extJSON: `{
  "schema_version": 1, "id": "acme-messaging", "version": "1.0.0",
  "components": { "channel_bridges": [{ "id": "desk-bridge", "protocol": "hq-channel-bridge/v1", "command": ["acme-bridge"], "env_allow": ["ANTIGRAVITY_CSRF_TOKEN"] }] }
}`,
			wantSub: "protected host variable",
		},
		{
			name: "unsupported protocol rejected",
			extJSON: `{
  "schema_version": 1, "id": "acme-messaging", "version": "1.0.0",
  "components": { "channel_bridges": [{ "id": "desk-bridge", "protocol": "hq-channel-bridge/v9", "command": ["acme-bridge"] }] }
}`,
			wantSub: "unsupported protocol",
		},
		{
			name: "timeout out of range rejected",
			extJSON: `{
  "schema_version": 1, "id": "acme-messaging", "version": "1.0.0",
  "components": { "channel_bridges": [{ "id": "desk-bridge", "protocol": "hq-channel-bridge/v1", "command": ["acme-bridge"], "timeout_ms": 30000 }] }
}`,
			wantSub: "timeout_ms",
		},
		{
			name: "self hq binary rejected",
			extJSON: `{
  "schema_version": 1, "id": "acme-messaging", "version": "1.0.0",
  "components": { "channel_bridges": [{ "id": "desk-bridge", "protocol": "hq-channel-bridge/v1", "command": ["hq", "status"] }] }
}`,
			wantSub: "shell or generic interpreter wrapper",
		},
		{
			name: "go runner wrapper rejected",
			extJSON: `{
  "schema_version": 1, "id": "acme-messaging", "version": "1.0.0",
  "components": { "channel_bridges": [{ "id": "desk-bridge", "protocol": "hq-channel-bridge/v1", "command": ["go", "run", "."] }] }
}`,
			wantSub: "shell or generic interpreter wrapper",
		},
		{
			name: "tiny max_body_runes rejected",
			extJSON: `{
  "schema_version": 1, "id": "acme-messaging", "version": "1.0.0",
  "components": { "channel_bridges": [{ "id": "desk-bridge", "protocol": "hq-channel-bridge/v1", "command": ["acme-bridge"], "max_body_runes": 2 }] }
}`,
			wantSub: "max_body_runes",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			packDir := filepath.Join(t.TempDir(), "acme-messaging")
			writeBridgePackFiles(t, packDir, `{"name":"acme-messaging","version":"1.0.0"}`, tc.extJSON)
			_, err := LintPack(packDir)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("expected error containing %q, got: %v", tc.wantSub, err)
			}
		})
	}
}

func TestEnablePackChannelBridgeCollisionAndTrustPinning(t *testing.T) {
	vaultDir := filepath.Join(t.TempDir(), "local")
	if err := os.MkdirAll(filepath.Join(vaultDir, "memory"), 0o755); err != nil {
		t.Fatalf("mkdir vault: %v", err)
	}

	makePackWithBin := func(packID, bridgeID string) (string, string) {
		packDir := filepath.Join(vaultDir, "packs", packID)
		binDir := filepath.Join(packDir, "bin")
		if err := os.MkdirAll(binDir, 0o755); err != nil {
			t.Fatalf("mkdir bin: %v", err)
		}
		binPath := filepath.Join(binDir, packID+"-bridge")
		if err := os.WriteFile(binPath, []byte("compiled-bridge-binary-"+packID), 0o755); err != nil {
			t.Fatalf("write fake bin: %v", err)
		}
		writeBridgePackFiles(t, packDir,
			`{"name":"`+packID+`","version":"1.0.0"}`,
			`{
  "schema_version": 1,
  "id": "`+packID+`",
  "version": "1.0.0",
  "components": {
    "channel_bridges": [
      {
        "id": "`+bridgeID+`",
        "protocol": "hq-channel-bridge/v1",
        "command": ["./bin/`+packID+`-bridge", "run"]
      }
    ]
  }
}`,
		)
		return packDir, binPath
	}

	packADir, binAPath := makePackWithBin("pack-alpha", "shared-desk")
	packBDir, _ := makePackWithBin("pack-beta", "shared-desk")

	if _, err := EnablePack(context.Background(), vaultDir, "pack-alpha", packADir, false); err != nil {
		t.Fatalf("EnablePack pack-alpha: %v", err)
	}
	if _, err := EnablePack(context.Background(), vaultDir, "pack-beta", packBDir, false); err == nil || !strings.Contains(err.Error(), "unqualified channel_bridges ID collision") {
		t.Fatalf("expected unqualified channel_bridges ID collision error, got: %v", err)
	}

	// Disambiguate via winner_overrides and re-enable pack-beta
	if err := UpdateVaultState(context.Background(), vaultDir, func(st *VaultState) error {
		st.WinnerOverrides = map[string]string{"shared-desk": "pack-alpha"}
		return nil
	}); err != nil {
		t.Fatalf("UpdateVaultState winner_overrides: %v", err)
	}
	if _, err := EnablePack(context.Background(), vaultDir, "pack-beta", packBDir, false); err != nil {
		t.Fatalf("expected EnablePack pack-beta to succeed with winner_overrides, got: %v", err)
	}

	// Pin SHA-256 trust bundle for pack-alpha and verify tamper rejection + 1 deduplicated brief_rollup notice
	bundle, err := BuildTrustBundle(vaultDir, "pack:pack-alpha", packADir)
	if err != nil {
		t.Fatalf("BuildTrustBundle: %v", err)
	}
	bRaw, err := json.Marshal(bundle)
	if err != nil {
		t.Fatalf("json.Marshal(bundle): %v", err)
	}
	if err := ApplyTrustBundle(vaultDir, bRaw); err != nil {
		t.Fatalf("ApplyTrustBundle: %v", err)
	}

	execCached, err := VerifyTrustedBinary(vaultDir, "pack:pack-alpha", "./bin/pack-alpha-bridge")
	if err != nil || !strings.Contains(execCached, "exec-cache") {
		t.Fatalf("expected VerifyTrustedBinary to stage in exec-cache, got path=%q err=%v", execCached, err)
	}

	// Tamper with the binary on disk -> VerifyTrustedBinary must fail closed!
	if err := os.WriteFile(binAPath, []byte("TAMPERED-BINARY-BYTES"), 0o755); err != nil {
		t.Fatalf("tamper binary: %v", err)
	}
	if _, err := VerifyTrustedBinary(vaultDir, "pack:pack-alpha", "./bin/pack-alpha-bridge"); err == nil {
		t.Fatalf("expected VerifyTrustedBinary to fail closed after binary tampering")
	}
}
