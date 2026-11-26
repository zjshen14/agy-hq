# Product Spec 0003: Critical User Journeys (CUJs) & E2E Acceptance Criteria

> **Status:** Active (`internal/e2e/cuj*_test.go` — 50 `TestCUJ*` functions across CUJs 1–48)
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
| **CUJ-6** | Parallel Multi-Repo Git Worktree Lifecycle & Orphan Prune | `hq worktree add`, `hq worktree list`, `hq worktree attach`, `hq worktree remove`, `hq worktree prune` | `TestCUJ6_ParallelWorktreeLifecycle`, `TestCUJ_MultiWorktreeLifecycleAndOrphanPrune` |
| **CUJ-7** | Quarantined Inbox Drop & 0-Token Heartbeat Gatekeeper | `hq heartbeat`, `local/inbox/` | `TestCUJ7_InboxRoutingAndHeartbeatGatekeeper` |
| **CUJ-8** | Zero-Token Per-Turn Journaling & Live Console Sync | `hq hook stop`, `local/CONSOLE.md`, `hq_console.md` | `TestCUJ8_TurnJournalingAndLiveConsoleSync` |
| **CUJ-9** | Engine Upgrade & Non-Destructive Vault Migration | `hq upgrade`, `hq migrate` | `TestCUJ9_EngineUpgradeAndVaultMigration` |
| **CUJ-10** | Front Desk Non-Blocking Guardrails | `hq hook pre-tool`, `internal/hooks` | `TestCUJ10_FrontDeskNonBlockingEnforcement` |
| **CUJ-11** | Subagent Lifecycle & Task Board Tracking | `hq hook pre-tool`, `invoke_subagent`, `manage_subagents` | `TestCUJ11_SubagentLifecycleAndTaskboardTracking` |
| **CUJ-12** | Automated Trigger Scheduling & Background Daemon | `hq daemon`, `hq heartbeat`, `local/triggers.yaml` | `TestCUJ12_AutomatedTriggerSchedulingAndDaemon` |
| **CUJ-13** | Front Desk Init Hardening & Trigger Arming | `hq init`, `hq hook session-start`, `hq hook pre-tool` | `TestCUJ13_FrontDeskInitHardeningAndTriggerArming` |
| **CUJ-14** | Gmail Connector Lifecycle (Watcher, Quarantine & Offers) | `hq watch`, `hq offer`, `templates/connectors/gmail/` | `TestCUJ14_GmailLayer1AndLayer2Lifecycle` |
| **CUJ-15** | 4-Layer Memory Engine & Dreaming Verification | `hq memory`, `local/memory/topics/`, `local/memory/facts.jsonl` | `TestCUJ15_MemoryEngineUpgradeAndDreamingVerification` |
| **CUJ-16A** | Google Chat Integration, Quarantine Lockdown & Approval-Bound Send | `hq gchat`, `local/memory/gchat/` | `TestCUJ16_GoogleChatQuarantineAndApproval` |
| **CUJ-16B** | Scalable Task Board, Per-Tier Visibility & Cold Archive Recall | `hq tasks`, `local/memory/tasks.json` | `TestCUJ16_TaskBoardTieredConsoleAndColdArchive` |
| **CUJ-16C** | Antigravity Sidebar Conversation Archive Sync | `hq archive`, `hq tasks compact`, `hq reopen`, `hq tasks sync-sidebar` | `TestCUJ_SidebarConversationArchiveSync` |
| **CUJ-17** | Bounded Agent Recursive Self-Improvement (RSI) | `hq rsi`, `internal/rsi` | `TestCUJ17_AgentRSIMiningVerificationAndApproval` |
| **CUJ-18** | Automated CI/CD Pipeline, PII/Vault Guard & Harness/Go/Markdown Gate | `hq ci`, `internal/ci` | `TestCUJ18_CIPipelinePIIGuardAndHarnessContract` |
| **CUJ-19** | End-to-End Token & Context Observability, Subagent DAG Attribution & Budget Circuit Breaker | `hq obs`, `internal/obs`, `hq hook stop`, `hq doctor` | `TestCUJ19_TokenObservabilityAnomalyAndBudgetGuards` |
| **CUJ-20** | Open-Source `--demo` Onboarding, Governance TCB & Git History PII Sanitization | `hq init --demo`, `hq doctor --json`, `hq ci check-pii --history`, `hq ci scrub-history` | `TestCUJ20_OpenSourceDemoOnboardingGovernanceAndPIIGuard` |
| **CUJ-21** | Zero-Token Cursor Gatekeepers, 4-Tier Attention Outbox & RSI Proactive Noise Mining | `hq heartbeat --force-gatekeepers`, `hq outbox`, `hq brief`, `hq ideas`, `hq rsi mine` | `TestCUJ21_ZeroTokenGatekeepersOutboxDrainAndRSIProactiveNoise` |
| **CUJ-22** | Stateful Multi-Day `Goals`, Milestones & Zero-Token `WaitingOn` SLA Sweep | `hq goals`, `hq heartbeat`, `local/memory/goals.json` | `TestCUJ22_StatefulGoalsMilestonesAndWaitingOnSLASweep` |
| **CUJ-23** | Context-Scheduled Console `Spaces`, On-Demand Entity Dossiers & 4-Layer Surgical `forget` | `hq spaces`, `hq memory add-entity|lookup|forget` | `TestCUJ23_ScheduledConsoleSpacesEntityLookupAndSurgicalForget` |
| **CUJ-24** | Universal Credential Vault Read-Guard, `24h` Proposal TTL & Outbound Contextual-PII Badge | `hq hook pre-tool`, `hq propose --ttl`, `hq approve` | `TestCUJ24_UniversalCredentialGuardProposalTTLAndContextualPIIBadge` |
| **CUJ-25** | Global Plugin Packaging, Standalone Vault Resolution & External Workspace Safety | `hq plugin`, `hq version`, `hq vault-path` | `TestCUJ25_GlobalPluginPackagingAndStandaloneVault` |
| **CUJ-26** | Crash-Safe Event Inbox, Authenticated Webhook Push & Precheck Error Health | `hq events`, `hq heartbeat`, `hq doctor` | `TestCUJ26_CrashSafeEventInboxWebhookPushAndPrecheckHealth` |
| **CUJ-27** | Agent-Armed Durable Follow-Up Timer Across Run Exit | `hq timers`, `hq heartbeat` | `TestCUJ27_AgentFollowUpTimerAcrossRunExit` |
| **CUJ-28** | Deferred Approval Resumption, Shell-Free Argv Scrub & Progress Checkpoints | `hq propose`, `hq approve`, `hq reject`, `hq progress` | `TestCUJ28_DeferredApprovalResumptionArgvScrubAndProgressCheckpoints` |
| **CUJ-29** | Lean `[Auto]` `SessionStart`, Gray-Zone `PreToolUse` Guard, Exec Sandbox & Outbox Eval | `hq hook session-start`, `hq hook pre-tool`, `hq outbox eval` | `TestCUJ29_UnattendedAutoGrayZoneGuardSandboxAndOutboxEval` |
| **CUJ-30** | Modular Connector Protocol, CLI Transport & 0-Token Watcher Engine | `hq connector`, `hq watch` | `TestCUJ30_ConnectorProtocolCLITransportAndWatcherEngine` |
| **CUJ-31** | Provenance Taint, Human-Presence Approval Proof & Offer Gateway | `hq hook session-start`, `hq hook pre-tool`, `hq offer stage`, `hq approve --cap` | `TestCUJ31_ProvenanceTaintApprovalProofAndOfferGateway` |
| **CUJ-32** | Outbox Quiet-Hours DND, Tainted Dedup Isolation & Fenced System Cards | `hq outbox add`, `hq heartbeat` | `TestCUJ32_OutboxQuietHoursAndFencedSystemDelivery` |
| **CUJ-33** | Lease-Based Morning Brief & Quarantined Extensions | `hq brief collect`, `hq brief commit` | `TestCUJ33_LeaseBasedMorningBriefAndStructuredExtensions` |
| **CUJ-34** | Companion Extension Packs, Trust Ceremony, Host Portability & Pure-Go Sidecar | `hq extension`, `hq dispatch resolve`, `hq sidecar console` | `TestCUJ34_ExtensionPacksTrustAndHostPortability` |
| **CUJ-35** | Two-Tier RSI, Vault Skills (`local/skills/`), Inline Overlays & Tier 2 Upstream Export | `hq skill`, `hq rsi`, `hq dispatch resolve` | `TestCUJ35_TwoTierRSIVaultSkillsAndOverlays` |
| **CUJ-36** | Customizable Outbound Approval Policy, Native Coexistence & Lifecycle Hardening | `hq propose`, `hq hook pre-tool`, `hq approve`, `hq reject --dismiss` | `TestCUJ36_CustomizableApprovalPolicyAndUXHardening` |
| **CUJ-38** | Vendor-Neutral Front Desk Channel Bridge & Rotation Tunneling | `hq channel`, `hq daemon`, `hq hook stop` | `TestCUJ38_FrontDeskChannelBridgeAndRotationTunneling` |
| **CUJ-39** | Memory v2 Orthogonal Vault, Scoped Playbooks, BM25 Search & Eval | `hq memory add-fact|search|dream-plan|eval|compact` | `TestCUJ39_MemoryV2ScopedTopicsSearchDreamPlanAndEval` |
| **CUJ-40** | Layer-1 Task Board Isolation & Nested Child Filtering | `hq hook pre-tool|session-start|stop`, `hq adopt`, `hq tasks clean-nested` | `TestCUJ40_Layer1TaskBoardIsolationCLI` |
| **CUJ-41** | Pluggable Workspace Providers: Pack Contract, Home-Placement Spawn, Provider-Delegated Archive & Workspace Hygiene | `hq extension lint`, `hq workspace`, `hq spawn --workspace`, `hq archive`, `hq hook pre-tool`, `hq hook session-start` | `TestCUJ41_PluggableWorkspaceProviders` |
| **CUJ-42** | Unified Session Authorization Lattice, Quarantine Hardening & Asymmetric Interactive Presets | `internal/authz`, `hq hook pre-tool`, `hq status`, `hq brief format-quarantined` | `TestCUJ42_UnifiedAuthzLatticeAndQuarantineHardening` |
| **CUJ-43** | Scoped Delegate Read-Only Investigation, Curated Public Status & Ring-0 Egress Gateway | `hq delegate poll|read-request|read-task|run|reply|audit`, `hq progress save --public-status`, `CONSOLE.md` | `TestCUJ43_ScopedDelegateReadOnlyCuratedStatusAndEgress` |
| **CUJ-44** | CaMeL `queue_only` Task Nudge (0-Token Fast Path + Zone-2 Delivery) & Non-Widening Configure | `hq delegate poll|show|configure|ack-nudge`, `hq hook session-start`, `hq approve` | `TestCUJ44_CaMeLQueueOnlyNudgeAndNonWideningConfigure` |
| **CUJ-45** | 0-Token Pulse Look-Ahead (`D1`–`D10`), 3-State Goal Readiness & 3m Outbox Burst Coalescing | `hq goals checklist|park`, `hq pulse precheck`, `hq outbox add|eval` | `TestCUJ45_PulseAnticipationReadinessAndBurstCoalescing` |
| **CUJ-46** | 1-Tap `DecisionOption` Cards, Ring-0 Taint Safety, Proposal Restage, `hq cancel` & Noisy-Interrupt RSI | `hq outbox act`, `hq approvals expired|restage`, `hq cancel`, `hq rsi mine` | `TestCUJ46_DecisionOptionCardsTaintSafetyAndProposalRestage` |
| **CUJ-47** | Composable Living Spaces, Locale-Neutral vs. Vault i18n, 0-Token `dataSources` & CSP Sandbox | `hq space create|refresh|sync-goal|export-artifact`, `:4242/living-spaces/<slug>` | `TestCUJ47_ComposableLivingSpacesI18nDataSourcesAndCSPSandbox` |
| **CUJ-48** | 4-Tab Console UX, Per-Turn Multi-Model Token Calibration, Disjoint Tree Attribution, `512k` Ceiling & Dual-Artifact Front Desk Lifecycle | `hq obs tokens|top|anomalies`, `hq console`, `hq hook session-start|pre-tool`, `hq claim-desk`, `:4242/` | `TestCUJ48_ConsoleUXTokenCalibrationAndDualArtifactSync` |

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
1. **Automatic Multi-Depth Complexity Tiering:**
   - `quick` prompts (`watchlist` / gatekeeper polling, `status`, `triage`, `summary`, `typo`) → `gemini-4-hydrogen-low` (`flash_lite`).
   - `routine` / medium synthesis prompts (`morning-brief`, `edgar-close-watch`, filing extraction, multi-source summaries) → `gemini-4-hydrogen-medium` (`flash`, canonical tier `standard`).
   - `standard` engineering prompts (feature coding, unit tests, bug fixes) → `gemini-4-neon-medium` (`flash`).
   - `heavy` sweep prompts (exhaustive multi-file refactors, hard E2E/test debugging) → `gemini-4-neon-high` (`flash`, canonical tier `standard`).
   - `deep-medium` consolidation prompts (`sleep-consolidate`, deep research memos) → `gemini-4-neon-high` (`pro`, canonical tier `deep`).
   - `deep` high-stakes prompts (`architecture`, `design`, `security`, `audit`, `race condition`, `root cause`, `KC-1` / thesis falsification) → `gemini-4-carbon-high` (`pro`).
