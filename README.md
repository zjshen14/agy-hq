<p align="center">
  <img src="assets/branding/icons/icon-obsidian-128x128.png" width="120" height="120" alt="agy-hq logo" />
</p>

<h1 align="center"><code>agy-hq</code> — Personal Agent OS for Google Antigravity</h1>

<p align="center">
  Turn an always-on Mac or Linux workstation running <b>Google Antigravity</b> into a 24/7 proactive personal assistant—while keeping <b>100% of your memory, files, and credentials isolated inside a git-ignored private Vault (<code>local/</code>)</b>.
</p>

<p align="center">
  <a href="https://github.com/zjshen14/agy-hq/actions/workflows/ci.yml"><img src="https://github.com/zjshen14/agy-hq/actions/workflows/ci.yml/badge.svg" alt="CI Gate" /></a>
  <a href="LICENSE"><img src="https://img.shields.io/badge/License-Apache_2.0-blue.svg" alt="License: Apache 2.0" /></a>
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.24%2B_(0_External_Deps)-00ADD8?logo=go&logoColor=white" alt="Go 1.24+ Zero External Deps" /></a>
  <a href="docs/architecture.md"><img src="https://img.shields.io/badge/Platform-macOS_%7C_Linux-334155" alt="macOS and Linux" /></a>
</p>

<p align="center">
  <a href="#quickstart-1-line-setup">Quickstart</a> ·
  <a href="#how-agy-hq-works">How It Works</a> ·
  <a href="#two-layer-architecture-open-source-engine-vs-private-vault">Architecture</a> ·
  <a href="#built-in-skills">Built-In Skills</a> ·
  <a href="#cli-reference-binhq">CLI Reference</a> ·
  <a href="docs/README.md">Documentation Hub</a>
</p>

---

## How `agy-hq` Works

Most agent setups fail in daily life for one of three reasons: long coding/research runs block your main chat, background polling burns tokens when idle, or personal data leaks into prompt context and git history. **`agy-hq`** solves all three with a single zero-dependency Go microkernel (`bin/hq`) plugged into Antigravity's native hooks, sidecars, and Mobile Remote Control:

| Pillar | What Happens in Daily Use | Mechanical Guarantee |
| :--- | :--- | :--- |
| **1. Non-Blocking Front Desk & Worktrees** | Chat with one pinned `[HQ] Front Desk` thread from desktop or phone. Quick questions answer inline; coding and deep research fan out into isolated git worktrees (`local/worktrees/<repo>-<task>/`). | `PreToolUse` hooks cap inline lookups at `3` calls and block direct source edits or test suites on the Front Desk thread. |
| **2. 0-Token Console, Gatekeepers & Spaces** | Drop PDFs, CSVs, or 10-Qs into `local/inbox/`. Background `launchd`/`systemd` timers run SHA-256 cursor pre-checks, sweep `WaitingOn` SLAs, route attention across 4 tiers (`interrupt`, `brief_rollup`, `ideas_feed`, `silent_ledger`), and adapt Console `Spaces` by time of day. | Pure-Go gatekeepers consume **0 LLM tokens** when feeds are unchanged, waking isolated `flash` workers only on real deltas and respecting Quiet Hours. |
| **3. Approval-Bound Outbound & Credential Safety** | Emails, Google Chat messages, third-party PR merges, and releases are drafted and previewed in chat (`24h` TTL + contextual-PII warning badges) for 1-tap Mobile Remote Control approval. | `PreToolUse` hard-blocks direct outbound mutations and raw credential reads (`local/private/{gmail,gchat,credentials}/`) across **all** sessions; `./bin/hq approve <id> --sha256=<prefix>` verifies payload hashes in Go. |
| **4. 4-Layer Living Memory & Silent Dreaming** | Every turn logs a 0-token journal. At 11:30 PM, silent dreaming verifies claims against raw transcripts, updates Topic Wiki & on-demand Entity Dossiers (`hq memory lookup`), supports 4-layer `hq memory forget`, mines friction (`hq rsi`), and rolls insights into your 7:30 AM Morning Brief. | Strict **Two-Layer Vault** separation (`Invariants P1–P6`) ensures personal notes, memory, and PII in `local/` can never be committed to the public engine repo. |

