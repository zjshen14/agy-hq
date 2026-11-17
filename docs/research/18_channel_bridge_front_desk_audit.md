# Research 18: Vendor-Neutral Front Desk Channel Bridge, Rotation Tunneling & Security Boundary Audit

- **Status:** Complete
- **Date:** 2026-11-17
- **Companion RFC:** [RFC-0023: Vendor-Neutral Front Desk Channel Bridge & Rotation Tunneling](../design/0023-front-desk-channel-bridge.md)
- **Prior Specs Reviewed:** [RFC-0004](../design/0004-heartbeat-and-channels.md), [ADR-0004](../decisions/0004-remote-control-first-pluggable-channels.md), [RFC-0008](../design/0008-gchat-integration.md), [RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md), [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md), [RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md), [RFC-0020](../design/0020-proactive-ambient-synthesis-and-mobile-push.md), [RFC-0021](../design/0021-customizable-approval-policy-and-ux.md)

---

## 1. Executive Summary & Audit Scope

`agy-hq` anchors interactive coordination in a single leased **`[HQ] Front Desk`** conversation (`<vault>/memory/front_desk.json`), backed by Antigravity Desktop, Antigravity Remote Control, and the local `:4242` Web Console. In day-to-day operation, however, users also work from external interactive messaging clients (enterprise chat rooms, self-hosted team messengers, or personal bot channels) where an external interactive messaging sidecar or chat bot extension mirrors `[HQ] Front Desk` notifications and tunnels verified owner replies back to the active desk.

A forensic audit of `agy-hq`'s prior channel designs and current runtime reveals two structural gaps preventing clean external Front Desk bridging:

1. **No Vendor-Neutral Channel Bridge Extension Contract:** Early designs ([RFC-0004](../design/0004-heartbeat-and-channels.md), [RFC-0008 §4.2](../design/0008-gchat-integration.md)) proposed compiling vendor-specific `ChannelAdapter` drivers directly into `bin/hq` and storing destination IDs in LLM-readable `<vault>/config.yaml`. Neither `internal/channels/` nor a companion-pack bridge protocol was ever implemented, while [RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md) later established that organization- or vendor-specific integrations belong in SHA-256-pinned companion extension packs (`hq-extension.json`), keeping core `agy-hq` 100% pure Go stdlib and vendor-neutral.
2. **Silent Post-Rotation Tunnel Split-Brain:** `[HQ] Front Desk` rotates its `ConversationID` across shift handovers (`hq claim-desk`, `hq release-desk`, `hq adopt`, and fresh-Vault bootstrap). Today, `hq daemon` never detects `front_desk.json` `ConversationID` transitions or notifies external bridges. Any external chat sidecar bound to an outgoing Front Desk shift continues routing owner messages into an archived or demoted `[Task:...]` conversation, while open proposal and alert threads in the external chat client lose association with the new Front Desk shift.

This audit inspects all relevant prior RFCs and six core codebase subsystems to establish the exact invariants, failure modes, and Step 1 / Step 2 separation required for [RFC-0023](../design/0023-front-desk-channel-bridge.md).

---

## 2. Retrospective Audit of Prior RFCs (`G1`–`G5`)

