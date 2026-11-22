package pulse

import (
	"os"
	"path/filepath"
	"strings"
	"time"
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

func cleanVal(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.Index(s, " #"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		return s[1 : len(s)-1]
	}
	return s
}

// ParseConfig parses the top-level `pulse:` block from config.yaml content.
func ParseConfig(content string) Config {
	cfg := DefaultConfig()
	inPulse := false
	for _, rawLine := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " \t"))
		if indent == 0 {
			inPulse = strings.HasPrefix(trimmed, "pulse:")
			continue
		}
		if !inPulse || indent < 2 {
			continue
		}
		idx := strings.Index(trimmed, ":")
		if idx < 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:idx])
		rawVal := strings.TrimSpace(trimmed[idx+1:])
		val := cleanVal(rawVal)
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
			s := strings.TrimPrefix(strings.TrimSuffix(rawVal, "]"), "[")
			var hs []time.Duration
			for _, part := range strings.Split(s, ",") {
				if d, err := time.ParseDuration(cleanVal(part)); err == nil && d > 0 {
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
