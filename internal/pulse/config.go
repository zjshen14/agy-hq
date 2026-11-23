package pulse

import (
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/yamlmini"
)

// Config holds configurable lead times for the RFC-0027 0-token anticipation engine (`pulse:` in config.yaml).
type Config struct {
	Enabled           bool            `json:"enabled"`
	PreMeetingLead    time.Duration   `json:"preMeetingLead"`
	CountdownHorizons []time.Duration `json:"countdownHorizons"`
	CutoffLead        time.Duration   `json:"cutoffLead"`
	MinSlotBuffer     time.Duration   `json:"minSlotBuffer"`
	StaleDraftAge     time.Duration   `json:"staleDraftAge"`
}

// DefaultConfig returns locale- and domain-neutral RFC-0027 defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:           true,
		PreMeetingLead:    30 * time.Minute,
		CountdownHorizons: []time.Duration{168 * time.Hour, 72 * time.Hour, 24 * time.Hour},
		CutoffLead:        6 * time.Hour,
		MinSlotBuffer:     45 * time.Minute,
		StaleDraftAge:     4 * time.Hour,
	}
}

// ParseConfig parses the top-level `pulse:` block from config.yaml content.
func ParseConfig(content string) Config {
	cfg := DefaultConfig()
	inPulse := false
	for _, rawLine := range yamlmini.Lines(content) {
		clean := yamlmini.StripComment(rawLine)
		if clean == "" {
			continue
		}
		indent := yamlmini.LeadingIndent(rawLine)
		if indent == 0 {
			inPulse = strings.HasPrefix(clean, "pulse:")
			continue
		}
		if !inPulse || indent < 2 {
			continue
		}
		key, rawVal, ok := yamlmini.SplitKV(clean)
		if !ok {
			continue
		}
		val := yamlmini.UnquoteScalar(rawVal)
		switch key {
		case "enabled":
			cfg.Enabled = strings.EqualFold(val, "true") || val == "1" || strings.EqualFold(val, "yes")
		case "pre_meeting_lead":
			if d, err := time.ParseDuration(val); err == nil && d > 0 {
				cfg.PreMeetingLead = d
			}
		case "cutoff_lead":
			if d, err := time.ParseDuration(val); err == nil && d > 0 {
				cfg.CutoffLead = d
			}
		case "min_slot_buffer":
			if d, err := time.ParseDuration(val); err == nil && d > 0 {
				cfg.MinSlotBuffer = d
			}
		case "stale_draft_age":
			if d, err := time.ParseDuration(val); err == nil && d > 0 {
				cfg.StaleDraftAge = d
			}
		case "countdown_horizons":
			items, isList := yamlmini.ParseInlineList(rawVal)
			if !isList {
				s := strings.TrimPrefix(strings.TrimSuffix(rawVal, "]"), "[")
				for _, part := range strings.Split(s, ",") {
					if v := yamlmini.UnquoteScalar(part); v != "" {
						items = append(items, v)
					}
				}
			}
			var hs []time.Duration
			for _, item := range items {
				if d, err := time.ParseDuration(item); err == nil && d > 0 {
					hs = append(hs, d)
				}
			}
			if len(hs) > 0 {
				cfg.CountdownHorizons = hs
			}
		}
	}
	return cfg
}

// LoadConfig reads `<vault>/config.yaml` and returns parsed Pulse defaults.
func LoadConfig(vaultDir string) Config {
	data, err := os.ReadFile(filepath.Join(vaultDir, "config.yaml"))
	if err != nil {
		return DefaultConfig()
	}
	return ParseConfig(string(data))
}
