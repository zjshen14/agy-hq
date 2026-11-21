# RFC-0017c: Non-Destructive Lease-Based Morning Brief Collector & Structured Extensions

- **Status:** Implemented
- **Date:** 2026-11-14
- **Suite:** Part 3 of 4 — see also [RFC-0017a (Connector Protocol & Watcher Engine)](0017a-connector-protocol-and-watcher-engine.md), [RFC-0017b (Provenance Taint & Offer Gateway)](0017b-provenance-taint-and-offer-gateway.md), and [RFC-0017d (Companion Extension Packs, Unified Trust & Host Portability)](0017d-enterprise-extensibility-and-portability.md)
- **Depends On:** [ADR-0003 (Two-Layer Vault)](../decisions/0003-two-layer-oss-and-private-vault.md), [RFC-0014](0014-proactive-engine-and-living-context.md), [RFC-0017a](0017a-connector-protocol-and-watcher-engine.md), [RFC-0017b](0017b-provenance-taint-and-offer-gateway.md), [RFC-0017d](0017d-enterprise-extensibility-and-portability.md), [RFC-0018](0018-always-on-agent-runtime-hardening.md)
- **Companion Research:** [13 — General Watcher Mechanism, Quarantine Provenance & Pluggable Enterprise Connector Audit](../research/13_proactive_channel_watcher_audit.md)

---

## 1. Motivation & Threat Model

`/morning-brief` (`skills/morning-brief/SKILL.md`) currently couples the daily digest directly to consumer `gcalendar`, `hq gmail search`, `hq gchat search`, and `gh pr status`, forcing enterprise workstations (Slack, Jira / `twg`, Gerrit, corporate mail/calendar) to run dead consumer steps and tempting users to place personal or regional checks into tracked OSS skill files.

Decoupling `/morning-brief` must simultaneously close **two critical reliability and security hazards** ([Research 13 §4.2–§4.3](../research/13_proactive_channel_watcher_audit.md)):
1. **Read-Destructive Outbox Drain (`§5.7`):** `DrainBriefRollup` (`internal/outbox/outbox.go:578-604`) marks items `delivered` and writes `reported.jsonl` **on read**. Running `hq brief collect` manually before the `07:30` cron—or hitting an API `429`/crash mid-turn—permanently loses overnight `brief_rollup` and quiet-hours-held items.
2. **Untrusted Laundering into `front_desk_cron` (`A4`, `A5`):** `templates/triggers.yaml:3-8` runs `morning-brief` as `mode: front_desk_cron` inside **`[HQ] Front Desk`**—the longest-lived, highest-privilege coordinator context. Feeding raw `quarantined: true` connector output, third-party incident/ticket strings from enterprise CLIs, or unvetted plugin Markdown instructions (`hq-brief/*.md`) directly into Front Desk exposes it to prompt injection.

---

## 2. Architecture Overview

```mermaid
flowchart TD
    Peek["hq brief collect (default: 100% read-only peek)"]
    Lease["hq brief collect --lease (20m lease_token under flock)"]

    subgraph Sources["Parallel Bounded Collector (5s wall-clock cap)"]
        OS["Core OS State: Pending Proposals (TTL warning), Goals/SLAs, Worktrees, Stat-Cached 24h Tokens"]
        OB["Locked Outbox: Overnight brief_rollup + Pre-07:30 Quiet-Hours Holds (ProposalIDs intact)"]
        TC["Trusted Internal Sources (quarantined: false via Vault justification)"]
        QC["Quarantined Connectors & Default argv Extensions (quarantined: true): Counts & Opaque IDs Only"]
    end

    Lease --> Sources
    QC -->|"Optional unread delta"| QR["Tainted Brief Reader Worker -> Go-Validated JSON Schema + Nonce Fence"]
    OS & OB & TC & QR --> FD["HQ Front Desk (/morning-brief or pack override)"]
    FD -->|"1. Write brief-YYYY-MM-DD.md"| Note["<vault>/notes/general/brief-YYYY-MM-DD.md"]
    Note -->|"2. hq brief commit --lease=<token>"| Commit["Mark Leased Outbox Items Delivered + Fenced reported.jsonl"]
```

---

## 3. Two-Phase Non-Destructive Collection (`internal/brief` + `hq brief`)