2. **User Override Priority:**
   - Explicit CLI flags (`--model=opus --depth=high`, `--model=sonnet --depth=medium`, `--model=hydrogen --depth=xhigh`, `--model=neon --depth=xhigh`, `--tier=routine|heavy|deep-medium`) and natural-language prompt hints (`"use opus"`, `"use sonnet"`, `"use neon"`, `"use flash"`) always override automatic tiering.
3. **429 Quota Fallback & Circuit Breaker:**
   - If a non-Flash model hits `429 RESOURCE_EXHAUSTED`, `SpawnTask` automatically retries once on `gemini-4-hydrogen-medium`.
   - Daily background spawns are capped at `maxDailySpawns` (15), tripping the circuit breaker (`circuitOpen: true`) when exhausted.

### CUJ-5: Approval-Bound Outbound Execution & Tamper Rejection
1. **Hard-Deny in `PreToolUse` (`hq hook pre-tool`):**
   - Direct execution of `gws gmail send`, `gws chat send`, `gh pr merge`, `gh release create`, or `vercel --prod` via `run_command` is blocked with `{"decision":"deny"}`.
2. **Proposal Staging (`hq propose`):**
   - Writes `local/memory/pending/<id>.json` with deterministic `SHA256(command + "\n" + workDir)` and updates `hq_console.md`.
