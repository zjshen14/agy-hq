package taint

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/store"
)

const (
	SchemaVersion     = 2
	ProfileFailClosed = "fail_closed"
)

// SessionGrant persists an immutable, write-once Ring-0 capability grant attached to a tainted
// conversation (RFC-0026 §3.4). A nil Grant on a clean SchemaVersion <= 1 record represents a
// legacy RFC-0017b Quarantined Reader session.
type SessionGrant struct {
	GrantID        string   `json:"grant_id"`
	Profile        string   `json:"profile"` // "fail_closed" | "classifier" | "scoped_delegate"
	SourceFQID     string   `json:"source_fqid,omitempty"`
	CallerID       string   `json:"caller_id,omitempty"`
	OriginThread   string   `json:"origin_thread,omitempty"`
	PolicyID       string   `json:"policy_id,omitempty"`
	CandidateIDs   []string `json:"candidate_ids,omitempty"`
	RequestID      string   `json:"request_id,omitempty"`
	ExpiresAt      string   `json:"expires_at,omitempty"`
	Tasks          []string `json:"tasks,omitempty"`
	Workspaces     []string `json:"workspaces,omitempty"`
	Skills         []string `json:"skills,omitempty"`
	AllowedHQVerbs []string `json:"allowed_hq_verbs,omitempty"`
	Commands       []string `json:"commands,omitempty"`
	NudgeIDs       []string `json:"nudge_ids,omitempty"`
	EgressMode     string   `json:"egress_mode,omitempty"`
}

// Record persists an immutable provenance-taint marker for a conversation at
// <vault>/private/taint/<sha256(convID)>.json and appends to <vault>/private/taint/ledger.jsonl.
type Record struct {
	SchemaVersion int           `json:"schema_version"`
	ConvID        string        `json:"conv_id"`
	TaintedAt     string        `json:"tainted_at"`
	UpdatedAt     string        `json:"updated_at,omitempty"`
	Source        string        `json:"source"`
	Connector     string        `json:"connector,omitempty"`
	Account       string        `json:"account,omitempty"`
	BatchID       string        `json:"batch_id,omitempty"`
	EventIDs      []string      `json:"event_ids,omitempty"`
	ItemKeys      []string      `json:"item_keys,omitempty"`
	Grant         *SessionGrant `json:"grant,omitempty"`
}

func pendingGrantDir(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "taint", "pending")
}

func pendingGrantPath(vaultDir, grantID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(grantID)))
	return filepath.Join(pendingGrantDir(vaultDir), hex.EncodeToString(sum[:16])+".json")
}

// StagePendingGrant writes a pre-spawn immutable SessionGrant under <vault>/private/taint/pending/ (0600)
// using O_EXCL under RankTaint before the background event is leased/spawned.
func StagePendingGrant(ctx context.Context, vaultDir string, g SessionGrant) error {
	if strings.TrimSpace(vaultDir) == "" {
		return fmt.Errorf("taint: vaultDir is required")
	}
	g.GrantID = strings.TrimSpace(g.GrantID)
	if g.GrantID == "" {
		return fmt.Errorf("taint: grant_id is required")
	}
	if strings.TrimSpace(g.Profile) == "" {
		return fmt.Errorf("taint: grant profile is required")
	}
	return store.With(ctx, store.RankTaint, taintLockPath(vaultDir), func(_ context.Context) error {
		dir := pendingGrantDir(vaultDir)
		if st, err := os.Lstat(dir); err == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("taint: refusing symlinked pending grant dir %s", dir)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
		p := pendingGrantPath(vaultDir, g.GrantID)
		data, err := json.MarshalIndent(g, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')
		f, err := store.OpenCreateExclNoFollow(p, 0o600)
		if err != nil {
			return err
		}
		if _, wErr := f.Write(data); wErr != nil {
			_ = f.Close()
			_ = os.Remove(p)
			return wErr
		}
		if sErr := f.Sync(); sErr != nil {
			_ = f.Close()
			_ = os.Remove(p)
			return sErr
		}
		return f.Close()
	})
}

// ConsumePendingGrant loads and removes a staged pending grant by grantID under RankTaint.
func ConsumePendingGrant(ctx context.Context, vaultDir, grantID string) (SessionGrant, bool, error) {
	if strings.TrimSpace(vaultDir) == "" || strings.TrimSpace(grantID) == "" {
		return SessionGrant{}, false, nil
	}
	var out SessionGrant
	var found bool
	err := store.With(ctx, store.RankTaint, taintLockPath(vaultDir), func(_ context.Context) error {
		p := pendingGrantPath(vaultDir, grantID)
		st, lErr := os.Lstat(p)
		if lErr != nil {
			if os.IsNotExist(lErr) {
				return nil
			}
			return lErr
		}
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return fmt.Errorf("taint: refusing non-regular pending grant %s", p)
		}
		raw, rErr := os.ReadFile(p)
		if rErr != nil {
			return rErr
		}
		if uErr := json.Unmarshal(raw, &out); uErr != nil {
			return fmt.Errorf("taint: corrupt pending grant %s: %w", p, uErr)
		}
		if strings.TrimSpace(out.GrantID) != strings.TrimSpace(grantID) {
			return fmt.Errorf("taint: pending grant id mismatch")
		}
		found = true
		_ = os.Remove(p)
		return nil
	})
	return out, found, err
}