### 3.1 Idempotent Peek vs. Caller-Bound `--lease` / `hq brief commit` (`S19`, `P1-3`)
1. **Default Read-Only Peek (`hq brief collect [--json]`):**
   - Performs **zero** file writes on `memory/outbox/items.jsonl` or `reported.jsonl`; expired leases (`now > LeaseExpiresAt`) are evaluated as a **computed read view** so previews stay 100% side-effect-free.
2. **Caller-Bound Lease Acquisition (`hq brief collect --lease [--ttl=20m] [--json]`):**
   - Acquires `internal/store` `RankOutbox` on `memory/outbox/items.jsonl`, mints `lease_token = "blease_<random128>"`, and stores **`LeaseTokenHash = sha256(lease_token)`** + `LeaseConvID = envConvID` + `LeaseExpiresAt = now + 20m` (preventing readable-outbox lease theft; `OutboxDispatcher` skips items carrying an unexpired `LeaseTokenHash` and dispatches interrupts via 3-phase claim → unlocked deliver → commit).
3. **Post-Write Commit (`hq brief commit --lease=<token> --brief-file=<path>`):**
   - Verifies caller `envConvID == LeaseConvID`, confines `<brief-file>` canonically under `<vault>/notes/` (`no symlinks`, non-empty), and atomically transitions matching `LeaseTokenHash` items to `Status: "delivered"` + batch-appends nonce-fenced entries to `reported.jsonl` inside the same `RankOutbox` critical section.
   - **Backward-Compatible `hq outbox drain` Wrapper (`CUJ-21`):** Updates `.agents/plugins/agy-hq/skills/morning-brief/SKILL.md` to use `--lease` / `commit` while preserving `hq outbox drain`'s exact stdout (`"Drained N brief_rollup item(s)"`) and emitting its deprecation notice strictly on `stderr`.

### 3.2 Realistic Bounded Execution & Proposal TTL Warnings (`C14`, `§5.7`)
- **Bounded Parallel Fan-Out:** Connector `brief` capabilities and deterministic `argv` extensions execute inside the [RFC-0017a](0017a-connector-protocol-and-watcher-engine.md) bounded worker pool (`per_connector_timeout` default `3s`, overall collector deadline `5s`). Offline corporate VPNs or expired SSO tokens return `{section, connector, status: "offline", error_code: "transient"|"auth_expired"}` without stalling the brief.
- **Stat-Cached Token Telemetry:** Reads the pre-aggregated stat-cached observability index (`<100ms`) resolved via `internal/hostpaths` ([RFC-0017d §5.2](0017d-enterprise-extensibility-and-portability.md)) rather than cold-scanning raw transcript JSONLs.
- **Expiring Proposal Alerting:** Proposals with `< 4h` remaining TTL (or expired overnight) carry `ttl_warning: "expires_soon" | "expired_overnight"` so `/morning-brief` highlights them first or offers 1-step re-staging.

### 3.3 Gated Front Desk Pre-Run Compaction & Post-Compaction Priority Reload
Because `morning-brief` executes as `mode: front_desk_cron` inside long-lived **`[HQ] Front Desk`**, stale multi-day context increases uncached cold-cache prompt tokens across idle overnight gaps while `SessionStart` does not re-run after `/compact`. To keep `CompactThenContinue` safe and free of wasted summaries:
1. **Explicit Opt-In Metadata (`compact_before_run: true`):** Only `front_desk_cron` schedules that explicitly set `compact_before_run: true` (seeded on `morning-brief` in `templates/triggers.yaml` and added non-destructively to unmodified vaults via `v12` migration) pass opt-in gating metadata (`compact_before_run`, optional `compact_context_tokens`, optional `compact_min_planner_steps`) into `DispatchPendingEvents`. Regular `interrupt` outbox cards, channel inbound messages, follow-up timers, and deferred approvals delivered via `DeliverMessageToSession` never compact.
2. **Dual-Condition Compaction Gate (`EvaluateFrontDeskCompactionGate`):** `internal/obs` filters `CHECKPOINT` steps to completed compactions (`""`, `"DONE"`, `"CORTEX_STEP_STATUS_DONE"`), tracks `PlannerResponsesSinceCheckpoint`, and exposes `InspectFrontDeskContext` (registered into `internal/kernel` at package init). Compaction triggers **only** when BOTH `CurrentContextTokens >= threshold` (schedule `compact_context_tokens` > `budget.json` `autoCompactContextTokens` > `warnContextTokens` `120,000`) **and** `PlannerResponsesSinceCheckpoint >= minSteps` (default `2`, skipping freshly compacted or near-empty shifts).
3. **Idle-Safe LS Delivery & Plain Fallbacks (`SendUserCascadeMessage`):**
   - Calls `LanguageServerService/SendUserCascadeMessage` (`Connect-Protocol-Version: 1`, `x-codeium-csrf-token`) with `blocking: false`, `deliveryStrategy: "MESSAGE_DELIVERY_STRATEGY_WHEN_IDLE"` (preventing preemption of active turns or running subagents), and omits `cascadeConfig` so the Front Desk session's active `planModel` is never overridden.
   - Prepends `{"item":{"slashCommand":{"info":{"name":"compact","type":"SLASH_COMMAND_TYPE_SYSTEM"}}}}` ahead of `{"text": "<prompt>"}` strictly on the Language Server RPC when the gate passes.
   - If the Language Server rejects `slashCommand` with an HTTP error, retries `SendUserCascadeMessage` with plain prompt text (`items: [{"text": "<prompt>"}]`) instead; if LS is unavailable and `agentapi send-message` is used as a fallback, it strictly delivers plain prompt text only (never raw `"/compact"`).
