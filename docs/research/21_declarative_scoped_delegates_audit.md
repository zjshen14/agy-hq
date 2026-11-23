# Research 21: Unified Session Authorization Spectrum, Live Quarantine Hardening & Declarative Scoped Delegates Audit

- **Status:** Complete (v2 — Incorporates 3 Independent Adversarial Reviews)
- **Date:** 2026-11-20
- **Companion RFC:** [RFC-0026: Unified Session Authorization Lattice & Declarative Scoped Delegates](../design/0026-declarative-scoped-delegates.md)
- **Prior Specs Reviewed:** [RFC-0003](../design/0003-approval-bound-execution.md), [RFC-0014](../design/0014-proactive-engine-and-living-context.md), [RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md), [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md), [RFC-0017c](../design/0017c-lease-based-morning-brief-and-extensions.md), [RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md), [RFC-0018](../design/0018-always-on-agent-runtime-hardening.md), [RFC-0021](../design/0021-customizable-approval-policy-and-ux.md), [RFC-0023](../design/0023-front-desk-channel-bridge.md), [RFC-0025](../design/0025-pluggable-workspace-providers.md)

---

## 1. Executive Summary & Root Architectural Diagnosis

When teammates or always-on peer team agents send requests to a user's `agy-hq` instance — *"What is the CI status of `billing-v2`?"* or *"Can you prioritize reviewing change `#482910`?"* — companion packs today fall into the **Bespoke Adapter Trap**:

- Full-privilege personal sessions hold ambient authority over `USER.md`, `MEMORY.md`, confidential tasks (`q4-calibration`, `personal-taxes`), workspaces, and external CLIs — unsafe for untrusted peer ingress.
- Quarantined Reader sessions ([RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md)) withhold all Vault memory and restrict tools to hardcoded inbox-triage subcommands, escalating everything else to `[HQ] Front Desk` as passive `TierInterrupt` alerts where Front Desk rules prohibit autonomous execution.
- As a result, pack authors end up hardcoding bespoke Go handlers and rebuilding + re-pinning binaries for every incremental team-facing capability.

A deeper forensic audit of `agy-hq` — informed by three independent adversarial reviews (Security, Systems Architecture, and Implementation Feasibility/DX) — reveals that **the Bespoke Adapter Trap is a symptom of a more fundamental design gap**:

1. **`agy-hq` does not have "two profiles"; it already has five ad-hoc session archetypes** (`quarantined_reader`, `unattended_auto`, `subagent`, `front_desk`, and `interactive_task`) computed via scattered boolean flags across `pre_tool.go`, `session_start.go`, and `stop.go`. Bolting a sixth and seventh special case (`scoped_delegate`, `classifier`) onto `if ctx.tainted()` would accumulate fragile one-off guardrails and invite privilege-composition bugs.
2. **Quarantined Reader (`⊥`) and Full-Privilege Interactive (`⊤`) are two extremes of ONE generic, partial-order Capability Lattice (`internal/authz`).** Crucially, `quarantined_reader` (private inbox reads + zero auto-egress) and `scoped_delegate` (curated shareable reads + egress-filtered auto-reply) are **incomparable siblings** in that lattice — never unioned, because unioning them re-opens Willison's Lethal Trifecta.
3. **The audit uncovered four live-codebase security bugs in the current Quarantine implementation** (`triage-result` path traversal, `hq watch read` empty-`ItemKeys` fail-open, `hq status`/`hq memory lookup` taint leaks, and `brief format-quarantined --file` unguarded read) that must be fixed alongside unifying session authorization.

This research documents eight forensic findings (`D1`–`D8`), establishes the formal Capability Lattice and four Non-Interference Invariants (`NI-1`–`NI-4`), and derives requirements `UA1`–`UA10` for **RFC-0026 (v2)**.

---

## 2. Forensic Codebase Audit (`D1`–`D8`)

### `D1` — Scattered 5-Boolean Ad-Hoc Role Model (`pre_tool.go`, `session_start.go`, `stop.go`)

