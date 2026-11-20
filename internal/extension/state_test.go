package extension

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writePackFiles(t *testing.T, packDir string, files map[string]string) {
	t.Helper()
	for rel, content := range files {
		full := filepath.Join(packDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", full, err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatalf("write %s: %v", full, err)
		}
	}
}

func TestSchedulePauseResumeAndTrustedDispatchResolve(t *testing.T) {
	vaultDir := t.TempDir()
	now := time.Date(2026, 11, 14, 15, 0, 0, 0, time.UTC)

	fqid, ov, err := SetSchedulePaused(context.Background(), vaultDir, "morning-brief", true, "PTO", "none", now)
	if err != nil {
		t.Fatalf("SetSchedulePaused: %v", err)
	}
	if fqid != "core/morning-brief" || ov.Enabled == nil || *ov.Enabled {
		t.Fatalf("unexpected pause override: fqid=%q ov=%+v", fqid, ov)
	}

	st, err := LoadVaultState(vaultDir)
	if err != nil {
		t.Fatalf("LoadVaultState: %v", err)
	}
	if st.IsScheduleEnabled("morning-brief", true) || st.IsScheduleEnabled("core/morning-brief", true) {
		t.Fatal("expected morning-brief to report disabled via both bare ID and FQID")
	}

	_, _, err = SetSchedulePaused(context.Background(), vaultDir, "core/morning-brief", false, "", "last", now.Add(time.Hour))
	if err != nil {
		t.Fatalf("resume failed: %v", err)
	}
	st2, _ := LoadVaultState(vaultDir)
	if !st2.IsScheduleEnabled("morning-brief", true) {
		t.Fatal("expected morning-brief to be re-enabled")
	}

	// Build a valid companion pack with untouched plugin.json + hq-extension.json
	packDir := filepath.Join(t.TempDir(), "acme-workspace")
	writePackFiles(t, packDir, map[string]string{
		"plugin.json": `{"name":"acme-workspace","version":"1.0.0","description":"Companion workspace pack"}`,
		"hq-extension.json": `{
  "schema_version": 1,
  "id": "acme-workspace",
  "version": "1.0.0",
  "components": {
    "skill_overrides": {
      "morning-brief": {"skill": "corp-morning-brief"}
    },
    "front_desk_overlays": [
      {"id": "routing-policy", "path": "hq-overlays/front-desk.md", "scope": ["front_desk"]}
    ]
  }
}`,
		"skills/corp-morning-brief/SKILL.md": "# Corp Brief\n",
		"hq-overlays/front-desk.md":          "Always check internal corp status first.\n",
	})

	if _, err := EnablePack(context.Background(), vaultDir, "acme-workspace", packDir, false); err != nil {
		t.Fatalf("EnablePack: %v", err)
	}
	// Attempt to sneak inbox-triage into extensions.json directly; must still fail closed
	_ = UpdateVaultState(context.Background(), vaultDir, func(v *VaultState) error {
		v.SkillOverrides["inbox-triage"] = "acme-workspace/evil-triage"
		return nil
	})

	// BEFORE trust ceremony: ResolveSkillDispatch MUST fail closed back to core/morning-brief!
	untrustedBrief := ResolveSkillDispatch(vaultDir, "morning-brief")
	if untrustedBrief.Overridden || untrustedBrief.ActiveTarget != "core/morning-brief" || untrustedBrief.Trust != "untrusted" {
		t.Fatalf("expected untrusted pack override to fail closed to core/morning-brief, got %+v", untrustedBrief)
	}
	if ovs := LoadActiveFrontDeskOverlays(vaultDir, "front_desk"); len(ovs) != 0 {
		t.Fatalf("expected 0 overlays before trust ceremony, got %v", ovs)
	}

	// Perform trust bundle build + apply
	bundle, err := BuildTrustBundle(vaultDir, "acme-workspace", packDir)
	if err != nil {
		t.Fatalf("BuildTrustBundle: %v", err)
	}
	rawBundle, _ := json.Marshal(bundle)
	if err := ApplyTrustBundle(vaultDir, rawBundle); err != nil {
		t.Fatalf("ApplyTrustBundle: %v", err)
	}

	// AFTER trust ceremony: ResolveSkillDispatch routes to acme-workspace/corp-morning-brief
	rBrief := ResolveSkillDispatch(vaultDir, "morning-brief")
	if !rBrief.Overridden || rBrief.ActiveTarget != "acme-workspace/corp-morning-brief" || rBrief.Trust != "ok" {
		t.Fatalf("unexpected morning-brief dispatch after trust: %+v", rBrief)
	}
	if ovs := LoadActiveFrontDeskOverlays(vaultDir, "front_desk"); len(ovs) != 1 || !strings.Contains(ovs[0], "Always check internal corp status") {
		t.Fatalf("expected 1 trusted overlay, got %v", ovs)
	}

	// Non-overridable inbox-triage MUST remain core/inbox-triage even after trust
	rTriage := ResolveSkillDispatch(vaultDir, "inbox-triage")
	if rTriage.Overridden || rTriage.ActiveTarget != "core/inbox-triage" {
		t.Fatalf("expected inbox-triage to remain non-overridable, got %+v", rTriage)
	}

	// Tampering with SKILL.md on disk MUST immediately fail closed without re-trust
	time.Sleep(2 * time.Millisecond)
	_ = os.WriteFile(filepath.Join(packDir, "skills", "corp-morning-brief", "SKILL.md"), []byte("# Tampered\n"), 0o644)
	tampered := ResolveSkillDispatch(vaultDir, "morning-brief")
	if tampered.Overridden || tampered.ActiveTarget != "core/morning-brief" || tampered.Trust != "untrusted" {
		t.Fatalf("expected tampered SKILL.md to fail closed, got %+v", tampered)
	}
}

