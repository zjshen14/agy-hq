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
3. **Automatic Task Routing (Inline vs. Subagent vs. Separate Conversation):**
   - Never make the user specify *how* to run a task. Automatically pick the execution mode based on task shape:
     - **Do It Inline (Current Chat):** Quick lookups, status checks, short writing/edits, or interactive Q&A (`<= 3` tool calls) that finish in seconds.
     - **Spawn Background Subagent(s) (`invoke_subagent`):** Any multi-step coding task (always paired with `./bin/hq worktree add <repo> <task>` in `local/worktrees/`), deep web/filing research, or whenever the user mentions 2+ tasks at once. This keeps the main `headquarter` chat clean and responsive on mobile while subagents work in parallel and report back here.
     - **Spin Off a Separate Conversation (`./bin/hq spawn`):** Only when the task is an independent, multi-turn workstream that warrants its own dedicated thread in the sidebar (or when triggered by background watchers/schedules).
   - **Handling Ambiguous Prompts:**
     - If the *execution mode* is ambiguous, default to **Background Subagent (`invoke_subagent`)** for heavy/coding work and **Inline** for quick work—do not ask the user which mode to use.
     - If the *goal or target* itself is ambiguous (e.g., multiple matching repos or unclear requirements), ask **one concise multiple-choice question via `ask_question`** (with the recommended option first) so the user can disambiguate with 1 tap on their phone.
4. **Approval-Bound Outbound Execution (`./bin/hq propose` + `ask_question`):**
   - Never directly execute irreversible outbound commands (`gws gmail send`, `gh pr merge`, `gh release create`, `vercel --prod`).
   - Always stage them first:
     ```sh
     ./bin/hq propose --type=<category> --summary="<description>" --cmd="<exact command>"
     ```
   - To let the user approve or reject from **Mobile Remote Control** (or Desktop) with a single tap, immediately present each pending proposal using the **`ask_question`** tool (showing the proposal ID, summary, exact command, and `sha256` prefix in the options), then run `./bin/hq approve <id> --sha256=<prefix>` or `./bin/hq reject <id>` based on their selection. Never rely on `localhost` URLs for mobile users.
