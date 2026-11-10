# Product Spec 0003: Critical User Journeys (CUJs) & E2E Acceptance Criteria

> **Status:** Active (`internal/e2e/cuj_e2e_test.go`)
> **Goal:** Define the crisp, deterministic expected behaviors across every core `agy-hq` user journey and enforce them via automated black-box End-to-End (E2E) tests against the compiled `bin/hq` binary.

---

## Overview of Supported CUJs

| CUJ ID | Journey Name | Primary Entry Points | E2E Test Function |
| :--- | :--- | :--- | :--- |
| **CUJ-1** | Vault Bootstrap, Idempotency & Health Check | `hq init`, `hq doctor`, `hq status` | `TestCUJ1_VaultBootstrapAndIdempotency` |
| **CUJ-2** | Front Desk Lease, Turn-1 Init, Handover & Demotion | `hq hook session-start`, `hq adopt`, `hq release-desk`, `hq claim-desk` | `TestCUJ2_FrontDeskLifecycleAndHandover` |
| **CUJ-3** | Dedicated Task Conversation Lifecycle (Spawn, Adopt, Stop Sync, Complete) | `hq spawn`, `hq adopt`, `hq hook stop`, `hq complete` | `TestCUJ3_TaskConversationLifecycle` |
| **CUJ-4** | Smart Model & Thinking-Depth Routing + 429 Quota Fallback | `hq spawn`, `kernel.ResolveModel`, `kernel.SpawnTask` | `TestCUJ4_SmartModelRoutingAndQuotaFallback` |
| **CUJ-5** | Approval-Bound Outbound Execution & Tamper Rejection | `hq hook pre-tool`, `hq propose`, `hq approve`, `hq reject` | `TestCUJ5_ApprovalBoundOutboundExecution` |
| **CUJ-6** | Parallel Multi-Repo Git Worktree Lifecycle | `hq worktree add`, `hq worktree list`, `hq worktree remove` | `TestCUJ6_ParallelWorktreeLifecycle` |
| **CUJ-7** | Quarantined Inbox Drop & 0-Token Heartbeat Gatekeeper | `hq heartbeat`, `local/inbox/` | `TestCUJ7_InboxRoutingAndHeartbeatGatekeeper` |
| **CUJ-8** | Zero-Token Per-Turn Journaling & Live Console Sync | `hq hook stop`, `local/CONSOLE.md`, `hq_console.md` | `TestCUJ8_TurnJournalingAndLiveConsoleSync` |
| **CUJ-9** | Engine Upgrade & Non-Destructive Vault Migration | `hq upgrade`, `hq migrate` | `TestCUJ9_EngineUpgradeAndVaultMigration` |
| **CUJ-10** | Front Desk Non-Blocking Guardrails | `hq hook pre-tool`, `internal/hooks` | `TestCUJ10_FrontDeskNonBlockingEnforcement` |
| **CUJ-11** | Subagent Lifecycle & Task Board Tracking | `hq hook pre-tool`, `invoke_subagent`, `manage_subagents` | `TestCUJ11_SubagentLifecycleAndTaskboardTracking` |
| **CUJ-12** | Automated Trigger Scheduling & Background Daemon | `hq daemon`, `hq heartbeat`, `local/triggers.yaml` | `TestCUJ12_AutomatedTriggerSchedulingAndDaemon` |
| **CUJ-13** | Front Desk Init Hardening & Trigger Arming | `hq init`, `hq hook session-start`, `hq hook pre-tool` | `TestCUJ13_FrontDeskInitHardeningAndTriggerArming` |
| **CUJ-14** | Decoupled Native Gmail Infrastructure & Workflows | `hq gmail`, `local/memory/gmail/` | `TestCUJ14_GmailLayer1AndLayer2Lifecycle` |
| **CUJ-15** | 4-Layer Memory Engine & Dreaming Verification | `hq memory`, `local/memory/topics/`, `local/memory/facts.jsonl` | `TestCUJ15_MemoryEngineUpgradeAndDreamingVerification` |
| **CUJ-16A** | Google Chat Integration, Quarantine Lockdown & Approval-Bound Send | `hq gchat`, `local/memory/gchat/` | `TestCUJ16_GoogleChatQuarantineAndApproval` |
| **CUJ-16B** | Scalable Task Board, Per-Tier Visibility & Cold Archive Recall | `hq tasks`, `local/memory/tasks.json` | `TestCUJ16_TaskBoardTieredConsoleAndColdArchive` |
| **CUJ-17** | Bounded Agent Recursive Self-Improvement (RSI) | `hq rsi`, `internal/rsi` | `TestCUJ17_AgentRSIMiningVerificationAndApproval` |
| **CUJ-18** | Automated CI/CD Pipeline, PII/Vault Guard & Harness/Go/Markdown Gate | `hq ci`, `internal/ci` | `TestCUJ18_CIPipelinePIIGuardAndHarnessContract` |
| **CUJ-19** | End-to-End Token & Context Observability, Subagent DAG Attribution & Budget Circuit Breaker | `hq obs`, `internal/obs`, `hq hook stop`, `hq doctor` | `TestCUJ19_TokenObservabilityAnomalyAndBudgetGuards` |

