package connector

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/templates"
)

const ConnectorSchemaVersion = 1

var (
	reValidConnID        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,62}$`)
	reFlagTemplate       = regexp.MustCompile(`^--[a-zA-Z0-9][a-zA-Z0-9_-]*=\{\{(\?|\*)?([a-zA-Z0-9_]+)\}\}$`)
	rePositionalTemplate = regexp.MustCompile(`^\{\{(\??)([a-z][a-z0-9_]{0,31})\}\}$`)
	reStrictScalarVal    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+=-]{0,127}$`)
	reRelaxedQueryText   = regexp.MustCompile(`^[A-Za-z0-9\[\](][A-Za-z0-9._:@/+=, ()'"?!#&\[\]-]{0,255}$`)
)

var forbiddenShellBasenames = map[string]bool{
	"sh":      true,
	"bash":    true,
	"zsh":     true,
	"dash":    true,
	"ksh":     true,
	"ssh":     true,
	"env":     true,
	"xargs":   true,
	"node":    true,
	"perl":    true,
	"ruby":    true,
	"python":  true,
	"python2": true,
	"python3": true,
}

// DriverSpec configures either a built-in driver ("gmail", "gchat", "gcalendar", "jsonmap")
// or an external `hq-connector-protocol/v1` command.
type DriverSpec struct {
	Builtin string   `json:"builtin,omitempty"`
	Command []string `json:"command,omitempty"`
}

// PredicateSpec filters mapped JSON objects before emitting NormalizedItems.
type PredicateSpec struct {
	Pointer string `json:"pointer"`
	Op      string `json:"op"` // "eq" | "neq" | "contains" | "nonempty"
	Value   string `json:"value,omitempty"`
}

// JSONMapSpec defines RFC 6901 JSON-Pointer mappings for read-only CLI JSON/NDJSON outputs.
type JSONMapSpec struct {
	Format            string              `json:"format,omitempty"` // "json" | "ndjson"
	ItemsPointer      string              `json:"items_pointer,omitempty"`
	NextCursorPointer string              `json:"next_cursor_pointer,omitempty"`
	NextPagePointer   string              `json:"next_page_pointer,omitempty"`
	ItemID            string              `json:"item_id,omitempty"`
	ThreadKey         string              `json:"thread_key,omitempty"`
	Timestamp         string              `json:"timestamp,omitempty"`
	Sender            string              `json:"sender,omitempty"`
	SubjectOrRoom     string              `json:"subject_or_room,omitempty"`
	Permalink         string              `json:"permalink,omitempty"`
	Body              string              `json:"body,omitempty"`
	Participants      string              `json:"participants,omitempty"`
	Mentions          string              `json:"mentions,omitempty"`
	IsDirect          string              `json:"is_direct,omitempty"`
	IsAutomated       string              `json:"is_automated,omitempty"`
	HasSchedule       string              `json:"has_schedule,omitempty"`
	Transforms        map[string][]string `json:"transforms,omitempty"`
	Predicates        []PredicateSpec     `json:"predicates,omitempty"`
}

// VerbOutputSpec configures output parsing for a CLI verb.
type VerbOutputSpec struct {
	Format  string       `json:"format,omitempty"`
	JSONMap *JSONMapSpec `json:"jsonmap,omitempty"`
}

// CLITransportSpec defines the CLI binary, auto-install policy, verbs, and output mappers.
type CLITransportSpec struct {
	Path              string                    `json:"path,omitempty"`
	AutoInstallPlugin bool                      `json:"auto_install_plugin,omitempty"`
	Verbs             map[string][]string       `json:"verbs,omitempty"`
	Output            map[string]VerbOutputSpec `json:"output,omitempty"`
}

// TransportSpec selects how a connector talks to its backing service.
type TransportSpec struct {
	Kind string            `json:"kind,omitempty"` // "cli" | "direct_oauth" | "stdio"
	CLI  *CLITransportSpec `json:"cli,omitempty"`
}

// QuerySpec declares a read-only context query (`hq watch query`) and its taint sensitivity class.
type QuerySpec struct {
	Verb          string   `json:"verb"`
	Sensitivity   string   `json:"sensitivity"` // "low" | "medium" | "high"
	AllowedParams []string `json:"allowed_params,omitempty"`
	ProjectFields []string `json:"project_fields,omitempty"`
}

