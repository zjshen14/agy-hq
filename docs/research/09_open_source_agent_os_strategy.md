# 09 — Open-Source Personal Agent OS Strategy: Forensic Privacy Audit, Developer Onboarding & Launch Playbook

- **Date:** 2026-11-10
- **Task Scope:** `[Task:oss] Open Source Strategy & Launch Plan` (`hq/open-source-strategy`)
- **Companion Specification:** [RFC-0013: Open-Source Readiness, Forensic Privacy Sanitization, Governance & Launch Architecture](../design/0013-open-source-launch-strategy.md)

---

## 1. Executive Summary

Open-sourcing a **Personal Agent OS** presents a fundamentally harder security and privacy challenge than open-sourcing a traditional library or CLI tool. Because the author uses `agy-hq` daily as a live life-and-engineering operating system—managing family schedules, Google Chat spaces, private investment theses, tax workpapers, and multi-repo software engineering—the boundary between the **public engine (`agy-hq`)** and the **private user Vault (`local/`)** is continuously tested under real workloads.

Before transitioning `github.com/zjshen14/agy-hq` from a private repository to a public open-source release, we conducted a **full forensic audit of all tracked files (`git ls-files`) and the complete Git object graph (`git log --all -p`)** on `HEAD` (`4a8642c`).

### Key Empirical Findings
1. **Structural Vault Separation (`P1`–`P5`) Passed, But Relational/Contextual PII (`P6`) Leaked into 13 Tracked Files:**
   - While [`internal/ci/pii.go`](../../internal/ci/pii.go) (implemented in [RFC-0011](../design/0011-ci-pipeline-and-regression-harness.md)) successfully prevented any file under `local/`, any workstation `$HOME` path, any API key, or any unapproved email address from remaining in `HEAD`, **contextual personal identifiers** (real family member names, real Google Chat Space IDs, a personal username handle, and private project slugs) were used as realistic examples in `AGENTS.md`, `.agents/plugins/agy-hq/skills/*/SKILL.md`, `cmd/hq/main.go`, `docs/`, and unit test fixtures (`internal/gchat/gchat_test.go`, `internal/webui/webui_test.go`).
