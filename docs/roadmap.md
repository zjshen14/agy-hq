# Implementation Roadmap & Action Tracker

This file is the **single source of truth** for tracking what is done, what is in progress right now, and what comes next across all design docs (`PRD`, `RFC`, and `ADR`).

- **How we use this file:**
  1. **Top-of-File "Up Next" Queue:** Always shows the immediate next 1–3 concrete tasks so we never have to hunt across multiple design docs to know what to build next.
  2. **Granular Milestone Checklists (`M0` → `M4`):** Every engineering task is a `- [ ]` checkbox linked to its governing `RFC`/`PRD` spec and target file path.
  3. **Commit-by-Commit Updates:** As we complete each item, we check it off (`- [x]`) in the same commit as the code change.

---

## 🎯 Up Next (Immediate Action Queue)

1. **[M4.1]** Implement optional Telegram adapter (`internal/channels/telegram.go`) using outbound HTTPS long-polling and inline approval buttons locked to `owner_chat_id`.
2. **[M4.2]** Add `hq eval` task replay runner (`pass^3`) to benchmark model and skill performance across synthetic scenarios.
3. **[M4.3]** Add optional `local/memory/index.sqlite` SQLite FTS5 historical journal search index.

---

## Milestone Checklists

### M0: Foundation Spikes & Safety Prerequisites
*Goal: Prove the Antigravity hook/sidecar/CLI integration points in isolation and lock down sandbox settings before running any autonomous tasks.*
- **Specs:** [RFC-0001](design/0001-vault-and-task-isolation.md), [RFC-0003](design/0003-approval-bound-execution.md), [ADR-0001](decisions/0001-option-1.5-hybrid-on-antigravity.md)

- [x] **M0.0:** Initialize `agy-hq` git repository, `.gitignore` Vault protections, `.git/hooks/pre-commit` PII guard, and modular `docs/` hierarchy (`PRD` / `RFC` / `ADR`).
- [x] **M0.1:** Scaffold `go.mod` (`github.com/zjshen14/agy-hq`) and `cmd/hq/main.go` with subcommands (`init`, `hook`, `propose`, `approve`, `reject`, `worktree`, `status`, `doctor`).
- [x] **M0.2:** Smoke-test `.agents/hooks.json` (`SessionStart`, `PreToolUse`, `Stop`) to confirm stdin JSON payload parsing (`conversationId`, `transcriptPath`, `workspacePaths`) and stdout JSON responses.
- [x] **M0.3:** Implement `SpawnFlashTask` (`agentapi new-conversation --model=flash`) in `internal/kernel/kernel.go` with daily spawn quota enforcement and `RESOURCE_EXHAUSTED` circuit-breaker detection.
- [x] **M0.4:** Add `hq doctor` check that inspects `~/.gemini/config/config.json` and warns if `Turbo` mode is active (recommending `Default` or `Auto` so macOS Seatbelt `sandbox-exec` is enforced) and confirms Remote Control status.

---

### M1: Core `hq` Microkernel, Private Vault & Starter Skills
*Goal: Deliver a working daily-driver assistant in Antigravity with persistent memory injection, 0-token turn journaling, approval-bound execution, the right-hand HQ Console, and starter skills.*
- **Specs:** [RFC-0001](design/0001-vault-and-task-isolation.md), [RFC-0002](design/0002-memory-and-consolidation.md), [RFC-0003](design/0003-approval-bound-execution.md), [PRD-0002](product/0002-open-workflows-and-skills.md)

- [x] **M1.1 (`hq init`, `hq worktree` & Templates):** Build `internal/vault`, `internal/worktree`, and `templates/` so `bin/hq init` creates the `local/` directory tree (`USER.md`, `MEMORY.md`, `config.yaml`, `triggers.yaml`, `inbox/`, `notes/`, `worktrees/`, `repos/`, `private/`, `memory/{journal,pending,outbox,proposals}`) and initializes `local/.git`.
- [x] **M1.2 (`hq hook session-start`):** Inject `local/USER.md`, `local/MEMORY.md`, and active facts from `local/memory/facts.jsonl` (`valid_to == null`) as a `systemMessage` in `<10ms`.
- [x] **M1.3 (`hq hook stop`):** Read `transcriptPath` directly in Go (0 LLM tokens) when a turn ends and write an immutable turn summary to `local/memory/journal/YYYY-MM-DD/<convId>-<stepIdx>.json`.
- [x] **M1.4 (`hq hook pre-tool` & `hq propose` / `approve` / `reject`):**
  - Hard-deny direct outbound/irreversible commands (`gws gmail send`, `gh pr merge`, `gh release create`, `vercel --prod`) in `PreToolUse`, prompting the agent to stage them via `./bin/hq propose`.
  - Implement `./bin/hq propose`, `./bin/hq approve --sha256=<prefix>`, and `./bin/hq reject` with SHA-256 verification and `local/memory/interventions.jsonl` audit logging.
