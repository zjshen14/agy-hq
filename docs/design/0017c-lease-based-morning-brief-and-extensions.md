# RFC-0017c: Non-Destructive Lease-Based Morning Brief Collector & Structured Extensions

- **Status:** Proposed (Phase 1 Review)
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

### 3.1 Idempotent Peek vs. `--lease` / `hq brief commit`
1. **Default Read-Only Peek (`hq brief collect [--json]`):**
   - Performs **zero** state mutations on `memory/outbox/items.jsonl` or `reported.jsonl`. Safe for CLI previews, debugging, and dry runs at any time.
2. **Lease Acquisition (`hq brief collect --lease [--ttl=20m] [--json]`):**
   - Acquires `internal/store` `flock` on `memory/outbox/items.jsonl`, generates `lease_token = "blease_<random128>"`, and stamps `LeaseToken` + `LeaseExpiresAt = now + 20m` onto eligible pending `brief_rollup` items (plus overnight quiet-hours-held `interrupt` items created before `quiet_hours.end`, preventing double-notification between quiet-hours end and `07:30`).
   - Items remain `Status: "pending"` until committed; if a turn crashes or hits `429`, expired leases revert automatically on the next read or `OutboxDispatcher` tick.
3. **Post-Write Commit (`hq brief commit --lease=<token> --brief-file=<path>`):**
   - Verifies `<brief-file>` exists and is non-empty, then atomically transitions items matching `lease_token` to `Status: "delivered"` and records nonce-fenced entries in `reported.jsonl`.
   - **Legacy `hq outbox drain` Migration:** Updates `skills/morning-brief/SKILL.md` in the same commit to use `--lease` / `commit`; `hq outbox drain` becomes a compatibility wrapper emitting a deprecation notice on `stderr`.

### 3.2 Realistic Bounded Execution & Proposal TTL Warnings (`C14`, `§5.7`)
- **Bounded Parallel Fan-Out:** Connector `brief` capabilities and deterministic `argv` extensions execute inside the [RFC-0017a](0017a-connector-protocol-and-watcher-engine.md) bounded worker pool (`per_connector_timeout` default `3s`, overall collector deadline `5s`). Offline corporate VPNs or expired SSO tokens return `{section, connector, status: "offline", error_code: "transient"|"auth_expired"}` without stalling the brief.
- **Stat-Cached Token Telemetry:** Reads the pre-aggregated stat-cached observability index (`<100ms`) resolved via `internal/hostpaths` ([RFC-0017d §5.2](0017d-enterprise-extensibility-and-portability.md)) rather than cold-scanning raw transcript JSONLs.
- **Expiring Proposal Alerting:** Proposals with `< 4h` remaining TTL (or expired overnight) carry `ttl_warning: "expires_soon" | "expired_overnight"` so `/morning-brief` highlights them first or offers 1-step re-staging.

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

## 7. Phase 2 Verification Plan (`CUJ-30b` & `CUJ-32`)

- **`TestCUJ30b_LeaseBasedMorningBriefAndStructuredExtensions`:** Verifies Step-0 `hq dispatch resolve` disabled/override handling, idempotent `hq brief collect` peek, `--lease` crash/expiry recovery (`0` lost items), `hq brief commit` post-file-write delivery + `< 4h` TTL warnings, `quarantined: true` default on `mode: "argv"` brief extensions (counts/IDs + tainted reader JSON fence only), unpinned `brief.d` / pack `mode: "prompt"` rejection, and clean execution with consumer connectors disabled.
- **`TestCUJ32_OutboxQuietHoursAndFencedSystemDelivery`:** Verifies `tz:`/`enabled` parser fix, `07:30` quiet-hours alignment, `DedupKey` + `ThreadKey` burst superseding (both `OutboxItem` and `Proposal`), nonce-fenced system card formatting, and `hq outbox eval` `100%` pass.
