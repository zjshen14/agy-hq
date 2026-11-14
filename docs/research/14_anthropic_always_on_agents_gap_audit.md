# 14 — Anthropic Always-On Agents Architecture & Forensic Gap Audit Against `agy-hq`

- **Date:** 2026-11-14
- **Author:** `[Task:headquarter] Benchmark Anthropic Always-On Agents` (`gemini-4-carbon-high`)
- **Primary Source:** William Hu (Anthropic), [*Building always-on agents*](https://claude.com/resources/articles/building-always-on-agents) (Nov 13, 2026)
- **Companion Anthropic Specifications Audited:** [*The Components of Agent Memory*](https://www.anthropic.com/research/components-of-agent-memory), [*Claude Code Auto Mode*](https://www.anthropic.com/research/claude-code-auto-mode), [*Claude Code Hooks — Defer a Tool Call*](https://code.claude.com/docs/en/hooks#defer-a-tool-call-for-later), [*Context Compaction*](https://platform.claude.com/docs/en/build-with-claude/compaction), [*Secure Deployment & Credential Management*](https://code.claude.com/docs/en/agent-sdk/secure-deployment#credential-management), and [*Claude Managed Agents*](https://claude.com/platform/claude-managed-agents)
- **Related RFC:** [RFC-0018: Always-On Agent Runtime Hardening — Durable Event Inbox, Self-Scheduled Follow-Up Timers, Deferred Approval Resumption, Cross-Run Progress/Reported Memory & OS Credential Sandboxing](../design/0018-always-on-agent-runtime-hardening.md)
- **Numbering Note:** Research IDs `12`–`13` and RFC IDs `0016`–`0017` are reserved for the concurrent Phase 1 `proactive-gmail-watcher` and `sync-conversation-archive` branches.

---

## 1. Executive Summary

On November 13, 2026, Anthropic published [*Building always-on agents*](https://claude.com/resources/articles/building-always-on-agents), codifying the production runtime architecture behind stateful, proactive agents across Claude Platform, Claude Code, and Claude Managed Agents. Anthropic attributes the practical resurgence of always-on agents to two compounding shifts: **(1) materially stronger model judgment** in deciding *when* to act or check in with a user, and **(2) steep reductions in long-horizon inference cost** (e.g., `78%` lower cost per solved long-horizon coding benchmark task on Opus 5.5 vs. Opus 5, compared with `40%` lower cost on short turns).

Crucially, Anthropic emphasizes that an "always-on" agent is **not** a continuously running LLM loop. It is a surrounding systems architecture built from **five decoupled primitives**—an **Event Inbox**, **Timers** (`Follow-up` vs. `Schedule`), **Structured File Memory** (including `reported.md` and `progress/`), **Unattended Permissions** (`Credential Proxy` + `Auto Mode` classifiers + `Deferred Tool Calls`), and a **Paced Outbox**—that keeps the agent asleep at `0` tokens `99%+` of the time while preserving continuity across brief, event-triggered runs.

Benchmarking Anthropic's reference design (and its companion specifications) against `agy-hq` (`HEAD` including [RFC-0014](../design/0014-proactive-engine-and-living-context.md) and [RFC-0015](../design/0015-global-plugin-packaging.md), plus the two concurrent Phase 1 designs `proactive-gmail-watcher` and `sync-conversation-archive`) yields a clear two-part verdict:

1. **Where `agy-hq` Already Leads:** `agy-hq` surpasses Anthropic's cloud-hosted baseline in **local-first Vault privacy** ([ADR-0003](../decisions/0003-two-layer-oss-and-private-vault.md) + mathematical `P1`–`P6` PII verification in [`internal/ci/pii.go`](../../internal/ci/pii.go)), **0-token shell precheck gatekeepers** ([RFC-0004](../design/0004-heartbeat-and-channels.md) / [RFC-0014](../design/0014-proactive-engine-and-living-context.md)), **stat-cached prefix-cache & QBU anomaly telemetry** ([RFC-0012](../design/0012-token-observability.md)), **4-layer bi-temporal ground-truth transcript memory** ([RFC-0006](../design/0006-memory-engine-upgrade.md)), and **multi-repo Git worktree engineering orchestration**.
2. **Six Concrete Runtime Gaps in `agy-hq`:** Forensic code inspection reveals six structural failure modes where `agy-hq` diverges from Anthropic's reliability invariants:
   - **[P0] Gap 1 — Premature Ack & Competing Spawns (No Unified Durable Event Inbox):** File drops (`ProcessInbox`), cron gatekeepers (`EvaluateDaemonGatekeepers`), and Gmail/GChat batch coalescers (`RunHeartbeat`) mutate state cursors or move files *before* `SpawnTask` succeeds or finishes—permanently stranding files or dropping events on `CircuitOpen`, `agentapi` downtime, or mid-run crashes, while three independent daemon tickers can launch competing runs on the same scope.
   - **[P0] Gap 2 — Missing Agent-Set `Follow-Up` Timers:** `triggers.yaml` supports static user crons and `WaitingOn` tracks counterparty email SLAs, but an agent cannot dynamically schedule a durable one-shot self-wakeup (`hq timers set --in=25m ...`) that survives session exit and daemon restarts.
   - **[P1] Gap 3 — One-Way Approval Dead-End & Unsanitized `sh -c` Execution:** `hq approve` and `hq reject` terminate at shell execution without routing approval outcomes or rejection steering (`--reason`) back to resume the originating session, and `ApproveProposal` executes raw `sh -c` with unfiltered `os.Environ()`.
   - **[P1] Gap 4 — Missing `reported` Anti-Repetition Ledger, Missing `progress/<slug>.md` Checkpoints & Heavy `SessionStart` Overhead:** Disjointed proactive runs lack a compact ledger of recently delivered alerts (relying only on exact lowercase `Title` match within `4h`), lack structured per-task `progress/` handoff files across compactions/runs, and pay a ~3.5k–5k token full-Vault `SessionStart` tax even inside narrow `[Auto]` workers.
   - **[P2] Gap 5 — Hook-Only Quarantine vs. OS Credential Sandboxing & Gray-Zone Action Classifier:** Regex checks in `PreToolUse` inspect top-level command strings, leaving child subprocesses (`go test`, `python3`, `make`) running with ambient host filesystem access to `~/.ssh` and `local/private/`, while gray-zone mutating CLI verbs (`curl -X POST`, `gh pr create`, `git push --force`) bypass static outbound regexes in unattended `[Auto]` runs.
   - **[P2] Gap 6 — Silent Precheck Error Masking & Missing Offline Signal-to-Noise Evals:** Broken `precheck_cmd` executions (`cmdErr != nil` on expired auth or `15s` timeout) are miscounted as healthy `zeroTokenSaves++` (`GatekeeperSavesToday`), and `agy-hq` lacks an offline fixture benchmark (`hq outbox eval`) to verify proactive routing precision and noise suppression.

---

## 2. Anatomy of Anthropic's Always-On Agent Reference Architecture

### 2.1 The Sleep-Wake Run Cycle ("Always-On" as a UX Illusion)

Anthropic defines the fundamental execution unit of an always-on agent through three nested concepts:
- **Run:** A brief, bounded stretch of execution triggered by one or more events (`Asleep` → `Wake on Event` → `Act within Permissions` → `Persist Memory / Timers / Outbox` → `Asleep`).
- **Session:** The durable transcript of messages and tool calls across one or more runs. Events that continue prior work (a user reply, a deferred approval answer, or a task follow-up timer) **resume an existing session**; independent background triggers **start a fresh session**.
- ** Surrounding Runtime:** The deterministic non-LLM control plane that governs *when* runs wake, *how* concurrent triggers serialize, *what* context loads into the prompt prefix, and *how* outcomes reach the human.

### 2.2 Primitive 1 — Durable Event Inbox (`Handler` → `Inbox` → `Single Active Worker`)

Anthropic explicitly warns against having webhooks, file watchers, or cron schedulers invoke the agent loop directly:
> *"One tempting approach is to have each event source call the agent directly, but that has a few failure modes. For example, two events arriving at the same time can start competing runs, and a crash can lose an event entirely."*

Instead, every trigger source writes into a unified **Event Inbox** backed by four strict operational rules:
1. **Fast Ingestion (`Handler`):** Webhook/IPC endpoints perform only lightweight structural validation, deduplicate via source event IDs, append a `pending` row (`When | What happened | Session | Status`), wake the worker, and return immediately. Deeper semantic or prompt-injection inspection happens inside the run, never blocking ingestion.
2. **Inbox as Crash-Recovery Source of Truth (`Lease` → `Done`):** When woken, the worker reads `pending` rows from durable storage, runs the agent loop, waits for completion, and **only marks rows `done` after the run finishes cleanly** (or marks stale/superseded rows `dropped`). If the worker crashes mid-run, unacknowledged rows remain recoverable for retry.
3. **Single Active Worker Serialization:** At most one active worker processes a given inbox (or partition/scope) at a time, eliminating competing runs that duplicate side effects or clobber shared state. Events arriving mid-run queue cleanly for the next batch (or fold into the active session).
4. **Same-Session Event Batching:** Multiple pending events targeting the same session (e.g., three rapid user messages or five coalesced thread updates) merge into a single run turn so the model reasons and responds once.

### 2.3 Primitive 2 — Two-Class Durable Timers (`Follow-up` vs. `Schedule`)

Anthropic separates future time-based triggers into their own durable **Timers** store so the Event Inbox contains *only* immediately actionable work:
- **Follow-ups (Agent-Scheduled One-Shot Deadlines):** Created dynamically *by the agent during a run* (e.g., *"Thu 18:00: Check whether the vendor replied or whether CI finished deploying"*). Marked `done` upon firing.
- **Schedules (User-Configured Recurring Cadences):** Standing periodic jobs (e.g., *"Daily 08:00: Send morning briefing"*). Updated to their next `dueAt` occurrence upon firing.

Crucially, **the timer scheduler never spawns an agent directly**: on a fixed poll interval, it transitions due timers (`done` or next `dueAt`) and **enqueues a standard event into the Event Inbox**, inheriting single-worker serialization, deduplication, and crash retry automatically.

### 2.4 Primitive 3 — Four-Part File Memory, Prompt-Cached Prefix & Compaction

Across [*Building always-on agents*](https://claude.com/resources/articles/building-always-on-agents), [*The Components of Agent Memory*](https://www.anthropic.com/research/components-of-agent-memory), and [*Compaction*](https://platform.claude.com/docs/en/build-with-claude/compaction), Anthropic establishes four memory design rules:

1. **The Pointer Rule:** *"Memory should only hold what the agent can't look up again. If something already lives in another system, such as an issue tracker, you should save a pointer instead."*
2. **Strict Split Between "Loaded Every Run" vs. "Read When Needed":**
   - **Loaded Every Run (Lean, Static-First Prefix for Prompt Caching):**
     - `instructions.md` — Persistent user behavioral rules (critical when running unattended without live user steering).
     - `index.md` — Compact directory index pointing to `notes/` and `progress/`.
     - `reported.md` — **"What it's already told the user"** (or the worker's last few outbound messages) so disjointed background runs never repeat the same alert or insight.
   - **Read On Demand (Progressive Disclosure):**
     - `notes/` — Focused topic and user-preference files pulled only when relevant.
     - `progress/` — **One structured file per ongoing task** covering *what has been completed*, *verified state*, and *what remains*, allowing any fresh run or post-compaction turn to pick up seamlessly where the prior run stopped.
3. **Pre-Compaction Memory Flush:** When a long-lived session approaches context limits and triggers server/client compaction (summarizing older turns while keeping the tail), turn-level detail is lossy; agents must flush durable decisions and task progress to `notes/` and `progress/` *along the way* before compaction truncates raw tool outputs.
4. **Offline Dreaming:** Periodic background review of raw session transcripts curates memories, prunes contradicted notes, and surfaces cross-run behavioral patterns.

### 2.5 Primitive 4 — Unattended Permissions (`Credential Proxy` + `Auto Mode` + `Deferred Tool Calls`)

Because unattended runs hold the user's authority without live supervision, Anthropic combines three mechanisms:
1. **Out-of-Sandbox Credential Proxy & Brain/Hands Separation ([*Secure Deployment*](https://code.claude.com/docs/en/agent-sdk/secure-deployment#credential-management) & [*Claude Managed Agents*](https://claude.com/platform/claude-managed-agents)):** Raw OAuth tokens, cookies, and API keys never enter the agent's filesystem or environment variables. In Managed Agents, the **Session** (append-only event log), **Harness** ("brain"), and **Sandbox** ("hands") are decoupled so tool commands run inside an isolated container/jail while an external host proxy injects credentials onto allowlisted outbound requests.
2. **Auto Mode Two-Layer Risk Classifiers ([*Claude Code Auto Mode*](https://www.anthropic.com/research/claude-code-auto-mode)):** Because users approve `~93%` of routine permission prompts reflexively, Anthropic pairs an **input-layer prompt-injection probe** on untrusted tool outputs with an **action-layer risk classifier** that blocks destructive, exfiltrative, or scope-escalating actions in unattended runs while letting routine read/edit/test steps proceed autonomously.
3. **Deferred Tool Calls (`permissionDecision: "defer"` in `PreToolUse`):** When an unattended run hits a decision requiring human approval or clarification, the `PreToolUse` hook returns `defer`. Instead of blocking a process or dropping context, the runtime **saves the session mid-call and ends the run immediately** (`0` idle compute; the worker is free to process other inbox events). An interactive approval/question card surfaces to the user, and when the user responds, **their decision enters the Event Inbox as a resumption event** targeting that saved session.

### 2.6 Primitive 5 — Three-Terminal Outbox, Dispatcher & Signal-to-Noise Evals

Every background run terminates in exactly one of three explicit states:
1. **Nothing to say** (silent completion — the modal outcome of healthy background checks),
2. **Send** (append an outcome message to the durable **Outbox** table), or
3. **Ask the user** (emit a deferred approval/clarification request).

A separate background **Dispatcher** reads `pending` Outbox rows, enforces pacing/quiet-hours/channel retries, delivers to the user, and marks rows `sent`. Finally, Anthropic recommends **offline signal-to-noise evals** over realistic event streams—advising builders to *"start quiet and earn more of the user's attention over time."*

---

## 3. Seven-Dimension Benchmark Matrix (`Anthropic` vs. `agy-hq`)

The table below benchmarks Anthropic's reference architecture against `agy-hq` on `HEAD` ([RFC-0014](../design/0014-proactive-engine-and-living-context.md) + [RFC-0015](../design/0015-global-plugin-packaging.md)) while accounting for the two concurrent Phase 1 designs (`proactive-gmail-watcher` and `sync-conversation-archive`):

| Dimension | Anthropic Always-On Reference | `agy-hq` Current State (`HEAD` + Concurrent Sibling RFCs) | Gap Assessment |
| :--- | :--- | :--- | :--- |
| **1. Heartbeat & Event Loops** | Unified **Event Inbox** (`Handler` fast-acks; `Scheduler` & watchers enqueue rows; **1 active worker** per scope batches pending rows and marks `done` *after* run completion) + **2-class Timers** (`Follow-up` + `Schedule`). | `hq daemon` ([`cmd/hq/main.go`](../../cmd/hq/main.go) `L1407-1431`) runs 3 independent tickers (`15s` `ProcessInbox`, `30s` `EvaluateDaemonGatekeepers`, `30m` `RunHeartbeat`) that call `SpawnTask` directly with **no unified event queue**, **no per-scope active-worker lock**, **no loopback HTTP/CLI event ingress**, and **no agent-set `Follow-up` timers**. | **High Gap (`P0`):** Direct-spawn races + missing agent `Follow-up` timer store. |
| **2. State Persistence & Cursors** | Durable append-only Session log + Event Inbox (`pending` → `leased` → `done`/`dropped`); source cursors advance only upon durable inbox commit or run completion. | `tasks.json` + `tasks_archive.jsonl` ([RFC-0009](../design/0009-task-board-redesign.md)); `trigger_cursors.json`, `triage_coalesce.json`, and `inbox/queued/` mutate **before** `SpawnTask` succeeds. Sibling `proactive-gmail-watcher` adds `StagedLeases` *only* for Gmail `AccountState`, and `sync-conversation-archive` reconciles Antigravity `.pb` archives. | **High Gap (`P0`):** Kernel-wide at-least-once event lease (`pending` → `leased` → `done`) needed across all sources. |
| **3. Memory, Anti-Repetition & Compaction** | `instructions.md` + `index.md` + **`reported.md`** loaded every run; `notes/` + **`progress/<task>.md`** read on demand; pointer-only rule; pre-compaction flush + offline dreaming. | 4-layer memory ([RFC-0006](../design/0006-memory-engine-upgrade.md)): `MEMORY.md` (`<=80` lines), `topics/*.md`, `entities/*.md`, `facts.jsonl`, ground-truth `VerifyTranscript` dreaming, surgical `hq memory forget`. **Missing** `reported.md` ledger, **missing** `progress/<slug>.md` checkpoints, and **missing** pre-compaction flush nudges. | **Medium-High Gap (`P1`):** Add `reported` ring buffer, `hq progress`, and pre-compaction hooks. |
| **4. Error Recovery & Observability** | Crash-recoverable `pending` inbox rows; Outbox dispatcher delivery retries; offline **signal-to-noise evals** calibrating notification volume. | Quota circuit breaker (`MaxAutoSpawnsPerDay`, `MaxDailyQBU`), `hq obs` `A1`–`A5` anomalies ([RFC-0012](../design/0012-token-observability.md)). **Bug:** `gatekeeper.go` `L238-242` counts failed `precheck_cmd` runs (`cmdErr != nil`) as `zeroTokenSaves++`; daemon loop ignores errors (`main.go` `L1419-1428`); no offline `outbox eval` harness. | **Medium Gap (`P0` bug + `P2` evals):** Separate precheck errors/backoff from 0-token saves + add `hq outbox eval`. |
| **5. Human-in-the-Loop Interruptions** | `PreToolUse` `defer` pauses & saves session at `0` idle compute; interactive card surfaces via Outbox/UI; user answer enters Event Inbox to **resume saved session**. | `hq propose` stages SHA-256 JSON (`24h` TTL) + 1-tap `ask_question`. Sibling `proactive-gmail-watcher` adds `DeliverPendingInterrupts` push to Front Desk, **but** `hq approve` / `hq reject` (`approvals.go` `L301-419`) are terminal—neither resumes `prop.ConversationID` with approval output or rejection `--reason`. | **High Gap (`P1`):** Close the deferred loop (`--resume-on-decide` + rejection steering event into Event Inbox). |
| **6. Cost & Token Efficiency** | `0` tokens while asleep; lean static-first initial context overhead (`instructions` + `index` + `reported`); prompt caching (`CHR`); progressive disclosure. | 0-token `precheck_cmd` Gatekeepers (`RFC-0014` eliminated `~24%` Front Desk cold-cache cron tax) + `hq obs` cache-hit (`CHR`) tracking. **However**, `HandleSessionStart` (`hooks.go` `L193-472`) still injects the full ~3.5k–5k token Vault context (25 facts, worktrees, tasks, triggers) into narrow ephemeral `[Auto]` workers. | **Medium Gap (`P1`):** Add role-scoped lean `SessionStart` profile + static-first prefix ordering. |
| **7. Sandboxing & Permissions** | Out-of-sandbox **Credential Proxy** + container/OS sandbox ("hands" vs. "brain") + **Auto Mode** 2-layer classifiers (prompt-injection + risky-action guard). | `PreToolUse` taint flag (`tainted_<convID>.json`), universal `local/private/` regex read-guard (`hooks.go` `L581-655`), contextual-PII badges. **However**, child subprocesses inherit full host UID filesystem/env access, `ApproveProposal` runs raw `sh -c` with unfiltered `os.Environ()`, and unattended `[Auto]` runs lack gray-zone command classification. | **Medium Gap (`P1`/`P2`):** Structured argv + env scrubbing in `ApproveProposal`, gray-zone `[Auto]` gate, and optional OS `bwrap`/`sandbox-exec` jail. |

---

## 4. Five Areas Where `agy-hq` Already Surpasses Anthropic's Baseline

Before addressing gaps, it is critical not to regress the architectural strengths where `agy-hq` already exceeds Anthropic's reference guide:

1. **Zero-Cloud Local Vault Sovereignty & Mathematical PII Verification ([ADR-0003](../decisions/0003-two-layer-oss-and-private-vault.md), [RFC-0011](../design/0011-ci-pipeline-and-regression-harness.md)):** Anthropic's reference assumes cloud relational tables and hosted session stores. `agy-hq` keeps 100% of personal state on local disk (`0700`/`0600`) inside a separate private Git Vault (`local/`) guarded by six deterministic PII/secret/history invariants (`P1`–`P6` in [`internal/ci/pii.go`](../../internal/ci/pii.go)) and zero external Go dependencies (`go.mod` stdlib-only).
2. **Deterministic 0-Token Shell Gatekeepers (`precheck_cmd` + SHA-256 Delta Cursors):** Anthropic's timer scheduler wakes an LLM worker whenever a recurring schedule fires. `agy-hq`'s `triggers.yaml` v2 ([RFC-0014](../design/0014-proactive-engine-and-living-context.md)) executes lightweight deterministic Go/CLI prechecks (`15s` timeout, `stdout_sha256` / `exit_zero_nonempty`) first, waking an LLM worker **only when external state actually changed**.
3. **4-Layer Bi-Temporal Memory & Ground-Truth Transcript Verification ([RFC-0006](../design/0006-memory-engine-upgrade.md)):** Where Anthropic sketches flat Markdown files plus conceptual dreaming, `agy-hq` already enforces an `80`-line `MEMORY.md` index cap, progressive-disclosure `topics/*.md` and `entities/*.md` dossiers, bi-temporal `facts.jsonl` (`valid_at` / `invalid_at` / `expired_at`), deterministic `VerifyTranscript` dreaming grounding checks, and 4-layer `hq memory forget --block-pii` cascades.
4. **Cryptographic SHA-256 Outbound Payload Binding (`hq propose`):** Anthropic's `PreToolUse` `defer` pauses tool execution on tool-call JSON arguments, whereas `agy-hq` binds `sha256(action_type + ":" + canonical_command)` with mandatory blockquote payload previews, `P6` contextual-PII warning badges, and `24h` expiration TTLs.
5. **First-Class Multi-Repo Git Worktree Orchestration & Prefix-Cache Telemetry ([RFC-0012](../design/0012-token-observability.md)):** `agy-hq` natively manages isolated `<vault>/worktrees/<repo>-<task>` checkouts alongside stat-cached parent-child subagent token attribution, prefix cache-hit rates (`CHR`), and `A1`–`A5` anomaly detection.

---

## 5. Forensic Code Audit: Six Concrete Gaps in `agy-hq`

### Gap 1 (`P0`): Premature Cursor Ack, Stranded File Drops & Competing Daemon Spawns (Missing Unified Event Inbox)

Tracing all background trigger paths in `agy-hq` confirms the exact failure modes Anthropic warns against when event sources call `SpawnTask` directly without a durable Event Inbox:

1. **Stranded File Drops in `ProcessInbox` ([`internal/kernel/kernel.go`](../../internal/kernel/kernel.go) `L2180-2201`):**
   - On `L2189`, `os.Rename(srcPath, dstPath)` moves `local/inbox/<file>` into `local/inbox/queued/<file>` **before** calling `SpawnFlashTask` (`L2195`).
   - If `SpawnFlashTask` returns an error (e.g., `CircuitOpen` daily spawn cap reached, `agentapi` port temporarily unreachable, or Antigravity restarting), `ProcessInbox` logs `"queued; spawn deferred"` (`L2197`) and returns `nil`.
   - On every subsequent `15s` tick, `os.ReadDir(inboxDir)` skips subdirectories (`if e.IsDir() { continue }` at `L2181`). **The dropped file remains stranded inside `local/inbox/queued/` forever and is never retried.**
2. **Premature Gatekeeper Cursor Mutation in `EvaluateDaemonGatekeepers` ([`internal/kernel/gatekeeper.go`](../../internal/kernel/gatekeeper.go) `L188-278`):**
   - `cur.LastCronSlot = slotKey` (`L188`), `cur.LastStdoutSHA256 = shaHex` (`L234`), and `cur.LastTriggeredAt` (`L255`) are mutated and persisted via `SaveTriggerCursors` (`L277`) **regardless of whether `SpawnTask` (`L267`) succeeds or fails**, and long before the spawned worker finishes. Any transient spawn failure or mid-run worker crash permanently drops that cron slot or state-hash delta.
3. **Destructive Batch Coalescer Drain in `RunHeartbeat` ([`internal/kernel/kernel.go`](../../internal/kernel/kernel.go) `L2222, 2238-2244` & [`internal/kernel/gatekeeper.go`](../../internal/kernel/gatekeeper.go) `L379`):**
   - `EnqueueOrFlushCoalescedTriage` removes flushed Gmail/GChat batches from `local/memory/triage_coalesce.json` (`remaining` slice at `gatekeeper.go:349-380`) *prior* to `SpawnFlashTask`. If `SpawnFlashTask` fails (`L2239`), the coalesced batch is lost from `triage_coalesce.json` *(note: concurrent branch `proactive-gmail-watcher` introduces `StagedLeases` for Gmail `AccountState`, but GChat, gatekeepers, file drops, Waiting-On nudges, and external webhooks still lack a unified kernel-level event queue)*.
4. **Competing Concurrent Spawns Across Three Uncoordinated Tickers ([`cmd/hq/main.go`](../../cmd/hq/main.go) `L1410-1431`):**
   - `hq daemon` runs `15s`, `30s`, and `30m` tickers inside one goroutine, while users or external hooks can invoke `hq heartbeat` concurrently in separate CLI processes without a file lock or per-scope active-worker lease—allowing duplicate `[Auto]` workers on the same scope (`inbox-triage`, `swe-pulse`, or a resumed task session) to run simultaneously.
5. **Zero External / Webhook Event Ingress:** `agy-hq` is 100% poll-bound; local git post-commit scripts, CI completion hooks, or local calendar/mail helpers have no `hq events enqueue` CLI or loopback `POST /api/events` endpoint to push structured events into the Vault.

### Gap 2 (`P0`): No Agent-Scheduled Durable `Follow-Up` Timers (`hq timers`)

Comparing `agy-hq`'s scheduling primitives against Anthropic's two-class `Timers` table (`Follow-up` vs. `Schedule`):
- [`internal/kernel/daemon.go`](../../internal/kernel/daemon.go) (`L20-29`) parses static recurring cron schedules from `local/triggers.yaml` (`Schedule` class only).
- [`internal/kernel/goals.go`](../../internal/kernel/goals.go) (`L24-35`) models `WaitingOn` counterparty reply SLAs (`hq goals wait-on`), which only emit passive `brief_rollup`/`interrupt` outbox cards when a human contact has not replied by `DueBy`.
- Antigravity's built-in `schedule(DurationSeconds=...)` tool is bound to an open, in-memory conversation lifetime and vanishes if Antigravity restarts or if an ephemeral `[Auto]` worker exits cleanly.

Consequently, an `agy-hq` worker **cannot** finish a 30-second run, go to sleep at `0` tokens, and leave behind a durable self-scheduled follow-up such as *"Wake me in 20 minutes in session `<conv-id>` (or fresh `swe-pulse` scope) to verify the staging deploy healthcheck passed"* or *"Re-check flight seat availability at 18:00 UTC today."*

### Gap 3 (`P1`): One-Way Approval Dead-End (No Deferred Session Resumption) & Raw `sh -c` Execution

Auditing [`internal/approvals/approvals.go`](../../internal/approvals/approvals.go) (`L301-419`) and [`internal/outbox/outbox.go`](../../internal/outbox/outbox.go) (`L383-430`) surfaces three gaps relative to Anthropic's `Deferred Tool Calls` + `Outbox Dispatcher`:

1. **Terminal `ApproveProposal` / `RejectProposal` Lifecycle (`approvals.go:370-419`):**
   - `Proposal` already records `ConversationID` (`approvals.go:38`), yet neither `ApproveProposal` nor `RejectProposal` notifies or resumes that originating conversation when the decision happens via `:4242` WebUI, `hq approve`, or `[HQ] Front Desk`.
   - Worse, when a user rejects a draft with steering feedback (`hq reject prop-123 --reason="Change Tuesday to Thursday at 2pm"`), `reason` is written only to `interventions.jsonl` (`L415`)—no event wakes the drafting session to revise and re-propose! Similarly, multi-step workflows that need post-approval continuation (e.g., after sending a vendor email, automatically registering a `48h` `hq goals wait-on` SLA and updating `progress/<slug>.md`) terminate prematurely at shell execution.
2. **Outbox Dispatcher Unification (`outbox.go:383-430`):**
   - On `HEAD`, `outbox.Add` writes JSON files to `local/memory/outbox/` with no background `Dispatcher` delivering `TierInterrupt` items to `[HQ] Front Desk` or `ChannelAdapter`. *(Concurrent branch `proactive-gmail-watcher` adds `DeliverPendingInterrupts`; `RFC-0018` generalizes this into the unified Outbox Dispatcher with retry backoff and `reported` logging.)*
3. **Raw `sh -c` & Unfiltered `os.Environ()` in `ApproveProposal` (`approvals.go:350-369`):**
   - Line `350` runs `exec.Command("sh", "-c", prop.Command)` and line `354` passes the entire host `os.Environ()` (including `ANTIGRAVITY_CSRF_TOKEN` or ambient developer tokens in the shell) into the executed command, rather than supporting structured `CommandArgv []string` execution under a scrubbed allowlist environment.

### Gap 4 (`P1`): Missing `reported` Anti-Repetition Ledger, Missing `progress/<slug>.md` Checkpoints & Heavy `SessionStart` Prefix

Auditing [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go) (`L193-472`, `L1515-1560`), [`internal/outbox/outbox.go`](../../internal/outbox/outbox.go) (`L324-352`), and [`internal/obs/anomaly.go`](../../internal/obs/anomaly.go) (`L145-170`) highlights three context-continuity gaps:

1. **No `reported` Ledger Across Disjointed Proactive Runs:**
   - `outbox.topicDedupKey` (`outbox.go:324-328`) deduplicates strictly on `sha256(tier|source|skill|lowercase(title))` inside a `4h` window. If two consecutive runs (`09:00` and `14:00`, or slightly reworded LLM titles like `"PR #42 CI Failing"` vs. `"CI Failure on PR #42"`) evaluate the same condition, `outbox` passes it through **and** the worker has zero visibility into what `agy-hq` already told the user in the last `24h`–`72h`. Injecting a compact, bounded `reported` summary (`<= 15` recent delivered items) eliminates semantic re-notification at the reasoning source.
2. **No Structured `progress/<slug>.md` Task Checkpoints or Pre-Compaction Flush:**
   - `ActiveTask.LastSummary` (`hooks.go:1528`) stores only a single `500`-character tail string overwritten on `Stop`, while `obs/anomaly.go` (`L145-170`) passively flags `A1 Context Bloat` (`>150k` peak prompt tokens) *after* the fact. Long-horizon tasks spanning multiple sessions, `hq reopen` cold recalls, or Antigravity `/compact` boundaries lack Anthropic's `local/memory/progress/<slug>.md` structured checkpoint (`Goal`, `Completed`, `Verified`, `Next Steps`, `Open Blockers`) and proactive pre-compaction flush guidance (`>120k` tokens).
3. **Unscoped `SessionStart` Overhead in Ephemeral `[Auto]` Workers (`hooks.go:193-472`):**
   - `HandleSessionStart` injects `USER.md`, `MEMORY.md`, Wiki topics index, Entity dossiers index, `25` active facts, active goals, `triggers.yaml`, git worktrees, pending proposals, and the hot task board into **every** session—even narrow `hydrogen-low` `[Auto]` triage or gatekeeper workers that only need `USER.md` + compact `MEMORY.md` + `reported` + scoped topic/entity pointers. Worse, placing volatile timestamps/counters ahead of stable sections degrades prefix cache hit rates (`CHR`).

### Gap 5 (`P2`): Hook-Only Quarantine vs. OS Credential Sandboxing & Unattended Gray-Zone Action Gate

Auditing [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go) (`L478-655`):
1. **Ambient Subprocess Filesystem/Network Exposure:** `PreToolUse` blocks direct `read_file` or top-level shell tokens matching `local/private/{gmail,gchat,credentials}` (`L581-655`). However, once `run_command` launches an allowed binary or build script (e.g., `go test ./...`, `npm test`, `python3 analyze.py`, or a skill helper) inside an `[Auto]` or tainted session, that child process executes under the user's ambient POSIX UID with unrestricted read access to `~/.ssh`, `~/.config`, and `local/private/`. Wrapping unattended/quarantined shell commands in an optional zero-dependency OS namespace/seatbelt profile (`bwrap` on Linux, `sandbox-exec` on macOS) brings Claude Managed Agents' *"brain vs. sandboxed hands"* isolation to local workstations.
2. **Narrow Static Outbound Regexes (`hooks.go:478-489`):** `outboundCommandPatterns` checks 10 specific CLI patterns (`hq gmail send`, `gh pr merge`, `git push --tags`, `npm publish`, etc.). In an **interactive** coding session, narrow patterns avoid friction; but in an **unattended `[Auto]` background session** (`kind == "auto"`), gray-zone state-mutating commands—such as `curl -X POST/PUT/PATCH/DELETE` to external hosts, `gh pr create|close|comment`, `gh issue create|close`, `git push --force`, or `cargo/twine publish`—are not intercepted for `hq propose` staging.

### Gap 6 (`P2`): Silent Precheck Failure Masking & Missing Offline Signal-to-Noise Evals

1. **Concrete Telemetry & Reliability Bug (`internal/kernel/gatekeeper.go:218-242`):**
   ```go
   outBytes, cmdErr := cmd.CombinedOutput()
   ...
   if cmdErr == nil && trimmedOut != "" && shaHex != cur.LastStdoutSHA256 {
       shouldSpawn = true
   }
   if !shouldSpawn {
       cur.SkipsCount++
       cursors[s.ID] = cur
       zeroTokenSaves++ // <-- BUG: increments even when cmdErr != nil!
       continue
   }
   ```
   When a watcher's `precheck_cmd` fails (`cmdErr != nil`—for example, expired OAuth token, missing binary, syntax error, or `15s` context deadline exceeded), `!shouldSpawn` is true, so `EvaluateDaemonGatekeepers` **increments `zeroTokenSaves` (`GatekeeperSavesToday`) and silently discards `cmdErr`** (compounded by `cmd/hq/main.go:1419-1428` ignoring return errors with `_`). Users see inflating "0-token gatekeeper saves" on the Console while their watcher is completely dead!
2. **Incomplete Rejection Feedback & Missing Offline Routing Evals:** `ComputeSkillDismissalStats` ([`internal/outbox/outbox.go`](../../internal/outbox/outbox.go) `L580`) tracks `ideas_feed` dismissals (`dismissed / total >= 60%`) for [RFC-0010](../design/0010-agent-recursive-self-improvement.md) RSI trace mining, but ignores repeated `hq reject` interventions on noisy `hq propose` drafts (`interventions.jsonl`) and provides no deterministic `hq outbox eval` benchmark suite to regression-test proactive signal-to-noise routing before changing `AttentionConfig` or `triggers.yaml`.

---

## 6. Prioritized Improvement Roadmap (`RFC-0018`)

We translate these six findings into a staged, pure-Go stdlib architecture in [RFC-0018](../design/0018-always-on-agent-runtime-hardening.md) while explicitly **rejecting** external database dependencies (PostgreSQL/Redis) and **rejecting** unapproved autonomous external messaging:

| Priority & Phase | Target Gap | Core `RFC-0018` Deliverable | Key Verification Gate |
| :--- | :--- | :--- | :--- |
| **`P0` — Phase 2A** | **Gap 1 & Gap 6a** | **Unified Durable Event Inbox (`internal/events`) & Single-Worker Scope Leases:** `local/memory/events.jsonl` (`pending` → `leased` → `done`/`dropped`), atomic cursor commit *after* durable enqueue, retry of `local/inbox/queued/` orphans, loopback `POST /api/events` + `hq events` CLI, and `ConsecutiveErrors` / `LastError` health tracking on broken `precheck_cmd` watchers. | `CUJ-25`: Crash/CircuitOpen recovery (`0` stranded drops, `0` competing spawns, broken precheck surfaced in Console). |
| **`P0` — Phase 2B** | **Gap 2** | **Two-Class Durable Timers (`hq timers`):** Agent-set `Follow-up` one-shot timers (`hq timers set --in=25m|--at=... --scope=... [--session=<id>] --prompt=...`) alongside recurring `triggers.yaml` schedules in `local/memory/timers.json`, firing exclusively into the Event Inbox. | `CUJ-26`: Ephemeral run sets `Follow-up` timer → exits (`0` idle tokens) → daemon fires into Event Inbox → resumes target session. |
| **`P1` — Phase 2C** | **Gap 3 & Gap 4** | **Deferred Approval Resumption, Structured Argv Execution & `reported`/`progress` Memory:** `--resume-on-decide` + `hq reject --reason` resumption events into Event Inbox; `CommandArgv` + scrubbed env in `ApproveProposal`; `local/memory/reported.jsonl` + `local/memory/progress/<slug>.md` (`hq progress`) + lean static-first `[Auto]` `SessionStart` profile + pre-compaction flush nudge. | `CUJ-27`: Reject draft with `--reason` → session auto-resumes & re-drafts; `[Auto]` runs suppress semantic duplicates via `reported` & cut `SessionStart` tokens by `>=50%`. |
| **`P2` — Phase 2D** | **Gap 5 & Gap 6b** | **OS Credential Sandbox (`bwrap`/`sandbox-exec`), Unattended Action Classifier & Offline Outbox Evals:** Optional zero-dep OS sandbox wrapper hiding `local/private/` and `~/.ssh` from `[Auto]`/tainted subprocesses, unattended gray-zone outbound classifier, proposal-rejection RSI mining, and `hq outbox eval` fixture benchmark. | `CUJ-28`: Sandboxed `[Auto]` child script blocked from reading `local/private/`; `hq outbox eval` enforces precision/noise thresholds in CI. |