3. **Cryptographic Verification & Execution (`hq approve` / `hq reject`):**
   - Approving with a mismatched `--sha256` prefix fails and leaves the proposal untouched.
   - Approving with a matching `--sha256` prefix executes the command deterministically, removes the pending proposal, appends an audit record to `local/memory/interventions.jsonl`, and updates `hq_console.md`.

### CUJ-6: Parallel Multi-Repo Git Worktree Lifecycle
1. **`hq worktree add <repo> <task>`:** Creates `local/worktrees/<repo>-<task>` (avoiding duplicate `<repo>-<repo>-` prefixes if `<task>` already begins with `<repo>-`) on branch `hq/<task>`.
2. **`hq worktree list`:** Lists active worktrees and syncs them into `hq_console.md`.
3. **`hq worktree remove <name>`:** Cleanly removes the worktree directory and prunes the git worktree reference.
4. **`hq worktree attach` & `hq worktree prune`:** Attaches existing worktrees to tasks and safely prunes merged/clean orphan worktrees (`TestCUJ_MultiWorktreeLifecycleAndOrphanPrune`).

### CUJ-7: Quarantined Inbox Drop & 0-Token Heartbeat Gatekeeper
1. **0-Token Idle Heartbeat (`hq heartbeat`):** Returns `HEARTBEAT_OK (0 LLM tokens)` when `local/inbox/` and `local/memory/pending/` are empty and today's brief exists.
2. **Quarantine Inbox Router:** Files dropped into `local/inbox/` are classified (`*.csv/*.ofx` → `ledger-ingest`, tax PDFs → `tax-prep`, SEC filings → `thesis-check`, other files → `inbox-triage`), moved atomically to `local/inbox/queued/`, and dispatched to background Flash tasks.

### CUJ-8: Zero-Token Per-Turn Journaling & Live Console Sync
1. **`hq hook stop`:** Parses the JSONL transcript in 0 LLM tokens, extracts `toolsUsed`, `filesModified`, and single-line sanitized `responseSummary`, writes `local/memory/journal/YYYY-MM-DD/<convId>-<execNum>.json`, and refreshes `local/CONSOLE.md` and the Front Desk's `hq_console.md`.

---

## CUJ-9: Multi-Machine Engine Upgrade & Non-Destructive Vault Migration
1. **Pre-Upgrade Vault Snapshot (`local/.git`):** Before touching anything, `./bin/hq upgrade` (or `hq migrate`) commits any uncommitted changes in `local/.git` as a restore point (`chore(vault): pre-upgrade snapshot`).
2. **Upstream Code Pull & Binary Rebuild:** Pulls the latest commits on the root `agy-hq` repository (`git pull --ff-only`), recompiles `./bin/hq`, and re-execs the newly compiled `./bin/hq migrate` subprocess so newly pulled migration steps take effect immediately.
3. **Auto-Rebuild on `git pull` (`SessionStart` Hook):** Even if the user runs `git pull` manually without running `./bin/hq upgrade`, the `SessionStart` hook in `.agents/plugins/agy-hq/hooks.json` detects when `.go` files in `cmd/` or `internal/` are newer than `./bin/hq`, automatically recompiles `./bin/hq`, and runs `./bin/hq migrate --quiet`.
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
3. **Trigger Reconciliation:** Tracks `schedule` and `manage_task` calls in `PreToolUse` and reconciles against `local/triggers.yaml` so declared jobs (`gmail-inbox-watch`, `morning-brief`, `sleep-consolidate`) are never left dormant.
4. **Subagent Pointer Isolation:** Subagent and task conversation turns never overwrite the Front Desk's `front_desk_turn.json` lookup counter or `active_artifact_dir.txt`.

