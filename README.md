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
  <a href="go.mod"><img src="https://img.shields.io/badge/Go-1.22%2B_(0_External_Deps)-00ADD8?logo=go&logoColor=white" alt="Go 1.22+ Zero External Deps" /></a>
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
- **Runtime:** **Google Antigravity** desktop app + **Git** + **Go 1.22+** *(auto-installed via Homebrew on macOS if missing)*

### Option A: Try Instantly with Synthetic `--demo` Data *(Recommended First Run)*

Explore the live Task Board, Inbox Triage, Thesis Check (`ACME` Form 10-Q), and SHA-256 Approval Console in under 60 seconds using synthetic `[Sample]` data:

```sh
git clone https://github.com/zjshen14/agy-hq.git headquarter && cd headquarter && ./scripts/bootstrap.sh --demo
```

### Option B: Clean Personal Vault Bootstrap

```sh
git clone https://github.com/zjshen14/agy-hq.git headquarter && cd headquarter && ./scripts/bootstrap.sh
```

**What `bootstrap.sh` sets up automatically:**
1. Compiles `./bin/hq` locally in `< 2s` from pure Go standard library source (`0` external `go.mod` dependencies).
2. Runs `./bin/hq init` to scaffold your git-ignored private Vault (`./local/USER.md`, `./local/MEMORY.md`, `./local/CONSOLE.md`, `./local/.git`, and `0700`-permissioned private directories).
3. Installs `.git/hooks/pre-commit` and `.git/hooks/pre-push` mathematical PII & harness guards (`Invariants P1–P6`, `Gates H1–H4B`).
4. Installs the background `hq daemon` user service and runs `./bin/hq doctor` to verify environment health.

> **Zero-Touch GUI Option:** Even if you only `git clone` and open `headquarter` directly in Antigravity without running `./scripts/bootstrap.sh`, the `SessionStart` hook in [`.agents/hooks.json`](.agents/hooks.json) automatically compiles `./bin/hq`, initializes `./local/`, claims your `[HQ] Front Desk` conversation, and pins the live `hq_console.md` artifact on Turn 1.
>
> **Tip — Pin Your Front Desk:** Pin your `[HQ] Front Desk` conversation to the top of the Antigravity sidebar (via the conversation header menu) so your coordinator stays anchored above ephemeral task threads.
>
> **Live Aux Console:** Open the embedded Antigravity Sidecar pane via [`sidecar://agy-hq-console/`](.agents/plugins/agy-hq/sidecars/agy-hq-console/sidecar.json) or run `./bin/hq webui` for the standalone dashboard at `http://127.0.0.1:4242`.

---

## Two-Layer Architecture: Open-Source Engine vs. Private Vault

```text
agy-hq/                                # LAYER 1: Open-Source Agent OS (Tracked Root Git Repo)
├── AGENTS.md                          # Front Desk routing, Vault boundaries & approval invariants
├── cmd/hq/                            # Single static Go CLI entrypoint (zero external dependencies)
├── internal/                          # Go microkernel (hooks, kernel, outbox, approvals, obs, rsi, ci, webui, gmail, gchat)
├── templates/                         # Clean starter blueprints copied into ./local/ by `hq init`
├── docs/                              # PRDs, technical RFCs (0001–0014), ADRs & research archive
├── assets/branding/                   # Official Asymmetric Wing Cradle icon pack & social preview
├── .agents/
│   ├── hooks.json                     # SessionStart (memory), PreToolUse (guardrails), Stop (0-token log)
│   └── plugins/agy-hq/                # Official plugin bundle (Aux Sidecar + 9 progressive-disclosure skills)
│
└── local/                             # LAYER 2: Your Private Vault (Git-Ignored; Own Private local/.git)
    ├── USER.md                        # Your profile, communication preferences & goals
    ├── MEMORY.md                      # Bounded Roadmap Index (< 2,500 tokens) auto-injected at SessionStart
    ├── CONSOLE.md                     # Zero-token live Task Board, Console Spaces & Ideas snapshot
    ├── config.yaml & triggers.yaml    # Attention/Quiet Hours, Spaces, quota budgets & 0-token cursor gatekeepers
    ├── inbox/                         # Zero-prompt drop folder (CSVs, PDFs, filings, receipts)
    ├── notes/                         # Personal notes, research, drafts & investment theses
    ├── worktrees/                     # Isolated parallel git worktrees (<repo>-<task>/)
    ├── private/                       # High-PII records (ledger/, taxes/) & isolated credentials (gmail/, gchat/)
    └── memory/                        # topics/*.md, entities/*.md, goals.json, outbox/, facts.jsonl, journal/, pending/
```

