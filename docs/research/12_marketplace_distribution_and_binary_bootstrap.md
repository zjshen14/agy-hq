# 12 — Antigravity Marketplace Distribution, Repository Layout & Verified Binary Bootstrap Audit

- **Date:** 2026-11-14
- **Author:** `[Task:agy-hq] Marketplace Distribution & Binary Bootstrap` (`gemini-4-carbon-high`)
- **Related RFC:** [RFC-0016: Antigravity Marketplace Distribution, Automated GitHub Release Matrix & Verified Binary Bootstrap](../design/0016-marketplace-distribution-and-binary-bootstrap.md)
- **Builds On:** [11 — Antigravity Global Plugin Packaging & Vault Isolation Audit](./11_plugin_packaging_audit.md), [RFC-0015: Standalone Global Antigravity Plugin Packaging](../design/0015-global-plugin-packaging.md)

---

## 1. Executive Summary

[RFC-0015](../design/0015-global-plugin-packaging.md) decoupled `agy-hq` from a single repository checkout—introducing embedded templates (`//go:embed`), workspace-agnostic 5-step Vault resolution (`ResolveVaultDir()`), project-scoped vs. opt-in global installation (`hq plugin install`), and sidecar daemon environment isolation.

However, `RFC-0015` still assumes the user **cloned the `agy-hq` repository and compiled `hq` locally with a Go toolchain** (`./scripts/bootstrap.sh` or `hq plugin install`). When an external user clicks **+ Add** in the **Antigravity Marketplace** (or runs `agy plugin install <github-url>`), Antigravity unpacks **only** the static plugin directory into `~/.gemini/config/plugins/agy-hq/`—it does **not** run `go build`, does **not** execute `scripts/bootstrap.sh`, and treats the unpacked plugin directory as **read-only and disposable across updates**.

To prepare `agy-hq` for public GitHub installation and submission to Google's official **[Antigravity Marketplace Partner Intake](https://forms.gle/2EX5RFYPoJe1UgxR9)**, we audited the live upstream Antigravity Marketplace specification (`antigravity.google/docs/build-plugins`, `docs/marketplace`, `docs/plugins`) and ran empirical CLI probes against `agy` `1.5.8` in isolated `$HOME` sandboxes. This document records the exact upstream rules, CLI probe outputs, repository layout tradeoffs, and supply-chain security requirements driving [RFC-0016](../design/0016-marketplace-distribution-and-binary-bootstrap.md).

---

## 2. Official Antigravity Marketplace Specification Audit (`agy` `1.5.8`)