| Prior Spec | Channel / Notification Design Proposed | Current Codebase Reality | Structural Gap Identified |
| :--- | :--- | :--- | :--- |
| **[RFC-0004](../design/0004-heartbeat-and-channels.md) & [ADR-0004](../decisions/0004-remote-control-first-pluggable-channels.md)** | Proposed an in-process Go `ChannelAdapter` interface (`SendBrief`, `SendApprovalRequest`, `ListenEvents`) with compiled-in vendor drivers (`gchat.go`, `telegram.go`) reading `owner_space_id` / `owner_chat_id` from `<vault>/config.yaml`. | `internal/channels/` does not exist in `internal/`. [`templates/config.yaml`](../../templates/config.yaml) (`L4–14`) still carries legacy `channels:` stub keys (`0644` permissions, readable by untainted LLMs). | **`G1` (Compiled-In Vendor Coupling & Readable Destination Config):** Embedding bot SDKs or enterprise chat transports in `bin/hq` violates `ADR-0002` (zero external deps) and `RFC-0017d` pack isolation. Storing destination room/webhook identifiers in `<vault>/config.yaml` exposes private routing topology to LLM file reads. |
| **[RFC-0008](../design/0008-gchat-integration.md)** | Built `internal/gchat` CRUD + quarantined reader + 1-step `hq gchat propose` (§2–3), and sketched `internal/channels/gchat.go` (§4.2) for single-space self-chat mirroring. | Layer 1A (`internal/gchat`) shipped and is used for outbound `gchat_send` proposals and inbound triage, but §4.2 `internal/channels/gchat.go` was never built. | **`G2` (Data-Connector vs. Control-Bridge Conflation):** Multi-party workspace chat connectors (`hq-connector-protocol/v1`) ingest untrusted external messages into `TaintOnSpawn: true` quarantined workers, whereas a Front Desk bridge is a single-owner outbound notification + desk-rotation control channel. |
| **[RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md) & [RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md)** | Introduced `hq-extension.json`, `<vault>/private/trust.json` (`VerifyTrustedFile` + `VerifyTrustedBinary` + `exec-cache`), and `connector.RunSubprocess` env scrubbing. | [`internal/extension/pack.go`](../../internal/extension/pack.go) (`L120–131`) indexes `connectors`, `brief_extensions`, `schedules`, `inbox_rules`, `skill_overrides`, `front_desk_overlays`, `routing_defaults`, and `approval_rules`, but has no `channel_bridges` surface. | **`G3` (Missing `channel_bridges` Pack Component):** Companion packs have no sanctioned manifest entry or stdio protocol to register a trusted outbound/rotation Front Desk bridge binary. |
| **[RFC-0020](../design/0020-proactive-ambient-synthesis-and-mobile-push.md)** | Proposed an optional zero-body one-way push webhook (`ntfy` / Pushover / Shortcuts in §4.3) triggered on `TierInterrupt` outbox delivery. | Still in `Proposed` status; covers stateless generic lock-screen badges only. | **`G4` (Stateless Push Cannot Maintain Threads or Desk Bindings):** One-way push relays have no concept of a pinned `desk_thread`, per-proposal/per-task `item_threads`, or `bind-desk` shift rotation rebinding. |
| **[RFC-0021](../design/0021-customizable-approval-policy-and-ux.md)** | Hardened `approvals.rules`, `24h` `DefaultAskTTL`, two-phase idempotent `VerifyAskAndMintCap` -> `ApproveFromCLI`, and surgical `ask_question` prompt rewriting. | Implemented in [`internal/approvals/presence.go`](../../internal/approvals/presence.go) and [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go). | **`G5` (Chat Approval Boundary Must Preserve Transcript Proof):** Plain-text chat replies (`USER_INPUT`) are intentionally rejected by `VerifyAskAndMintCap`; bridges must preserve native `ask_question` card verification + `:4242` Web Console fallback rather than inventing plain-text chat `approve` commands. |

---

## 3. Forensic Codebase Audit Across Six Subsystems

### 3.1 Extension Manifest & Binary Trust Registry (`internal/extension/`)

