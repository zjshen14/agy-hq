# Implementation Roadmap & Action Tracker

This file is the **single source of truth** for tracking what is done, what is in progress right now, and what comes next across all design docs (`PRD`, `RFC`, and `ADR`).

- **How we use this file:**
  1. **Top-of-File "Up Next" Queue:** Always shows the immediate next 1–3 concrete tasks so we never have to hunt across multiple design docs to know what to build next.
  2. **Granular Milestone Checklists (`M0` → `M4`):** Every engineering task is a `- [ ]` checkbox linked to its governing `RFC`/`PRD` spec and target file path.
  3. **Commit-by-Commit Updates:** As we complete each item, we check it off (`- [x]`) in the same commit as the code change.

---

## 🎯 Up Next (Immediate Action Queue)

1. **[M4.1]** Ship an optional Telegram companion `hq-channel-bridge/v1` extension pack (building on the `internal/channels/` bridge runtime from [RFC-0023](design/0023-front-desk-channel-bridge.md)) with outbound long-polling and signed `bca_`/`bcr_` approval callbacks.
2. **[M4.2]** Extend `hq memory eval` into a general `hq eval` task replay runner (`pass^3`) to benchmark model and `SKILL.md` performance across synthetic scenarios.
3. **[M4.3]** Add optional `local/memory/index.sqlite` SQLite FTS5 index (extending [RFC-0024](design/0024-memory-v2-prediction-and-eval.md)'s pure-Go BM25 `hq memory search`) once journals/facts scale to multi-year corpora.

---

## Milestone Checklists

### M0: Foundation Spikes & Safety Prerequisites
*Goal: Prove the Antigravity hook/sidecar/CLI integration points in isolation and lock down sandbox settings before running any autonomous tasks.*
- **Specs:** [RFC-0001](design/0001-vault-and-task-isolation.md), [RFC-0003](design/0003-approval-bound-execution.md), [ADR-0001](decisions/0001-option-1.5-hybrid-on-antigravity.md)

- [x] **M0.0:** Initialize `agy-hq` git repository, `.gitignore` Vault protections, `.git/hooks/pre-commit` PII guard, and modular `docs/` hierarchy (`PRD` / `RFC` / `ADR`).
- [x] **M0.1:** Scaffold `go.mod` (`github.com/zjshen14/agy-hq`) and `cmd/hq/main.go` with subcommands (`init`, `hook`, `propose`, `approve`, `reject`, `worktree`, `status`, `doctor`).
- [x] **M0.2:** Smoke-test `.agents/hooks.json` (`SessionStart`, `PreToolUse`, `Stop`) to confirm stdin JSON payload parsing (`conversationId`, `transcriptPath`, `workspacePaths`) and stdout JSON responses. *(Since consolidated into `.agents/plugins/agy-hq/hooks.json` in RFC-0015)*
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
- [x] **M1.6 (Starter Skills Bundle):** Author the public-safe starter skills in `.agents/plugins/agy-hq/skills/*/SKILL.md` *(6 domain/channel starter skills moved to `templates/skills/` → `<vault>/skills/` in RFC-0019)*:
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
  - 4-layer stack: Roadmap Index (`MEMORY.md` < 2,500 tokens, *later tightened to `<= 40 lines / 2,200 bytes` in RFC-0024*), Knowledge Wiki (`local/memory/topics/*.md`), bi-temporal facts (`local/memory/facts.jsonl`), and raw transcripts.
  - CLI fact management (`hq memory add-fact`, `prune-fact`, `compact`, `topics`).
  - Ground-truth dreaming verification (`hq memory verify-transcript`) preventing unverified or optimistic self-reports from compounding in long-term memory.
- [x] **M2.8 (End-to-End Black-Box Test Suite — CUJs 1–20):**
  - Full black-box regression suite in `internal/e2e/cuj_e2e_test.go` verifying all 20 Critical User Journeys against the compiled `bin/hq` binary (100% passing).

---

### M3: Production Integrations, Platform Extensibility, Security Lattice & Proactive v3
*Goal: Scale `agy-hq` with hybrid Google Chat transport, tiered task board management, embedded sidecar console, recursive self-improvement, CI defense-in-depth, global plugin packaging, modular connectors/bridges/workspaces, Memory v2, unified `authz` & scoped delegates, and Muse proactive actions.*
- **Specs:** [RFC-0008](design/0008-gchat-integration.md)–[RFC-0027](design/0027-muse-proactive-actions.md) (`RFC-0020` superseded by `RFC-0027`)

- [x] **M3.1 (Decoupled Google Chat Integration — RFC-0008):**
  - Hybrid transport in `internal/gchat/`: Workspace OAuth REST API v1 + personal `@gmail.com` `--headless=new` Chrome CDP WebSocket transport (`127.0.0.1:9222`) with network jail and isolated profile in `local/private/gchat/chrome-profile`.
  - 1-step outbound proposal staging: `./bin/hq gchat propose --to="..." --body="..."` with fuzzy name/space resolution and composer tab pre-warming.
  - Quarantined reader gate (`hq gchat read-quarantined`) with session tainting (`local/memory/tainted_sessions.json`, *later hardened to `local/private/taint/` and `hq offer stage` in RFC-0017b*) to neutralize prompt injection.
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
  - Accelerated CUJ E2E suite (`internal/e2e/cuj*_test.go`, sharing a `sync.Once` binary build across all `49` `TestCUJ*` tests for CUJs 1–47).
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
- [x] **M3.10 (Global Plugin Packaging & Standalone Vault — RFC-0015):**
  - Global plugin bundle (`~/.gemini/config/plugins/agy-hq/`), `hq plugin` / `hq version` / `hq vault-path`, `//go:embed` templates, and single-source `.agents/plugins/agy-hq/hooks.json`.
- [x] **M3.11 (Sidebar Conversation Archive Sync — RFC-0016):**
  - `hq tasks sync-sidebar`, ConnectRPC `UpdateConversationAnnotations` mirroring, and offline retry queue.
- [x] **M3.12 (Modular Connectors, Taint, Brief & Extensions — RFC-0017a–0017d):**
  - Modular connectors & 0-token watchers (`hq connector`, `hq watch`), fail-closed `O_EXCL` taint & `/dev/fd/3` `hq offer stage` + `--cap`, two-phase `hq brief collect --lease`/`commit`, companion `hq-extension.json` packs, `private/trust.json`, `hq dispatch resolve`, and pure-Go `hq sidecar console`.
- [x] **M3.13 (Always-On Agent Runtime Hardening — RFC-0018):**
  - Crash-safe `hq events`, durable `hq timers`, `hq progress` checkpoints, `hq exec-sandboxed`, `reported.jsonl`, and `hq outbox eval`.
- [x] **M3.14 (Two-Tier RSI & Vault Skill Split — RFC-0019):**
  - Split `.agents/plugins/agy-hq/skills/` (`3` Core OS skills) from `local/skills/` + `6` `templates/skills/` starters (`hq skill`), with Tier-1 Vault overlays (`hq rsi export --upstream`, `migrate-overlays`).
- [x] **M3.15 (Customizable Approval Policy & UX Hardening — RFC-0021):**
  - Data-driven `approvals.rules`, `owned_repos`, `24h` `ask_ttl`, `--dismiss`, and 0-token `Reject`/git-ref reconciliation.
- [x] **M3.16 (Vendor-Neutral Front Desk Channel Bridge — RFC-0023):**
  - `hq-channel-bridge/v1` (`internal/channels/`, `hq channel`), Front Desk rotation tunneling (`bind-desk`), and signed `bca_`/`bcr_` callbacks.
- [x] **M3.17 (Memory Engine v2, Prediction & Eval — RFC-0024):**
  - Dual-capped `MEMORY.md` (`<= 40L / 2.2 KB`), scoped `facts.jsonl`, `1:1` `## Active Playbook`, pure-Go BM25 `hq memory search`, `dream-plan --precheck`, and `hq memory eval` (`F1`–`F4`).
- [x] **M3.18 (Pluggable Workspace Providers — RFC-0025):**
  - `internal/workspace` provider seam (`hq-workspace-provider/v1`), `hq workspace`, `hq spawn --workspace`, `workspaces.json`, and `W1`–`W4` hygiene.
- [x] **M3.19 (Unified Authz Lattice & Scoped Delegates — RFC-0026):**
  - 5-axis `internal/authz` Lattice (`NI-1`–`NI-4`), `approvals.interactive_preset`, and Declarative Scoped Delegates (`internal/delegates`, `internal/redact`, `hq delegate`, CaMeL `queue_only` / `spawn_sandboxed_worker`).
- [x] **M3.20 (Muse Proactive Actions & Living Spaces — RFC-0027, superseding RFC-0020):**
  - 0-token `internal/pulse` look-ahead (`hq pulse`, `D1`–`D10`), 1-tap `DecisionOption` cards (`hq outbox act`, `hq cancel`, `hq approvals restage`), 3-state Goal checklists, and composable Living Spaces (`internal/living`, `hq space`).
- [x] **M3.21 (Harness Anti-Slop Gates & Round-3 Compaction):**
  - Static CI gates `H5-A`–`H5-F`, `CUJs 1–47` (`49` `TestCUJ*` E2E tests), and CLI/template/doc compaction.

---

### M4: Future Extensions & Evaluation Harness
*Goal: Measure reliability (`pass^k`) across model/skill changes, support companion messaging channel packs, and scale memory lookup over multi-year journals.*
- **Specs:** [ADR-0004](decisions/0004-remote-control-first-pluggable-channels.md), [RFC-0023](design/0023-front-desk-channel-bridge.md), [RFC-0024](design/0024-memory-v2-prediction-and-eval.md)

- [ ] **M4.1 (Telegram Companion Bridge Pack):** Ship an optional Telegram `hq-channel-bridge/v1` companion pack (extending `internal/channels/`) with outbound HTTPS long-polling + signed `bca_`/`bcr_` callbacks locked to `owner_chat_id`.
- [ ] **M4.2 (General Task Eval Suite):** Extend `hq memory eval` into an `hq eval` runner to replay representative tasks (`pass^3`) before promoting proposed `SKILL.md` updates.
- [ ] **M4.3 (SQLite FTS5 Index):** Add optional `local/memory/index.sqlite` FTS5 index (extending BM25 `hq memory search`) rebuilt from `journal/` and `facts.jsonl` once corpora scale to multi-year logs.
