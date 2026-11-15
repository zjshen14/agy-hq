package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// ConnectorDoctorItem reports health or drift status for a connector (`hq connector doctor` & `hq doctor`).
type ConnectorDoctorItem struct {
	Name   string `json:"name"`
	Status string `json:"status"` // "PASS" | "WARN" | "FAIL"
	Detail string `json:"detail"`
}

type legacyGmailRegistry struct {
	DefaultAccount string `json:"defaultAccount"`
	Accounts       map[string]struct {
		Alias      string `json:"alias"`
		Email      string `json:"email"`
		AGYAccount string `json:"agyAccount"`
		TokenFile  string `json:"tokenFile"`
		CLIPath    string `json:"cliPath"`
		Query      string `json:"query"`
		Enabled    bool   `json:"enabled"`
	} `json:"accounts"`
}

type legacyGChatRegistry struct {
	DefaultAccount string `json:"defaultAccount"`
	Accounts       map[string]struct {
		Alias         string `json:"alias"`
		Email         string `json:"email"`
		Transport     string `json:"transport"`
		ReadTokenFile string `json:"readTokenFile"`
		SendTokenFile string `json:"sendTokenFile"`
		WebhookFile   string `json:"webhookFile"`
		OwnerSpaceID  string `json:"ownerSpaceId"`
		Enabled       bool   `json:"enabled"`
	} `json:"accounts"`
}

