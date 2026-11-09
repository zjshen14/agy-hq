# 06 — Task Board, Console Layout & Conversation Lifecycle Audit

- **Date:** 2026-11-08
- **Author:** `[Task:taskboard] taskboard-redesign` (`gemini-4-argon-high`)
- **Related RFC:** [RFC-0009: Scalable Task Board, Tiered Console Layout & Hot/Cold Lifecycle Redesign](../design/0009-task-board-redesign.md)

---

## 1. Executive Summary

Within the first 24 hours of live multi-agent operation in `agy-hq`, `local/memory/tasks.json` accumulated **23 tracked conversations** (6 top-level `[Task:<scope>]` sidebar threads and 17 ephemeral `[Subagent:<scope>]` background runs). While the flat JSON slice and single Markdown table in `local/CONSOLE.md` / `hq_console.md` worked well for `< 5` concurrent tasks during initial bootstrap, auditing the live Vault state and hook pipeline revealed **six structural scaling bottlenecks** that degrade both human UX (on Desktop and Mobile Remote Control) and LLM context efficiency (`SessionStart`).

---

## 2. Empirical Audit of Current `tasks.json` & Console State

### 2.1 Live Vault Snapshot Breakdown (`2026-11-08T16:45-08:00`)

| Category | Count | Share of `tasks.json` | Lifecycle Characteristic |
| :--- | :---: | :---: | :--- |
| **Active Top-Level `[Task:<scope>]` Threads** | `3` | `13.0%` | Multi-turn co-pilot design/coding sessions in sidebar (`brand-icon-design`, `gchat-integration-rfc`, `taskboard-redesign`). |
| **Active `[Subagent:<scope>]` Workers** | `1` | `4.3%` | Short-lived background run (`subagent-agy-hq-pin-suggestion-ux-enhancer`, ~2–5 mins). |
| **Completed Top-Level `[Task:<scope>]` Threads** | `2` | `8.7%` | Finished RFC/audit threads (`agy-hq-gmail-rfc`, `agy-hq-init-audit`) waiting for 24h grace period. |
| **Completed / Cancelled `[Subagent:<scope>]` Workers** | `16` | `69.6%` | Single-turn fire-and-report runs (median duration `92 seconds`) that already reported back to their parent conversation. |
| **Archived `[Task:<scope>]` Threads** | `1` | `4.3%` | Worktree cleaned up (`agy-hq-memory-upgrade`), preserved in `tasks.json`. |
| **Total Tracked Entries** | **`23`** | **`100%`** | Projected 30-day volume at current pace: **500–700 entries**. |

---

## 3. Six Root-Cause Architectural Bottlenecks

### Bottleneck 1: Chronological Append Inversion (`Oldest-First` Rendering)
- **Code Location:** [`internal/kernel/kernel.go`](../../internal/kernel/kernel.go) (`RegisterTask` L196–282) and [`internal/webui/webui.go`](../../internal/webui/webui.go) (`SyncMarkdownConsole` L396–427).
- **Mechanism:** `RegisterTask` appends newly created tasks to the tail of the `tasks.json` slice. `SyncMarkdownConsole` iterates `for _, t := range visibleTasks` in slice index order (`0 .. len-1`) without sorting by status or `LastUpdatedAt`.
- **Impact:** The oldest completed subagents from yesterday sit at the top of `hq_console.md` (rows 1–14), while the user's **active tasks are buried at the very bottom** of a 22-row table. On Mobile Remote Control, the user must scroll past 18 finished subagents to find active work.

