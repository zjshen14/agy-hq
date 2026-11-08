package doctor

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// CheckResult represents a single diagnostic check in `hq doctor`.
type CheckResult struct {
	Name   string `json:"name"`
	Status string `json:"status"` // PASS, WARN, FAIL
	Detail string `json:"detail"`
}

// Run executes environment, sandbox, CLI, and Vault health checks.
func Run(vaultDir string) []CheckResult {
	var results []CheckResult

	// 1. Check Antigravity permission preset in ~/.gemini/config/config.json
	home, _ := os.UserHomeDir()
	cfgPath := filepath.Join(home, ".gemini", "config", "config.json")
	if data, err := os.ReadFile(cfgPath); err == nil {
		var rootCfg map[string]interface{}
		if json.Unmarshal(data, &rootCfg) == nil {
			cfg, _ := rootCfg["userSettings"].(map[string]interface{})
			if cfg == nil {
				cfg = rootCfg
			}
			preset, _ := cfg["permissionPreset"].(string)
			if strings.Contains(strings.ToUpper(preset), "TURBO") {
				results = append(results, CheckResult{
					Name:   "Antigravity Sandbox Preset",
					Status: "WARN",
					Detail: fmt.Sprintf("permissionPreset is %q (Seatbelt sandbox off). Switch to Default or Auto before running untrusted background automations.", preset),
				})
			} else {
				results = append(results, CheckResult{
					Name:   "Antigravity Sandbox Preset",
					Status: "PASS",
					Detail: fmt.Sprintf("permissionPreset is %q", preset),
				})
			}
			rc, _ := cfg["remoteControlEnabled"].(bool)
			if rc {
				results = append(results, CheckResult{
					Name:   "Antigravity Remote Control",
					Status: "PASS",
					Detail: "remoteControlEnabled is true in config.json",
				})
			} else {
				results = append(results, CheckResult{
					Name:   "Antigravity Remote Control",
					Status: "WARN",
					Detail: "remoteControlEnabled is false; enable it with `agy remote-control start`",
				})
			}
		}
	} else {
		results = append(results, CheckResult{
			Name:   "Antigravity Config",
			Status: "WARN",
			Detail: "Could not read ~/.gemini/config/config.json",
		})
	}

	// 2. Check agy and agentapi CLIs
	for _, bin := range []string{"agy", "agentapi"} {
		path, err := exec.LookPath(bin)
		if err != nil {
			for _, candidate := range []string{
				filepath.Join(home, ".local", "bin", bin),
				filepath.Join(home, ".gemini", "antigravity", "bin", bin),
			} {
				if st, sErr := os.Stat(candidate); sErr == nil && !st.IsDir() {
					path = candidate
					err = nil
					break
				}
			}
		}
		if err == nil && path != "" {
			results = append(results, CheckResult{
				Name:   bin + " CLI",
				Status: "PASS",
				Detail: path,
			})
		} else {
			results = append(results, CheckResult{
				Name:   bin + " CLI",
				Status: "WARN",
				Detail: bin + " not found on PATH",
			})
		}
	}

	// 3. Check local/ Vault initialization & git repo isolation
	if _, err := os.Stat(filepath.Join(vaultDir, "USER.md")); err == nil {
		results = append(results, CheckResult{
			Name:   "Private Vault (local/)",
			Status: "PASS",
			Detail: fmt.Sprintf("Initialized at %s", vaultDir),
		})
	} else {
		results = append(results, CheckResult{
			Name:   "Private Vault (local/)",
			Status: "WARN",
			Detail: "Not initialized yet; run `./bin/hq init`",
		})
	}

	return results
}
