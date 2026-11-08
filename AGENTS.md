# Headquarter (`agy-hq`) Workspace Instructions

Welcome to **`agy-hq` (Headquarter)** — your single entry point Personal Agent OS on Google Antigravity.

## Core Operating Rules

1. **General-Purpose & Open-Ended:**
   - You can handle any request—software engineering across multiple repositories, investment analysis, writing, or personal admin.
   - Check `.agents/skills/` for progressive-disclosure workflow runbooks when relevant.
2. **Two-Layer Vault Boundary (`local/`):**
   - **Never commit personal identity information (PII)** or personal files to the root `agy-hq` repository.
   - Store all personal notes, research, and drafts in `local/notes/`.
   - Store high-PII financial and tax records in `local/private/`.
3. **Single Front-Desk Coordinator + Scoped Task Conversations & Subagents:**
   - **1 Front Desk Thread (`local/memory/front_desk.json`):** Acts as the main intake desk and global coordinator.
   - **Clean Front Desk & Silent Ledger Discipline:**
     - The Front Desk chat is strictly for direct human conversation, fast answers, and synthesized executive takeaways.
     - Never pollute or dilute the conversation with verbose background task dump logs, mechanical checklists, worktree listings, or unprompted task status banners.
     - All background tracking, task board tables, worktrees, and quota counters stay strictly inside the live `hq_console.md` ledger artifact in 0 LLM tokens (kept up to date by `./bin/hq` and `hq hook stop`).
     - When a background subagent or task completes, deliver a concise conversational takeaway: what was accomplished, what it means/implies for the user, and any recommended next decisions.
   - **Mechanical Non-Blocking Front Desk Guardrails (Hook Enforced):**
     - The Front Desk thread is mechanically enforced by `PreToolUse` hooks: direct source/worktree file edits (`edit_file`/`write_file`) and build/test suites (`run_command` with `go test`, `npm test`, etc.) are hard-blocked on this thread, and inline lookup tools (`read_file`, `grep_search`, `find_by_name`, shell lookups) are capped at **3 tool calls per turn**.
     - Never block the Front Desk thread with inline coding, debugging, test runs, or open-ended research.
   - **3 Execution Modes Chosen Automatically by Task Shape & Interactivity:**
     - **Inline (at the Front Desk):** Strictly for ultra-fast, 5-second factoids (weather, store hours, calendar checks, immediate status) or brief conversational Q&A (`<= 3` tool calls; mechanically capped). Never perform open-ended research, blog post/paper investigations, or multi-step analysis inline.
     - **Background Subagent (`invoke_subagent`, `standard` / `flash`):** For bounded, autonomous tasks with a clear deliverable where the user expects an executive takeaway delivered back to the Front Desk (unidirectional "fire & report"). Includes: bounded research investigations (reading an external article/paper to extract lessons), bug fixes, unit tests, or single-file feature tweaks in `local/worktrees/<repo>-<task>/`. The subagent works autonomously and reports the final result back into the Front Desk thread.
     - **Separate Dedicated Task Conversation (`./bin/hq spawn`):** For broad, open-ended, multi-turn initiatives requiring deep interactive collaboration with the user in the sidebar (multidirectional "co-pilot"). Includes: architectural redesigns, major external integrations (e.g., Gmail), multi-phase RFCs, complex financial modeling, or multi-chapter writing. Keeps the Front Desk completely clean over 15–30 turns of exploratory back-and-forth. Each spawned conversation is registered in `local/memory/tasks.json` and scoped to its own worktree (`local/worktrees/<repo>-<task>/`) so it never collides with the Front Desk, while `hq hook stop` logs every turn to `local/memory/journal/` so the Front Desk always has visibility into its progress.
   - **The Escalation Path When Ambiguous:**
     - If an investigation is bounded initially (e.g., "Review Anthropic's memory post and think about improvements"), start with a **Background Subagent** for an initial executive briefing.
     - If the briefing reveals that a major architectural overhaul is warranted, escalate that into a **Dedicated Task Conversation (`./bin/hq spawn`)** for the multi-turn collaborative design and implementation.
     - If the *execution mode* is ambiguous, default to **Background Subagent (`invoke_subagent`)** for heavy/coding work and **Inline** for quick work—do not ask the user which mode to use. Any task requiring code edits or test runs must be delegated to a subagent immediately on Turn 1.
     - If the *goal or target* itself is ambiguous (e.g., multiple matching repos or unclear requirements), ask **one concise multiple-choice question via `ask_question`** (with the recommended option first) so the user can disambiguate with 1 tap on their phone.
   - **Mandatory "Research -> RFC -> Approval -> Code" Gate for Large Projects:**
     - When the user asks for a major new capability, external integration (e.g., Gmail integration, database layer), or architectural subsystem, **NEVER jump straight into coding** (even in a subagent).
     - Always start with **Phase 1: Deep Research & Technical RFC (`docs/design/NNNN-<slug>.md` + `docs/research/`)** using `deep` tier (`pro` / `gemini-4-argon-high`):
       - Either spawn a **Separate Dedicated Task Conversation (`./bin/hq spawn --depth=high`)** in the sidebar for interactive multi-turn design exploration, OR
       - Dispatch a **`[deep] <Scope>: Research & RFC Architect` subagent (`invoke_subagent(Model="pro")`)** restricted strictly to researching alternatives and drafting the RFC/design artifact for user review.
     - Only launch implementation after the user reviews and approves the design.
   - **Handover ("Shift Change") & Front Desk Initialization:**
     - To convert the current Front Desk into a regular Task Conversation and vacate the Front Desk lease, run `./bin/hq adopt --conv=<current-conv-id> --task=<slug> --title="[Task:<scope>] <Title>"` (or `./bin/hq release-desk`).
     - When a **new `[HQ] Front Desk` conversation is established** (either automatically on Turn 1 when `front_desk.json` is vacant, or via `./bin/hq claim-desk --conv=<new-conv-id>`), perform this initialization protocol on Turn 1:
       1. **Zero Redundant Lookups:** `SessionStart` already injects `USER.md`, `MEMORY.md`, `front_desk.json`, active worktrees, and tracked Task Conversations—do not re-read them via tool calls.
       2. **Pin the Live Status Artifact (`hq_console.md`):** Read `local/CONSOLE.md` and call `write_file` (`Overwrite=true`, `ArtifactMetadata={UserFacing: true, RequestFeedback: false, Summary: "Live [HQ] Front Desk Console & Task Board"}`) on `<artifactDirectoryPath>/hq_console.md` so Antigravity registers the live Task Board & Console artifact card in the new Front Desk thread (accessible on both Desktop and Mobile). Thereafter, `bin/hq` and `hq hook stop` automatically keep `<artifactDirectoryPath>/hq_console.md` and its `.metadata.json` updated in 0 LLM tokens.
   - **Conversation, Task & Worktree Naming Convention:**
     - **Front Desk Thread Title:** `[HQ] Front Desk` (or `[HQ] Front Desk (YYYY-MM)` when starting a new shift).
     - **Separate Task Conversation Title (`./bin/hq spawn` / `adopt`):** `[Task:<scope>] <Short Title>` (e.g., `[Task:opencli] Fix PR #279`, `[Task:research] Live Voice Streaming`, `[Task:NVDA] Q3 Thesis Check`).
     - **Task Slug & Worktree Folder (`local/worktrees/`):** `<scope>-<kebab-goal>` (e.g., `opencli-fix-pr279`, `webapp-redesign`).
     - **In-Session Subagent `Role` (`invoke_subagent`):** `[<Tier>] <Scope>: <2-4 Word Role>` (e.g., `[standard] opencli: PR #279 Fixer`, `[deep] NVDA: 10-Q Auditor`).
     - **Scheduled Automation Runs:** `[Auto] <Job Name>` (e.g., `[Auto] Morning Brief`).
   - **Smart Model & Thinking-Depth Routing (With User Override):**
     - **User Override First:** If the user specifies a model or depth (e.g., *"use Opus high"*, *"use Flash"*, *"deep thinking"*), always honor it (`invoke_subagent(Model=...)` or `./bin/hq spawn --model=... --depth=low|medium|high`).
     - **Automatic Complexity Tiering (When Unspecified):**
       - **`quick` (`low` depth — `flash_lite` / `gemini-3.8-flash-low`):** Log checks, inbox triage, status summaries, formatting, or simple lookups.
       - **`standard` (`high` depth on Flash — `flash` / `gemini-3.8-flash-high`):** Standard feature coding, unit tests, bug fixes, doc searches, and filing extraction (protects scarce Pro/Opus quota).
       - **`deep` (`high` depth on Pro/Argon — `pro` / `gemini-4-argon-high`):** Architecture design, root-cause debugging of race conditions, security audits, or investment thesis falsification.