---

## Quickstart (1-Line Setup)

### Prerequisites
- **OS:** macOS (`launchd`) or Linux (`systemd`)
- **Runtime:** **Google Antigravity** desktop app + **Git** + **Go 1.24+** *(auto-installed via Homebrew on macOS if missing)*

### Option A: Try Instantly with Synthetic `--demo` Data *(Recommended First Run)*

Explore the live Task Board, Inbox Triage, Thesis Check (`ACME` Form 10-Q), and SHA-256 Approval Console in under 60 seconds using synthetic `[Sample]` data:

```sh
git clone https://github.com/zjshen14/agy-hq.git headquarter && cd headquarter && ./scripts/bootstrap.sh --demo
```

### Option B: Clean Personal Vault Bootstrap *(Project-Scoped by Default)*

Installs `~/.local/bin/hq` on `PATH`, initializes `./local/`, and scopes the `agy-hq` plugin **strictly to this project** (so unrelated projects on your machine stay 100% untouched; to attach `agy-hq` to another specific project later, run `hq plugin install --project=/path/to/project`):

```sh
git clone https://github.com/zjshen14/agy-hq.git headquarter && cd headquarter && ./scripts/bootstrap.sh
```

### Option C: Optional Global Install Across Every Antigravity Workspace (`~/.gemini/config/plugins/agy-hq/`)

```sh
git clone https://github.com/zjshen14/agy-hq.git headquarter && cd headquarter && ./scripts/bootstrap.sh --global
```

Installs `~/.local/bin/hq` on `PATH` and syncs the validated `agy-hq` v0.3.1-dev plugin bundle into `~/.gemini/config/plugins/agy-hq/` (`hq plugin status` / `hq plugin validate`) so your personal Agent OS, 3 Core OS skills (plus 6 opt-in starter templates installable into `<vault>/skills/` via `hq skill add`), Console sidecar, and private Vault work seamlessly from any folder or repository.

**What `bootstrap.sh` sets up automatically:**
1. Compiles `./bin/hq` locally in `< 2s` from pure Go standard library source (`0` external `go.mod` dependencies, embedded starter `templates/`).
2. Runs `./bin/hq init` to scaffold your git-ignored private Vault (`./local/USER.md`, `./local/MEMORY.md`, `./local/CONSOLE.md`, `./local/.git`, and `0700`-permissioned private directories; or `~/.gemini/antigravity/hq/vault` via `hq init --global-vault`).
3. Installs `.git/hooks/pre-commit` and `.git/hooks/pre-push` mathematical PII & harness guards (`Invariants P1–P6`, `Gates H1–H5F`).
4. Installs the background `hq daemon` user service and runs `./bin/hq doctor` to verify environment health.

> **Zero-Touch GUI Option:** Even if you only `git clone` and open `headquarter` directly in Antigravity without running `./scripts/bootstrap.sh`, the `SessionStart` hook in [`.agents/plugins/agy-hq/hooks.json`](.agents/plugins/agy-hq/hooks.json) automatically compiles `./bin/hq`, initializes `./local/`, claims your `[HQ] Front Desk` conversation, and pins the live `hq_console.md` artifact on Turn 1.
>
> **Tip — Pin Your Front Desk:** Pin your `[HQ] Front Desk` conversation to the top of the Antigravity sidebar (via the conversation header menu) so your coordinator stays anchored above ephemeral task threads.
>
> **Live Aux Console:** Open the embedded Antigravity Sidecar pane via [`sidecar://agy-hq-console/`](.agents/plugins/agy-hq/sidecars/agy-hq-console/sidecar.json) or run `hq webui` for the standalone dashboard at `http://127.0.0.1:4242`.

---

## Two-Layer Architecture: Open-Source Engine vs. Private Vault