// PollCapSpec configures 0-token background polling limits.
type PollCapSpec struct {
	Enabled  bool `json:"enabled"`
	MaxPages int  `json:"max_pages,omitempty"`
	PageSize int  `json:"page_size,omitempty"`
}

// ToggleCapSpec enables or disables a boolean capability.
type ToggleCapSpec struct {
	Enabled bool `json:"enabled"`
}

// BriefCapSpec configures Morning Brief participation for a connector.
type BriefCapSpec struct {
	Enabled     bool   `json:"enabled"`
	Quarantined *bool  `json:"quarantined,omitempty"` // defaults to true
	TimeoutMs   int    `json:"timeout_ms,omitempty"`
	Section     string `json:"section,omitempty"`
}

// OfferActionSpec defines an approval-bound outbound action (`hq offer stage`).
type OfferActionSpec struct {
	Verb                string            `json:"verb,omitempty"`
	DeliveryMode        string            `json:"delivery_mode,omitempty"` // "draft_only" | "direct_send"
	RecipientParams     map[string]string `json:"recipient_params,omitempty"`
	SupportsWaitOn      bool              `json:"supports_wait_on,omitempty"`
	RequiresReplyThread *bool             `json:"requires_reply_thread,omitempty"`
}

// CapabilitiesSpec declares all supported operations on a ConnectorManifest.
type CapabilitiesSpec struct {
	Poll            *PollCapSpec               `json:"poll,omitempty"`
	InspectThread   *ToggleCapSpec             `json:"inspect_thread,omitempty"`
	ReadQuarantined *ToggleCapSpec             `json:"read_quarantined,omitempty"`
	Brief           *BriefCapSpec              `json:"brief,omitempty"`
	Offers          map[string]OfferActionSpec `json:"offers,omitempty"`
}

// ConnectorManifest represents pinned executable connector metadata (`connector.json`).
type ConnectorManifest struct {
	SchemaVersion          int                  `json:"schema_version"`
	ID                     string               `json:"id"`
	Description            string               `json:"description,omitempty"`
	ParticipantLock        *bool                `json:"participant_lock,omitempty"`     // fail-closed default: true
	AllowTaintedCreate     *bool                `json:"allow_tainted_create,omitempty"` // fail-closed default: false
	RequiresDesktopSession bool                 `json:"requires_desktop_session,omitempty"`
	EnvAllow               []string             `json:"env_allow,omitempty"`
	Driver                 DriverSpec           `json:"driver"`
	Transport              TransportSpec        `json:"transport,omitempty"`
	Queries                map[string]QuerySpec `json:"queries,omitempty"`
	Capabilities           CapabilitiesSpec     `json:"capabilities,omitempty"`
}

// TokenRefs stores Vault-relative or absolute paths to OAuth/webhook token files (`0600`).
type TokenRefs struct {
	Read    *string `json:"read"`
	Send    *string `json:"send"`
	Webhook *string `json:"webhook"`
}

// AccountInstance holds pure data configuration for a single connector account in `instance.json`
// (strictly forbidding executable `path`, `argv`, `verbs`, or `env` keys).
type AccountInstance struct {
	Email         string    `json:"email,omitempty"`
	AgyAccount    string    `json:"agy_account,omitempty"`
	Enabled       *bool     `json:"enabled,omitempty"`
	TransportKind string    `json:"transport_kind,omitempty"` // "cli" | "direct_oauth" | "oauth" | "webhook" | "cdp" | "gmail-bridge"
	DefaultQuery  string    `json:"default_query,omitempty"`
	WatchSpaces   []string  `json:"watch_spaces,omitempty"`
	DeliveryMode  string    `json:"delivery_mode,omitempty"`
	TokenRefs     TokenRefs `json:"token_refs,omitempty"`
}

// IsEnabled returns true unless Enabled is explicitly false.
func (a AccountInstance) IsEnabled() bool {
	if a.Enabled != nil {
		return *a.Enabled
	}
	return true
}

// InstanceConfig represents `<vault>/private/connectors/<id>/instance.json` (`0600`).
type InstanceConfig struct {
	SchemaVersion  int                        `json:"schema_version"`
	Connector      string                     `json:"connector"`
	DefaultAccount string                     `json:"default_account,omitempty"`
	Enabled        *bool                      `json:"enabled,omitempty"`
	Accounts       map[string]AccountInstance `json:"accounts"`
}