---

## CUJ-14: Gmail Connector Lifecycle (Watcher, Quarantine & Offers)
1. **Multi-Account Connector Instance (`private/connectors/gmail/instance.json`):** Stores account configurations (`primary`, `secondary`, etc.) backed by the official Antigravity `gmail` plugin CLI or read-only OAuth tokens via `templates/connectors/gmail/connector.json`.
2. **0-Token Watcher Precheck (`hq watch precheck --connector=gmail`):** Polls unread threads in pure Go without LLM exposure and deduplicates previously seen messages.
3. **Quarantined Thread Read (`hq watch read --connector=gmail`):** Strips hidden HTML/scripts into sanitized, nonce-delimited plain text and marks the reading session tainted.
4. **Approval-Bound Reply Offers (`hq offer stage --connector=gmail` → `hq approve`):** Enforces participant locks and SHA-256 verification before executing outbound drafts or replies via the connector.

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
2. **Hypothesis & 4-Gate Sandboxed Verification (`hq rsi hypothesize`, `hq rsi verify`):** Synthesizes bounded `<!-- rsi:rule -->` deltas and verifies them against Gate 1 (Ring 0 TCB AST & token budget), Gate 2 (counterfactual trace replay), Gate 3 (`go vet` + unit tests), and Gate 4 (CUJ E2E suite).
3. **1-Tap Approval & Canary Tracking (`hq rsi propose`, `hq approve`, `hq rsi status`):** Stages verified patches via `hq propose --type=rsi_patch`, injects the canonical scorecard into `ask_question`, applies on `hq approve`, and tracks canary metrics (`archive.jsonl` / `hq rsi ablate`) on post-merge friction regression.

---

## CUJ-18: Automated CI/CD Pipeline, PII/Vault Guard & Harness/Go/Markdown Gate
1. **Mathematical PII & Vault Boundary Scanner (`hq ci check-pii`):** Enforces all 6 invariants (P1 forbidden `local/` paths, P2 `.gitignore` & no gitlink submodule on `local/`, P3 workstation home path freedom, P4 secret/token/SSN regexes, P5 strict synthetic email allowlist, P6 contextual PII & real `spaces/<id>` guard) across staged diffs (`--staged`), the full tracked tree, and commit history (`--history`).
2. **Agent Harness, Go, Markdown & Anti-Slop Regression Contracts (`hq ci check-harness`):** Verifies Gate H1 (Skill catalog, YAML frontmatter, `<=150` lines, `<!-- rsi:rule -->` schema), Gate H2-A/H2-B/H2-C (single-source `.agents/plugins/agy-hq/hooks.json` contract, `plugin.json` == `version.Version` SemVer parity, sidecar `main.mjs` byte parity, and `PreToolUse` security/credential-guard/quarantine/Front-Desk matrix), Gate H3 (`kernel.ResolveModel`, `kernel.ClassifyInboxFile`, and `templates/triggers.yaml` integrity), Gate H4-A (`gofmt -s` formatting + zero-external-dependency `go.mod` contract), Gate H4-B (balanced Markdown code fences, zero conflict markers, valid relative links/images across all tracked `.md` files, and 100% `docs/README.md` index coverage), and Gate H5 (`H5-A` zero production `"testing"` imports, `H5-B` zero dead unexported symbols via `go/types`, `H5-C` zero unlocked raw `os.WriteFile` on lock-ranked Vault JSON stores, `H5-D` full console sidecar byte/mode parity, `H5-E` `TestCUJ*` E2E-to-Doc parity, and `H5-F` monolith file LOC ceiling ratchet).
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

---

## CUJ-21: Zero-Token Cursor Gatekeepers, 4-Tier Attention Outbox & RSI Proactive Noise Mining ([RFC-0014](../design/0014-proactive-engine-and-living-context.md))
1. **Cursor-Deduplicated Pre-Checks (`hq heartbeat --force-gatekeepers`):** Evaluates `daemon_gatekeeper` schedules (`precheck_cmd`) against `local/memory/trigger_cursors.json`. New `stdout_sha256` output dispatches an isolated `flash` worker; identical `stdout_sha256` output on subsequent runs skips execution in **0 LLM tokens** (`gatekeeperSkips=1`).
2. **Morning Brief Outbox Rollup (`hq outbox add`, `hq brief collect --lease` / `hq brief commit`):** Background runs emit `brief_rollup` items (`hq outbox add --tier=brief_rollup`), which `/morning-brief` leases and atomically commits once synthesized into `<vault>/notes/briefs/`.
3. **Proactive Noise Feedback Loop (`hq ideas dismiss` $\rightarrow$ `hq rsi mine`):** When a skill emits $\ge 5$ resolved `ideas_feed` suggestions with a $> 70\%$ user dismissal rate, `hq rsi mine` deterministically emits a `proactive_noise` friction event to tighten that skill's threshold.

---

## CUJ-22: Stateful Multi-Day `Goals`, Milestones & Zero-Token `WaitingOn` SLA Sweep ([RFC-0014](../design/0014-proactive-engine-and-living-context.md))
1. **Goal & Milestone Lifecycle (`hq goals add|milestone|check-off|complete`):** Tracks multi-day intentions and milestones in `local/memory/goals.json` and renders progress in the live Console.
2. **Counterparty Silence Detection (`hq goals wait-on|due|mark-replied`):** Registers external reply SLAs (`FollowUpAfter`). `hq heartbeat` sweeps active obligations in **0 LLM tokens**, transitioning overdue silent threads to `nudge_queued` and surfacing an outbox follow-up alert until resolved via `hq goals mark-replied`.