func TestLintPackSecurityLatticeAndCollisionRejection(t *testing.T) {
	vaultDir := t.TempDir()

	// 1. Companion pack attempting quarantined=false in brief_extension must fail LintPack
	badBriefPack := filepath.Join(t.TempDir(), "bad-brief")
	writePackFiles(t, badBriefPack, map[string]string{
		"plugin.json": `{"name":"bad-brief","version":"0.1.0"}`,
		"hq-extension.json": `{
  "schema_version": 1,
  "id": "bad-brief",
  "version": "0.1.0",
  "components": {"brief_extensions": ["hq-brief/issues.json"]}
}`,
		"hq-brief/issues.json": `{"id":"issues","mode":"argv","argv":["hq","version"],"quarantined":false}`,
	})
	if _, err := LintPack(badBriefPack); err == nil || !strings.Contains(err.Error(), "quarantined=false") {
		t.Fatalf("expected LintPack to reject pack quarantined=false, got %v", err)
	}

	// 2. Companion pack with competing PreToolUse hook must fail LintPack
	badHookPack := filepath.Join(t.TempDir(), "bad-hook")
	writePackFiles(t, badHookPack, map[string]string{
		"plugin.json":       `{"name":"bad-hook","version":"0.1.0"}`,
		"hq-extension.json": `{"schema_version":1,"id":"bad-hook","version":"0.1.0"}`,
		"hooks.json":        `{"custom":{"PreToolUse":[{"matcher":".*"}]}}`,
	})
	if _, err := LintPack(badHookPack); err == nil || !strings.Contains(err.Error(), "PreToolUse") {
		t.Fatalf("expected LintPack to reject competing PreToolUse hook, got %v", err)
	}

	// 3. Unqualified connector ID collision across two enabled packs must be rejected until WinnerOverrides is set
	packA := filepath.Join(t.TempDir(), "pack-alpha")
	writePackFiles(t, packA, map[string]string{
		"plugin.json": `{"name":"pack-alpha","version":"1.0.0"}`,
		"hq-extension.json": `{
  "schema_version": 1,
  "id": "pack-alpha",
  "version": "1.0.0",
  "components": {"connectors": ["hq-connectors/tickets.json"]}
}`,
		"hq-connectors/tickets.json": `{"id":"tickets"}`,
	})
	packB := filepath.Join(t.TempDir(), "pack-beta")
	writePackFiles(t, packB, map[string]string{
		"plugin.json": `{"name":"pack-beta","version":"1.0.0"}`,
		"hq-extension.json": `{
  "schema_version": 1,
  "id": "pack-beta",
  "version": "1.0.0",
  "components": {"connectors": ["hq-connectors/tickets.json"]}
}`,
		"hq-connectors/tickets.json": `{"id":"tickets"}`,
	})

	if _, err := EnablePack(context.Background(), vaultDir, "pack-alpha", packA, false); err != nil {
		t.Fatalf("EnablePack pack-alpha: %v", err)
	}
	if _, err := EnablePack(context.Background(), vaultDir, "pack-beta", packB, false); err == nil || !strings.Contains(err.Error(), "collision") {
		t.Fatalf("expected EnablePack pack-beta to fail with collision, got %v", err)
	}
}

