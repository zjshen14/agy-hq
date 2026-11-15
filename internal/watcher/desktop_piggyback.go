package watcher

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/zjshen14/agy-hq/internal/connector"
	"github.com/zjshen14/agy-hq/internal/extension"
	"github.com/zjshen14/agy-hq/internal/hostpaths"
	"github.com/zjshen14/agy-hq/internal/store"
)

const DesktopPiggybackMinInterval = 15 * time.Minute

func piggybackStampPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "watcher", ".desktop_piggyback_stamp")
}

func piggybackLockPath(vaultDir string) string {
	return filepath.Join(vaultDir, "memory", "watcher", ".desktop_piggyback.lock")
}

// MaybeLaunchDesktopPiggyback launches a detached (`Setsid: true`) `hq watch precheck --desktop-only`
// child from `Stop` when `ANTIGRAVITY_CREDENTIAL_SCOPE` is present in memory, rate-limited to once per 15m
// with non-blocking singleflight locking and ZERO disk persistence of `ANTIGRAVITY_CREDENTIAL_SCOPE`.
func MaybeLaunchDesktopPiggyback(vaultDir string, now time.Time) bool {
	if strings.TrimSpace(vaultDir) == "" {
		return false
	}
	scope := strings.TrimSpace(os.Getenv("ANTIGRAVITY_CREDENTIAL_SCOPE"))
	if scope == "" {
		return false
	}
	if hostpaths.IsTempPath(vaultDir) && os.Getenv("HQ_ENABLE_TEST_PIGGYBACK") != "1" {
		return false
	}
	if now.IsZero() {
		now = time.Now()
	}

	minInterval := DesktopPiggybackMinInterval
	if raw := strings.TrimSpace(os.Getenv("HQ_DESKTOP_PIGGYBACK_INTERVAL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d >= 0 {
			minInterval = d
		}
	}

	claimed := false
	ok, err := store.TryWith(context.Background(), store.RankWatcher, piggybackLockPath(vaultDir), func(_ context.Context) error {
		stampFile := piggybackStampPath(vaultDir)
		if raw, rErr := os.ReadFile(stampFile); rErr == nil {
			if last, pErr := time.Parse(time.RFC3339, strings.TrimSpace(string(raw))); pErr == nil {
				if now.Sub(last) < minInterval {
					return nil
				}
			}
		}
		// Write ONLY the RFC3339 timestamp — NEVER persist ANTIGRAVITY_CREDENTIAL_SCOPE to disk!
		if wErr := store.WriteAtomic(stampFile, []byte(now.UTC().Format(time.RFC3339)+"\n"), 0o600); wErr != nil {
			return wErr
		}
		claimed = true
		return nil
	})
	if err != nil || !ok || !claimed {
		return false
	}

	exe := extension.ResolveSelfHQExecutable()
	if strings.TrimSpace(exe) == "" {
		return false
	}

	devNull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return false
	}
	defer devNull.Close()

	cmd := exec.Command(exe, "watch", "precheck", "--desktop-only")
	cmd.Dir = vaultDir
	cmd.Env = connector.SanitizedSubprocessEnv(nil, []string{"AGY_ACCOUNT", "ANTIGRAVITY_CREDENTIAL_SCOPE"}, map[string]string{
		"HQ_VAULT_DIR": vaultDir,
	}, true)
	cmd.Stdin = devNull
	cmd.Stdout = devNull
	cmd.Stderr = devNull
	store.SetDetachedSession(cmd)

	if err := cmd.Start(); err != nil {
		return false
	}
	_ = cmd.Process.Release()
	return true
}