```text
agy-hq/                                # LAYER 1: Open-Source Agent OS (Tracked Root Git Repo)
├── AGENTS.md                          # Contributor, RFC & CI engineering rules for agy-hq
├── cmd/hq/                            # Single static Go CLI entrypoint (zero external dependencies)
├── internal/                          # Pure-stdlib Go microkernel (approvals, authz, brief, channels, ci, connector, delegates, events, extension, gchat, gmail, hooks, kernel, living, obs, offer, outbox, plugin, pulse, rsi, sandbox, store, taint, timers, vault, version, watcher, webui, workspace, worktree, etc.)
├── templates/                         # Embedded (//go:embed) starter blueprints, 6 opt-in skill templates (skills/), connectors & sidecar
├── docs/                              # PRDs (0001–0003), technical RFCs (0001–0027), ADRs (0001–0006) & research archive (01–22)
├── assets/branding/                   # Official Asymmetric Wing Cradle icon pack & social preview
├── .agents/
│   ├── skills.json                    # Registers private <vault>/skills/ (local/skills) for Antigravity discovery
│   └── plugins/agy-hq/                # Standalone plugin bundle (plugin.json, hooks.json, rules/AGENTS.md, Sidecar + 3 Core OS skills)
│
└── local/                             # LAYER 2: Your Private Vault (Git-Ignored; or ~/.gemini/antigravity/hq/vault globally)
    ├── USER.md                        # Your profile, communication preferences & goals
    ├── MEMORY.md                      # Bounded Active Priorities index (<= 40 lines / <= 2,200 bytes) auto-injected at SessionStart
    ├── CONSOLE.md                     # Zero-token live Task Board, Console Spaces & Ideas snapshot
    ├── config.yaml & triggers.yaml    # Attention/Quiet Hours, Spaces, quota budgets & 0-token cursor gatekeepers
    ├── inbox/                         # Zero-prompt drop folder (CSVs, PDFs, filings, receipts)
    ├── notes/                         # Personal notes, research, drafts & investment theses
    ├── worktrees/                     # Isolated parallel git worktrees (<repo>-<task>/)
    ├── private/                       # High-PII records (ledger/, taxes/, profile.md) & isolated credentials/taint/bridges
    └── memory/                        # topics/*.md, entities/*.md, goals.json, tasks.json, events/, timers/, outbox/, facts.jsonl, journal/, pending/, progress/, living-spaces/, obs/, rsi/
```

---

## Built-In Skills

`agy-hq` is **100% general-purpose out of the box**—you can ask it to debug any repository, write an essay, or plan a trip without pre-registering workflows. Following [RFC-0019](docs/design/0019-two-tier-rsi-vault-overlay.md), skills are separated into **3 Core OS Skills** (bundled cleanly with the plugin and upgraded atomically via `hq upgrade`) and **6 Opt-In Starter Templates** (`templates/skills/`, installed into your private `local/skills/` Vault via `./bin/hq skill add <name|--all>`):

### Core OS Harness Skills (`.agents/plugins/agy-hq/skills/`)

| Skill | Trigger / Use Case | Built-In Guardrail |
| :--- | :--- | :--- |
| [`morning-brief`](.agents/plugins/agy-hq/skills/morning-brief/SKILL.md) | 7:30 AM daily digest across calendar, triaged inbox, PRs/worktrees, and watchlist alerts | Read-only synthesis; outbound replies staged via `hq propose` |
| [`sleep-consolidate`](.agents/plugins/agy-hq/skills/sleep-consolidate/SKILL.md) | 11:30 PM silent nightly dreaming, transcript verification, Topic Wiki distillation & Vault git commit | Zero Front Desk chat output; verifies claims via `hq memory verify-transcript` |
| [`inbox-triage`](.agents/plugins/agy-hq/skills/inbox-triage/SKILL.md) | Quarantined triage of files dropped into `local/inbox/` or unread messages | Tainted-session lockdown blocks web egress, file writes & private Vault reads |

### Opt-In Domain & Channel Starter Templates (`templates/skills/` → `local/skills/`)