---

## CUJ-23: Context-Scheduled Console `Spaces`, On-Demand Entity Dossiers & 4-Layer Surgical `forget` ([RFC-0014](../design/0014-proactive-engine-and-living-context.md))
1. **Schedule-Aware Console `Spaces` (`hq spaces list|active|pin|hide|auto`):** Evaluates weekday/weekend wall-clock windows in pure Go (0 LLM tokens, 0 untrusted JS) to render active domain cards (`morning-ops`, `market-watch`, `engineering`, plus custom `local/console_spaces/*.yaml` definitions) in `CONSOLE.md`, `hq_console.md`, and `:4242`.
2. **On-Demand Entity Dossiers (`hq memory add-entity|entities|lookup`):** Stores `0600`-permissioned contact/counterparty dossiers in `local/memory/entities/<slug>.md` with `<5ms` lookup and **0 bytes** injected into `SessionStart`.
3. **4-Layer Surgical `forget` (`hq memory forget [--block-pii] [--dry-run]`):** Excises matching facts (`facts.jsonl`), roadmap bullets (`MEMORY.md`), topic/entity files (`topics/`, `entities/`), and note lines (`notes/`), optionally appending the forgotten string to `local/private/pii-denylist.txt` and snapshotting `local/.git`.

---

## CUJ-24: Universal Credential Vault Read-Guard, `24h` Proposal TTL & Outbound Contextual-PII Badge ([RFC-0014](../design/0014-proactive-engine-and-living-context.md))
1. **Universal Credential Vault Isolation (`hq hook pre-tool`):** Denies direct `read_file`, `write_file`, `edit_file`, `grep_search`, `find_by_name`, and shell inspection (`cat`, symlinked path tokens, etc.) targeting `local/private/{gmail,gchat,credentials,pii-denylist.txt}` across **every** session (tainted or untainted), while preserving untainted worker access to `local/private/{ledger,taxes}/`.
2. **Outbound Contextual-PII Leak Warning (`hq propose` + `ask_question`):** Scans staged outbound payloads against `pii-denylist.txt` and SSN/account/street-address patterns, recording `RiskFlags` and prepending a prominent `⚠️ [PII WARNING: ...]` banner into `ask_question` and `:4242`.
3. **Deterministic Proposal Expiration (`--ttl`, Default `24h`):** Rejects `hq approve` calls on expired pending proposals (`status = "expired"`) so stale drafts can never execute days later.

---

## CUJ-16C: Antigravity Sidebar Conversation Archive Sync ([RFC-0016](../design/0016-sidebar-conversation-archive-sync.md))
1. **ConnectRPC Sidebar Mirroring (`hq archive`, `hq tasks compact`, `hq reopen`):** Mirrors task archival and cold recall into Antigravity's sidebar (`UpdateConversationAnnotations` `is_archived`) while enforcing Front Desk, pinned-task, and active-turn safety gates.
2. **Offline Retry Queue & Backfill (`hq tasks sync-sidebar`):** Queues sidebar archive/unarchive mutations durably when no live language server is reachable and drains or backfills cleanly once reconnected.

---

## CUJ-25: Global Plugin Packaging, Standalone Vault Resolution & External Workspace Safety ([RFC-0015](../design/0015-global-plugin-packaging.md))
1. **Global Bundle & Embedded Templates (`hq plugin install|status|uninstall`, `hq version`):** Installs `~/.local/bin/hq` and `~/.gemini/config/plugins/agy-hq/` with single-source `.agents/plugins/agy-hq/hooks.json` and embedded `templates/` scaffolding.
2. **5-Step Vault Resolution & External Repo Isolation (`hq vault-path`):** Resolves the user's canonical Vault across workspaces without scattering `local/` directories into external repositories and keeps regular project sessions free of Front Desk lookup caps.

---

## CUJ-26: Crash-Safe Event Inbox, Authenticated Webhook Push & Precheck Error Health ([RFC-0018](../design/0018-always-on-agent-runtime-hardening.md))
1. **Durable Lease State Machine (`hq events`):** Transitions inbound events atomically through `pending` -> `leased` -> `done`/`dropped` with crash-safe lease expiry recovery and single-worker scope serialization.
2. **Loopback Push Ingress & Precheck Health (`hq doctor`):** Accepts authenticated webhook push events into the durable inbox and surfaces failing gatekeeper prechecks in `hq doctor` instead of silently masking errors as `0`-token skips.

---

## CUJ-27: Agent-Armed Durable Follow-Up Timer Across Run Exit ([RFC-0018](../design/0018-always-on-agent-runtime-hardening.md))
1. **Durable Timer Persistence (`hq timers set|list|cancel`):** Persists self-scheduled follow-up timers across process restarts while blocking tainted sessions from arming wakeups.
2. **Originating Session Resumption (`hq heartbeat`):** Evaluates due timers in `0` LLM tokens and resumes the originating task conversation (or dispatches a bounded worker) once the timer fires.

---

## CUJ-28: Deferred Approval Resumption, Shell-Free Argv Scrub & Progress Checkpoints ([RFC-0018](../design/0018-always-on-agent-runtime-hardening.md))
1. **Closed-Loop Approval/Rejection Wakeup (`hq approve` / `hq reject`):** Automatically notifies the originating task conversation with structured execution output or rejection reason so multi-step runs resume cleanly.
2. **Scrubbed Execution & Cross-Run Progress (`hq progress`):** Supports shell-free `CommandArgv` proposals with scrubbed child environments and structured `local/memory/progress/<slug>.md` checkpoints across turns.

---

## CUJ-29: Lean `[Auto]` `SessionStart`, Gray-Zone `PreToolUse` Guard, Exec Sandbox & Outbox Eval ([RFC-0018](../design/0018-always-on-agent-runtime-hardening.md))
1. **Fail-Closed Unattended Guardrails (`hq hook session-start`, `hq hook pre-tool`):** Serves a lean static-first `SessionStart` context to `[Auto]` runs and blocks gray-zone external mutations (`git push`, `gh issue`, `gh pr`, outbound `curl POST`) unless staged via `hq propose`.
2. **Offline Outbox Replay (`hq outbox eval`):** Scores attention-tier routing and suppression accuracy deterministically against labeled fixture corpora in `0` LLM tokens.

