<p align="center">
  <img src="assets/branding/icons/icon-obsidian-128x128.png" width="128" height="128" alt="agy-hq logo" />
</p>

# `agy-hq` — Personal Agent OS for Google Antigravity

**`agy-hq`** (Headquarter) is a lightweight, open-source **Personal Agent OS** built on top of **Google Antigravity**. It turns an always-on Mac running Antigravity into a 24/7 general-purpose, proactive personal assistant—while keeping **100% of your personal memory, files, and credentials isolated in a private `Vault` (`local/`)**.

---

## Why `agy-hq`?

Frontier personal agents (Meta Muse, OpenAI `dots`, Claude Duet/Cowork, OpenClaw, Nous Hermes) share six core capabilities:
1. **100% General-Purpose & Open-Ended** (handles any ad-hoc request—coding, writing a post, deep research, planning, or life admin—without boxing you into rigid categories)
2. **Always-On Execution** (runs 24/7 on your always-on Mac desktop)
3. **Silent-by-Default Heartbeat** (0-token background checks in Go that only wake the LLM when something is actionable)
4. **Event Triggers & Automated Watchdogs** (`fsnotify` drop-folder watching in `local/inbox/`, Pre-IPO secondary watchdog in Secondary Market at 11:00 AM & 4:00 PM PT, SEC/EDGAR close-watch, RSS/filing monitors, CI checks)
5. **Pocket Access, 1-Tap Approvals & Interactive Console** (piggybacking on **Antigravity Remote Control** Web/PWA, embedded Antigravity Sidecar aux pane [`sidecar://agy-hq-console/`](.agents/plugins/agy-hq/sidecars/agy-hq-console/sidecar.json) on port 4242, and Google Chat CDP adapter)
6. **Consolidated Memory, Compounding Skills & RSI** (0-token `Stop` turn capture, nightly silent dreaming memory consolidation, 4-stage recursive self-improvement `hq rsi`, and progressive-disclosure `.agents/plugins/agy-hq/skills/*/SKILL.md` workflows)

---

## Quickstart (1-Line Setup)

On a clean machine with **Google Antigravity** installed, run:

```sh
git clone https://github.com/zjshen14/agy-hq.git headquarter && cd headquarter && ./scripts/bootstrap.sh
```

What this single line does:
1. Checks for Go (installing via Homebrew on macOS if missing) and compiles `./bin/hq`.
2. Runs `./bin/hq init` to scaffold your git-ignored private Vault (`./local/USER.md`, `./local/MEMORY.md`, `./local/CONSOLE.md`, `./local/.git`, and isolated directories).
3. Installs a `.git/hooks/pre-commit` PII guard so personal paths or emails can never be committed to the root repo.
4. Runs `./bin/hq doctor` to verify all health checks pass.

> **Zero-Touch GUI Option:** Even if you only `git clone` and open `headquarter` directly in Antigravity without running `./scripts/bootstrap.sh`, the `SessionStart` hook in [`.agents/hooks.json`](.agents/hooks.json) automatically compiles `./bin/hq`, initializes `./local/`, claims your `[HQ] Front Desk` conversation, and pins the live `hq_console.md` artifact on Turn 1.
>
> 💡 **Sidebar Pinning:** Always pin your `[HQ] Front Desk` conversation to the Antigravity sidebar (via the conversation header menu) so it stays permanently anchored at the very top above ephemeral task threads.
>
> 🎛️ **Interactive Console & Sidecar:** Access the live Task Board, pending approvals, and system telemetry directly inside Antigravity's Aux Pane via [`sidecar://agy-hq-console/`](.agents/plugins/agy-hq/sidecars/agy-hq-console/sidecar.json) or run `./bin/hq webui` for the browser dashboard at `http://127.0.0.1:4242`.

---

## Two-Layer Architecture: Open-Source Engine vs. Private Vault