| Starter Skill | Trigger / Use Case | Built-In Guardrail |
| :--- | :--- | :--- |
| [`gchat`](templates/skills/gchat/SKILL.md) | Search, quarantined read, and 1-step outbound staging (`hq gchat propose`) for Google Chat Spaces/DMs *(auto-seeded on `hq gchat login`)* | Network-jailed CDP / OAuth transport + 1-tap SHA-256 send approval |
| [`swe-pulse`](templates/skills/swe-pulse/SKILL.md) | Multi-repo software engineering in isolated `local/worktrees/<repo>-<task>/` checkouts + PR/CI checks | Third-party PR merges & external pushes gated by `hq propose` |
| [`thesis-check`](templates/skills/thesis-check/SKILL.md) | Evaluates earnings releases and 10-K/10-Q/8-K filings against written theses & kill criteria | Mandatory filing/page citations; zero brokerage execution tools |
| [`filing-diff`](templates/skills/filing-diff/SKILL.md) | Section-by-section semantic diff of consecutive SEC 10-K / 10-Q filings (risk factors, footnotes, MD&A) | Deterministic section pairing & verbatim delta citations |
| [`ledger-ingest`](templates/skills/ledger-ingest/SKILL.md) | Parses bank CSV/OFX drops from `local/inbox/` into double-entry Beancount/hledger postings | Enforces zero-balance accounting integrity inside `local/private/ledger/` |
| [`tax-prep`](templates/skills/tax-prep/SKILL.md) | Extracts W-2, 1099, 1098, and K-1 forms into checklists and deterministic tax workpapers | Confined to `local/private/taxes/` with script-verified arithmetic |

---

## CLI Reference (`./bin/hq`)

### Everyday Commands

| Command | Purpose |
| :--- | :--- |
| `./bin/hq init [--demo]` | Initialize git-ignored `local/` Vault (optionally seed synthetic `[Sample]` dataset) |
| `./bin/hq doctor [--json]` | Verify Go toolchain, Antigravity sandbox, daemon, Vault health, and git hooks |
| `./bin/hq status` | Show pending outbound approvals, active git worktrees, and Front Desk status |
| `./bin/hq webui` | Launch interactive HQ Console dashboard on `http://127.0.0.1:4242` |
| `./bin/hq tasks [--active\|--all]` | Inspect active Layer-1 Task Conversations, subagents, and cold archive |
| `./bin/hq goals [list\|add\|wait-on\|due]` | Track multi-day Goals, milestones, and counterparty `WaitingOn` reply SLAs |
| `./bin/hq ideas [list\|promote\|snooze\|dismiss]` | Review, promote to Task, snooze, or dismiss proactive `ideas_feed` suggestions |
| `./bin/hq spaces [list\|active\|pin\|hide\|auto]` | Inspect or pin schedule-aware Console Spaces (`morning-ops`, `market-watch`, `engineering`) |
| `./bin/hq obs tokens` | View daily uncached token burn, prefix-cache hit ratio (`CHR %`), and latency |
| `./bin/hq upgrade` | Snapshot `local/.git`, pull latest `agy-hq` commits, recompile `bin/hq`, and migrate |
| `./bin/hq ci pre-push` | Run full local verification gate (`P1–P6` PII, `H1–H5F` contracts/anti-slop, unit & CUJ E2E tests) |

<details>
<summary><b>Expand Full CLI Subcommand Reference (Task Lifecycle, Approvals, Extensions, Channels, Delegates, Daemon, Observability, Gmail, GChat, Memory & RSI)</b></summary>

<br />

