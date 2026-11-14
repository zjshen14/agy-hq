# 12 — Antigravity Sidebar Conversation Archive Synchronization Audit

- **Date:** 2026-11-14
- **Companion Technical Design:** [RFC-0016: Antigravity Sidebar Conversation Archive Synchronization](../design/0016-sidebar-conversation-archive-sync.md)
- **Prior Related Audits:** [04 — Google Antigravity Capability Audit](04_antigravity_audit.md) · [06 — Task Board, Console Layout & Conversation Lifecycle Audit](06_taskboard_and_console_audit.md) · [11 — Antigravity Global Plugin Packaging & Vault Isolation Audit](11_plugin_packaging_audit.md)

---

## 1. Problem Statement & Observed UX Gap

Following [RFC-0009](../design/0009-task-board-redesign.md), `agy-hq` cleanly manages task lifecycle inside the private Vault across hot (`<vault>/memory/tasks.json`) and cold (`<vault>/memory/tasks_archive.jsonl`) storage:

1. **Manual Archival (`hq archive <slug>` / WebUI `POST /api/tasks/archive`):** Removes merged Git worktrees and marks the task `status: "archived"` in `<vault>/memory/tasks.json`.
2. **Automated Compaction (`hq tasks compact` / `hq memory compact` / `/sleep-consolidate` Stage 4):** Auto-archives unpinned `completed`/`cancelled` tasks (`24h` TTL for `kind: "task"`, immediate `0h` rotation for `kind: "subagent"` and `kind: "auto"`) and rotates archived entries out of `<vault>/memory/tasks.json` into `<vault>/memory/tasks_archive.jsonl`.
3. **Cold Recall (`hq reopen <slug>`):** Restores an archived task from cold JSONL back into hot `<vault>/memory/tasks.json` and recreates its worktree if a `hq/<slug>` branch exists.

However, **none of these transitions propagate to Google Antigravity's own conversation metadata**. Every top-level `[Task:<scope>]` conversation spawned via `hq spawn` (`agentapi new-conversation`) and every scheduled `[Auto]` triage/brief worker remains **unarchived in the Antigravity left sidebar** indefinitely unless the user manually hovers and clicks Archive on each conversation in the UI. Over days of autonomous operation, dozens of already-compacted tasks accumulate in the sidebar alongside the pinned `[HQ] Front Desk` and genuinely active threads.

---

## 2. Empirical Surface Audit: `agy` CLI, `agentapi`, and `LanguageServerService`

We audited all three sanctioned Antigravity programmatic surfaces to determine whether conversation archival, hiding, or deletion is exposed cleanly without touching internal database/state files on disk.

### 2.1 `agy` CLI (`v1.5.8`) & `agentapi` CLI

| Surface | Inspected Commands | Archive / Hide / Delete Support | Notes |
|---|---|---|---|
| `agy` CLI (`1.5.8`) | `acp`, `agent`/`agents`, `changelog`, `help`, `install`, `mcp`, `mic-serve`, `models`, `plugin`/`plugins`, `remote-control`, `update` | **None (Headless CLI)** | Interactive session archiving/deletion exists only inside the TUI `/resume` picker (`~/.gemini/antigravity-cli/`), which is also storage-isolated from Antigravity Desktop App 2.0 (`~/.gemini/antigravity/`). |
| `agentapi` CLI | `get-conversation-metadata`, `new-conversation`, `send-message` | **None** | Exposes only 3 subcommands; `get-conversation-metadata <id>` is read-only. |

### 2.2 `LanguageServerService` ConnectRPC (`buf.build/agy-protos/agy-protos`)

`agy-hq` already communicates with Antigravity's local `LanguageServerService` over ConnectRPC HTTP/JSON in [kernel.go](../../internal/kernel/kernel.go) (`GetCascadeModelConfigData`, `StartCascade`, `SendUserCascadeMessage`) using the sanctioned environment variables `ANTIGRAVITY_LS_ADDRESS` and `ANTIGRAVITY_CSRF_TOKEN`.

Inspecting all 375 RPC methods and message descriptors in `exa.language_server_pb.LanguageServerService` (`language_server.proto`, `jetski_cortex.proto`, and `cortex.proto`) revealed three relevant RPC families:

