# 23 — External Cloud Agent & `agy-hq` Interoperability Audit

- **Date:** 2026-11-22
- **Companion RFC:** [RFC-0028: External Cloud Agent Interoperability — Scoped Handoff, Structured Status & Approval Isolation](../design/0028-external-agent-interoperability.md)
- **Scope:** Empirical codebase audit of `agy-hq` ingress/egress seams, 4-option architectural comparison for interoperating with an external cloud assistant (e.g., Cursor Grok Bot or any cloud Linux agent sandbox with Shell, browser, MCP connectors, cron, and webhooks), human-approval preservation, and threat modeling.

---

## 1. Executive Summary & Problem Statement

Many users run a **split personal-agent topology**:

1. **An external cloud assistant** (such as a Cursor Grok Bot instance on a hosted Linux VM) with 24/7 cloud uptime, Shell/browser tools, MCP connectors (GitHub, Gmail, Google Calendar), scheduled routines, and outbound webhook/email triggers.
2. **`agy-hq` on the user's local workstation**, providing local repo access, isolated Git worktrees, the 4-layer private Vault, Antigravity IDE integration, the `:4242` local Console, and `agy-hq`'s Ring-0 taint lattice + 1-tap human approval gate (`hq propose` / `hq approve`).

Today, bridging the two by having the cloud assistant's headless browser log into `antigravity.google.com/remote` and type prompts into the web UI fails on both **reliability** and **security**:

- **Operational brittleness:** Fragile DOM selectors, SSO/2FA session expirations, CAPTCHA/anti-bot challenges, Front Desk shift rotations (`front_desk.json` `ConversationID` changes), and zero machine-readable correlation (no `request_id`, no `task_slug`, no state transitions, no structured PR/artifact links).
- **Security boundary collapse (`NI-3` violation):** Typing into the owner's Remote Control UI injects cloud-generated text directly into **`[HQ] Front Desk` as `IngressOwnerInteractive (⊤)`**. Because the cloud assistant routinely browses the web, reads emails, and processes external calendar invites, a single indirect prompt injection on the cloud box becomes a **Confused Deputy** with owner-level authority inside `agy-hq`—bypassing taint quarantine and risking unauthorized `ModeAutoOnly` actions or forged approval clicks.

**Core Finding:** `agy-hq` **already implements ~80% of the required interop kernel** via [RFC-0026 (`internal/authz` + `internal/delegates`)](../design/0026-declarative-scoped-delegates.md), [RFC-0027 (`DecisionOption` 1-tap buttons & timeout recovery)](../design/0027-muse-proactive-actions.md), [RFC-0018 (Event Inbox & deferred approval resumption)](../design/0018-always-on-agent-runtime-hardening.md), and [RFC-0023 (owner-only mobile approval mirrors)](../design/0023-front-desk-channel-bridge.md). Bridging an external cloud assistant cleanly does **not** require exposing `:4242`, sharing `daemon_token`, or building a parallel webhook daemon. It requires closing **three targeted gaps (`G1`–`G3`)** on top of `hq-delegate-transport/v1`, paired with a **pull-only GitHub Issues/Labels dispatch transport** as minimal v1.

---

## 2. Live Codebase Audit: Existing Seams vs. Real Gaps

