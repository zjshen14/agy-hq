# RFC-0001: Two-Layer Vault Split & Per-Task Parallel Isolation

- **Status:** Approved
- **Related Docs:** [ADR-0003](../decisions/0003-two-layer-oss-and-private-vault.md), [ADR-0006](../decisions/0006-general-agent-and-skills-over-packs.md), [architecture.md](../architecture.md)

---

## 1. Objectives

1. **Open-Source Safe:** Strictly separate the public `agy-hq` engine and starter skills from the user's private `local/` Vault.
2. **Unrestricted Generality:** Allow the agent to handle *any* open-ended task without requiring pre-registered domain packs.
3. **True Parallel Isolation:** Allow any number of concurrent tasks (of the same or different types) to execute in parallel without file collisions, git conflicts, or context leaks.

---

## 2. Two-Layer Filesystem Contract

- **Layer 1 (`agy-hq` Root Repo — Public-Safe):** Contains `cmd/hq/`, `internal/`, `.agents/hooks.json`, `.agents/skills/*/SKILL.md`, and `templates/`.
- **Layer 2 (`local/` or `$HQ_VAULT_DIR` — Private Git Repo):** Ignored by the root `.gitignore` and guarded by `.git/hooks/pre-commit`. Initialized via `bin/hq init` with its own private `.git` repository (whose own `local/.gitignore` excludes `worktrees/` and `repos/`):
  - `local/USER.md` & `local/MEMORY.md` — Core user profile and consolidated memory.
  - `local/config.yaml` & `local/triggers.yaml` — Channel settings, quota budgets, declarative cron schedules, 0-token Go pre-checks, and `local/inbox/` file-drop routing rules.
  - `local/inbox/` — Drag-and-drop folder watched via `fsnotify`.
  - `local/notes/` — Open-ended folder for non-repo work (blog posts, research memos, investment theses, trip plans, scratch scripts).
  - `local/worktrees/<repo>-<task>/` & `local/repos/` — Isolated `git worktree` checkouts of any local or remote GitHub repository so `headquarter` serves as your single entry point on Antigravity Desktop and Remote Control while running parallel engineering tasks inside its workspace sandbox.
  - `local/private/` — High-PII files (`ledger/`, `taxes/`) protected by stricter context guardrails in `hq hook pre-tool`.
  - `local/memory/` — `facts.jsonl`, `topics/*.md`, `journal/`, `pending/`, `outbox/`, `proposals/`.

---

## 3. Declarative Triggers (`local/triggers.yaml`)

Instead of scattering schedules across domain packs, all proactive triggers live in one flat, user-editable `local/triggers.yaml` file:

```yaml
concurrency:
  max_parallel_runs: 4
  max_daily_spawns: 15
  default_model: flash

schedules:
  - id: morning-brief
    cron: "CRON_TZ=America/Los_Angeles 30 7 * * *"
    skill: morning-brief
  - id: edgar-close-watch
    cron: "CRON_TZ=America/Los_Angeles 30 13 * * 1-5"
    precheck_cmd: ["hq", "check-rss", "--watchlist", "local/workspace/invest/watchlist.yaml"]
    skill: thesis-check
  - id: sleep-consolidate
    cron: "CRON_TZ=America/Los_Angeles 30 23 * * *"
    skill: sleep-consolidate

inbox_rules:
  - name: bank-statements
    extensions: [".csv", ".ofx", ".qfx"]
    skill: ledger-reconcile
  - name: tax-forms
    extensions: [".pdf"]
    filename_regex: "(?i)(w-?2|1099|1098|k-?1|tax)"
    skill: tax-prep
  - name: sec-filings
    extensions: [".pdf", ".htm", ".html"]
    filename_regex: "(?i)(10-[kq]|8-k|earnings)"
    skill: thesis-check
```
*(Any file dropped into `local/inbox/` that does not match a specific rule is routed to a general triage prompt so the agent can inspect and handle it.)*

---

## 4. Four Mechanisms for Parallel Task Isolation

Because you may run two coding tasks, an investment research task, and a blog post draft at the exact same time, isolation is enforced **per task (`conversationId`)** rather than per domain:

1. **Independent Conversation Contexts:**
   - Every task runs as its own Antigravity conversation trajectory (`conversationId`) with an isolated context window.
2. **Lock-Free, Immutable Per-Conversation Files (Zero Race Conditions):**
   - Parallel sessions **never edit shared memory or queue files in place**:
     - `hq hook stop` writes turn logs to `local/memory/journal/YYYY-MM-DD/<convId>-<stepIdx>.json`.
     - `hq propose` writes staged approvals to `local/memory/pending/<uuid>.json`.
     - `hq notify` writes outbound cards to `local/memory/outbox/<uuid>.json`.
   - Only the single-writer **Nightly Consolidation** job merges the day's immutable journal files into `local/MEMORY.md` and `local/memory/topics/*.md`.
3. **Single-Entry-Point Git Worktree Isolation (`local/worktrees/<repo>-<task>/`):**
   - `headquarter` acts as your **single main entry point** in Antigravity Desktop and Remote Control across all your engineering projects.
   - Whenever an agent or subagent modifies a git repository (whether already cloned in `~/Workspace/<repo>` or newly cloned into `local/repos/<repo>.git`), `hq worktree add <repo> <task>` checks out an isolated `git worktree` at `local/worktrees/<repo>-<task>/`.
   - Because each worktree sits inside `headquarter/local/worktrees/`, it stays inside Antigravity's active workspace sandbox boundary, while Git and `gh` automatically bind to the worktree's own `.git` and GitHub `origin` remote—allowing multiple subagents to work on the same or completely different GitHub repositories in parallel without switching Antigravity projects.
4. **High-PII Vault Protection (`local/private/`):**
   - While `local/notes/` and `local/worktrees/` are open to any task, `hq hook pre-tool` blocks access to `local/private/` (tax returns, raw ledger files) unless the task was explicitly invoked for accounting/tax work, preventing accidental PII leakage during general browsing or writing tasks.