---

## Detailed Expected Behaviors

### CUJ-1: Vault Bootstrap, Idempotency & Health Check
1. **Initial Bootstrap (`hq init`):**
   - Creates `local/` with `USER.md`, `MEMORY.md`, `config.yaml`, `triggers.yaml`, `.gitignore`, `CONSOLE.md`, all required subdirectories (`inbox/`, `notes/`, `worktrees/`, `repos/`, `private/{ledger,taxes}/`, `memory/{pending,outbox,proposals,journal}/`), and an isolated `local/.git` repository.
2. **Idempotency Guarantee:**
   - Re-running `hq init` after a user customizes `local/USER.md` or `local/MEMORY.md` **must never overwrite** existing files.
3. **Health Diagnostics (`hq doctor`):**
   - Verifies Antigravity sandbox preset, Remote Control state, `agy` and `agentapi` binaries (including fallback paths when invoked inside minimal hook `PATH` environments), and `local/` Vault readiness.

### CUJ-2: Front Desk Lease, Turn-1 Initialization, Handover & Demotion
1. **Auto-Claim on Vacant Desk (`hq hook session-start`):**
   - When `local/memory/front_desk.json` does not exist, the first new conversation (`Conv-A`) that runs `SessionStart` atomically claims `front_desk.json`.
   - `SessionStart` injects the **`🚀 New [HQ] Front Desk Initialization Required`** directive instructing the agent to pin `<artifactDir>/hq_console.md` without redundant file/CLI reads.
   - `SyncMarkdownConsole` writes `<artifactDir>/hq_console.md` **and** `<artifactDir>/hq_console.md.metadata.json` (`userFacing: true`, `requestFeedback: false`).
2. **Single-Intake Protection Against Race Conditions:**
   - If a second conversation (`Conv-B`) starts while `Conv-A` holds the Front Desk lease, `Conv-B` **must not** overwrite `front_desk.json`; instead, it receives `⚠️ Front Desk Notice` identifying `Conv-A` as the active intake coordinator.
3. **Front Desk Turns Excluded from Task List (`hq hook stop`):**
   - When the active Front Desk (`Conv-A`) finishes a turn, `hq hook stop` writes a turn journal entry to `local/memory/journal/YYYY-MM-DD/` and refreshes `hq_console.md`, but **never** registers the Front Desk itself as a task in `local/memory/tasks.json`.
4. **Demoting Front Desk to a Normal Task Conversation (`hq adopt`):**
   - Running `hq adopt --conv=<Conv-A> --task=<slug> --title="..."` registers `Conv-A` in `local/memory/tasks.json` (auto-prefixing `[Task:<scope>]` if omitted), **vacates `local/memory/front_desk.json`**, and **removes `hq_console.md` + `hq_console.md.metadata.json` from `Conv-A`'s artifact directory**.
   - Even if `Conv-A` finishes its current turn (`hq hook stop`) while `front_desk.json` is vacant, `SyncMarkdownConsole` **must not** recreate `hq_console.md` inside `Conv-A`'s artifact directory.
