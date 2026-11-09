---
name: swe-pulse
description: >-
  Single-entry-point software engineering orchestrator. Manages isolated git
  worktrees in local/worktrees/<repo>-<task>/, checks open PRs and CI status across
  repositories, and stages PR merges via ./bin/hq propose.
---

# Single-Entry-Point Engineering Pulse (`/swe-pulse`)

Work across any number of local or remote GitHub repositories in parallel from inside `headquarter`.

## Steps

1. **Create or Inspect Isolated Git Worktrees:**
   - To start a coding task on any repo (`~/Workspace/<repo>` or `owner/repo` on GitHub) without leaving `headquarter` or dirtying your main checkout:
     ```sh
     ./bin/hq worktree add <repo-path-or-github-slug> <task-name>
     ```
   - To list all active worktrees:
     ```sh
     ./bin/hq worktree list
     ```
2. **Parallel Subagent & Task Delegation:**
   - When spawning a dedicated task conversation or delegating to a subagent for `agy-hq` or another repo, always pass `--repo=.` (or `--repo=<path>`) so it works inside an isolated git worktree under `local/worktrees/<repo>-<task>/` instead of dirtying the root checkout.
3. **Zero-Dirty-Tree Auto-Commit & Push:**
   - Whenever a coding turn, follow-up bugfix, or Phase 1 RFC/research doc (`docs/design/`, `docs/research/`, `docs/README.md`) is completed and `go test ./...` passes, automatically commit and run `git push origin main` (or `git push -u origin hq/<task>`) before ending the turn so `git status -s` is always clean.
4. **CI & PR Triage:**
   - Inside the target worktree, run `gh pr status` and `gh pr checks` to inspect open PRs and failing GitHub Actions runs.
   - Stage any third-party PR merge or release action via `./bin/hq propose --type=pr_merge --summary="..." --cmd="gh pr merge ..."`.