| Category | Command | Description |
| :--- | :--- | :--- |
| **Core & Diagnostics** | `./bin/hq version [--json]` | Show `hq` binary SemVer, git commit, Vault path, and schema version |
| | `./bin/hq vault-path [--json]` | Print resolved private Vault directory and resolution source |
| | `./bin/hq plugin <install\|uninstall\|status\|validate>` | Manage project-scoped (default) or `--global` plugin bundle & binary (`--strict`, `--console-sidecar`) |
| | `./bin/hq skill <list\|add\|remove>` | Manage opt-in Vault skills (`<vault>/skills/`) from `templates/skills/` |
| | `./bin/hq init [--demo] [--global-vault] [--attach-plugin] [--starters=...\|--all-starters]` | Initialize git-ignored private `local/` (or global) Vault and optionally seed `[Sample]` data |
| | `./bin/hq console` | Sync `local/CONSOLE.md` and active Front Desk `hq_console.md` artifact |
| | `./bin/hq doctor [--json]` | Verify Go toolchain, Antigravity sandbox, Remote Control, CLI tools, and Vault health |
| | `./bin/hq status [--quiet]` | Display pending approvals queue, active worktrees, workspaces, and channel bridges |
| | `./bin/hq upgrade [--no-pull]` | Snapshot Vault, pull latest commits, recompile CLI, sync plugin, and migrate |
| | `./bin/hq migrate [--quiet]` | Run non-destructive Vault schema and template migrations |
| | `./bin/hq hook <session-start\|pre-tool\|stop>` | Antigravity `hooks.json` lifecycle handlers |
| **Task Board & Lifecycle** | `./bin/hq tasks [--active\|--all\|--archive\|--kind=\|--scope=\|--search=]` | Query tiered Task Board, active tasks, and cold archive |
| | `./bin/hq tasks <stats\|compact\|sync-sidebar\|pin\|unpin\|clean-nested>` | Task Board telemetry, cold compaction, sidebar archive sync, pinning, and nested cleanup |
| | `./bin/hq spawn --task=<slug> --prompt="..." [--repo=...\|--workspace=...] [--tier=\|--model=\|--depth=]` | Spawn dedicated Task Conversation in isolated git worktree or provider workspace |
| | `./bin/hq subagent --role="..." --prompt="..." [--model=\|--depth=]` | Launch nested child subagent under current conversation |
| | `./bin/hq complete <slug-or-id> [--summary="..."]` | Mark an active task conversation as completed |
| | `./bin/hq archive <slug-or-id> [--force] [--no-sidebar]` | Archive a completed task conversation |
| | `./bin/hq reopen <slug-or-id> [--no-worktree] [--no-sidebar]` | Reopen an archived task conversation (recalls from cold archive if needed) |
| | `./bin/hq adopt --conv=<id> --task=<slug> [--title="..."]` | Adopt existing conversation (or demote Front Desk to a Task) |
| | `./bin/hq claim-desk / release-desk` | Manage the single active `[HQ] Front Desk` intake lease |
| | `./bin/hq progress <save\|show\|list\|archive>` | Structured cross-run & pre-compaction task checkpoints |
| **Worktrees & Workspaces** | `./bin/hq worktree <add\|list\|attach\|remove\|prune>` | Create, list, attach, remove, or prune isolated git worktrees in `<vault>/worktrees/` |
| | `./bin/hq workspace <providers\|list\|create\|adopt\|status\|handoff\|unbind\|remove\|own\|prune>` | Manage pluggable provider workspaces (`RFC-0025`) |
| **Safety & Approvals** | `./bin/hq propose --type=... --summary="..." (--cmd="..." \| --draft=... \| --rsi-id=...) [--ttl=24h]` | Stage outbound action (`24h` TTL + contextual-PII scan) for 1-tap approval |
| | `./bin/hq approve <id> [--sha256=<prefix>] [--cap=<token>]` | Verify `>= 8`-char SHA-256 prefix, check TTL/capability, and execute staged action |
| | `./bin/hq reject <id> [--reason="..."] [--dismiss]` | Reject (or quietly dismiss) a pending outbound proposal |
| | `./bin/hq approvals <list\|expired\|restage\|dismiss>` | Inspect pending/expired approvals, restage timed-out cards, or bulk-dismiss |
| | `./bin/hq cancel <id-or-scope> [--reason="..."]` | Cancel pending outbound proposals or background runs |
| | `./bin/hq exec-sandboxed [--workdir=...] [--net-isolated] -- <cmd>` | Execute subprocess inside OS credential sandbox |
| **Proactive Engine, Goals & Spaces** | `./bin/hq outbox <add\|list\|act\|eval>` | Route background insights (`interrupt`, `brief_rollup`, `ideas_feed`, `silent_ledger`), act on cards, and eval SNR |
| | `./bin/hq pulse <precheck\|status>` | Run or inspect 0-token proactive look-ahead checks (`RFC-0027`) |
| | `./bin/hq ideas <list\|promote\|snooze\|dismiss>` | Manage proactive `ideas_feed` queue (dismissals feed `hq rsi mine`) |
| | `./bin/hq goals <list\|add\|milestone\|check-off\|checklist\|park\|unpark\|wait-on\|mark-replied\|due\|complete>` | Manage multi-day Goals, 3-state checklists, and counterparty `WaitingOn` SLAs |
| | `./bin/hq spaces <list\|active\|pin\|hide\|auto>` | Inspect or override schedule-aware Console Spaces |
| | `./bin/hq space <list\|create\|show\|build\|refresh\|sync-goal\|export-artifact\|export>` | Manage composable Living Spaces (`RFC-0027`) |
| **Events, Timers & Triggers** | `./bin/hq events <enqueue\|list\|inspect\|retry\|drop\|dispatch\|token>` | Manage durable crash-safe Event Inbox and webhook tokens |
| | `./bin/hq timers <set\|list\|cancel>` | Manage durable one-shot Follow-Up and recurring Schedule timers |
| | `./bin/hq triggers <list\|pause\|resume\|fire>` | Inspect, pause, resume, or fire declarative gatekeeper schedules |
| **Connectors, Extensions, Channels & Delegates** | `./bin/hq extension <list\|lint\|enable\|refresh\|disable\|trust\|apply-trust\|doctor>` | Manage companion `hq-extension.json` packs and SHA-256 trust hashes |
| | `./bin/hq dispatch resolve [<skill>] [--trigger=<fqid>] [--json]` | Resolve active Core OS skill, Vault skill, or RSI overlay rules |
| | `./bin/hq connector <list\|lint\|migrate\|doctor\|test\|apply-offer>` | Manage modular JSON-RPC/CLI connectors (`RFC-0017a`) |
| | `./bin/hq watch <status\|precheck\|read\|query\|ack\|prune-backlog\|reset-poisoned>` | Run 0-token watcher prechecks, quarantined read/query, and item acks |
| | `./bin/hq offer stage --connector=... [--thread=...] [--to=...] [--body=...]` | Stage taint-safe outbound offers with `participant_lock` and `/dev/fd/3` |
| | `./bin/hq brief <collect\|commit\|format-quarantined>` | Lease-based Morning Brief packet assembly and commit (`RFC-0017c`) |
| | `./bin/hq channel <list\|status\|test\|pause\|resume\|audit\|configure>` | Manage Front Desk `hq-channel-bridge/v1` bridges & SHA-256 audit log (`RFC-0023`) |
| | `./bin/hq delegate <list\|show\|validate\|configure\|trust-command\|test\|bind-target\|ack-nudge\|audit>` | Manage declarative scoped peer delegates (`RFC-0026`) |
| **Daemon, Web UI, Sidecar & Heartbeat** | `./bin/hq daemon [install\|uninstall\|status\|run] [--addr=127.0.0.1:4242]` | Run or manage `launchd` (macOS) / `systemd` (Linux) background daemon service |
| | `./bin/hq webui [--addr=127.0.0.1:4242]` | Start interactive Web UI Console & Antigravity Sidecar server |
| | `./bin/hq sidecar console [--listen=...] [--supervise]` | Run native Go stdio JSON-RPC console sidecar |
| | `./bin/hq heartbeat [--dry-run] [--force-gatekeepers]` | Run 0-token cursor pre-checks, `WaitingOn` SLA sweep, Dead-Man's Switch, and inbox router |
| **Token & Cost Observability** | `./bin/hq obs tokens [--days=7]` | Daily uncached input, cache read, output tokens, `CHR %`, and latency breakdown |
| | `./bin/hq obs top [--tree]` | Rank conversations by self or full parent-child tree uncached token burn |
| | `./bin/hq obs inspect <conv-or-slug>` | Per-turn context growth, prefix-cache hits, and compaction telemetry |
| | `./bin/hq obs anomalies / budget / reconcile` | Detect cold-cache thrash or runaway turns, check daily budgets, and reconcile DAGs |
| **Google Chat Primitives** | `./bin/hq gchat init / add-account / accounts / status` | Configure Google Chat accounts and inspect CDP / OAuth / bridge status |
| | `./bin/hq gchat propose --to=... --body=...` | 1-step stage Google Chat draft & proposal (spaces, DMs, emails, handles) |
| | `./bin/hq gchat spaces / search [--query="..."]` | List spaces/DMs or search Chat messages without LLM exposure |
| | `./bin/hq gchat read-quarantined --space=...` | Read Chat messages in sandboxed quarantine (taints session against egress) |
| | `./bin/hq gchat draft / send-draft / precheck / triage / triage-result` | Draft management, pure-Go unread precheck, and quarantined triage |
| | `./bin/hq gchat login / logout / restore-session / launch-chrome` | Guided Chrome login (`--remote` / `--reset`) and CDP session management |
| **Memory & RSI Engine** | `./bin/hq memory status / topics / entities / search` | Inspect 4-layer memory hierarchy, Wiki pages, Entity Dossiers, and BM25 search |
| | `./bin/hq memory lookup / add-entity / profile <get\|set\|unset>` | On-demand dossier lookup (`< 5ms`) and private household profile autofill |
| | `./bin/hq memory forget "<query>" [--block-pii]` | Cascading 4-layer surgical scrub across facts, `MEMORY.md`, topics, entities, and notes |
| | `./bin/hq memory dream-plan / eval / verify-transcript / commit` | Dreaming precheck, `F1`–`F4` memory eval, transcript verification, and Vault commit |
| | `./bin/hq memory add-fact / prune-fact / compact` | Record, soft-prune, or compact scoped bi-temporal facts with provenance |
| | `./bin/hq rsi mine [--since=24h]` | Mine 0-token ground-truth friction events from journals |
| | `./bin/hq rsi hypothesize / verify / propose / apply / ablate / export / migrate-overlays / status` | Synthesize, 4-gate verify, stage, apply, ablate, and export two-tier RSI patches |
| **CI/CD & Harness Contracts** | `./bin/hq ci pre-push / pre-commit` | Run full local CI gate or fast pre-commit PII/format/link check |
| | `./bin/hq ci check-pii [--staged\|--history]` | Mathematical PII & Vault boundary scanner (`Invariants P1–P6`) |
| | `./bin/hq ci denylist-add / scrub-history --replacements=...` | Manage local PII denylist or preview/apply (`--apply`) git history sanitization |
| | `./bin/hq ci check-harness` | Verify skills (`H1`), hook parity & security (`H2`), routing (`H3`), Go/docs (`H4`), and anti-slop & CUJ/LOC contracts (`H5-A`–`H5-F`) |
| | `./bin/hq ci install-hooks` | Install `.git/hooks/pre-commit` and `.git/hooks/pre-push` |