---

## Built-In Skills

`agy-hq` is **100% general-purpose out of the box**—you can ask it to debug any repository, write an essay, or plan a trip without pre-registering workflows. For recurring high-stakes domains, it includes **9 progressive-disclosure skills** (`<= 150` lines each, loaded only when relevant):

| Skill | Trigger / Use Case | Built-In Guardrail |
| :--- | :--- | :--- |
| [`morning-brief`](.agents/plugins/agy-hq/skills/morning-brief/SKILL.md) | 7:30 AM daily digest across calendar, triaged inbox, PRs/worktrees, and watchlist alerts | Read-only synthesis; outbound replies staged via `hq propose` |
| [`sleep-consolidate`](.agents/plugins/agy-hq/skills/sleep-consolidate/SKILL.md) | 11:30 PM silent nightly dreaming, transcript verification, Topic Wiki distillation & Vault git commit | Zero Front Desk chat output; verifies claims via `hq memory verify-transcript` |
| [`swe-pulse`](.agents/plugins/agy-hq/skills/swe-pulse/SKILL.md) | Multi-repo software engineering in isolated `local/worktrees/<repo>-<task>/` checkouts + PR/CI checks | Third-party PR merges & external pushes gated by `hq propose` |
| [`inbox-triage`](.agents/plugins/agy-hq/skills/inbox-triage/SKILL.md) | Quarantined triage of files dropped into `local/inbox/` or unread messages | Tainted-session lockdown blocks web egress, file writes & private Vault reads |
| [`gchat`](.agents/plugins/agy-hq/skills/gchat/SKILL.md) | Search, quarantined read, and 1-step outbound staging (`hq gchat propose`) for Google Chat Spaces/DMs | Network-jailed CDP / OAuth transport + 1-tap SHA-256 send approval |
| [`thesis-check`](.agents/plugins/agy-hq/skills/thesis-check/SKILL.md) | Evaluates earnings releases and 10-K/10-Q/8-K filings against written theses & kill criteria | Mandatory filing/page citations; zero brokerage execution tools |
| [`filing-diff`](.agents/plugins/agy-hq/skills/filing-diff/SKILL.md) | Section-by-section semantic diff of consecutive SEC 10-K / 10-Q filings (risk factors, footnotes, MD&A) | Deterministic section pairing & verbatim delta citations |
| [`ledger-ingest`](.agents/plugins/agy-hq/skills/ledger-ingest/SKILL.md) | Parses bank CSV/OFX drops from `local/inbox/` into double-entry Beancount/hledger postings | Enforces zero-balance accounting integrity inside `local/private/ledger/` |
| [`tax-prep`](.agents/plugins/agy-hq/skills/tax-prep/SKILL.md) | Extracts W-2, 1099, 1098, and K-1 forms into checklists and deterministic tax workpapers | Confined to `local/private/taxes/` with script-verified arithmetic |

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
| `./bin/hq ci pre-push` | Run full local verification gate (`P1–P6` PII, `H1–H4B` contracts, unit & CUJ E2E tests) |

<details>
<summary><b>Expand Full CLI Subcommand Reference (Task Lifecycle, Approvals, Daemon, Observability, Gmail, GChat, Memory & RSI)</b></summary>

<br />