| RPC Method | Request / Key Fields | Behavior | Suitability for `agy-hq` |
|---|---|---|---|
| **`UpdateConversationAnnotations`** | `UpdateConversationAnnotationsRequest` (`cascadeId`, `cascadeIds`, `annotations`, `mergeAnnotations`) | Non-destructively updates `ConversationAnnotations` (`archived`, `archivalStatusTimestamp`, `pinned`, `starred`, `title`, etc.) on one or many conversations and pushes live reactive updates to the sidebar UI (`StreamCascadeSummariesReactiveUpdates`). | **Selected (Primary Write API)** — exact same RPC invoked when clicking Archive/Unarchive in the Antigravity sidebar. |
| **`GetAllCascadeTrajectories`** | `GetAllCascadeTrajectoriesRequest` (`excludeSubtrajectories: true`) | Returns `map<string, CascadeTrajectorySummary>` (`trajectorySummaries`) with live `status` (`CascadeRunStatus`), `annotations` (`archived`, `pinned`, `starred`, `title`), `notFullyIdle`, `hasActiveChildren`, `hasRunningSubagents`, `backgroundTaskCount`, and `waitingSteps`. | **Selected (Primary Pre-Flight Safety & Backfill Read API)** — inspects live UI pin/star/running state in 1 unary call. |
| `DeleteCascadeTrajectory` / `JetboxDeleteSummary` | `DeleteCascadeTrajectoryRequest` (`cascadeId`) | Permanently deletes trajectory steps and summary state. | **Rejected (Destructive)** — breaks `conversation://<id>` links, `hq reopen` cold recall, `hq obs` token attribution, and `hq memory verify-transcript` ground-truth auditing. |

*(Note: `exa.cortex_pb.SourceMetadata` also defines `hideFromConversationList` (`field 3`) for internal creation-time subtrajectories, which explains why `invoke_subagent` / `StartCascade(parentCascadeId != "")` child subagents do not clutter the top-level sidebar, whereas top-level `agentapi new-conversation` threads require `UpdateConversationAnnotations` when completed/archived.)*

---

## 3. Wire Schema & Protobuf Presence Semantics

### 3.1 `UpdateConversationAnnotationsRequest` (`language_server.proto`, `syntax = "proto3"`)

```protobuf
message UpdateConversationAnnotationsRequest {
  string cascade_id = 1; // json = "cascadeId"
  exa.jetski_cortex_pb.ConversationAnnotations annotations = 2; // json = "annotations"
  bool merge_annotations = 3; // json = "mergeAnnotations"
  repeated string cascade_ids = 4; // json = "cascadeIds"
}
```

- **Single vs. Batch Targeting:** `cascadeId` updates a single conversation; `cascadeIds` (`repeated string`) updates a batch of conversations atomically in one RPC call.
- **Partial Merge (`mergeAnnotations: true`):** Preserves existing `title`, `tags`, `pinned`, `starred`, `groupId`, `permissionPreset`, and `coordinator` fields on the conversation while mutating only explicitly populated fields on `annotations`.

### 3.2 `ConversationAnnotations` (`jetski_cortex.proto`, `syntax = "proto3"` with `proto3_optional`)

Crucially, descriptor inspection of `jetski_cortex.proto` confirms that scalar fields on `ConversationAnnotations` are declared `optional` (`proto3_optional = true` backed by synthetic oneofs `_title`, `_archived`, `_starred`, `_marked_as_unread`, `_pinned`, etc.):

```protobuf
message ConversationAnnotations {
  optional string title = 1; // json = "title", oneof _title
  repeated string tags = 3; // json = "tags"
  optional bool archived = 4; // json = "archived", oneof _archived
  google.protobuf.Timestamp archival_status_timestamp = 5; // json = "archivalStatusTimestamp"
  optional bool starred = 6; // json = "starred", oneof _starred
  google.protobuf.Timestamp last_user_view_time = 7; // json = "lastUserViewTime"
  optional bool marked_as_unread = 8; // json = "markedAsUnread", oneof _marked_as_unread
  optional bool pinned = 12; // json = "pinned", oneof _pinned
  // ...
}
```