| Existing `agy-hq` Subsystem | Code Reference | What It Already Provides | Why It Fits / Does Not Fit Cloud-Agent Interop |
| :--- | :--- | :--- | :--- |
| **Session Authorization Lattice** (`RFC-0026`) | [`internal/authz/authz.go`](../../internal/authz/authz.go) | 8-profile capability lattice (`fail_closed`, `classifier`, `quarantined_reader`, `scoped_delegate`, `subagent`, `unattended_auto`, `front_desk`, `interactive_task`) + 4 mechanical Non-Interference invariants (`NI-1`–`NI-4`). | **Direct fit.** Establishes `IngressUntrustedPeer` strictly below `owner_interactive (⊤)` and forbids `hq approve` / `ModeAutoOnly` bypasses below `⊤`. |
| **Declarative Scoped Delegates** (`RFC-0026`) | [`internal/delegates/protocol.go`](../../internal/delegates/protocol.go), [`config.go`](../../internal/delegates/config.go), [`nudge.go`](../../internal/delegates/nudge.go), [`egress.go`](../../internal/delegates/egress.go) | SHA-256-pinned `hq-delegate-transport/v1` (`poll`, `reply`, `publish_snapshot`, `status`), JSONC policy (`private/delegates/policy.json`), caller allowlists, pre-preflight rate limits & cooldowns, `CuratedTaskView`, `queue_only` & `spawn_sandboxed_worker`, and Ring-0 `internal/redact` egress gateway. | **Best foundation (80% complete)**, with 3 specific gaps (`G1`–`G3` below) for long-running multi-step cloud-assistant handoffs. |
| **Loopback WebUI & API** (`:4242`) | [`internal/webui/webui.go`](../../internal/webui/webui.go) | Loopback guard (`IsLoopbackAddr`), CSRF, and `Bearer <daemon_token>` guarding `/api/events`, `/api/channels/inbound`, `/api/channels/callback`, `/api/channels/flush`. | **Must NEVER be exposed externally.** `daemon_token` is a root local credential that can enqueue untainted prompts (`/api/events`), speak as the owner (`/api/channels/inbound`), and execute `ApproveFromTrustedDaemon` (`/api/channels/callback`). |
| **Front Desk Channel Bridge** (`RFC-0023`) | [`internal/channels/inbound.go`](../../internal/channels/inbound.go), [`callback.go`](../../internal/channels/callback.go), [`protocol.go`](../../internal/channels/protocol.go) | Verified 1:1 human-owner mirror to `[HQ] Front Desk`, shift-rotation `bind-desk`, `HandleStop` `desk_reply`, `KindTaskDone` / `KindTaskBlocked`, and one-shot `bca_`/`bcr_` approval buttons. | **Wrong for bot intake, right for owner approvals.** `channel_bridges` assumes the remote principal is the human owner (`owner_interactive`). Connecting a cloud AI agent to `inbound` collapses `NI-3`. It remains the ideal surface for *the owner* to tap Approve on mobile when a cloud-initiated task stages an `hq propose`. |
| **Connector & Watcher Engine** (`RFC-0017a/b`) | [`internal/connector/manifest.go`](../../internal/connector/manifest.go), [`builtin_drivers.go`](../../internal/connector/builtin_drivers.go), [`internal/watcher/watcher.go`](../../internal/watcher/watcher.go) | 0-token background polling (`gmail`, `gchat`, `gcalendar`, `jsonmap`), `DKIM/SPF` checks (`IsDKIMOrSPFAligned`), `TaintOnSpawn: true` (`quarantined_reader`), `hq offer stage`. | Built for multi-party untrusted inbox triage (`quarantined_reader`), which intentionally has **zero** auto-reply and **zero** task/worktree dispatch (`NI-1`). |
| **Event Inbox & Approvals** (`RFC-0018`, `RFC-0021`, `RFC-0027`) | [`internal/events/events.go`](../../internal/events/events.go), [`internal/approvals/presence.go`](../../internal/approvals/presence.go), [`internal/outbox/decisions.go`](../../internal/outbox/decisions.go) | Durable `pending -> leased -> done/dropped` queue with `DedupKey`/`ScopeKey`, `VerifyAskAndMintCap`, `hq approvals restage`, and 1-tap `DecisionOption` buttons (`hq outbox act`). | **Direct fit** for Gate 1 intake approval, Gate 2 outbound approval, timeout alerts, and deferred worker resumption. |

### The Three Exact Gaps (`G1`–`G3`) in Current `agy-hq`

