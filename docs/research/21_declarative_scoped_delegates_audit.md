# Research 21: Declarative Scoped Delegates, Capability Attenuation & Safe Peer-to-Task Dispatch Audit

- **Status:** Complete
- **Date:** 2026-11-20
- **Companion RFC:** [RFC-0026: Declarative Scoped Delegates](../design/0026-declarative-scoped-delegates.md)
- **Prior Specs Reviewed:** [RFC-0003](../design/0003-approval-bound-execution.md), [RFC-0014](../design/0014-proactive-engine-and-living-context.md), [RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md), [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md), [RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md), [RFC-0018](../design/0018-always-on-agent-runtime-hardening.md), [RFC-0021](../design/0021-customizable-approval-policy-and-ux.md), [RFC-0023](../design/0023-front-desk-channel-bridge.md), [RFC-0025](../design/0025-pluggable-workspace-providers.md)

---

## 1. Executive Summary & The "Bespoke Adapter Trap"

`agy-hq` provides strong, mechanically enforced isolation for two opposite ends of the trust spectrum:

1. **Full-privilege personal sessions** (interactive `[HQ] Front Desk`, Task Conversations, and untainted `[Auto]` workers) that hold read/write access to the user's private Vault, all Task Board conversations, local workspaces, and authenticated external CLIs — gated on outbound side effects by `hq propose` ([RFC-0003](../design/0003-approval-bound-execution.md), [RFC-0021](../design/0021-customizable-approval-policy-and-ux.md)).
2. **Quarantined Reader sessions** (`TaintOnSpawn: true`, [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md)) that ingest untrusted external messages from email or chat, withhold all private Vault memory, block every file write and external CLI, and restrict the worker solely to staging proposals (`hq offer stage`) or passive alerts (`hq outbox add`).

This binary split works well for **single-player personal triage** (an owner reviewing their own inbox or messaging their own `[HQ] Front Desk` through an owner-authenticated channel bridge under [RFC-0023](../design/0023-front-desk-channel-bridge.md)). It breaks down immediately when **other people or peer team agents** need to interact with a user's `agy-hq` instance — for example:

- A teammate asks an always-on team bot *"What is the current CI/rollout status of `billing-v2`?"* while the user is away or in deep focus.
- A colleague asks *"Can you prioritize reviewing change `#482910` today?"* when the user already has a dedicated, rule-governed `task:code-review` session running.
- A project lead asks for the latest milestone progress on an explicitly shared workstream without ever exposing unrelated confidential tasks (e.g., performance calibration, HR notes, or personal finances) that sit on the exact same Task Board.

Because `agy-hq` has **no middle tier** between full privilege and blind quarantine, companion extension packs fall into the **Bespoke Adapter Trap**:

- Every new team-facing capability forces the pack author to write custom Go handler code inside a trusted binary, bypass the LLM entirely with rigid hardcoded keyword parsers, rebuild the binary, re-approve a new SHA-256 trust pin in `<vault>/private/trust.json`, and often redeploy the remote peer bot as well.
- Anything that falls outside those hardcoded handlers drops into `[HQ] Front Desk` as a passive `TierInterrupt` alert — where Front Desk safety rules explicitly forbid taking autonomous action on alert text, stalling the request until the owner manually intervenes.

This audit inspects seven architectural seams (`D1`–`D7`) in `agy-hq`, grounds the solution in prompt-injection defense and capability security literature (CaMeL control/data-flow separation and Object-Capability attenuation), and derives the requirements (`SD1`–`SD9`) for **RFC-0026: Declarative Scoped Delegates**.

---

## 2. Forensic Codebase Audit: Seven Delegation Seams (`D1`–`D7`)

### `D1` — Binary Taint Enforcement Leaves No Scoped Capability Tier (`internal/hooks/pre_tool.go`, `internal/hooks/quarantine.go`)