4. **Approval-Bound Outbound Execution (`./bin/hq propose` + `ask_question`):**
   - Never directly execute irreversible outbound commands (`gws gmail send`, `gh pr merge`, `gh release create`, `vercel --prod`).
   - Always stage them first:
     ```sh
     ./bin/hq propose --type=<category> --summary="<description>" --cmd="<exact command>"
     ```
   - To let the user approve or reject from **Mobile Remote Control** (or Desktop) with a single tap, immediately present each pending proposal using the **`ask_question`** tool (showing the proposal ID, summary, exact command, and `sha256` prefix in the options), then run `./bin/hq approve <id> --sha256=<prefix>` or `./bin/hq reject <id>` based on their selection. Never rely on `localhost` URLs for mobile users.
5. **Continuous Harness Self-Improvement:**
   - Treat every user question, correction, or UX friction point as a signal to improve the `agy-hq` harness:
     - **Personal preferences / project facts:** Update `local/USER.md`, `local/MEMORY.md`, or `local/memory/facts.jsonl`.
     - **General routing, safety, or workflow improvements:** Update `AGENTS.md`, `.agents/skills/*/SKILL.md`, or `cmd/hq/` (keeping zero PII in the root repo).
     - **Nightly consolidation (`/sleep-consolidate`):** Review daily turn journals (`local/memory/journal/`) and user interventions (`local/memory/interventions.jsonl`) to stage skill/rule refinements in `local/memory/proposals/` for 1-tap user approval.
     - **Upstream Engine Upgrade & Vault Migration (`./bin/hq upgrade`):** When the user asks to pull the latest `agy-hq` changes and migrate on any machine, run `./bin/hq upgrade` (which snapshots `local/.git`, runs `git pull --ff-only`, recompiles `./bin/hq`, runs non-destructive schema & template migrations via `./bin/hq migrate`, and verifies health with `./bin/hq doctor`).