</details>

---

## Documentation & Design Specifications

Explore the **[Documentation Hub (`docs/README.md`)](docs/README.md)** for guided reading paths across:
- **[System Architecture Overview](docs/architecture.md)** & **[Implementation Roadmap](docs/roadmap.md)**
- **Product Specs (`PRD-0001` – `PRD-0003`):** Day-in-the-life UX, open-ended skills, and all 49 E2E Critical User Journey tests (`CUJs 1–47`)
- **Technical RFCs (`RFC-0001` – `RFC-0027`):** Two-Layer Vault, 4-layer memory v2 & dreaming, SHA-256 approval policy, Gmail & Google Chat quarantine transports, bounded two-tier RSI, token observability, CI/anti-slop gates, global plugin packaging, sidebar archive sync, modular connectors/taint/brief/extensions, durable runtime hardening, channel bridges, pluggable workspaces, scoped delegates, and Muse proactive actions/Living Spaces
- **Architecture Decision Records (`ADR-0001` – `ADR-0006`)** & **Research Archive (`01` – `22`)**

---

## Contributing, Security & License

- **Contributions Welcome:** Read [CONTRIBUTING.md](CONTRIBUTING.md) and our [Code of Conduct](CODE_OF_CONDUCT.md) before submitting pull requests or new `SKILL.md` workflows.
- **Security Policy:** Please report security issues privately via **[GitHub Private Vulnerability Reporting](https://github.com/zjshen14/agy-hq/security/advisories/new)**—see [SECURITY.md](SECURITY.md) for our mechanical threat model and response SLA.
- **License:** Licensed under the **[Apache License 2.0](LICENSE)** (`Apache-2.0`).
