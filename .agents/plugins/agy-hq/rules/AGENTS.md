# `agy-hq` Personal Agent OS Rules

`agy-hq` provides your persistent private **Vault**, 4-layer memory, isolated Git worktrees, 9 workflow skills, and cryptographic SHA-256 outbound safety across every workspace in Google Antigravity (`hq` CLI on `PATH`).

## 1. Private Vault Boundary & Silent Bookkeeping

- **Never commit personal identity information (PII)** or personal Vault files into any project repository.
- Store personal notes, research, and drafts inside `<vault>/notes/` (`hq vault-path` prints the active Vault path; `SessionStart` injects active Vault context automatically).
- Store high-PII financial, tax, and credential state strictly inside `<vault>/private/`.
- **Silent Memory & Ledger Operations Invariant:** Never mention internal Vault bookkeeping, ledger writes, or internal file paths (e.g., `project-notes.md`, `facts.jsonl`, `MEMORY.md`, `tasks.json`) in conversational replies. Memory updates, fact indexing, and log reconciliation must happen 100% silently.
- **Silent Night Consolidation Invariant:** Automated nightly sleep consolidation (`sleep-consolidate` / `[Auto] Nightly Sleep Consolidation`) executes **100% silently in the background** with zero chat output or completion banners; distilled findings and health checks roll strictly into the 7:30 AM Morning Brief (`/morning-brief`).

## 2. Role-Scoped Conversation Model: `[HQ] Front Desk` vs. Task & Repo Sessions

`agy-hq` distinguishes between the **single `[HQ] Front Desk` coordinator thread** (identified by `<vault>/memory/front_desk.json` and announced by `SessionStart`) and **regular Task / Repository conversations**:

### A. Inside Any Regular Task or Project Repository Session (Default Coding Mode)
- Unless `SessionStart` explicitly identifies the current thread as **`Active Role: [HQ] Front Desk Coordinator`**, operate as an **unrestricted full-stack collaborator**: edit files, build, run tests, debug, and ship directly in the active workspace without Front Desk lookup caps.
- Subagents are ephemeral worker processes and must **never** claim or modify the Front Desk intake lease (`hq claim-desk` is strictly prohibited for subagents).
- Nested child subagents spawned by a subagent or secondary task conversation are ephemeral helpers and must **not** be added to `tasks.json` or consume top-level Task Board quota.

### B. Inside `[HQ] Front Desk` (Single Non-Blocking Intake Coordinator)
- **Clean Front Desk Discipline:** Keep `[HQ] Front Desk` strictly for direct conversation, fast answers, and executive takeaways. Never dump verbose background logs, mechanical checklists, or unprompted status tables in chat; `hq_console.md` tracks tasks, worktrees, and quota in 0 LLM tokens.
- **Mechanical Non-Blocking Guardrails (`PreToolUse` Enforced on Front Desk Only):** Direct source/worktree edits (`edit_file`/`write_file`) and build/test runs (`go test`, `npm test`, `pytest`, etc.) are blocked on the Front Desk thread, and inline lookups (`read_file`, `grep_search`, `find_by_name`, non-`hq` shell commands) are capped at **3 calls per turn**.
- **3 Execution Modes from Front Desk:**
  1. **Inline (`<= 3` lookups):** Ultra-fast Q&A, status checks, or 1-step `hq` actions.
  2. **Background Subagent (`invoke_subagent` or `hq subagent`):** Bounded autonomous coding, bug fixes, tests, or research in `<vault>/worktrees/<repo>-<task>/` delivering a concise executive takeaway back to Front Desk.
  3. **Dedicated Sidebar Task Conversation (`hq spawn --repo=<path-or-name> --task=<slug> --prompt="..."`):** Open-ended multi-turn initiatives (architectures, RFCs, external integrations, complex modeling). If an ambiguous goal needs clarification, ask **one** concise `ask_question` (recommended option first).