```text
agy-hq/                                # LAYER 1: Open-Source Agent OS (This Git Repo)
├── .gitignore                         # Strictly ignores /local/, /bin/, and secrets
├── AGENTS.md                          # General-purpose personal assistant rules
├── assets/branding/                   # Official Asymmetric Wing Cradle icon pack (SVG, PNG & social preview)
├── cmd/hq/                            # Go CLI: init | hook | daemon | propose | notify | status
├── internal/                          # Go microkernel (hooks, 0-token trigger runner, approvals, Aux UI)
├── templates/                         # Starter blueprints copied by `hq init` into ./local/
├── docs/                              # Product specs, technical RFCs, ADRs & research
├── .agents/
│   ├── hooks.json                     # SessionStart (memory), PreToolUse (action guard), Stop (0-token log)
│   └── plugins/agy-hq/                # Official agy-hq plugin bundle (icon + progressive-disclosure skills)
│
└── local/                             # LAYER 2: Your Private Vault (Separate Private Git Repo)
    ├── USER.md                        # Your profile, preferences, writing voice & goals
    ├── MEMORY.md                      # Consolidated long-term memory + topic notes
    ├── triggers.yaml                  # Your cron schedules, 0-token pre-checks & inbox/ drop rules
    ├── inbox/                         # Zero-prompt file drop folder (CSVs, PDFs, notes, receipts)
    ├── workspace/                     # Open workspace for any task (posts, research, projects, notes)
    ├── private/                       # High-PII records (beancount ledger, taxes) with extra guardrails
    └── memory/                        # facts.jsonl, journal/YYYY-MM-DD/<convId>.json, pending/, outbox/
```

---

## Documentation & Specifications

Full product specs ([PRD 0001–0003](docs/README.md)), technical designs ([RFC 0001–0011+](docs/README.md)), architecture decision records ([ADR 0001–0006](docs/README.md)), and frontier research live in **[`docs/`](docs/README.md)**.

---

## CLI Quick Reference (`./bin/hq`)