- [x] **M1.5 (`hq daemon` & Right-Hand `HQ Console`):**
  - Implement `internal/webui` HTTP server (`127.0.0.1:4242`) and `hq daemon` / `hq webui`.
  - Render the **Pending Approvals Queue** (`[Approve (sha256)]` / `[Reject]`) and execute approved commands deterministically in Go with zero LLM in the loop.
  - Render the **Active Worktrees**, **Today's Turn Journal**, and **System Health** views.
- [x] **M1.6 (Starter Skills Bundle):** Author the public-safe starter skills in `.agents/plugins/agy-hq/skills/*/SKILL.md`:
  - [x] `morning-brief/SKILL.md`
  - [x] `sleep-consolidate/SKILL.md`
  - [x] `inbox-triage/SKILL.md`
  - [x] `ledger-ingest/SKILL.md`
  - [x] `tax-prep/SKILL.md`
  - [x] `thesis-check/SKILL.md`
  - [x] `filing-diff/SKILL.md`
  - [x] `swe-pulse/SKILL.md`

---

### M2: Proactive Loop, 0-Token Gatekeeper & Production Capabilities
*Goal: Turn `agy-hq` from a reactive assistant into an always-on proactive agent OS with automated scheduling, robust Front Desk coordination, decoupled Gmail integration, and 4-layer memory hierarchy.*
- **Specs:** [PRD-0001](product/0001-vision-and-ux.md), [PRD-0003](product/0003-critical-user-journeys.md), [RFC-0004](design/0004-heartbeat-and-channels.md), [RFC-0005](design/0005-gmail-integration.md), [RFC-0006](design/0006-memory-engine-upgrade.md), [RFC-0007](design/0007-front-desk-initialization-hardening.md)