4. **Post-Compaction Priority Reload (`active_priorities`):** `hq brief collect` (`internal/brief`) populates `active_priorities` directly from `<vault>/MEMORY.md` (bounded to `MaxMemoryIndexLines` / `MaxMemoryIndexBytes`) so `/morning-brief` always loads fresh overnight priorities after `/compact` without re-running `SessionStart` or spending Front Desk inline lookup quota.

---

## 4. Provenance-Safe Brief Synthesis (`A4` Closed)

Because `/morning-brief` runs inside untainted `[HQ] Front Desk`, `hq brief collect` enforces a strict provenance split:
1. **`quarantined: false` Sources (Trusted Internal Metrics Only):** Local engineering worktrees, internal numeric counters, and Vault-justified internal telemetry pass structured fields directly into the brief packet.
2. **`quarantined: true` Connectors & Default `argv` Extensions (`gmail`, `gchat`, `slack`, Ticket/Incident CLIs):**
   - `hq brief collect` emits **aggregate counts, authenticated sender domains, and opaque item/thread IDs only**—plus already-triaged overnight `OutboxItem`s rendered inside [RFC-0017b](0017b-provenance-taint-and-offer-gateway.md) cryptographic nonce fences (`BoundedSubject <= 120` runes + proposal metadata).
   - **Tainted Reader Summarization (`--summarize-quarantined` / `[tier:quick] brief: Quarantined Digest Reader`):** Any unread quarantined items not already summarized overnight are read exclusively inside an **auto-tainted reader worker** (non-overridable by packs per [RFC-0017d §4.2](0017d-enterprise-extensibility-and-portability.md)) constrained to return a **Go-validated strict JSON array** (`{item_id, urgency_enum, category_enum, bounded_subject}`), wrapped in a cryptographic nonce fence before `[HQ] Front Desk` sees the packet. Raw untrusted bodies **never** enter Front Desk context.

---

## 5. Structured Brief Extensions (`<vault>/brief.d/` & Pack `hq-brief/`, `A5` Closed)

Brief extensions integrate directly into the unified companion pack & trust model ([RFC-0017d §2–§3](0017d-enterprise-extensibility-and-portability.md)):

### 5.1 Mode 1 — Deterministic `argv` Extensions (`quarantined: true` Default)
Companion packs index `hq-brief/*.json` under `hq-extension.json` `components.brief_extensions` (and local users may place `.json` specs in `<vault>/brief.d/`), SHA-256 pinned in `<vault>/private/trust.json`:
```json
{
  "schema_version": 1,
  "id": "team-oncall-status",
  "section": "Operations",
  "mode": "argv",
  "argv": ["corp-oncall", "current", "--team=platform", "--json"],
  "timeout_ms": 2500,
  "quarantined": true
}
```
- **Fail-Closed Provenance Default:** Because enterprise CLI outputs (incident titles, customer ticket summaries, commit titles) frequently contain third-party text, **`quarantined` defaults to `true`** on all `mode: "argv"` brief extensions (`hq extension lint` rejects `quarantined: false` in companion packs; relaxing to `false` requires a `<vault>/extensions.json` `security_loosening` entry with a $\ge 16$-char justification recorded in `private/trust.json`).

