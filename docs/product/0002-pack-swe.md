# PRD-0002: Starter Pack — Software Engineering (`packs/swe`)

- **Status:** Approved
- **Pack Path:** `packs/swe/` (Open-Source Spec) · `local/packs/swe/` (Private Vault State)
- **Related Docs:** [RFC-0001 (Pack Isolation)](../design/0001-vault-and-pack-isolation.md), [RFC-0003 (Approval Execution)](../design/0003-approval-bound-execution.md)

---

## 1. Purpose

Provide a general-purpose **Software Engineering (`swe`)** pack that works across any codebase you maintain or contribute to—whether open-source libraries, production services, or new prototypes—automating repository health monitoring, CI failure diagnosis, and spec-to-PR implementation.

---

## 2. Core Primitives

| Dimension | `packs/swe` Implementation |
|---|---|
| **State Artifacts (`local/packs/swe/`)** | `projects.yaml` (registered repos, branches, test commands), `specs/*.md`, and isolated `git worktree` checkouts |
| **Event Triggers** | Failing CI runs (`gh run list`), open PR review activity (`gh pr list`), Dependabot alerts, or spec/bug files dropped in `local/inbox/` |
| **Execution Pattern** | **Single-Writer Worktree Loop:** Read-only explorer subagents gather context; a single writer operates inside an isolated `git worktree` (`Workspace: "share"` or `"branch"`) |
| **Verification Oracle** | Deterministic build, linter, and test suite execution (`go test`, `pytest`, `npm test`) before any diff is surfaced |
| **Security Boundary** | Scoped to registered repository paths and `local/packs/swe/`; zero access to personal email, financial ledgers, or tax records in `packs/admin` |
| **Approval Gates (`hq propose`)** | Opening/merging PRs (`gh pr create`, `gh pr merge`) and pushing branches (`git push`) |

---

## 3. Included Skills

1. **`swe-project-pulse`:** Runs a 0-token pre-check across all repos in `local/packs/swe/projects.yaml` to detect broken builds, stalled PRs, or review requests, summarizing only actionable items into the Morning Brief.
2. **`swe-dispatch`:** Takes a feature spec or bug description, creates an isolated git worktree, implements and tests the change until the repo's test suite passes, and stages the branch/PR via `hq propose`.