Two critical implementation details follow directly from this schema and `agy changelog` history:

1. **Go Pointer Serialization for `archived: false` (`hq reopen`):** Because `archived` uses `proto3_optional` (`oneof _archived`), `mergeAnnotations: true` honors explicit `"archived": false` on the wire to unarchive a thread. However, in Go's `encoding/json`, a plain `bool` field tagged `json:"archived,omitempty"` omits `false`, leaving `_archived` unset! Therefore, `agy-hq`'s request struct **must** use `*bool` (`Archived *bool `json:"archived,omitempty"``) so unarchiving serializes `"archived": false` explicitly while omitting `"archived"` when `nil`.
2. **Mandatory `archivalStatusTimestamp`:** Antigravity's changelog explicitly notes a prior UI bug when `archival_status_timestamp` was omitted during archival (*"Fixed a bug where the archival status timestamp was not correctly saved when archiving conversations"*). Every archive and unarchive call must populate `"archivalStatusTimestamp"` with the current UTC RFC3339 timestamp (`time.Now().UTC().Format(time.RFC3339Nano)`).

### 3.3 `CascadeTrajectorySummary` & `CascadeRunStatus` (`GetAllCascadeTrajectories`)

`GetAllCascadeTrajectories` (`{"excludeSubtrajectories": true}`) returns `trajectorySummaries` keyed by `cascadeId`. Each summary exposes:

- `status` (`exa.cortex_pb.CascadeRunStatus`):
  - `CASCADE_RUN_STATUS_UNSPECIFIED` (`0`)
  - `CASCADE_RUN_STATUS_IDLE` (`1`)
  - `CASCADE_RUN_STATUS_RUNNING` (`2`)
  - `CASCADE_RUN_STATUS_CANCELING` (`3`)
  - `CASCADE_RUN_STATUS_BUSY` (`4`)
- Live activity flags: `notFullyIdle` (`bool`), `hasActiveChildren` (`bool`), `hasRunningSubagents` (`bool`), `backgroundTaskCount` (`uint32`), and `waitingSteps` (`repeated CortexTrajectoryStepWithIndex`).
- Live UI annotations: `annotations.archived` (`bool`), `annotations.pinned` (`bool`), `annotations.starred` (`bool`), and `annotations.title` (`string`).

---

## 4. Credential & Environment Availability Across Execution Contexts

We audited where `ANTIGRAVITY_LS_ADDRESS` and `ANTIGRAVITY_CSRF_TOKEN` are present at runtime:

| Execution Context | `ANTIGRAVITY_LS_ADDRESS` & `ANTIGRAVITY_CSRF_TOKEN` | Behavior Needed |
|---|---|---|
| Agent `run_command` tool (`[HQ] Front Desk`, `[Task:*]`, subagents, `/sleep-consolidate`) | **Present** (injected by Antigravity executor) | Immediate synchronous ConnectRPC call (`<= 1.5s` timeout); queue on transient failure. |
| Lifecycle Hooks (`SessionStart`, `PreToolUse`, `Stop`) | **Present** (inherited by `hq hook` process) | `Stop` hook can perform a `0ms` `os.Stat` check on `<vault>/memory/sidebar_sync_queue.json` and opportunistically flush due items if non-empty. |
| `hq daemon` / WebUI `:4242` spawned by `agy-hq-console` sidecar ([main.mjs](../../templates/sidecars/agy-hq-console/main.mjs)) | **Present** (`sanitizedDaemonEnv()` preserves all `ANTIGRAVITY_*` env vars) | WebUI `POST /api/tasks/archive` and `RunHeartbeat` sync directly; if LS restarts on a new port, sidecar restarts or queue retries on next agent/hook invocation. |
| External User Shell / SSH / Standalone `systemd` `hq daemon` | **Absent** (or stale if Desktop App is closed) | **Must never fail local Vault operations.** `hq archive`, `hq reopen`, and `hq tasks compact` succeed locally and enqueue the desired sidebar state into `<vault>/memory/sidebar_sync_queue.json` for automatic drain on the next LS-connected invocation. |