| Category | Command | Description |
|---|---|---|
| **Core & Diagnostics** | `./bin/hq init` | Initialize git-ignored private `local/` Vault and local git repo |
| | `./bin/hq doctor` | Verify Antigravity sandbox, Remote Control, CLI tools, and Vault health |
| | `./bin/hq status` | Display pending approvals queue, active worktrees, and health |
| | `./bin/hq upgrade` | Snapshot Vault, pull latest commits, recompile CLI, and migrate |
| | `./bin/hq migrate` | Run non-destructive Vault schema and template migrations |
| **Task Board & Lifecycle** | `./bin/hq tasks [--active\|--all\|--archive]` | Query tiered Task Board, active tasks, and cold archive |
| | `./bin/hq tasks stats` | Task Board telemetry, hot/cold distribution, and slot usage |
| | `./bin/hq tasks compact` | Rotate dormant/completed tasks into cold archive (`tasks_archive.jsonl`) |
| | `./bin/hq tasks pin / unpin` | Pin or unpin high-priority tasks to the top of the Task Board |
| | `./bin/hq spawn --task=<slug> --prompt="..."` | Spawn dedicated Task Conversation in isolated worktree |
| | `./bin/hq subagent --role="..." --prompt="..."` | Launch nested child subagent under current conversation |
| | `./bin/hq complete <slug-or-id>` | Mark an active task conversation as completed |
| | `./bin/hq archive <slug-or-id>` | Archive a completed task conversation |
| | `./bin/hq reopen <slug-or-id>` | Reopen an archived task conversation (recalls from cold archive if needed) |
| | `./bin/hq adopt --conv=<id> --task=<slug>` | Adopt existing conversation (or demote Front Desk to a Task) |
| | `./bin/hq claim-desk --conv=<id>` | Claim the single active `[HQ] Front Desk` intake lease |
| | `./bin/hq release-desk` | Release Front Desk lease so next new conversation claims it |
| | `./bin/hq worktree add <repo> <task>` | Create isolated git worktree in `local/worktrees/<repo>-<task>/` |
| **Safety & Approvals** | `./bin/hq propose --type=... --summary="..." --cmd="..."` | Stage irreversible outbound action for 1-tap approval |
| | `./bin/hq approve <id> [--sha256=<prefix>]` | Verify SHA-256 and deterministically execute staged action |
| | `./bin/hq reject <id>` | Reject and remove pending outbound proposal |
| **Daemon, Web UI & Heartbeat** | `./bin/hq daemon install` | Install `systemd` or `launchd` background HQ daemon service |
| | `./bin/hq daemon status` | Check background daemon status, PID, and uptime |
| | `./bin/hq daemon uninstall` | Cleanly stop and uninstall background daemon service |
| | `./bin/hq daemon run` | Run background daemon watcher and HTTP server on `127.0.0.1:4242` |
| | `./bin/hq webui [--addr=127.0.0.1:4242]` | Start interactive Web UI Console & Antigravity Sidecar |
| | `./bin/hq heartbeat` | Run 0-token idle pre-checks, Dead-Man's Switch, and drop router |
| **Google Chat Primitives** | `./bin/hq gchat propose --to=... --body=...` | 1-step stage Google Chat draft & proposal (spaces, DMs, handles) |
| | `./bin/hq gchat spaces` | List Google Chat spaces and DMs (fuzzy name cache) |
| | `./bin/hq gchat search [--query="..."]` | Search Chat messages without LLM exposure |
| | `./bin/hq gchat read-quarantined --space=...` | Read Chat messages in sandboxed quarantine (subagent only) |
| | `./bin/hq gchat login [--remote] [--reset]` | Guided 1-time Chrome login (desktop or phone prompt via `--remote`) |
| **Gmail Primitives** | `./bin/hq gmail init --email=<email>` | Configure primary or secondary Gmail account |
| | `./bin/hq gmail status` | Inspect configured Gmail accounts and sidecar auth status |
| | `./bin/hq gmail list [--query="..."]` | Search and list Gmail messages without LLM exposure |
| | `./bin/hq gmail read --thread=<id>` | Read email thread quarantined in sandboxed text format |
| | `./bin/hq gmail draft --to=... --body=...` | Create or update draft email for staged review |
| | `./bin/hq gmail triage` | Run pure-Go unread sidecar precheck and batch triage items |
| **Memory & RSI Engine** | `./bin/hq memory status / topics` | Inspect 4-layer memory hierarchy and Knowledge Wiki pages |
| | `./bin/hq memory verify-transcript <convId>` | Verify claims against raw transcripts before promoting memory |
| | `./bin/hq memory add-fact / prune-fact` | Record or soft-prune bi-temporal atomic facts with provenance |
| | `./bin/hq memory compact` | Compact Roadmap Index (`MEMORY.md`) and deduplicate facts |
| | `./bin/hq rsi mine [--since=24h]` | Mine 0-token ground-truth friction events from journals |
| | `./bin/hq rsi hypothesize / verify / propose` | Synthesize, 4-gate verify, and stage self-improvement patches |
| | `./bin/hq rsi status` | Display RSI pipeline status, active hypotheses, and canaries |
| **CI/CD & Harness Contracts** | `./bin/hq ci pre-push` | Run full local CI gate (PII P1–P5, H1–H4B, `go vet`, unit & CUJ tests) |
| | `./bin/hq ci pre-commit` | Fast pre-commit scanner (staged PII, `gofmt -s`, markdown links) |
| | `./bin/hq ci check-pii` | Mathematical PII and Vault boundary integrity scanner |
| | `./bin/hq ci check-harness` | Verify skills, hook parity, model routing, and docs index |
| | `./bin/hq ci install-hooks` | Install `.git/hooks/pre-commit` and `.git/hooks/pre-push` |
