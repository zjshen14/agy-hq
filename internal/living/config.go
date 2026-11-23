package living

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/zjshen14/agy-hq/internal/yamlmini"
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

// ParseConfig parses the top-level `living_spaces:` block from config.yaml content.
func ParseConfig(content string) Config {
	cfg := DefaultConfig()
	inLiving := false
	inLabels := false
	for _, rawLine := range yamlmini.Lines(content) {
		clean := yamlmini.StripComment(rawLine)
		if clean == "" {
			continue
		}
		indent := yamlmini.LeadingIndent(rawLine)
		if indent == 0 {
			inLiving = strings.HasPrefix(clean, "living_spaces:")
			inLabels = false
			continue
		}
		if !inLiving {
			continue
		}
		if indent == 2 {
			inLabels = false
			key, rawVal, ok := yamlmini.SplitKV(clean)
			if !ok {
				continue
			}
			switch key {
			case "default_locales":
				if list, isList := yamlmini.ParseInlineList(rawVal); isList && len(list) > 0 {
					cfg.DefaultLocales = list
				}
			case "default_surfaces":
				if list, isList := yamlmini.ParseInlineList(rawVal); isList && len(list) > 0 {
					cfg.DefaultSurfaces = list
				}
			case "locale_labels":
				inLabels = true
			}
			continue
		}
		if inLabels && indent >= 4 {
			rawKey, rawVal, ok := yamlmini.SplitKV(clean)
			if !ok {
				continue
			}
			k := yamlmini.UnquoteScalar(rawKey)
			v := yamlmini.UnquoteScalar(rawVal)
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