5. **Fresh Front Desk Establishment:**
   - The next new conversation (`Conv-C`) automatically claims `front_desk.json` on Turn 1, receives `Conv-A` under `## Tracked Task Conversations`, and gets `hq_console.md` + `hq_console.md.metadata.json` in `Conv-C`'s artifact directory.

### CUJ-3: Dedicated Task Conversation Lifecycle
1. **Spawning a Task Conversation (`hq spawn`):**
   - Creates an isolated worktree `local/worktrees/<repo>-<task>/` (when `--repo` is provided), resolves the model tier, invokes `agentapi new-conversation` with `--title="[Task:<scope>] <slug> (<tier>/<model>)"`, and registers the task in `local/memory/tasks.json`.
2. **Pre-Existing Conversation Adoption (`hq adopt` & `hq hook stop`):**
   - Pre-existing conversations created before `agy-hq` setup can be adopted explicitly via `hq adopt` or automatically on their next turn via `hq hook stop`.
   - Subsequent `hq hook stop` runs on an already-tracked task conversation **must preserve** its curated `taskSlug`, `title`, `tier`, `worktreeDir`, and `status`, updating only `model`, `lastUpdatedAt`, and `lastSummary`.
3. **Completing a Task (`hq complete`):**
   - Running `hq complete <task-slug-or-conv-id> [--summary="..."]` marks the task `completed` in `local/memory/tasks.json` and updates the Task Board in `hq_console.md`.

### CUJ-4: Smart Model & Thinking-Depth Routing + 429 Quota Fallback
1. **Automatic Complexity Tiering:**
   - `quick` prompts (`status`, `triage`, `brief`, `summary`, `typo`) → `gemini-3.8-flash-low` (`flash_lite`).
   - `standard` prompts (feature coding, unit tests, bug fixes) → `gemini-3.8-flash-high` (`flash`).
   - `deep` prompts (`architecture`, `design`, `security`, `audit`, `race condition`, `root cause`, `thesis`) → `gemini-4-argon-high` (`pro`).
2. **User Override Priority:**
   - Explicit CLI flags (`--model=opus --depth=high`, `--model=sonnet --depth=medium`) and natural-language prompt hints (`"use opus"`, `"use sonnet"`, `"use flash"`) always override automatic tiering.
3. **429 Quota Fallback & Circuit Breaker:**
   - If a non-Flash model hits `429 RESOURCE_EXHAUSTED`, `SpawnTask` automatically retries once on `gemini-3.8-flash-medium`.
   - Daily background spawns are capped at `maxDailySpawns` (15), tripping the circuit breaker (`circuitOpen: true`) when exhausted.

### CUJ-5: Approval-Bound Outbound Execution & Tamper Rejection
1. **Hard-Deny in `PreToolUse` (`hq hook pre-tool`):**
   - Direct execution of `gws gmail send`, `gws chat send`, `gh pr merge`, `gh release create`, or `vercel --prod` via `run_command` is blocked with `{"decision":"deny"}`.
2. **Proposal Staging (`hq propose`):**
   - Writes `local/memory/proposals/<id>.json` with deterministic `SHA256(command + "\n" + workDir)` and updates `hq_console.md`.
3. **Cryptographic Verification & Execution (`hq approve` / `hq reject`):**
   - Approving with a mismatched `--sha256` prefix fails and leaves the proposal untouched.
   - Approving with a matching `--sha256` prefix executes the command deterministically, removes the pending proposal, appends an audit record to `local/memory/interventions.jsonl`, and updates `hq_console.md`.

### CUJ-6: Parallel Multi-Repo Git Worktree Lifecycle
1. **`hq worktree add <repo> <task>`:** Creates `local/worktrees/<repo>-<task>` (avoiding duplicate `<repo>-<repo>-` prefixes if `<task>` already begins with `<repo>-`) on branch `hq/<task>`.
2. **`hq worktree list`:** Lists active worktrees and syncs them into `hq_console.md`.
3. **`hq worktree remove <name>`:** Cleanly removes the worktree directory and prunes the git worktree reference.