| Category | Command | Description |
| :--- | :--- | :--- |
| **Core & Diagnostics** | `./bin/hq init [--demo]` | Initialize git-ignored private `local/` Vault (optionally seed `[Sample]` dataset) |
| | `./bin/hq doctor [--json]` | Verify Go toolchain, Antigravity sandbox, Remote Control, CLI tools, and Vault health |
| | `./bin/hq status` | Display pending approvals queue, active worktrees, and health |
| | `./bin/hq upgrade [--no-pull]` | Snapshot Vault, pull latest commits, recompile CLI, and migrate |
| | `./bin/hq migrate` | Run non-destructive Vault schema and template migrations |
| **Task Board & Lifecycle** | `./bin/hq tasks [--active\|--all\|--archive]` | Query tiered Task Board, active tasks, and cold archive |
| | `./bin/hq tasks stats` | Task Board telemetry, hot/cold distribution, and slot usage |
| | `./bin/hq tasks compact` | Rotate dormant/completed tasks into cold archive (`tasks_archive.jsonl`) |
| | `./bin/hq tasks pin / unpin <slug>` | Pin or unpin high-priority tasks to the top of the Task Board |
| | `./bin/hq spawn --task=<slug> --prompt="..."` | Spawn dedicated Task Conversation in isolated worktree |
| | `./bin/hq subagent --role="..." --prompt="..."` | Launch nested child subagent under current conversation |
| | `./bin/hq complete <slug-or-id>` | Mark an active task conversation as completed |
| | `./bin/hq archive <slug-or-id>` | Archive a completed task conversation |
| | `./bin/hq reopen <slug-or-id>` | Reopen an archived task conversation (recalls from cold archive if needed) |
| | `./bin/hq adopt --conv=<id> --task=<slug>` | Adopt existing conversation (or demote Front Desk to a Task) |
| | `./bin/hq claim-desk / release-desk` | Manage the single active `[HQ] Front Desk` intake lease |
| | `./bin/hq worktree add <repo> <task>` | Create isolated git worktree in `local/worktrees/<repo>-<task>/` |
| **Safety & Approvals** | `./bin/hq propose --type=... --summary="..." --cmd="..." [--ttl=24h]` | Stage irreversible outbound action (`24h` TTL + contextual-PII scan) for 1-tap approval |
| | `./bin/hq approve <id> --sha256=<prefix>` | Verify `>= 8`-char SHA-256 prefix, check TTL expiry, and execute staged action |
| | `./bin/hq reject <id>` | Reject and remove pending outbound proposal |
| **Proactive Outbox, Goals & Spaces** | `./bin/hq outbox <add\|list\|drain>` | Route background insights across `interrupt`, `brief_rollup`, `ideas_feed`, and `silent_ledger` |
| | `./bin/hq ideas <list\|promote\|snooze\|dismiss>` | Manage proactive `ideas_feed` queue (dismissals feed `hq rsi mine`) |
| | `./bin/hq goals <list\|add\|milestone\|check-off\|wait-on\|mark-replied\|due\|complete>` | Manage multi-day Goals and 0-token counterparty `WaitingOn` reply SLAs |
| | `./bin/hq spaces <list\|active\|pin\|hide\|auto>` | Inspect or override schedule-aware Console Spaces |
| **Daemon, Web UI & Heartbeat** | `./bin/hq daemon install / status / uninstall` | Manage `launchd` (macOS) or `systemd` (Linux) background HQ daemon service |
| | `./bin/hq daemon [--addr=127.0.0.1:4242]` | Run background daemon watcher and loopback HTTP server |
| | `./bin/hq webui [--addr=127.0.0.1:4242]` | Start interactive Web UI Console & Antigravity Sidecar |
| | `./bin/hq heartbeat [--dry-run] [--force-gatekeepers]` | Run 0-token cursor pre-checks, `WaitingOn` SLA sweep, Dead-Man's Switch, and inbox router |
| **Token & Cost Observability** | `./bin/hq obs tokens [--days=7]` | Daily uncached input, cache read, output tokens, `CHR %`, and latency breakdown |
| | `./bin/hq obs top [--tree]` | Rank conversations by self or full parent-child tree uncached token burn |
| | `./bin/hq obs inspect <conv-or-slug>` | Per-turn context growth, prefix-cache hits, and compaction telemetry |
| | `./bin/hq obs anomalies / budget / reconcile` | Detect cold-cache thrash or runaway turns, check daily budgets, and reconcile DAGs |
| **Google Chat Primitives** | `./bin/hq gchat propose --to=... --body=...` | 1-step stage Google Chat draft & proposal (spaces, DMs, emails, handles) |
| | `./bin/hq gchat spaces` | List Google Chat spaces and DMs (fuzzy name cache) |
| | `./bin/hq gchat search [--query="..."]` | Search Chat messages without LLM exposure |
| | `./bin/hq gchat read-quarantined --space=...` | Read Chat messages in sandboxed quarantine (taints session against egress) |
| | `./bin/hq gchat login [--remote] [--reset]` | Guided 1-time Chrome login (desktop or phone prompt via `--remote`) |
| **Gmail Primitives** | `./bin/hq gmail init --email=<email>` | Configure primary or secondary Gmail account |
| | `./bin/hq gmail status / list` | Inspect configured Gmail accounts or list messages without LLM exposure |
| | `./bin/hq gmail read --thread=<id>` | Read email thread in quarantined plain-text format |
| | `./bin/hq gmail draft --to=... --body=...` | Create or update draft email for staged review |
| | `./bin/hq gmail triage` | Run pure-Go unread sidecar precheck and batch triage items |
| **Memory & RSI Engine** | `./bin/hq memory status / topics / entities` | Inspect 4-layer memory hierarchy, Knowledge Wiki pages, and Entity Dossiers |
| | `./bin/hq memory lookup <query>` | On-demand (`< 5ms`, `0` `SessionStart` tokens) contact & counterparty dossier lookup |
| | `./bin/hq memory add-entity --name=...` | Create or merge an Entity Dossier in `local/memory/entities/<slug>.md` |
| | `./bin/hq memory forget "<query>" [--block-pii]` | Cascading 4-layer surgical scrub across facts, `MEMORY.md`, topics, entities, and notes |
| | `./bin/hq memory verify-transcript <convId>` | Verify claims against raw transcripts before promoting memory |
| | `./bin/hq memory add-fact / prune-fact / compact` | Record, soft-prune, or compact bi-temporal atomic facts with provenance |
| | `./bin/hq rsi mine [--since=24h]` | Mine 0-token ground-truth friction events from journals |
| | `./bin/hq rsi hypothesize / verify / propose / status` | Synthesize, 4-gate verify, and stage bounded self-improvement patches |
| **CI/CD & Harness Contracts** | `./bin/hq ci pre-push / pre-commit` | Run full local CI gate or fast pre-commit PII/format/link check |
| | `./bin/hq ci check-pii [--staged\|--history]` | Mathematical PII & Vault boundary scanner (`Invariants P1–P6`) |
| | `./bin/hq ci scrub-history --replacements=...` | Preview or apply (`--apply`) deterministic git commit history sanitization |
| | `./bin/hq ci check-harness` | Verify skills (`H1`), hook parity & security (`H2`), routing (`H3`), and Go/docs (`H4`) |
| | `./bin/hq ci install-hooks` | Install `.git/hooks/pre-commit` and `.git/hooks/pre-push` |