Session permissions are currently re-derived independently in three hook handlers:
- [`internal/hooks/pre_tool.go`](../../internal/hooks/pre_tool.go) caches `isAuto()`, `isSubagent()`, and `tainted()`, then computes `isFrontDesk` across its tool-handling branches.
- [`internal/hooks/session_start.go`](../../internal/hooks/session_start.go) independently computes `isTaintedStart`, `isSubagent`, `isAutoWorker`, `leanAuto`, and `isFrontDesk`.
- [`internal/hooks/stop.go`](../../internal/hooks/stop.go) independently checks `isFrontDesk` and `isTainted`.
- [`validateTaintedHQArgv`](../../internal/hooks/quarantine.go) hardcodes a single 9-subcommand allowlist for every tainted session regardless of purpose.

Adding delegated workers and classifiers as more `if` branches on top of `if ctx.tainted()` creates split-brain policy logic and directly caused a **P0 composition bug** in RFC-0026 v1 (inheriting `validateTaintedHQArgv` alongside `auto_reply`).

### `D2` — Four Live-Codebase Quarantine Security Bugs & Taint-Leak Surfaces

Auditing every verb currently permitted to tainted sessions exposed four concrete defects in `main` that must be remediated as Phase 2A prerequisites:

1. **Path Traversal in `hq gmail|gchat triage-result`:**
   - [`internal/gmail/gmail.go`](../../internal/gmail/gmail.go) and [`internal/gchat/gchat.go`](../../internal/gchat/gchat.go) *(since consolidated into [`internal/connector/messaging_helpers.go`](../../internal/connector/messaging_helpers.go))* write `filepath.Join(dir, batchID+".json")` and `filepath.Join(notesDir, batchID+".md")` with **zero validation on `batchID`** (while `strictArgv` allows `.` and `/`). A prompt-injected tainted session passing `--batch=../../USER` overwrites `<vault>/USER.md`, `--batch=../../MEMORY` overwrites `<vault>/MEMORY.md`, and `--batch=../../../extensions` overwrites `<vault>/extensions.json`.
2. **Fail-Open Fallthrough in `hq watch read`:**
   - [`itemAllowedByLeasedOrTaintedScope`](../../internal/watcher/watcher.go) checks `if alreadyTainted && len(rec.ItemKeys) > 0` (`L1085`) and falls through to `return nil` (`L1099`). Any tainted session whose `taint.Record` has empty `ItemKeys` (or no `connector` event metadata) passes unchecked and can read arbitrary Gmail/GChat threads across any connector/account.
3. **Private Data Leaks in `hq status`, `hq memory lookup`, and `hq brief format-quarantined`:**
   - [`cmd/hq/main.go`](../../cmd/hq/main.go) (`hq status`) performs zero taint check and prints the host `vaultDir` path, active bridges, and **every pending approval proposal's `ID`, `Summary`, `ActionType`, and `SHA256[:12]`**.
   - [`LookupEntities`](../../internal/vault/entities_and_forget.go) returns the entire contact directory (`ListEntities`) when `query == ""`, and `entityMatchesQuery` (`L343-L356`) matches against **unredacted `Context`, `Spaces`, and `Notes` before** CLI redaction — creating a boolean search oracle over private notes.
   - [`cmd/hq/extensions_cli.go`](../../cmd/hq/extensions_cli.go) (`hq brief format-quarantined --file=<path>`) performs an unguarded `os.ReadFile(*fileIn)` outside `protectedPrivateSubs`.
4. **Raw Tainted Output & Uncurated Auto-Checkpoints Flowing Across Trust Boundaries:**
   - [`internal/hooks/stop.go`](../../internal/hooks/stop.go) writes `tm.LastResponse` verbatim into `ActiveTask.LastSummary` even for tainted sessions, which `SessionStart` then injects into untainted `[HQ] Front Desk`.
   - [`internal/hooks/stop.go`](../../internal/hooks/stop.go) auto-writes raw `summary` and raw host `filesModified` paths into `memory/progress/<slug>.md` at 120k context tokens — proving that neither `ActiveTask.LastSummary` nor raw `memory/progress/<slug>.md` is curated for external peer disclosure.

