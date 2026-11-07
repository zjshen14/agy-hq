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
2. **Parallel Subagent Delegation:**
   - When running multiple coding tasks in parallel, assign each subagent (`invoke_subagent`) its own worktree directory under `local/worktrees/<repo>-<task>/`.
3. **CI & PR Triage:**
   - Inside the target worktree, run `gh pr status` and `gh pr checks` to inspect open PRs and failing GitHub Actions runs.
   - Stage any PR merge or release action via `./bin/hq propose --type=pr_merge --summary="..." --cmd="gh pr merge ..."`.