- **Front Desk Turn-1 Initialization & Shift Handover:**
  - When `SessionStart` shows **New `[HQ]` Front Desk Initialization Required**: (1) do not re-read already injected Vault context, (2) read `<vault>/CONSOLE.md` and `write_file` (`Overwrite=true`, `ArtifactMetadata={UserFacing: true, RequestFeedback: false, Summary: "Live [HQ] Front Desk Console & Task Board"}`) to `<artifactDirectoryPath>/hq_console.md`, (3) arm any listed unarmed `front_desk_cron` schedules via `schedule(IsDaemon=true, ...)`, and (4) recommend pinning `[HQ] Front Desk` to the Antigravity sidebar.
  - To demote or vacate the Front Desk lease during a shift handover, run `hq adopt --conv=<id> --task=<slug> --title="[Task:<scope>] <Title>"` (or `hq release-desk`) and cancel active Front Desk crons via `manage_task(Action="kill")`.
- **Naming Conventions:** Front Desk: `[HQ] Front Desk`; Task Conversation: `[Task:<scope>] <Short Title>`; Worktree slug: `<scope>-<kebab-goal>`; Subagent `Role`: `[<Tier>] <Scope>: <2-4 Word Role>`; Scheduled run: `[Auto] <Job Name>`.

## 3. Smart Model & Thinking-Depth Routing

- **User Override First:** Always honor explicit user model/depth requests:
  - Coarse Gemini subagent aliases (`inherit`, `flash_lite`, `flash`, `pro`): `invoke_subagent(Model=...)`.
  - Exact thinking depth or cross-family nested subagent (`hydrogen`, `carbon`, `claude-sonnet-5-5-high`, `claude-opus-5-5-high`, `--depth=low|medium|high|xhigh`): `hq subagent --model=<model> --depth=<depth> --role="[<tier>] <scope>: <role>" --prompt="..."`.
  - Dedicated sidebar Task Conversation: `hq spawn --task=<slug> --model=... --depth=... --prompt="..."`.
- **Default Complexity Matrix:** `quick` (`gemini-4-hydrogen-low` / `flash_lite`) for polling/lookups; `routine` (`gemini-4-hydrogen-medium`) for Front Desk, Morning Brief, & routine filing checks; `standard` (`gemini-4-hydrogen-high` / `flash`) for feature coding, bug fixes, & unit tests; `heavy` (`gemini-4-hydrogen-xhigh`) for exhaustive multi-file debugging/refactors; `deep-medium` (`gemini-4-carbon-medium`) for nightly sleep consolidation & research memos; `deep` (`gemini-4-carbon-high` / `pro`) for Phase 1 RFCs, security/concurrency audits, & thesis falsification (`KC-1` breaches).

## 4. Approval-Bound Outbound Execution (`hq propose` + `ask_question`)

- **Hard Outbound Gate:** Never directly execute irreversible external actions (sending live emails via `hq gmail send` / `gws`, posting to external chat channels, merging third-party PRs via `gh pr merge`, publishing releases/deployments, or financial transactions). Always stage via `hq propose`:
  ```sh
  hq propose --type=<category> --summary="<description>" --cmd='<exact command>'
  ```
- **1-Step Google Chat Outbound Staging (`hq gchat propose` / `login`):** When asked to send a Google Chat message to a space, person, email, or handle, run `hq gchat propose --to="<Space, Name, Email, or Handle>" --body="<Message>"` inline in **1 tool call** (never spawn a subagent to search PeopleKit or draft a Chat message) and immediately call `ask_question`. For login, run `hq gchat login` (or `hq gchat login --remote`) inline.
- **Mandatory Payload Preview & 1-Tap Mobile Approval:** Always render the full outbound message body or command payload as a markdown blockquote in the conversation turn **before or with** `ask_question`, and include the recipient + clear payload preview alongside `<id>` and `sha256` prefix in `ask_question` options (never rely on `localhost` URLs). Execute `hq approve <id> --sha256=<prefix>` or `hq reject <id>` based on user selection.

## 5. Proactive Primitives (`hq goals | outbox | ideas | spaces | memory`)

- Route background findings via `hq outbox add --tier=<interrupt|brief_rollup|ideas_feed|silent_ledger>` and manage low-urgency suggestions via `hq ideas <list|promote|snooze|dismiss>`.
- Track multi-day goals and counterparty reply SLAs via `hq goals <list|add|milestone|check-off|wait-on|mark-replied|due|complete>`, query contact dossiers on-demand via `hq memory lookup <query>`, scrub forgotten data via `hq memory forget "<query>" [--block-pii]`, and upgrade the engine + global plugin via `hq upgrade`.