### Bottleneck 2: Conflation of Persistent Sidebar Tasks with Ephemeral Subagents
- **Code Location:** [`internal/kernel/kernel.go`](../../internal/kernel/kernel.go) (`ActiveTask` L118–130), [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go) (`HandleSessionStart` L196–206, `HandleStop` L747–795).
- **Mechanism:** `ActiveTask` has no explicit `Kind` (`task` vs. `subagent` vs. `auto`) or `ParentConversationID` field. Every `invoke_subagent` call creates a top-level peer row in `tasks.json`.
- **Impact:**
  1. **70%+ Noise Ratio:** Ephemeral 90-second helper subagents (`Calendar Event Creator`, `Schedule Slot Finder`) visually drown out multi-day engineering and investment initiatives.
  2. **Lost Parent-Child Lineage:** When a dedicated task thread (`brand-icon-design`, `tk000005`) spawns a nested child subagent (`subagent-brand-opus-5-5-creative-director`, `sa000005`), `tasks.json` does not record `parentConversationId: "tk000005..."`, making it impossible to group subagent runs under their parent task.

### Bottleneck 3: Missing `Idle` & `Stalled` Lifecycle States
- **Code Location:** [`internal/kernel/kernel.go`](../../internal/kernel/kernel.go) (`ActiveTask.Status` L129), [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go) (`HandleStop` L770–795).
- **Mechanism:** While subagents automatically transition `active -> completed` on their first `Stop` hook, top-level `[Task:<scope>]` conversations stay `active` indefinitely until someone manually runs `./bin/hq complete <slug>`.
- **Impact:** A sidebar task conversation that has been untouched for 12 hours or 4 days still displays `🟢 active` alongside a conversation that is actively executing tools right now. Conversely, if a background subagent crashes or stalls before firing `Stop`, it remains `🟢 active` forever with no `⚠️ stalled` warning.

### Bottleneck 4: Unbounded `O(N)` Context Bloat in `SessionStart`
- **Code Location:** [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go) (`HandleSessionStart` L284–291).
- **Mechanism:** On every turn of every conversation, `HandleSessionStart` loops over **all** entries in `kernel.ListTasks(vaultDir)`—including `completed`, `cancelled`, and `archived` tasks—and injects them into `<SYSTEM_MESSAGE>`.
- **Impact:**
  - At 23 tasks, `## Tracked Task Conversations` injects ~700 tokens on every turn.
  - At 300 tasks (2 weeks of normal usage), `SessionStart` will inject **~9,000 tokens of dead subagent history on every turn**, violating [RFC-0006](../design/0006-memory-engine-upgrade.md)'s Layer 1 `< 1,500 token` budget.

### Bottleneck 5: Single-File Storage Without Cold Rotation (`tasks.json`)
- **Code Location:** [`internal/kernel/kernel.go`](../../internal/kernel/kernel.go) (`ListTasks`, `RegisterTask`, `ArchiveTask`).
- **Mechanism:** Every task and subagent ever created stays in `local/memory/tasks.json` forever. `ArchiveTask` only sets `"status": "archived"` in-place.
- **Impact:** Every `PreToolUse`, `SessionStart`, `Stop`, and `SyncMarkdownConsole` invocation reads, parses, and re-marshals the entire historical array. Without rolling compaction into an append-only cold ledger (`local/memory/tasks_archive.jsonl`), `tasks.json` grows without bound.

### Bottleneck 6: Missing CLI Views (`./bin/hq tasks`), Missing WebUI Section & `hq spawn` UX Glitches
1. **No `./bin/hq tasks` Subcommand:** Users and agents have `hq complete`, `hq archive`, and `hq reopen`, but no `./bin/hq tasks` CLI to list, filter (`--active`, `--kind=task`, `--scope=...`, `--archive`), search, or compact tasks from the terminal.
2. **Missing Task Board in Local WebUI (`http://127.0.0.1:4747`):** `internal/webui/webui.go` loads `Tasks` into `DashboardState` (L126), but `pageTmpl` (L250–325) never renders `{{.Tasks}}`.
3. **Unescaped Backtick Stripping in `hq spawn --prompt="..."`:** When an agent spawns a task via bash `run_command` using double quotes (`./bin/hq spawn --task=... --prompt="... \`hq_console.md\` ..."`), bash evaluates backticked code spans as subshell commands (`hq_console.md: command not found`), silently erasing inline code spans from the child prompt and `LastSummary`.