func strPtrOrNil(s string) *string {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func lookupLegacyGmailCLIPath(vaultDir string) string {
	if strings.TrimSpace(vaultDir) == "" {
		return ""
	}
	raw, err := os.ReadFile(filepath.Join(vaultDir, "private", "gmail", "accounts.json"))
	if err != nil {
		return ""
	}
	var reg legacyGmailRegistry
	if json.Unmarshal(raw, &reg) != nil {
		return ""
	}
	if def, ok := reg.Accounts[reg.DefaultAccount]; ok && strings.TrimSpace(def.CLIPath) != "" {
		return strings.TrimSpace(def.CLIPath)
	}
	for _, acc := range reg.Accounts {
		if strings.TrimSpace(acc.CLIPath) != "" {
			return strings.TrimSpace(acc.CLIPath)
		}
	}
	return ""
}

func synthesizeLegacyInstance(vaultDir, connID string) (InstanceConfig, bool) {
	switch connID {
	case "gmail":
		raw, err := os.ReadFile(filepath.Join(vaultDir, "private", "gmail", "accounts.json"))
		if err != nil {
			return InstanceConfig{}, false
		}
		var reg legacyGmailRegistry
		if json.Unmarshal(raw, &reg) != nil || len(reg.Accounts) == 0 {
			return InstanceConfig{}, false
		}
		ic := InstanceConfig{
			SchemaVersion:  ConnectorSchemaVersion,
			Connector:      "gmail",
			DefaultAccount: reg.DefaultAccount,
			Accounts:       make(map[string]AccountInstance, len(reg.Accounts)),
		}
		for alias, a := range reg.Accounts {
			en := a.Enabled
			tk := "cli"
			if strings.TrimSpace(a.TokenFile) != "" && strings.TrimSpace(a.CLIPath) == "" {
				tk = "direct_oauth"
			}
			ic.Accounts[alias] = AccountInstance{
				Email:         strings.TrimSpace(a.Email),
				AgyAccount:    strings.TrimSpace(a.AGYAccount),
				Enabled:       &en,
				TransportKind: tk,
				DefaultQuery:  strings.TrimSpace(a.Query),
				TokenRefs: TokenRefs{
					Read: strPtrOrNil(a.TokenFile),
					Send: strPtrOrNil(a.TokenFile),
				},
			}
		}
		return ic, true

	case "gchat":
		raw, err := os.ReadFile(filepath.Join(vaultDir, "private", "gchat", "accounts.json"))
		if err != nil {
			return InstanceConfig{}, false
		}
		var reg legacyGChatRegistry
		if json.Unmarshal(raw, &reg) != nil || len(reg.Accounts) == 0 {
			return InstanceConfig{}, false
		}
		ic := InstanceConfig{
			SchemaVersion:  ConnectorSchemaVersion,
			Connector:      "gchat",
			DefaultAccount: reg.DefaultAccount,
			Accounts:       make(map[string]AccountInstance, len(reg.Accounts)),
		}
		for alias, a := range reg.Accounts {
			en := a.Enabled
			var spaces []string
			if strings.TrimSpace(a.OwnerSpaceID) != "" {
				spaces = []string{strings.TrimSpace(a.OwnerSpaceID)}
			}
			tk := strings.TrimSpace(a.Transport)
			if tk == "" {
				tk = "cli"
			}
			ic.Accounts[alias] = AccountInstance{
				Email:         strings.TrimSpace(a.Email),
				Enabled:       &en,
				TransportKind: tk,
				WatchSpaces:   spaces,
				TokenRefs: TokenRefs{
					Read:    strPtrOrNil(a.ReadTokenFile),
					Send:    strPtrOrNil(a.SendTokenFile),
					Webhook: strPtrOrNil(a.WebhookFile),
				},
			}
		}
		return ic, true
	}
	return InstanceConfig{}, false
}

// MigrateLegacyAccounts non-destructively writes `<vault>/private/connectors/<id>/instance.json`
// from legacy `private/{gmail,gchat}/accounts.json` while keeping legacy files intact.
func MigrateLegacyAccounts(ctx context.Context, vaultDir, target string) ([]string, error) {
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "" {
		target = "all"
	}
	var targets []string
	switch target {
	case "all":
		targets = []string{"gmail", "gchat"}
	case "gmail", "gchat":
		targets = []string{target}
	default:
		return nil, fmt.Errorf("unsupported connector migration target %q (expected gmail, gchat, or all)", target)
	}

	var migrated []string
	for _, id := range targets {
		synth, ok := synthesizeLegacyInstance(vaultDir, id)
		if !ok {
			continue
		}
		if err := SaveInstance(ctx, vaultDir, synth); err != nil {
			return migrated, err
		}
		migrated = append(migrated, fmt.Sprintf("%s -> %s (%d account(s))", id, instanceFilePath(vaultDir, id), len(synth.Accounts)))
	}
	return migrated, nil
}

func fingerprintInstanceAccounts(ic InstanceConfig) string {
	var parts []string
	parts = append(parts, "default="+ic.DefaultAccount)
	keys := make([]string, 0, len(ic.Accounts))
	for k := range ic.Accounts {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := ic.Accounts[k]
		parts = append(parts, fmt.Sprintf("%s:%s:%v", k, strings.ToLower(a.Email), a.IsEnabled()))
	}
	return strings.Join(parts, "|")
}

// CheckDrift compares legacy `private/{gmail,gchat}/accounts.json` against `private/connectors/<id>/instance.json`
// when both exist, surfacing version-skew drift warnings in `hq doctor`.
func CheckDrift(vaultDir string) []ConnectorDoctorItem {
	var items []ConnectorDoctorItem
	for _, id := range []string{"gmail", "gchat"} {
		instPath := instanceFilePath(vaultDir, id)
		if _, err := os.Stat(instPath); err != nil {
			continue
		}
		synth, hasLegacy := synthesizeLegacyInstance(vaultDir, id)
		if !hasLegacy {
			continue
		}
		cur, err := LoadInstance(vaultDir, id)
		if err != nil {
			items = append(items, ConnectorDoctorItem{
				Name:   "connector.drift:" + id,
				Status: "FAIL",
				Detail: err.Error(),
			})
			continue
		}
		if fingerprintInstanceAccounts(synth) != fingerprintInstanceAccounts(cur) {
			items = append(items, ConnectorDoctorItem{
				Name:   "connector.drift:" + id,
				Status: "WARN",
				Detail: fmt.Sprintf("account drift detected between private/%s/accounts.json and private/connectors/%s/instance.json (re-run `hq connector migrate %s`)", id, id, id),
			})
		} else {
			items = append(items, ConnectorDoctorItem{
				Name:   "connector.drift:" + id,
				Status: "PASS",
				Detail: fmt.Sprintf("instance.json and legacy %s/accounts.json are in sync", id),
			})
		}
	}
	return items
}

// DoctorConnectors validates connector manifests, trust pins, CLI availability, and account drift.
func DoctorConnectors(vaultDir, filterID string) []ConnectorDoctorItem {
	filterID = strings.TrimSpace(filterID)
	conns, err := ListConnectors(vaultDir)
	if err != nil {
		return []ConnectorDoctorItem{{Name: "connector.list", Status: "FAIL", Detail: err.Error()}}
	}
	var items []ConnectorDoctorItem
	for _, rc := range conns {
		if filterID != "" && rc.ID != filterID && rc.FQID != filterID {
			continue
		}
		detail := fmt.Sprintf("fqid=%s source=%s accounts=%d participant_lock=%v",
			rc.FQID, rc.SourceID, len(rc.Instance.Accounts), rc.EffectiveParticipantLock(vaultDir))
		status := "PASS"
		if rc.Manifest.RequiresDesktopSession && os.Getenv("ANTIGRAVITY_CREDENTIAL_SCOPE") == "" {
			detail += " [paused (no active desktop conversation)]"
		}
		if rc.Manifest.Transport.CLI != nil && strings.TrimSpace(rc.Manifest.Transport.CLI.Path) != "" {
			binPath, bErr := resolveVerifiedCLIBinary(vaultDir, rc)
			if bErr != nil {
				status = "FAIL"
				detail += " — " + bErr.Error()
			} else {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				out, _, pErr := RunSubprocess(ctx, SubprocessSpec{
					Binary:   binPath,
					Args:     []string{"--version"},
					EnvAllow: rc.Manifest.EnvAllow,
					Timeout:  2 * time.Second,
				})
				cancel()
				if pErr == nil && len(strings.TrimSpace(string(out))) > 0 {
					first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
					detail += fmt.Sprintf(" (bin=%s: %s)", binPath, first)
				}
			}
		}
		items = append(items, ConnectorDoctorItem{
			Name:   "connector:" + rc.ID,
			Status: status,
			Detail: detail,
		})
	}
	items = append(items, CheckDrift(vaultDir)...)
	return items
}

// TestConnectorFixture verifies a connector's `jsonmap` output rules offline against sample files in `fixtureDir`
// (`poll.stdout`, `poll.json`, or `poll.ndjson`).
func TestConnectorFixture(vaultDir, connID, fixtureDir string) ([]NormalizedItem, error) {
	rc, err := Resolve(vaultDir, connID)
	if err != nil {
		return nil, err
	}
	var raw []byte
	var chosen string
	for _, name := range []string{"poll.stdout", "poll.json", "poll.ndjson", "list_messages.json"} {
		p := filepath.Join(fixtureDir, name)
		if b, rErr := os.ReadFile(p); rErr == nil {
			raw = b
			chosen = name
			break
		}
	}
	if chosen == "" {
		return nil, fmt.Errorf("no fixture output file (poll.stdout, poll.json, poll.ndjson, or list_messages.json) found in %s", fixtureDir)
	}

	var jm JSONMapSpec
	if rc.Manifest.Transport.CLI != nil {
		if vo, ok := rc.Manifest.Transport.CLI.Output["poll"]; ok && vo.JSONMap != nil {
			jm = *vo.JSONMap
			if jm.Format == "" {
				jm.Format = vo.Format
			}
		} else {
			keys := make([]string, 0, len(rc.Manifest.Transport.CLI.Output))
			for k := range rc.Manifest.Transport.CLI.Output {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			for _, k := range keys {
				vo := rc.Manifest.Transport.CLI.Output[k]
				if vo.JSONMap != nil {
					jm = *vo.JSONMap
					if jm.Format == "" {
						jm.Format = vo.Format
					}
					break
				}
			}
		}
	}
	if strings.HasSuffix(chosen, ".ndjson") && jm.Format == "" {
		jm.Format = "ndjson"
	}
	if jm.ItemID == "" {
		jm.ItemID = "/id"
	}
	items, _, _, err := MapCLIOutput(raw, jm)
	return items, err
}