// IsEnabled returns true unless Enabled is explicitly false.
func (ic InstanceConfig) IsEnabled() bool {
	if ic.Enabled != nil {
		return *ic.Enabled
	}
	return true
}

// ResolvedConnector bundles a trust-verified ConnectorManifest with its Vault InstanceConfig.
type ResolvedConnector struct {
	ID       string            `json:"id"`
	FQID     string            `json:"fqid"`
	SourceID string            `json:"source_id"` // "vault" | "pack:<id>" | "builtin"
	Path     string            `json:"path,omitempty"`
	SHA256   string            `json:"sha256"`
	Manifest ConnectorManifest `json:"manifest"`
	Instance InstanceConfig    `json:"instance"`
}

// EffectiveParticipantLock returns whether RFC 5322 / channel participant lock is enforced (default true).
func (rc ResolvedConnector) EffectiveParticipantLock(vaultDir string) bool {
	if rc.Manifest.ParticipantLock == nil || *rc.Manifest.ParticipantLock {
		if loosened, _ := extension.IsSecurityLoosened(vaultDir, rc.SourceID, rc.ID+".participant_lock", rc.SHA256); loosened {
			return false
		}
		return true
	}
	if strings.HasPrefix(rc.SourceID, "pack:") {
		return true
	}
	if loosened, _ := extension.IsSecurityLoosened(vaultDir, rc.SourceID, rc.ID+".participant_lock", rc.SHA256); loosened {
		return false
	}
	return true
}

// EffectiveAllowTaintedCreate returns whether tainted sessions may create new outbound threads (default false).
func (rc ResolvedConnector) EffectiveAllowTaintedCreate(vaultDir string) bool {
	if rc.Manifest.AllowTaintedCreate == nil || !*rc.Manifest.AllowTaintedCreate {
		return false
	}
	if strings.HasPrefix(rc.SourceID, "pack:") {
		return false
	}
	loosened, _ := extension.IsSecurityLoosened(vaultDir, rc.SourceID, rc.ID+".allow_tainted_create_ack", rc.SHA256)
	return loosened
}

// EffectiveBriefQuarantined returns whether Morning Brief outputs from this connector are quarantined (default true).
func (rc ResolvedConnector) EffectiveBriefQuarantined(vaultDir string) bool {
	if rc.Manifest.Capabilities.Brief == nil || rc.Manifest.Capabilities.Brief.Quarantined == nil || *rc.Manifest.Capabilities.Brief.Quarantined {
		if loosened, _ := extension.IsSecurityLoosened(vaultDir, rc.SourceID, rc.ID+".brief.quarantined", rc.SHA256); loosened {
			return false
		}
		return true
	}
	if strings.HasPrefix(rc.SourceID, "pack:") {
		return true
	}
	loosened, _ := extension.IsSecurityLoosened(vaultDir, rc.SourceID, rc.ID+".brief.quarantined", rc.SHA256)
	return !loosened
}

func isForbiddenShellBinary(rawBin string) bool {
	base := strings.ToLower(filepath.Base(strings.TrimSpace(rawBin)))
	if forbiddenShellBasenames[base] {
		return true
	}
	return strings.HasPrefix(base, "python3.")
}

// ParseManifestStrict decodes `connector.json` with `DisallowUnknownFields()` and validates core invariants.
func ParseManifestStrict(raw []byte) (ConnectorManifest, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var m ConnectorManifest
	if err := dec.Decode(&m); err != nil {
		return ConnectorManifest{}, fmt.Errorf("strict connector.json decode failed: %w", err)
	}
	if m.SchemaVersion != ConnectorSchemaVersion {
		return ConnectorManifest{}, fmt.Errorf("unsupported connector schema_version %d (expected %d)", m.SchemaVersion, ConnectorSchemaVersion)
	}
	m.ID = strings.TrimSpace(m.ID)
	if !reValidConnID.MatchString(m.ID) {
		return ConnectorManifest{}, fmt.Errorf("invalid connector id %q", m.ID)
	}
	if strings.TrimSpace(m.Driver.Builtin) == "" && len(m.Driver.Command) == 0 {
		return ConnectorManifest{}, fmt.Errorf("connector %q must specify either driver.builtin or driver.command", m.ID)
	}
	return m, nil
}