func taintDir(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "taint")
}

func taintLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "taint", ".taint.lock")
}

func ledgerPath(vaultDir string) string {
	return filepath.Join(vaultDir, "private", "taint", "ledger.jsonl")
}

func legacyTaintedSessionsPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "tainted_sessions.json")
}

func convHashHex(convID string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(convID)))
	return hex.EncodeToString(sum[:])
}

func markerPath(vaultDir, convID string) string {
	return filepath.Join(taintDir(vaultDir), convHashHex(convID)+".json")
}

// ResolveCallerConvID resolves the calling conversation ID strictly from ANTIGRAVITY_CONVERSATION_ID
// in live sessions, falling back to flagConv only in hermetic temp-vault tests when env is unset
// (and HQ_STRICT_CALLER_ENV != "1").
func ResolveCallerConvID(vaultDir, flagConv string) (string, error) {
	strict := os.Getenv("HQ_STRICT_CALLER_ENV") == "1"
	envConv := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CONVERSATION_ID"))
	fc := strings.TrimSpace(flagConv)
	if store.InGoTest() && !strict && hostpaths.IsTempPath(vaultDir) && fc != "" {
		return fc, nil
	}
	if envConv != "" {
		return envConv, nil
	}
	if !strict && hostpaths.IsTempPath(vaultDir) && fc != "" {
		return fc, nil
	}
	return "", fmt.Errorf("taint: ANTIGRAVITY_CONVERSATION_ID is required for quarantined ingress")
}

func mergeSortedUnique(existing, extra []string) ([]string, bool) {
	if len(extra) == 0 {
		return existing, false
	}
	seen := make(map[string]bool, len(existing)+len(extra))
	var out []string
	for _, s := range existing {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	changed := false
	for _, s := range extra {
		s = strings.TrimSpace(s)
		if s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
			changed = true
		}
	}
	sort.Strings(out)
	return out, changed
}

