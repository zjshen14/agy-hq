# 11 — Antigravity Global Plugin Packaging & Vault Isolation Audit

- **Date:** 2026-11-13
- **Author:** `[Task:agy-hq] Package agy-hq as Global Plugin` (`gemini-4-carbon-high`)
- **Related RFC:** [RFC-0015: Standalone Global Antigravity Plugin Packaging, Embedded Templates & Workspace-Agnostic Vault Resolution](../design/0015-global-plugin-packaging.md)

---

## 1. Executive Summary

Today, `agy-hq` operates primarily when Google Antigravity is opened directly inside the `agy-hq` repository checkout (or an isolated worktree created under `local/worktrees/`). Packaging `agy-hq` as a standalone, globally installable Antigravity plugin (`~/.gemini/config/plugins/agy-hq/` paired with `~/.local/bin/hq` on `PATH`) unlocks full Personal Agent OS capabilities—9 progressive-disclosure skills, 0-token lifecycle hooks, outbound SHA-256 approval gates, turn journaling, and the `:4242` HQ Console sidecar—across **every** workspace on a user's machine without cloning `agy-hq` into third-party repositories.

Before authoring [RFC-0015](../design/0015-global-plugin-packaging.md), we audited the upstream Antigravity customization loader (`agy` CLI `1.5.6` and Antigravity Desktop `2.31.2`) alongside every path, hook, CI gate, and Vault resolver in `agy-hq`. That audit surfaced **seven concrete architectural and validation defects** that must be solved together for safe global packaging.

---

## 2. Upstream Antigravity Plugin Loader Findings (`agy` `1.5.6` / Desktop `2.31.2`)

| Mechanism | Empirical Upstream Behavior | Packaging Implication for `agy-hq` |
| :--- | :--- | :--- |
| **Plugin Manifest & Validation** | `agy plugin validate --strict <dir>` parses `plugin.json` (JSONC), checks `hooks.json` command paths relative to `<plugin_dir>`, validates `rules/`, `skills/`, and `sidecars/`, and parses optional `sidecars.json` as `customizations.JSONConfig` (`{"entries": [...], "inherits": [...]}`). | `.agents/plugins/agy-hq/sidecars.json` (currently a JSON array `[{...}]`) fails validation and is ignored by the loader; `sidecars/agy-hq-console/sidecar.json` is already auto-discovered natively as `agy-hq/agy-hq-console`. |
| **Hook Working Directory & Env** | Command hooks in `<plugin_dir>/hooks.json` execute with `cwd = <plugin_dir>`, injecting `PLUGIN_ROOT` (`<plugin_dir>`) and `PLUGIN_DATA` into the subprocess environment (without string-substituting them inside `command`). | Relative `./bin/hq` or `../../../bin/hq` paths fail outside the `agy-hq` checkout and trigger `agy plugin validate` warnings. Hooks must resolve `hq` via `PATH` (`~/.local/bin/hq`), `$PLUGIN_ROOT/bin/hq`, or a guarded repo-dev check. |
| **`PLUGIN_DATA` Surface Split** | Per upstream hook documentation, app-data roots differ by host surface: `~/.gemini/antigravity/` (Desktop 2.0), `~/.gemini/antigravity-cli/` (`agy` CLI), and `~/.gemini/antigravity-ide/` (IDE). Moreover, `PLUGIN_DATA` is unset in normal user shells (`hq status`) and `systemd`/`launchd` (`hq daemon`). | Storing the user's primary private Vault inside `$PLUGIN_DATA/vault` would cause **3-way cross-surface split-brain** and risk data loss on `agy plugin uninstall`. The global Vault must live at a surface-agnostic path (`~/.gemini/antigravity/hq/vault`) backed by a pointer config (`~/.gemini/antigravity/hq/config.json`). |
| **Plugin Namespace & Priority** | Plugin `name` in `plugin.json` shares one global namespace. Discovery priority is: (1) Workspace `.agents/plugins/*`, (2) Declared `plugins.json`, (3) Global `~/.gemini/config/plugins/*`. On `name` collision, the first discovered plugin wins and duplicates are dropped silently; `config.json` enablement is keyed by directory name (`agy-hq`). | Keeping identical `name: "agy-hq"` and directory `agy-hq/` across workspace and global installs ensures clean shadowing (1x load) when developing inside `agy-hq`, whereas renaming either copy would load **both** plugins simultaneously (2x skills, 2x rules, 2x hooks). |
| **Hook Merging Across Sources** | Named hooks from *separate* active customization sources (e.g., workspace `.agents/hooks.json` **plus** active plugin `.agents/plugins/agy-hq/hooks.json`) are merged and executed sequentially (`hooks.md`). | Having both `.agents/hooks.json` and `.agents/plugins/agy-hq/hooks.json` in the `agy-hq` repo causes `SessionStart`, `PreToolUse`, and `Stop` to execute **twice** on every turn inside `headquarter`. |
| **Plugin Rules (`rules/AGENTS.md`)** | Plain Markdown (no YAML frontmatter), capped at `24,000` bytes per file and sharing the `20,000`-token global rules budget. Deduplicated by resolved file path, **not** by prose similarity. | Global OS rules belong in `.agents/plugins/agy-hq/rules/AGENTS.md`, while root `AGENTS.md` must slim down to `agy-hq` contributor/CI rules to avoid double-billing ~2,500 tokens inside `headquarter`. |

---

## 3. Seven Codebase Defects Identified in Preflight Audit