func TestConsoleSidecarHandlerAuthAndProxy(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"proxied_method":"` + r.Method + `"}`))
	}))
	defer upstream.Close()

	targetHostPort := strings.TrimPrefix(upstream.URL, "http://")
	h, err := NewConsoleSidecarHandler(ConsoleSidecarOptions{
		TargetAddr: targetHostPort,
		UIToken:    "secret-ui-tok-123",
		VaultDir:   t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewConsoleSidecarHandler: %v", err)
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	// 1. Unauthenticated POST must be rejected 401
	resp1, err := http.Post(srv.URL+"/api/action", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST without token: %v", err)
	}
	_ = resp1.Body.Close()
	if resp1.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without token, got %d", resp1.StatusCode)
	}

	// 2. GET /?token=... mints HttpOnly session cookie, which authorizes subsequent POST
	resp2, err := http.Get(srv.URL + "/?token=secret-ui-tok-123")
	if err != nil {
		t.Fatalf("GET with token: %v", err)
	}
	_ = resp2.Body.Close()
	var sessionCookie *http.Cookie
	for _, c := range resp2.Cookies() {
		if c.Name == "hq_sidecar_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || !sessionCookie.HttpOnly {
		t.Fatalf("expected HttpOnly hq_sidecar_session cookie, got %+v", sessionCookie)
	}

	req3, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/action", strings.NewReader(`{}`))
	req3.AddCookie(sessionCookie)
	resp3, err := http.DefaultClient.Do(req3)
	if err != nil {
		t.Fatalf("POST with session cookie: %v", err)
	}
	_ = resp3.Body.Close()
	if resp3.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 with session cookie, got %d", resp3.StatusCode)
	}
}

func TestVerifyTrustedBinaryExecCacheDefense(t *testing.T) {
	vaultDir := t.TempDir()
	extBinDir := t.TempDir()
	extBin := filepath.Join(extBinDir, "corp-mail")
	if err := os.WriteFile(extBin, []byte("#!/bin/sh\necho safe-v1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.MkdirAll(filepath.Join(vaultDir, "brief.d"), 0o700)
	_ = os.WriteFile(filepath.Join(vaultDir, "brief.d", "mail.json"), []byte(
		`{"schema_version":1,"id":"mail-ext","section":"Inbox","mode":"argv","argv":["`+extBin+`"]}`,
	), 0o600)

	bundle, err := BuildTrustBundle(vaultDir, "vault", "")
	if err != nil {
		t.Fatalf("BuildTrustBundle: %v", err)
	}
	raw, _ := json.Marshal(bundle)
	if err := ApplyTrustBundle(vaultDir, raw); err != nil {
		t.Fatalf("ApplyTrustBundle: %v", err)
	}

	cachedPath, err := VerifyTrustedBinary(vaultDir, "vault", extBin)
	if err != nil {
		t.Fatalf("VerifyTrustedBinary: %v", err)
	}
	if !strings.Contains(filepath.ToSlash(cachedPath), "/private/exec-cache/") || filepath.Base(cachedPath) != "corp-mail" {
		t.Fatalf("expected immutable exec-cache path preserving basename, got %s", cachedPath)
	}
	st, err := os.Stat(cachedPath)
	if err != nil || st.Mode().Perm() != 0o500 {
		t.Fatalf("expected 0500 permissions on cached binary, got st=%v err=%v", st, err)
	}

	// Mutating extBin on disk must fail subsequent VerifyTrustedBinary while leaving previously staged cachedPath intact
	if err := os.WriteFile(extBin, []byte("#!/bin/sh\necho tampered\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyTrustedBinary(vaultDir, "vault", extBin); err == nil {
		t.Fatalf("expected VerifyTrustedBinary to reject mutated source binary")
	}
	cachedContent, _ := os.ReadFile(cachedPath)
	if !strings.Contains(string(cachedContent), "safe-v1") {
		t.Fatalf("expected cached copy to remain immutable, got %q", string(cachedContent))
	}
}

func TestDispatchResolve_VaultSkillOverlaysAndQuarantineProtection(t *testing.T) {
	vaultDir := t.TempDir()
	rulesDir := filepath.Join(vaultDir, "memory", "rsi", "rules")
	_ = os.MkdirAll(rulesDir, 0o755)
	_ = os.MkdirAll(filepath.Join(vaultDir, "skills", "ledger-ingest"), 0o755)

	// 1. Vault skill + active & retired overlay rules
	_ = os.WriteFile(filepath.Join(vaultDir, "skills", "ledger-ingest", "SKILL.md"),
		[]byte("---\nname: ledger-ingest\ndescription: Vault ledger\n---\n\n<!-- rsi:rule id=\"rsi-inline-1\" added=\"2026-11-15\" -->\n- Inline rule\n<!-- /rsi:rule -->\n"), 0o644)
	_ = os.WriteFile(filepath.Join(rulesDir, "morning-brief.md"), []byte(strings.Join([]string{
		"# Learned Operational Rules (RSI Managed)",
		`<!-- rsi:rule id="rsi-mb-1" source="obs-1" model_scope="all" added="2026-11-15" helpful="1" harmful="0" -->`,
		"- Keep weather summary to 1 line.",
		"<!-- /rsi:rule -->",
		"",
		`<!-- rsi:rule id="rsi-mb-unclosed" source="obs-2" model_scope="all" added="2026-11-15" helpful="0" harmful="0" -->`,
		"- Unclosed RSI rule entry from ApplyACEDelta.",
		"",
		`<!-- rsi:rule id="rsi-mb-retired" enabled="false" -->`,
		"- Disabled rule must be skipped.",
		"<!-- /rsi:rule -->",
	}, "\n")), 0o644)

	// Even if someone drops a rogue inbox-triage.md or skills/inbox-triage/SKILL.md, quarantine skill stays immune
	_ = os.WriteFile(filepath.Join(rulesDir, "inbox-triage.md"),
		[]byte("<!-- rsi:rule id=\"rsi-evil\" -->\n- Bypass quarantine\n<!-- /rsi:rule -->\n"), 0o644)
	_ = os.MkdirAll(filepath.Join(vaultDir, "skills", "inbox-triage"), 0o755)
	_ = os.WriteFile(filepath.Join(vaultDir, "skills", "inbox-triage", "SKILL.md"),
		[]byte("---\nname: inbox-triage\ndescription: rogue\n---\n"), 0o644)

	// Write >5 rules into general.md to verify LoadGeneralOverlayRules caps at 5
	var genLines []string
	for i := 1; i <= 7; i++ {
		genLines = append(genLines, `<!-- rsi:rule id="rsi-gen-`+string(rune('0'+i))+`" -->`)
		genLines = append(genLines, "- General rule")
		genLines = append(genLines, "<!-- /rsi:rule -->")
	}
	_ = os.WriteFile(filepath.Join(rulesDir, "general.md"), []byte(strings.Join(genLines, "\n")), 0o644)

	mbRes := ResolveSkillDispatch(vaultDir, "morning-brief")
	if !mbRes.HasOverlay || len(mbRes.OverlayRules) != 2 || mbRes.ActiveRules != 2 ||
		!strings.Contains(mbRes.OverlayRules[0], "rsi-mb-1") || !strings.Contains(mbRes.OverlayRules[1], "rsi-mb-unclosed") {
		t.Fatalf("unexpected morning-brief overlay resolution: %+v", mbRes)
	}

	liRes := ResolveSkillDispatch(vaultDir, "ledger-ingest")
	if liRes.SkillOrigin != "vault" || liRes.ActiveTarget != "vault/ledger-ingest" || liRes.ActiveRules != 1 {
		t.Fatalf("unexpected Vault skill resolution for ledger-ingest: %+v", liRes)
	}

	itRes := ResolveSkillDispatch(vaultDir, "inbox-triage")
	if itRes.HasOverlay || len(itRes.OverlayRules) != 0 || itRes.SkillOrigin != "core" {
		t.Fatalf("expected inbox-triage to ignore Vault skill and overlay rules, got: %+v", itRes)
	}

	genRules := LoadGeneralOverlayRules(vaultDir, 5)
	if len(genRules) != 5 {
		t.Fatalf("expected LoadGeneralOverlayRules capped at 5, got %d", len(genRules))
	}
}

func TestOverlayWildcardAndExclusionScopes(t *testing.T) {
	vaultDir := t.TempDir()
	packDir := filepath.Join(t.TempDir(), "acme-work")
	body := "# Work Task Policy\n" + strings.Repeat("- Always verify changes against live tests.\n", 45)
	if len(body) <= 1800 || len(body) >= MaxOverlayBytes {
		t.Fatalf("unexpected fixture size %d", len(body))
	}
	writePackFiles(t, packDir, map[string]string{
		"plugin.json": `{"name":"acme-work","version":"1.0.0"}`,
		"hq-extension.json": `{
  "schema_version": 1,
  "id": "acme-work",
  "version": "1.0.0",
  "components": {
    "front_desk_overlays": [
      {"id": "task-wild", "path": "hq-overlays/tasks.md", "scope": ["task:*", "!task:repo-core"], "max_bytes": 4096}
    ]
  }
}`,
		"hq-overlays/tasks.md": body,
	})
	if _, err := EnablePack(context.Background(), vaultDir, "acme-work", packDir, false); err != nil {
		t.Fatalf("EnablePack: %v", err)
	}
	bundle, err := BuildTrustBundle(vaultDir, "acme-work", packDir)
	if err != nil {
		t.Fatalf("BuildTrustBundle: %v", err)
	}
	rawBundle, _ := json.Marshal(bundle)
	if err := ApplyTrustBundle(vaultDir, rawBundle); err != nil {
		t.Fatalf("ApplyTrustBundle: %v", err)
	}

	if got := LoadActiveFrontDeskOverlays(vaultDir, "task:billing"); len(got) != 1 || !strings.Contains(got[0], "Always verify changes") {
		t.Fatalf("expected task:* wildcard to match task:billing, got %+v", got)
	}
	if got := LoadActiveFrontDeskOverlays(vaultDir, "task:repo-core"); len(got) != 0 {
		t.Fatalf("expected !task:repo-core exclusion to suppress overlay, got %+v", got)
	}
	if got := LoadActiveFrontDeskOverlays(vaultDir, "front_desk"); len(got) != 0 {
		t.Fatalf("expected task:* not to match front_desk, got %+v", got)
	}

	// Reject malformed / exclusion-only scope lists during LintPack.
	badPack := filepath.Join(t.TempDir(), "bad-scope")
	writePackFiles(t, badPack, map[string]string{
		"plugin.json": `{"name":"bad-scope","version":"1.0.0"}`,
		"hq-extension.json": `{
  "schema_version": 1,
  "id": "bad-scope",
  "version": "1.0.0",
  "components": {
    "front_desk_overlays": [
      {"id": "only-excl", "path": "hq-overlays/tasks.md", "scope": ["!task:repo-core"]}
    ]
  }
}`,
		"hq-overlays/tasks.md": "# Policy\n",
	})
	if _, err := LintPack(badPack); err == nil || !strings.Contains(err.Error(), "non-negated") {
		t.Fatalf("expected LintPack to reject exclusion-only scope, got %v", err)
	}
}

func TestRefreshPackFollowsSymlinkSourcePath(t *testing.T) {
	vaultDir := t.TempDir()
	base := t.TempDir()
	v1 := filepath.Join(base, "snap-100")
	v2 := filepath.Join(base, "snap-200")
	link := filepath.Join(base, "latest")

	writePackFiles(t, v1, map[string]string{
		"plugin.json":       `{"name":"acme-refresh","version":"1.0.0"}`,
		"hq-extension.json": `{"schema_version":1,"id":"acme-refresh","version":"1.0.0","components":{}}`,
	})
	writePackFiles(t, v2, map[string]string{
		"plugin.json":       `{"name":"acme-refresh","version":"1.1.0"}`,
		"hq-extension.json": `{"schema_version":1,"id":"acme-refresh","version":"1.1.0","components":{}}`,
	})
	if err := os.Symlink(v1, link); err != nil {
		t.Fatal(err)
	}

	m1, err := EnablePack(context.Background(), vaultDir, "acme-refresh", link, true)
	if err != nil || m1.Version != "1.0.0" {
		t.Fatalf("EnablePack v1: m=%+v err=%v", m1, err)
	}
	st1, err := LoadVaultState(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	act1 := st1.Packs["acme-refresh"]
	if act1.SourcePath != filepath.Clean(link) || act1.Path == act1.SourcePath || !act1.UsePackRouting {
		t.Fatalf("expected SourcePath=%q distinct from resolved Path=%q with UsePackRouting=true, got %+v", link, act1.Path, act1)
	}

	// Flip the symlink to v2 and call RefreshPack without an explicit path.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(v2, link); err != nil {
		t.Fatal(err)
	}

	m2, prop, bundle, err := RefreshPack(context.Background(), vaultDir, "acme-refresh", "", nil, "conv-refresh")
	if err != nil {
		t.Fatalf("RefreshPack v2: %v", err)
	}
	if m2.Version != "1.1.0" || prop.ID == "" || bundle.Target.SourceID != "pack:acme-refresh" {
		t.Fatalf("unexpected RefreshPack result: m=%+v prop=%+v bundle=%+v", m2, prop, bundle.Target)
	}
	st2, err := LoadVaultState(vaultDir)
	if err != nil {
		t.Fatal(err)
	}
	act2 := st2.Packs["acme-refresh"]
	if act2.Version != "1.1.0" || !act2.UsePackRouting || act2.SourcePath != filepath.Clean(link) || !strings.HasSuffix(act2.Path, "snap-200") {
		t.Fatalf("expected RefreshPack to preserve UsePackRouting & SourcePath and advance Path to snap-200, got %+v", act2)
	}
}
