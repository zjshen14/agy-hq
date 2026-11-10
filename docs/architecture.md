# System Architecture Overview

> For detailed subsystem specifications, see [Technical Designs (`docs/design/`)](README.md#3-technical-designs-docsdesign). For product workflows, see [Product Designs (`docs/product/`)](README.md#2-product-designs-docsproduct).

---

## 1. Architectural Philosophy

**`agy-hq` (Headquarter)** turns an always-on Mac running Google Antigravity into a 24/7 general-purpose, proactive personal assistant using four core principles:

1. **General-Purpose by Default, Boosted by Additive Skills ([ADR-0006](decisions/0006-general-agent-and-skills-over-packs.md), [PRD-0002](product/0002-open-workflows-and-skills.md)):** The agent is never boxed into rigid domain packs. It can handle *any* open-ended request (writing a post, planning a trip, debugging code, researching a stock) out of the box, while progressive-disclosure `.agents/skills/*/SKILL.md` files provide expert runbooks when relevant.
2. **Piggyback on Antigravity's Native Strengths ([ADR-0001](decisions/0001-option-1.5-hybrid-on-antigravity.md)):** Reuse Antigravity's code execution, macOS Seatbelt sandbox, Chrome DevTools browser automation, MCP client, subagents, cron scheduler, and mobile **Remote Control** PWA.
3. **A Thin Go Microkernel (`bin/hq`, [ADR-0002](decisions/0002-go-microkernel-binary.md)):** Plug into Antigravity's `hooks.json` and `sidecar.json` interfaces to add cross-session memory, zero-token event pre-checks, lock-free per-task parallel isolation, and deterministic approval gates.
4. **Strict Two-Layer Separation ([ADR-0003](decisions/0003-two-layer-oss-and-private-vault.md), [RFC-0001](design/0001-vault-and-task-isolation.md)):** Keep the open-source engine and reusable skills in the public-safe root repository, while isolating 100% of personal profile data, memories, journals, triggers, and files inside `local/` (an independent private git repository).

---

## 2. System Topology

```mermaid
flowchart TB
  subgraph Touchpoints["1. User Touchpoints (PRD-0001)"]
    RC["Antigravity Remote Control (Web/PWA)\n1-Tap Proposals & Mobile Chat"]
    DESK["Antigravity Desktop App\nSingle Front Desk Coordinator + Task Sidebar"]
    SIDECAR["Antigravity Aux Pane Sidecar\nsidecar://agy-hq-console/ (Live Dashboard)"]
    DROP["Zero-Prompt Drop Folder\nlocal/inbox/"]
  end

  subgraph DaemonLayer["2. Always-On Background Daemon (RFC-0004, systemd / launchd)"]
    DAEMON["hq daemon (run / service)\n• 15s Drop Folder Router (fsnotify / polling)\n• 30m Zero-Token Heartbeat Gatekeeper & Dead-Man's Switch\n• Native Gmail & Google Chat Sidecar Watchers"]
    WEBUI["HQ Console Web Server\nhttp://127.0.0.1:4242 (Aux Dashboard)"]
  end

  subgraph Microkernel["3. Go Microkernel Engine (bin/hq)"]
    HOOKS["Lifecycle Hooks (RFC-0002, RFC-0003, RFC-0007)\n• SessionStart: Injects USER.md, MEMORY.md, Topic Wiki Index\n• PreToolUse: Non-blocking Front Desk guard & SHA-256 action blocklist\n• Stop: 0-token transcript extraction & monotonic turn journaling"]
    EXEC["Approval-Bound Executor (RFC-0003)\n• Watches local/memory/pending/<id>.json\n• SHA-256 verification + 0-LLM deterministic execution"]
    TASKS["Layer-1 Task Engine (RFC-0001, RFC-0007, RFC-0009)\n• local/memory/tasks.json (active) vs tasks_archive.jsonl (cold)\n• Isolated git worktrees (local/worktrees/<repo>-<task>/)"]
    GCHAT["Google Chat CDP & Gmail Transports (RFC-0005, RFC-0008)\n• Headless Chrome CDP WebSocket (127.0.0.1:9222)\n• Quarantined readers & 1-step proposal staging"]
    RSI["Recursive Self-Improvement Engine (RFC-0010)\n• 4-stage friction mining, hypothesis, 4-gate verification"]
    CI["CI/CD & Harness Contracts (RFC-0011)\n• Mathematical PII guards (P1-P5) & Harness gates (H1-H4B)"]
  end

  subgraph AgentLayer["4. Antigravity Agent Sessions (PRD-0002)"]
    FD["[HQ] Front Desk Coordinator\n(Non-blocking intake, <=3 lookups, 0 source edits)"]
    SUB["Direct In-Session Subagents\n(invoke_subagent bounded tasks -> tasks.json)"]
    TASKCONV["Dedicated Task Conversations\n(hq spawn open-ended sidebar threads -> tasks.json)"]
    CHILD["Ephemeral Child Subagents\n(nested subagents -> nested_conversations.json)"]
  end

  RC <--> DESK
  RC <--> DAEMON
  DESK <--> SIDECAR
  DROP --> DAEMON
  DAEMON --> HOOKS
  DESK <--> HOOKS
  HOOKS --> FD
  FD --> SUB
  FD --> TASKCONV
  SUB -.-> CHILD
  TASKCONV -.-> CHILD
  HOOKS <--> EXEC
  HOOKS <--> TASKS
  HOOKS <--> GCHAT
  HOOKS <--> RSI
  HOOKS <--> CI
```

---

## 3. Background Daemon Architecture (`systemd` / `launchd`)

To deliver genuine 24/7 proactive personal assistance without requiring open IDE windows or manual CLI runs, `agy-hq` includes an always-on background daemon:

- **OS Service Management:** Installed and uninstalled via `./bin/hq daemon install`, `./bin/hq daemon status`, and `./bin/hq daemon uninstall`.
  - On **macOS**: Configures a user LaunchAgent in `~/Library/LaunchAgents/com.antigravity.hq.daemon.plist` with `RunAtLoad` and `KeepAlive`.
  - On **Linux**: Configures a user systemd service unit in `~/.config/systemd/user/hq-daemon.service` with automatic restarts and journal logging.
- **Zero-Token Idle Pre-Checks (RFC-0004):** The daemon executes two high-frequency pure-Go timers in 0 LLM tokens:
  1. **15-Second Inbox Scanner:** Scans `local/inbox/`, routes incoming files (CSVs $\rightarrow$ `ledger-ingest`, tax PDFs $\rightarrow$ `tax-prep`, SEC filings $\rightarrow$ `thesis-check`, general $\rightarrow$ `inbox-triage`), moves them to `local/inbox/queued/`, and triggers quarantined execution.
  2. **30-Minute Heartbeat Gatekeeper & Dead-Man's Switch:** Evaluates daily quota caps, checks unread status, and monitors whether today's `brief-YYYY-MM-DD.md` has been compiled by 08:00 AM, firing a high-priority alert if missed.
- **Embedded Aux Web UI:** Hosts the local HTTP server (`127.0.0.1:4242`) powering both the Antigravity Sidecar pane (`sidecar://agy-hq-console/`) and standalone browser dashboard.

---

## 4. Mechanical Non-Blocking Front Desk Guardrails

The single `[HQ] Front Desk` conversation acts as the intake coordinator across Desktop and Mobile Remote Control. To keep this thread responsive and prevent long-running tasks from locking the user out of conversations, `internal/hooks` enforces mechanical `PreToolUse` guardrails:

```mermaid
flowchart TD
    U["User Request at [HQ] Front Desk"] --> C{"Task Shape & Scope"}
    C -->|"Quick Factoid (<=3 tool calls)"| INLINE["1. Inline Execution\nFast fact check (weather, calendar, status)"]
    C -->|"Bounded Autonomous Task"| SUB["2. Background Subagent\ninvoke_subagent (standard / flash)\nUnidirectional report back to Front Desk"]
    C -->|"Open-Ended / Architectural Initiative"| SPAWN["3. Dedicated Task Conversation\nhq spawn (deep / pro)\nIndependent sidebar thread + isolated worktree"]
```

- **Zero Source/Worktree Edits:** Direct file modifications (`edit_file`, `write_file`) in source code or worktrees are hard-blocked by `PreToolUse` on the Front Desk thread.
- **Zero Heavy Build/Test Suites:** Commands like `go test`, `npm test`, `cargo test`, or `make test` are rejected inline; they must be executed inside dedicated subagents or task worktrees.
- **Inline Lookup Cap:** Inline research tool calls (`read_file`, grep, find, shell lookups) are capped at **3 calls per turn**. (Turn-1 initialization housekeeping reads of `local/CONSOLE.md`, `hq_console.md`, and `local/triggers.yaml` are explicitly exempted).
- **Silent Ledger Discipline:** Subagent logs, quota counters, and task board tables stay strictly inside the live `hq_console.md` artifact; the Front Desk only delivers synthesized executive takeaways upon task completion.

---

## 5. Task Conversation Lifecycle State Machine

Task conversations and subagents move through a deterministic lifecycle tracked in `local/memory/tasks.json` and mirrored in the live `hq_console.md` Task Board:

```mermaid
stateDiagram-v2
    direction LR
    [*] --> Active: hq spawn / invoke_subagent / hq adopt
    Active --> Completed: hq complete / subagent Stop turn
    Completed --> Archived: hq archive
    Archived --> Active: hq reopen
    Active --> Cancelled: manage_subagents(kill)
    Completed --> [*]
    Archived --> [*]
    Cancelled --> [*]
```

| State | Transition Trigger | Actions Taken |
|---|---|---|
| **`🟢 active`** | `hq spawn --task=<slug>`, `invoke_subagent`, or `hq adopt` | Allocates isolated worktree (`local/worktrees/<repo>-<task>/`), registers task in `tasks.json`, syncs `hq_console.md`. |
| **`✅ completed`** | `hq complete <slug-or-id>` or child subagent `Stop` hook | Updates task status to `completed`, preserves summary and timing, updates Task Board. |
| **`📦 archived`** | `hq archive <slug-or-id>` | Moves completed task to archived section on Task Board, freeing active view space while preserving history. |
| **`🟢 active` (reopened)** | `hq reopen <slug-or-id>` | Restores archived task back to active state for further work. |
| **`🛑 cancelled`** | `manage_subagents(Action: "kill")` | Subagent killed before normal turn completion; marked cancelled in `tasks.json`. |

### 5.1 Layer-1 Taskboard & Quota Invariant (`tasks.json` vs. `nested_conversations.json`)

To prevent hierarchical agent swarms from exhausting quota and flooding the UI task board with transient child workers, `agy-hq` enforces a strict **Layer-1 Taskboard Invariant**:

- **Layer 1 Tasks (`local/memory/tasks.json` & `hq_console.md`):**
  Only direct tasks orchestrated by the user or the Front Desk coordinator count toward the Task Board and daily background spawn quota (`maxDailySpawns: 15`):
  1. Direct subagents dispatched from the Front Desk via `invoke_subagent` or `hq spawn --subagent`.
  2. Dedicated sidebar Task Conversations spawned via `./bin/hq spawn --task=<slug>`.
  3. Automated proactive triggers (`kind: auto`) defined in `local/triggers.yaml`.
- **Ephemeral Child Processes (`local/memory/nested_conversations.json`):**
  Nested subagents created by a subagent or secondary task conversation to handle complex sub-problems are NOT tasks. They are ephemeral child processes recorded in `nested_conversations.json`:
  - They do **not** appear on the Task Board or in `tasks.json`.
  - They do **not** consume or increment the top-level daily spawn quota.
  - They do **not** generate standalone task status cards on the Front Desk.
- **Hot/Cold Lifecycle & Compaction (RFC-0009):**
  The Task Board maintains a fixed display budget in `hq_console.md` (100% active tasks shown, capped completed/dormant). The nightly consolidation workflow (`/sleep-consolidate`) runs `./bin/hq tasks compact` to rotate completed (>24h) and dormant (>48h) tasks from `tasks.json` into cold append-only storage (`local/memory/tasks_archive.jsonl`). If an archived task is revived via `./bin/hq reopen <slug>`, the microkernel transparently recalls it from cold storage back to `tasks.json`.

---

## 6. Google Chat Chrome CDP Transport & Quarantine Gate (RFC-0008)

To support Google Chat across both Google Workspace and personal `@gmail.com` accounts without fragile third-party scrapers or unvetted browser extensions, `agy-hq` implements a dual-transport microkernel architecture:

1. **Hybrid Transport Adapters (`internal/gchat/`):**
   - **Workspace Accounts:** Native Google Chat REST API v1 (`chat.spaces`, `chat.messages`) using OAuth2 service/user credentials.
   - **Personal `@gmail.com` Accounts:** Chrome DevTools Protocol (CDP) WebSocket transport (`http://127.0.0.1:9222`) driving an isolated `--headless=new` Chrome instance with network jail (DNS and URL allowlists restricted to Google Chat domains) and cookies stored exclusively in `local/private/gchat/chrome-profile`.
2. **1-Step Outbound Staging (`./bin/hq gchat propose`):**
   - Accepting human-friendly space names, email addresses, or handles:
     ```sh
     ./bin/hq gchat propose --to="Project Launch Space" --body="Hello team"
     ```
   - Automatically resolves names via fuzzy cache (`spaces_cache.json`) or PeopleKit search, pre-warms the target space tab in headless Chrome, drafts the message, and stages a SHA-256 bound `gchat_send` proposal (`local/memory/proposals/<id>.json`).
3. **Quarantine Reader Gate (`hq gchat read-quarantined`):**
   - Full message body inspection is restricted to quarantined subagents. Calling `read-quarantined` permanently flags the conversation ID in `local/memory/tainted_sessions.json`.
   - Tainted sessions have `search_web` blocked and `run_command` restricted strictly to read/draft/propose operations, neutralizing prompt-injection exfiltration attacks from malicious chat messages.
4. **1-Tap Mobile Approval:**
   - Outbound messages are presented via `ask_question` with exact SHA-256 prefixes and recipient previews, executed deterministically by `./bin/hq approve <id> --sha256=<prefix>` with zero LLM intervention.

---

## 7. Interactive Web UI Console & Antigravity Sidecar (Port 4242)

The HQ Console provides real-time visibility and control across both Desktop and Mobile Remote Control through an embedded Go HTTP microkernel (`internal/webui/webui.go`):

1. **Dual-Mode Access:**
   - **Antigravity Aux Pane Sidecar:** Registered in [`.agents/plugins/agy-hq/sidecars/agy-hq-console/sidecar.json`](../.agents/plugins/agy-hq/sidecars/agy-hq-console/sidecar.json) via URI `sidecar://agy-hq-console/`. Automatically mounts alongside the chat conversation in the Antigravity desktop IDE.
   - **Standalone Web UI:** Served directly by `./bin/hq daemon` and `./bin/hq webui` on `http://127.0.0.1:4242` for desktop browsers and mobile devices.
2. **Deterministic Zero-Token Markdown Console Sync:**
   - On every lifecycle hook event (`SessionStart`, `PreToolUse`, `Stop`) and background daemon heartbeat tick, `webui.SyncMarkdownConsole` regenerates `local/CONSOLE.md` and writes `<artifactDir>/hq_console.md` (and its `.metadata.json`) in 0 LLM tokens.
   - The Front Desk thread automatically pins this live artifact on Turn 1 (`consolePinned: true` in `front_desk.json`), providing an always-updated task ledger without consuming conversation context.
3. **Live Dashboard Telemetry:**
   - **Pending Approvals Queue:** Displays staged outbound actions (`prop-*`) with 1-click deterministic execution buttons verified against SHA-256 hashes.
   - **Tiered Task Board:** Displays active tasks, model tiers, worktrees, and elapsed time with hot/cold archive counters.
   - **Active Git Worktrees:** Real-time visibility into isolated task workspaces (`local/worktrees/<repo>-<task>/`).
   - **System Health & Daemon Telemetry:** Heartbeat status, daily spawn quota gauges (`spawnsToday / maxDailySpawns`), and service uptime.

---

## 8. Four-Layer Memory Hierarchy & Silent Dreaming (RFC-0006)

To prevent long-running agents from suffering from memory bloat or compounding false self-reported summaries, `agy-hq` implements Anthropic-grounded four-layer memory architecture:

```mermaid
flowchart TD
    subgraph L1["Layer 1: Injected Context / Roadmap Index"]
        M["local/MEMORY.md (< 2,500 tokens)\nTightly bounded index auto-injected by SessionStart on every turn"]
    end
    subgraph L2["Layer 2: Knowledge Wiki / Topic Pages"]
        TOPICS["local/memory/topics/*.md\nCurated markdown wiki pages for domains, architecture, and projects\nLoaded on-demand via explicit retrieval directives"]
    end
    subgraph L3["Layer 3: Episodic Notes & Bi-Temporal Facts"]
        FACTS["local/memory/facts.jsonl + journal/YYYY-MM-DD/<convId>.json\nAtomic facts with provenance managed via hq memory add-fact / prune-fact"]
    end
    subgraph L4["Layer 4: Immutable Raw Transcripts"]
        TRANS["transcript.jsonl (Antigravity Brain Logs)\nFull ground-truth execution transcripts used by hq memory verify-transcript"]
    end
    L1 -->|Index pointers| L2
    L2 -->|Distilled from| L3
    L3 -->|Verified against| L4
```

1. **Layer 1: Roadmap Index (`local/MEMORY.md`):** Tightly capped summary (<2,500 tokens) of active priorities, durable user preferences, and a clickable index of topic pages.
2. **Layer 2: Knowledge Wiki (`local/memory/topics/*.md`):** Modular topic pages covering system architecture, project playbooks, and domain lessons. `SessionStart` injects retrieval directives so the agent reads matching topic files before re-investigating.
3. **Layer 3: Bi-Temporal Atomic Facts (`local/memory/facts.jsonl`):** Structured facts with provenance timestamps (`valid_from`, `valid_to`, `source_conv`, `superseded_by`). Managed via CLI commands: `hq memory add-fact`, `hq memory prune-fact`, and `hq memory compact`.
4. **Layer 4: Immutable Raw Transcripts (`transcript.jsonl`):** The ground-truth log of every tool call, input, and response. Nightly consolidation ("dreaming" via `/sleep-consolidate`) runs `hq memory verify-transcript` to falsify self-reported claims against raw execution before promoting them into durable memory.

### 8.1 Silent Night Consolidation Invariant (Zero-Output Background Dreaming)

Automated nightly sleep consolidation (`sleep-consolidate` / `[Auto] Nightly Sleep Consolidation`, scheduled at 11:30 PM PT via `local/triggers.yaml`) executes under a strict **Zero-Output Invariant**:
- **100% Silent Execution:** The consolidation workflow runs entirely in the background without posting status banners, mechanical checklists, progress updates, or completion announcements to the `[HQ] Front Desk` conversation.
- **Vault & Ledger Confinement:** All memory audits, transcript verifications (`hq memory verify-transcript`), topic wiki distillations (`local/memory/topics/*.md`), bi-temporal fact prunings (`hq memory prune-fact`), and RSI proposal staging remain strictly confined to `local/`, `local/memory/journal/`, and the private Vault git repository.
- **Morning Brief Roll-Forward:** Any distilled operational knowledge, system health warnings, or actionable proposals roll forward cleanly into the next day's 7:30 AM Morning Brief (`/morning-brief`), keeping the Front Desk conversation pristine.

### 8.2 Silent Memory & Ledger Operations Invariant

Internal memory updates and bookkeeping operations must be 100% transparent and silent:
- **Zero Implementation Details in Conversation:** Never announce file modifications or path references (e.g., `project-notes.md`, `facts.jsonl`, `MEMORY.md`, `tasks.json`) in conversational replies.
- **Seamless Experience:** Fact indexing, topic page refreshes, and turn logging occur quietly in the background without cognitive clutter, allowing the user to experience seamless continuity without being burdened with internal bookkeeping mechanics.

---

## 9. Repository & Vault Layout

```text
agy-hq/                                # LAYER 1: Open-Source Agent OS (Root Git Repo)
├── .gitignore                         # Ignores /local/, /bin/, and *.secret*
├── AGENTS.md                          # General-purpose assistant rules (mounts ./local/)
├── go.mod                             # Go module for bin/hq
├── cmd/hq/                            # CLI: init | hook | daemon | propose | notify | status | memory | gmail | gchat | spawn | subagent | tasks | rsi | ci
├── internal/                          # Go microkernel packages
│   ├── kernel/                        # Trigger runner, worker pool, daemon installer, quota circuit breaker
│   ├── hooks/                         # SessionStart, PreToolUse action/PII guard, Stop parser
│   ├── approvals/                     # SHA-256 pending queue & deterministic CLI executor
│   ├── ci/                            # CI/CD PII scanners, harness contracts & git hook installer
│   ├── doctor/                        # Diagnostic checks for sandbox, CLI, and Vault health
│   ├── e2e/                           # Black-box E2E tests for CUJ 1–18
│   ├── gchat/                         # Decoupled Google Chat CDP WebSocket & REST transports
│   ├── gmail/                         # Decoupled Gmail Layer 1 primitives & sidecar engine
│   ├── rsi/                           # 4-stage recursive self-improvement engine
│   ├── vault/                         # Memory auditing, 4-layer topics, bi-temporal fact engine
│   ├── webui/                         # Interactive Aux Console (sidecar://agy-hq-console/, http://127.0.0.1:4242)
│   └── worktree/                      # Isolated git worktree lifecycle manager
├── templates/                         # Starter blueprints copied into ./local/ by `hq init`
├── docs/                              # Documentation Hub (product/, design/, decisions/, research/)
├── .agents/
│   ├── hooks.json                     # Connects Antigravity lifecycle hooks to ./bin/hq
│   └── plugins/agy-hq/                # Official plugin bundle:
│       ├── sidecars.json              # Sidecar manifest for agy-hq-console aux pane
│       ├── sidecars/agy-hq-console/   # Aux Pane entrypoint and sidecar bridge
│       └── skills/                    # Additive, progressive-disclosure SKILL.md workflows:
│           ├── morning-brief/
│           ├── sleep-consolidate/
│           ├── swe-pulse/
│           ├── thesis-check/
│           ├── inbox-triage/
│           ├── ledger-ingest/
│           ├── tax-prep/
│           ├── filing-diff/
│           └── gchat/
│
└── local/                             # LAYER 2: Private User Vault (Independent Private Git Repo)
    ├── .git/                          # Backed up to private remote (never public)
    ├── .gitignore                     # Ignores worktrees/ and repos/ inside the Vault git repo
    ├── config.yaml                    # Channel settings, concurrency & daily quota budgets
    ├── triggers.yaml                  # Declarative cron schedules, 0-token pre-checks & inbox rules
    ├── USER.md                        # Personal profile, preferences, writing voice & goals
    ├── MEMORY.md                      # Consolidated Roadmap Index (<2,500 tokens)
    ├── CONSOLE.md                     # Live status artifact & task board template
    ├── inbox/                         # Drag-and-drop folder (CSVs, PDFs, notes, receipts)
    ├── notes/                         # Open-ended non-repo work (posts, research, theses, plans)
    ├── worktrees/                     # Parallel git worktrees (<repo>-<task>/) across any GitHub repo
    ├── repos/                         # Optional bare git clones for external GitHub repos
    ├── private/                       # High-PII records (ledger/, taxes/, gchat/) with strict context guard
    └── memory/                        # 4-layer memory store:
        ├── topics/                    # Topic wiki pages (RFC-0006)
        ├── facts.jsonl                # Bi-temporal atomic facts (RFC-0006)
        ├── journal/YYYY-MM-DD/        # Monotonic per-turn execution journals
        ├── tasks.json                 # Tracked Layer-1 task conversations & lifecycle states
        ├── tasks_archive.jsonl        # Cold storage for archived tasks (RFC-0009)
        ├── nested_conversations.json  # Ephemeral child subagent processes
        ├── front_desk.json            # Active Front Desk intake lease & armed triggers
        ├── gmail/                     # Gmail registry, drafts, and quarantine staging (RFC-0005)
        ├── gchat/                     # Google Chat spaces cache, drafts, and session state (RFC-0008)
        ├── pending/                   # Staged proposals for 1-tap approval (RFC-0003)
        ├── proposals/                 # Full proposal metadata and execution logs
        ├── outbox/                    # Executed proposals and delivery payloads
        └── interventions.jsonl        # Cryptographic execution and intervention audit trail
```
