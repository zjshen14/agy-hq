package living

import (
	"encoding/json"
	"strings"
)

// Supported composable Living Space block types (RFC-0027 §4.3).
const (
	BlockKPIStrip          = "kpi_strip"
	BlockTimelineAccordion = "timeline_accordion"
	BlockComparisonTable   = "comparison_table"
	BlockChecklistLedger   = "checklist_ledger"
	BlockCalloutNotes      = "callout_notes"
)

// LocalizedText accepts either a plain JSON string ("Launch") or a locale map ({"en": "Launch", "zh": "发布"}).
type LocalizedText struct {
	Plain  string
	Values map[string]string
}

// Text constructs a plain single-locale LocalizedText.
func Text(s string) LocalizedText {
	return LocalizedText{Plain: s}
}

// MultiText constructs a multi-locale LocalizedText.
func MultiText(m map[string]string) LocalizedText {
	cp := make(map[string]string, len(m))
	for k, v := range m {
		cp[k] = v
	}
	return LocalizedText{Values: cp}
}

// UnmarshalJSON decodes either a JSON string or map[string]string.
func (lt *LocalizedText) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*lt = LocalizedText{}
		return nil
	}
	if strings.HasPrefix(trimmed, "\"") {
		var s string
		if err := json.Unmarshal(data, &s); err != nil {
			return err
		}
		*lt = LocalizedText{Plain: s}
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal(data, &m); err != nil {
		return err
	}
	*lt = LocalizedText{Values: m}
	return nil
}

// MarshalJSON writes a compact string when single-valued or a locale object when multi-locale.
func (lt LocalizedText) MarshalJSON() ([]byte, error) {
	if len(lt.Values) > 0 {
		return json.Marshal(lt.Values)
	}
	return json.Marshal(lt.Plain)
}

// Resolve returns the localized string for `locale`, falling back to `fallback`, `"en"`, or any available entry.
func (lt LocalizedText) Resolve(locale, fallback string) string {
	if len(lt.Values) == 0 {
		return lt.Plain
	}
	if v := strings.TrimSpace(lt.Values[locale]); v != "" {
		return v
	}
	if fallback != "" {
		if v := strings.TrimSpace(lt.Values[fallback]); v != "" {
			return v
		}
	}
	if v := strings.TrimSpace(lt.Values["en"]); v != "" {
		return v
	}
	for _, v := range lt.Values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return lt.Plain
}

// IsEmpty reports whether LocalizedText carries any non-blank value.
func (lt LocalizedText) IsEmpty() bool {
	return strings.TrimSpace(lt.Resolve("en", "")) == ""
}

// PerspectiveSpec defines an optional viewpoint/audience tab (`Exec / Eng`, `Adult / Kids`, etc.).
type PerspectiveSpec struct {
	ID    string        `json:"id"`
	Label LocalizedText `json:"label"`
}

// DataSourceSpec binds a Space block to a 0-token live data source (`hq_builtin` or `trusted_argv`).
type DataSourceSpec struct {
	ID              string   `json:"id"`
	Kind            string   `json:"kind"`             // hq_builtin | trusted_argv
	Target          string   `json:"target,omitempty"` // workspaces | goals | delegates | obs (for hq_builtin)
	Argv            []string `json:"argv,omitempty"`   // trusted binary + args (for trusted_argv)
	TimeoutMs       int      `json:"timeoutMs,omitempty"`
	LastRefreshedAt string   `json:"lastRefreshedAt,omitempty"`
	LastError       string   `json:"lastError,omitempty"`
}

// KPIMetric is a single KPI pill in a `kpi_strip` block.
type KPIMetric struct {
	Label LocalizedText `json:"label"`
	Value LocalizedText `json:"value"`
	Tone  string        `json:"tone,omitempty"` // neutral | ok | warn | accent
}

// TimelineEntry is a collapsible item in a `timeline_accordion` block.
type TimelineEntry struct {
	ID           string          `json:"id"`
	DateLabel    LocalizedText   `json:"dateLabel"`
	Title        LocalizedText   `json:"title"`
	Summary      LocalizedText   `json:"summary,omitempty"`
	Group        string          `json:"group,omitempty"`
	Perspectives []string        `json:"perspectives,omitempty"`
	Details      []LocalizedText `json:"details,omitempty"`
	Callout      LocalizedText   `json:"callout,omitempty"`
}