// Mark writes an O_EXCL 0600 per-conversation taint marker and appends an fsynced ledger entry
// under RankTaint BEFORE any untrusted content is returned to the caller.
func Mark(ctx context.Context, vaultDir string, rec Record) (Record, error) {
	if strings.TrimSpace(vaultDir) == "" {
		return Record{}, fmt.Errorf("taint: vaultDir is required")
	}
	convID := strings.TrimSpace(rec.ConvID)
	if convID == "" {
		return Record{}, fmt.Errorf("taint: convID is required")
	}
	nowStr := time.Now().UTC().Format(time.RFC3339)
	if strings.TrimSpace(rec.TaintedAt) == "" {
		rec.TaintedAt = nowStr
	}
	if rec.Grant != nil {
		rec.SchemaVersion = SchemaVersion
	} else {
		rec.SchemaVersion = 1
	}
	rec.ConvID = convID
	if strings.TrimSpace(rec.Source) == "" {
		rec.Source = "quarantined_read"
	}
	rec.EventIDs, _ = mergeSortedUnique(nil, rec.EventIDs)
	rec.ItemKeys, _ = mergeSortedUnique(nil, rec.ItemKeys)

	var persisted Record
	err := store.With(ctx, store.RankTaint, taintLockPath(vaultDir), func(_ context.Context) error {
		dir := taintDir(vaultDir)
		if st, lErr := os.Lstat(dir); lErr == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("taint: refusing symlinked taint directory %s", dir)
		}
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}

		mp := markerPath(vaultDir, convID)
		if st, lErr := os.Lstat(mp); lErr == nil && st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("taint: refusing symlinked marker %s", mp)
		}

		data, err := json.MarshalIndent(rec, "", "  ")
		if err != nil {
			return err
		}
		data = append(data, '\n')

		shouldAppendLedger := false
		f, openErr := store.OpenCreateExclNoFollow(mp, 0o600)
		if openErr == nil {
			if _, wErr := f.Write(data); wErr != nil {
				_ = f.Close()
				_ = os.Remove(mp)
				return wErr
			}
			if sErr := f.Sync(); sErr != nil {
				_ = f.Close()
				_ = os.Remove(mp)
				return sErr
			}
			if cErr := f.Close(); cErr != nil {
				return cErr
			}
			if df, dErr := os.Open(dir); dErr == nil {
				_ = df.Sync()
				_ = df.Close()
			}
			persisted = rec
			shouldAppendLedger = true
		} else if errors.Is(openErr, os.ErrExist) {
			// Marker already exists — taint is monotonic and can never be cleared.
			// SessionGrant is strictly write-once immutable and incomparable across sibling profiles (RFC-0026 P0-1).
			rawExisting, rErr := os.ReadFile(mp)
			if rErr != nil {
				return rErr
			}
			var existing Record
			if uErr := json.Unmarshal(rawExisting, &existing); uErr != nil {
				return fmt.Errorf("taint: corrupt existing marker %s: %w", mp, uErr)
			}
			existing.ConvID = convID
			if existing.Grant != nil {
				if rec.Grant != nil && rec.Grant.GrantID != existing.Grant.GrantID {
					return fmt.Errorf("taint: refusing to overwrite immutable session grant %q with %q", existing.Grant.GrantID, rec.Grant.GrantID)
				}
				if rec.Grant == nil && (rec.Connector != "" || len(rec.ItemKeys) > 0) {
					return fmt.Errorf("taint: refusing cross-profile inbox taint merge onto granted session %q", existing.Grant.Profile)
				}
				// Idempotent re-bind of the same grant or event IDs: preserve Grant strictly unchanged.
				persisted = existing
				return nil
			}
			if rec.Grant != nil {
				return fmt.Errorf("taint: refusing to attach grant %q onto already-tainted quarantined_reader session", rec.Grant.Profile)
			}
			if existing.SchemaVersion == 0 {
				existing.SchemaVersion = 1
			}
			evMerged, evChanged := mergeSortedUnique(existing.EventIDs, rec.EventIDs)
			itMerged, itChanged := mergeSortedUnique(existing.ItemKeys, rec.ItemKeys)
			metaChanged := false
			if existing.Connector == "" && rec.Connector != "" {
				existing.Connector = rec.Connector
				metaChanged = true
			}
			if existing.Account == "" && rec.Account != "" {
				existing.Account = rec.Account
				metaChanged = true
			}
			if existing.BatchID == "" && rec.BatchID != "" {
				existing.BatchID = rec.BatchID
				metaChanged = true
			}
			if evChanged || itChanged || metaChanged {
				existing.EventIDs = evMerged
				existing.ItemKeys = itMerged
				existing.UpdatedAt = nowStr
				updBytes, mErr := json.MarshalIndent(existing, "", "  ")
				if mErr != nil {
					return mErr
				}
				if wErr := store.WriteAtomic(mp, append(updBytes, '\n'), 0o600); wErr != nil {
					return wErr
				}
				shouldAppendLedger = true
			}
			persisted = existing
		} else {
			return openErr
		}

		if !shouldAppendLedger {
			return nil
		}

		grantProfile := ""
		grantID := ""
		if persisted.Grant != nil {
			grantProfile = persisted.Grant.Profile
			grantID = persisted.Grant.GrantID
		}
		ledgerEntry := map[string]interface{}{
			"timestamp":      nowStr,
			"schema_version": persisted.SchemaVersion,
			"conv_id":        convID,
			"source":         rec.Source,
			"connector":      rec.Connector,
			"account":        rec.Account,
			"batch_id":       rec.BatchID,
			"event_ids":      rec.EventIDs,
			"item_keys":      rec.ItemKeys,
			"grant_profile":  grantProfile,
			"grant_id":       grantID,
			"marker_sha":     convHashHex(convID)[:16],
		}
		line, mErr := json.Marshal(ledgerEntry)
		if mErr != nil {
			return mErr
		}
		return store.AppendLineNoFollow(ledgerPath(vaultDir), append(line, '\n'), 0o600)
	})
	return persisted, err
}