---

## CUJ-30: Modular Connector Protocol, CLI Transport & 0-Token Watcher Engine ([RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md))
1. **Declarative Connector & CLI Transport (`hq connector`):** Separates immutable `connector.json` transport profiles from per-account `instance.json` state across Gmail, Google Chat, Calendar, and companion packs.
2. **Cursor-Locked Watcher Engine (`hq watch precheck|ack`):** Runs scrubbed connector subprocesses under `internal/store` rank locks, paginates `events.Enqueue`, and quarantines poison items after bounded retries.

---

## CUJ-31: Provenance Taint, Human-Presence Approval Proof & Offer Gateway ([RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md))
1. **Lease-Time Taint & Offer Staging (`hq offer stage`):** Binds `[hq-spawn:<token>]` quarantined sessions on `SessionStart`, enforces authenticated thread-participant recipient locks on `reply_draft`, and requires prior `inspect_thread` verification.
2. **Transcript-Verified Human-Presence Capability (`hq approve --cap`):** Mints short-lived single-use HMAC capability tokens in `PreToolUse` when `ask_question` surfaces a verified proposal and rejects capability-less `hq approve` calls in live sessions.

---

## CUJ-32: Outbox Quiet-Hours DND, Tainted Dedup Isolation & Fenced System Cards ([RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md) & [RFC-0017c](../design/0017c-lease-based-morning-brief-and-extensions.md))
1. **Quiet-Hours DND Suppression & Tainted Dedup Isolation (`hq outbox add`):** Holds `interrupt` deliveries during configured quiet hours (unless `--urgent-override` on untainted callers) and isolates tainted dedup keys so quarantined readers cannot suppress legitimate alerts.
2. **Nonce-Fenced System Cards:** Wraps untrusted summaries in per-delivery cryptographic boundary fences before waking `[HQ] Front Desk`.

---

## CUJ-33: Lease-Based Morning Brief & Quarantined Extensions ([RFC-0017c](../design/0017c-lease-based-morning-brief-and-extensions.md))
1. **Two-Phase Brief Lease (`hq brief collect --lease` / `hq brief commit`):** Collects outbox rollups, `<4h` expiring proposals, tainted reader summaries, and `brief.d` extension sections non-destructively until explicitly committed after brief persistence.

---

## CUJ-34: Companion Extension Packs, Trust Ceremony, Host Portability & Pure-Go Sidecar ([RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md))
1. **Trust-Pinned Companion Packs (`hq extension lint|enable|trust`, `hq dispatch resolve`):** Discovers companion `hq-extension.json` packs, enforces explicit SHA-256 binary pinning in `local/private/trust.json`, resolves role-scoped overlays and `hq triggers pause|resume`, and serves the pure-Go console sidecar (`hq sidecar console`).

---

## CUJ-35: Two-Tier RSI, Vault Skills (`local/skills/`), Inline Overlays & Tier 2 Upstream Export ([RFC-0019](../design/0019-two-tier-rsi-vault-overlay.md))
1. **Core OS vs. Vault Skill Split (`hq skill`, `hq dispatch resolve`):** Keeps Core OS skills in `.agents/plugins/agy-hq/skills/`, manages user-space starter/custom skills in `local/skills/` (wired via `.agents/skills.json`), injects Tier-1 personal RSI rules inline via `hq dispatch resolve`, and exports sanitized Tier-2 upstream skill patches.

---

## CUJ-36: Customizable Outbound Approval Policy, Native Coexistence & Lifecycle Hardening ([RFC-0021](../design/0021-customizable-approval-policy-and-ux.md))
1. **Data-Driven `approvals.rules` & Native Coexistence (`hq propose`, `hq hook pre-tool`):** Evaluates `always`, `third_party_only` (`owned_repos`), `auto_only`, and `never` rules across interactive, `[Auto]`, and tainted sessions, rejects dummy `echo`/`true` placeholder commands, preserves multi-question `ask_question` payloads with `24h` `ask_ttl` idempotency, auto-supersedes prior `(ConversationID, ActionType)` proposals, and reconciles UI `Reject` selections and already-landed git refs in `0` LLM tokens.

---

## CUJ-38: Vendor-Neutral Front Desk Channel Bridge & Rotation Tunneling ([RFC-0023](../design/0023-front-desk-channel-bridge.md))
1. **Trust-Pinned `hq-channel-bridge/v1` Lifecycle (`hq channel`, `hq daemon`, `hq hook stop`):** Supervises SHA-256-pinned companion channel bridges with LLM-invisible `0600` credentials (`local/private/channels/`), tunnels inbound messages and `0`-token `desk_reply` mirrors across Front Desk rotations (`bind-desk`), fans out `interrupt` and task `blocked`/`done` cards, and verifies single-use `bca_`/`bcr_` mobile approval/rejection callbacks.

---

## CUJ-39: Memory v2 Orthogonal Vault, Scoped Playbooks, BM25 Search, Gatekeeper Dream-Plan & Eval ([RFC-0024](../design/0024-memory-v2-prediction-and-eval.md))
1. **Orthogonal Scoped Context & Budget Envelopes (`hq hook session-start`, `hq memory add-fact`):** Enforces dual line + byte caps (`MEMORY.md <= 40L / 2.2 KB`, full `<= 12 KB`, `lean_auto <= 4.8 KB` with zero starvation, tainted `<= 800 B`), routes scoped `facts.jsonl` (`--scope`, `--expires`, `--pinned`) and 1:1 `Playbook-Owner` `## Active Playbook` sections (`0` on Front Desk), blocks unattended `[Auto]` direct writes, and guarantees `0`-token TTL expiration.
2. **Full-Vault BM25 Recall, Gatekeeper Dreaming & 3-Layer Eval (`hq memory search|dream-plan|eval|compact`):** Retrieves allowlisted Vault memory layers with clickable `file://#Lstart-Lend` provenance while excluding `private/**` and tainted journals, gates nightly `sleep-consolidate` via `dream-plan --precheck` (`exit_zero_nonempty`) with commit-only cursor advancement, archives `> 14d` journals/briefs safely, and verifies `F1`–`F4` fixtures and M1–M4 metrics via `hq memory eval`.