</details>

---

## Documentation & Design Specifications

Explore the **[Documentation Hub (`docs/README.md`)](docs/README.md)** for guided reading paths across:
- **[System Architecture Overview](docs/architecture.md)** & **[Implementation Roadmap](docs/roadmap.md)**
- **Product Specs (`PRD-0001` – `PRD-0003`):** Day-in-the-life UX, open-ended skills, and all 24 E2E Critical User Journeys
- **Technical RFCs (`RFC-0001` – `RFC-0014`):** Two-Layer Vault, 4-layer memory dreaming, SHA-256 approval gates, Gmail & Google Chat quarantine transports, bounded RSI, token observability, CI/privacy verification, and Proactive Engine v2
- **Architecture Decision Records (`ADR-0001` – `ADR-0006`)** & **Research Archive (`01` – `10`)**

---

## Contributing, Security & License

- **Contributions Welcome:** Read [CONTRIBUTING.md](CONTRIBUTING.md) and our [Code of Conduct](CODE_OF_CONDUCT.md) before submitting pull requests or new `SKILL.md` workflows.
- **Security Policy:** Please report security issues privately via **[GitHub Private Vulnerability Reporting](https://github.com/zjshen14/agy-hq/security/advisories/new)**—see [SECURITY.md](SECURITY.md) for our mechanical threat model and response SLA.
- **License:** Licensed under the **[Apache License 2.0](LICENSE)** (`Apache-2.0`).
