# Research 20: Git-Worktree Coupling Audit, Non-Git Workspace Requirements & Pluggable Workspace Provider Design Space

- **Status:** Complete
- **Date:** 2026-11-19
- **Companion RFC:** [RFC-0025: Pluggable Workspace Providers](../design/0025-pluggable-workspace-providers.md)
- **Prior Specs Reviewed:** [RFC-0001](../design/0001-vault-and-task-isolation.md), [RFC-0007](../design/0007-front-desk-initialization-hardening.md), [RFC-0009](../design/0009-task-board-redesign.md), [RFC-0015](../design/0015-global-plugin-packaging.md), [RFC-0016](../design/0016-sidebar-conversation-archive-sync.md), [RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md), [RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md), [RFC-0021](../design/0021-customizable-approval-policy-and-ux.md), [RFC-0023](../design/0023-front-desk-channel-bridge.md)

---

## 1. Executive Summary & Audit Scope

`agy-hq` isolates parallel engineering work in **git worktrees** under `<vault>/worktrees/<repo>-<task>/` ([RFC-0001](../design/0001-vault-and-task-isolation.md)). That model is a good fit for open-source repositories, but many engineers spend most of their day in workspace systems that are not git at all:

- **Virtual-filesystem monorepo clients** (a FUSE-mounted client per change, created and deleted by a vendor CLI, often hundreds per user).
- **Centralized VCS clients** (Perforce-style clients/streams where "a change" is a server-side changelist, not a commit).
- **Alternative DVCS front-ends** layered on a monorepo (Jujutsu workspaces, Mercurial shares, Sapling checkouts), where several working copies share one history.

In those systems the unit of isolation is a **workspace** created by the VCS tool itself, the unit of review is a **change** (changelist / revision / pull request), and "is it merged into `main`?" is answered by a review server rather than `git merge-base`.

This audit inspects every place where `agy-hq` hard-codes git-worktree semantics, measures how a non-git workspace currently behaves when a task tries to use one, and derives the requirements (`WR1`–`WR8`) for a vendor-neutral **Workspace Provider** seam.

**Key finding:** git is not only a creation mechanism in `agy-hq`. It is woven into **seven subsystems**: workspace creation, the Ring-0 removal guard, task metadata, the archive/reopen/compact lifecycle, spawn routing (`projectId`), the Front Desk hygiene hooks, and every console surface. A non-git workspace today is invisible to all seven: it is never listed, never tracked on the Task Board, never hygiene-checked, and never cleaned up. Users work around this by keeping every task conversation in the Vault's own project and pasting absolute paths into prompts by hand.

---

## 2. Forensic Codebase Audit: Seven Git-Coupled Subsystems (`C1`–`C7`)

### `C1` — Workspace Creation Is Git-Only (`internal/worktree/`)

