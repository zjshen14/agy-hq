package version

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"
)

// Version is the canonical SemVer release version of the agy-hq microkernel and plugin bundle.
// Gate H2-C enforces 100% parity between this constant and .agents/plugins/agy-hq/plugin.json.
const Version = "0.1.0"

// GitCommit may be overridden at build time via -ldflags "-X github.com/zjshen14/agy-hq/internal/version.GitCommit=<sha>".
var GitCommit = "dev"

var reSemVer = regexp.MustCompile(`^\d+\.\d+\.\d+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?$`)

// IsValidSemVer returns true if v is a valid SemVer 2.0 string without a leading "v" prefix.
func IsValidSemVer(v string) bool {
	return reSemVer.MatchString(strings.TrimSpace(v))
}

// EffectiveCommit returns the injected GitCommit or falls back to Go's embedded VCS build metadata.
func EffectiveCommit() string {
	if c := strings.TrimSpace(GitCommit); c != "" && c != "dev" {
		return c
	}
	if info, ok := debug.ReadBuildInfo(); ok && info != nil {
		var rev string
		var dirty bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty {
				rev += "-dirty"
			}
			return rev
		}
	}
	return "dev"
}

// PluginManifest captures core fields from plugin.json.
type PluginManifest struct {
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Version     string `json:"version"`
}

// ReadPluginManifest parses <pluginDir>/plugin.json.
func ReadPluginManifest(pluginDir string) (*PluginManifest, error) {
	p := filepath.Join(pluginDir, "plugin.json")
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var m PluginManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	return &m, nil
}
