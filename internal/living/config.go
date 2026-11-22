package living

import (
	"os"
	"path/filepath"
	"strings"
)

// Config holds framework defaults and optional Vault overrides for Living Spaces (`living_spaces:` in config.yaml).
type Config struct {
	DefaultLocales  []string          `json:"defaultLocales"`
	LocaleLabels    map[string]string `json:"localeLabels"`
	DefaultSurfaces []string          `json:"defaultSurfaces"`
}

// DefaultConfig returns locale- and domain-neutral OSS framework defaults (single-locale `["en"]`).
func DefaultConfig() Config {
	return Config{
		DefaultLocales: []string{"en"},
		LocaleLabels: map[string]string{
			"en": "English",
			"zh": "中文",
			"ja": "日本語",
			"es": "Español",
			"fr": "Français",
			"de": "Deutsch",
			"ko": "한국어",
		},
		DefaultSurfaces: []string{"webui", "artifact"},
	}
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

func parseInlineList(raw string) []string {
	s := strings.TrimSpace(raw)
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	var out []string
	for _, p := range strings.Split(s, ",") {
		if v := cleanYAMLVal(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// ParseConfig parses the top-level `living_spaces:` block from config.yaml content.
func ParseConfig(content string) Config {
	cfg := DefaultConfig()
	inLiving := false
	inLabels := false
	for _, rawLine := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " \t"))
		if indent == 0 {
			inLiving = strings.HasPrefix(trimmed, "living_spaces:")
			inLabels = false
			continue
		}
		if !inLiving {
			continue
		}
		if indent == 2 {
			inLabels = false
			idx := strings.Index(trimmed, ":")
			if idx < 0 {
				continue
			}
			key := strings.TrimSpace(trimmed[:idx])
			rawVal := strings.TrimSpace(trimmed[idx+1:])
			switch key {
			case "default_locales":
				if list := parseInlineList(rawVal); len(list) > 0 {
					cfg.DefaultLocales = list
				}
			case "default_surfaces":
				if list := parseInlineList(rawVal); len(list) > 0 {
					cfg.DefaultSurfaces = list
				}
			case "locale_labels":
				inLabels = true
			}
			continue
		}
		if inLabels && indent >= 4 {
			idx := strings.Index(trimmed, ":")
			if idx < 0 {
				continue
			}
			k := cleanYAMLVal(trimmed[:idx])
			v := cleanYAMLVal(trimmed[idx+1:])
			if k != "" && v != "" {
				cfg.LocaleLabels[k] = v
			}
		}
	}
	return cfg
}

// LoadConfig reads `<vault>/config.yaml` and returns parsed Living Spaces settings.
func LoadConfig(vaultDir string) Config {
	data, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml"))
	if err != nil {
		return DefaultConfig()
	}
	return ParseConfig(string(data))
}