// TableRow is a row in a `comparison_table` block.
type TableRow struct {
	Cells        []LocalizedText `json:"cells"`
	Group        string          `json:"group,omitempty"`
	Perspectives []string        `json:"perspectives,omitempty"`
	Badge        string          `json:"badge,omitempty"`
}

// ChecklistRow renders a 3-state readiness item (`confirmed` | `open` | `skipped`) in `checklist_ledger`.
type ChecklistRow struct {
	ID          string        `json:"id"`
	Title       LocalizedText `json:"title"`
	Tag         string        `json:"tag,omitempty"`
	Status      string        `json:"status"` // confirmed | open | skipped
	Group       string        `json:"group,omitempty"`
	ExternalRef string        `json:"externalRef,omitempty"`
	StatusNote  LocalizedText `json:"statusNote,omitempty"`
	SkipReason  LocalizedText `json:"skipReason,omitempty"`
	CutoffAt    string        `json:"cutoffAt,omitempty"`
	ActionURL   string        `json:"actionUrl,omitempty"`
}

// CalloutNote is a compact callout card in a `callout_notes` block.
type CalloutNote struct {
	Tone         string        `json:"tone,omitempty"` // info | warn | ok
	Title        LocalizedText `json:"title"`
	Body         LocalizedText `json:"body"`
	Group        string        `json:"group,omitempty"`
	Perspectives []string      `json:"perspectives,omitempty"`
}

// BlockSpec is one composable section in a Living Space.
type BlockSpec struct {
	ID           string        `json:"id"`
	Type         string        `json:"type"` // kpi_strip | timeline_accordion | comparison_table | checklist_ledger | callout_notes
	Title        LocalizedText `json:"title,omitempty"`
	DataSourceID string        `json:"dataSourceId,omitempty"`
	Perspectives []string      `json:"perspectives,omitempty"`
	// kpi_strip fields
	CountdownAt    string        `json:"countdownAt,omitempty"`
	CountdownLabel LocalizedText `json:"countdownLabel,omitempty"`
	ConfirmedCount int           `json:"confirmedCount,omitempty"`
	OpenCount      int           `json:"openCount,omitempty"`
	SkippedCount   int           `json:"skippedCount,omitempty"`
	ShowReadiness  bool          `json:"showReadiness,omitempty"`
	Metrics        []KPIMetric   `json:"metrics,omitempty"`
	// timeline_accordion fields
	Entries []TimelineEntry `json:"entries,omitempty"`
	// comparison_table fields
	Columns []LocalizedText `json:"columns,omitempty"`
	Rows    []TableRow      `json:"rows,omitempty"`
	// checklist_ledger fields
	Items []ChecklistRow `json:"items,omitempty"`
	// callout_notes fields
	Notes []CalloutNote `json:"notes,omitempty"`
}

// SpaceDoc is the canonical JSON source stored at <vault>/living_spaces/<slug>/space.json.
type SpaceDoc struct {
	SchemaVersion int               `json:"schemaVersion"`
	Slug          string            `json:"slug"`
	Title         LocalizedText     `json:"title"`
	Subtitle      LocalizedText     `json:"subtitle,omitempty"`
	GoalSlug      string            `json:"goalSlug,omitempty"`
	Timezone      string            `json:"timezone,omitempty"`
	Locales       []string          `json:"locales,omitempty"`
	LocaleLabels  map[string]string `json:"localeLabels,omitempty"`
	Perspectives  []PerspectiveSpec `json:"perspectives,omitempty"`
	Groups        []string          `json:"groups,omitempty"`
	DataSources   []DataSourceSpec  `json:"dataSources,omitempty"`
	Blocks        []BlockSpec       `json:"blocks,omitempty"`
	CreatedAt     string            `json:"createdAt"`
	UpdatedAt     string            `json:"updatedAt"`
}

// SpaceSummary is a lightweight directory listing entry for Console & `hq space list`.
type SpaceSummary struct {
	Slug      string   `json:"slug"`
	Title     string   `json:"title"`
	GoalSlug  string   `json:"goalSlug,omitempty"`
	Locales   []string `json:"locales,omitempty"`
	UpdatedAt string   `json:"updatedAt"`
	HasHTML   bool     `json:"hasHtml"`
}
