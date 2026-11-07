# Implementation Roadmap & Action Tracker

This file is the **single source of truth** for tracking what is done, what is in progress right now, and what comes next across all design docs (`PRD`, `RFC`, and `ADR`).

- **How we use this file:**
  1. **Top-of-File "Up Next" Queue:** Always shows the immediate next 1–3 concrete tasks so we never have to hunt across multiple design docs to know what to build next.
  2. **Granular Milestone Checklists (`M0` → `M4`):** Every engineering task is a `- [ ]` checkbox linked to its governing `RFC`/`PRD` spec and target file path.
  3. **Commit-by-Commit Updates:** As we complete each item, we check it off (`- [x]`) in the same commit as the code change.

---

## 🎯 Up Next (Immediate Action Queue)

1. **[M3.1]** Implement optional Google Chat adapter (`internal/channels/gchat.go`) behind `ChannelAdapter` using Desktop OAuth (`chat.spaces`, `chat.messages`) locked to `owner_space_id`.
2. **[M3.2]** Implement optional Telegram adapter (`internal/channels/telegram.go`) using outbound HTTPS long-polling and inline approval buttons locked to `owner_chat_id`.
3. **[M4.1 & M4.2]** Add `hq eval` task replay runner (`pass^3`) and SQLite FTS5 historical journal search index.

---

## Milestone Checklists

### M0: Foundation Spikes & Safety Prerequisites
*Goal: Prove the Antigravity hook/sidecar/CLI integration points in isolation and lock down sandbox settings before running any autonomous tasks.*
- **Specs:** [RFC-0001](design/0001-vault-and-task-isolation.md), [RFC-0003](design/0003-approval-bound-execution.md), [ADR-0001](decisions/0001-architecture-option-1-5-antigravity-plus-go-kernel.md)

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
  - Implement `internal/webui` HTTP server (`127.0.0.1:4747`) and `hq daemon`.
  - Render the **Pending Approvals Queue** (`[Approve (sha256)]` / `[Reject]`) and execute approved commands deterministically in Go with zero LLM in the loop.
  - Render the **Active Worktrees**, **Today's Turn Journal**, and **System Health** views.
- [x] **M1.6 (Starter `.agents/skills/`):** Author the public-safe starter skills in `.agents/skills/*/SKILL.md`:
  - [x] `morning-brief/SKILL.md`
  - [x] `sleep-consolidate/SKILL.md`
  - [x] `inbox-triage/SKILL.md`
  - [x] `ledger-ingest/SKILL.md`
  - [x] `tax-prep/SKILL.md`
  - [x] `thesis-check/SKILL.md`
  - [x] `filing-diff/SKILL.md`
  - [x] `swe-pulse/SKILL.md`

---

### M2: Proactive Loop, 0-Token Gatekeeper & Scheduled Automations
*Goal: Turn `agy-hq` from a reactive assistant into a proactive, silent-by-default agent that monitors events and delivers morning briefs to your phone via Antigravity Remote Control.*
- **Specs:** [PRD-0001](product/0001-vision-and-ux.md), [RFC-0004](design/0004-heartbeat-and-channels.md)

- [x] **M2.1 (`local/inbox/` Drop Router):** Scan `local/inbox/` in `internal/kernel/kernel.go`, classify dropped files (`ledger-ingest`, `tax-prep`, `thesis-check`, or `inbox-triage` fallback), move to `local/inbox/queued/`, and dispatch `--model=flash` background tasks.
- [x] **M2.2 (30m Zero-Token Heartbeat Gatekeeper & Dead-Man's Switch):** Run deterministic pre-checks (`./bin/hq heartbeat` & background loop in `hq daemon`); record `HEARTBEAT_OK (0 LLM tokens)` when idle, enforce `max_daily_spawns: 15`, and alert if `brief-YYYY-MM-DD.md` is missing after 08:00 AM.
- [x] **M2.3 (Scheduled Automation Blueprints):** Provide `templates/sidecars/{hq-morning-brief,hq-edgar-close-watch,hq-sleep-consolidate}/sidecar.json` pinned to `--model=flash`.
- [x] **M2.4 (End-to-End Verification):** Verified `hq heartbeat`, Dead-Man's Switch clearance with `local/notes/general/brief-2026-11-07.md`, and 1-tap SHA-256 approval execution.

---

### M3: External Chat Channel Adapters (Phase 2)
*Goal: Mirror Morning Briefs and interactive `[Approve]` / `[Reject]` cards to external messaging apps without changing any core engine or skill logic.*
- **Specs:** [ADR-0004](decisions/0004-remote-control-first-pluggable-channels.md), [RFC-0004](design/0004-heartbeat-and-channels.md)

- [ ] **M3.1 (Google Chat Adapter — `internal/channels/gchat.go`):** Implement Desktop OAuth user-level scopes (`chat.spaces`, `chat.messages`) and/or Workspace Cloud Pub/Sub listener with hardcoded owner Space ID exfiltration guard.
- [ ] **M3.2 (Telegram Adapter — `internal/channels/telegram.go`):** Implement outbound HTTPS long-polling + inline approval buttons locked to the owner's `chat_id`.

---

### M4: Evaluation Harness & Historical Search Index
*Goal: Measure reliability (`pass^k`) across model/skill changes and scale memory lookup over multi-year journals.*
- **Specs:** [RFC-0002](design/0002-memory-and-consolidation.md)

- [ ] **M4.1 (Private Eval Suite):** Add `hq eval` runner to replay representative tasks (`pass^3`) before promoting proposed `SKILL.md` updates.
- [ ] **M4.2 (SQLite FTS5 Index):** Add optional `local/memory/index.sqlite` FTS5 index rebuilt from `journal/` and `facts.jsonl` once markdown/JSONL files exceed ~5,000 lines.
