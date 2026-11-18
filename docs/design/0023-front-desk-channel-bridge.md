# RFC-0023: Vendor-Neutral Front Desk Channel Bridge & Rotation Tunneling

- **Status:** Implemented
- **Created:** 2026-11-17
- **Companion Research:** [Research 18: Vendor-Neutral Front Desk Channel Bridge, Rotation Tunneling & Security Boundary Audit](../research/18_channel_bridge_front_desk_audit.md)
- **Extends / Updates:** [RFC-0004](./0004-heartbeat-and-channels.md), [RFC-0007](./0007-front-desk-initialization-hardening.md), [RFC-0008](./0008-gchat-integration.md), [RFC-0017a](./0017a-connector-protocol-and-watcher-engine.md), [RFC-0017b](./0017b-provenance-taint-and-offer-gateway.md), [RFC-0017d](./0017d-enterprise-extensibility-and-portability.md), [RFC-0020](./0020-proactive-ambient-synthesis-and-mobile-push.md), [RFC-0021](./0021-customizable-approval-policy-and-ux.md)

---

## 1. Motivation & Problem Statement

`agy-hq` coordinates personal and engineering operations through a single leased **`[HQ] Front Desk`** intake conversation (`<vault>/memory/front_desk.json`), complemented by Antigravity Desktop, Antigravity Remote Control, and the local `:4242` Web Console. Many users also operate an external interactive messaging sidecar or chat bot extension (e.g., an enterprise chat bot, self-hosted messenger bridge, or private mobile bot DM) to receive urgent `TierInterrupt` alerts, track task completion/blockers in grouped threads, and send verified owner instructions back to `[HQ] Front Desk`.

As documented in [Research 18](../research/18_channel_bridge_front_desk_audit.md), five architectural gaps (`G1`–`G5`) and four runtime hazards (`R1`–`R4`) currently prevent clean, vendor-neutral Front Desk bridging:

1. **No Vendor-Neutral Companion Bridge Contract (`G1`, `G3`):** [RFC-0004](./0004-heartbeat-and-channels.md) sketched compiled-in Go vendor drivers (`internal/channels/*.go`) reading destination IDs from LLM-readable `<vault>/config.yaml` (`0644`). Compiling chat bot SDKs or enterprise RPC transports into `bin/hq` violates `ADR-0002` (`0` external Go dependencies) and [RFC-0017d](./0017d-enterprise-extensibility-and-portability.md), while [`internal/extension/pack.go`](../../internal/extension/pack.go) lacks a `channel_bridges` manifest component.
2. **Post-Rotation Front Desk Tunnel Split-Brain (`R1`, `R2`):** `[HQ] Front Desk` rotates its `ConversationID` across `hq claim-desk`, `hq release-desk`, `hq adopt` (which demotes the outgoing Front Desk into a `[Task:<scope>]` coding thread), and fresh-Vault bootstrap. Today `hq daemon` never detects `front_desk.json` `ConversationID` changes or notifies external sidecars—leaving external chat sessions pinned to stale or demoted conversations and orphaning open proposal/alert threads.
3. **Outbox Retry Coupling & Desk-Vacancy Blindness (`R3`):** [`DispatchOutboxToFrontDesk`](../../internal/kernel/events_dispatch.go#L481-L495) exits early when `front_desk.json` is vacant and couples delivery status to `DeliverMessageToSession`. Synchronously coupling an external bridge inside `DispatchPendingInterrupts` would either re-inject duplicate `🔔 Proactive Alert` cards into `[HQ] Front Desk` on transient chat-API `429`/`503` errors or drop external alerts permanently.
4. **Connector vs. Control-Bridge Provenance Boundary (`G2`, `R4`):** Multi-sender `connectors` (`hq-connector-protocol/v1`) ingest untrusted external inputs into `TaintOnSpawn: true` quarantined workers and must **never** route into `[HQ] Front Desk`. Conversely, outbound bridge notifications derived from tainted triage workers must strip command/injection cues (`SanitizeAntiCue`) and defang URLs so external chat clients do not auto-fetch attacker tracking beacons.
5. **Human-Presence Approval Integrity & Standalone Bot Round-Trip (`G5`, `G6`):** [`VerifyAskAndMintCap`](../../internal/approvals/presence.go#L337-L341) intentionally rejects plain-text `USER_INPUT` messages so injected chat strings cannot forge `hq approve`. Meanwhile, standalone messaging bots (such as Telegram, Slack, WhatsApp, Discord, or Matrix companion packs) run outside the desktop UI and cannot open loopback `:4242` from a phone on cellular or safely scrape internal `transcript.jsonl` logs for replies. Channel bridges therefore need (a) a daemon-authenticated inbound turn + `HandleStop` `desk_reply` mirroring loop, and (b) optional cryptographically signed one-shot button callback tokens (`capabilities.callback_buttons`) alongside native `ask_question` cards and `:4242` Web Console fallback.

---

## 2. Design Goals & Non-Goals

### Goals
1. **100% Vendor-Neutral Core Contract (`channel_bridges` + `hq-channel-bridge/v1`):** Allow any companion extension pack (`hq-extension.json` `components.channel_bridges`) to register a SHA-256-pinned executable speaking a 3-verb JSON-over-stdio protocol (`bind-desk`, `notify`, `status`), verified via [`VerifyTrustedBinary`](../../internal/extension/trust.go#L304).
2. **Seamless Front Desk Rotation Tunneling (`<= 15s` Convergence):** Detect `front_desk.json` `ConversationID` transitions (`"" -> new_cid`, `old_cid -> new_cid`, `old_cid -> ""` vacation, and external sidecar restart drift) on `hq daemon`'s `fastTicker` (`15s`) and `wakeCh`, invoking `bind-desk` to rebind the pinned `desk_thread` and open `item_threads` in place.
3. **Zero-Scraping Bidirectional Owner Turn & 1-Tap Button Callback Gateways:** Provide daemon-authenticated `POST /api/channels/inbound` (`hq channel inbound`) + zero-token `HandleStop` `desk_reply` mirroring so standalone bot sidecars never touch internal `transcript.jsonl` files, plus optional one-shot 192-bit callback tokens (`bca_...` / `bcr_...` via `POST /api/channels/callback` / `hq channel callback`) for platforms with interactive buttons (e.g., Telegram Inline Keyboards, Slack Block Kit, WhatsApp Interactive Buttons).
4. **LLM-Invisible Destination & Callback Secrets (`0600`) & Ranked Runtime State (`RankChannels = 65`):** Isolate destination routing/credentials in `<vault>/private/channels/<bridge-id>.json` (`0600`) and hashed button capabilities in `<vault>/private/approval_bridge_caps/<token-sha256>.json` (`0600`), guarded across all LLM sessions by `PreToolUse` and OS sandboxes, and persist decoupled per-bridge state (`last_desk_cid`, `desk_thread`, LRU `item_threads`, `pending_replies`, bounded 64-item retry queue) in `<vault>/memory/channels/<bridge-id>.json` (`0600`).
5. **Unified Policy, Anti-Cue Scrubbing & SHA-256 Audit Trail:** Enforce `InQuietHours`, non-urgent hourly rate caps, UTF-8 rune truncation, `SanitizeAntiCue` + tainted URL defanging, and append-only SHA-256 audit logging (`<vault>/memory/channels/channels.jsonl`) uniformly in Go.
6. **Crisp Step 1 / Step 2 Separation:** Keep `agy-hq` core free of vendor chat clients while providing a complete contract for external companion packs (Step 2)—spanning both host-integrated sidecars and standalone Telegram/Slack/WhatsApp bots—to operate bi-directionally without patching core.

### Non-Goals
- **No Compiled-In Third-Party Chat Bot SDKs or Cloud Relays:** `go.mod` remains 100% pure Go stdlib (`ADR-0002` / CI Gate `H4-A`), and Vault data never transits an `agy-hq`-operated server (`ADR-0003`).
- **No Multi-Sender Inbound Polling via `channel_bridges`:** Multi-party email/chat spaces continue using `connectors` + `hq watch precheck` (`TaintOnSpawn: true`). `hq-channel-bridge/v1` has **no** `poll` verb.
- **No Plain-Text Chat `hq approve` Shortcut:** Typing `"approve"` in a plain-text chat message never mints an approval capability; human-presence verification requires either a genuine `ask_question` tool completion (`VerifyAskAndMintCap`), a single-use cryptographic `bca_...` button token minted exclusively for a `callback_buttons` bridge binary, or `:4242` Web Console CSRF verification (`ApproveFromTrustedDaemon`).
- **No Standalone `<vault>/bridges/` Discovery Namespace (`YAGNI`):** Because bidirectional Front Desk bridges pair with a companion sidecar/bot daemon, discovery scopes strictly to `hq-extension.json` companion packs (`<pack>/<id>`); simple 1-way lock-screen webhooks stay on [RFC-0020](./0020-proactive-ambient-synthesis-and-mobile-push.md) `mobile_push`.

---

## 3. Architecture Overview

```mermaid
flowchart TD
    subgraph Trust["Ring-0 Trust & Private Secrets (0600, LLM-Invisible)"]
        EXT["Companion Pack hq-extension.json\ncomponents.channel_bridges"]
        TR["<vault>/private/trust.json\nManifestSHA256 + VerifyTrustedBinary"]
        PCFG["<vault>/private/channels/<id>.json\nDestination & Policy"]
        BCAP["<vault>/private/approval_bridge_caps/\nSHA-256 One-Shot Button Caps"]
    end

    subgraph Core["hq daemon, WebUI Loopback & internal/channels (Pure Go Stdlib)"]
        FD["<vault>/memory/front_desk.json\nAtomic Lease (SetFrontDesk / ReleaseFrontDesk)"]
        OB["DispatchOutboxToFrontDesk + HandleStop\nTierInterrupt + Task blocked/done + desk_reply"]
        GW["Daemon-Token Gateways (:4242 / CLI)\n/api/channels/inbound & /api/channels/callback"]
        CH["internal/channels Engine (RankChannels=65)\n• Rotation Diff + PendingReplyCorrelation\n• InQuietHours + Rate Cap + SanitizeAntiCue\n• Bounded Retry Queue + channels.jsonl Audit"]
    end

    subgraph Subproc["Scrubbed Subprocess (Setpgid + /dev/fd/3)"]
        BR["External Channel Bridge Binary\nProtocol: hq-channel-bridge/v1\nOps: bind-desk | notify | status"]
    end

    subgraph Step2["External Messaging Sidecar / Bot Pack (Step 2)"]
        ROOM["Pinned desk_thread &\nGrouped item_threads"]
        AUTH["1:1 Verified Owner Auth\n+ Native ask_question Cards or Inline Buttons"]
    end

    EXT --> TR --> CH
    PCFG --> CH
    BCAP <--> CH
    FD -->|"15s fastTicker / wakeCh"| CH
    OB -->|"Sanitized Outbox, Task & desk_reply"| CH
    CH -->|"stdin JSON + optional fd3 secret"| BR
    BR --> ROOM
    AUTH -->|"Bearer daemon_token (inbound / callback)"| GW
    GW --> CH
    GW -->|"Deliver owner turn / 0-token approve"| FD
```

### Front Desk Shift Handover & Bidirectional Turn / Button Callback Sequence

```mermaid
sequenceDiagram
    participant U as Owner (Desktop / Mobile Chat)
    participant S as External Messaging Sidecar / Bot
    participant D as hq daemon (:4242 + fastTicker)
    participant C as internal/channels (RankChannels=65)
    participant B as Bridge Binary (hq-channel-bridge/v1)
    participant FD as [HQ] Front Desk + Stop Hook

    Note over U,FD: 1. Front Desk Shift Rotation (`cid_old` -> `cid_new`)
    U->>FD: Start new shift (`hq claim-desk --conv=cid_new`)
    D->>C: Detect `last_desk_cid != cid_new` -> VerifyTrustedBinary
    C->>B: `op: "bind-desk"` (`cid_old` -> `cid_new`, `desk_thread`, `open_item_threads`)
    B->>S: Rebind `desk_thread` + open `item_threads` in place
    B-->>C: `{"ok":true,"desk_thread":{"external_thread_id":"thr_desk_01","rebound":true}}`

    Note over U,FD: 2. Bidirectional Mobile Turn + 1-Tap Button Approval
    U->>S: Send DM in `thr_desk_01` (1:1 owner principal verified)
    S->>D: `POST /api/channels/inbound` (`Bearer daemon_token`, `thr_desk_01`, `text`)
    D->>C: Dedup + record `PendingReplyCorrelation(cid_new, thr_desk_01)`
    D->>FD: Deliver owner prompt to `cid_new`
    FD->>FD: Stage `hq propose` (`prop-123`) + finish turn (`HandleStop`)
    FD->>C: `EnqueueTurnReplyIfCorrelated` (`kind: "desk_reply"`, `prop-123`)
    C->>C: Mint one-shot `bca_<32>` / `bcr_<32>` -> store `SHA-256` in `approval_bridge_caps/`
    C->>B: `op: "notify"` (`kind: "desk_reply"`, `actions: [{approve_callback_token, reject_callback_token}]`)
    B->>S: Post reply + `[Approve]` / `[Reject]` inline buttons in `thr_desk_01`
    U->>S: Tap `[Approve]` button (1:1 owner principal verified)
    S->>D: `POST /api/channels/callback` (`Bearer daemon_token`, `callback_token: "bca_..."`)
    D->>C: Atomic `os.Rename` claim `<sha256>.json` -> `ApproveFromTrustedDaemon(prop-123)` (0 LLM tokens)
```

---

## 4. Vendor-Neutral `channel_bridges` Extension Contract & `hq-channel-bridge/v1` Protocol

### 4.1 `hq-extension.json` Manifest Schema & Binary Trust Pinning

Extend `PackComponents` in [`internal/extension/pack.go`](../../internal/extension/pack.go#L120-L131) with `ChannelBridges []ChannelBridgeSpec`:

```json
{
  "schema_version": 1,
  "id": "acme-messaging",
  "version": "1.0.0",
  "min_hq_version": "0.3.1",
  "requires": {
    "bins": [
      { "name": "acme-chat-bridge", "probe_argv": ["acme-chat-bridge", "--version"] }
    ]
  },
  "components": {
    "channel_bridges": [
      {
        "id": "desk-bridge",
        "protocol": "hq-channel-bridge/v1",
        "description": "Bidirectional Front Desk rotation & notification bridge",
        "command": ["acme-chat-bridge", "channel-bridge"],
        "env_allow": ["ACME_CHAT_PROFILE"],
        "requires_desktop_session": false,
        "timeout_ms": 5000,
        "max_body_runes": 2000,
        "capabilities": {
          "desk_binding": true,
          "item_threads": true,
          "task_digests": true,
          "native_ask_cards": false,
          "callback_buttons": true
        }
      }
    ]
  }
}
```

#### Go Types & `LintPack` / `BuildTrustBundle` Enforcement
```go
// BridgeCapabilitiesSpec declares optional capabilities supported by a channel bridge.
type BridgeCapabilitiesSpec struct {
	DeskBinding     bool `json:"desk_binding,omitempty"`
	ItemThreads     bool `json:"item_threads,omitempty"`
	TaskDigests     bool `json:"task_digests,omitempty"`
	NativeAskCards  bool `json:"native_ask_cards,omitempty"`
	CallbackButtons bool `json:"callback_buttons,omitempty"`
}

// ChannelBridgeSpec declares a vendor-neutral Front Desk channel bridge in hq-extension.json.
type ChannelBridgeSpec struct {
	ID                     string                 `json:"id"`
	Protocol               string                 `json:"protocol"` // must be "hq-channel-bridge/v1"
	Description            string                 `json:"description,omitempty"`
	Command                []string               `json:"command"`
	EnvAllow               []string               `json:"env_allow,omitempty"`
	RequiresDesktopSession bool                   `json:"requires_desktop_session,omitempty"`
	TimeoutMs              int                    `json:"timeout_ms,omitempty"`
	MaxBodyRunes           int                    `json:"max_body_runes,omitempty"`
	Capabilities           BridgeCapabilitiesSpec `json:"capabilities,omitempty"`
}
```

1. **Strict Static Validation (`LintPack` in [`internal/extension/pack.go`](../../internal/extension/pack.go#L197)):**
   - `id` matches `^[a-z0-9][a-z0-9-]{1,62}$` and is unique within the pack; runtime FQID is `<pack>/<id>` (e.g., `acme-messaging/desk-bridge`).
   - `protocol` must equal `"hq-channel-bridge/v1"`.
   - `command` must be non-empty; `command[0]` must **not** be a shell or generic interpreter wrapper (`sh`, `bash`, `zsh`, `dash`, `ksh`, `ssh`, `env`, `xargs`, `python*`, `node`, `perl`, `ruby`), ensuring `VerifyTrustedBinary` hashes the actual compiled bridge executable rather than `/bin/sh`.
   - `env_allow` cannot include any key in `unconditionallyBlockedEnv` (`ANTIGRAVITY_CSRF_TOKEN`, `ANTIGRAVITY_LS_ADDRESS`, `ANTIGRAVITY_AGENTAPI_EXE`, `ANTIGRAVITY_CONVERSATION_ID`, `ANTIGRAVITY_TRAJECTORY_ID`, `ANTIGRAVITY_PROJECT_ID`, `ANTIGRAVITY_SOURCE_METADATA`).
   - `EnablePack` (`pack.go:474`) rejects unqualified `id` collisions across enabled packs unless `<vault>/extensions.json` `winner_overrides[<id>]` names a winner.
2. **SHA-256 Pinning & Execution Cache ([`internal/extension/trust.go`](../../internal/extension/trust.go)):**
   - `BuildTrustBundle` (`trust.go:617`) iterates `manifest.Components.ChannelBridges`, resolves `cb.Command[0]` via `pinBinaryRef`, and records `{raw_ref, resolved_path, sha256}` in `<vault>/private/trust.json` (`Targets["pack:<packID>"].Binaries`), alongside `ManifestSHA256` over `hq-extension.json`.
   - Before every bridge invocation, `internal/channels/` verifies `hq-extension.json` (`LoadActiveTrustedPacks`) and calls `extension.VerifyTrustedBinary(vaultDir, sourceID, cb.Command[0])`, executing the immutable `0500` binary staged in `<vault>/private/exec-cache/<sha256>/<basename>` via [`connector.RunSubprocess`](../../internal/connector/exec.go#L271). Any hash mismatch fails closed for that bridge and emits at most one deduplicated `brief_rollup` re-trust notice.

---

### 4.2 `hq-channel-bridge/v1` Wire Protocol

All operations execute as bounded, short-lived stdio calls (`default timeout = 5s`, `max = 15s`, `Setpgid: true` process-group kill on timeout):
- **`stdin`:** Single UTF-8 JSON request object (zero routing IDs, message bodies, or metadata ever appear on CLI `argv` / `/proc/<pid>/cmdline`).
- **`/dev/fd/3` (`ExtraFiles[0]`):** Optional read-only pipe carrying raw bytes from `<vault>/private/channels/<bridge-id>.json`'s `credential_file` when configured (`has_credential_fd3: true`), eliminating temp-file TOCTOU and environment-variable secret exposure.
- **`stdout`:** Single UTF-8 JSON response object (`<= 64 KiB`).
- **`stderr`:** Diagnostic text only; scrubbed via [`connector.SanitizeStderr`](../../internal/connector/exec.go#L216) (`<= 400` runes, `<plugin-card>` tags and control characters stripped).

#### Operation 1: `bind-desk` (Initial Claim, Shift Rotation & Vacation)

**Request (`stdin`):**
```json
{
  "protocol": "hq-channel-bridge/v1",
  "op": "bind-desk",
  "bridge_id": "acme-messaging/desk-bridge",
  "idempotency_key": "bind-desk:acme-messaging/desk-bridge:00001111:9a41bc10:2026-11-17T21:15:00Z",
  "timestamp": "2026-11-17T21:15:02Z",
  "has_credential_fd3": true,
  "silent": false,
  "destination": {
    "room_id": "room_01HXYZ9876",
    "account": "work"
  },
  "desk": {
    "lease_state": "active",
    "conversation_id": "9a41bc10-7712-4c11-8a00-112233445566",
    "previous_conversation_id": "00001111-2222-4333-8444-555566667777",
    "project_id": "proj_default",
    "workspace_name": "agy-hq",
    "updated_at": "2026-11-17T21:15:00Z"
  },
  "desk_thread": {
    "external_thread_id": "thr_desk_01",
    "metadata": { "pinned": "true" }
  },
  "open_item_threads": [
    {
      "item_key": "proposal:prop-1794904000-a1b2c3d4e5f6",
      "kind": "proposal",
      "external_thread_id": "thr_item_42",
      "status": "open"
    }
  ]
}
```
- When `hq release-desk` or `hq adopt` vacates `front_desk.json` without an immediate successor, `desk.lease_state` is `"vacated"` and `desk.conversation_id` is `""`, instructing the external sidecar to **immediately unbind `previous_conversation_id`** so inbound chat messages are never misrouted into a demoted `[Task:<scope>]` session (`Finding R1`).
- During quiet hours, `silent: true` instructs the bridge to update its internal routing table silently without posting a visible chat banner that would buzz the user's device.

**Response (`stdout`):**
```json
{
  "protocol": "hq-channel-bridge/v1",
  "ok": true,
  "deduplicated": false,
  "desk_thread": {
    "external_thread_id": "thr_desk_01",
    "permalink": "https://chat.example.com/room/room_01HXYZ9876/thread/thr_desk_01",
    "rebound": true,
    "metadata": { "pinned": "true" }
  },
  "item_threads": [
    {
      "item_key": "proposal:prop-1794904000-a1b2c3d4e5f6",
      "external_thread_id": "thr_item_42",
      "rebound": true
    }
  ]
}
```

#### Operation 2: `notify` (Interrupts, `desk_reply` Mirroring, Task Digests & Signed Button Actions)

**Request (`stdin`):**
```json
{
  "protocol": "hq-channel-bridge/v1",
  "op": "notify",
  "bridge_id": "acme-messaging/desk-bridge",
  "idempotency_key": "outbox:idea-1794904100-7f8e9d0c",
  "timestamp": "2026-11-17T21:16:05Z",
  "has_credential_fd3": true,
  "destination": {
    "room_id": "room_01HXYZ9876",
    "account": "work"
  },
  "desk_conversation_id": "9a41bc10-7712-4c11-8a00-112233445566",
  "thread_hint": {
    "mode": "item_thread",
    "desk_external_thread_id": "thr_desk_01",
    "item_key": "proposal:prop-1794904000-a1b2c3d4e5f6",
    "existing_external_thread_id": "thr_item_42"
  },
  "notification": {
    "kind": "interrupt",
    "severity": "high",
    "source_skill": "inbox-triage",
    "scope": "watch:gmail:work",
    "title": "Actionable reply needed on Q4 partner contract",
    "body_markdown": "Counterparty requested sign-off before 5:00 PM PST. Staged reply draft for review.",
    "proposal_ids": ["prop-1794904000-a1b2c3d4e5f6"],
    "actions": [
      {
        "proposal_id": "prop-1794904000-a1b2c3d4e5f6",
        "action_type": "email_send",
        "summary": "Reply to Q4 partner contract thread",
        "sha256_prefix": "9f86d081884c",
        "approve_callback_token": "bca_7Xq9LmN2pR4tV8wY1zB3cD5fG6hJ0kM_",
        "reject_callback_token": "bcr_2Ab4CdE6fG8hJ0kL3mN5pQ7rS9tU1vW-",
        "expires_at": "2026-11-18T21:16:00Z"
      }
    ],
    "task_slug": "",
    "tainted_origin": true
  }
}
```
- Supported `notification.kind` values: `"interrupt"`, `"critical_breach"`, `"desk_reply"`, `"task_done"`, `"task_blocked"`, `"test_ping"`.
- Supported `thread_hint.mode` values: `"item_thread"` (open or reply inside `existing_external_thread_id` keyed by `item_key`, when `capabilities.item_threads == true`) or `"desk_thread"` (post inside `desk_external_thread_id`).
- **Compact Signed Button Callback Tokens (`notification.actions`):** Populated **only** when `capabilities.callback_buttons == true`, `policy.privacy_mode != "zero_body"`, and `notification.proposal_ids` references one or more unexpired pending proposals in `<vault>/memory/pending/`. Each `approve_callback_token` (`bca_<32 base64url chars>`) and `reject_callback_token` (`bcr_<32 base64url chars>`) encodes `192` bits of `crypto/rand` entropy in exactly **`36` ASCII bytes**—fitting directly inside Telegram's `64`-byte `InlineKeyboardButton.callback_data` ceiling as well as Slack Block Kit `button.value`, WhatsApp Interactive Button `id`, and Discord `custom_id` fields without requiring a stateful token-mapping table in the bot sidecar.

**Response (`stdout`):**
```json
{
  "protocol": "hq-channel-bridge/v1",
  "ok": true,
  "delivery_id": "msg_998120",
  "deduplicated": false,
  "external_thread_id": "thr_item_42",
  "desk_external_thread_id": "thr_desk_01"
}
```

#### Operation 3: `status` (Health, Auth & Sidecar Drift Probe)

**Request (`stdin`):**
```json
{
  "protocol": "hq-channel-bridge/v1",
  "op": "status",
  "bridge_id": "acme-messaging/desk-bridge",
  "timestamp": "2026-11-17T21:20:00Z",
  "has_credential_fd3": true,
  "destination": { "room_id": "room_01HXYZ9876", "account": "work" },
  "desk_conversation_id": "9a41bc10-7712-4c11-8a00-112233445566",
  "desk_external_thread_id": "thr_desk_01"
}
```

**Response (`stdout`):**
```json
{
  "protocol": "hq-channel-bridge/v1",
  "ok": true,
  "healthy": true,
  "auth_state": "authenticated",
  "inbound_tunnel_bound_cid": "9a41bc10-7712-4c11-8a00-112233445566",
  "detail": "Connected to room_01HXYZ9876 (owner principal verified)"
}
```
- **Self-Healing Sidecar Drift Check:** If an external interactive sidecar restarts independently and loses its in-memory binding (`inbound_tunnel_bound_cid != fd.ConversationID`), `hq channel status` / periodic daemon health checks detect the mismatch and automatically enqueue an idempotent `bind-desk` call.

#### Typed Error Taxonomy & End-to-End Idempotency

When `ok == false` (or exit status `!= 0`), bridges return `error: {"code": "...", "message": "...", "retry_after_seconds": 0}`:

| Error `code` | Examples | `internal/channels/` Runtime Behavior |
| :--- | :--- | :--- |
| **`transient`** | Network timeout, `502`/`503`/`504`, subprocess deadline, unstructured non-zero exit | Retain item in `retry_queue` with bounded exponential backoff (`15s -> 1m -> 5m -> 15m -> 30m`, max `6` attempts). Emit 1 `brief_rollup` alert only on strike 3; downgrade exhausted item to `brief_rollup` on strike 6. |
| **`rate_limited`** | Provider HTTP `429` / burst quota | Retain item; schedule `next_attempt_at = now + clamp(retry_after_seconds, 5s, 1h)` (defaulting to exponential backoff) without tripping permanent pause. |
| **`auth_expired`** | Expired OAuth/SSO token or revoked bot session | Pause bridge dispatches (`health.status = "paused_auth"`) until `hq channel status`/`test` or refreshed `credential_file` mtime clears it; emit **at most one** `brief_rollup` re-auth notice per episode. |
| **`destination_not_found`** / **`permission_denied`** / **`invalid_request`** / **`permanent`** | Deleted chat room, bot removed from room, malformed schema | **Permanent classification:** drop item immediately (`0` futile retries). For destination-wide codes (`destination_not_found`, `permission_denied`), pause bridge (`paused_permanent`) until `<vault>/private/channels/<bridge-id>.json` `config_sha256` or pack manifest hash changes, logging 1 `brief_rollup` diagnostic. |

- **Idempotency Guarantee:** Every mutating call (`bind-desk`, `notify`) carries a deterministic `idempotency_key` (`^[A-Za-z0-9._:/-]{8,160}$`). Bridges pass `idempotency_key` as the provider's client dedup key (returning `ok: true, deduplicated: true` on replays), while `internal/channels/` maintains a 256-entry `recent_delivered_keys` ring buffer so `hq daemon` never re-sends an already-delivered item.

---

## 5. `internal/channels/` Runtime, Private Config & Shared Policy Pipeline

### 5.1 LLM-Invisible Destination Config (`<vault>/private/channels/<bridge-id>.json`)

Destination coordinates and per-bridge policy live strictly inside `<vault>/private/channels/<safe-id>.json` (`0600`, where `<safe-id>` maps `<pack>/<id>` injectively to `<pack>__<id>.json` since `^[a-z0-9][a-z0-9-]{1,62}$` forbids `_`):

```json
{
  "schema_version": 1,
  "bridge_id": "acme-messaging/desk-bridge",
  "enabled": true,
  "credential_file": "private/channels/credentials/desk-bridge.token",
  "destination": {
    "room_id": "room_01HXYZ9876",
    "account": "work",
    "thread_mode": "grouped"
  },
  "policy": {
    "deliver_interrupts": true,
    "deliver_task_done": false,
    "deliver_task_blocked": true,
    "mirror_all_desk_turns": false,
    "respect_quiet_hours": true,
    "allow_critical_breach": true,
    "max_non_urgent_per_hour": 6,
    "max_body_runes": 1600,
    "privacy_mode": "sanitized_digest"
  }
}
```

- **Strict Data-Only Schema (`DisallowUnknownFields()`):** Any executable fields (`command`, `argv`, `path`, `env`) inside `<vault>/private/channels/*.json` are rejected at parse time (matching `connector/instance.json` in [RFC-0017a §3.1](./0017a-connector-protocol-and-watcher-engine.md)).
- **Three-Layer LLM Invisibility & Exfiltration Guard:**
  1. **`PreToolUse` Universal Credential Guard ([`internal/hooks/hooks.go`](../../internal/hooks/hooks.go#L646), `L1276–1510`):** Add `"channels"` and `"approval_bridge_caps"` to `protectedPrivateSubs` and `reLocalCredentialShell`, denying `read_file`, `write_file`, `edit_file`, `grep_search`, `find_by_name`, and `run_command` access to `<vault>/private/{channels,approval_bridge_caps}/**` across **all** sessions (untainted Front Desk, Task, Subagent, `[Auto]`, and tainted).
  2. **OS Credential Sandbox ([`internal/sandbox/sandbox.go`](../../internal/sandbox/sandbox.go#L250-L270)):** `<vault>/private` is already masked by `--tmpfs` (`bwrap`) and Seatbelt `(deny file-read* file-write*)` for all sandboxed commands.
  3. **Zero-Recipient CLI & Redacted Diagnostics:** `hq channel` outbound/test verbs accept **no `--to` / `--room` / `--url` flags**; `hq status`, `CONSOLE.md`, and `:4242` `/api/state` surface only `bridge_id`, `enabled`, `health.status`, and `dest_sha256[:12]` (never raw room IDs or webhook URLs). Modifying `<vault>/private/channels/<bridge-id>.json` from inside an agent session requires an approval-gated `hq channel apply-config` proposal (`rsi_apply` rule in [`templates/config.yaml`](../../templates/config.yaml#L113)).

### 5.2 Ranked Bridge State Store (`<vault>/memory/channels/<bridge-id>.json`, `RankChannels = 65`)

Runtime cursor, thread mapping, inbound reply correlation, and retry state live in `<vault>/memory/channels/<safe-id>.json` (`0600`, protected from direct LLM `write_file`/`edit_file` in `hooks.go:1525–1549`):

- **Lock Hierarchy ([`internal/store/store.go`](../../internal/store/store.go#L23-L32)):**
  - Define `RankChannels Rank = 65` (`RankOutbox (60) < RankChannels (65) < RankTaint (70)`), acquiring `<vault>/memory/channels/.channels.lock` via `store.With`.
  - **3-Phase Lock-Free Subprocess Execution:**
    - **Phase A (under `RankChannels`):** Inspect `front_desk.json` diff and `retry_queue`, claim due `PendingDeskBind` / `QueuedBridgeNotification` work items, and release `RankChannels`.
    - **Phase B (unlocked):** Resolve pack manifest, run `VerifyTrustedBinary` (which may safely call `outbox.Add` under `RankOutbox = 60` without rank inversion), read `<vault>/private/channels/<safe-id>.json`, mint one-shot `bca_`/`bcr_` button tokens in `<vault>/private/approval_bridge_caps/` when `capabilities.callback_buttons == true`, and execute `connector.RunSubprocess`.
    - **Phase C (under `RankChannels`):** Reload state, commit updated `last_desk_cid`, `desk_thread`, LRU `item_threads` (`<= 256` entries, `14d` TTL), `pending_replies` (`<= 32` entries, `30m` TTL), `recent_delivered_keys` & `recent_inbound_keys` (`<= 256` entries each, `72h` TTL), `rate_window`, and `retry_queue` (`<= 64` entries; on overflow, evict oldest `task_done` before `task_blocked`, never evicting `critical_breach` / `interrupt` / `desk_reply` ahead of background digests), and append audit lines to `channels.jsonl`.

### 5.3 Shared Policy Enforcement & `channels.jsonl` SHA-256 Audit Log

1. **Quiet-Hours Integration:** Evaluates [`outbox.InQuietHours`](../../internal/outbox/outbox.go#L246) when `policy.respect_quiet_hours` is `true` (default). During quiet hours, `task_done`/`task_blocked` digests roll into `/morning-brief` (`brief_rollup`), unsolicited `interrupt` items hold until quiet hours end unless `it.Critical && !it.Tainted && attCfg.QuietHours.AllowCriticalBreach && policy.allow_critical_breach`, `bind-desk` executes with `silent: true`, and owner-initiated `desk_reply` responses bypass quiet-hours suppression because the user just actively messaged the bot.
2. **Non-Urgent Rate Caps & Silent Task Filtering:**
   - Enforces `policy.max_non_urgent_per_hour` (default `6/hour`) across `task_done` and `task_blocked` notifications; rate-capped items coalesce into `brief_rollup`.
   - Enforces a runaway circuit-breaker ceiling (`30/hour`) on unsolicited notifications.
   - **Strict Layer-1 & Silent Consolidation Filter:** Nested child subagents (`IsNestedSubagentConversation`) and silent/routine `[Auto]` tasks (`sleep-consolidate`, quiet gatekeeper sweeps) **never** emit `task_done` channel notifications, preserving `AGENTS.md` §1–2 invariants.
3. **`SanitizeAntiCue`, URL Defanging, Privacy Modes & Rune Truncation:**
   - Every outbound `title`, `source_skill`, `scope`, and `body_markdown` passes through [`outbox.SanitizeAntiCue`](../../internal/outbox/dispatcher.go#L24).
   - When `tainted_origin == true`, `http://` and `https://` URLs inside `body_markdown` are defanged (`hxxp://`, `hxxps://`) so external chat apps never unfurl attacker beacons (`Finding R4`).
   - When `policy.privacy_mode == "zero_body"`, `title` and `body_markdown` are replaced with generic zero-PII text (`"agy-hq: 1 urgent item awaiting review in Front Desk"`) and `notification.actions` is omitted, unifying cleanly with [RFC-0020 §4.3](./0020-proactive-ambient-synthesis-and-mobile-push.md).
   - UTF-8 rune-safe truncation caps `body_markdown` at `min(spec.MaxBodyRunes, policy.MaxBodyRunes, 4000)` (default `1600` runes).
4. **SHA-256 Audit Log (`<vault>/memory/channels/channels.jsonl`, `0600`):**
   - Appended via `store.AppendLineNoFollow` (`<= 5000` hot lines, compacted on `hq daemon`'s `30m` heartbeat loop; both `<vault>/memory/channels/**` and `<vault>/private/{channels,approval_bridge_caps}/**` are blocked from direct LLM reads/writes/shell access in `PreToolUse`), storing **zero raw room IDs, zero callback tokens, and zero message bodies**:
     ```json
     {"ts":"2026-11-17T21:16:05Z","bridge_id":"acme-messaging/desk-bridge","op":"notify","kind":"interrupt","idempotency_key":"outbox:idea-1794904100-7f8e9d0c","desk_cid":"9a41bc10-7712-4c11-8a00-112233445566","dest_sha256":"e3b0c44298fc","external_thread_hash":"8f434346648f","payload_sha256":"6b86b273ff34fce19d6b804eff5a3f5747ada4eaa22f1d49c01e52ddb7875b4b","outcome":"delivered","attempt":1,"latency_ms":142}
     ```

---

## 6. Daemon Rotation Detection, Outbox Fan-Out, Inbound/Callback Gateways & CLI Surface

### 6.1 Atomic Front Desk Lease & `fastTicker` Rotation Hook

1. **Atomic `front_desk.json` Writes (`Finding R2`):** Update `saveFrontDeskLease` in [`internal/kernel/kernel.go`](../../internal/kernel/kernel.go#L1701-L1710) to use `store.WriteAtomic(frontDeskPath(vaultDir), data, 0o644)` so concurrent `15s` daemon reads never observe partial JSON.
2. **Daemon Loop Integration ([`cmd/hq/main.go`](../../cmd/hq/main.go#L1742-L1793)):**
   - Call `channels.SyncAndFlush(vaultDir, now, *dryRun)` at daemon startup (`L1743`), on `wakeCh` (`L1759`), and on every `15s` `fastTicker` tick (`L1771`).
   - **Zero-Overhead Steady State:** An in-process cached `(lastDeskCID, nextRetryAt, activeBridgesHash)` fast check skips disk locks and subprocess spawns in `<50µs` whenever `front_desk.json` is unchanged and `retry_queue` has no due items.
   - **Rotation State Machine:** Whenever `(fd.ConversationID, hasFD)` diverges from `state.LastDeskCID`, enqueue `PendingDeskBind` (`lease_state: "active"` when `hasFD`, `"vacated"` when `!hasFD` after `release-desk`/`adopt`) and execute `bind-desk` across all enabled trusted bridges with `capabilities.desk_binding == true`, passing up to `32` open `item_threads` for in-place rebinding.

### 6.2 Decoupled `DispatchOutboxToFrontDesk` & Task Digest Hooks

1. **`TierInterrupt` Outbox Fan-Out ([`internal/kernel/events_dispatch.go`](../../internal/kernel/events_dispatch.go#L481-L495)):**
   - **When `[HQ] Front Desk` is leased (`fd.ConversationID != ""`):** `outbox.DispatchPendingInterrupts` delivers to `fd.ConversationID` as today and returns `delivered []OutboxItem`; `DispatchOutboxToFrontDesk` passes `delivered` to `channels.EnqueueOutboxInterrupts(vaultDir, fd.ConversationID, delivered, now)` followed by `channels.FlushPending(vaultDir, now, dryRun)`. Transient bridge failures retry inside `channels`'s per-bridge queue **without** reverting `OutboxItem.Status` or double-waking `fd.ConversationID`.
   - **When `[HQ] Front Desk` is temporarily vacated (`fd.ConversationID == ""`):** `OutboxItem` remains `StatusPending` for the next Front Desk shift, while `channels.EnqueuePendingInterruptsWhenDeskVacant(vaultDir, now)` peeks eligible non-brief-leased `TierInterrupt` items and delivers them to enabled bridges under `idempotency_key = "outbox:" + it.ID`. When a new Front Desk later claims the lease and `DispatchPendingInterrupts` marks the item `StatusDelivered`, `recent_delivered_keys["outbox:"+it.ID]` prevents any duplicate send to the external channel!
   - **Deterministic `item_key` Selection:** `proposal:<propID>` when `len(it.ProposalIDs) > 0`, else `thread:<connector>:<threadKey>` when present, else `outbox:<it.ID>`.
2. **Opt-In Layer-1 Task `done` & Default `blocked` Digests:**
   - **`task_done` (`policy.deliver_task_done: false` default; opt-in `true`):** Hook explicit Layer-1 `kind: "task"` completions in [`kernel.CompleteTask`](../../internal/kernel/kernel.go#L857) under `max_non_urgent_per_hour` (`6/h`), enqueuing `idempotency_key = "task-done:" + slug + ":" + completedAt` (`item_key = "task:" + slug`). Inline subagents (whose takeaways `[HQ] Front Desk` already summarizes directly) and `[Auto]` background runs never emit `task_done`.
   - **`task_blocked` (`policy.deliver_task_blocked: true` default):** Hook `approvals.CreateWithOptions` (when `prop.ConversationID != "" && prop.ConversationID != fd.ConversationID`) to notify the user that a background task/worker staged `prop-...` and is awaiting human review (`idempotency_key = "task-blocked:" + prop.ID`, `item_key = "proposal:" + prop.ID`).

### 6.3 Inbound Owner Turn Gateway, `HandleStop` `desk_reply` Mirroring & Signed Button Callbacks

To make standalone bot extensions (e.g., Telegram, Slack, WhatsApp, Discord, Matrix) 100% bi-directional without scraping internal `transcript.jsonl` logs or opening `:4242` over a public network, `agy-hq` exposes three daemon-authenticated gateways ([`internal/webui/webui.go`](../../internal/webui/webui.go#L188-L195) + non-agent CLI equivalents):

1. **Inbound Owner Turn Gateway (`POST /api/channels/inbound` & `hq channel inbound`):**
   - **Strict Daemon-Token & Non-Agent Isolation:** HTTP requests require `Authorization: Bearer <daemon_token>` (`events.ValidateDaemonToken`; browser CSRF headers are rejected on `/api/channels/*`). CLI `hq channel inbound` reads JSON strictly from `stdin`, verifies `os.Getenv("ANTIGRAVITY_CONVERSATION_ID") == ""`, and is hard-blocked across all LLM sessions in `PreToolUse` (`\bhq\s+channel\s+(?:inbound|callback|apply-config)\b` and `/api/channels/(?:inbound|callback|flush)`). Both verify that `bridge_id` belongs to an active, trust-verified pack (`VerifyTrustedBinary`).
   - **Deduplication, Thread Routing & Vacant-Desk Auto-Bootstrap:** Deduplicates `(bridge_id, external_message_id)` against `recent_inbound_keys` (`256` entries, `72h` TTL). Resolves `external_thread_id` against open `item_threads` (routing task-thread replies directly to an active `[Task:<scope>]` session when matched), else routes to `front_desk.json` `ConversationID`—automatically bootstrapping a fresh `[HQ] Front Desk` conversation if `front_desk.json` is currently `vacated` rather than dropping the owner's turn or routing into a demoted task session.
   - **Correlation & Wakeup:** Records a `PendingReplyCorrelation` (`{correlation_id, bridge_id, target_conversation_id, external_thread_id, item_key, expires_at: now + 30m}`) under `RankChannels = 65`, enqueues `Source: "channel_inbound"` via `events.Enqueue`, wakes `hq daemon` immediately, and writes an `op: "inbound", kind: "owner_turn"` SHA-256 audit row to `channels.jsonl`.
2. **Zero-Token `HandleStop` `desk_reply` Mirroring ([`internal/hooks/hooks.go`](../../internal/hooks/hooks.go#L2000-L2015)):**
   - At turn completion in `HandleStop`, `channels.EnqueueTurnReplyIfCorrelated(vaultDir, payload.ConversationID, payload.LastResponse, turnStart, now)` checks for an unexpired `PendingReplyCorrelation` matching `payload.ConversationID` (consuming it one-shot so subsequent desktop-originated turns stay local unless `policy.mirror_all_desk_turns: true` is opted in).
   - Attaches any pending `Proposal` IDs staged by `payload.ConversationID` during that turn (so `hq propose` previews automatically arrive with inline `Approve`/`Reject` callback tokens in the same `desk_reply`), enqueues `op: "notify", kind: "desk_reply"` (`idempotency_key = "desk-reply:" + convID + ":" + strconv.Itoa(execNum)`), and sends a best-effort `<150ms` loopback `POST /api/channels/flush` nudge (`Bearer <daemon_token>`) for sub-second reply delivery (`15s` `fastTicker` fallback).
3. **One-Shot Signed Button Callback Gateway (`POST /api/channels/callback` & `hq channel callback`):**
   - **Zero Plaintext Token Storage on Disk (`<vault>/private/approval_bridge_caps/<token-sha256>.json`, `0600`):** When `internal/channels/` prepares a `notify` payload for a `capabilities.callback_buttons == true` bridge, it generates fresh `bca_<32 base64url>` (`approve`) and `bcr_<32 base64url>` (`reject`) tokens, writing **only** `SHA-256(token)` (`BridgeCapRecord{ProposalID, BridgeID, Decision, FullSHA256, TokenHash, PairHash, VaultCanon, ExpiresAt}`) to `<vault>/private/approval_bridge_caps/<token-sha256>.json` (`0600`, `min(Proposal.ExpiresAt, now + DefaultAskTTL)`).
   - **Atomic One-Shot Verification & 0-Token Execution:** When the companion sidecar verifies an owner button tap (`callback_query.from.id == pinned_owner_id`) and calls `/api/channels/callback` (`Bearer <daemon_token>`) or `hq channel callback` (`stdin` JSON) with `{bridge_id, callback_token, reason?, suppress_resume?}`:
     1. Validates `^bc[ar]_[A-Za-z0-9_-]{32}$` and active trust on `bridge_id`.
     2. Computes `tokHash = sha256Hex(callback_token)` and atomically claims `<vault>/private/approval_bridge_caps/<tokHash>.json` via `os.Rename` (`O(1)` lookup, zero directory scan, race-free single-use replay protection), also removing the sibling `PairHash` file so tapping `Approve` immediately invalidates `Reject` (and vice versa).
     3. Verifies `subtle.ConstantTimeCompare(rec.TokenHash, tokHash) == 1`, `rec.BridgeID == bridge_id`, `rec.VaultCanon == canonicalVaultPath(vaultDir)`, and `!now.After(rec.ExpiresAt)`.
     4. Executes [`approvals.ApproveFromTrustedDaemon`](../../internal/approvals/presence.go#L410) (`decision == "approve"`, which re-verifies `Proposal.SHA256 == rec.FullSHA256`, `RegisteredApplyPrecondition`, and atomic `pending/<propID>.json` claim) or [`approvals.RejectProposalWithOptions`](../../internal/approvals/approvals.go#L863) (`decision == "reject"`) in **`0` LLM tokens**, cleans up bridge cap artifacts in [`CleanupApprovalArtifacts`](../../internal/approvals/presence.go#L194), and records `op: "callback", kind: "callback_approve" | "callback_reject"` in `channels.jsonl`.

### 6.4 CLI, Loopback API & `hq doctor` Surface

Add top-level `hq channel` subcommand (and register `"channel"` in `validHQTopSubcmd` in [`internal/ci/harness.go`](../../internal/ci/harness.go#L291)):
- `hq channel list [--json]` — Lists discovered `channel_bridges` across companion extension packs with enabled, trust, and redacted health state.
- `hq channel status [<bridge-fqid>] [--json]` — Invokes `op: "status"`, checks `inbound_tunnel_bound_cid` alignment against `front_desk.json`, and triggers self-healing `bind-desk` on drift.
- `hq channel test <bridge-fqid>` — Sends a synthetic `kind: "test_ping"` notification to the pre-configured `<vault>/private/channels/<bridge-id>.json` destination (zero recipient flags).
- `hq channel pause <bridge-fqid> [--reason="..."]` / `hq channel resume <bridge-fqid>` — Toggles `ChannelBridgeOverrides` in `<vault>/extensions.json`.
- `hq channel configure <bridge-fqid>` — Reads strict `0600` private destination JSON from `stdin` (writing directly when invoked outside an agent session, or staging an approval-gated `/dev/fd/3` `hq channel apply-config` `rsi_apply` proposal inside an agent session).
- `hq channel audit [--limit=20]` — Displays recent SHA-256 audit rows from `channels.jsonl`.
- `hq channel inbound` / `hq channel callback` / `hq channel flush` — Daemon/sidecar-only `stdin` JSON gateways (`POST /api/channels/inbound`, `POST /api/channels/callback`, `POST /api/channels/flush` on loopback `:4242` with `Bearer <daemon_token>`, or CLI with `--daemon-token-file` / `HQ_DAEMON_TOKEN`); hard-blocked across all LLM sessions in `PreToolUse` and when `ANTIGRAVITY_CONVERSATION_ID != ""`.
- `hq doctor` — Validates `<vault>/private/channels/*.json` strict schemas (`0600` permissions, non-symlink), `private/trust.json` binary pins, and `health.status`.

---

## 7. Security Invariants & Step 2 Companion Pack Integration Boundary

### 7.1 Five Non-Negotiable Ring-0 Security Invariants

1. **Inbound Multi-Sender Quarantine Never Bypassed:** `hq-channel-bridge/v1` has no `poll` operation, and `hq watch precheck` never routes multi-sender connector items into `[HQ] Front Desk`. Inbound Front Desk tunneling (`/api/channels/inbound`) is restricted exclusively to trusted Step 2 companion sidecars enforcing cryptographic 1:1 owner-principal authentication (`verified_sender_id == pinned_owner_id` inside a dedicated owner-only DM/control channel).
2. **Cryptographic Human-Presence Approval Proof Preserved:** `VerifyAskAndMintCap` ([`presence.go`](../../internal/approvals/presence.go#L337-L341)) continues rejecting plain-text `USER_INPUT` transcript steps (so typing `"approve prop-..."` in chat never executes a proposal). `internal/channels/` never reads `private/approval_asks/` or transmits `NonceCode` / `rawCap` values. Instead, companion bridges use one of three verified approval paths: (a) host-brokered native `ask_question` prompt cards (`native_ask_cards: true`), (b) single-use `192`-bit `bca_`/`bcr_` button tokens (`callback_buttons: true`) stored as `SHA-256` hashes in LLM-invisible `private/approval_bridge_caps/` and passed only to the SHA-256-pinned bridge binary, or (c) `:4242` Web Console / Antigravity Remote Control fallback.
3. **Zero LLM Destination or Callback Token Exfiltration:** Because `<vault>/private/channels/**` and `<vault>/private/approval_bridge_caps/**` (`0600`) plus `hq channel inbound|callback|apply-config` are blocked across all sessions by `PreToolUse` + OS sandboxes—and `hq channel` accepts no destination flags—even a prompt-injected session cannot redirect outbound notifications or self-approve a staged proposal.
4. **Subprocess Privilege Isolation:** Bridge binaries must be SHA-256-pinned in `<vault>/private/trust.json` (`VerifyTrustedBinary`), execute from `<vault>/private/exec-cache/`, receive `SanitizedSubprocessEnv` with `ANTIGRAVITY_CSRF_TOKEN` and `ANTIGRAVITY_LS_ADDRESS` unconditionally stripped, and receive credentials only via `/dev/fd/3`.
5. **Zero-Internal-Leak Open-Source Purity:** All protocol identifiers (`hq-channel-bridge/v1`, `channel_bridges`), schemas, tests, and CLI surfaces remain 100% vendor-neutral and pure Go stdlib (`0` external dependencies).

### 7.2 How an External Companion Pack Implements Step 2 Cleanly

An external extension pack (living in its own private or open-source repository—whether a host-integrated enterprise sidecar or a standalone Telegram, Slack, WhatsApp, Discord, or Matrix bot) builds full bidirectional Front Desk chat tunneling in five clean steps with **zero patches to `agy-hq` core**:

1. **Plugin Packaging:** Keep `<pack>/plugin.json` standard (`agy plugin validate --strict` clean) and add `components.channel_bridges` in `<pack>/hq-extension.json` pointing to its compiled adapter binary (`protocol: "hq-channel-bridge/v1"`).
2. **`bind-desk` Handler:** Persist `{conversation_id, lease_state, desk_thread, item_threads}` in the companion sidecar's private local state (`0600`). On first bind, create or pin the external `desk_thread`; on `old_cid -> new_cid`, update routing in place (posting a concise shift divider unless `silent: true`); on `lease_state == "vacated"`, suspend `previous_conversation_id` forwarding immediately.
3. **`notify` & `status` Handlers:** Map `thread_hint` (`desk_thread` vs `item_thread`), `notification.kind` (including `desk_reply`), and `idempotency_key` to the target chat API, rendering `notification.actions` as native interactive `[Approve]` / `[Reject]` buttons when `callback_buttons: true`, returning `external_thread_id`, and reporting `inbound_tunnel_bound_cid` in `status`.
4. **Inbound Owner Tunnel & Interactive Approvals:** In the companion sidecar's inbound listener (e.g., Telegram long-polling, Slack Socket Mode, WhatsApp webhook, or host RPC stream), verify platform transport signatures + `sender_id == pinned_owner_id`:
   - On owner text messages, forward `{bridge_id, external_message_id, external_thread_id, text}` to `POST /api/channels/inbound` (`Bearer <daemon_token>`) or `hq channel inbound`; `HandleStop` automatically delivers the `desk_reply` back to `external_thread_id` without any `transcript.jsonl` parsing.
   - On interactive button taps (`callback_buttons: true`), forward `{bridge_id, callback_token}` to `POST /api/channels/callback` (`Bearer <daemon_token>`) or `hq channel callback` for 0-token execution (or broker native `ask_question` cards when `native_ask_cards: true`).
5. **Activation & Trust Ceremony:** Run `hq extension enable <pack>`, approve `hq extension trust <pack>` (`private/trust.json`), write `<vault>/private/channels/<pack>__<id>.json` (`0600`), and verify with `hq channel test <pack>/<id>`.

---

## 8. Phase 2 Implementation Plan, Verification Matrix & Resolved Decisions

### Phase 2 Implementation Breakdown

1. **`internal/store/store.go` & `internal/extension/{pack,state,trust}.go`:** Add `RankChannels = 65`; add `ChannelBridgeSpec` (including `CallbackButtons`), `LintPack` validation, `EnablePack` collision check, `ChannelBridgeOverrides` in `VaultState`, and `BuildTrustBundle` binary pinning.
2. **`internal/channels/` (`protocol.go`, `config.go`, `state.go`, `policy.go`, `dispatcher.go`, `inbound.go`, `callback.go`, `channels_test.go`):** Implement `hq-channel-bridge/v1` client (`connector.RunSubprocess` + `/dev/fd/3`), strict `0600` private config loader, 3-phase `RankChannels` state/retry/correlation manager, `bca_`/`bcr_` SHA-256 button capability minting & one-shot `os.Rename` verification (`private/approval_bridge_caps/`), `InQuietHours` + rate cap + `SanitizeAntiCue` + tainted URL defanging, and `channels.jsonl` SHA-256 audit logger.
3. **`internal/kernel/{kernel,events_dispatch}.go`, `internal/approvals/presence.go` & `internal/hooks/hooks.go`:** Switch `saveFrontDeskLease` to `store.WriteAtomic`; wire `SyncAndFlush` into `cmd/hq/main.go` (`fastTicker` & `wakeCh`); hook `DispatchOutboxToFrontDesk` (active + vacated desk fan-out), default `task_blocked`, opt-in Layer-1 `kind: "task"` `task_done`, and `HandleStop` `EnqueueTurnReplyIfCorrelated` (`desk_reply`); sweep bridge caps in `CleanupApprovalArtifacts`; add `"channels"` and `"approval_bridge_caps"` + `hq channel inbound|callback|apply-config` guards to `hooks.go`.
4. **`cmd/hq/main.go`, `internal/webui/webui.go`, `internal/doctor/doctor.go`, `internal/ci/harness.go` & `internal/e2e/cuj_e2e_test.go`:** Add `hq channel <list|status|test|pause|resume|audit|inbound|callback>`, loopback `POST /api/channels/{inbound,callback,flush}` (`Bearer <daemon_token>`), `hq doctor` checks, `validHQTopSubcmd["channel"]`, and `TestCUJ38_FrontDeskChannelBridgeAndRotationTunneling`.

### Verification Matrix (`Unit Tests` + `CUJ-38`)

| Scenario | Test / Gate | Key Assertions |
| :--- | :--- | :--- |
| **Manifest Lint & `private/trust.json` Pinning** | `extension_test.go` + `CUJ-38` | `LintPack` rejects shell-wrapper `command[0]` and blocked `ANTIGRAVITY_*` `env_allow`; `VerifyTrustedBinary` blocks tampered bridge binary and emits 1 `brief_rollup` notice. |
| **Rotation Rebinding (`claim-desk`, `release-desk`, `adopt`)** | `channels_test.go` + `CUJ-38` | `"" -> cid1 -> cid2` invokes `bind-desk` rebinding `desk_thread` and open `item_threads` in place; `hq adopt` (`cid2 -> ""`) sends `lease_state: "vacated"` within 1 tick; `status` drift self-heals. |
| **Bidirectional `inbound` -> `HandleStop` `desk_reply` & Button Callbacks** | `channels_test.go` + `hooks_test.go` + `CUJ-38` | Verified `inbound` turn routes to active/auto-bootstrapped Front Desk and triggers correlated `HandleStop` `desk_reply` with 36-byte `bca_`/`bcr_` tokens; `callback` executes 0-token `ApproveFromTrustedDaemon` once, invalidates sibling `PairHash` + replays, while plain-text `"approve"` via `inbound` never mints a capability. |
| **Decoupled Outbox Retry & Vacated-Desk Delivery** | `channels_test.go` + `CUJ-38` | Simulated bridge `transient` / `rate_limited` errors back off and succeed on retry with **zero** duplicate `[HQ] Front Desk` deliveries; `permanent` errors drop immediately without retry storms; vacated-desk interrupts deliver once across subsequent desk claim. |
| **Credential Guard, Anti-Cue Scrubbing & Audit Hash** | `hooks_test.go` + `channels_test.go` + `CUJ-38` | `PreToolUse` blocks all LLM access to `private/{channels,approval_bridge_caps}/**`, `hq channel inbound|callback|apply-config`, `/api/channels/*`, and writes on `memory/channels/**`; tainted summaries scrub command cues and defang URLs; `channels.jsonl` contains SHA-256 hashes and zero raw room IDs, tokens, or bodies. |
| **Full Pre-Push CI Gate** | `./bin/hq ci pre-push` | 100% pass across `P1`–`P6` PII checks, `H1`–`H4B` harness/markdown contracts, `agy plugin validate --strict`, `go vet ./...`, and full unit + E2E test suites. |

### Resolved Design Decisions

1. **Action-Required Default (`deliver_task_done: false`):** Outbound bridge policy defaults to `deliver_interrupts: true`, `deliver_task_blocked: true`, and **`deliver_task_done: false`** so chat notifications remain strictly high-signal and action-required out of the box. When opted in (`"deliver_task_done": true`), `task_done` scopes strictly to explicit Layer-1 `kind: "task"` completions (`hq complete`) under `max_non_urgent_per_hour: 6`, excluding inline subagents and `[Auto]` runs.
2. **Companion-Pack-Only Discovery (`hq-extension.json`, `YAGNI` on `<vault>/bridges/`):** Phase 2 scopes `channel_bridges` discovery strictly to enabled, trust-verified companion extension packs (`<pack>/hq-extension.json` -> `<pack>/<id>`), avoiding a second `vault/<id>` discovery namespace while stateful bidirectional bridges ship alongside companion sidecars.
3. **First-Class Standalone Bot Bidirectional & Button Callback Gateways (`inbound` + `desk_reply` + `callback_buttons`):** To support standalone mobile/team messaging bots (e.g., Telegram, Slack, WhatsApp, Discord, Matrix) cleanly without `transcript.jsonl` scraping or `:4242` port exposure, core includes daemon-authenticated `POST /api/channels/inbound` (`hq channel inbound`) paired with zero-token `HandleStop` `desk_reply` correlation, plus optional compact `36`-byte `bca_`/`bcr_` one-shot button tokens (`capabilities.callback_buttons: true` -> `POST /api/channels/callback` / `hq channel callback`) stored solely as `SHA-256` hashes in LLM-invisible `<vault>/private/approval_bridge_caps/`. Plain-text chat messages continue to be rejected for approval minting (`VerifyAskAndMintCap`).