### `D3` — Quarantined Read Allowlist Misses Companion Pack `SKILL.md` Roots (`quarantine.go`, `trust.go`)

- [`isAllowedTaintedReadPath`](../../internal/hooks/quarantine.go) permits `read_file` only under the conversation's own artifact dir, `$PLUGIN_ROOT/skills`, `<PluginsDir>/*/skills`, `<AppDataDir>/hq/plugins/*/skills`, and `<repo>/.agents/plugins/*/skills`.
- Companion extension packs registered via `<vault>/extensions.json` and pinned in `<vault>/private/trust.json` ([`internal/extension/pack.go`](../../internal/extension/pack.go)) may reside at arbitrary canonical roots, causing [`resolveScheduleDispatch`](../../internal/kernel/gatekeeper.go) skill overrides to fail on Turn 1 in tainted runs.
- Crucially, `internal/extension/trust.go` currently hashes `SKILL.md` files only for `SkillOverrides`, not all declared `<pack>/skills/` directories — so fixing `D3` requires pinning declared skill files in `TrustedTarget.Files` and restricting Scoped Delegates strictly to their granted `Skills`.

### `D4` — No Curated, Allowlist-Scoped Cross-Task & Workspace Projection (`session_start.go`, `progress.go`, `workspaces.json`)

- `<vault>/memory/tasks.json`, `<vault>/memory/progress/<slug>.md` ([`vault.LoadProgress`](../../internal/vault/progress.go)), and `<vault>/memory/workspaces.json` mix team-shareable projects (`billing-v2`) with strictly confidential tasks (`q4-calibration`, `personal-taxes`).
- `LedgerEntry` in `memory/workspaces.json` contains host paths (`Root`, `CanonicalRoot`, `WorkDir`), raw stderr (`Status.Error`), `WriterTask`, and `BoundTasks` (which can list co-bound non-granted confidential task slugs).
- Tainted sessions withhold everything (`MaxSessionTaintBytes = 800` in [`internal/vault/memory.go`](../../internal/vault/memory.go)), while untainted sessions read everything. A Ring-0 read mediator must project **only explicitly curated shareable fields** (`## Public Status` + projected workspace change states, never `LastSummary`, never host paths, never `BoundTasks`).

### `D5` — Unattended Interactive Escalation & 2nd-Order Prompt Injection in Peer-to-Task Dispatch (`events_dispatch.go`, `pre_tool.go`)

- [`DeliverMessageToSession`](../../internal/kernel/events_dispatch.go) and [RFC-0023](../design/0023-front-desk-channel-bridge.md) [`channels.HandleInbound`](../../internal/channels/inbound.go) assume an **owner-authenticated 1:1 sender**.
- Directly waking an existing interactive `kind: "task"` session (e.g., `task:code-review`) from a peer request — even via a static CaMeL template — hits **two critical hazards**:
  1. **`isAuto == false` bypass:** `isAutoConversation()` returns `false` for `kind: "task"`, so [`HandlePreToolUse`](../../internal/hooks/pre_tool.go) evaluates `approvals.EvaluateCommand` with `isAuto = false`, **disabling `ModeAutoOnly` approval gates** (`git push`, `gh pr review/comment`, `workspace_auto_guard`) while the owner is away!
  2. **2nd-order prompt injection via referenced objects:** A validated `change_id=482910` points to untrusted external content (PR/CL description, diffs, review comments). Driving untrusted objects unattended into an interactive write-enabled task re-introduces remote code/action injection.