// LintManifest performs full static security validation on `connector.json` (`hq connector lint`).
func LintManifest(raw []byte, sourceID string) (ConnectorManifest, error) {
	m, err := ParseManifestStrict(raw)
	if err != nil {
		return ConnectorManifest{}, err
	}
	isPack := strings.HasPrefix(sourceID, "pack:")
	if isPack {
		if m.ParticipantLock != nil && !*m.ParticipantLock {
			return ConnectorManifest{}, fmt.Errorf("connector %q sets participant_lock=false (forbidden in companion pack %s)", m.ID, sourceID)
		}
		if m.AllowTaintedCreate != nil && *m.AllowTaintedCreate {
			return ConnectorManifest{}, fmt.Errorf("connector %q sets allow_tainted_create=true (forbidden in companion pack %s)", m.ID, sourceID)
		}
		if m.Capabilities.Brief != nil && m.Capabilities.Brief.Quarantined != nil && !*m.Capabilities.Brief.Quarantined {
			return ConnectorManifest{}, fmt.Errorf("connector %q sets capabilities.brief.quarantined=false (forbidden in companion pack %s)", m.ID, sourceID)
		}
	}

	hasPlaceholders := false
	if m.Transport.CLI != nil {
		for verbName, tmpl := range m.Transport.CLI.Verbs {
			if len(tmpl) == 0 {
				return ConnectorManifest{}, fmt.Errorf("connector %q verb %q has empty argv template", m.ID, verbName)
			}
			afterEndOfOptions := false
			for _, tok := range tmpl {
				if tok == "--" && !afterEndOfOptions {
					afterEndOfOptions = true
					continue
				}
				if strings.Contains(tok, "{{") || strings.Contains(tok, "}}") {
					hasPlaceholders = true
					if afterEndOfOptions {
						pm := rePositionalTemplate.FindStringSubmatch(tok)
						if pm == nil || pm[2] == "payload_file" {
							return ConnectorManifest{}, fmt.Errorf("connector %q verb %q token %q violates post--- positional template rule (expected {{param}} or {{?opt}})", m.ID, verbName, tok)
						}
					} else if !reFlagTemplate.MatchString(tok) {
						return ConnectorManifest{}, fmt.Errorf("connector %q verb %q token %q violates fused-flag template rule (expected --flag={{param}}, --flag={{?opt}}, --flag={{*list}}, or --flag={{payload_file}})", m.ID, verbName, tok)
					}
				}
			}
		}
		if hasPlaceholders && strings.TrimSpace(m.Transport.CLI.Path) != "" && isForbiddenShellBinary(m.Transport.CLI.Path) {
			return ConnectorManifest{}, fmt.Errorf("connector %q cli.path %q uses forbidden shell/interpreter basename with {{param}} templates", m.ID, m.Transport.CLI.Path)
		}
	}
	if len(m.Driver.Command) > 0 && isForbiddenShellBinary(m.Driver.Command[0]) {
		for _, tok := range m.Driver.Command[1:] {
			if strings.Contains(tok, "{{") {
				return ConnectorManifest{}, fmt.Errorf("connector %q driver.command[0] %q uses forbidden shell/interpreter basename with {{param}} templates", m.ID, m.Driver.Command[0])
			}
		}
	}
	return m, nil
}

// ParseInstanceStrict decodes `<vault>/private/connectors/<id>/instance.json` with `DisallowUnknownFields()`.
func ParseInstanceStrict(raw []byte) (InstanceConfig, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var ic InstanceConfig
	if err := dec.Decode(&ic); err != nil {
		return InstanceConfig{}, fmt.Errorf("strict instance.json decode failed (executable keys like path/argv/env are forbidden in instance.json): %w", err)
	}
	if ic.SchemaVersion == 0 {
		ic.SchemaVersion = ConnectorSchemaVersion
	}
	if ic.Accounts == nil {
		ic.Accounts = make(map[string]AccountInstance)
	}
	for alias, acc := range ic.Accounts {
		tk := strings.TrimSpace(acc.TransportKind)
		if tk != "" {
			switch tk {
			case "cli", "direct_oauth", "oauth", "webhook", "cdp", "gmail-bridge", "mock":
			default:
				return InstanceConfig{}, fmt.Errorf("account %q in instance.json has invalid transport_kind %q", alias, tk)
			}
		}
	}
	return ic, nil
}