---

## 5. `agy-hq` Task Lifecycle Touchpoints & Historical Backfill Scope

Auditing [kernel.go](../../internal/kernel/kernel.go), [main.go](../../cmd/hq/main.go), [webui.go](../../internal/webui/webui.go), and [SKILL.md](../../.agents/plugins/agy-hq/skills/sleep-consolidate/SKILL.md) identified every code path that transitions task archival state:

1. **`kernel.ArchiveTask(vaultDir, target, force)`:** Called by `hq archive <target>` ([main.go](../../cmd/hq/main.go)) and `POST /api/tasks/archive` ([webui.go](../../internal/webui/webui.go)). Currently removes the Git worktree and sets `Status = "archived"`, `ArchivedAt = now`. Needs immediate `archived: true` sidebar sync (respecting Front Desk / UI-pinned guards unless `--no-sidebar` is passed).
2. **`kernel.ReopenTask(vaultDir, target, recreateWorktree)`:** Called by `hq reopen <target>`. Restores task to `Status = "active"` in `<vault>/memory/tasks.json`. Needs reverse `archived: false` sidebar sync (and queue supersession) so reopened conversations immediately return to the active sidebar.
3. **`kernel.CompactTasks(vaultDir, opts)`:** Called by `hq tasks compact [--dry-run]`, `hq memory compact`, and `/sleep-consolidate` Stage 4. Auto-archives stale completed `task` (`24h`), `subagent` (`0h`), and `auto` (`0h`) entries and rotates `archived` tasks to `<vault>/memory/tasks_archive.jsonl`. When `!opts.DryRun`, needs to sync all newly archived/rotated `ConversationID`s and drain any due entries in `<vault>/memory/sidebar_sync_queue.json` — giving `/sleep-consolidate` automatic nightly sidebar cleanup in **zero extra LLM tool calls**.
4. **Historical Backfill (`hq tasks sync-sidebar [--dry-run] [--backfill]`):** Existing Vaults already contain dozens of historical tasks in `<vault>/memory/tasks_archive.jsonl` (and `status: "archived"` entries in `<vault>/memory/tasks.json`) that were compacted before sidebar sync existed. A dedicated, idempotent reconciliation command must cross-reference those historical UUIDs against `GetAllCascadeTrajectories`, enforce strict multi-layer safety checks (never touching `[HQ] Front Desk`, Vault-pinned or UI-pinned/starred threads, active/idle/stalled Vault tasks, or live running trajectories), support `--dry-run` preview, and batch `UpdateConversationAnnotations` cleanly.

---

## 6. Key Architectural Conclusions for RFC-0016

1. **Sanctioned ConnectRPC Only:** Use `LanguageServerService/GetAllCascadeTrajectories` and `LanguageServerService/UpdateConversationAnnotations` (`mergeAnnotations: true`, `archived: *bool`, `archivalStatusTimestamp: RFC3339Nano`) over `$ANTIGRAVITY_LS_ADDRESS` + `$ANTIGRAVITY_CSRF_TOKEN`. Never mutate internal `~/.gemini/antigravity/` state files directly and never call `DeleteCascadeTrajectory`.
2. **Local-First Non-Blocking Resilience:** Keep Vault state transitions (`tasks.json` / `tasks_archive.jsonl`) authoritative and instantaneous. Enforce short HTTP timeouts (`1.5s` unary / `3s` batch) and persist offline/transient failures in a deduplicated `<vault>/memory/sidebar_sync_queue.json` with bounded exponential backoff and terminal `404`/`NotFound` pruning.
3. **Defense-in-Depth Safety Invariants:** Combine local Vault invariants (`front_desk.json`, `ActiveTask.Pinned`, `ActiveTask.Status`, caller `$ANTIGRAVITY_CONVERSATION_ID`) with live `GetAllCascadeTrajectories` UI state (`annotations.pinned`, `annotations.starred`, `annotations.title`, `CascadeRunStatus`, `notFullyIdle`, `hasActiveChildren`, `hasRunningSubagents`, `backgroundTaskCount`, `waitingSteps`) so active, Front Desk, or pinned threads can never be accidentally archived.
