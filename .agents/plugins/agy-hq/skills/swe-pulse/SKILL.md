---
name: swe-pulse
description: >-
  Single-entry-point software engineering orchestrator. Manages isolated git
  worktrees in local/worktrees/<repo>-<task>/, checks open PRs and CI status across
  repositories, and stages PR merges via hq propose.
---

# Single-Entry-Point Engineering Pulse (`/swe-pulse`)

Work across any number of local or remote GitHub repositories in parallel from inside `headquarter`.

## Steps

1. **Create or Inspect Isolated Git Worktrees:**
   - To start a coding task on any repo (`~/Workspace/<repo>` or `owner/repo` on GitHub) without leaving `headquarter` or dirtying your main checkout:
     ```sh
     hq worktree add <repo-path-or-github-slug> <task-name>
     ```
   - To list all active worktrees:
     ```sh
     hq worktree list
     ```
2. **Parallel Subagent & Task Delegation:**
   - When spawning a dedicated task conversation or delegating to a subagent for `agy-hq` or another repo, always pass `--repo=.` (or `--repo=<path>`) so it works inside an isolated git worktree under `local/worktrees/<repo>-<task>/` instead of dirtying the root checkout.
3. **Local Pre-Submit CI Gate & Zero-Dirty-Tree Auto-Push (When Working on `agy-hq`):**
   - Before committing or pushing any code or documentation change in `agy-hq`, rebuild the local binary and pass the full CI gate (`go build -o bin/hq ./cmd/hq && hq ci pre-push`, enforced automatically by `.git/hooks/pre-commit` and `.git/hooks/pre-push`).
   - Whenever a coding turn, follow-up bugfix, or Phase 1 RFC/research doc (`docs/design/`, `docs/research/`, `docs/README.md`) passes `hq ci pre-push`, automatically commit and run `git push origin main` (or `git push -u origin hq/<task>`) before ending the turn so `git status -s` is always clean.
4. **CI & PR Triage:**
   - Inside the target worktree, run `gh pr status` and `gh pr checks` to inspect open PRs and failing GitHub Actions runs.
   - Stage any third-party PR merge or release action via `hq propose --type=pr_merge --summary="..." --cmd="gh pr merge ..."`.

## Learned Operational Rules (RSI Managed)

<!-- rsi:rule id="rsi-rule-swe-pulse-2c42ca" source="obs-20261109-2c42ca57" model_scope="all" added="2026-11-09" helpful="0" harmful="0" -->
- **[Learned Rule · worker_aci_or_skill]:** When using `user_feedback` in `swe-pulse`, Never perform multi-file code modifications or test suite runs inline on the Front Desk; dispatch invoke_subagent or hq spawn. *(Observed 2x; prevents: Always delegate multi-file coding and bugfix tasks (>3 tool calls) to a background subagent in local/worktrees/ instead of blocking the [HQ] Front Desk thread.)*