### Defect 1: `agy plugin validate .agents/plugins/agy-hq` Fails (1 Error, 2 Warnings)
Running `agy plugin validate --strict .agents/plugins/agy-hq` on `HEAD` (`bdcd98d`) fails:
- `✗ sidecars.json: json: cannot unmarshal array into Go value of type customizations.JSONConfig. The loader ignores this file`
- `⚠ hooks.json: hook "hq-kernel": ./bin/hq doesn't exist. Hook commands run in the plugin folder` (`PreToolUse` and `Stop`)

### Defect 2: Live Double-Hook Execution Inside `headquarter` (`.agents/hooks.json` + Plugin `hooks.json`)
Because both [`.agents/hooks.json`](../../.agents/hooks.json) and [`.agents/plugins/agy-hq/hooks.json`](../../.agents/plugins/agy-hq/hooks.json) are active inside the `headquarter` workspace, Antigravity merges and executes both:
1. **`SessionStart` 2x Token Tax:** `[agy-hq Session Context Loaded]` (`USER.md`, `MEMORY.md`, Wiki index, 25 facts, worktrees, tasks, triggers) is injected **twice** at the start of every `headquarter` session (~2,500–3,500 wasted prompt tokens).
2. **`PreToolUse` 2x Lookup Counter Increment:** On `[HQ] Front Desk`, `turnState.Count++` in [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go) increments twice for a single `read_file`, prematurely tripping the 3-lookup cap on the 2nd lookup (`count = 4 > 3`).

### Defect 3: `ResolveRoot()` & `ResolveVaultDir()` Third-Party `go.mod` & Worktree Traps
In [`internal/vault/vault.go`](../../internal/vault/vault.go):
- `ResolveRoot()` walks up from `cwd` looking for **any** `go.mod` file (without checking `module github.com/zjshen14/agy-hq`), falling back to `cwd`, and `ResolveVaultDir()` returns `filepath.Join(root, "local")`.
- Once `hq` is installed on `PATH` (`~/.local/bin/hq`), opening *any* third-party Go project (e.g., `~/Workspaces/opencli`) or non-Go folder (`~/Documents`) causes `SessionStart` to auto-initialize a stray `./local/` Vault and private `.git` repo inside that unrelated directory!
- Furthermore, running bare `hq` inside an `agy-hq` git worktree (`<vault>/worktrees/headquarter-<task>`) matches `<worktree>/go.mod` and resolves to `<worktree>/local` instead of the enclosing `<vault>`.

### Defect 4: Hardcoded `./bin/hq` Regexes in `PreToolUse` Security & Front Desk Exemptions
Updating the 9 bundled `SKILL.md` files from `./bin/hq` to `hq` without updating [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go) would break three runtime regexes:
- `reAllowedQuarantineHQCmd` (`^\s*(?:\./)?bin/hq\s+...`): would **deny** `hq gmail draft` or `hq propose` inside quarantined reader sessions!
- `reQuarantinedReadCmd` (`^\s*(?:\./)?bin/hq\s+(?:gmail|gchat)\s+read-quarantined\b`): would miss taint attribution in transcript audits when invoked as `hq gmail read-quarantined`!
- Front Desk `isHQCmd` (`strings.HasPrefix(trimmed, "./bin/hq") || strings.HasPrefix(trimmed, "bin/hq")`): would treat `hq status` or `hq gchat propose` on `[HQ] Front Desk` as a non-HQ shell lookup, consuming the 3-call turn cap!

### Defect 5: Third-Party Workspace Hijack on Vacant Front Desk Lease & `.git/hooks` Clobber
- **Vacant Front Desk Hijack:** In [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go) (`HandleSessionStart` L167 & `HandleStop` L1067), if `front_desk.json` is vacant (`!ok`), the first non-subagent conversation automatically claims the `[HQ] Front Desk` lease. Under global plugin installation, a user opening `~/projects/webapp` to write code before ever creating a Front Desk thread would have their coding session hijacked into `[HQ] Front Desk`, blocking `edit_file` and `npm test`!
- **Third-Party `.git/hooks` Overwrite:** In [`cmd/hq/main.go`](../../cmd/hq/main.go) (`init` L76, `migrate` L104) and [`internal/vault/vault.go`](../../internal/vault/vault.go) (L249), `InstallGitHooks(rootDir)` runs unconditionally—which would overwrite `.git/hooks/pre-commit` and `pre-push` in a third-party repository if `rootDir` resolved to `cwd`.

### Defect 6: Unembedded `templates/` Directory
`hq init`, `hq migrate`, and `HandleSessionStart` auto-bootstrap read `templates/{USER.md,MEMORY.md,config.yaml,triggers.yaml,gitignore.tmpl}` from `filepath.Join(rootDir, "templates")` on disk. A standalone `~/.local/bin/hq` binary executed outside the `agy-hq` checkout cannot bootstrap or migrate a Vault unless `templates/` is embedded into the binary via Go's stdlib `//go:embed`.

### Defect 7: Sidecar Binary Discovery (`sidecars/agy-hq-console/main.mjs`)
[`main.mjs`](../../.agents/plugins/agy-hq/sidecars/agy-hq-console/main.mjs) only checks `HQ_BIN_PATH` and parent directories (`bin/hq`) up from `__dirname`, falling back to `process.cwd()/bin/hq`. When installed at `~/.gemini/config/plugins/agy-hq/sidecars/agy-hq-console/main.mjs`, walking up `__dirname` never finds `~/.local/bin/hq`, leaving the HQ Console Aux Pane stuck on `502 Connecting to agy-hq daemon...` unless `hq daemon` was already started externally.