- [`HandlePreToolUse`](../../internal/hooks/pre_tool.go#L309-L333) branches on a single boolean (`ctx.tainted()`). Once a session is marked tainted in `<vault>/private/taint/<sha256(convID)>.json` ([`taint.Mark`](../../internal/taint/taint.go#L110-L241)), every tool except `update_focus`, a narrow `read_file` allowlist, and `run_command` is unconditionally denied.
- [`validateTaintedHQArgv`](../../internal/hooks/quarantine.go#L366-L429) restricts `run_command` to **nine** hardcoded canonical `hq` subcommands (`status`, `watch read|query|ack`, `offer stage`, `outbox add`, `timers set`, `goals mark-replied`, `memory lookup`, `brief format-quarantined`, and `gmail|gchat read-quarantined|read|triage-result`).
- There is no mechanism to attach a **narrow, caller-authorized capability grant** (e.g., *"read progress of task `billing-v2` only"*, *"run read-only `ci-cli status`"*, *"emit a parameterized nudge to `task:code-review`"*, or *"send an egress-filtered auto-reply"*) while keeping the rest of the Quarantine deny-by-default wall intact.

### `D2` — Quarantined Read Allowlist Misses Companion Pack `SKILL.md` Roots (`internal/hooks/quarantine.go`)

- [`isAllowedTaintedReadPath`](../../internal/hooks/quarantine.go#L431-L485) allows a tainted session to call `read_file` only on:
  1. Its own conversation artifact directory (excluding `.system_generated`),
  2. `$PLUGIN_ROOT/skills/`, and
  3. `<base>/*/skills/` under `roots.PluginsDir`, `<AppDataDir>/hq/plugins`, or `<repo>/.agents/plugins`.
- However, companion extension packs registered in `<vault>/extensions.json` and SHA-256-pinned in `<vault>/private/trust.json` ([`internal/extension/pack.go`](../../internal/extension/pack.go#L244-L268)) may reside at any canonical path on disk (for instance, a mounted enterprise plugin directory or a user-managed pack checkout).
- When [`resolveScheduleDispatch`](../../internal/kernel/gatekeeper.go#L86-L107) injects `Companion Skill Override: Activate <skill> at <SkillPath>` into a tainted background worker, the worker's first `read_file` on that trusted pack's `SKILL.md` is **denied** by `isAllowedTaintedReadPath`, wasting turns or aborting the run.

### `D3` — No Allowlist-Scoped Cross-Task Read Isolation (`internal/hooks/session_start.go`, `internal/vault/progress.go`)

- A user's active Task Board (`<vault>/memory/tasks.json`), progress checkpoints ([`vault.LoadProgress`](../../internal/vault/progress.go#L100-L117) at `<vault>/memory/progress/<slug>.md`), and workspace status cache (`<vault>/memory/workspaces.json`) hold **mixed-confidentiality tasks** side by side: shareable engineering workstreams (`billing-v2`, `checkout-latency`) coexist with strictly private threads (`q4-calibration`, `hiring-notes`, `personal-taxes`).
- In [`HandleSessionStart`](../../internal/hooks/session_start.go#L325-L329), any tainted session early-returns with a `<= 800B` banner (`vault.MaxSessionTaintBytes`), withholding all Vault memory layers and all task metadata. Furthermore, [`isAllowedTaintedReadPath`](../../internal/hooks/quarantine.go#L453-L458) unconditionally blocks all reads inside `<vault>/`.
- Conversely, an untainted session receives `MEMORY.md`, `USER.md`, topic indexes, and full read access to `tasks.json`.
- There is no Ring-0 read mediator that exposes **strictly allowlisted** tasks' sanitized progress checkpoints and cached workspace states while keeping every non-allowlisted task physically unaddressable (and undetectable even by existence/title leakage).

### `D4` — No Safe Control/Data Boundary Into Existing Specialized Tasks (`internal/kernel/events_dispatch.go`, `internal/channels/inbound.go`)

- `agy-hq` already provides deterministic message delivery into an existing Antigravity conversation via [`DeliverMessageToSession`](../../internal/kernel/events_dispatch.go#L26-L57) (LanguageServer `SendUserCascadeMessage` with `agentapi send-message` fallback) and [`DispatchPendingEvents`](../../internal/kernel/events_dispatch.go#L210-L231).
- [RFC-0023](../design/0023-front-desk-channel-bridge.md) uses this in [`channels.HandleInbound`](../../internal/channels/inbound.go#L128-L220) to route inbound messages to `[HQ] Front Desk` or a matched task session (`findActiveTaskConversation`). Critically, RFC-0023's threat model assumes the sender is the **verified owner** talking to themselves in a 1:1 channel — never a third-party colleague.
- Routing raw prose from a third-party peer into an existing privileged task (such as `task:code-review`, which holds VCS/review CLI authority) would inject untrusted external text directly into an untainted write-capable context — a textbook indirect prompt injection vector.
- Conversely, escalating third-party requests as passive Front Desk `TierInterrupt` cards hits the passive-alert safety invariant: `[HQ] Front Desk` must not execute actions from alert text without a fresh owner turn, so valid, routine peer requests (like *"please move change `#482910` to the top of the review queue"*) sit idle until the human returns.

### `D5` — No Vendor-Neutral Peer/Delegate Transport Seam (`internal/extension/pack.go`, `internal/channels/protocol.go`)

- Companion packs currently support three SHA-256-pinned executable seams in [`PackComponents`](../../internal/extension/pack.go#L244-L257):
  1. `connectors` ([RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md)) — personal inbox polling (email/chat read-only watch + quarantine).
  2. `channel_bridges` ([RFC-0023](../design/0023-front-desk-channel-bridge.md), [`internal/channels/protocol.go`](../../internal/channels/protocol.go#L19-L216)) — 1:1 owner Front Desk mirror (`bind-desk`, `notify`, `status`).
  3. `workspace_providers` ([RFC-0025](../design/0025-pluggable-workspace-providers.md)) — non-git VCS workspace lifecycle (`probe`, `list`, `create`, `status`, `remove`).
- None of these models a **multi-caller peer delegation transport**: polling authenticated requests originated by *other principals* (`caller_id`, `caller_groups`), delivering structured `reply` / `ack` payloads keyed by `request_id`, and optionally pushing sanitized offline read-only snapshots (`publish_snapshot`) so an always-on cloud/team peer agent can answer basic status questions even when the owner's workstation is asleep.
- Without a permanent, domain-agnostic transport contract (`hq-delegate-transport/v1`), packs overload `daemon_gatekeeper` schedules (`precheck_cmd`) and bake domain business logic directly into compiled binaries.

### `D6` — No Ring-0 Egress Privacy/Commitment Gateway or Delegate Policy Self-Tamper Guard (`internal/hooks/pre_tool.go`)

- [`protectedPrivateSubs`](../../internal/hooks/pre_tool.go#L28-L31) and [`protectedStores`](../../internal/hooks/pre_tool.go#L567-L587) lock down credentials, taint markers, `trust.json`, channels, and workspace ledgers, while [`handlePreToolProtectedWrites`](../../internal/hooks/pre_tool.go#L592-L624) and [`handlePreToolRunCommand`](../../internal/hooks/pre_tool.go#L499-L510) block `[Auto]` sessions from modifying `config.yaml` or `hq-extension.json`.
- However, `agy-hq` has no Ring-0 **egress gateway** for automated peer replies that deterministically checks outbound text for:
  - Vault `private/pii-denylist.txt` tokens and host home paths,
  - **Cross-task leakage** (titles or slugs of *non-allowlisted* tasks in `tasks.json`),
  - Internal harness control tokens (`[hq-spawn:...]`, `prop-...`, `bca_...`, `bcr_...`),
  - User-configured `blocked_patterns` (e.g., blocking an automated delegate from making unilateral deadline promises, LGTM/merge commitments, or HR/compensation disclosures),
  - Per-policy and global hourly auto-reply rate limits.
- Nor is `<vault>/delegates.yaml` or `<vault>/private/delegates/` currently covered by `protectedPrivateSubs` / `protectedStores` against policy self-tampering.

### `D7` — Monolith LOC Ceilings & Lock-Rank Ordering (`internal/ci/slop.go`, `internal/store/store.go`)

- CI Gate `H5-F` ([`monolithLOCCeilings`](../../internal/ci/slop.go#L44-L50)) enforces tight ceilings on five core files: `internal/kernel/kernel.go` (`2015` LOC), `cmd/hq/main.go` (`615` LOC), `internal/hooks/hooks.go` (`415` LOC), `internal/webui/webui.go` (`1115` LOC), and `internal/kernel/workspace_lifecycle.go` (`1075` LOC). Any new delegation subsystem must live in a standalone package (`internal/delegates/`) wired through thin, focused adapter files (`cmd/hq/delegate_cmd.go`, `internal/hooks/delegate_hooks.go`, `internal/kernel/delegate_daemon.go`).
- [`internal/store/store.go`](../../internal/store/store.go#L23-L38) defines a strict increasing lock-rank ladder:
  `RankExtensions (10)` → `RankTriggerCur (20)` → `RankWatcher (30)` → `RankTimers (35)` → `RankEvents (40)` → `RankGoals (50)` → `RankOutbox (60)` → `RankChannels (65)` → `RankTaint (70)` → `RankTasks (75)` → `RankWorkspaces (76)` → `RankMemory (78)` → `RankSubagentQueue (80)` → `RankQuota (85)`.
- A clean gap exists at **`RankDelegates = 72`** (between `RankTaint` and `RankTasks`), and keeping `RankDelegates` a **pure leaf lock** (zero nested store acquisitions inside the lock callback, mirroring `RankWorkspaces = 76` and `RankMemory = 78`) eliminates any possibility of lock-rank inversions with `events.Enqueue (40)`, `outbox.Add (60)`, or `taint.Mark (70)`.

---

## 3. Security Literature & Theoretical Grounding

### 3.1 CaMeL: Separating Control Flow from Data Flow (Debenedetti et al., 2025)

In *"Defeating Prompt Injections by Design"* (Debenedetti, Shumailov, Carlini, Tramèr et al., March 2025), the authors demonstrate that prompt-injection defenses based solely on prompting or classifier heuristics remain bypassable when untrusted data can alter control flow or invoke arbitrary tools. CaMeL's core architectural insight has two pillars:

1. **Control-flow / data-flow separation:** Untrusted data may be processed by a **Quarantined LLM** with *no privileged tools* to extract structured, schema-typed values — while a **Trusted Controller** (deterministic code or a separate privileged execution path) consumes only validated structured fields, never letting raw untrusted text dictate arbitrary tool calls.
2. **Runtime capability policies:** Every tool invocation and data flow is checked by a deterministic policy engine enforcing provenance and least-privilege constraints before execution.

**Direct application to `agy-hq`:**
- For **Read-Only Peer Queries** (*"What is the CI status of `billing-v2`?"*), a **Scoped Delegate** worker stays monotonically tainted (`taint.Mark`), cannot touch private Vault memory or write tools, and receives a Ring-0 **Capability Grant** limited strictly to explicitly opted-in tasks, declared read-only CLI verbs (`strictArgv` + typed arg validators), and an egress-filtered `hq delegate reply` gateway.
- For **Actions Affecting Privileged Tasks** (*"Prioritize reviewing change `#482910`"*), raw peer prose **never** enters `task:code-review`. Instead, either a 0-token regex matcher or a Quarantined Parameter Extractor produces `{change_id: "482910", urgency: "high"}`; Ring-0 Go validates types/patterns, optionally executes a deterministic read-only preflight probe (e.g., verifying change `#482910` is open and assigned to the user), and delivers a **100% static Go-rendered template** into `task:code-review` via `DeliverMessageToSession`. Even a fully adversarial prompt injection in the peer's message body cannot smuggle a single unvalidated character into the target task!

### 3.2 Object-Capability (ocap) Attenuation & Confused Deputy Defense

Classical Object-Capability theory (Hardy 1988, *"The Confused Deputy"*; Miller 2006) establishes that **ambient authority** is the root cause of confused-deputy bugs: when a deputy (the user's agent) holds ambient access to all tasks and tools while acting on behalf of an external caller, the caller can trick the deputy into misusing authority the caller was never granted.

`agy-hq` breaks ambient authority for peer delegation through **Cryptographically Bound Per-Session Capability Grants**:
- Capabilities are **never** ambient; they are attenuated per `(transport, caller_id, policy_id)` before spawn.
- The grant is written once (`O_EXCL`, `0600`) to an LLM-invisible Ring-0 file (`<vault>/private/delegates/grants/<sha256(convID)>.json`) bound via `[hq-spawn:<token>]` at `SessionStart`.
- Because `PreToolUse` checks the Ring-0 grant file rather than trusting any prompt text or CLI flag supplied by the worker, a prompt-injected delegate cannot escalate from `billing-v2` to `q4-calibration`, cannot pass unapproved flags to a read-only CLI, and cannot bypass the egress filter.

### 3.3 Breaking the Lethal Trifecta (Willison / RFC-0003 / RFC-0017b)

Simon Willison's **Lethal Trifecta** warns against combining (1) access to private data, (2) exposure to untrusted external content, and (3) an external communication channel in one agent context.

RFC-0026 preserves the Trifecta invariant by **eliminating leg (1) — general private data**:
- Every Scoped Delegate session has **zero** access to `USER.md`, `MEMORY.md`, `facts.jsonl`, `topics/`, `private/`, or non-allowlisted tasks.
- The *only* data readable inside the session is data the owner explicitly marked as **shareable with that caller group** (`grants.tasks`, `grants.workspaces`, `grants.readonly_commands`).
- Even within that explicitly shareable slice, every outbound reply is still scanned by the deterministic Ring-0 Egress Gateway (`pii-denylist.txt`, non-allowlisted task leak check, local path scrub, commitment regexes, and rate caps) before leaving the host.

---

## 4. Comparative Design Space

| Option | How Adding a New Capability Works | Security Against Prompt Injection | Cross-Task Isolation | Maintenance Burden | Verdict |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **A. Bespoke Go Handlers in Companion Pack** *(current workaround)* | Write Go parser + RPC client + formatter per feature; rebuild binary; re-approve SHA-256 trust pin | High (deterministic Go), but zero LLM flexibility for phrasings | Manual per handler | High — every capability is a code change & redeploy | **Rejected** (the Bespoke Adapter Trap) |
| **B. Full-Privilege Peer-to-Agent Messaging** | Forward colleague messages directly into `[HQ] Front Desk` or target Task Conversations | **Broken** — direct indirect prompt injection into write-enabled sessions | **Broken** — ambient access to confidential tasks & Vault memory | Low | **Rejected** (violates RFC-0003 / Lethal Trifecta) |
| **C. Pure Quarantined Reader + Front Desk Escalation** *(current `agy-hq` core)* | Always escalate to Front Desk `TierInterrupt` | Strong | Strong (withholds everything) | Low, but **deadlocks** — requires manual human action for every routine request | **Baseline fallback** only |
| **D. Declarative Scoped Delegates + Parameterized Task Nudges** *(proposed)* | Add a 10–20 line policy block in `<vault>/delegates.yaml` (no Go rebuild, no binary re-pin, no bot redeploy) | **Strong** — monotonic taint + Ring-0 ocap grant + CaMeL static-template task nudges + Ring-0 egress gateway | **Strong** — explicit opt-in allowlist; unlisted tasks physically unreachable & egress-scrubbed | **Low** — write transport binary once, declare policies in YAML | **Recommended** |

---

## 5. Derived Requirements (`SD1`–`SD9`)

| ID | Requirement | Seam Addressed |
| :--- | :--- | :--- |
| **SD1** | **Permanent, domain-agnostic peer transport protocol (`hq-delegate-transport/v1`):** Companion packs declare `components.delegate_transports` in `hq-extension.json`, SHA-256-pinned in `private/trust.json`, speaking minimal JSON-over-stdio verbs (`poll`, `reply`, `publish_snapshot`, `status`). Adding a capability never requires modifying or rebuilding the transport binary. | `D5` |
| **SD2** | **Declarative policy & capability schema (`<vault>/delegates.yaml`):** Users declare `caller_groups`, `policies[]` (`scoped_worker`, `parameterized_nudge`, `quarantined_escalate`), explicit opt-in `grants`, `egress` policies, and optional `snapshots` in plain YAML (with optional read-only pack templates). Zero ambient grants by default. | `D1`, `D5` |
| **SD3** | **Monotonic taint + Ring-0 immutable capability grants:** Every delegated session remains monotonically tainted (`taint.Mark`), omits all general private Vault memory at `SessionStart`, and is bound via `[hq-spawn:<token>]` to an `O_EXCL` `0600` LLM-invisible grant file under `private/delegates/grants/`. | `D1`, `D3`, `D6` |
| **SD4** | **Allowlist-scoped task & workspace read mediation:** Expose only explicitly allowlisted tasks' `memory/progress/<slug>.md`, sanitized task status, and cached `memory/workspaces.json` state via `hq delegate read-task` / `read-workspace`. Non-allowlisted tasks remain physically unaddressable. | `D3` |
| **SD5** | **Strict read-only command execution (`hq delegate run`):** Execute declared read-only CLI verbs with pinned non-shell `command[0]`, fixed prefix argv, strictly typed parameter validation, zero shell metacharacters (`strictArgv`), scrubbed environment, and bounded timeouts. | `D1` |
| **SD6** | **CaMeL-safe parameterized task nudges (`hq delegate nudge`):** Deliver peer action requests into opted-in existing specialized tasks ONLY as 100% static Go-rendered templates populated exclusively with schema-validated typed parameters and verified caller metadata (plus optional deterministic read-only preflight probe) — zero raw untrusted prose crossing the boundary. Support a 0-LLM-token fast path when `match.regex` extracts all required params deterministically. | `D4` |
| **SD7** | **Ring-0 Egress Gateway & policy self-tamper protection:** Every automated reply via `hq delegate reply` passes PII denylist, local/Vault path scrub, non-allowlisted task title/slug leak detection, harness token scrub, `blocked_patterns`, and sliding-window rate limits — failing closed to `stage_approval` or Front Desk `TierInterrupt` on any violation, with mandatory `brief_rollup` audit trail. Protect `delegates.yaml` and `private/delegates/` in `PreToolUse`. | `D6` |
| **SD8** | **Fix trusted companion pack `SKILL.md` reads in tainted sessions:** Extend `isAllowedTaintedReadPath` to permit read-only access to `skills/` subdirectories of enabled, SHA-256-trusted companion packs (verified against `private/trust.json`), closing `D2` for both Quarantined Readers and Scoped Delegates. | `D2` |
| **SD9** | **Pure Go stdlib, modular architecture, conflict-free lock rank & zero vendor leaks:** Keep `go.mod` at 0 external dependencies (`Gate H4-A`), isolate implementation in `internal/delegates/` under `Gate H5-F` ceilings, assign leaf `store.RankDelegates = 72`, and keep 100% of core code/docs/tests vendor-neutral (`P1`–`P6`). | `D7` |