| Requirement Area | Upstream Marketplace Rule (`antigravity.google/docs/build-plugins`) | Current `agy-hq` State (`v0.1.0` at `HEAD`) | Gap to Close in `RFC-0016` |
| :--- | :--- | :--- | :--- |
| **Submission Channel** | Validated public plugins are submitted via the **[Antigravity Marketplace Partner Intake Form](https://forms.gle/2EX5RFYPoJe1UgxR9)**; direct pre-approval installs run via `agy plugin install <github-url>`. | Local `hq plugin install` only; no GitHub Releases binary artifacts yet. | Publish verified multi-OS release binaries + clean GitHub install URL + Partner Intake submission checklist. |
| **Manifest JSON Schema** | `https://antigravity.google/schemas/v1/plugin.json` enforces `"additionalProperties": false` and `"required": ["name"]`. Allowed keys: `$schema`, `name`, `displayName`, `description`, `version`, `disabled`, `logo`, `suggestedPrompts` (`maxItems: 3`), `skills`, `author`, `homepage`, `repository`, `license`, `keywords`, `extensions`. | All keys in [`.agents/plugins/agy-hq/plugin.json`](../../.agents/plugins/agy-hq/plugin.json) are schema-legal, `description` is `159` chars (within the `120–160` char target), and `suggestedPrompts` has `3` items; missing `$schema`. | Add `"$schema": "https://antigravity.google/schemas/v1/plugin.json"` and enforce schema key allowlist in CI Gate `H2-C`. |
| **Branding (`displayName`)** | Human-readable product/brand title shown on cards instead of `name`. Must **not** prefix with `"Antigravity"`, `"Gemini"`, `"Claude"`, or `"Cursor"`, and must omit suffixes like `"Plugin"` or `"MCP"`. | `"displayName": "agy-hq (Headquarter)"` leads with the machine slug `agy-hq`. | Update `"displayName"` to `"Headquarter"` (clean brand title while keeping `"name": "agy-hq"`). |
| **Bundled Logo Asset** | Relative path to a bundled `1:1` square asset (`>= 128x128px`) with `~10%` transparent margin, legible on Light and Dark themes. | [`.agents/plugins/agy-hq/assets/logo.svg`](../../.agents/plugins/agy-hq/assets/logo.svg) is `1024x1024` with `94px` (`9.18%`) transparent padding and dual-theme contrast border; an unused `85 KB` `logo.png` duplicate also sits in `assets/`. | Keep `assets/logo.svg` and drop the unreferenced `85 KB` `assets/logo.png` duplicate to shrink bundle size by `~70%`. |
| **Pre-Install `README.md`** | Rendered directly on the Marketplace plugin card **Overview** tab *before* installation to explain prerequisites, capabilities, and workflows. | [`.agents/plugins/agy-hq/README.md`](../../.agents/plugins/agy-hq/README.md) currently instructs users to run `./scripts/bootstrap.sh --global` from a source checkout. | Rewrite plugin `README.md` for Marketplace users (1-click install, zero-Go automatic binary bootstrap, privacy & approval guarantees). |
| **`hooks.json` & `scripts/`** | Hook commands run with `cwd = <plugin-root>` and **must use relative `./scripts/...` paths rather than `${PLUGIN_ROOT}`**; executables under `<plugin-root>/scripts/` retain `0755` permissions in Marketplace bundles. | [`.agents/plugins/agy-hq/hooks.json`](../../.agents/plugins/agy-hq/hooks.json) embeds 400-character inline `sh` one-liners referencing `${PLUGIN_ROOT:-.}` and has no `scripts/` directory. | Extract hook launcher into `.agents/plugins/agy-hq/scripts/hq-hook.sh` (`0755`) and simplify `hooks.json` to `./scripts/hq-hook.sh <event>`. |
| **Read-Only `<plugin-root>`** | Installed Marketplace plugins unpack into `~/.gemini/config/plugins/<name>`, which is **read-only and replaced atomically on every plugin update**. Never write binaries or state inside `<plugin-root>`. | Local `hq plugin install` created `<plugin-root>/bin/hq` convenience symlinks; Marketplace installs have **no `hq` binary anywhere on disk** on first launch! | Store version-pinned verified binaries outside `<plugin-root>` at `~/.gemini/antigravity/hq/bin/hq-v<SemVer>` (plus `~/.local/bin/hq` symlink when absent). |

---

## 3. Empirical CLI Probes: Monorepo Root vs. Clean Plugin Distribution (`agy` `1.5.8`)

A critical architectural question for `agy-hq` is **how the Git repository should be structured** so that:
1. External users can install seamlessly via Marketplace and `agy plugin install`,
2. `agy plugin validate --strict` passes with `0` errors and `0` warnings, **and**
3. Contributors working inside the `headquarter` Go repository keep **project-scoped** plugin loading (`.agents/plugins/agy-hq/`) without polluting `~/.gemini/config/plugins/` or double-firing hooks.

We ran four isolated empirical probes using `agy` `1.5.8` inside temporary `$HOME` sandboxes (`/tmp/hq-mkt-*`):

### Probe 1: `agy plugin install` Against Bare Monorepo Root (`https://github.com/zjshen14/agy-hq`)
```text
Cloning plugin from https://github.com/zjshen14/agy-hq.git...
Error: could not detect plugin structure at /tmp/plugin-install-1960590634
```
- **Finding:** When passed a bare `<owner>/<repo>` URL, `agy plugin install` inspects the root of the default branch for `plugin.json` or `.antigravity-plugin/plugin.json`. Because `agy-hq` nests its workspace plugin at `.agents/plugins/agy-hq/plugin.json`, bare root cloning fails.

### Probe 2: `agy plugin install` Against GitHub Tree Subpath (`https://github.com/zjshen14/agy-hq/tree/main/.agents/plugins/agy-hq`)
```text
Cloning plugin from https://github.com/zjshen14/agy-hq.git...
  [ok]    agy-hq
          ✔ skills      : 9 installed (filing-diff, gchat, inbox-triage, ledger-ingest, morning-brief, sleep-consolidate, swe-pulse, tax-prep, thesis-check)
          ✔ rules       : 1 installed (AGENTS)
          ✔ hooks       : 1 installed (hq-kernel)
          Installed to $HOME/.gemini/config/plugins/agy-hq
```
- **Finding:** `agy` `1.5.8` **natively parses GitHub `/tree/<ref>/<subpath>` URLs**! It clones the repository into a temporary staging folder, checks out `<ref>` (`main`, a release branch, or a `vX.Y.Z` tag), and copies **only `<subpath>`** (`assets/`, `hooks.json`, `plugin.json`, `README.md`, `rules/`, `sidecars/`, `skills/`) into `~/.gemini/config/plugins/agy-hq/`—completely excluding `cmd/`, `internal/`, `docs/`, and `go.mod`!

### Probe 3: Promoting `.antigravity-plugin/plugin.json` + Components to Monorepo Root
```json
{
  "manifest": ".antigravity-plugin/plugin.json",
  "name": "agy-hq",
  "components": { "agents": 0, "hooks": 1, "mcpServers": 0, "rules": 1, "sidecars": 1, "skills": 9 },
  "findings": [
    {
      "severity": "warning",
      "file": "AGENTS.md",
      "message": "never loaded: plugins load rules only from rules/. Move it to rules/AGENTS.md"
    }
  ]
}
```
- **Finding:** Moving plugin components (`skills/`, `rules/`, `hooks.json`, `sidecars/`) to the Go monorepo root introduces **three severe regressions**:
  1. **Breaks Workspace-Scoped Discovery in `headquarter`:** Per Antigravity's workspace discovery rules, opening a repository as a project discovers workspace plugins inside `.agents/plugins/*`—**not** via a root `.antigravity-plugin/plugin.json`. Contributors who intentionally keep `~/.gemini/config/plugins/` empty (project-scoped mode) would lose workspace plugin auto-discovery or require self-referential `.agents/plugins.json` workarounds.
  2. **Clones Entire Go Codebase & Docs into `~/.gemini/config/plugins/agy-hq/`:** Installing from a monorepo root copies `cmd/`, `internal/`, `docs/research/`, `docs/design/`, and `assets/branding/` (~`3.5 MB` of source/docs) into every user's `~/.gemini/config/plugins/agy-hq/`.
  3. **Fails `agy plugin validate --strict`:** Root `AGENTS.md` triggers a strict validation warning requiring `jq` exemptions in CI.

### Probe 4: Pure Standalone Bundle (`./scripts/hq-hook.sh` + `plugin.json` on `plugin-release` Branch or Mirror Repo)
```json
{
  "manifest": "plugin.json",
  "name": "agy-hq",
  "agentPluginsSpec": false,
  "components": { "agents": 0, "hooks": 1, "mcpServers": 0, "rules": 1, "sidecars": 1, "skills": 9 },
  "findings": []
}
```
- **Finding:** Publishing the pure `.agents/plugins/agy-hq/` bundle (`~36 KB`, zero Go source files) to an automated `plugin-release` branch (`https://github.com/zjshen14/agy-hq/tree/plugin-release`, plus optional mirror repo `https://github.com/zjshen14/agy-hq-plugin` if Partner Intake requires a root URL without `/tree/...`) achieves `"findings": []` under `agy plugin validate --strict --json`, zero source bloat, and zero disruption to contributors' project-scoped `headquarter` setup.

---

## 4. Zero-Go-Toolchain Binary Bootstrap: Security, Latency & Portability Analysis

Because `agy-hq`'s 0-token hooks (`SessionStart`, `PreToolUse`, `Stop`), 9 skills, and `:4242` HQ Console sidecar are powered by the pure-stdlib Go microkernel (`hq`), a Marketplace install must provision a platform-matched `hq` binary automatically on first launch without requiring Go.

### 4.1 Threat Model & Cryptographic Verification Requirements
Auto-downloading an executable inside a lifecycle hook crosses `agy-hq`'s **Ring-0 Trusted Computing Base** ([SECURITY.md](../../SECURITY.md)). Unverified `curl | sh` patterns are unacceptable. Every automatic download must enforce:
1. **Pinned Release Version:** `scripts/hq-hook.sh` reads `"version"` from its sibling `<plugin-root>/plugin.json` (e.g., `0.1.0`) and fetches **only** `v0.1.0` (`hq-<os>-<arch>` and `checksums.txt`), never floating `latest`, ensuring deterministic compatibility between bundled `SKILL.md` commands and binary CLI flags.
2. **TLS 1.2+ & Mandatory Pre-Exec SHA-256 Verification:** Download both `checksums.txt` and `hq-<os>-<arch>` into an unexecutable temporary file (`0600`) inside `~/.gemini/antigravity/hq/bin/.tmp.$$/`, compute its SHA-256 (`sha256sum`, `shasum -a 256`, or `openssl dgst -sha256`), and assert an **exact 64-hex-character match** against `checksums.txt` **before** running `chmod 0755` or atomically renaming (`mv`) into `~/.gemini/antigravity/hq/bin/hq-v<VER>`.
3. **GitHub Artifact Build Provenance:** `.github/workflows/release.yml` generates cryptographic SLSA build provenance attestations via `actions/attest-build-provenance` so security-conscious users can independently verify any release binary with `gh attestation verify hq-<os>-<arch> --repo zjshen14/agy-hq`.

### 4.2 Lifecycle Hook Timeout Budgets & Concurrency Races

| Concurrent / Timing Edge Case | Empirical Constraint | Required Safeguard |
| :--- | :--- | :--- |
| **`SessionStart` + Sidecar Simultaneous Cold Boot** | When a user enables `agy-hq` and opens a chat, Antigravity starts `sidecars/agy-hq-console/main.mjs` and fires `SessionStart` concurrently. | Use a POSIX-portable atomic directory lock (`mkdir ~/.gemini/antigravity/hq/bin/.bootstrap.lock` with PID + 60s stale-lock recovery—`flock` is absent on default macOS) shared by both `scripts/hq-hook.sh` and `main.mjs`. |
| **`PreToolUse` Tight `5s` Timeout** | `PreToolUse` runs synchronously before every tool call with `timeout: 5`. Starting a cold download on slow Wi-Fi inside `PreToolUse` risks timing out. | `SessionStart` (`timeout: 20`) and `main.mjs` perform cold downloads. `PreToolUse` checks local fast-paths (`<1ms`); if `.bootstrap.lock` is active, it waits at most `2s`, and if still absent (e.g., offline first boot), exits `0` with `{}` rather than stalling tool calls. |
| **Contributor `HEAD` Binary Protection** | Contributors run `hq upgrade` from source (`v0.1.0` + newer commits at `HEAD`). | If `../../../cmd/hq/main.go` exists **or** `~/.gemini/antigravity/hq/config.json` declares an existing `sourceRepoDir` with an executable `~/.local/bin/hq` (`SemVer >= plugin.json`), execute the local binary (`0` network calls) and **never** overwrite a regular file at `~/.local/bin/hq`. |
| **Fresh macOS Login `PATH` Gap** | On fresh macOS installs, `~/.local/bin` is often absent from GUI-inherited `$PATH`, which would cause `hq status` in `run_command` to fail with `command not found`. | Symlink `~/.local/bin/hq -> ~/.gemini/antigravity/hq/bin/hq-v<VER>` when `~/.local/bin/hq` is absent, **and** have `hq-hook.sh` pass `HQ_ORIG_PATH_MISSING=1` when `hq` was not on inherited `PATH` so `SessionStart` injects a 1-line `PATH` hint for `run_command`. |
| **Binary-Only `hq upgrade`** | Currently [`cmd/hq/main.go`](../../cmd/hq/main.go) exits non-zero in `hq upgrade` when `cmd/hq` source is absent. | Teach `hq upgrade` a dual-mode path: source `git pull + go build` when `cmd/hq` exists (preserving contributor `HEAD`), and verified GitHub Release self-upgrade + `hq migrate` when installed standalone. |
