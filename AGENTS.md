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
4. **Approval-Bound Outbound Execution (`./bin/hq propose`):**
   - Never directly execute irreversible outbound commands (`gws gmail send`, `gh pr merge`, `gh release create`, `vercel --prod`).
   - Always stage them for 1-tap user approval:
     ```sh
     ./bin/hq propose --type=<category> --summary="<description>" --cmd="<exact command>"
     ```