- [`worktree.Add`](../../internal/worktree/worktree.go#L14-L91) accepts only (a) a local directory that answers `git rev-parse --show-toplevel`, or (b) a remote URL / GitHub `owner/repo` slug that it `git clone --bare`s into `<vault>/repos/`. It then runs `git worktree add -b hq/<task>`.
- There is no branch for "ask some other tool to make an isolated workspace". A local directory that is not a git repo silently becomes `gitRepoDir` and fails at `git worktree add`.
- [`worktree.List`](../../internal/worktree/worktree.go#L160-L176) is a plain `os.ReadDir(<vault>/worktrees)`. Anything outside that directory is invisible.

### `C2` — The Ring-0 Removal Guard Is Defined by Path, Not Ownership (`internal/kernel/worktree_sync.go`)

- [`IsManagedWorktreeDir`](../../internal/kernel/worktree_sync.go#L46-L56) returns true only for a direct child of `<vault>/worktrees/`. Every destructive path checks it first: [`removeManagedWorktree`](../../internal/kernel/worktree_sync.go#L423-L452), [`validateTaskWorktreesForArchive`](../../internal/kernel/worktree_sync.go#L402-L421), and [`PruneOrphanWorktrees`](../../internal/kernel/worktree_sync.go#L597-L646).
- This is a sound safety property: `agy-hq` can never delete a directory it did not create. But it equates *"created by hq"* with *"lives under `<vault>/worktrees/`"*. Workspace systems that dictate their own mount root (e.g. `/srv/ws/<user>/<name>/`) can never satisfy it, so they can never be cleaned up, even when `agy-hq` created them.
- [`AttachWorktreeToTaskWithOptions`](../../internal/kernel/worktree_sync.go#L185-L259) also returns early when `!IsManagedWorktreeDir`, so a non-git workspace cannot even be *recorded* against a task.

### `C3` — Task Metadata Has Only Git-Shaped Fields (`internal/kernel/kernel.go`)

- [`ActiveTask`](../../internal/kernel/kernel.go#L196-L219) carries `WorktreeDir`, `WorktreeDirs`, and `RepoRoot`. There is no field for *which tool* owns the workspace, its stable identifier, or the changes/reviews it holds.
- [`ManagedWorktreeStatus`](../../internal/kernel/worktree_sync.go#L472-L484) exposes `Branch`, `Clean`, `MergedIntoMain`, and `PushedToOrigin`, all computed by shelling out to `git`. A change that is "in review", "approved, waiting to land", or "landed as a different identifier after server-side rebase" has no representation.

### `C4` — Archive / Reopen / Compact Lifecycle Shells Out to Git

| Lifecycle step | Code | Git assumption |
| :--- | :--- | :--- |
| Dirty check before archive | [`validateTaskWorktreesForArchive`](../../internal/kernel/worktree_sync.go#L402-L421), [`isWorktreeCleanOrAbsent`](../../internal/kernel/kernel.go#L1221-L1235) | `git status --porcelain` |
| Removal on archive | [`removeManagedWorktree`](../../internal/kernel/worktree_sync.go#L423-L452) | `git worktree remove` + `git worktree prune` |
| Restore on reopen | [`restoreWorktreeForReopen`](../../internal/kernel/kernel.go#L1046-L1074) | `git worktree add` from branch `hq/<slug>` |
| Dormant auto-archive | [`CompactTasks`](../../internal/kernel/kernel.go#L1243-L1513) via `areAllTaskWorktreesMergedAndClean` | `git merge-base --is-ancestor` / `git cherry` against `origin/main` |
| Orphan pruning | [`PruneOrphanWorktrees`](../../internal/kernel/worktree_sync.go#L597-L646) | `keep:unmerged` derived from `git` default-branch refs |

For a non-git workspace, every one of these either no-ops (not managed) or would return wrong answers (e.g. `git status` failing would mark the workspace dirty forever).

### `C5` — Spawn Routing Binds Conversations to the Vault's Project (`projectId`)

Antigravity scopes each conversation to a **project** (a `<ConfigDir>/projects/<id>.json` entry listing folder resources). Plugin hooks, project-scoped plugin installs, and agent-API messaging are all project-relative. `agy-hq` therefore takes great care to start every spawned conversation in a *verified* project:

- [`resolveSpawnTarget`](../../internal/kernel/kernel.go#L2974-L3036) derives `(workDir, projectID)` from the Front Desk lease, the cached Language Server session, or `ANTIGRAVITY_PROJECT_ID`, then calls [`ResolveVerifiedWorkspaceProjectID`](../../internal/kernel/project_registry.go#L288-L347) to repair poisoned IDs.
- Managed worktrees work only because of a special case: [`ResolveVerifiedWorkspaceProjectID`](../../internal/kernel/project_registry.go#L298-L306) lets a directory under `<vault>/worktrees/` borrow the **primary Vault project** (`IsManagedWorktreeDir` → `primaryVaultWorkspaceDir`). A worktree is a sub-folder of the Vault project's resource tree.
- [`buildCascadeRequestBody`](../../internal/kernel/kernel.go#L4320-L4367) sends either `projectEnvConfig.projectId` **or** `workspaceUris`, never both (`StartCascade` rejects both with HTTP 400, see the comment at `L4354`).
- [`spawnTaskViaLS`](../../internal/kernel/kernel.go#L3120-L3198) refuses an unverified `agentapi` fallback when no verified `projectId` exists (`TestProjectIDPoisoningRepairAndWorkspaceSafeSpawn` in [`project_and_auto_test.go`](../../internal/kernel/project_and_auto_test.go#L44-L152)).

**Consequence for non-git workspaces:** a workspace at an arbitrary mount root is *not* inside the Vault project. Starting the conversation "in" that workspace means starting it in a **different project**, where:

1. A **project-scoped** `agy-hq` plugin install (`hq plugin install` default, [RFC-0015](../design/0015-global-plugin-packaging.md)) is not loaded, so `SessionStart` / `PreToolUse` / `Stop` hooks never fire. The task is never registered, never journaled, and never hygiene-gated.
2. The agent API rejects cross-project messages unless the caller's source metadata is cleared, which breaks parent ↔ child `send_message` report-backs and `[HQ] Front Desk` delivery.
3. Ad-hoc folders opened directly in Antigravity get their own `isWorkspaceOnly` project entries, so one physical workspace can map to several project IDs over time.

This is the root cause of the operational rule "keep task conversations in the Vault project": it is the only placement where hooks are guaranteed to run.

### `C6` — Front Desk Hygiene Hooks Know Only `<vault>/worktrees/`

- [`shouldAutoClaimVacantFrontDesk`](../../internal/hooks/hooks.go#L133-L175) treats paths inside `<vault>/worktrees/` (and git/language-project markers) as "regular coding sessions" that must not claim the Front Desk lease. When the hook payload carries `workspacePaths`, any non-Vault path correctly returns `false`. But on the cwd fallback path (empty `workspacePaths`, `L141-L161`), a non-git workspace root has none of `.git`, `go.mod`, `package.json`, `Cargo.toml`, or `pyproject.toml` at its top, so the function falls through to `return true` and a conversation started there while the lease is vacant **can claim the Front Desk**.
- [Rule A](../../internal/hooks/hooks.go#L1957-L2008) (no source edits on the Front Desk) works by deny-by-default outside the artifact dir and `<vault>` non-worktree paths, so it already covers external workspaces. That is correct, but by accident rather than by design.
- Nothing prevents **two task conversations from writing into the same workspace**. For DVCS front-ends that auto-snapshot the working copy into the current change, two concurrent writers silently mix their edits into one change. Git worktrees avoid this structurally (one branch per worktree); other systems need an explicit single-writer rule.
- Destructive workspace verbs (delete workspace, purge untracked files, drop a change) are not covered by any default approval rule. [RFC-0021](../design/0021-customizable-approval-policy-and-ux.md) `approval_rules` can express them per pack, but no pack component ties them to the workspace they protect.

### `C7` — Console, WebUI, `SessionStart`, and `hq status` Render Directory Names

| Surface | Code | What it shows |
| :--- | :--- | :--- |
| `SessionStart` Zone 2 | [`hooks.go`](../../internal/hooks/hooks.go#L545-L551) | `## Active Git Worktrees`: basename + path from `worktree.List` |
| `CONSOLE.md` / `hq_console.md` | [`webui.go`](../../internal/webui/webui.go#L2406-L2414) | `## 3. Active Parallel Git Worktrees` |
| Task Board badge | [`webui.go`](../../internal/webui/webui.go#L2382-L2386) | `🌿 wt` / `🌿 N wts` from `ExistingWorktreeDirs` |
| `:4242` WebUI | [`webui.go`](../../internal/webui/webui.go#L1785-L1794), [`DashboardState.Worktrees`](../../internal/webui/webui.go#L69) | `[]string` of paths |
| `hq status` | [`main.go`](../../cmd/hq/main.go#L346-L371) | `Active Worktrees: N` |

None of these surfaces can show *which tool* owns a workspace, whether it holds changes awaiting review, or whether the owning task is still alive.

---

## 3. Empirical Walkthrough: A Task in a Non-Git Workspace Today

The following sequence is traced through the current `main` code paths cited above (a static walkthrough with synthetic names, not a recorded live run):

1. **Front Desk:** `hq spawn --task=ledger-fix --prompt="Fix the rounding bug"` (no `--repo`, because `--repo` requires git). The task conversation starts in the Vault project (`C5`). Good: hooks fire, the task appears on the Task Board.
2. **Task conversation:** the agent runs the vendor CLI to create a client at `/srv/ws/user/ledger-fix/` and edits files there. `Stop` journals `filesModified`, but nothing binds the workspace to the task: auto-binding happens only through `hq worktree add|attach` and nested `SpawnSubagent` ([`main.go`](../../cmd/hq/main.go#L662-L672), [`kernel.go`](../../internal/kernel/kernel.go#L4832-L4858)), and even an explicit `hq worktree attach /srv/ws/user/ledger-fix` is rejected because the path is not under `<vault>/worktrees/` (`C2`). The Task Board row shows no workspace badge (`C7`).
3. **Parallel task:** a second task for a follow-up change is pointed at the same client by mistake. Both agents edit it; with an auto-snapshotting front-end both sets of edits land in one change (`C6`).
4. **Completion:** `hq complete ledger-fix` → after 24h, `CompactTasks` archives the task. Its client is not managed, so it is left behind with uncommitted edits and an open change (`C4`).
5. **Week later:** the user has dozens of orphaned clients. `hq worktree prune` reports `Scanned 0`. No surface lists them (`C7`).

The task did run, but `agy-hq` provided no isolation, tracking, hygiene, or cleanup for its workspace.

---

## 4. Requirements (`WR1`–`WR8`)

| ID | Requirement | Derived From |
| :--- | :--- | :--- |
| **WR1** | **Pluggable creation & discovery.** A companion pack can teach `hq` to create, list, probe, inspect, and remove workspaces of any VCS without patching core or adding Go dependencies (`ADR-0002`, CI Gate `H4-A`). | `C1`, `C7` |
| **WR2** | **Ownership-based safety.** `hq` may only remove workspaces it created (or that the user explicitly handed over), proven by an ownership ledger rather than a directory prefix. Removing a workspace that still holds changes requires human approval. | `C2`, `C4` |
| **WR3** | **Provider-neutral task bindings.** A task can hold several workspace bindings from different providers, each with a stable ID, root path, role (`primary` / `secondary` / `readonly`), and a cached change list. Existing `WorktreeDir` data migrates losslessly. | `C3` |
| **WR4** | **Provider-delegated lifecycle.** Archive, reopen, compact, and prune ask the owning provider "dirty? open changes? all landed?" instead of running `git`. Git stays the built-in provider with today's exact behavior. | `C4` |
| **WR5** | **Hook-preserving placement.** Spawning a task "in" an external workspace must not silently move the conversation to a project where `agy-hq` hooks do not run. The default keeps the conversation in the Vault project and binds the workspace as data; placing it in a foreign project is opt-in and refused unless hooks are proven to load there. | `C5` |
| **WR6** | **Workspace hygiene.** Core enforces (a) at most one writing task per workspace, (b) no Front Desk lease claims from inside a provider-owned workspace, and (c) provider-declared destructive verbs routed through `hq propose`. | `C6` |
| **WR7** | **Change & review surfacing without prompt injection.** The Task Board, console, WebUI, and `SessionStart` show each bound workspace's provider, dirtiness, and change states, from a daemon-refreshed cache (0 subprocesses at render time), exposing only enums, opaque IDs, and sanitized short titles to LLM-visible surfaces. | `C3`, `C7`, [RFC-0017c](../design/0017c-lease-based-morning-brief-and-extensions.md) precedent |
| **WR8** | **Same trust model as every other pack executable.** Provider binaries are SHA-256-pinned in `<vault>/private/trust.json`, executed from `exec-cache` under `connector.RunSubprocess` (scrubbed env, `Setpgid`, timeouts), and never run from tainted sessions. | [RFC-0017d §3](../design/0017d-enterprise-extensibility-and-portability.md), [RFC-0023 §4.1](../design/0023-front-desk-channel-bridge.md) |

---

## 5. Design Space

### 5.1 Where the Adapter Lives

| Option | Description | Verdict |
| :--- | :--- | :--- |
| **A. Compiled-in Go drivers** per VCS inside `bin/hq` | Each VCS gets `internal/workspace/<vcs>.go`. | **Rejected.** Vendor CLIs, paths, and server APIs would leak into the OSS core; violates the two-layer split and `RFC-0017d` pack isolation. |
| **B. Skill-only convention** | A pack ships a `SKILL.md` telling the agent which CLI commands to run; `hq` stays unaware. | **Rejected as sole mechanism.** Gives the agent the instructions but none of `WR2`–`WR7`: no ownership ledger, no lifecycle hooks, no console, no hygiene. (Still useful as a complement for interactive guidance.) |
| **C. Generic `connector` with workspace verbs** | Reuse `hq-connector-protocol/v1`. | **Rejected.** Connectors model *untrusted multi-sender inboxes* with `TaintOnSpawn` quarantine ([RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md)). Workspace control is owner-only, filesystem-mutating, and must not taint the calling task. Same split as `channel_bridges` vs. `connectors` ([Research 18 `G2`](18_channel_bridge_front_desk_audit.md)). |
| **D. Dedicated `workspace_providers` pack component + `hq-workspace-provider/v1` JSON-over-stdio protocol, with git as a built-in Go provider behind the same interface** | Mirrors the shipped `channel_bridges` pattern ([RFC-0023](../design/0023-front-desk-channel-bridge.md)). | **Selected.** Reuses `LintPack`, `BuildTrustBundle`, `VerifyTrustedBinary`, `exec-cache`, and `RunSubprocess` unchanged; keeps every vendor detail in Layer 2. |

### 5.2 Conversation Placement (`WR5`)

| Mode | Conversation project | Workspace relationship | Hooks guaranteed? |
| :--- | :--- | :--- | :--- |
| **Home (default)** | Vault project (verified, exactly as today) | Bound as data: absolute path + provider ID in the prompt preamble and task metadata | **Yes**: same project as Front Desk |
| **Foreign (opt-in)** | The workspace's own registered project | Native: the workspace is the conversation's root | Only if the `agy-hq` plugin is **globally** installed **and** the target project is verified; otherwise refuse |

Home mode is a strict superset of what users already do by hand, and it keeps the Task Board, approvals, and `send_message` report-backs working unchanged. Foreign mode exists for providers whose tooling needs the workspace to be the editor's root (e.g. per-workspace rules discovery). It is gated by a deterministic preflight rather than a warning.

### 5.3 Change Model

Every surveyed system can be reduced to the same small vocabulary:

- **Workspace state:** `clean` | `dirty` | `missing` | `unknown`.
- **Change state:** `local` (not yet uploaded) | `draft` | `in_review` | `approved` | `landed` | `abandoned`.
- **Attention flag:** whether the change is waiting on the workspace owner.

`landed` replaces git's `merged into origin/main`, and "all changes landed + clean" replaces `MergedIntoMain && Clean` as the prune / dormant-archive precondition. The provider maps its native states onto this enum; core never parses vendor output.

### 5.4 Discovery Scale

Monorepo users commonly have **hundreds** of workspaces, many of them short-lived export or review clients. `SessionStart` and the console must therefore show only workspaces **bound to live tasks** (bounded, like the [RFC-0009](../design/0009-task-board-redesign.md) Task Board tiers), while the full inventory is available on demand through `hq workspace list --provider=<id>` and the WebUI, paginated by the provider.

---

## 6. Recommendations

1. Introduce `internal/workspace/` with a `Provider` interface and a built-in `git-worktree` provider that wraps today's `internal/worktree` + `worktree_sync.go` behavior byte-for-byte.
2. Add `components.workspace_providers` to `hq-extension.json` and a 5-verb `hq-workspace-provider/v1` protocol (`probe`, `list`, `create`, `status`, `remove`) executed exactly like channel bridges.
3. Replace the path-prefix Ring-0 guard with an **ownership ledger** (`<vault>/memory/workspaces.json`, `RankWorkspaces`) while keeping `IsManagedWorktreeDir` as the git provider's own invariant.
4. Generalize `ActiveTask` with `Workspaces []WorkspaceBinding`, migrating `WorktreeDir`/`WorktreeDirs`/`RepoRoot` into `git-worktree` bindings on read.
5. Default to **Home placement**; gate **Foreign placement** behind a global-plugin + verified-project preflight.
6. Add core hygiene rules `W1`–`W4` in `PreToolUse` and a new `workspace_remove` approval category.
7. Render a provider-neutral **Workspaces** section everywhere `Active Git Worktrees` appears today, from a daemon-refreshed cache.

These are specified in [RFC-0025](../design/0025-pluggable-workspace-providers.md).