- [x] **M2.1 (`local/inbox/` Drop Router & Quota):** Scan `local/inbox/` in `internal/kernel/kernel.go`, classify dropped files (`ledger-ingest`, `tax-prep`, `thesis-check`, or `inbox-triage` fallback), move to `local/inbox/queued/`, and dispatch background tasks with daily spawn quota circuit breaker.
- [x] **M2.2 (30m Zero-Token Heartbeat Gatekeeper & Dead-Man's Switch):** Run deterministic pre-checks (`./bin/hq heartbeat` & background loop in `hq daemon`); record `HEARTBEAT_OK (0 LLM tokens)` when idle, enforce `max_daily_spawns: 15`, and alert if `brief-YYYY-MM-DD.md` is missing after 08:00 AM.
- [x] **M2.3 (Background Daemon Service — `systemd` / `launchd`):** Implement `./bin/hq daemon install`, `status`, and `uninstall` managing LaunchAgent on macOS and systemd user services on Linux for true 24/7 autonomous monitoring.
- [x] **M2.4 (Front Desk Initialization Hardening & Auto-Arm Daemon — RFC-0007):**
  - Persistent Turn-1 initialization directive in `SessionStart` until live status artifact card (`hq_console.md`) is pinned.
  - Automatically arm all declared cron schedules in `local/triggers.yaml` (`morning-brief`, `edgar-close-watch`, `sleep-consolidate`).
  - Track `schedule` and `manage_task` calls in `PreToolUse` and reconcile armed triggers.
  - Exempt Turn-1 initialization housekeeping reads from the Front Desk lookup cap.
- [x] **M2.5 (Task Conversation Lifecycle & Native Subagent Tracking):**
  - Full task state machine (`active` → `completed` → `archived` / `reopen`) managed via `tasks.json` and CLI (`hq complete`, `hq archive`, `hq reopen`).
  - Track native `invoke_subagent` calls, quota increments, task board registration, and completion/cancellation via `PreToolUse` and `Stop` hooks.
  - Mechanical non-blocking Front Desk guardrails blocking direct source/worktree edits and heavy test suites, with a 3-lookup per-turn cap.
- [x] **M2.6 (Decoupled Native Gmail Integration — RFC-0005):**
  - Layer 1 primitives: Multi-account registry (`hq gmail init` / `accounts`), search (`hq gmail list`), sandboxed text reader (`hq gmail read`), and draft management (`hq gmail draft`).
  - Safe staged execution: Live sending strictly gated behind approval proposals (`hq propose --type=gmail_send` → `hq gmail send`).
  - Pure-Go unread sidecar precheck (`hq gmail triage`), batch triage results recording, and quarantined attachment handling.
- [x] **M2.7 (Four-Layer Memory Engine & Dreaming Falsification — RFC-0006):**
  - 4-layer stack: Roadmap Index (`MEMORY.md` < 2,500 tokens), Knowledge Wiki (`local/memory/topics/*.md`), bi-temporal facts (`local/memory/facts.jsonl`), and raw transcripts.
  - CLI fact management (`hq memory add-fact`, `prune-fact`, `compact`, `topics`).
  - Ground-truth dreaming verification (`hq memory verify-transcript`) preventing unverified or optimistic self-reports from compounding in long-term memory.
- [x] **M2.8 (End-to-End Black-Box Test Suite — CUJs 1–20):**
  - Full black-box regression suite in `internal/e2e/cuj_e2e_test.go` verifying all 20 Critical User Journeys against the compiled `bin/hq` binary (100% passing).

---

### M3: Production Integrations, Task Board Hierarchy & Self-Improvement
*Goal: Scale `agy-hq` with hybrid Google Chat transport, tiered task board management, embedded sidecar console, automated pre-IPO deal flow monitoring, recursive self-improvement, CI defense-in-depth, and Proactive Engine v2.*
- **Specs:** [RFC-0008](design/0008-gchat-integration.md), [RFC-0009](design/0009-task-board-redesign.md), [RFC-0010](design/0010-agent-recursive-self-improvement.md), [RFC-0011](design/0011-ci-pipeline-and-regression-harness.md), [RFC-0012](design/0012-token-observability.md), [RFC-0013](design/0013-open-source-launch-strategy.md), [RFC-0014](design/0014-proactive-engine-and-living-context.md)

- [x] **M3.1 (Decoupled Google Chat Integration — RFC-0008):**
  - Hybrid transport in `internal/gchat/`: Workspace OAuth REST API v1 + personal `@gmail.com` `--headless=new` Chrome CDP WebSocket transport (`127.0.0.1:9222`) with network jail and isolated profile in `local/private/gchat/chrome-profile`.
  - 1-step outbound proposal staging: `./bin/hq gchat propose --to="..." --body="..."` with fuzzy name/space resolution and composer tab pre-warming.
  - Quarantined reader gate (`hq gchat read-quarantined`) with session tainting (`local/memory/tainted_sessions.json`) to neutralize prompt injection.
  - Guided 1-time Chrome login with remote phone prompt support (`hq gchat login --remote`).
- [x] **M3.2 (Scalable Task Board & Layer-1 Hierarchy — RFC-0009):**
  - Layer-1 invariant: only top-level tasks and direct Front Desk subagents tracked in `tasks.json` and `hq_console.md`; child subagents tracked ephemerally in `nested_conversations.json`.
  - Two-tier hot/cold storage (`tasks.json` + `tasks_archive.jsonl`) with seamless cold recall via `hq reopen`.
  - CLI management: `hq tasks`, `hq tasks stats`, `hq tasks compact`, `hq tasks pin/unpin`.
- [x] **M3.3 (Interactive Web UI Console & Antigravity Sidecar):**
  - Embedded Go HTTP server on `127.0.0.1:4242` serving both browser dashboard and Antigravity Aux Pane (`sidecar://agy-hq-console/`).
  - Real-time Task Board, pending approval buttons with SHA-256 verification, git worktree status, and daemon health telemetry.
  - Zero-token Markdown sync (`webui.SyncMarkdownConsole`) updating `CONSOLE.md` and `hq_console.md` on every hook and heartbeat tick.
- [x] **M3.4 (Custom Domain Watchdogs & Structured Vault Ledgers):**
  - Declarative custom schedules in `local/triggers.yaml` (e.g., `edgar-close-watch`, deal-flow or filing monitors) paired with domain notes in `local/notes/investing/`.
  - Quarantined prospectus/filing extraction and structured retrospective ledgers without user prompting.
- [x] **M3.5 (Bounded Agent Recursive Self-Improvement — RFC-0010):**
  - 4-stage pipeline in `internal/rsi/`: 0-token friction trace mining (`hq rsi mine`), ACE delta hypotheses (`hq rsi hypothesize`), 4-gate sandboxed verification harness (`hq rsi verify`), and 1-tap approval staging (`hq rsi propose`).
  - Multi-tier canary tracking and automatic rollback on post-merge friction regression.
- [x] **M3.6 (Automated CI/CD Pipeline & Regression Harness — RFC-0011):**
  - Two-tier defense-in-depth: local pre-commit/pre-push hooks (`hq ci install-hooks`) and GitHub Actions workflow (`.github/workflows/ci.yml`).
  - Mathematical 6-invariant PII scanner (`hq ci check-pii [--history]`) and Harness contract verifier (`hq ci check-harness`).
  - Accelerated CUJ E2E suite (`cuj_e2e_test.go`) running in ~1.8 seconds with 100% test coverage.
- [x] **M3.7 (End-to-End Token & Context Observability — RFC-0012):**
  - Stat-cached transcript token/latency ingestion, ground-truth parent-child subagent DAG reconciliation, and `./bin/hq obs` CLI (`tokens`, `top`, `inspect`, `anomalies`, `budget`, `reconcile`).
  - Automated `SessionStart` 160k Front Desk `/compact` nudge and daily uncached/deep token budget circuit breakers.
- [x] **M3.8 (Open-Source Readiness, Governance & 5-Minute `--demo` Onboarding — RFC-0013):**
  - Contextual PII invariant (`P6-ContextualPII`), optional `local/private/pii-denylist.txt`, full commit history scanner (`hq ci check-pii --history`), deterministic history scrubber (`hq ci scrub-history`), and subprocess `GIT_DIR` sanitization.
  - Apache License 2.0 (`LICENSE`), `SECURITY.md`, `CONTRIBUTING.md`, `CODE_OF_CONDUCT.md`, GitHub templates, and opt-in synthetic sample dataset (`./bin/hq init --demo` / `./scripts/bootstrap.sh --demo`).
- [x] **M3.9 (Proactive Engine v2 & Living Context — RFC-0014):**
  - 0-token cursor gatekeepers (`trigger_cursors.json`), 10-minute triage coalescer, and daemon-aware trigger arming eliminating cold-cache Front Desk wakeups.
  - 4-tier Attention Router (`internal/outbox`: `interrupt`, `brief_rollup`, `ideas_feed`, `silent_ledger`), Quiet Hours gate, `./bin/hq outbox` + `./bin/hq ideas`, and RSI `proactive_noise` dismissal mining.
  - Stateful multi-day `Goals` & `WaitingOn` SLA sweep (`./bin/hq goals`), schedule-aware Console `Spaces` (`./bin/hq spaces` + `:4242` tabs), on-demand Entity Dossiers (`./bin/hq memory lookup`), and 4-layer surgical `./bin/hq memory forget [--block-pii]`.
  - Universal credential vault read-guard across all sessions (`local/private/{gmail,gchat,credentials,pii-denylist.txt}`), `24h` proposal expiration (`ExpiresAt`), outbound contextual-PII warning badges (`RiskFlags`), and `CUJ-21`..`CUJ-24` E2E tests.

---

### M4: Future Extensions & Evaluation Harness
*Goal: Measure reliability (`pass^k`) across model/skill changes, support alternative messaging channels, and scale memory lookup over multi-year journals.*
- **Specs:** [ADR-0004](decisions/0004-remote-control-first-pluggable-channels.md), [RFC-0002](design/0002-memory-and-consolidation.md)

- [ ] **M4.1 (Telegram Adapter — `internal/channels/telegram.go`):** Implement outbound HTTPS long-polling + inline approval buttons locked to the owner's `chat_id`.
- [ ] **M4.2 (Private Eval Suite):** Add `hq eval` runner to replay representative tasks (`pass^3`) before promoting proposed `SKILL.md` updates.
- [ ] **M4.3 (SQLite FTS5 Index):** Add optional `local/memory/index.sqlite` FTS5 index rebuilt from `journal/` and `facts.jsonl` once markdown/JSONL files exceed ~5,000 lines.