---

## CUJ-40: Layer-1 Task Board Isolation & Nested Child Filtering
1. **Nested & Workflow Child Exclusion (`hq hook pre-tool|session-start|stop`):** Tracks raw `agentapi new-conversation` spawns while exempting explicit `--title="[Task:...]` forks, keeps workflow-hidden children (`hideFromConversationList` / `run_workflow`), nested subagent trajectories (`nestingDepth >= 1`), and raw `agentapi` helper children off `local/memory/tasks.json`, and records them in `local/memory/nested_conversations.json`.
2. **Layer-1 Preservation & Legacy Cleanup (`hq adopt`, `hq tasks clean-nested --json`):** Preserves top-level tasks, Front Desk helper subagents, standalone user chats, and explicitly adopted `[Task:...]` tasks on `tasks.json`, while sweeping leaked legacy `Kind: "task"` subagent entries into `nested_conversations.json`.

---

## CUJ-41: Pluggable Workspace Providers, Home-Placement Spawn, Provider-Delegated Archive & Workspace Hygiene ([RFC-0025](../design/0025-pluggable-workspace-providers.md))
1. **Pack Contract & Per-Provider Trust (`hq extension lint|enable`, `hq workspace providers`):** Strictly parses `hq-extension.json` `workspace_providers` (unknown fields rejected) and rejects shell or interpreter `command[0]`, relative, filesystem-root, or home-directory `root_patterns`, protected `env_allow` variables, out-of-range `timeout_ms`, unknown protocols, `foreign_placement: true` (deferred in v1), and pack `approval_rules` using the reserved `wsd-` prefix. After the trust ceremony, a tampered provider binary disables only that provider.
2. **Home-Placement Spawn & Rollback (`hq spawn --workspace=<provider-fqid>`):** Creates an `owned`, lease-enforced ledger entry in `local/memory/workspaces.json`, pre-registers the task with its primary binding before the conversation exists (and links the conversation before its first prompt on the Language Server path), starts the conversation in the caller's verified Vault project with the `[hq-workspace …]` preamble, and on spawn failure removes the pre-registered task, the binding, and only the workspace it just created.
3. **Provider-Delegated Archive & Approval-Gated Removal (`hq archive`, `hq workspace remove`, `hq propose --type=workspace_remove`):** Removes a clean, all-landed `owned` workspace; keeps an `owned` workspace with open changes (unbound, `orphaned_open_changes`) and points at an approval-gated `hq workspace remove <key> --force`; only unbinds `attached` workspaces, which `hq` never deletes.
4. **Workspace Hygiene & Sanitized Surfaces (`hq hook pre-tool`, `hq hook session-start`, `CONSOLE.md`, `hq status`):** W1 lets only the writer task edit a lease-enforced workspace and points other callers at `hq workspace handoff` or `hq tasks`; W3 keeps sessions opened from `root_patterns` directories from auto-claiming a vacant Front Desk; W4 gates provider `destructive_commands` under `wsd-<pack>-<id>`. Injected change titles are scrubbed on `CONSOLE.md` and absent from `SessionStart`, an empty ledger keeps the console byte-identical, and legacy `worktreeDir` tasks round-trip unchanged in `tasks.json`.

---

## CUJ-42: Unified Session Authorization Lattice, Quarantine Hardening & Asymmetric Interactive Presets ([RFC-0026](../design/0026-declarative-scoped-delegates.md))
1. **Live Quarantine Hardening & Fail-Closed Clamping (`hq hook pre-tool`, `hq status`, `hq brief format-quarantined`):** Denies `hq status` and `--file` on `hq brief format-quarantined` inside tainted sessions, validates `triage-result` batch IDs against traversal, fails closed on empty/corrupt taint scopes, and clamps expired v2 `SessionGrant` markers to `ProfileFailClosed` (denying even `update_focus`).
2. **Asymmetric Interactive Friction (`approvals.interactive_preset`, `approvals.interactive_gate_mode`):** Relaxes `workspace_own`, `workspace_remove --force`, and non-core pack rules to 0 popups strictly in interactive owner sessions (`low_friction`) while keeping them 100% gated below top-of-lattice (`[Auto]` / subagents), and supports opt-in native `"force_ask"` prompts for live interactive gates.

---

## CUJ-43: Scoped Delegate Read-Only Investigation, Curated Public Status & Ring-0 Egress Gateway ([RFC-0026](../design/0026-declarative-scoped-delegates.md))
1. **Opaque Ingress & Curated Read Mediation (`hq delegate poll|read-request|read-task|run`):** Polls SHA-256-pinned `hq-delegate-transport/v1` transports, persists raw peer prose strictly in LLM-invisible `local/private/delegates/requests/`, spawns tainted `scoped_delegate` workers with a `<= 2048 B` Grant Banner (zero private Vault memory), serves ONLY explicitly curated `## Public Status` (`hq progress save --public-status`) and projected workspace views, and scrubs + nonce-fences stdout from SHA-256-pinned `Targets["delegates"]` commands under per-request run caps.
2. **Defense-in-Depth Egress & Audit Surfaces (`hq delegate reply --body-b64`, `hq delegate audit`, `CONSOLE.md`):** Decodes multiline `--line` / `--body-b64` replies without enabling `write_file`, blocks secrets, non-granted task slugs, sensitive terms, and commitment patterns (failing closed to 1-tap FD3 `apply-reply` approval), delivers clean replies bound to `(request_id, origin_thread)`, and surfaces read-only telemetry in `CONSOLE.md` and `:4242`.

---