### 5.2 Mode 2 — Vault-Only Trusted Checklists (`<vault>/brief.d/*.md`)
Free-form natural-language brief checklists (e.g., `<vault>/brief.d/morning-activities.md`) are **never** loaded from companion plugins (`hq-brief/` permits `mode: "argv"` JSON only). Inside `<vault>/brief.d/`, a `.md` checklist is included in `hq brief collect` **only** when pinned in `<vault>/private/trust.json` via human-presence-verified `hq extension trust vault` ([RFC-0017d §3](0017d-enterprise-extensibility-and-portability.md)).

### 5.3 Trampoline-Aware, Vendor-Neutral `skills/morning-brief/SKILL.md`
`skills/morning-brief/SKILL.md` becomes a concise, 100% vendor- and persona-neutral workflow:
0. **Dispatch & Trigger Guard ([RFC-0017d §2.3, §4.2](0017d-enterprise-extensibility-and-portability.md)):** Run `hq dispatch resolve --skill=morning-brief [--trigger=core/morning-brief]`: if `enabled == false`, stop cleanly; if a trusted companion `skill_override` is active, activate that skill and stop following this file.
1. Run `hq brief collect --lease --json` (spawning the tainted reader worker only when `unurged_quarantined_ids` is non-empty).
2. Execute any `trust.json`-verified `<vault>/brief.d/*.md` checklists and review active `<vault>/notes/` project deltas.
3. Save `<vault>/notes/general/brief-YYYY-MM-DD.md`, run `hq brief commit --lease=<token> --brief-file=<path>`, and present the executive brief + 1-tap `ask_question` approvals (highlighting any `expires_soon` proposals).

---

## 6. Personal vs. Enterprise Customization Matrix (`0` Core Code Changes)

| Capability | Personal Workstation Default | Enterprise Workstation Setup | Extension Unit (`RFC-0017d`) |
|---|---|---|---|
| **Mail & Chat** | Embedded `core/gmail` + `core/gchat` | `corp-mail` / `gmail` (`cli.path`, `draft_only`) + `gchat` / `slack` | `instance.json` or pack `hq-connectors/` |
| **Calendar** | Embedded `core/gcalendar` (`low` `freebusy`) | Corporate `gcalendar` (`AGY_ACCOUNT=user@acme.com`) or enterprise CLI | `<vault>/connectors/` or pack |
| **Engineering / Ops** | Local git worktrees + optional `gh` | `jira` (`jsonmap` over `twg`), `gerrit`, `corp-oncall` | Pack `hq-extension.json` (`hq extension trust`) |
| **Custom Brief & Overrides** | Private `<vault>/brief.d/*.md` (`hq extension trust vault`) | Pack `hq-brief/*.json` (`quarantined: true`) + optional `skill_overrides` | `<vault>/brief.d/` or companion pack |

---

## 7. Phase 2 Verification Plan (`CUJ-30b`, `CUJ-32`, `CUJ-45`)

- **`TestCUJ30b_LeaseBasedMorningBriefAndStructuredExtensions`:** Verifies Step-0 `hq dispatch resolve` disabled/override handling, idempotent `hq brief collect` peek, `--lease` crash/expiry recovery (`0` lost items), `hq brief commit` post-file-write delivery + `< 4h` TTL warnings, `quarantined: true` default on `mode: "argv"` brief extensions (counts/IDs + tainted reader JSON fence only), unpinned `brief.d` / pack `mode: "prompt"` rejection, and clean execution with consumer connectors disabled.
- **`TestCUJ32_OutboxQuietHoursAndFencedSystemDelivery`:** Verifies `tz:`/`enabled` parser fix, `07:30` quiet-hours alignment, `DedupKey` + `ThreadKey` burst superseding (both `OutboxItem` and `Proposal`), nonce-fenced system card formatting, and `hq outbox eval` `100%` pass.
- **`TestCUJ45_MorningBriefGatedFrontDeskCompaction`:** Verifies `v12` migration enabling `compact_before_run` cleanly, dual-condition gating (`CurrentContextTokens` watermark + `PlannerResponsesSinceCheckpoint` after `DONE` `CHECKPOINT` ignoring incomplete `RUNNING` checkpoints), `CompactThenContinue` JSON schema (`Connect-Protocol-Version: 1`, `MESSAGE_DELIVERY_STRATEGY_WHEN_IDLE`, omitted `cascadeConfig`), plain fallback when LS rejects `slashCommand`, plain-text `agentapi` fallback safety, `active_priorities` reload in `hq brief collect`, and zero compaction on regular outbox/timer alerts.