### CUJ-7: Quarantined Inbox Drop & 0-Token Heartbeat Gatekeeper
1. **0-Token Idle Heartbeat (`hq heartbeat`):** Returns `HEARTBEAT_OK (0 LLM tokens)` when `local/inbox/` and `local/memory/pending/` are empty and today's brief exists.
2. **Quarantine Inbox Router:** Files dropped into `local/inbox/` are classified (`*.csv/*.ofx` → `ledger-ingest`, tax PDFs → `tax-prep`, SEC filings → `thesis-check`, other files → `inbox-triage`), moved atomically to `local/inbox/queued/`, and dispatched to background Flash tasks.

### CUJ-8: Zero-Token Per-Turn Journaling & Live Console Sync
1. **`hq hook stop`:** Parses the JSONL transcript in 0 LLM tokens, extracts `toolsUsed`, `filesModified`, and single-line sanitized `responseSummary`, writes `local/memory/journal/YYYY-MM-DD/<convId>-<execNum>.json`, and refreshes `local/CONSOLE.md` and the Front Desk's `hq_console.md`.

---

## CUJ-9: Multi-Machine Engine Upgrade & Non-Destructive Vault Migration
1. **Pre-Upgrade Vault Snapshot (`local/.git`):** Before touching anything, `./bin/hq upgrade` (or `hq migrate`) commits any uncommitted changes in `local/.git` as a restore point (`chore(vault): pre-upgrade snapshot`).
2. **Upstream Code Pull & Binary Rebuild:** Pulls the latest commits on the root `agy-hq` repository (`git pull --ff-only`), recompiles `./bin/hq`, and re-execs the newly compiled `./bin/hq migrate` subprocess so newly pulled migration steps take effect immediately.
3. **Auto-Rebuild on `git pull` (`SessionStart` Hook):** Even if the user runs `git pull` manually without running `./bin/hq upgrade`, the `SessionStart` hook in `.agents/hooks.json` detects when `.go` files in `cmd/` or `internal/` are newer than `./bin/hq`, automatically recompiles `./bin/hq`, and runs `./bin/hq migrate --quiet`.
4. **Non-Destructive Schema & Template Migration (`local/memory/schema_version.json`):**
   - Never overwrites user-customized `USER.md`, `MEMORY.md`, `config.yaml`, or `triggers.yaml`.
   - Creates any newly added directories or template files.
   - Additively merges new ignore rules from `templates/gitignore.tmpl` into `local/.gitignore`.
   - Additively appends any new top-level YAML sections from `templates/config.yaml` and `templates/triggers.yaml` into `local/config.yaml` and `local/triggers.yaml`.
   - Runs versioned data migrations (e.g., normalizing legacy `tasks.json` titles to `[Task:<scope>] <Title>` and cleaning up deprecated `hq_console_mobile.md`) and updates `local/memory/schema_version.json`.

---

## CUJ-10: Front Desk Non-Blocking Guardrails
1. **Mechanical Enforcement:** The Front Desk thread is mechanically guarded by `PreToolUse` hooks against inline source/worktree edits (`edit_file`/`write_file`) and heavy build/test suites (`go test`, `npm test`, etc.).
2. **Inline Investigation Cap:** Capped at 3 lookup/research tool calls per turn. Subagent invocations (`invoke_subagent`, `manage_subagents`) are exempt from this cap.

---

## CUJ-11: Native Subagent Invocation Lifecycle & Task Board Tracking
1. **Quota Increment:** When `invoke_subagent` is called, `kernel.RecordSubagentSpawn` increments `SpawnsToday` in `local/memory/quota_state.json` (and `quota-YYYY-MM-DD.json`), updating `hq_console.md` to reflect live background spawn counts.
2. **Task Registration (`SessionStart`):** Subagents launched via `invoke_subagent` are matched from `pending_subagents.json` and registered in `local/memory/tasks.json` with normalized `[Subagent:<scope>] <Role> [<tier>]` titles and `subagent-<scope>-<role>` slugs, marked `🟢 active`.
3. **Turn Completion (`Stop`):** When the subagent's turn completes, `HandleStop` marks the task `✅ completed` in `local/memory/tasks.json` and records the subagent's response summary.
4. **Lifecycle Cancellation (`manage_subagents`):** Subagents killed via `manage_subagents` (`Action: "kill"` or `"kill_all"`) are immediately transitioned to `🛑 cancelled` on the Task Board.