func instanceFilePath(vaultDir, connID string) string {
	return filepath.Join(vaultDir, "private", "connectors", connID, "instance.json")
}

func instanceLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "connectors", ".instance.lock")
}

// LoadInstance loads `<vault>/private/connectors/<id>/instance.json` if present, falling back to non-destructive
// in-memory synthesis from legacy `private/{gmail,gchat}/accounts.json` during the dual-read window.
func LoadInstance(vaultDir, connID string) (InstanceConfig, error) {
	connID = strings.TrimSpace(connID)
	ic := InstanceConfig{
		SchemaVersion: ConnectorSchemaVersion,
		Connector:     connID,
		Accounts:      make(map[string]AccountInstance),
	}
	if strings.TrimSpace(vaultDir) == "" || connID == "" {
		return ic, nil
	}
	p := instanceFilePath(vaultDir, connID)
	st, err := os.Lstat(p)
	if err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return ic, fmt.Errorf("refusing non-regular or symlinked instance.json %s", p)
		}
		raw, rErr := os.ReadFile(p)
		if rErr != nil {
			return ic, rErr
		}
		return ParseInstanceStrict(raw)
	}
	if !os.IsNotExist(err) {
		return ic, err
	}
	if synth, ok := synthesizeLegacyInstance(vaultDir, connID); ok {
		return synth, nil
	}
	return ic, nil
}

// SaveInstance atomically writes `<vault>/private/connectors/<id>/instance.json` (`0600`) under RankExtensions.
func SaveInstance(ctx context.Context, vaultDir string, ic InstanceConfig) error {
	connID := strings.TrimSpace(ic.Connector)
	if strings.TrimSpace(vaultDir) == "" || !reValidConnID.MatchString(connID) {
		return fmt.Errorf("valid vaultDir and connector id are required")
	}
	ic.SchemaVersion = ConnectorSchemaVersion
	rawCheck, err := json.MarshalIndent(ic, "", "  ")
	if err != nil {
		return err
	}
	if _, vErr := ParseInstanceStrict(rawCheck); vErr != nil {
		return vErr
	}
	return store.With(ctx, store.RankExtensions, instanceLockPath(vaultDir), func(_ context.Context) error {
		p := instanceFilePath(vaultDir, connID)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			return err
		}
		return store.WriteAtomic(p, append(rawCheck, '\n'), 0o600)
	})
}

func validateParamValue(paramName, val string) error {
	if val == "" {
		return fmt.Errorf("empty value for required parameter %q", paramName)
	}
	if val[0] == '-' || val[0] == '@' {
		return fmt.Errorf("parameter %q value %q cannot start with '-' or '@'", paramName, val)
	}
	if strings.ContainsAny(val, "\n\r\x00") {
		return fmt.Errorf("parameter %q contains forbidden control characters", paramName)
	}
	if paramName == "query" || paramName == "subject" {
		if !reRelaxedQueryText.MatchString(val) {
			return fmt.Errorf("parameter %q value %q failed safe query/subject character policy", paramName, val)
		}
		return nil
	}
	if paramName == "to" || paramName == "cc" {
		addrs, err := ParseRFC5322Emails(val)
		if err != nil || len(addrs) == 0 {
			return fmt.Errorf("parameter %q value %q failed RFC 5322 email validation", paramName, val)
		}
		return nil
	}
	if !reStrictScalarVal.MatchString(val) {
		return fmt.Errorf("parameter %q value %q failed strict identifier validation", paramName, val)
	}
	return nil
}

