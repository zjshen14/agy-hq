# Implementation Roadmap & Action Tracker

This file is the **single source of truth** for tracking what is done, what is in progress right now, and what comes next across all design docs (`PRD`, `RFC`, and `ADR`).

- **How we use this file:**
  1. **Top-of-File "Up Next" Queue:** Always shows the immediate next 1–3 concrete tasks so we never have to hunt across multiple design docs to know what to build next.
  2. **Granular Milestone Checklists (`M0` → `M4`):** Every engineering task is a `- [ ]` checkbox linked to its governing `RFC`/`PRD` spec and target file path.
  3. **Commit-by-Commit Updates:** As we complete each item, we check it off (`- [x]`) in the same commit as the code change.

---

## 🎯 Up Next (Immediate Action Queue)

1. **[M1.5]** Build `hq daemon` and the right-hand **HQ Console** web server (`internal/webui`, `.agents/sidecar.json` → `sidecar://hq/dashboard`) for 1-click `[Approve]` / `[Reject]` and quota monitoring.
2. **[M1.6]** Create the public-safe starter skills in `.agents/skills/*/SKILL.md` (`morning-brief`, `sleep-consolidate`, `inbox-triage`, `ledger-ingest`, `tax-prep`, `thesis-check`, `filing-diff`, `swe-pulse`).
3. **[M0.3]** Smoke-test `agentapi` CLI background task invocation (`--model=flash`) from `hq daemon`.

---

## Milestone Checklists

### M0: Foundation Spikes & Safety Prerequisites
*Goal: Prove the Antigravity hook/sidecar/CLI integration points in isolation and lock down sandbox settings before running any autonomous tasks.*
- **Specs:** [RFC-0001](design/0001-vault-and-task-isolation.md), [RFC-0003](design/0003-approval-bound-execution.md), [ADR-0001](decisions/0001-architecture-option-1-5-antigravity-plus-go-kernel.md)

- [x] **M0.0:** Initialize `agy-hq` git repository, `.gitignore` Vault protections, `.git/hooks/pre-commit` PII guard, and modular `docs/` hierarchy (`PRD` / `RFC` / `ADR`).
- [x] **M0.1:** Scaffold `go.mod` (`github.com/zjshen14/agy-hq`) and `cmd/hq/main.go` with subcommands (`init`, `hook`, `propose`, `approve`, `reject`, `worktree`, `status`, `doctor`).
- [x] **M0.2:** Smoke-test `.agents/hooks.json` (`SessionStart`, `PreToolUse`, `Stop`) to confirm stdin JSON payload parsing (`conversationId`, `transcriptPath`, `workspacePaths`) and stdout JSON responses.
- [ ] **M0.3:** Smoke-test `agentapi` CLI invocation from Go (`--model=flash`) to confirm non-interactive task spawning and error/quota detection.
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
- [ ] **M1.5 (`hq daemon` & Right-Hand `HQ Console`):**
  - Implement `internal/console` HTTP server (`127.0.0.1:<port>`) and `.agents/sidecar.json` (`sidecar://hq/dashboard`).
  - Render the **Pending Approvals Queue** (`[Approve (sha256)]` / `[Reject]`) and execute approved commands deterministically in Go with zero LLM in the loop.
  - Render the **Active Watchers & Daily Quota Meter** view.
- [ ] **M1.6 (Starter `.agents/skills/`):** Author the public-safe starter skills in `.agents/skills/*/SKILL.md`:
  - [ ] `morning-brief/SKILL.md`
  - [ ] `sleep-consolidate/SKILL.md`
  - [ ] `inbox-triage/SKILL.md`
  - [ ] `ledger-ingest/SKILL.md`
  - [ ] `tax-prep/SKILL.md`
  - [ ] `thesis-check/SKILL.md`
  - [ ] `filing-diff/SKILL.md`
  - [ ] `swe-pulse/SKILL.md`

---

### M2: Proactive Loop, 0-Token Gatekeeper & Scheduled Automations
*Goal: Turn `agy-hq` from a reactive assistant into a proactive, silent-by-default agent that monitors events and delivers morning briefs to your phone via Antigravity Remote Control.*
- **Specs:** [PRD-0001](product/0001-vision-and-ux.md), [RFC-0004](design/0004-heartbeat-and-channels.md)

- [ ] **M2.1 (`fsnotify` Inbox Drop Watcher):** Watch `local/inbox/` in `hq daemon`, match dropped files against `local/triggers.yaml` (or general triage fallback), and dispatch background tasks.
- [ ] **M2.2 (30m Zero-Token Heartbeat Gatekeeper):** Run deterministic `precheck_cmd` checks every 30 minutes in Go; only spawn a `--model=flash` conversation when new actionable items exist and daily spawn budget (`max_daily_spawns: 15`) is within limits.
- [ ] **M2.3 (Scheduled Automations):** Configure the three core schedules (`07:30` Morning Brief, `13:30` Post-Close Filing Watch, `23:30` Nightly Sleep-Time Consolidation) pinned to `--model=flash`.
- [ ] **M2.4 (Remote Control End-to-End Verification):** Verify Morning Brief delivery and 1-tap approval execution from a phone browser via `antigravity.google` Remote Control (`remote_control.go` `ChannelAdapter`).

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