---

## CUJ-12: Automated Trigger Scheduling & Background Daemon
1. **Background Daemon Lifecycle (`hq daemon`):** `hq daemon install` registers a user-level service (`systemd` user service on Linux or `launchd` LaunchAgent on macOS) running `hq daemon run` in the background; `hq daemon status` checks service status; `hq daemon uninstall` unregisters and removes the service.
2. **Heartbeat Diagnostics & Alerting (`hq doctor`):** Warns if schedules are declared in `local/triggers.yaml` but the background daemon service is inactive.
3. **0-Token Periodic Execution:** The background daemon runs the 15-second `local/inbox/` router and 30-minute `RunHeartbeat` gatekeeper without waking the LLM unless actionable items require dispatch.

---

## CUJ-13: Front Desk Init Hardening & Trigger Arming
1. **Persistent Turn-1 Init Banner:** Even if `hq init` or `hq claim-desk` creates `front_desk.json` ahead of time, `SessionStart` persistently injects the Turn-1 initialization directive on every turn until `ConsolePinned` is true and all `triggers.yaml` schedules are armed.
2. **Console Pinning State Tracking:** Detects `write_file` targeting `hq_console.md` in `PreToolUse` and marks `consolePinned: true` in `front_desk.json`.
3. **Trigger Reconciliation:** Tracks `schedule` and `manage_task` calls in `PreToolUse` and reconciles against `local/triggers.yaml` so declared jobs (`morning-brief`, `edgar-close-watch`, `sleep-consolidate`) are never left dormant.
4. **Subagent Pointer Isolation:** Subagent and task conversation turns never overwrite the Front Desk's `front_desk_turn.json` lookup counter or `active_artifact_dir.txt`.

---

## CUJ-14: Decoupled Native Gmail Infrastructure & Workflows
1. **Multi-Account Registry (`hq gmail init` / `add-account` / `accounts`):** Stores account configurations and OAuth token references securely in `local/config.yaml` / `local/memory/gmail/`.
2. **Safe Inspection & Quarantined Reader:** `hq gmail search` lists messages in raw JSON without LLM exposure; `hq gmail read-quarantined` extracts headers and strips HTML/scripts into safe plain text.
3. **Staged Sending via Approval Proposals:** `hq gmail draft` creates or updates draft messages; sending live emails requires staging via `hq propose --type=gmail_send` with SHA-256 confirmation before `hq gmail send-draft` can execute.
4. **Batch Triage & Attachment Quarantine:** Supports pure-Go sidecar unread prechecks (`hq gmail precheck`), batch triage result recording (`hq gmail triage-result`), and quarantined attachment sandboxing with auto-routing (`hq gmail attachment-quarantine`).

---

## CUJ-15: 4-Layer Memory Engine & Dreaming Verification
1. **Four-Layer Hierarchy:** Separates memory into (1) Injected Context / Roadmap Index (`local/MEMORY.md`), (2) Knowledge Wiki / Topic Pages (`local/memory/topics/*.md`), (3) Bi-temporal Facts (`local/memory/facts.jsonl`), and (4) Immutable Raw Transcripts (`transcript.jsonl`).
2. **CLI Fact Management:** `hq memory add-fact` adds atomic facts with provenance; `hq memory prune-fact` soft-prunes or supersedes facts with timestamps; `hq memory topics` indexes topic wiki pages.
3. **Ground-Truth Dreaming Verification:** `hq memory verify-transcript` audits claims against session transcripts, preventing hallucinated milestones or false bug-fix memories from polluting long-term memory.

---

