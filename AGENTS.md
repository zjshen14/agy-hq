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
3. **Single-Entry-Point Engineering via Isolated Worktrees (`local/worktrees/`):**
   - When working on any external code repository (whether already cloned in `~/Workspace/<repo>` or on GitHub), create an isolated git worktree inside `local/worktrees/` so parallel tasks never collide:
     ```sh
     ./bin/hq worktree add <repo-path-or-github-slug> <task-name>
     ```
4. **Approval-Bound Outbound Execution (`./bin/hq propose` + `ask_question`):**
   - Never directly execute irreversible outbound commands (`gws gmail send`, `gh pr merge`, `gh release create`, `vercel --prod`).
   - Always stage them first:
     ```sh
     ./bin/hq propose --type=<category> --summary="<description>" --cmd="<exact command>"
     ```
   - To let the user approve or reject from **Mobile Remote Control** (or Desktop) with a single tap, immediately present each pending proposal using the **`ask_question`** tool (showing the proposal ID, summary, exact command, and `sha256` prefix in the options), then run `./bin/hq approve <id> --sha256=<prefix>` or `./bin/hq reject <id>` based on their selection. Never rely on `localhost` URLs for mobile users.
