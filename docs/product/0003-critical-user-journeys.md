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