2. **Historical Git Commit Diffs Contain Scrubbed Personal Emails & Relational PII:**
   - Commits `fced00e` and `b3c7550` (prior to RFC-0011's email allowlist) committed a real personal Gmail address (`<personal-handle>@gmail.com`) into tracked files. Although removed from `HEAD` in commit `2cea73a`, those strings remain permanently visible in `git log -p` unless the Git history is sanitized prior to public release.
3. **Subprocess `GIT_DIR` Environment Bleed Created Orphaned Test Commits in Root `.git`:**
   - Prior to commit `4a8642c`, running `go test ./...` from inside a git hook caused `exec.Command("git", "init", ...)` inside `vault.Init` (`internal/vault/vault.go`) and `worktree.Add` (`internal/worktree/worktree.go`) to inherit `GIT_DIR` from the parent hook process, writing temporary test commits (`d774976`, `7fdd069`, `32d60f5` on branch `hq/sample-repo-fix-bug`) into the root `.git` object store.
4. **Template & Port Configuration Drift in `templates/`:**
   - `templates/config.yaml` still specifies `listen_addr: "127.0.0.1:4747"` whereas the daemon, WebUI, and Antigravity Sidecar standardized on `127.0.0.1:4242`.
   - `templates/USER.md` references a personal topic filename (`<family-topic>.md`).

---

## 2. Forensic Privacy & Git History Audit (`HEAD` `4a8642c`)

### 2.1 Why Existing PII Gates (`P1`–`P5`) Missed Contextual Leaks

In [RFC-0011 §4](../design/0011-ci-pipeline-and-regression-harness.md), `CheckPII` (`internal/ci/pii.go`) defined five syntactic invariants:
- **`P1-ForbiddenPath`:** Blocks `local/`, `vault/`, `bin/`, `.env*`, `*.pem`, `*.key`, `client_secret*.json`, `token*.json`.
- **`P2-VaultIgnore` / `P2-SubmoduleLeak`:** Enforces `/local/` and `/vault/` in `.gitignore` and rejects gitlink submodules (`mode 160000`).
- **`P3-WorkstationPath`:** Blocks `/home/<user>/`, `/Users/<user>/`, and `$HOME`.
- **`P4-SecretPattern` / `P4-SSNPattern`:** Blocks OAuth tokens (`ya29.*`, `1//*`), API keys (`AIza*`, `ghp_*`, `sk-*`), PEM blocks, and SSNs.
- **`P5-EmailAllowlist`:** Restricts email literals (`*@*.*`) to a strict synthetic allowlist (`@example.com`, `alice@gmail.com`, etc.).

**The Gap:** Syntactic regexes for paths, tokens, and email addresses cannot detect **relational/contextual PII**—such as a child's first name in a school project chat space (`"<Child-Name> Science Project"`), a family chat group name (`"<Family-Group-Chat>"`), a spouse's full name (`"<Spouse-Full-Name>"`), or an opaque 11-character Google Chat Space ID (`spaces/<real-space-id-1>`, `spaces/<real-dm-id-2>`, `spaces/<real-dm-id-3>`) that maps to a private family conversation.

### 2.2 Complete Inventory of Contextual Leaks in Tracked Files (`HEAD`)

| Category | Leaked Identifier Pattern(s) (Redacted) | Tracked Files Affected on `HEAD` (`4a8642c`) | Remediation Target |
| :--- | :--- | :--- | :--- |
| **C1: Family & Contact Names** | `"<Child-Name> Science Project"`, `"<Child-Name> Science Fair"`, `"<Child-Name> Group"`, `"<Child-Name> Game Finder"`, `"<Family-Group-Chat>"`, `"<Spouse-Full-Name>"`, `"<Spouse-First-Name>"`, `"<Author-First-Name>"` | • `AGENTS.md`<br>• `.agents/plugins/agy-hq/skills/gchat/SKILL.md`<br>• `cmd/hq/main.go`<br>• `docs/architecture.md`<br>• `docs/design/0008-gchat-integration.md`<br>• `docs/design/0009-task-board-redesign.md`<br>• `docs/research/06_taskboard_and_console_audit.md`<br>• `internal/gchat/gchat.go`<br>• `internal/gchat/gchat_test.go`<br>• `internal/webui/webui_test.go` | Replace with synthetic engineering/team personas: `"Project Launch Space"`, `"Engineering Core"`, `"Alice Chen"`, `"Alice"`, `"Alex"`. |
| **C2: Real Google Chat Space IDs** | 3 real 11-char Google Chat `spaces/<id>` literals | • `.agents/plugins/agy-hq/skills/gchat/SKILL.md`<br>• `docs/design/0008-gchat-integration.md`<br>• `internal/gchat/gchat.go`<br>• `internal/gchat/gchat_test.go`<br>• `internal/webui/webui_test.go` | Replace with synthetic space IDs: `spaces/AAAA_LAUNCH_01`, `spaces/AAAA_DM_ALICE`, `spaces/AAAA_DM_BOB`. |
| **C3: Personal Handle** | Personal Gmail username handle (and historical `<personal-handle>@gmail.com`) | • `AGENTS.md`<br>• `.agents/plugins/agy-hq/skills/gchat/SKILL.md`<br>• `internal/gchat/cdp_ws.go`<br>• Historical commits `fced00e`, `b3c7550` | Replace with synthetic handle `"alex_dev"` / `"alice@gmail.com"`. *(Note: GitHub repo owner `zjshen14` in `go.mod` and repo URLs is intentional public attribution).* |
| **C4: Personal Vault Topic & Task Slugs** | Personal family topic `.md` filename, private project slug, and personal task IDs | • `AGENTS.md`<br>• `README.md`<br>• `templates/USER.md`<br>• `.agents/plugins/agy-hq/skills/sleep-consolidate/SKILL.md`<br>• `.agents/plugins/agy-hq/skills/thesis-check/SKILL.md`<br>• `docs/architecture.md`<br>• `docs/roadmap.md` | Replace with generic workspace examples: `project-notes.md`, `webapp-redesign`, `sleep-consolidate`, `deal-flow-watch`. |

### 2.3 Git History Contamination & Remediation Options

Even after scrubbing all 13 files in a new commit on `main`, anyone who clones the repository can run `git log -p` and recover:
1. The personal `@gmail.com` address from commits `fced00e` and `b3c7550`.
2. Every family name, space name, and real Google Chat Space ID from commits `de2ed04`, `36d5078`, `effecc2`, `b323001`, `d2e0687`, `fced00e`, `b3c7550`, `7e061c3`, and `d8c0f9b`.
3. Orphaned test branch `refs/heads/hq/sample-repo-fix-bug` (`32d60f5`, `7fdd069`, `d774976`).

We evaluate two mechanical history-remediation strategies prior to flipping the repository to Public:

| Strategy | Mechanism | Pros | Cons |
| :--- | :--- | :--- | :--- |
| **Option A: Automated History Scrub (`git filter-repo --replace-text` / `hq ci scrub-history`)** | Rewrites all ~55 commits on `main` using a deterministic replacement table (`replacements.txt`) that maps every C1–C4 identifier and email to its synthetic equivalent across all historical blobs and commit messages, and deletes stray test refs. | Preserves the full chronological engineering evolution (all 55 commits, RFC progression, and commit timestamps) showing how `agy-hq` was built. | Requires force-pushing rewritten SHAs to `origin/main` and recreating active worktrees (`local/worktrees/*`). |
| **Option B: Clean Epoch Squash ("Genesis Release `v0.1.0`")** | Creates a single orphan commit (or a curated 5-commit architectural epoch sequence) from the fully scrubbed tree as the public root commit, archiving pre-release history in a private backup remote. | Mathematically impossible for any deleted historical blob or reverted diff to leak; ultra-small `.git` packfile size (`< 500 KB`). | Loses granular per-PR commit history in the public repo. |

---

## 3. Open-Source Licensing & Governance Benchmark

### 3.1 License Comparison for Agent Infrastructure (`MIT` vs. `Apache-2.0`)

We benchmarked the licensing models of comparable developer tools, Go microkernels, and agent frameworks (LangChain/LangGraph, CrewAI, AutoGen, Ollama, Hugo, Docker/Moby, Kubernetes):

| Dimension | **Apache License 2.0** | **MIT License** | **Dual (`MIT OR Apache-2.0`)** |
| :--- | :--- | :--- | :--- |
| **Developer Adoption & Friction** | Very High (CNCF, Google, Kubernetes, Go ecosystem standard) | Highest (shortest text, zero-ceremony) | High (Rust ecosystem standard) |
| **Express Patent Grant & Retaliation (§3)** | **Yes** — Contributors grant an explicit patent license; license terminates if a user sues the project for patent infringement. | **No** — Relies on implied license doctrine; no defensive patent termination clause. | **Yes** (via Apache-2.0 leg) |
| **Brand & Trademark Protection (§6)** | **Yes** — Explicitly states the license does not grant rights to use project trademarks (`agy-hq` name & Asymmetric Wing Cradle logo). | **No** — Silent on trademarks. | **Yes** |
| **Modification Notice Requirement (§4)** | Requires redistributors to retain `NOTICE` and state significant file modifications. | Only requires retaining copyright notice. | Permissive |

### 3.2 Security Policy (`SECURITY.md`) as a Product Differentiator

Most open-source personal agents treat security as an afterthought—exposing users to Simon Willison's **"Lethal Trifecta"** (access to private data + exposure to untrusted content + ability to communicate externally). By publishing a formal `SECURITY.md` that documents:
1. **Ring 0 Trusted Computing Base (TCB) boundaries** (`internal/hooks`, `internal/approvals`, `internal/ci`, `.agents/hooks.json`),
2. **Quarantine Taint Isolation** (`hq gmail read`, `hq gchat read-quarantined`), and
3. **Cryptographic SHA-256 Outbound Approvals** (`hq propose` + `hq approve --sha256`),
`agy-hq` turns its security architecture into a primary reason developers trust it with their personal machine.

---

## 4. Competitive Positioning & Value Proposition Analysis

### 4.1 Why Existing Personal Agent Approaches Fall Short

| Approach | Examples | Core Bottleneck | How `agy-hq` Solves It |
| :--- | :--- | :--- | :--- |
| **1. Heavy Standalone Agent Frameworks** | LangGraph, AutoGen, CrewAI | Require maintaining custom Python orchestration servers, vector DB containers, custom web UIs, and separate API billing; poor native IDE/filesystem ergonomics. | **Option 1.5 Hybrid on Antigravity ([ADR-0001](../decisions/0001-option-1.5-hybrid-on-antigravity.md)):** Piggybacks on Google Antigravity's native agent runtime, Seatbelt sandbox, MCP ecosystem, and mobile Remote Control PWA—adding a **zero-dependency Go microkernel (`./bin/hq`)**. |
| **2. Single-Thread Chat Wrappers & "Claws"** | OpenClaw, custom Telegram/Slack bots | Everything runs in one giant chat thread. Coding or deep research blocks the chat for 10 minutes; background logs pollute the conversation; high token burn from polling. | **Non-Blocking `[HQ] Front Desk` + Isolated Git Worktrees:** `PreToolUse` mechanically caps the Front Desk at `<= 3` lookups and blocks inline coding/testing, routing work to background subagents or dedicated `[Task:<scope>]` sidebar conversations in `local/worktrees/<repo>-<task>/`. |
| **3. Cloud-Hosted Personal Assistants** | Notion AI, Lindy, Dust | Your bank CSVs, tax returns (`W-2`/`1099`), investment theses, and personal journals live on third-party SaaS servers; cannot edit local git repos. | **Two-Layer Vault Split ([ADR-0003](../decisions/0003-two-layer-oss-and-private-vault.md)):** 100% of personal state lives in an untracked, local private git repo (`local/`) protected by mathematical pre-commit/pre-push PII gates. |
| **4. Prompt-Only "Dotfiles" Repos** | `.cursorrules`, static `CLAUDE.md` gists | Statically injected prompts rot quickly, lack background daemons, cannot enforce outbound safety mechanically, and blow up context windows as `MEMORY.md` grows. | **4-Layer Memory + Silent Nightly Dreaming + Bounded RSI:** `MEMORY.md` stays `<= 80` lines as a Roadmap Index; nightly `sleep-consolidate` verifies turn journals against raw transcripts and distills `local/memory/topics/*.md`; `hq rsi` mines friction and verifies skill improvements in a 4-gate sandbox. |

---

## 5. 5-Minute Onboarding & Launch Funnel Analysis

Empirical studies of open-source developer tool adoption (e.g., Homebrew, Ollama, Supabase, Astro) show a sharp drop-off whenever first-time setup requires:
1. Installing multiple language runtimes or Docker containers,
2. Configuring OAuth credentials *before* seeing the UI work, or
3. Staring at an empty dashboard ("blank-canvas paralysis").

To achieve a **true `< 5-minute` time-to-magic**, `agy-hq` requires:
- **Zero-Config Demo Mode (`./bin/hq init --demo` / `./scripts/bootstrap.sh --demo`):** Seeds the untracked `local/` Vault with realistic synthetic artifacts (a sample `Acme-2026-10-Q.html` in `local/inbox/`, 3 realistic tasks on the Task Board, 1 pending SHA-256 outbound approval in `local/memory/pending/`, and a sample `MEMORY.md` + topic wiki page) so that running `./bin/hq webui` or opening Antigravity immediately displays a rich, interactive Personal Agent OS console.
- **Pre-Built Release Binaries (`goreleaser` / GitHub Releases):** In addition to building from source in `0.8s` with Go, providing pre-compiled static binaries (`darwin/arm64`, `darwin/amd64`, `linux/amd64`, `linux/arm64`) allows instant installation even before Go is installed.