- **`PackComponents` Schema ([`internal/extension/pack.go`](../../internal/extension/pack.go#L120-L142)):**
  - `LoadPackManifest` decodes `<pack>/hq-extension.json` with `json.NewDecoder.DisallowUnknownFields()` (`L174`), ensuring `<pack>/plugin.json` remains 100% untouched for `agy plugin validate --strict` (`L205–215`).
  - Adding `channel_bridges` to `PackComponents` fits naturally alongside `connectors` and `brief_extensions`, with `EnablePack` (`L474–495`) enforcing unqualified ID collision checks across enabled packs (disambiguated via `winner_overrides` in `<vault>/extensions.json`).
- **SHA-256 Pinning & TOCTOU-Safe Execution ([`internal/extension/trust.go`](../../internal/extension/trust.go#L304-L411)):**
  - `BuildTrustBundle` (`L564–705`) hashes `hq-extension.json` (`ManifestSHA256`) and pins external binary paths via `pinBinaryRef` (`L459–494`).
  - At execution time, `VerifyTrustedBinary` (`L304`) reads the resolved binary with `store.OpenReadNoFollow`, verifies its SHA-256 against `<vault>/private/trust.json` in constant time, and stages an immutable `0500` copy under `<vault>/private/exec-cache/<sha256>/<basename>` (`stageVerifiedExecCache`, `L313–370`).
  - **Audit Takeaway:** Reusing `VerifyTrustedBinary` + `connector.RunSubprocess` ([`internal/connector/exec.go`](../../internal/connector/exec.go#L271-L357)) gives `channel_bridges` TOCTOU-proof binary execution, process-group timeout kills (`Setpgid: true`), `SanitizeStderr` `<plugin-card>` stripping, and unconditional `ANTIGRAVITY_CSRF_TOKEN` / `ANTIGRAVITY_LS_ADDRESS` scrubbing out of the box.

### 3.2 Front Desk Lease Lifecycle & Rotation Paths (`internal/kernel/kernel.go`, `cmd/hq/main.go`)

Inspection of all callers of `SetFrontDesk` and `ReleaseFrontDesk` identifies **four distinct lease transitions** on `<vault>/memory/front_desk.json`:

1. **Initial Claim (`"" -> new_cid`):**
   - Triggered on Turn 1 of a fresh Vault via `SessionStart` (`hooks.go`) or `HandleStop` bootstrap fallback ([`internal/hooks/hooks.go`](../../internal/hooks/hooks.go#L2030-L2035)), or manually via `hq claim-desk --conv=<new_cid>` ([`cmd/hq/main.go`](../../cmd/hq/main.go#L810-L867)).
2. **Shift Handover Rotation (`old_cid -> new_cid`):**
   - `hq claim-desk --conv=<new_cid>` (`cmd/hq/main.go:834–855`) archives the outgoing shift as `headquarter-front-desk-<short>` (`Status: "completed"`) in `tasks.json`, removes `hq_console.md` from `prev.ArtifactDir` ([`internal/kernel/kernel.go`](../../internal/kernel/kernel.go#L1737-L1742)), and writes the new `ConversationID` to `front_desk.json`.
3. **Explicit Lease Vacation (`old_cid -> ""`):**
   - `hq release-desk` (`cmd/hq/main.go:868–879` -> [`ReleaseFrontDesk`](../../internal/kernel/kernel.go#L2452-L2468)) deletes `front_desk.json` and writes `<vault>/memory/.vacated`.
4. **Shift Demotion via Task Adoption (`old_cid -> ""`, followed later by `"" -> new_cid`):**
   - `hq adopt --conv=<old_cid> --task=<slug>` (`cmd/hq/main.go:902–923`) calls `ReleaseFrontDesk` and converts `old_cid` into an active `[Task:<scope>]` coding/research conversation!

**Two Concrete Code Findings:**
- **Finding `R1` (Demotion Split-Brain Hazard):** During Path 4 (`hq adopt`), `front_desk.json` transitions `old_cid -> ""` *before* a replacement Front Desk conversation exists. If an external sidecar only rebinds on non-empty `new_cid` (ignoring `old_cid -> ""`), any mobile chat message sent before a new Front Desk opens is injected directly into the demoted `[Task:<scope>]` coding session! `bind-desk` must therefore model `lease_state: "active" | "vacated"` explicitly.
- **Finding `R2` (Non-Atomic Lease Writes):** `saveFrontDeskLease` ([`internal/kernel/kernel.go`](../../internal/kernel/kernel.go#L1701-L1710)) currently uses `os.WriteFile(frontDeskPath(vaultDir), data, 0o644)` in place rather than `store.WriteAtomic`. Polling `front_desk.json` every `15s` from `hq daemon` risks observing a truncated file mid-write unless `saveFrontDeskLease` switches to `store.WriteAtomic`.

### 3.3 Daemon Tickers & Event/Outbox Dispatch (`cmd/hq/main.go`, `internal/kernel/events_dispatch.go`)

- **Cross-Process Convergence via `fastTicker` ([`cmd/hq/main.go`](../../cmd/hq/main.go#L1741-L1793)):**
  - `hq daemon` runs four loop arms: `wakeCh` (in-process channel inside the daemon process), `fastTicker` (`15s`), `cronTicker` (`30s`), and `hbTicker` (`30m`) (`internal/kernel/daemon.go` holds service management and trigger config loaders, while the live select loop resides in `cmd/hq/main.go:1742–1793`).
  - Because CLI commands (`hq claim-desk`, `hq release-desk`, `hq adopt`, `hq outbox add`, `hq complete`) execute in separate short-lived OS processes, they do not signal in-process `wakeCh`. `fastTicker` (`15s`) is therefore the primary cross-process convergence point (`<= 15s` worst-case rotation and notification latency).
- **Coupling Hazard in `DispatchOutboxToFrontDesk` ([`internal/kernel/events_dispatch.go`](../../internal/kernel/events_dispatch.go#L481-L495)):**
  - Today, `DispatchOutboxToFrontDesk` exits immediately (`return 0, nil`) if `GetFrontDesk` returns `!ok || fd.ConversationID == ""`.
  - When `fd.ConversationID` is present, `outbox.DispatchPendingInterrupts` ([`internal/outbox/dispatcher.go`](../../internal/outbox/dispatcher.go#L105-L225)) runs a 3-phase lock-safe pipeline under `store.RankOutbox` (`60`) and transitions each item to `StatusDelivered` once `DeliverMessageToSession` succeeds.
  - **Finding `R3` (Independent Retry Cursor Requirement):** Coupling external channel delivery synchronously inside `DispatchPendingInterrupts`'s `deliverFn` would cause a transient external chat outage (`HTTP 429` / `503`) to fail `deliverFn`, reverting `OutboxItem` to `StatusPending` and **re-injecting duplicate `🔔 Proactive Alert` cards into `[HQ] Front Desk` up to 5 times** (wasting LLM tokens on repeated Front Desk wakeups). Conversely, ignoring bridge errors after `StatusDelivered` would drop urgent alerts permanently on transient network blips. Channel bridges therefore require their own **per-bridge bounded retry queue** keyed by deterministic `idempotency_key` (`outbox:<item.ID>`), decoupled from `OutboxItem.Status`.

### 3.4 Outbox Formatting, Quiet Hours & Anti-Cue Scrubbing (`internal/outbox/`)

- **`InQuietHours` & Critical Breaches ([`internal/outbox/outbox.go`](../../internal/outbox/outbox.go#L246-L271), [`dispatcher.go`](../../internal/outbox/dispatcher.go#L143-L145)):**
  - `LoadAttentionConfig` parses `attention.quiet_hours` (`start`, `end`, `tz`/`timezone`, `allow_critical_breach`).
  - Tainted items (`it.Tainted == true`) have `Force = false` and `Critical = false` enforced mechanically in `EvaluateDelivery` (`outbox.go:470–474`), guaranteeing untrusted external inputs can never pierce quiet hours as fake critical breaches.
- **`SanitizeAntiCue` ([`internal/outbox/dispatcher.go`](../../internal/outbox/dispatcher.go#L24-L47)):**
  - Strips control characters (`< 0x20` except `\n`, `\t`), neutralizes `</hq_system_notification>` fence tags, redacts imperative command cues (`run hq approve`, `execute hq propose`, etc.) and prompt-injection phrases (`ignore previous instructions`), and truncates safely on UTF-8 rune boundaries.
  - **Finding `R4` (Link-Unfurl Beacon Vector on Tainted Summaries):** Many external chat clients automatically fetch URL previews (leaking recipient IP/metadata or triggering tracking beacons if an attacker embedded a URL inside a summarized external message). For notifications with `tainted_origin: true`, `internal/channels/` should additionally defang `http://` / `https://` schemes (`hxxp://` / `hxxps://`) alongside `SanitizeAntiCue`.

### 3.5 Human-Presence Approval Verification (`internal/approvals/presence.go`)

- **Transcript-Verified Capability Minting ([`internal/approvals/presence.go`](../../internal/approvals/presence.go#L261-L407)):**
  - When an agent calls `ask_question` referencing `<propID>`, `PreToolUse` (`hooks.go:900–903`) calls `RecordAsk`, generating a 6-character `NonceCode` in `<vault>/private/approval_asks/<propID>.json` (`0600`, `24h` TTL) and injecting `(Recommended) Approve (<propID> · sha256:<short> · code:<NonceCode>)` into `ask_question` options.
  - When `hq approve <propID> --sha256=<prefix>` runs, `PreToolUse` (`hooks.go:1392`) calls `VerifyAskAndMintCap`, which scans the tail of `transcript.jsonl` strictly for steps matching:
    ```go
    if st.Status != "DONE" || st.Source != "MODEL" || st.Type != "GENERIC" {
        continue
    }
    ```
  - Lines `337–340` enforce a foundational Ring-0 invariant:
    > *"Never trust USER_EXPLICIT / USER_INPUT steps (forgeable via SendUserCascadeMessage). Real ask_question user selections are recorded as MODEL + GENERIC + DONE tool results."*
- **Audit Takeaway:**
  1. Plain-text inbound chat messages (`"approve prop-..."`) injected via `SendUserCascadeMessage` or `agentapi send-message` land in `transcript.jsonl` as `USER_INPUT` and **cannot** satisfy `VerifyAskAndMintCap`. This is a feature, not a bug: it prevents any compromised sidecar or prompt-injected message stream from forging human approval.
  2. Interactive bridges whose host runtime brokers native `ask_question` prompt cards produce genuine `MODEL` + `GENERIC` + `DONE` tool completions and work seamlessly with `VerifyAskAndMintCap`. Bridges without native `ask_question` broker support surface the `prop-...` ID as a notification and rely on Antigravity Remote Control or `:4242` Web Console (`ApproveFromTrustedDaemon`, `presence.go:410`) as the universal 1-tap fallback.
  3. `internal/channels/` must **never** read `<vault>/private/approval_asks/` or include `NonceCode` / `rawCap` values in `hq-channel-bridge/v1` payloads.

### 3.6 Credential Vault Guards, Quarantine Isolation & Lock Hierarchy (`internal/hooks/`, `internal/sandbox/`, `internal/store/`)

- **All-Session `private/` Guard ([`internal/hooks/hooks.go`](../../internal/hooks/hooks.go#L646), `L1276–1510`):**
  - `protectedPrivateSubs` (`L1276–1279`) and `reLocalCredentialShell` (`L646`) block `read_file`, `write_file`, `edit_file`, `grep_search`, `find_by_name`, and `run_command` access across **all** sessions (including untainted Front Desk and coding tasks) for listed subdirectories under `<vault>/private/`, while `private/ledger/` remains readable by untainted finance skills (`ci/harness.go:656–665`).
  - Adding `"channels"` to `protectedPrivateSubs` and `reLocalCredentialShell` makes `<vault>/private/channels/<bridge-id>.json` (`0600`) mechanically invisible to every LLM session, while [`internal/sandbox/sandbox.go`](../../internal/sandbox/sandbox.go#L250-L270) already masks the entire `<vault>/private` directory via `--tmpfs` (`bwrap`) / Seatbelt `(deny file-read* file-write*)`.
  - Similarly, adding `filepath.Join(canonVault, "memory", "channels")` to the `write_file`/`edit_file`/shell integrity list (`hooks.go:1525–1549`) prevents LLMs from mutating bridge cursor/thread state or forging `channels.jsonl` audit records.
- **Lock Ordering (`internal/store/store.go`):**
  - Current lock ranks ([`internal/store/store.go`](../../internal/store/store.go#L23-L32)): `RankExtensions (10) < RankTriggerCur (20) < RankWatcher (30) < RankTimers (35) < RankEvents (40) < RankGoals (50) < RankOutbox (60) < RankTaint (70)`.
  - Because `extension.VerifyTrustedBinary` can emit a mismatch notice via `outbox.Add` (`RankOutbox = 60`, `trust.go:224`), `internal/channels/` must (a) assign `RankChannels = 65` (`RankOutbox < RankChannels < RankTaint`), and (b) **never hold `RankChannels` across `VerifyTrustedBinary` or `connector.RunSubprocess`**, using the same 3-phase claim -> unlocked exec -> commit pattern as `outbox.DispatchPendingInterrupts`.

---

## 4. Architectural Synthesis: Three Distinct Abstractions & Step 1 / Step 2 Split

A central finding of this audit is that `agy-hq` requires **three orthogonal external integration primitives**, each with its own trust and provenance model:

| Primitive | Direction & Cardinality | Trust & Taint Model | Protocol / Surface |
| :--- | :--- | :--- | :--- |
| **1. Data Connectors** (`RFC-0017a`) | Multi-sender inbound polling + approval-gated outbound drafts/sends (email inboxes, team chat rooms, issue trackers) | **Untrusted inbound (`TaintOnSpawn: true`)** -> isolated `[Auto]` quarantined reader; **never** touches Front Desk directly | `hq-connector-protocol/v1` + `jsonmap` (`components.connectors`) |
| **2. Zero-Body Push Ping** (`RFC-0020` §4.3) | Stateless 1-way lock-screen badge (`ntfy`, Pushover) | Third-party push relay -> **zero-body** opaque badge only; no threads or rotation state | Simple HTTPS webhook (`attention.mobile_push`) |
| **3. Front Desk Channel Bridge** (`RFC-0023`) | Single-owner Front Desk control mirror: `bind-desk` shift rotation + threaded `notify` digests (`desk_thread` & `item_threads`) + `status` health probe | SHA-256-pinned companion binary (`private/trust.json`), `0600` LLM-invisible `<vault>/private/channels/<bridge-id>.json`, anti-cue scrubbed digests; inbound owner auth stays strictly inside the trusted external sidecar | **`hq-channel-bridge/v1`** (`components.channel_bridges`) |

### Clean Step 1 (`agy-hq` Core) vs. Step 2 (External Extension Pack) Boundary

- **Step 1 (`agy-hq` Public Core — Specified in [RFC-0023](../design/0023-front-desk-channel-bridge.md)):**
  - Owns the vendor-neutral `channel_bridges` manifest schema in `hq-extension.json`, SHA-256 binary pinning in `<vault>/private/trust.json`, `hq-channel-bridge/v1` JSON-over-stdio protocol (`bind-desk`, `notify`, `status`), `internal/channels/` runtime state & retry queue, `<vault>/private/channels/<bridge-id>.json` (`0600`) secret isolation, `fastTicker` Front Desk rotation detection, `DispatchOutboxToFrontDesk` + optional task `done`/`blocked` fan-out, shared quiet-hours / rate-cap / `SanitizeAntiCue` policy enforcement, `channels.jsonl` SHA-256 audit logging, and `hq channel` / `hq doctor` CLI tooling.
  - Contains **zero** vendor SDKs, zero chat-platform REST/RPC clients, zero bot credentials, and zero organization-specific logic.
- **Step 2 (External Companion Extension Pack — Separate Repository / Plugin):**
  - Ships as a standard Antigravity plugin with an untouched `plugin.json` and a root `hq-extension.json` registering `components.channel_bridges`.
  - Implements the external interactive messaging sidecar / bot adapter: authenticates single-owner inbound messages against a pinned owner identity, routes verified owner turns to the active `[HQ] Front Desk` `ConversationID` maintained by `bind-desk` (honoring `lease_state: "vacated"`), renders native `ask_question` prompt cards where supported by the host runtime, and translates `hq-channel-bridge/v1` `bind-desk` / `notify` / `status` calls into platform-native threads and cards.

---

## 5. Summary of Design Recommendations for `RFC-0023`

1. **Extend `hq-extension.json` with `components.channel_bridges`:** Validate bridge declarations strictly in `LintPack` (`protocol == "hq-channel-bridge/v1"`, non-shell `command[0]`, scrubbed `env_allow`), pin `command[0]` in `<vault>/private/trust.json`, and execute via `VerifyTrustedBinary` + `connector.RunSubprocess` (`/dev/fd/3` for optional `credential_file` bytes).
2. **Define `hq-channel-bridge/v1` (`bind-desk`, `notify`, `status`):** Carry deterministic `idempotency_key`s, explicit `lease_state` (`active` vs `vacated`), `desk_thread` + `open_item_threads` rebinding on shift handovers, `inbound_tunnel_bound_cid` drift detection in `status`, and a 4-class typed error taxonomy (`transient`, `rate_limited`, `auth_expired`, `permanent`).
3. **Build `internal/channels/` with `RankChannels = 65` & LLM-Invisible Config:** Store destination routing in `<vault>/private/channels/<bridge-id>.json` (`0600`, guarded by `PreToolUse` across all sessions) and runtime state (`last_desk_cid`, `desk_thread`, `item_threads`, bounded 64-item retry queue) in `<vault>/memory/channels/<bridge-id>.json` (`0600`, write-guarded in `PreToolUse`), logging SHA-256-hashed audit entries to `<vault>/memory/channels/channels.jsonl`.
4. **Decouple Daemon Rotation & Outbox Fan-Out:** Switch `saveFrontDeskLease` to `store.WriteAtomic`, detect `front_desk.json` `ConversationID` transitions (including `old_cid -> ""` vacations and sidecar `status` drift) on `fastTicker` (`15s`), fan out `TierInterrupt` alerts (even across brief desk vacancies without duplicate Front Desk wakeups) plus opt-in rate-capped Layer-1 task `done`/`blocked` digests, and enforce `InQuietHours` + `SanitizeAntiCue` + tainted URL defanging uniformly in Go.