## CUJ-16A: Google Chat Integration, Quarantine Lockdown & Approval-Bound Send
1. **Hybrid Transport & Fuzzy Space Resolution (`hq gchat`):** Supports Workspace `oauth` and personal `@gmail.com` `cdp` + `gmail-bridge` transports with fuzzy display-name/email resolution across spaces and DMs.
2. **Quarantined Reader (`hq gchat read-quarantined`):** Strips HTML/scripts and taints the reading conversation (`conv-gchat-reader`), blocking outbound network tools and private Vault reads.
3. **1-Step Outbound Staging (`hq gchat propose`):** Stages outbound Chat messages as SHA-256-bound `gchat_send` proposals requiring 1-tap `ask_question` approval before `hq approve` dispatches the message.

---

## CUJ-16B: Scalable Task Board, Per-Tier Console Visibility, Hot/Cold Archive & Recall
1. **Per-Tier Console Visibility Budget (RFC-0009):** Active tasks are 100% visible on the Task Board in `hq_console.md`, while completed and dormant tasks are capped to prevent visual clutter and context bloat.
2. **Two-Tier Storage (`tasks.json` + `tasks_archive.jsonl`):** Active and recent tasks remain hot in `local/memory/tasks.json`. Completed tasks (>24h) and dormant tasks (>48h) with clean worktrees are compacted via `hq tasks compact` into append-only cold archive (`local/memory/tasks_archive.jsonl`).
3. **Seamless Cold Recall (`hq reopen`):** Running `hq reopen <slug-or-id>` transparently looks up tasks in `tasks_archive.jsonl`, recalls them back into `tasks.json`, and updates the Task Board.
4. **CLI Telemetry & Pinning:** `hq tasks` filters by status (`--active`, `--all`, `--archive`), kind, and scope; `hq tasks stats` outputs slot allocation and hot/cold counts; `hq tasks pin <slug>` and `unpin <slug>` ensure critical initiatives remain prominently visible.

---

## CUJ-17: Bounded Agent Recursive Self-Improvement (RSI)
1. **0-Token Friction Trace Mining (`hq rsi mine`):** Scans `local/memory/journal/` and `interventions.jsonl` to cluster recurring tool errors, hook denials, and user corrections.
2. **4-Gate Sandboxed Verification (`hq rsi run`):** Synthesizes bounded `<!-- rsi:rule -->` deltas and verifies them against Gate 1 (Ring 0 TCB AST & token budget), Gate 2 (counterfactual trace replay), Gate 3 (`go vet` + unit tests), and Gate 4 (CUJ E2E suite).
3. **1-Tap Approval & Canary Rollback (`hq approve` / `hq rsi rollback`):** Stages verified patches via `hq propose --type=rsi_patch`, injects the canonical scorecard into `ask_question`, applies on `hq approve`, and supports automatic canary rollback on post-merge friction regression.

---

## CUJ-18: Automated CI/CD Pipeline, PII/Vault Guard & Harness/Go/Markdown Gate
1. **Mathematical PII & Vault Boundary Scanner (`hq ci check-pii`):** Enforces all 5 invariants (P1 forbidden `local/` paths, P2 `.gitignore` & no gitlink submodule on `local/`, P3 workstation home path freedom, P4 secret/token/SSN regexes, P5 strict synthetic email allowlist) across both staged diffs (`--staged`) and the full tracked tree.
2. **Agent Harness, Go & Markdown Regression Contracts (`hq ci check-harness`):** Verifies Gate H1 (Skill catalog, YAML frontmatter, `<=150` lines, `<!-- rsi:rule -->` schema), Gate H2-A/H2-B (`.agents/hooks.json` == `.agents/plugins/agy-hq/hooks.json` byte parity + 18-probe `PreToolUse` security/quarantine/Front-Desk matrix), Gate H3 (`kernel.ResolveModel`, `kernel.ClassifyInboxFile`, and `templates/triggers.yaml` integrity), Gate H4-A (`gofmt -s` formatting + zero-external-dependency `go.mod` contract), and Gate H4-B (balanced Markdown code fences, zero conflict markers, valid relative links/images across all tracked `.md` files, and 100% `docs/README.md` index coverage).
3. **Local Git Hook Enforcement (`hq ci install-hooks` / `pre-commit` / `pre-push`):** Automatically installs `.git/hooks/pre-commit` and `.git/hooks/pre-push` and rejects commits containing leaked PII/credentials, unformatted Go files, or broken Markdown links before they leave the developer workstation.

