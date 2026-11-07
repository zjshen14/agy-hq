# PRD-0002: Starter Pack — Side-Project SWE (`packs/swe`)

- **Status:** Approved
- **Pack Path:** `packs/swe/` (Open-Source Spec) · `local/packs/swe/` (Private Vault State)
- **Related Docs:** [RFC-0001 (Pack Isolation)](../design/0001-vault-and-pack-isolation.md), [RFC-0003 (Approval Execution)](../design/0003-approval-bound-execution.md)

---

## 1. Problem Statement

Side projects stall because context evaporates between weekends, CI failures go unnoticed, dependencies drift, and small feature ideas sit in scattered notes instead of becoming tested PRs.

---

## 2. Capabilities & Workflows

1. **Multi-Repo Registry (`local/packs/swe/projects.yaml`):**
   - Tracks your active repositories, local filesystem paths, default branches, and test commands.
2. **Proactive Repo Pulse (`swe-project-pulse` Skill):**
   - Runs a lightweight 0-token pre-check (`gh pr list`, `gh run list`) to spot failing CI checks, stale branches, or Dependabot alerts across registered repos.
   - Summarizes actionable items into the Morning Brief.
3. **Spec-to-Branch Builder (`swe-dispatch` Skill):**
   - Given a short feature note or bug report (from chat or dropped into `local/inbox/`), spawns an agent in an **isolated git worktree** (`Workspace: "share"` or `"branch"`), implements the change, runs the test suite, and stages a PR proposal via `hq propose`.

---

## 3. Guardrails & Isolation Rules

- **Single Writer per Worktree:** Following empirical multi-agent coding findings, parallel subagents are **read-only researchers**; at most one writer operates in an isolated git worktree so two tasks never collide on the same working tree.
- **Approval-Gated Push & Merge:** Direct `git push` to default branches and `gh pr merge` are blocked by `hq hook pre-tool` and require explicit approval via `hq propose`.