- Therefore, CaMeL-safe peer nudges in v1 ship exclusively with **`delivery_mode: "queue_only"`** (appending static templates into a protected per-task priority queue rendered in Zone 2 on the owner's next interactive turn — zero unattended execution of external objects!). Any future automated triage worker must bind strictly to a tainted `NI-1`-compliant read-only delegate profile (zero `host_fs`, zero `vault_memory`, zero arbitrary shell), never `unattended_auto`.

### `D6` — No Vendor-Neutral Peer/Delegate Transport Seam (`extension/pack.go`, `channels/protocol.go`)

- [`PackComponents`](../../internal/extension/pack.go) defines `connectors` (personal inbox watch), `channel_bridges` (1:1 owner Front Desk mirror, [`internal/channels/protocol.go`](../../internal/channels/protocol.go)), and `workspace_providers` (non-git VCS workspaces).
- `channel_bridges` carries owner-equivalent trust and Front Desk shift-binding, so it MUST remain distinct from multi-caller untrusted peers.
- A permanent, domain-agnostic `hq-delegate-transport/v1` (`poll`, `reply`, `publish_snapshot`, `status`) — reusing `connector.RunSubprocess` and `extension.VerifyTrustedBinary` — ends per-capability Go binary rewrites while keeping owner bridges strictly isolated.

### `D7` — Pure-Go Stdlib Config Format, Policy Sovereignty & `strictArgv` Mechanics (`Gate H4-A`, `quarantine.go`, `pre_tool.go`)

1. **Zero-dep YAML parser hazard:** Under `ADR-0002` / `Gate H4-A` (0 external Go dependencies), `internal/approvals/policy.go` already requires **359 LOC** of bespoke state-machine parsing for a 2-level YAML schema. Hand-rolling a 6-level YAML parser (nested command arg lists, arbitrary maps, `template: |` block scalars, `#` inside regexes) for a **Ring-0 security policy** would take 800–1,200+ LOC and risk silent privilege escalation on indentation/quoting bugs. By contrast, **JSONC** (`encoding/json` + `DisallowUnknownFields()` + ~45-LOC quote-aware comment/trailing-comma stripper + `string | []string` multiline templates) matches `hq-extension.json`, `private/channels/*.json`, and `memory/workspaces.json`.
2. **Policy file sovereignty:** Placing policy at `<vault>/delegates.yaml` (writable by any untainted interactive coding session without `hq propose`) allows a prompt-injected coding session reading an untrusted repo/web page to add `attacker@evil.com` + `cat ~/.ssh/id_rsa` (`IsBlockedComponentCommand0` blocks shells/interpreters, not `cat`/`curl`). Moving policy to LLM-invisible **`<vault>/private/delegates/policy.json`** (`0600`) managed via **`hq delegate configure --file=<draft>`** → `hq propose` (`rsi_apply`) 1-tap diff approval (mirroring RFC-0023's `hq channel configure`) + requiring **SHA-256 pinning** of `commands[].command[0]` and `preflight.command[0]` in `private/trust.json` (matching RFC-0017c `brief.d`) closes both holes while keeping 10-second chat-assisted editing!
3. **`strictArgv` multiline reply bottleneck:** [`strictArgv`](../../internal/hooks/quarantine.go) forbids raw `\n`/`\r`, `\n` escapes, `$`, `` ` ``, and single-quote escapes, while `write_file` is denied in delegates. Supporting `--body-b64=<rfc4648>` (decoded in Go *before* Ring-0 egress checks) enables rich multiline markdown replies with zero filesystem write surface.

### `D8` — Exact Monolith Ceilings (`Gate H5-F`), Import Cycles & Unified Ring-0 Storage (`slop.go`, `store.go`, `taint.go`)

- **Monolith LOC ceilings** ([`internal/ci/slop.go`](../../internal/ci/slop.go), measured on current `main`):
  - `internal/hooks/hooks.go`: **405 / 415** (**10 LOC** headroom → add 0 LOC; put hook helpers in `internal/hooks/authz_hooks.go` / `delegate_hooks.go`)
  - `internal/kernel/workspace_lifecycle.go`: **1056 / 1075** (**19 LOC** headroom → add 0 LOC)
  - `cmd/hq/main.go`: **595 / 615** (**20 LOC** headroom → add <= 4 LOC; put CLI in `cmd/hq/delegate_cmd.go`)
  - `internal/webui/webui.go`: **1093 / 1115** (**22 LOC** headroom → add <= 8 LOC; put UI handlers in `internal/webui/delegates_ui.go`)
  - `internal/kernel/kernel.go`: **1991 / 2015** (**24 LOC** headroom → add <= 4 LOC; put daemon hook in `internal/kernel/delegate_daemon.go`)
- **Import-cycle hazard:** `internal/ci/pii.go` shares `package ci` with `slop.go` (importing `go/types`) and `harness.go` (importing `internal/hooks`, `internal/kernel`, `internal/workspace`). Runtime packages MUST NEVER import `internal/ci`; pure-stdlib secret/PII regexes must move into a zero-dep leaf package **`internal/redact`**.
- **Single Ring-0 session grant store (no split-brain):** [`internal/taint/taint.go`](../../internal/taint/taint.go) ALREADY implements `O_EXCL` `0600` per-conversation capability records under [`RankTaint = 70`](../../internal/store/store.go) with append-only `ledger.jsonl` failover and symlink refusal. Extending `taint.Record` to `SchemaVersion: 2` (`Grant *SessionGrant`, `nil` = legacy `quarantined_reader`) eliminates duplicate grant stores and guarantees atomic binding across BOTH [`events_dispatch.go`](../../internal/kernel/events_dispatch.go) (`bindSpawnedConv`) and [`quarantine.go`](../../internal/hooks/quarantine.go) (`bindSpawnTokenFromTranscript`). Transport state (`memory/delegates_state.json`) claims pure leaf lock **`RankDelegates = 66`** right beside `RankChannels = 65`.
- **SessionStart budget:** `vault.MaxSessionTaintBytes = 800` ([`internal/vault/memory.go`](../../internal/vault/memory.go)) must be paired with a dedicated `vault.MaxSessionDelegateBytes = 2048` so Scoped Delegate grant banners are never silently truncated.

---

## 3. Theoretical Grounding: Capability Lattice & Non-Interference

### 3.1 Why a Partial-Order Capability Lattice (Not a 1D Linear Spectrum)

Classical Object-Capability theory (Hardy 1988; Miller 2006), CaMeL (*"Defeating Prompt Injections by Design"*, Debenedetti et al., 2025), and Willison's Lethal Trifecta ([RFC-0003](../design/0003-approval-bound-execution.md)) together establish why session authorization in `agy-hq` is a **5-axis partial-order lattice** (`⊥` → `⊤`):

- **Top (`⊤`) — `interactive_task`:** Owner present (`IngressTrust: owner_interactive`), full Vault memory, leased workspace edits, interactive `approvals.rules`, and human-presence `hq approve`.
- **Upper-Middle — `front_desk`, `unattended_auto`, `subagent`:** Owner-initiated but progressively attenuated along explicit axes (no code edits / 3-lookup cap for Front Desk; no `ask_question` / no `hq approve` / `ModeAutoOnly` fail-closed for `unattended_auto` and `subagent`).
- **Incomparable Middle Siblings — `quarantined_reader` vs. `scoped_delegate`:**
  - `quarantined_reader` (RFC-0017b) reads **private personal inbox data** (`bound_batch`) and has **zero auto-egress** (only human-gated `offer stage` / `outbox add`).
  - `scoped_delegate` (RFC-0026) reads **explicitly curated shareable data** (`Public Status` + pinned read-only CLIs) and may emit **egress-filtered auto-reply** (or typed static nudges).
  - Their join (`quarantined_reader ∪ scoped_delegate`) is **illegal** because it combines private inbox reads + untrusted ingress + automated egress!
- **Bottom (`⊥`) — `classifier`:** Zero data reads beyond the single request envelope, zero writes, zero egress — permitted solely to emit `hq delegate route --request=<id> --policy=<id|escalate>`.

### 3.2 Four Mechanical Non-Interference Invariants (`NI-1`–`NI-4`)

Rather than hoping individual hooks remember every security rule, `authz.ValidateProfile(p)` mechanically enforces four invariants across **all** profiles at load time and bind time:

1. **`NI-1` (Lethal Trifecta Exclusion):** If `IngressTrust ∈ {untrusted_inbox, untrusted_peer}`, the profile MUST NOT combine any `private_read ∈ {host_fs, vault_memory, inbox_bound_batch, inbox_low_query, entity_routing, pending_proposals}` with any `auto_egress ∈ {egress_filtered_single_reply, interactive_approval_policy}`.
2. **`NI-2` (Untrusted Execution Containment):** If `IngressTrust ∈ {untrusted_inbox, untrusted_peer}`, then `ContextProjection ∉ {full_vault, lean_vault}`, `Writes` excludes `{workspace_edits_leased, vault_memory_write}`, tools are deny-by-default (`update_focus`, bounded `read_file`, `strictArgv` canonical `hq`-only `run_command`), and IPC env vars are scrubbed.
3. **`NI-3` (Human-Presence Top-of-Lattice Divider):** `human_presence_approve` (`hq approve`, proposal `ask_question`) and `interactive_approval_policy` (`ModeAutoOnly` bypass) are valid **ONLY** when `IngressTrust == owner_interactive`.
4. **`NI-4` (Ring-0 Sovereign Ceiling Above `⊤`):** `<vault>/private/**` (including `trust.json`, `channels/`, `delegates/`, `taint/`) and `.system_generated/**` are unreadable/unwritable across ALL profiles (`⊥`..`⊤`), mutating solely via human terminal CLI or `hq propose` (`rsi_apply`) + human-presence `hq approve`.

Dynamic tainting (e.g., an interactive session calling `hq watch read`) is simply **monotonic downward attenuation** (`profile' = Attenuate(profile, TaintCeiling)`), guaranteed never to widen authority.

---

## 4. Comparative Design Space

| Option | Generalizes Across All Sessions? | Prevents Lethal Trifecta & AI Slop? | Effort to Add a New Peer Capability | Verdict |
| :--- | :--- | :--- | :--- | :--- |
| **A. Bespoke Pack Go Handlers** *(status quo)* | No | Manual per handler | Rewrite Go + rebuild + re-pin binary | **Rejected** |
| **B. Ad-Hoc Delegates Bolted Onto `if ctx.tainted()`** *(RFC-0026 v1)* | No — adds 3 more special cases on top of 5 existing booleans | **Broken** (`P0-1` inherited `validateTaintedHQArgv` leaks approvals/inbox/contacts) | Edit YAML (fragile stdlib parser) | **Rejected** |
| **C. Unified `internal/authz` 5-Axis Capability Lattice + Declarative JSONC Delegates** *(RFC-0026 v2)* | **Yes** — unifies all 7 profiles (`classifier ⊥` through `interactive_task ⊤`) in one table-driven engine | **Strong** — `NI-1`..`NI-4` enforced mechanically; `taint.Record` v2 single store; `private/delegates/policy.json` + 1-tap `configure` | Add a ~15-line JSONC policy + 1-tap `hq delegate configure` approval (0 Go rebuilds) | **Recommended** |

---

## 5. Derived Requirements (`UA1`–`UA10`)

| ID | Requirement | Finding(s) |
| :--- | :--- | :--- |
| **UA1** | **Phase 2A-0 Live Quarantine Hardening:** Fix `triage-result` `batchID` path traversal, `hq watch read` empty-`ItemKeys` fail-open, `hq status` / `hq memory lookup` taint leaks, `brief format-quarantined --file` under taint, and `stop.go` tainted `LastSummary` redaction before enabling any delegated auto-egress. | `D2` |
| **UA2** | **Unified `internal/authz` Capability Lattice:** Replace scattered role booleans across `pre_tool.go`, `session_start.go`, and `stop.go` with one table-driven `SessionPolicy` covering all 7 profiles (`classifier` ⊥ → `interactive_task` ⊤), verified by `NI-1`–`NI-4`, with monotonic downward attenuation and 100% RFC-0017b backward compatibility. | `D1` |
| **UA3** | **Single Ring-0 Grant Store (`taint.Record` v2):** Extend `internal/taint` (`SchemaVersion: 2`, `Grant *SessionGrant`, `private/taint/pending/`) under `RankTaint = 70` with one atomic bind helper called from both `events_dispatch.go` and `quarantine.go`; claim pure leaf `RankDelegates = 66` for `memory/delegates_state.json`. | `D8` |
| **UA4** | **Permanent Dumb Peer Transport (`hq-delegate-transport/v1`):** Register under `hq-extension.json` `components.delegate_transports` (SHA-256 pinned, `poll`/`reply`/`publish_snapshot`/`status`, `auth_method: "platform_verified_sender"`, canonical ASCII `caller_id`, origin-thread binding). Keep strictly separate from RFC-0023 owner `channel_bridges`. | `D6` |
| **UA5** | **Sovereign JSONC Policy (`<vault>/private/delegates/policy.json`):** Zero-dep JSONC (`DisallowUnknownFields()`, `string | []string` templates), top-level hoisted `commands:` (all `command[0]` SHA-256 pinned in `private/trust.json`), orthogonal `grants:` + `egress:` (no `mode:` enum), `match.sources` (`transport:<fqid>`, future `connector:<id>`), no `composable: true` in v1, edited via `hq delegate configure --file=<draft>` → `rsi_apply` 1-tap approval. | `D7` |
| **UA6** | **Curated Shareable Reads & Opaque Peer Ingress:** `hq delegate read-task` exposes ONLY `{taskSlug, status, lastUpdatedAt}` + curated `## Public Status` (`hq progress save --public-status`, never written by 120k auto-checkpoint) + projected workspace state (no host paths, no `BoundTasks`, no `LastSummary`). Peer request bodies stay in `private/delegates/requests/` read via nonce-fenced `hq delegate read-request` (never raw in `events.json` or `journal/`). | `D2`, `D4` |
| **UA7** | **Safe CaMeL Parameterized Task Nudges:** Never wake interactive (`isAuto == false`) tasks unattended, never allow `spawn_if_missing: true`, and never dispatch untrusted external objects into untainted `unattended_auto` workers. Ship `delivery_mode: "queue_only"` exclusively in v1 (protected `memory/delegates_queues/` rendered in Zone 2 on the owner's next interactive turn + `hq delegate ack-nudge`), with `target_task_scope`, pre-preflight rate limits/cooldowns, uniform opaque failure acks, and 0-token regex fast path. | `D5` |
| **UA8** | **Multiline Egress Gateway, Escalation & Shared `internal/redact`:** Extract leaf `internal/redact` (zero `internal/ci` runtime import). Support `hq delegate reply --body-b64` (keeps `write_file` denied) + first-class `hq delegate escalate`. Reframe egress scanning as defense-in-depth behind input isolation (Unicode `Cf`/bidi strip, high-entropy blob block, exact word-boundary non-granted slugs minus granted tokens + `pii-denylist.txt` + `sensitive_terms`, commitment regexes, rate limits, FD3 `HashVersion: 2` `hq delegate apply-reply` staging on fail-closed). | `D7`, `D8` |
| **UA9** | **Verified Companion Pack `SKILL.md` Reads (`D3`):** Pin declared pack skill files in `TrustedTarget.Files` and allow read-only reads of verified skill paths in `authz.SessionPolicy.IsAllowedReadPath` (restricted to `grant.Skills` for Scoped Delegates). | `D3` |
| **UA10** | **Strict Gate `H4-A` / `H5-F` / `P1`–`P6` Hygiene:** Pure Go stdlib (`go.mod` 0 external deps), `MaxSessionDelegateBytes = 2048`, zero monolith ceiling breaches (`hooks.go` +0 LOC, `webui.go` <= 8 LOC), and 100% vendor-neutral core code/docs/tests. | `D8` |