// ExpandVerbArgv expands a validated verb template (`--flag={{param}}`, `--flag={{?opt}}`, `--flag={{*list}}`,
// `--flag={{payload_file}}`, or post-`--` positional `{{param}}` / `{{?opt}}`) into a concrete literal argv slice without invoking any shell.
func ExpandVerbArgv(tmpl []string, scalarParams map[string]string, listParams map[string][]string, payloadFile string) ([]string, error) {
	var out []string
	afterEndOfOptions := false
	pendingEndOfOptions := false
	emit := func(arg string) {
		if pendingEndOfOptions {
			out = append(out, "--")
			pendingEndOfOptions = false
		}
		out = append(out, arg)
	}
	for _, tok := range tmpl {
		if tok == "--" && !afterEndOfOptions {
			afterEndOfOptions = true
			pendingEndOfOptions = true
			continue
		}
		if !strings.Contains(tok, "{{") && !strings.Contains(tok, "}}") {
			emit(tok)
			continue
		}
		if afterEndOfOptions {
			pm := rePositionalTemplate.FindStringSubmatch(tok)
			if pm == nil || pm[2] == "payload_file" {
				return nil, fmt.Errorf("invalid post--- positional template token %q", tok)
			}
			val := strings.TrimSpace(scalarParams[pm[2]])
			if pm[1] == "?" && val == "" {
				continue
			}
			if err := validateParamValue(pm[2], val); err != nil {
				return nil, err
			}
			emit(val)
			continue
		}
		m := reFlagTemplate.FindStringSubmatch(tok)
		if m == nil {
			return nil, fmt.Errorf("invalid verb template token %q", tok)
		}
		eqIdx := strings.IndexByte(tok, '=')
		flagPrefix := tok[:eqIdx+1] // e.g. "--space="
		modifier := m[1]            // "", "?", or "*"
		key := m[2]

		if key == "payload_file" {
			if strings.TrimSpace(payloadFile) == "" {
				return nil, fmt.Errorf("verb requires {{payload_file}} pipe")
			}
			emit(flagPrefix + payloadFile)
			continue
		}

		switch modifier {
		case "?":
			val := strings.TrimSpace(scalarParams[key])
			if val == "" {
				continue // drop optional empty flag cleanly
			}
			if err := validateParamValue(key, val); err != nil {
				return nil, err
			}
			emit(flagPrefix + val)
		case "*":
			items := listParams[key]
			for _, item := range items {
				item = strings.TrimSpace(item)
				if item == "" {
					continue
				}
				if err := validateParamValue(key, item); err != nil {
					return nil, err
				}
				emit(flagPrefix + item)
			}
		default:
			val := strings.TrimSpace(scalarParams[key])
			if err := validateParamValue(key, val); err != nil {
				return nil, err
			}
			emit(flagPrefix + val)
		}
	}
	return out, nil
}

