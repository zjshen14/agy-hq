package kernel

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/store"
	"github.com/zjshen14/agy-hq/internal/watcher"
)

// ConnectorHealthItem summarizes one connector's local precheck/auth state (0-token, no network).
type ConnectorHealthItem struct {
	Connector     string `json:"connector"`
	Status        string `json:"status"` // "ok" | "AUTH_EXPIRED" | "DEGRADED"
	LastOKAt      string `json:"lastOkAt,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	LastErrorAt   string `json:"lastErrorAt,omitempty"`
	FailedCount   int    `json:"failedCount,omitempty"`
	TotalAccounts int    `json:"totalAccounts,omitempty"`
}

// MonitorHealthSummary is a 0-token dead-man's-switch snapshot of daemon & connector health
// assembled strictly from local Vault state files.
type MonitorHealthSummary struct {
	DaemonAlive     bool                  `json:"daemonAlive"`
	LastHeartbeatAt string                `json:"lastHeartbeatAt,omitempty"`
	HeartbeatStale  bool                  `json:"heartbeatStale,omitempty"`
	Connectors      []ConnectorHealthItem `json:"connectors,omitempty"`
}

const daemonHeartbeatStaleThreshold = 30 * time.Minute

// BuildMonitorHealthSummary reads local daemon, quota, watcher, and legacy gmail/gchat
// watermark files without making any network calls.
func BuildMonitorHealthSummary(vaultDir string, now time.Time) MonitorHealthSummary {
	if now.IsZero() {
		now = time.Now()
	}
	sum := MonitorHealthSummary{
		DaemonAlive: IsDaemonActiveFast(vaultDir),
	}
	q := LoadQuota(vaultDir)
	if strings.TrimSpace(q.LastHeartbeatAt) != "" {
		sum.LastHeartbeatAt = strings.TrimSpace(q.LastHeartbeatAt)
		if ts, pErr := time.Parse(time.RFC3339, sum.LastHeartbeatAt); pErr == nil {
			if sum.DaemonAlive && now.Sub(ts) > daemonHeartbeatStaleThreshold {
				sum.HeartbeatStale = true
			}
		}
	}

	type acctSample struct {
		connector string
		account   string
		ok        bool
		authErr   bool
		lastOK    time.Time
		lastOKRaw string
		lastErr   string
		errAtRaw  string
	}
	byKey := make(map[string]acctSample)

	// 1. Modular watcher state (<vault>/memory/watcher/state.json)
	if wst, err := watcher.LoadState(context.Background(), vaultDir); err == nil {
		for _, acc := range wst.Accounts {
			conn := strings.ToLower(strings.TrimSpace(acc.Connector))
			acct := strings.TrimSpace(acc.Account)
			if conn == "" || acct == "" {
				continue
			}
			isAuth := acc.AuthNoticeActive || strings.Contains(strings.ToLower(acc.LastStatus), "auth") || isAuthErrorText(acc.LastError)
			isDegraded := isAuth || acc.ConsecutiveTransient > 0 || (acc.LastError != "" && !strings.EqualFold(acc.LastStatus, "ok"))
			ts, _ := time.Parse(time.RFC3339, strings.TrimSpace(acc.LastPolledAt))
			byKey[conn+":"+acct] = acctSample{
				connector: conn,
				account:   acct,
				ok:        !isDegraded,
				authErr:   isAuth,
				lastOK:    ts,
				lastOKRaw: strings.TrimSpace(acc.LastPolledAt),
				lastErr:   strings.TrimSpace(acc.LastError),
			}
		}
	}

	// 2. Legacy Layer-1 gmail/gchat watermarks (<vault>/memory/{gmail,gchat}/state-*.json)
	for _, ch := range []string{"gmail", "gchat"} {
		matches, _ := filepath.Glob(filepath.Join(vaultDir, "memory", ch, "state-*.json"))
		for _, p := range matches {
			wm, err := store.ReadJSONFile[connector.AccountWatermark](p)
			if err != nil {
				continue
			}
			acct := strings.TrimSpace(wm.Account)
			if acct == "" {
				base := filepath.Base(p)
				acct = strings.TrimSuffix(strings.TrimPrefix(base, "state-"), ".json")
			}
			if acct == "" {
				continue
			}
			key := ch + ":" + acct
			lastOKTS, _ := time.Parse(time.RFC3339, strings.TrimSpace(wm.LastCheckedAt))
			errTS, _ := time.Parse(time.RFC3339, strings.TrimSpace(wm.LastErrorAt))
			sampleTS := lastOKTS
			if errTS.After(sampleTS) {
				sampleTS = errTS
			}
			if existing, exists := byKey[key]; exists && !existing.lastOK.IsZero() && existing.lastOK.After(sampleTS) {
				continue
			}
			isDegraded := wm.ConsecutiveFailures > 0 || strings.TrimSpace(wm.LastError) != ""
			byKey[key] = acctSample{
				connector: ch,
				account:   acct,
				ok:        !isDegraded,
				authErr:   isDegraded && isAuthErrorText(wm.LastError),
				lastOK:    lastOKTS,
				lastOKRaw: strings.TrimSpace(wm.LastCheckedAt),
				lastErr:   strings.TrimSpace(wm.LastError),
				errAtRaw:  strings.TrimSpace(wm.LastErrorAt),
			}
		}
	}

	// Aggregate per connector
	agg := make(map[string]*ConnectorHealthItem)
	aggLastOK := make(map[string]time.Time)
	for _, s := range byKey {
		it, ok := agg[s.connector]
		if !ok {
			it = &ConnectorHealthItem{
				Connector: s.connector,
				Status:    "ok",
			}
			agg[s.connector] = it
		}
		it.TotalAccounts++
		if !s.lastOK.IsZero() && s.lastOK.After(aggLastOK[s.connector]) {
			aggLastOK[s.connector] = s.lastOK
			it.LastOKAt = s.lastOKRaw
		}
		if !s.ok {
			it.FailedCount++
			if s.authErr {
				it.Status = "AUTH_EXPIRED"
			} else if it.Status != "AUTH_EXPIRED" {
				it.Status = "DEGRADED"
			}
			if it.LastError == "" {
				it.LastError = s.lastErr
				it.LastErrorAt = s.errAtRaw
			}
		}
	}

	keys := make([]string, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sum.Connectors = append(sum.Connectors, *agg[k])
	}
	return sum
}

func isAuthErrorText(msg string) bool {
	low := strings.ToLower(msg)
	for _, tok := range []string{"auth", "oauth", "token", "login", "unauthorized", "401", "403", "credential", "expired", "signed out", "reconnect"} {
		if strings.Contains(low, tok) {
			return true
		}
	}
	return false
}

func formatCompactClock(rfc3339 string, loc *time.Location) string {
	rfc3339 = strings.TrimSpace(rfc3339)
	if rfc3339 == "" {
		return "never"
	}
	ts, err := time.Parse(time.RFC3339, rfc3339)
	if err != nil {
		return rfc3339
	}
	if loc != nil {
		ts = ts.In(loc)
	}
	return ts.Format("15:04")
}

// FormatLine renders a compact 1-line dead-man's-switch summary suitable for
// both `hq status` and `CONSOLE.md`.
func (s MonitorHealthSummary) FormatLine(now time.Time) string {
	if now.IsZero() {
		now = time.Now()
	}
	loc := now.Location()
	daemonPart := "daemon: stopped"
	if s.DaemonAlive {
		if s.LastHeartbeatAt != "" {
			hb := formatCompactClock(s.LastHeartbeatAt, loc)
			if s.HeartbeatStale {
				daemonPart = fmt.Sprintf("daemon: alive (STALE heartbeat @ %s)", hb)
			} else {
				daemonPart = fmt.Sprintf("daemon: alive (heartbeat @ %s)", hb)
			}
		} else {
			daemonPart = "daemon: alive (no heartbeat yet)"
		}
	} else if s.LastHeartbeatAt != "" {
		daemonPart = fmt.Sprintf("daemon: stopped (last heartbeat @ %s)", formatCompactClock(s.LastHeartbeatAt, loc))
	}

	if len(s.Connectors) == 0 {
		return daemonPart
	}
	var cParts []string
	for _, c := range s.Connectors {
		lastOK := formatCompactClock(c.LastOKAt, loc)
		if c.Status == "ok" {
			cParts = append(cParts, fmt.Sprintf("%s: ok @ %s", c.Connector, lastOK))
		} else {
			cParts = append(cParts, fmt.Sprintf("%s: %s (last ok %s)", c.Connector, c.Status, lastOK))
		}
	}
	return daemonPart + " | " + strings.Join(cParts, ", ")
}
