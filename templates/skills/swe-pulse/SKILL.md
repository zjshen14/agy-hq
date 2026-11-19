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

0. **Extension Dispatch & Overlay Check:** Run `hq dispatch resolve --skill=swe-pulse --json`; if `overridden == true`, activate the resolved companion skill (`skill_path`) and stop following this file; if `has_overlay == true` with non-empty `overlay_rules`, apply those active personal rules for this run.
1. **Create or Inspect Isolated Git Worktrees:**
   - To start a coding task on any repository without dirtying its primary checkout:
     ```sh
     hq worktree add <repo-path-or-github-slug> <task-name>
     ```
   - To list all active worktrees:
     ```sh
     hq worktree list
     ```
2. **Parallel Subagent & Task Delegation:**
   - When spawning a dedicated task conversation or delegating to a subagent for multi-file edits or test runs, always pass `--repo=<path-or-name>` so work runs inside an isolated git worktree under `local/worktrees/<repo>-<task>/`.
   - For repositories served by a trusted workspace provider pack (non-git workspace systems; see `hq workspace providers`), pass `--workspace=<provider-fqid>` instead (or `<provider-fqid>:<workspace-id>` to attach an existing one); the task prompt then starts with an `[hq-workspace ...]` header naming the work directory. Track them with `hq workspace list`, and stage forced removals through `hq propose` (never run them directly).
3. **Verification & Clean Worktree Gate:**
   - Inside the target worktree, run the repository's configured build, lint, and test checks until green and verify `git status -s` is clean before concluding.
4. **CI & PR Triage:**
   - Inside the target worktree, run `gh pr status` and `gh pr checks` to inspect open PRs and failing GitHub Actions runs.
   - Stage any third-party PR merge, push, or release action via `hq propose --type=pr_merge --summary="..." --cmd="gh pr merge ..."` unless repository policy exempts automated pushes.