// Resolve resolves connector `rawID` (accepting bare `<id>`, `core/<id>`, `vault/<id>`, or `<pack>/<id>`)
// across `<vault>/connectors/` -> enabled & trusted companion packs -> embedded `templates/connectors/`,
// verifying SHA-256 trust pins before returning.
func Resolve(vaultDir, rawID string) (ResolvedConnector, error) {
	trimmed := strings.TrimSpace(rawID)
	if trimmed == "" {
		return ResolvedConnector{}, fmt.Errorf("connector ID is required")
	}
	wantNamespace := ""
	connID := trimmed
	if strings.Contains(trimmed, "/") {
		parts := strings.SplitN(trimmed, "/", 2)
		wantNamespace, connID = parts[0], parts[1]
	}
	if !reValidConnID.MatchString(connID) {
		return ResolvedConnector{}, fmt.Errorf("invalid connector ID %q", rawID)
	}

	inst, err := LoadInstance(vaultDir, connID)
	if err != nil {
		return ResolvedConnector{}, err
	}

	// 1. Vault local override: <vault>/connectors/<id>/connector.json (must be trusted under "vault")
	if (wantNamespace == "" || wantNamespace == "vault") && strings.TrimSpace(vaultDir) != "" {
		vaultPath := filepath.Join(vaultDir, "connectors", connID, "connector.json")
		if st, sErr := os.Stat(vaultPath); sErr == nil && !st.IsDir() {
			sha, vErr := extension.VerifyTrustedFile(vaultDir, "vault", vaultPath)
			if vErr != nil {
				return ResolvedConnector{}, vErr
			}
			raw, rErr := os.ReadFile(vaultPath)
			if rErr != nil {
				return ResolvedConnector{}, rErr
			}
			m, lErr := LintManifest(raw, "vault")
			if lErr != nil {
				return ResolvedConnector{}, lErr
			}
			return ResolvedConnector{
				ID:       m.ID,
				FQID:     "vault/" + m.ID,
				SourceID: "vault",
				Path:     vaultPath,
				SHA256:   sha,
				Manifest: m,
				Instance: inst,
			}, nil
		}
	}

	// 2. Enabled & trusted companion packs (`hq-extension.json` components.connectors / connector_defaults)
	if wantNamespace != "core" && wantNamespace != "vault" && strings.TrimSpace(vaultDir) != "" {
		extSt, _ := extension.LoadVaultState(vaultDir)
		packs := extension.LoadActiveTrustedPacks(vaultDir)
		winnerPack := strings.TrimSpace(extSt.WinnerOverrides[connID])
		for _, dp := range packs {
			if wantNamespace != "" && dp.ID != wantNamespace {
				continue
			}
			if wantNamespace == "" && winnerPack != "" && dp.ID != winnerPack {
				continue
			}
			var relCandidates []string
			relCandidates = append(relCandidates, dp.Manifest.Components.Connectors...)
			if defRel, ok := dp.Manifest.Components.ConnectorDefaults[connID]; ok && defRel != "" {
				relCandidates = append(relCandidates, defRel)
			}
			for _, rel := range relCandidates {
				full := filepath.Join(dp.Path, filepath.FromSlash(rel))
				raw, rErr := os.ReadFile(full)
				if rErr != nil {
					continue
				}
				m, lErr := LintManifest(raw, "pack:"+dp.ID)
				if lErr != nil || m.ID != connID {
					continue
				}
				sha, vErr := extension.VerifyTrustedFile(vaultDir, "pack:"+dp.ID, full)
				if vErr != nil {
					continue
				}
				return ResolvedConnector{
					ID:       m.ID,
					FQID:     dp.ID + "/" + m.ID,
					SourceID: "pack:" + dp.ID,
					Path:     full,
					SHA256:   sha,
					Manifest: m,
					Instance: inst,
				}, nil
			}
		}
	}

	// 3. Embedded reference connectors (`templates/connectors/<id>/connector.json`)
	if wantNamespace == "" || wantNamespace == "core" {
		embPath := filepath.ToSlash(filepath.Join("connectors", connID, "connector.json"))
		if raw, eErr := fs.ReadFile(templates.FS, embPath); eErr == nil {
			m, lErr := LintManifest(raw, "builtin")
			if lErr != nil {
				return ResolvedConnector{}, lErr
			}
			sum := sha256.Sum256(raw)
			return ResolvedConnector{
				ID:       m.ID,
				FQID:     "core/" + m.ID,
				SourceID: "builtin",
				SHA256:   hex.EncodeToString(sum[:]),
				Manifest: m,
				Instance: inst,
			}, nil
		}
	}

	return ResolvedConnector{}, fmt.Errorf("connector %q not found", rawID)
}

// ListConnectors enumerates all resolvable connectors across Vault, enabled trusted packs, and embedded builtins.
func ListConnectors(vaultDir string) ([]ResolvedConnector, error) {
	ids := map[string]bool{
		"gmail":     true,
		"gchat":     true,
		"gcalendar": true,
	}
	if strings.TrimSpace(vaultDir) != "" {
		if entries, err := os.ReadDir(filepath.Join(vaultDir, "connectors")); err == nil {
			for _, e := range entries {
				if e.IsDir() && reValidConnID.MatchString(e.Name()) {
					ids[e.Name()] = true
				}
			}
		}
		for _, dp := range extension.LoadActiveTrustedPacks(vaultDir) {
			for _, rel := range dp.Manifest.Components.Connectors {
				if raw, err := os.ReadFile(filepath.Join(dp.Path, filepath.FromSlash(rel))); err == nil {
					if m, pErr := ParseManifestStrict(raw); pErr == nil {
						ids[m.ID] = true
					}
				}
			}
			for k := range dp.Manifest.Components.ConnectorDefaults {
				if reValidConnID.MatchString(k) {
					ids[k] = true
				}
			}
		}
	}
	sorted := make([]string, 0, len(ids))
	for id := range ids {
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	var out []ResolvedConnector
	for _, id := range sorted {
		if rc, err := Resolve(vaultDir, id); err == nil {
			out = append(out, rc)
		}
	}
	return out, nil
}
