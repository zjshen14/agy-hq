package taint

import (
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
	"testing"
	"time"

	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/store"
)

const SchemaVersion = 1

// Record persists an immutable provenance-taint marker for a conversation at
// <vault>/private/taint/<sha256(convID)>.json and appends to <vault>/private/taint/ledger.jsonl.
type Record struct {
	SchemaVersion int      `json:"schema_version"`
	ConvID        string   `json:"conv_id"`
	TaintedAt     string   `json:"tainted_at"`
	UpdatedAt     string   `json:"updated_at,omitempty"`
	Source        string   `json:"source"`
	Connector     string   `json:"connector,omitempty"`
	Account       string   `json:"account,omitempty"`
	BatchID       string   `json:"batch_id,omitempty"`
	EventIDs      []string `json:"event_ids,omitempty"`
	ItemKeys      []string `json:"item_keys,omitempty"`
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
	if testing.Testing() && !strict && hostpaths.IsTempPath(vaultDir) && fc != "" {
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
	rec.SchemaVersion = SchemaVersion
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
		} else if errors.Is(openErr, os.ErrExist) {
			// Marker already exists — taint is monotonic and can never be cleared.
			// Only expand allowed scope metadata (EventIDs, ItemKeys, Connector/Account if empty).
			rawExisting, rErr := os.ReadFile(mp)
			if rErr != nil {
				return rErr
			}
			var existing Record
			if uErr := json.Unmarshal(rawExisting, &existing); uErr != nil {
				return fmt.Errorf("taint: corrupt existing marker %s: %w", mp, uErr)
			}
			existing.ConvID = convID
			existing.SchemaVersion = SchemaVersion
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
			}
			persisted = existing
		} else {
			return openErr
		}

		ledgerEntry := map[string]interface{}{
			"timestamp":  nowStr,
			"conv_id":    convID,
			"source":     rec.Source,
			"connector":  rec.Connector,
			"account":    rec.Account,
			"batch_id":   rec.BatchID,
			"event_ids":  rec.EventIDs,
			"item_keys":  rec.ItemKeys,
			"marker_sha": convHashHex(convID)[:16],
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
	if st, lErr := os.Lstat(mp); lErr == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return Record{ConvID: convID, Source: "fail_closed:non_regular_taint_marker"}, true
		}
		raw, rErr := os.ReadFile(mp)
		if rErr != nil {
			return Record{ConvID: convID, Source: "fail_closed:unreadable_taint_marker"}, true
		}
		var rec Record
		if uErr := json.Unmarshal(raw, &rec); uErr != nil || strings.TrimSpace(rec.ConvID) != convID {
			return Record{ConvID: convID, Source: "fail_closed:corrupt_taint_marker"}, true
		}
		return rec, true
	} else if !os.IsNotExist(lErr) {
		return Record{ConvID: convID, Source: "fail_closed:lstat_error"}, true
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