1. **`G1` — Free-Form Task Briefs vs. `NudgeSpec` 64-Rune Parameter Cap:**
   - In [`internal/delegates/config.go`](../../internal/delegates/config.go#L38), `MaxRegexArgRunes = 64` restricts `NudgeSpec` to short typed parameters (ideal for *"review CL 12345"*). Furthermore, [`internal/delegates/nudge.go`](../../internal/delegates/nudge.go#L314) spawns `spawn_sandboxed_worker` with `TaintOnSpawn: false` **only** because `RenderSafeTemplate` embeds zero raw peer prose.
   - A cloud assistant handing off a research or engineering task often needs to pass a multi-paragraph brief (goals, constraints, target repo). Injecting raw untrusted prose into an untainted `unattended_auto` worker (which has `ContextLeanVault` + `HostFS`) would violate `NI-1`/`NI-2`.
   - **Resolution:** Split intake into two safe tiers:
     - **Tier 1 (Typed Intents — 0 intake popups):** Pre-declared intents (`status_check`, `ci_check`, `run_skill`) with strictly typed short params reuse `RFC-0026` as-is.
     - **Tier 2 (Free-Form Briefs — Gate 1 or Vault-Blind Isolation):** Store the brief opaquely in `<vault>/private/delegates/requests/<reqID>.json` and either (a) surface a **1-tap `RFC-0027` `DecisionOption` launch card** to the owner (v1 default), or (b) opt-in spawn a **Vault-blind `external_task_worker`** (`ContextProjection: none`, single-repo worktree jail, `Egress: auto_propose_only` + Ring-0 redacted status updates only).

2. **`G2` — Multi-Milestone Status Reporting vs. Single-Shot `CompletedRequests`:**
   - Today, [`internal/delegates/nudge.go`](../../internal/delegates/nudge.go#L203) and [`internal/delegates/egress.go`](../../internal/delegates/egress.go) finalize a `request_id` after **one** reply (`st.CompletedRequests[stored.RequestID]`).
   - Delegated tasks take minutes to hours and pass through multiple lifecycle states (`accepted` → `running` → `awaiting_approval` → `completed` | `blocked` | `rejected` | `approval_expired`).
   - **Resolution:** Add an optional idempotent `op: "update_status"` verb to `hq-delegate-transport/v1` (updating one pinned status comment + syncing state labels in place) driven automatically at **0 LLM tokens** by `agy-hq` task, event, and proposal lifecycle hooks. Single-reply `RFC-0026` Q&A policies remain 100% unchanged.

3. **`G3` — No Reference GitHub Dispatch Transport Pack:**
   - `templates/connectors/` currently ships `gmail`, `gchat`, and `gcalendar` only; there is no `github` transport pack in `agy-hq`.
   - **Resolution:** Ship a small, pure-stdlib or `gh`-backed companion pack (`hq-extension.json` `components.delegate_transports`, SHA-256 pinned in `private/trust.json`) keeping `go.mod` at `0` external dependencies (`ADR-0002` / `Gate H4-A`).

---

## 3. Architectural Options Evaluated (4 Options)

### Option A — GitHub Issues & Labels via `hq-delegate-transport/v1` *(Recommended Minimal v1)*

- **Transport:** Pull-only HTTPS from local `hq daemon` (`gh api` / GitHub REST API with `If-None-Match` `ETag` conditional requests so idle polls consume **0 GitHub rate-limit points** and **0 LLM tokens**). Cloud assistant writes issues and reads comments using its existing GitHub MCP connector or `gh` CLI.
- **Public Repo vs. Private Dispatch Repo (Critical Hygiene Distinction):**
  - **Personal / cross-repo handoffs MUST use a dedicated private dispatch repository** (e.g., `<owner>/hq-dispatch`, `private: true`, 0 external collaborators). Posting personal assistant tasks, work context, or cross-repo summaries onto a public open-source repo (such as the public `agy-hq` repo) would leak private metadata and expose an open spam/prompt-injection intake surface.
  - **OSS repo-scoped engineering tasks** (e.g., *"investigate public issue #34"* or *"draft RFC for feature X"*) may optionally use an allowlisted public repo **strictly filtered** by immutable numeric GitHub `user.id`, explicit opt-in label (`hq:inbox`), and optional cryptographic envelope signature (`HMAC-SHA256`).
  - The exact same `github-dispatch-transport` binary supports both via `<vault>/private/delegates/transports/<id>.json` (`0600`, passed on `/dev/fd/3`).
- **Status Reporting:** `agy-hq` updates state labels (`hq:state/queued`, `hq:state/running`, `hq:state/awaiting-approval`, `hq:state/done`, `hq:state/blocked`) and upserts a single structured status comment (`agy-hq-status/v1`).
- **Near-Real-Time Push Back (v1.5, zero local ports):** An optional 15-line GitHub Actions workflow on the private dispatch repo triggers on `issue_comment` and posts an HMAC-signed webhook to the cloud assistant's inbound URL—using GitHub as a durable store-and-forward broker so the local host still exposes **zero inbound ports**.

### Option B — Authenticated HTTPS Webhook & Edge Queue Gateway

- **Transport:** Cloud assistant sends `HMAC-SHA256`-signed `POST /v1/handoff` requests to a cloud edge endpoint (e.g., Cloudflare Worker + Cloudflare Queues, or Tailscale node) that buffers requests when the local workstation sleeps. A local pull worker (or Tailscale-only local adapter) verifies signatures + nonces and feeds `hq-delegate-transport/v1` (never exposing `:4242` or `daemon_token`). Status updates push back via signed outbound HTTPS webhooks to the cloud assistant.
- **Why NOT direct `:4242` / Tailscale-to-`:4242`:** `internal/webui/webui.go` enforces `IsLoopbackAddr` (`127.0.0.1`/`::1`) and guards all privileged APIs with a single `daemon_token`. Tunneling `:4242` to a cloud box violates `NI-3` and `NI-4` by granting the cloud VM access to `/api/channels/inbound`, `/api/channels/callback`, and arbitrary untainted `/api/events`. Any HTTPS ingress MUST terminate at an isolated, unprivileged verifier that can only enqueue `IngressUntrustedPeer` items.
- **Tradeoff:** Best latency (`< 2s` round-trip) and cleanest machine-to-machine JSON transport, **but** requires provisioning and maintaining external relay infrastructure (edge worker, queue, TLS/HMAC key rotation) that is unnecessary for asynchronous 5–30 minute engineering/research handoffs in v1.

### Option C — Existing Email (`gmail` Connector) or Channel Bridge (`RFC-0023`)

- **C1 — Structured Email via `gmail` Connector (`RFC-0017a/b`):**
  - Cloud assistant sends labeled emails (e.g., `owner+hq-handoff@example.com` with JSON fenced payload); `hq daemon`'s `gmail-inbox-watch` polls every 15m and checks `DKIM/SPF` alignment (`IsDKIMOrSPFAligned`).
  - **Tradeoffs:** Zero new accounts, **but** coarse polling latency (15m default), MIME/quoting fragility, inbox clutter, and an **identity-blurring hazard**: when the cloud assistant uses the owner's own Gmail MCP connector, `From == owner`, making it impossible to distinguish human-sent emails from cloud-bot-sent emails without adding a separate cryptographic signature layer anyway.
- **C2 — `RFC-0023` Front Desk Channel Bridge (`channel_bridges`):**
  - **Explicitly Rejected for autonomous bot-to-bot intake.** `RFC-0023` is designed for **human-owner 1:1 messaging** directly into `[HQ] Front Desk` (`IngressOwnerInteractive`) and 1-tap `bca_`/`bcr_` approval callbacks (`ApproveFromTrustedDaemon`). Routing an autonomous cloud agent through `RFC-0023` breaks `NI-3` and turns any indirect prompt injection on the cloud box into an owner-privileged Front Desk turn.

### Option D — Private Git State / Mailbox Repository

- **Transport:** Cloud assistant pushes signed commits containing `inbox/<req-id>.json` to a private Git repo (`inbox` branch); local `hq daemon` runs `git fetch` and pushes `status/<req-id>.json` to an `outbox` branch.
- **Tradeoffs:** Strong cryptographic authenticity via SSH/GPG commit signatures, offline-tolerant, zero API rate limits, **but** poor ergonomics for cloud MCP connectors (most cloud assistants have first-class GitHub Issues MCP tools but awkward multi-branch Git push conflict handling), and no native mobile UI / comment notifications like GitHub Issues.

### Comparative Decision Matrix

| Criterion | Option A: GitHub Issues/Labels *(v1 Pick)* | Option B: Signed Webhook + Edge Queue | Option C1: Gmail Connector | Option C2: `RFC-0023` Channel Bridge | Option D: Private Git Mailbox |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **Inbound Exposure on Local Host** | **Zero** (pull-only HTTPS) | Low–Medium (tunnel/edge relay + local adapter) | **Zero** (pull-only OAuth/CLI) | N/A (local sidecar only) | **Zero** (pull-only Git) |
| **`:4242` & `daemon_token` Isolation** | **100% isolated** | Isolated only with separate verifier | **100% isolated** | **Violated** if exposed to cloud bot | **100% isolated** |
| **Taint & Approval Isolation (`NI-1`–`NI-3`)** | **Native** (`RFC-0026` `untrusted_peer`) | Native if routed into `delegates` | Partial (identity blur if same Gmail) | **Broken** (elevates bot to `⊤`) | Native (`untrusted_peer`) |
| **Offline Laptop Tolerance** | **Native** (GitHub stores queue) | Requires cloud queue (CF Queues/SQS) | **Native** (Gmail stores queue) | Poor | **Native** (Git remote stores queue) |
| **Cloud Assistant Tool Compatibility** | **Native** (GitHub MCP + `gh` CLI) | Requires custom webhook client + secrets | Native (Gmail MCP), messy MIME | Poor (needs local daemon token) | Moderate (branch conflict handling) |
| **Intake / Status Latency** | `15–30s` intake (`ETag` 0-cost); `~5s` push back via GH Actions | `< 2s` both ways | `~15m` default | `< 2s` | `30–60s` |
| **Human Visibility & Audit Trail** | **Excellent** (GitHub web/mobile UI + labels) | Poor (needs custom dashboard) | Moderate (clutters personal email) | Good (chat thread) | Low (raw JSON commits) |
| **v1 Build & Ops Effort** | **Lowest (~3–5 eng days, 0 infra)** | High (~6–10 days + cloud ops) | Medium (~3–4 days, high fragility) | Unsafe (rejected) | Medium (~4–5 days) |

---

## 4. Wire Contracts & Protocol Extensions

### 4.1 Inbound Handoff Envelope (`agy-hq-handoff/v1`)

Embedded as a fenced `json` block (or YAML frontmatter) in a GitHub Issue labeled `hq:inbox`:

```json
{
  "schema": "agy-hq-handoff/v1",
  "external_ref": "cloud-job-20261122-01",
  "intent": "research_rfc",
  "target_repo": "owner/project-repo",
  "base_branch": "main",
  "deliverable": "draft_pr",
  "priority": "normal",
  "signature": "hmac-sha256:optional-hex-digest"
}
```

- **Strict validation in Go (`internal/delegates`):** Unknown top-level keys rejected (`DisallowUnknownFields`); `external_ref` validated against `^[A-Za-z0-9._:-]{4,96}$` and used as `events.Event.DedupKey`; `intent` must match an allowlisted policy ID in `<vault>/private/delegates/policy.json`; `target_repo` must match an allowlisted `owner/repo` regex; raw issue body capped at `8 KiB` and stored opaquely under `<vault>/private/delegates/requests/<reqID>.json` (`0600`) with a random per-read nonce fence.

### 4.2 Outbound Status Envelope (`agy-hq-status/v1`)

Posted as a clean human-readable summary plus a machine-parseable HTML comment block (and mirrored to mutually exclusive GitHub labels `hq:state/<state>`):

```markdown
### `agy-hq` Status: `awaiting_approval`
- **Request:** `req-9f3a1c` (ref: `cloud-job-20261122-01`)
- **Task:** `project-repo-feature-rfc` (`ev-20261122-0042`)
- **Summary:** Draft RFC prepared on `hq/feature-rfc`; waiting for owner approval on 1 staged action.
- **Artifacts:** branch `hq/feature-rfc` · commit `abc1234`

<!-- agy-hq-status:v1
{"schema":"agy-hq-status/v1","request_id":"req-9f3a1c","external_ref":"cloud-job-20261122-01","task_slug":"project-repo-feature-rfc","event_id":"ev-20261122-0042","state":"awaiting_approval","summary":"Draft RFC prepared on hq/feature-rfc; waiting for owner approval on 1 staged action.","artifacts":{"branch":"hq/feature-rfc","commit_sha":"abc1234","pr_url":""},"approvals":{"pending_count":1,"categories":["messaging"]},"updated_at":"2026-11-22T18:10:00Z"}
-->
```

**Mandatory Egress Redaction Rules (`internal/redact`):**
- **Allowed fields ONLY:** `request_id`, `external_ref`, `task_slug`, `event_id`, fixed-enum `state` (`queued`, `awaiting_intake_approval`, `running`, `awaiting_approval`, `completed`, `blocked`, `rejected`, `approval_expired`, `failed`), curated `summary` (`<= 600` runes), safe `artifacts` (`branch`, `commit_sha`, `pr_url` matching `https://github.com/...`), and aggregate `approvals` (`pending_count`, `categories`).
- **Strictly Forbidden (fail-closed if detected):** local paths (`/home/`, `/Users/`, `vaultDir`), `prop-...` proposal IDs, `bca_`/`bcr_` callback tokens, `daemon_token`, private Vault facts/entities, raw `ActiveTask.LastSummary`, email addresses, or `sensitive_terms` from `policy.json`.

### 4.3 Backward-Compatible `hq-delegate-transport/v1` Extension

Add one optional verb to [`internal/delegates/protocol.go`](../../internal/delegates/protocol.go):

- `op: "update_status"` with fields: `request_id`, `origin_thread` (e.g., `owner/repo#42`), `status_state`, `labels_add`, `labels_remove`, `body`, `upsert_marker` (`"agy-hq-status:v1"`), and `idempotency_key`.
- Transport returns `{ "protocol": "hq-delegate-transport/v1", "ok": true, "external_comment_id": "..." }`.
- Existing transports implementing only `poll`, `reply`, `publish_snapshot`, `status` continue working with zero breaking changes.

---

## 5. Human Approval Flow & Taint Isolation Proof

### Two-Gate Approval Model

1. **Gate 1 — Intake Promotion (Prevents Unauthorized Free-Form Execution):**
   - **Typed narrow intents** (e.g., `status_check`, `ci_check`): run immediately under `RFC-0026` `scoped_delegate` or static-template `spawn_sandboxed_worker` (`0` popups).
   - **Free-form task briefs (v1 default):** `hq daemon` creates a `queued` / `awaiting_intake_approval` handoff record, updates the GitHub issue status, and emits a **1-tap `RFC-0027` `DecisionOption` card** to `[HQ] Front Desk`, `:4242` Console, and any configured owner `RFC-0023` mobile bridge:
     - `[Approve & Spawn Worktree]` · `[Run Vault-Blind Only]` · `[Reject Handoff]`
   - Only an owner-present tap (`hq outbox act`) promotes the brief into a live worktree session.
   - **Opt-in Phase 2B fast lane (`external_task_worker`):** For explicitly allowlisted low-risk tasks (e.g., read-only research or draft PR on an owned OSS repo), `agy-hq` may spawn a **Vault-blind sandboxed worker** (`ContextProjection: none`, no `USER.md`/`MEMORY.md`/`private/**`, no inbox reads, repo worktree jail, `Egress: auto_propose_only`). Because `private_read` is empty, `NI-1` holds even with autonomous redacted status updates.

2. **Gate 2 — Outbound Mutation Gate (`hq propose` + `NI-3`):**
   - Any worker created from an external agent handoff **always** evaluates approval rules below `⊤` (`auto_propose_only` — never receives `approvals.interactive_preset: low_friction` or `ModeAutoOnly` bypasses).
   - Staging an external send, release, or third-party PR merge calls `hq propose`, transitions the handoff status to `awaiting_approval` (with aggregate `pending_count` only), and sends the full payload blockquote + 1-tap `Approve` / `Reject` prompt **strictly to the human owner** (Antigravity `ask_question`, `:4242` CSRF, or owner-verified `RFC-0023` `bca_` button).
   - Upon owner approval/rejection, `agy-hq` executes the deterministic command in `0` LLM tokens, resumes the worker via `RFC-0018`, and emits the updated status back to GitHub.
   - If the `24h` approval TTL expires, `RFC-0027` alerts the owner with `[Re-open Timed-Out Card]` (`hq approvals restage`) and emits `state: "approval_expired"` so the cloud assistant never hangs indefinitely.

---

## 6. Security & Threat Model (`T1`–`T6`)

| Threat | Attack Vector | Concrete Mitigation in Proposed Design |
| :--- | :--- | :--- |
| **`T1` — Cloud Prompt Injection (Confused Deputy)** | Cloud assistant reads a malicious email/webpage that instructs it to file an `agy-hq` handoff exfiltrating Vault secrets or sending messages. | Transport ceiling clamps all intake to `IngressUntrustedPeer` (never `⊤`). Free-form briefs require Gate 1 owner tap (or Phase 2B Vault-blind `ContextProjection: none`). All external mutations require Gate 2 `hq propose` + human-presence `hq approve` (`NI-3`). Egress scrubbed by Ring-0 `internal/redact`. |
| **`T2` — Public Repo PII Leak / Drive-By Spam** | Personal tasks filed on a public repo leak life/work details; strangers open issues on a public repo to trigger `agy-hq`. | Default to a **private dispatch repo** (`0` collaborators) for personal/cross-repo work. Enforce immutable numeric GitHub `user.id` allowlist + explicit `hq:inbox` label + optional `HMAC-SHA256` signature check. Public repos limited strictly to OSS engineering intents with Ring-0 egress redaction. |
| **`T3` — Loopback `:4242` / `daemon_token` Compromise** | Exposing `:4242` or sharing `daemon_token` lets a compromised cloud VM call `/api/channels/callback` or `/api/events`. | **Zero network exposure:** `IsLoopbackAddr` remains strictly `127.0.0.1`/`::1`; `daemon_token` never leaves the local workstation. Pull-only outbound HTTPS to `api.github.com` only. |
| **`T4` — Self-Approval / Approval Spoofing** | Cloud assistant comments `"approve"` on an issue or tries to guess a proposal ID / `bca_` token. | Plain-text comments never mint approval capabilities (`VerifyAskAndMintCap` rejects text). Outbound status strips `prop-` IDs and never emits `bca_`/`bcr_` tokens. `NI-3` enforces human-presence proof exclusively on owner channels. |
| **`T5` — Token Blast Radius (Stolen GitHub PAT)** | Attacker steals the transport PAT from the cloud box or local transport config. | Fine-grained GitHub PAT scoped **solely** to `Issues: Read & Write` on the single dispatch repo (zero `Contents`, `Pull Requests`, `Actions`, or `Admin` permissions). Stored in `private/delegates/transports/` (`0600`, `/dev/fd/3`). Optional `HMAC-SHA256` signing key isolates issue-write access from handoff validity. |
| **`T6` — Runaway Cron / Quota DoS** | Faulty cloud routine files 50 duplicate issues per minute, burning Antigravity spawn quota. | `DedupKey` on `external_ref` + GitHub `issue_id`; pre-preflight per-policy hourly caps (`MaxNudgesPerHour`) & cooldowns under `RankDelegates = 66`; `hq daemon` daily spawn circuit breaker (`RankQuota = 85`); `PoisonedItems` dead-lettering after max retries. |

---

## 7. Phased Delivery & Rough Effort Estimate

| Phase | Deliverables | Rough Effort | Key Dependencies |
| :--- | :--- | :--- | :--- |
| **Phase 1 *(This PR)*** | Research audit (`docs/research/23_...`) + Design RFC (`docs/design/0028-...`) + `docs/README.md` index. | Complete | User review & approval |
| **v1 / Phase 2A (`P0`)** | 1. `hq-delegate-transport/v1` optional `update_status` verb + multi-milestone state tracking in `internal/delegates` (`RankDelegates = 66`).<br>2. Gate 1 1-tap `DecisionOption` intake card (`RFC-0027`) + 0-token lifecycle status callbacks (`accepted` → `running` → `awaiting_approval` → `completed` / `blocked` / `rejected` / `approval_expired`) scrubbed by `internal/redact`.<br>3. Reference SHA-256-pinned `github-dispatch-transport` companion pack (`gh api` + `ETag` 0-cost polling + numeric `user.id` + optional `HMAC-SHA256`).<br>4. Black-box E2E test **`CUJ-48`**. | **~3–5 eng days** (~600–900 LOC + tests) | Pure Go stdlib (`Gate H4-A`), `Gate H5` LOC ceilings |
| **Phase 2B (`P1`)** | 1. Reference opt-in GitHub Actions relay workflow (`notify-cloud-agent.yml`) pushing signed status comments to the cloud assistant's webhook in `~5s` (still 0 local inbound ports).<br>2. Optional Vault-blind `external_task_worker` lattice profile (`ContextProjection: none`, single-repo worktree jail) for zero-click pre-approved research/draft-PR tasks. | **~2–3 eng days** | Phase 2A |
| **Phase 2C (`P2`, Optional)** | Edge-queued HTTPS webhook transport (e.g., Cloudflare Worker + Queue → local pull adapter speaking the **exact same** `hq-delegate-transport/v1` contract) if sub-second intake latency is ever needed. | **~4–6 eng days** | Phase 2A (zero core kernel rewrite) |
