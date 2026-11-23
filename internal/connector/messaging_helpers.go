package connector

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/store"
)

var (
	reSafeDraftID      = regexp.MustCompile(`^[A-Za-z0-9._\-]{1,128}$`)
	reValidTriageBatch = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// MockDir returns the configured mock directory via envVar or <vaultDir>/memory/<subDir>/mock when present.
func MockDir(vaultDir, envVar, subDir string) string {
	if envVar != "" {
		if env := strings.TrimSpace(os.Getenv(envVar)); env != "" {
			return env
		}
	}
	if vaultDir != "" && subDir != "" {
		candidate := filepath.Join(vaultDir, "memory", subDir, "mock")
		if st, err := os.Stat(candidate); err == nil && st.IsDir() {
			return candidate
		}
	}
	return ""
}

// FindHostPluginBinary probes PATH, plugin_data, Antigravity plugins dir, and system bin dirs for binName.
func FindHostPluginBinary(vaultDir, binName string) string {
	if binName == "" {
		return ""
	}
	if lp, err := exec.LookPath(binName); err == nil && lp != "" {
		return lp
	}
	roots := hostpaths.Resolve(vaultDir)
	if roots.AppDataDir != "" {
		matches, _ := filepath.Glob(filepath.Join(roots.AppDataDir, "plugin_data", binName, "bin", binName+"-*"))
		if len(matches) > 0 {
			return matches[0]
		}
	}
	if roots.PluginsDir != "" {
		launcher := filepath.Join(roots.PluginsDir, binName, "skills", binName, "scripts", binName)
		if st, err := os.Stat(launcher); err == nil && !st.IsDir() {
			return launcher
		}
	}
	sysMatches, _ := filepath.Glob("/usr/share/antigravity/resources/app/extensions/antigravity/bin/" + binName + "-*")
	if len(sysMatches) > 0 {
		return sysMatches[0]
	}
	return ""
}

// ValidateSafeID validates an identifier against path traversal and safe filename constraints.
func ValidateSafeID(kind, raw string) (string, error) {
	id := strings.TrimSpace(raw)
	if id == "" || strings.HasPrefix(id, ".") || strings.Contains(id, "..") || !reSafeDraftID.MatchString(id) {
		return "", fmt.Errorf("invalid %s %q", kind, raw)
	}
	return id, nil
}

// ValidateDraftID validates draftID against path traversal and safe filename constraints.
func ValidateDraftID(channel, draftID string) (string, error) {
	return ValidateSafeID(channel+" draft ID", draftID)
}

// VerifyDraftSendPreconditions validates the hash prefix length, active account ownership, and contentHash match.
func VerifyDraftSendPreconditions(channel, activeEmail, draftFrom, draftContentHash, expectedHashPrefix string) error {
	normHash := strings.ToLower(strings.TrimSpace(expectedHashPrefix))
	if len(normHash) < 8 {
		return fmt.Errorf("%s draft contentHash prefix required (minimum 8 hex characters)", channel)
	}
	if !strings.EqualFold(draftFrom, activeEmail) {
		return fmt.Errorf("account mismatch: draft From=%s != active account %s", draftFrom, activeEmail)
	}
	if !strings.HasPrefix(strings.ToLower(draftContentHash), normHash) {
		curPrefix := draftContentHash
		if len(curPrefix) > 12 {
			curPrefix = curPrefix[:12]
		}
		return fmt.Errorf("%s draft contentHash mismatch (bait-and-switch blocked): current=%s, approved=%s", channel, curPrefix, expectedHashPrefix)
	}
	return nil
}

// AccountWatermark tracks per-account sidecar polling state in <vaultDir>/memory/<channel>/state-<alias>.json.
type AccountWatermark struct {
	Account        string          `json:"account"`
	LastCheckedAt  string          `json:"lastCheckedAt"`
	LastHistoryID  string          `json:"lastHistoryId,omitempty"`
	SeenMessageIDs map[string]bool `json:"seenMessageIds"`
}

func accountWatermarkPath(vaultDir, channel, alias string) string {
	return filepath.Join(vaultDir, "memory", channel, fmt.Sprintf("state-%s.json", alias))
}

// LoadAccountWatermark reads <vaultDir>/memory/<channel>/state-<alias>.json, initializing SeenMessageIDs if missing.
func LoadAccountWatermark(vaultDir, channel, alias string) AccountWatermark {
	cleanChannel, chErr := ValidateSafeID("channel", channel)
	cleanAlias, alErr := ValidateSafeID("account alias", alias)
	st := AccountWatermark{
		Account:        cleanAlias,
		SeenMessageIDs: make(map[string]bool),
	}
	if chErr != nil || alErr != nil {
		st.Account = alias
		return st
	}
	if loaded, err := store.ReadJSONFile[AccountWatermark](accountWatermarkPath(vaultDir, cleanChannel, cleanAlias)); err == nil {
		st = loaded
		if st.Account == "" {
			st.Account = cleanAlias
		}
	}
	if st.SeenMessageIDs == nil {
		st.SeenMessageIDs = make(map[string]bool)
	}
	return st
}

// SaveAccountWatermark atomically writes st to <vaultDir>/memory/<channel>/state-<alias>.json with 0644 permissions.
func SaveAccountWatermark(vaultDir, channel string, st AccountWatermark) error {
	cleanChannel, err := ValidateSafeID("channel", channel)
	if err != nil {
		return err
	}
	cleanAlias, err := ValidateSafeID("account alias", st.Account)
	if err != nil {
		return err
	}
	st.Account = cleanAlias
	return store.WriteJSONAtomic(accountWatermarkPath(vaultDir, cleanChannel, cleanAlias), st, 0o644)
}

// FilterMatchingAccounts returns sorted account aliases that are enabled and match filter (by alias or email).
func FilterMatchingAccounts[T any](accounts map[string]T, filter string, isEnabled func(T) bool, emailOf func(T) string) []string {
	var aliases []string
	for k, cfg := range accounts {
		if !isEnabled(cfg) {
			continue
		}
		if filter != "" && !strings.EqualFold(filter, k) && !strings.EqualFold(filter, emailOf(cfg)) {
			continue
		}
		aliases = append(aliases, k)
	}
	sort.Strings(aliases)
	return aliases
}

// ValidateTriageBatchID validates a triage batch identifier against path traversal and safe filename rules.
func ValidateTriageBatchID(batchID string) error {
	if !reValidTriageBatch.MatchString(batchID) || strings.Contains(batchID, "..") || strings.ContainsAny(batchID, `/\`) {
		return fmt.Errorf("invalid triage batch id %q (must match ^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$ without '..')", batchID)
	}
	return nil
}

// TriageDigestEntry holds channel-agnostic fields needed to format a triage markdown digest entry.
type TriageDigestEntry struct {
	Urgency    string
	Category   string
	TargetID   string
	Title      string
	SenderLab  string
	Sender     string
	Summary    string
	DraftID    string
	ProposalID string
}

// WriteTriageResults validates batchID, persists rawItems to memory/<channel>/triage/<batchID>.json,
// and writes a human-readable markdown digest to notes/inbox/<batchID>.md.
func WriteTriageResults(vaultDir, channel, defaultPrefix, digestHeading, batchID string, rawItems any, entries []TriageDigestEntry) error {
	cleanChannel, err := ValidateSafeID("triage channel", channel)
	if err != nil {
		return err
	}
	batchID = strings.TrimSpace(batchID)
	if batchID == "" {
		batchID = fmt.Sprintf("%s-%d", defaultPrefix, time.Now().Unix())
	} else if err := ValidateTriageBatchID(batchID); err != nil {
		return err
	}
	triagePath := filepath.Join(vaultDir, "memory", cleanChannel, "triage", batchID+".json")
	if err := store.WriteJSONAtomic(triagePath, rawItems, 0o600); err != nil {
		return err
	}

	var md strings.Builder
	fmt.Fprintf(&md, "# %s (%s)\n\n", digestHeading, time.Now().Format("2006-01-02 15:04"))
	for _, it := range entries {
		if it.Urgency == "ignore" {
			continue
		}
		senderLab := it.SenderLab
		if senderLab == "" {
			senderLab = "From"
		}
		fmt.Fprintf(&md, "- **[%s · %s]** `%s` — **%s** (%s: `%s`)\n  - %s\n",
			strings.ToUpper(it.Urgency), it.Category, it.TargetID, it.Title, senderLab, it.Sender, it.Summary)
		if it.DraftID != "" {
			fmt.Fprintf(&md, "  - Auto-Drafted Reply: `%s` (Proposal: `%s`)\n", it.DraftID, it.ProposalID)
		}
	}
	_ = store.WriteAtomic(filepath.Join(vaultDir, "notes", "inbox", batchID+".md"), []byte(md.String()), 0o600)
	return nil
}