---

## CUJ-19: End-to-End Token & Context Observability, Subagent DAG Attribution & Budget Circuit Breaker
1. **Transcript Telemetry Ingestion & DAG Attribution (`hq obs` / `hq hook stop`):** Extracts per-turn `input_tokens`, `cache_read_tokens`, `output_tokens`, `uncached_tokens`, `gross_tokens`, `cache_hit_ratio`, `peak_context_tokens`, `current_context_tokens`, `compaction_count`, and 3-part latency (`LLMLatencyMs`, `ToolLatencyMs`, `HumanWaitMs`) from `transcript.jsonl`, and reconciles parent-child `invoke_subagent` edges in `local/memory/tasks.json` while deduplicating dual-hook invocations (`B1`–`B3`).
2. **CLI & Console Observability (`hq obs tokens|top|inspect|anomalies|budget|reconcile`):** Ranks conversations/tasks by **Self** vs. **Tree (`--tree`)** uncached token burn, Quota Burn Units (`QBU`), gross tokens, context watermark, or latency; renders live token burn and Front Desk context watermark in `local/CONSOLE.md`, `hq_console.md`, and the Web UI (`/api/obs`).
3. **Context & Daily Budget Guards (`A1–A5`):** Injects a 1-line `/compact` nudge in `SessionStart` when Front Desk context exceeds `160k` tokens (`A1`), flags cold-cache thrash (`A2`), runaway turns (`A3`), and deep-tier subagent misrouting (`A4`), and trips the `QuotaState` circuit breaker for `kind: auto` background triggers when daily uncached/deep token burn reaches `100%` (`A5`) until midnight or `hq obs budget --reset-circuit`.

---

## CUJ-20: Open-Source `--demo` Onboarding, Governance TCB & Git History PII Sanitization (RFC-0013)

**User Intent:** Clone `agy-hq` on a clean workstation, evaluate the Task Board, Inbox Triage, Thesis Check, and SHA-256 Approval Console in under 5 minutes using synthetic `[Sample]` data (`./bin/hq init --demo`), and mathematically guarantee zero PII leaks across both `HEAD` and full git commit history (`./bin/hq ci check-pii --history`).

**Expected Deterministic Flow:**
1. **Opt-In Synthetic `--demo` Seeding (`hq init --demo`):** Bare `./bin/hq init` scaffolds an empty private `local/` Vault, whereas `./bin/hq init --demo` (or `./scripts/bootstrap.sh --demo`) non-destructively seeds `local/inbox/acme-corp-2026-10-Q.html`, `local/notes/investing/tickers/ACME/thesis.md`, 3 `[Sample]` Layer-1 tasks, 1 SHA-256 bound `[Sample]` pending approval, and 3 verified bi-temporal `[Sample]` facts.
2. **Structured Diagnostics & Governance Ring 0 TCB (`hq doctor --json`):** `./bin/hq doctor --json` reports structured `PASS`/`WARN` status across Go toolchain, Antigravity sandbox, Vault, Task Board, Memory budget, and Git hooks; `LICENSE` (`Apache-2.0`) and `SECURITY.md` are protected inside `rsi.Ring0ProtectedPaths`.
3. **Historical Commit Audit & Deterministic Sanitization (`hq ci check-pii --history` & `hq ci scrub-history`):** Invariant `P6-ContextualPII` blocks non-synthetic Google Chat `spaces/<id>` literals and terms listed in `local/private/pii-denylist.txt`; `hq ci check-pii --history` catches PII leaked in earlier commits even if deleted from `HEAD`; `hq ci scrub-history --replacements=<map> --apply` deterministically rewrites historical commits and prunes backup refs so `--history` passes with zero violations.