// MarkSession is a convenience wrapper around Mark for simple string sources.
func MarkSession(vaultDir, convID, source string) error {
	if strings.TrimSpace(convID) == "" {
		return nil
	}
	rec := Record{
		ConvID: convID,
		Source: source,
	}
	// Extract item key if source looks like "gmail:thread:<id>" or "gchat:space:<id>"
	if strings.HasPrefix(source, "gmail:thread:") || strings.HasPrefix(source, "gchat:space:") {
		rec.ItemKeys = []string{source}
		parts := strings.SplitN(source, ":", 2)
		rec.Connector = parts[0]
	}
	_, err := Mark(context.Background(), vaultDir, rec)
	return err
}

// Check returns (Record, true) if convID is tainted, failing CLOSED (returning true) if the marker
// or legacy tainted_sessions.json is symlinked, unreadable, or corrupt.
func Check(vaultDir, convID string) (Record, bool) {
	convID = strings.TrimSpace(convID)
	if convID == "" || strings.TrimSpace(vaultDir) == "" {
		return Record{}, false
	}

	mp := markerPath(vaultDir, convID)
	failClosedRec := func(reason string) Record {
		return Record{
			SchemaVersion: SchemaVersion,
			ConvID:        convID,
			Source:        "fail_closed:" + reason,
			Grant:         &SessionGrant{Profile: ProfileFailClosed},
		}
	}
	if st, lErr := os.Lstat(mp); lErr == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return failClosedRec("non_regular_taint_marker"), true
		}
		raw, rErr := os.ReadFile(mp)
		if rErr != nil {
			return failClosedRec("unreadable_taint_marker"), true
		}
		var rec Record
		if uErr := json.Unmarshal(raw, &rec); uErr != nil || strings.TrimSpace(rec.ConvID) != convID {
			return failClosedRec("corrupt_taint_marker"), true
		}
		if strings.HasPrefix(rec.Source, "fail_closed:") && rec.Grant == nil {
			rec.Grant = &SessionGrant{Profile: ProfileFailClosed}
		}
		return rec, true
	} else if !os.IsNotExist(lErr) {
		return failClosedRec("lstat_error"), true
	}

	// Append-only ledger failover (<vault>/private/taint/ledger.jsonl): if <sha256>.json was removed,
	// consult the durable ledger and fail closed on symlinked/unreadable/corrupt ledger files.
	// Any v2/grant-bearing session MUST clamp to ProfileFailClosed (never upgrading to quarantined_reader).
	lp := ledgerPath(vaultDir)
	if st, lErr := os.Lstat(lp); lErr == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return failClosedRec("non_regular_taint_ledger"), true
		}
		lf, oErr := store.OpenReadNoFollow(lp)
		if oErr != nil {
			return failClosedRec("unreadable_taint_ledger"), true
		}
		scanner := bufio.NewScanner(lf)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		var matched Record
		foundInLedger := false
		sawV2Grant := false
		corruptLedger := false
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var entry struct {
				Timestamp     string   `json:"timestamp"`
				SchemaVersion int      `json:"schema_version"`
				ConvID        string   `json:"conv_id"`
				Source        string   `json:"source"`
				Connector     string   `json:"connector"`
				Account       string   `json:"account"`
				BatchID       string   `json:"batch_id"`
				EventIDs      []string `json:"event_ids"`
				ItemKeys      []string `json:"item_keys"`
				GrantProfile  string   `json:"grant_profile"`
				GrantID       string   `json:"grant_id"`
			}
			if json.Unmarshal([]byte(line), &entry) != nil {
				corruptLedger = true
				break
			}
			if strings.TrimSpace(entry.ConvID) != convID {
				continue
			}
			if entry.SchemaVersion >= 2 || strings.TrimSpace(entry.GrantProfile) != "" || strings.TrimSpace(entry.GrantID) != "" {
				sawV2Grant = true
			}
			if !foundInLedger {
				foundInLedger = true
				matched = Record{
					SchemaVersion: 1,
					ConvID:        convID,
					TaintedAt:     entry.Timestamp,
					Source:        entry.Source,
					Connector:     entry.Connector,
					Account:       entry.Account,
					BatchID:       entry.BatchID,
				}
				if matched.Source == "" {
					matched.Source = "taint_ledger_failover"
				}
			}
			matched.EventIDs, _ = mergeSortedUnique(matched.EventIDs, entry.EventIDs)
			matched.ItemKeys, _ = mergeSortedUnique(matched.ItemKeys, entry.ItemKeys)
			if matched.Connector == "" && entry.Connector != "" {
				matched.Connector = entry.Connector
			}
			if matched.Account == "" && entry.Account != "" {
				matched.Account = entry.Account
			}
			if matched.BatchID == "" && entry.BatchID != "" {
				matched.BatchID = entry.BatchID
			}
		}
		sErr := scanner.Err()
		_ = lf.Close()
		if corruptLedger || sErr != nil {
			return failClosedRec("corrupt_taint_ledger"), true
		}
		if foundInLedger {
			if sawV2Grant || strings.HasPrefix(matched.Source, "fail_closed:") {
				return failClosedRec("v2_ledger_failover"), true
			}
			return matched, true
		}
	} else if !os.IsNotExist(lErr) {
		return failClosedRec("ledger_lstat_error"), true
	}

	// 1-release read-only fallback to legacy <vault>/memory/tainted_sessions.json
	legPath := legacyTaintedSessionsPath(vaultDir)
	if st, lErr := os.Lstat(legPath); lErr == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return Record{ConvID: convID, Source: "fail_closed:non_regular_legacy_taint"}, true
		}
		raw, rErr := os.ReadFile(legPath)
		if rErr != nil {
			return Record{ConvID: convID, Source: "fail_closed:unreadable_legacy_taint"}, true
		}
		if strings.TrimSpace(string(raw)) != "" {
			var legacyMap map[string]string
			if uErr := json.Unmarshal(raw, &legacyMap); uErr != nil {
				return Record{ConvID: convID, Source: "fail_closed:corrupt_legacy_taint"}, true
			}
			if reason, ok := legacyMap[convID]; ok {
				return Record{
					SchemaVersion: SchemaVersion,
					ConvID:        convID,
					Source:        reason,
				}, true
			}
		}
	}
	return Record{}, false
}

// IsTainted reports whether convID is tainted and returns a human-readable source string.
func IsTainted(vaultDir, convID string) (string, bool) {
	rec, ok := Check(vaultDir, convID)
	if !ok {
		return "", false
	}
	if rec.TaintedAt != "" {
		return fmt.Sprintf("%s (%s)", rec.Source, rec.TaintedAt), true
	}
	return rec.Source, true
}