## CUJ-44: CaMeL Parameterized Task Actions (`queue_only` & `spawn_sandboxed_worker`) & Non-Widening Configure ([RFC-0026](../design/0026-declarative-scoped-delegates.md))
1. **Zero-Token Regex Fast Path, Zone-2 Queue & Sandboxed Background Worker (`hq delegate poll`, `hq hook session-start`, `hq delegate ack-nudge`):** Extracts typed parameters via deterministic `match.regex`, enforces pre-preflight cooldowns/rate limits, runs SHA-256-pinned preflight commands (discarding raw output), and renders 100% static Go templates (zero raw peer prose) either into protected `local/memory/delegates_queues/<slug>.jsonl` (`delivery_mode: "queue_only"`, surfaced in Zone 2 `<= 800 B` and cleared via `hq delegate ack-nudge`) or into an isolated `unattended_auto` worker event (`delivery_mode: "spawn_sandboxed_worker"`, constrained by `NI-3` `auto_propose_only` + OS sandbox so it can execute investigative/drafting work autonomously without touching `⊤` threads or bypassing approvals).
2. **Semantic Policy Diff (`hq delegate show`, `hq delegate configure`, `hq approve`):** Applies non-widening policy tightenings immediately with 0 popups in interactive owner chat (with optimistic `--base-sha256` concurrency), while fusing privilege-widening edits + binary pins (including `queue_only` → `spawn_sandboxed_worker`) into a single 1-tap `rsi_apply` approval applied via `/dev/fd/3`.

---

## CUJ-45: 0-Token Pulse Look-Ahead (`D1`–`D10`), 3-State Goal Readiness & 3m Outbox Burst Coalescing ([RFC-0027](../design/0027-muse-proactive-actions.md))
1. **3-State Goal Readiness & Unavailable-Window Ripple (`hq goals add|checklist|park`, `hq pulse precheck`):** Tracks `confirmed` / `open` / `skipped` (tombstone) checklists, detects pre-meeting prep (`D1`), horizon/cutoff countdowns (`D2`), deadlines colliding with `blocksAvailability` windows (`D3`), schedule buffer/coverage conflicts (`D5`), and workspace `NeedsOwner` transitions (`D9`) in `0` LLM tokens with idempotent repeat ticks.
2. **Burst Coalescing, Incident Correlation & Scope Rules (`hq outbox add`, `hq outbox eval`):** Merges non-critical interrupts sharing `--incident-key` within `coalesce_window` (`3m`), links follow-ups within `30m`, applies `attention.scope_rules`, and passes all 20 golden fixtures in `hq outbox eval`.

---

## CUJ-46: 1-Tap `DecisionOption` Cards, Ring-0 Taint Safety, Proposal Restage, `hq cancel` & Noisy-Interrupt RSI ([RFC-0027](../design/0027-muse-proactive-actions.md))
1. **Closed-Enum Decision Execution & Timeout Recovery (`hq outbox act`, `hq approvals expired|restage`):** Rejects tainted cross-proposal `DecisionOption` references, catches expired proposals regardless of `> 12h` age via `D6` and `hq approvals expired`, and re-stages fresh `24h` proposals in `0` LLM tokens.
2. **Instant Reversal & Full-Funnel SNR Mining (`hq cancel`, `hq rsi mine`):** Cancels matching events, armed timers, and pending proposals in one command (logging `reverted_by_user` to `activity.jsonl`), and mines `>= 40%` `dismiss_noisy` interrupt rates into `FrictionProactiveNoise` observations targeting Vault-local RSI overlays.

---

## CUJ-47: Composable Living Spaces, Locale-Neutral vs. Vault i18n, 0-Token `dataSources` & CSP Sandbox ([RFC-0027](../design/0027-muse-proactive-actions.md))
1. **Composable Compilation, Goal Sync & Live Data Refresh (`hq space create|refresh|sync-goal|export-artifact`):** Hides the language toggle under the neutral `["en"]` framework default while enabling instant `<script id="hq-i18n">` switching under Vault `["en", "zh"]`, syncs 3-state Goal checklists and `hq_builtin` `workspaces` in `0` LLM tokens, and exports remote-accessible `generative_ui` HTML artifacts (`space_<slug>.html` + `.metadata.json`).
2. **Strict Loopback Isolation (`:4242/living-spaces/<slug>`):** Renders compiled Spaces inside `<iframe sandbox="allow-scripts">` (omitting `allow-same-origin`) with strict `Content-Security-Policy: default-src 'none'` on `/raw`.

---

## CUJ-48: 4-Tab Console UX, Per-Turn Multi-Model Token Calibration, Disjoint Tree Attribution, `512k` Ceiling & Dual-Artifact Front Desk Lifecycle ([RFC-0030](../design/0030-console-ux-token-audit.md))
1. **Per-Turn Model Pricing, `512k` Ceiling & `24h` Calibrated Anomalies (`hq obs tokens|top|anomalies`):** Sorts turn journals numerically (`-2.json` vs `-10.json`), attributes multi-model conversations per turn (`claude-opus-5-5-high` vs `gemini-4-neon-medium`), ranks disjoint root trees without child subagent double-counting, reports a `512k` host compaction ceiling alongside Front Desk `120k`/`160k`/`180k` cold-cache wakeup thresholds, and scopes live `A2`/`A3`/`A4` anomalies to `24h` (`ColdStartInputTokens`, `PlannerSteps`).
2. **Unified 4-Tab Console & Zero-Token Dual-Artifact Lifecycle (`:4242/`, `hq_console_live.html`, `hq hook session-start|pre-tool`, `hq claim-desk`):** Serves 4 tabs (`Needs Me`, `Work`, `Tokens`, `Ops`) with smart default tab selection (`Needs Me` when `ActionRequired > 0` else `Work` when `ActiveTasks > 0`) and plain `Tokens + Cache Hit % + Est. Cost ($)` labels across `:4242` (`Today · 7d · 30d · All`) and `< 45 KB` `hq_console_live.html` (`Today · 7d`), hydrates `write_file(..., Content="{{HQ_SYNC}}")` in `0` `read_file` calls on Turn 1, and retires both `hq_console.md` and `hq_console_live.html` (+ `.metadata.json`) cleanly on Front Desk rotation.
