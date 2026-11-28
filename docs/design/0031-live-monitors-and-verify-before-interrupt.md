# RFC-0031: Live External Monitors, Active Timezone Resolution, Verify-Before-Interrupt Corroboration & Safe External-Channel Boundaries

- **Status:** Proposed (Rev 1.1 — Locked `/grill-me` Decisions Q1–Q6; Awaiting Phase 2 Approval)
- **Authors:** `agy-hq` Architecture
- **Related Research:** [Research 26 — Live Monitors, Timezone Resolution, Corroboration & Family Channel Audit](../research/26_live_monitors_timezone_corroboration_and_family_channels_audit.md)
- **Builds On:** [RFC-0014](0014-proactive-engine-and-living-context.md), [RFC-0017a](0017a-connector-protocol-and-watcher-engine.md), [RFC-0023](0023-front-desk-channel-bridge.md), [RFC-0026](0026-declarative-scoped-delegates.md), [RFC-0027](0027-muse-proactive-actions.md), [RFC-0029](0029-rsi-v2-trace-eval-and-heldout-ratchet.md)

---

## 1. One-Page Executive Summary (TL;DR)

[RFC-0027](0027-muse-proactive-actions.md) gave `agy-hq` look-ahead checks across local Vault data (`hq pulse`), 1-tap decision buttons, and interactive Living Spaces. Real-world field testing across international travel and mobile alerts exposed **four concrete proactive gaps** where alerts either arrived hours late, missed live external disruptions entirely, or woke the user for an already-resolved failure.

### Problem Today → What We Propose

| # | Problem Today | What RFC-0031 Proposes | LLM Token Cost |
|---|---|---|---|
| **1. Home-TZ Blind Spot** | `gmail-inbox-watch`, `morning-brief`, and `quiet_hours` stay pinned to your home timezone (`America/Los_Angeles`). Traveling `+8h` ahead puts watchers to sleep from `06:00–15:00` local daytime. | **Active Timezone Resolver (`ResolveActiveTimezone`)**: Automatically follows your active travel/on-site Goal's `timezone` (or a quick `hq tz set Europe/London --until=...` override) for `tz_mode: active` schedules and `quiet_hours`. Fixed-clock jobs (e.g., US market open) stay pinned via `tz_mode: fixed`. | `0` tokens (pure Go) |
| **2. No Short-Lived Live Pollers** | `hq daemon` only polls Gmail/GChat and local Vault files. Watching a live train, flight, statuspage outage, or deploy rollout requires burning LLM tokens on fragile chat crons. | **Ephemeral, Goal-Bound Live Monitors (`internal/monitors/`)**: Auto-expiring, 0-token Go pollers (`http_json`, `http_text`, `ics_feed`, + opt-in SHA-256-pinned `hq-monitor-provider/v1` packs) bound to a Goal/window. Diffs live snapshots against last-delivered baseline, stays **100% silent** when unchanged, alerts only on material deltas, and commits baseline **only after delivery succeeds**. | `0` tokens on unchanged ticks |
| **3. Unverified Interrupts** | Failure alerts (payment failed, CI broken) fire immediately even when a paid receipt or green build already arrived. Conflicting live feeds can surface stale ETAs. | **Verify-Before-Interrupt Corroboration (`internal/outbox/corroborate.go`)**: Right before delivering an interrupt (Phase B, unlocked), Go runs a bounded 0-token check: (a) **Live Monitors** re-fetch live and resolve multi-source conflicts by preferring the fresher `source_updated_at` timestamp; (b) **Failure Alerts** auto-infer `CorroborationSpec` in Go (overridable via CLI flags), query the source connector for newer resolution signals (paid receipt / build passed), and downgrade already-resolved false alarms to `silent_ledger`. Late receipts auto-reconcile prior interrupts. | `0` tokens (pure Go) |
| **4. Off-Channel Family Updates** | Schedule changes texted in WhatsApp / Messenger / iMessage bypass Gmail/GChat — but scraping or routing third-party chats into Front Desk breaks Lethal Trifecta security. | **Strict 3-Layer Channel Boundary**: Keep RFC-0023 strictly 1:1 owner-only. Ingest third-party/family channels safely via: (1) opt-in read-only `hq-connector-protocol/v1` packs (`quarantined_reader`), (2) RFC-0026 scoped family delegates (`caller_groups` + CaMeL `queue_only` nudges), and (3) a zero-infra owner-forward fast-path (`hq space note` / `hq goals update-leg`) + read-only `.ics` calendar monitor. | `0` tokens for intake/ICS |

### Concrete Before / After Examples (Work & Personal)

- **Work — On-Site Conference Travel (`UTC+0` vs. Home `UTC-8`) & Production Rollout:**
  - *Before:* While at a London customer summit, `gmail-inbox-watch` sleeps all morning (`06:00–15:00 GMT`), a P1 customer email sits unread until afternoon, and watching a `statuspage.io` cloud incident requires manually refreshing or burning tokens on a chat cron.
  - *After:* Your active `london-summit` Goal (`timezone: Europe/London`, `blocksAvailability: true`) automatically shifts inbox watch, Morning Brief (`07:30 GMT`), and quiet hours (`22:00–07:00 GMT`) to local time. An auto-expiring 6-hour `http_json` monitor watches the incident JSON feed at `0` tokens and interrupts only when `status` changes (`investigating → resolved`).
- **Work — Transient CI / Billing Failure:**
  - *Before:* A flaky nightly build email fires a noisy interrupt; 2 minutes later the auto-retry passes, leaving a stale red alert on Front Desk.
  - *After:* The interrupt waits in its `3m` corroboration hold, runs a `0`-token connector search for a newer `"passed"` message on the same repo/pipeline, sees the green retry, and silently downgrades to `silent_ledger`.
- **Personal — Live Rail / Flight Disruption & Payment False Positive:**
  - *Before:* Your `17:00` train is cancelled at `14:35` local (`06:35` home TZ) while HQ is asleep; after you rebook onto an earlier train that breaks down mid-route, HQ has zero visibility into rolling ETA slips (`21:02 → 21:24 → 21:52`). Meanwhile, a 4-hour-old rideshare wallet-failure email wakes you up right before the settled receipt arrives.
  - *After:* Active TZ keeps watchers awake all UK daytime. A leg-bound live monitor (`T-4h` to arrival) catches the `14:35` cancellation immediately, then tracks the rebooked train, resolves a stale `19:30` destination board against a fresher `19:44` intermediate station board by `source_updated_at`, alerts once per `>= 10m` ETA slip, and stays silent when steady. The payment-failure alert runs a `0`-token receipt check before interrupting — and any late receipt automatically reconciles and clears the alert.

---

## 2. Strict OSS Framework vs. Private Vault Boundary

Per `agy-hq`'s Two-Layer Architecture ([ADR-0003](../decisions/0003-two-layer-oss-and-private-vault.md)), **no personal domains, rail APIs, family contacts, or home locations enter the OSS engine**:

| Layer | Lives In | Contains |
|---|---|---|
| **OSS Framework (`agy-hq` Core)** | `internal/kernel/timezone.go`, `internal/monitors/`, `internal/outbox/corroborate.go`, `cmd/hq/monitors_cli.go`, `templates/{config,triggers}.yaml` | Generic `ResolveActiveTimezone()`, `tz_mode: active \| fixed`, pure-Go stdlib `http_json` + `http_text` + `.ics` monitor engine, SSRF IP firewall, `hq-monitor-provider/v1` protocol, `DeltaPolicy` & `source_updated_at` freshness resolver, `Verify-Before-Interrupt` Phase B hook, generic failure/resolution corroboration spec, `hq tz` / `hq monitor` / `hq goals watch` CLIs |
| **Private User Vault (`<vault>/`)** | `<vault>/config.yaml`, `<vault>/triggers.yaml`, `<vault>/memory/monitors/state.json`, `<vault>/private/delegates/policy.json`, optional local companion packs | Your home timezone & `monitors.allowed_hosts` allowlist, active personal/work Goals & Living Spaces, optional private companion packs (e.g., National Rail / FlightAware monitor provider or WhatsApp/Telegram family connector), and your family `caller_groups` policy |

---

## 3. Gap 1 — Active Timezone Resolution (`internal/kernel/timezone.go`)

### 3.1 Deterministic Resolution Priority

A single pure-Go function `kernel.ResolveActiveTimezone(vaultDir, fallbackTZ string, now time.Time) ActiveTimezone`:

```go
type ActiveTimezone struct {
    Timezone string `json:"timezone"` // Validated IANA name (e.g. "Europe/London")
    Source   string `json:"source"`   // "override" | "goal:<slug>" | "config_home" | "fallback"
    Until    string `json:"until,omitempty"`
}
```

Resolution precedence (evaluated deterministically in `0` tokens, `< 1ms`):
1. **Explicit Vault Override (`<vault>/config.yaml` `attention.active_timezone_override`):**
   - Set/cleared via `hq tz set <IANA> [--until=<RFC3339|YYYY-MM-DD>]` and `hq tz clear` (or persisted in `<vault>/memory/tz_override.json` under `RankTriggerCur` so CLI mutations never rewrite hand-edited `config.yaml` comments!).
   - Auto-expires cleanly when `now >= until`.
2. **Active Goal Window (`<vault>/memory/goals.json`):**
   - Scan `status == "active"` non-parked Goals where:
     - `Goal.Timezone != ""` (valid `time.LoadLocation`),
     - `Goal.WindowStartAt != ""` and `Goal.WindowEndAt != ""` and `now` lies within `[start, end]`, AND
     - `Goal.FollowTimezone` is `true` (defaulting to `true` when `Goal.BlocksAvailability == true` unless explicitly set `false`).
   - Deterministic tie-break if multiple Goals overlap: narrowest window duration first, then lexicographical `Goal.Slug`.
3. **Vault Home Timezone:** `attention.quiet_hours.tz` in `<vault>/config.yaml`.
4. **Fallback:** The schedule's own static `CRON_TZ=` value or host `now.Location()`.

### 3.2 Schedule & Quiet-Hours Integration

1. **`ScheduleTrigger.TZMode` (`triggers.yaml`):**
   - Add optional field `tz_mode: fixed | active` (defaults to `fixed` for backward compatibility on all existing custom/pack schedules).
   - Also recognize inline `CRON_TZ=ACTIVE` in cron strings as an alias for `tz_mode: active`.
   - Update `templates/triggers.yaml` defaults so user-aligned schedules use `tz_mode: active`:
     - `gmail-inbox-watch`: `CRON_TZ=ACTIVE */15 7-22 * * *` (watches throughout your actual waking day, wherever you are).
     - `morning-brief`: `CRON_TZ=ACTIVE 30 7 * * *` (delivers at `07:30` local morning instead of `15:30` afternoon).
     - `sleep-consolidate`: `CRON_TZ=ACTIVE 30 23 * * *` (runs while you actually sleep).
   - Market/external clock jobs (e.g., `edgar-close-watch` `CRON_TZ=America/New_York 30 16 * * 1-5`) keep `tz_mode: fixed`.
2. **TZ-Shift Anti-Double-Fire Guard:**
   - Add `MatchesCronInLocation(cronExpr string, loc *time.Location, now time.Time) (bool, string)` alongside `MatchesCron`.
   - For daily schedules (fixed hour+minute, no `*` or `/` in hour field, e.g., `morning-brief` and `sleep-consolidate`), store `LastLocalDate` (`YYYY-MM-DD`) in `TriggerCursor` alongside `LastCronSlot` and enforce a minimum `12h` interval between runs when active TZ shifts mid-day (prevents double Morning Brief on an eastward flight day).
3. **Quiet Hours & Scope Rules (`internal/outbox/outbox.go`):**
   - Add `QuietHoursConfig.FollowActiveTZ *bool` (`follow_active_tz`, defaulting to `true` when `quiet_hours.enabled` is true).
   - Pass resolved `activeLoc` into `InQuietHoursAt(cfg, activeLoc, now)` (preserving `InQuietHours(cfg, now)` signature for existing callers/tests).
   - `hq status`, `hq doctor`, and HQ Console surface the active TZ badge when shifted away from home (e.g., `Active TZ: Europe/London (goal: uk-trip, home: America/Los_Angeles)`).
4. **Living Space ↔ Goal Timezone Linkage:**
   - Extend `hq space create` with optional `--window-start`, `--window-end`, `--tz` flags (auto-creating/linking a `blocksAvailability: true` Goal when `--goal` is omitted), and add a low-noise `hq pulse` advisory when an active Living Space has an upcoming `countdownAt` but no linked timezone-bearing Goal.

---

## 4. Gap 2 — Ephemeral, Goal-Bound Live Monitors (`internal/monitors/`)

### 4.1 Architecture & Lock Discipline

New dedicated package **`internal/monitors/`** (keeping `kernel.go` and `main.go` untouched/under `H5-F` ceilings) with state persisted at `<vault>/memory/monitors/state.json` (`0600`) under **`store.RankMonitors = 31`** (strictly between `RankWatcher = 30` and `RankPulse = 32`).

```
  hq daemon (60s monitorTicker)
        │
        ▼
  [Phase 1: Snapshot due monitors under RankMonitors=31, release lock]
        │
        ▼
  [Phase 2: Unlocked bounded fetch (http_json / ics / provider stdio, <= 8s)]
        │  └─ Multi-source fetch -> freshness_wins conflict resolution (source_updated_at)
        │  └─ Diff vs. Monitor.Baseline (last DELIVERED snapshot) via DeltaPolicy
        │       ├─ Unchanged / sub-threshold -> record LastCheckedAt only (0 tokens, 0 alerts)
        │       └─ Material delta -> emit OutboxItem (tier=interrupt, VerifyMode=monitor_refetch)
        ▼
  [Phase 3: Commit LastCheckedAt, LastObserved, PendingOutboxID under RankMonitors=31]
        │
        ▼
  outbox.DispatchPendingInterrupts (Phase B unlocked)
        ├─ Calls monitors.Reverify(monitorID) right before delivery
        │    ├─ Self-cleared -> downgrade OutboxItem to silent_ledger (no wakeup!)
        │    └─ Confirmed / fresher update -> refresh summary & deliver
        ▼
  [Post-Delivery Hook -> monitors.CommitBaseline(monitorID, deliveredSnapshot) under RankMonitors=31]
```

**Why Commit-Baseline-After-Delivery Matters:**
- If delivery fails (e.g., network drop, Front Desk offline, or quiet-hours hold), `Monitor.Baseline` remains at the **previous delivered state** — so the user never misses an urgent delta.
- The moment delivery succeeds (`StatusDelivered`), `Monitor.Baseline` atomically advances to the delivered snapshot — guaranteeing **zero duplicate alerts** on subsequent polls.

### 4.2 Core Data Model

```go
type Monitor struct {
    ID                  string          `json:"id"`                       // e.g. "mon-rail-1a23"
    Title               string          `json:"title"`                    // Human label
    Scope               string          `json:"scope,omitempty"`          // e.g. "travel" | "ops" | "engineering"
    GoalSlug            string          `json:"goalSlug,omitempty"`       // Linked Goal (optional)
    ChecklistItemID     string          `json:"checklistItemId,omitempty"`
    SpaceSlug           string          `json:"spaceSlug,omitempty"`      // Linked Living Space (optional)
    Status              string          `json:"status"`                   // "active" | "paused" | "completed" | "expired"
    Kind                string          `json:"kind"`                     // "http_json" | "http_text" | "ics_feed" | "pack_provider"
    Sources             []SourceSpec    `json:"sources"`                  // 1..3 sources (primary + corroborating)
    IntervalSeconds     int             `json:"intervalSeconds"`          // Min 60s, default 300s
    ActiveFrom          string          `json:"activeFrom,omitempty"`     // RFC3339 (e.g. T-4h)
    ExpiresAt           string          `json:"expiresAt"`                // Mandatory RFC3339 (max 72h default TTL)
    TerminalStatuses    []string        `json:"terminalStatuses,omitempty"` // Auto-complete (e.g. ["arrived","resolved","deployed"])
    CriticalStatuses    []string        `json:"criticalStatuses,omitempty"` // Owner-chosen statuses allowed to bypass quiet hours
    Delta               DeltaPolicy     `json:"delta"`
    Baseline            *Snapshot       `json:"baseline,omitempty"`       // Last DELIVERED snapshot
    LastObserved        *Snapshot       `json:"lastObserved,omitempty"`   // Most recent raw poll snapshot
    PendingOutboxID     string          `json:"pendingOutboxId,omitempty"`
    LastCheckedAt       string          `json:"lastCheckedAt,omitempty"`
    NextPollAt          string          `json:"nextPollAt,omitempty"`
    ConsecutiveErrors   int             `json:"consecutiveErrors,omitempty"`
    LastError           string          `json:"lastError,omitempty"`
    CreatedAt           string          `json:"createdAt"`
    UpdatedAt           string          `json:"updatedAt"`
}

type Snapshot struct {
    Status          string            `json:"status"`                    // Normalized enum/string (e.g. "on_time", "delayed", "cancelled", "degraded")
    ETA             string            `json:"eta,omitempty"`             // RFC3339 or HH:MM
    DelayMinutes    *int              `json:"delayMinutes,omitempty"`    // Optional signed delay delta
    Location        string            `json:"location,omitempty"`        // Platform, gate, region, or stage
    Summary         string            `json:"summary,omitempty"`         // Short anti-cue-sanitized text (<= 240 runes)
    Fields          map[string]string `json:"fields,omitempty"`          // Optional watched scalar fields
    SourceID        string            `json:"sourceId"`                  // Winning source label
    SourceUpdatedAt string            `json:"sourceUpdatedAt,omitempty"` // Authoritative feed timestamp (RFC3339)
    FetchedAt       string            `json:"fetchedAt"`                 // Local fetch timestamp (RFC3339)
    SupersededNote  string            `json:"supersededNote,omitempty"`  // Provenance note when freshness_wins resolved a conflict
}

type DeltaPolicy struct {
    MinETADeltaMinutes int      `json:"minEtaDeltaMinutes,omitempty"` // Default 10
    AlertOnStatus      bool     `json:"alertOnStatus"`                // Default true
    AlertOnLocation    bool     `json:"alertOnLocation"`              // Default true
    WatchFields        []string `json:"watchFields,omitempty"`
    AlertOnFirstPoll   bool     `json:"alertOnFirstPoll,omitempty"`   // Default false (seed baseline silently unless status != normal)
    AbnormalStatuses   []string `json:"abnormalStatuses,omitempty"`   // Alert even on first poll (e.g. ["cancelled","delayed","outage","failed"])
}
```

### 4.3 Two-Tier Provider Mechanism (0 External Go Dependencies)

1. **Built-In `http_json` Kind (`internal/monitors/http_json.go`):**
   - Pure Go `net/http` `GET` request with RFC 6901 JSON-Pointer field mapping (reusing `internal/connector/jsonmap.go` primitives).
   - Works out-of-the-box for `statuspage.io` (`/api/v2/summary.json`), GitHub Deployments/Releases/Actions status endpoints, public transit JSON APIs, weather alert JSON endpoints, and custom internal healthchecks.
2. **Built-In `http_text` Kind (`internal/monitors/http_text.go`):**
   - Pure Go `net/http` `GET` request over bounded text/HTML (`<= 512 KB`, strips `<script>`/`<style>`/tags into normalized plain text) + linear-time Go RE2 named-group regex extraction (`status`, `eta`, `location`, `summary`, `source_updated_at`). Zero ReDoS risk (Go `regexp` guarantees linear time), enabling public HTML rail/departure boards and simple status pages out-of-the-box with zero custom pack code.
3. **Built-In `ics_feed` Kind (`internal/monitors/ics.go`):**
   - Tiny pure-Go RFC 5545 `VEVENT` parser (`SUMMARY`, `DTSTART`, `DTEND`, `LOCATION`, `STATUS`, `LAST-MODIFIED`) for read-only shared calendar URLs (zero OAuth needed — covers shared family/team calendars cleanly).
4. **Pack-Extensible `hq-monitor-provider/v1` (`internal/monitors/provider.go`):**
   - Companion packs declare `monitor_providers` in `hq-extension.json`, SHA-256-pinned in `<vault>/private/trust.json` via `extension.VerifyTrustedBinary` (identical trust ceremony to RFC-0023/0025/0026).
   - Frozen JSON-over-stdio protocol (`hq-monitor-provider/v1`):
     - Request: `{"protocol":"hq-monitor-provider/v1","verb":"fetch","params":{...},"timeout_ms":8000}`
     - Response: `{"ok":true,"snapshot":{...}}` or `{"ok":false,"error_code":"transient|auth_expired|permanent","message":"..."}`
   - Enables domain-specific private packs (rail Darwin/LDBWS, FlightAware, internal deployment dashboards) without adding a single vendor dependency or parser to OSS `agy-hq`.

### 4.4 Multi-Source Freshness Resolution (`freshness_wins`)

When a monitor configures `2..3` sources (e.g., `origin_board` + `destination_board`, or `regional_probe` + `global_statuspage`):
1. Fetch all sources in parallel (bounded `8s` context timeout).
2. If only one succeeds, use it (with a warning footnote if the primary failed).
3. If multiple succeed and agree on `(Status, ETA, Location)` → pick highest-priority source cleanly.
4. If multiple succeed and **conflict**:
   - Compare parsed `SourceUpdatedAt` timestamps: the snapshot with the **strictly fresher `SourceUpdatedAt`** wins (e.g., intermediate station `19:44` beats destination board `19:30`).
   - If timestamps are equal or absent, fall back to declared source priority order (`Sources[0]` > `Sources[1]`).
   - Populate `Snapshot.SupersededNote` (e.g., `"Verified 19:44 via current-station board; supersedes stale 19:30 destination board"`), rendered directly on the interrupt card for full transparency.

### 4.5 Ring-0 Security Invariants for Monitors

- **M1 (Taint Creation Lockdown):** `hq monitor add|edit|resume` is hard-blocked in `quarantined_reader` and `scoped_delegate` profiles (`internal/authz` + `PreToolUse`). Tainted sessions can never arm outbound HTTP pollers.
- **M2 (SSRF & DNS-Rebinding Firewall):** `http_json`, `http_text`, and `ics_feed` enforce `https://` only, max `3` redirects, max `512 KB` body, `8s` timeout, and a custom `net.Dialer` `Control`/`DialContext` that validates the resolved IP against loopback, RFC1918 private, link-local/metadata (`169.254.0.0/16`, `fe80::/10`), and ULA (`fc00::/7`) ranges **after DNS resolution** on every hop.
- **M3 (Asymmetric Host Allowlist — Q3):** Untainted **owner-interactive** sessions (`front_desk` / `interactive_task`) may arm monitors targeting any public `https://` host directly (always protected by M1 taint lockdown + M2 post-DNS SSRF firewall). `<vault>/config.yaml` `monitors.allowed_hosts` (glob patterns like `*.statuspage.io`, `api.github.com`) is an optional strict lockdown list: when populated, all monitors must match it; for **`unattended_auto`** sessions, host allowlisting always fails closed unless the host matches `monitors.allowed_hosts`.
- **M4 (Bounded Caps & Auto-Expiry):** Hard caps: `MaxActiveMonitors = 25`, `MinIntervalSeconds = 60`, mandatory `ExpiresAt` (default `24h`, ceiling `72h` unless explicitly renewed), auto-transition to `completed` on `TerminalStatuses` or `expired` past `ExpiresAt`, and automatic circuit-breaker pause after `5` consecutive `permanent` errors.
- **M5 (Output Sanitization & Quiet-Hours Honesty):** All fetched strings pass `outbox.SanitizeAntiCue` and rune caps (`<= 240`). Monitor items are marked `Tainted: true` by default; `Critical: true` quiet-hours breach is permitted **only** if the owner explicitly configured `CriticalStatuses` on that monitor (e.g., `["cancelled", "outage"]`) and the normalized enum matches exact allowlisted status strings — never free-text injection.

### 4.6 CLI & Living Space Integration

- CLI (`cmd/hq/monitors_cli.go`):
  - `hq monitor add --id=<slug> --title="..." --kind=http_json --url=... --expires-in=6h [--interval=5m] [--goal=<slug>] [--min-eta-delta=10m]`
  - `hq goals watch <goal-slug> ...` (ergonomic sugar pre-populating `GoalSlug`, `Scope`, `SpaceSlug`, and `ActiveFrom`/`ExpiresAt` from the Goal/ChecklistItem window)
  - `hq monitor list [--all] [--json]`, `hq monitor check <id> [--dry-run]`, `hq monitor pause|resume|remove <id>`
- Living Space Built-In Binding (`internal/living/datasource.go`):
  - Add `hq_builtin` target `"monitors"` (optionally filtered by `goalSlug` / `spaceSlug`) so any Living Space renders a live status KPI/table block refreshed in `0` tokens on every material monitor update.

---

## 5. Gap 3 — Verify-Before-Interrupt Corroboration (`internal/outbox/corroborate.go`)

### 5.1 Pre-Dispatch Verification Seam in `DispatchPendingInterrupts`

`internal/outbox/dispatcher.go` already uses a clean 3-phase lock-safe pipeline:
- **Phase A (`RankOutbox`):** Claim eligible `pending` interrupts → `dispatching`.
- **Phase B (Unlocked):** Execute I/O without holding `RankOutbox`.
- **Phase C (`RankOutbox`):** Commit `delivered` or retry backoff.

RFC-0031 inserts an optional **0-token `VerifyFunc`** at the start of **Phase B** (strictly unlocked, bounded `5s` per item):

```go
type VerificationVerdict string

const (
    VerifyDeliver   VerificationVerdict = "deliver"    // Proceed with delivery (optionally with refreshed summary/footnote)
    VerifyDowngrade VerificationVerdict = "downgrade"  // Condition resolved/cleared -> downgrade to silent_ledger (or brief_rollup)
)

type VerificationResult struct {
    Verdict           VerificationVerdict
    DowngradeTier     string // default: TierSilentLedger
    DowngradedBy      string // "corroboration_cleared" | "corroborated_resolved"
    UpdatedTitle      string // optional refreshed title (e.g. fresher ETA)
    UpdatedSummary    string // optional refreshed summary
    ProvenanceNote    string // e.g. "Verified unresolved as of 15:01 UTC" or "Re-verify unavailable; showing 14:35 snapshot"
    ResolutionRef     string // corroborating threadKey / snapshot ID
    DeliveredSnapshot []byte // opaque payload passed back to post-delivery baseline commit hook
}
```

### 5.2 Mode A — Live Monitor Re-Verification (`verify_mode: "monitor_refetch"`)

Automatically attached to every `TierInterrupt` emitted by `internal/monitors/`:
1. Re-fetches the monitor's live source(s) (`5s` budget) right before sending the interrupt.
2. **Self-cleared (transient blip):** New live snapshot no longer has a material delta vs. `Monitor.Baseline` → returns `VerifyDowngrade` (`downgradedBy: "corroboration_cleared"`). User is never woken!
3. **Still active / updated further:** Refreshes `UpdatedSummary` with the latest verified snapshot (and `SupersededNote` if multi-source freshness resolved a conflict) → returns `VerifyDeliver`.
4. **Transient fetch error:** Fails open to `VerifyDeliver` with `ProvenanceNote: "Live re-verify unavailable; showing HH:MM snapshot"`.
5. **Post-delivery:** On `StatusDelivered`, `DispatchPendingInterrupts` invokes the registered post-delivery callback to commit `Monitor.Baseline = DeliveredSnapshot`.

### 5.3 Mode B — Connector Failure Corroboration (`verify_mode: "connector_corroborate"`)

Attached to failure/exception interrupts from `inbox-triage` / `watcher` (payment failed, card declined, delivery failed, CI/build failed, booking cancelled) via **automatic Go inference in `outbox.Add()`** (plus optional explicit CLI flag overrides `--corroborate-domain`, `--corroborate-tokens`, `--after`, `--no-corroborate`) into a compact `CorroborationSpec` on `OutboxItem`:

- **Go Auto-Inference (Q5):** When a tainted `inbox-triage` `TierInterrupt` has negative failure polarity (`titlePolarity < 0` or failure cues such as `"payment failed"`, `"declined"`, `"unfinished payment"`, `"build failed"`, `"undeliverable"`) and specifies `(Connector, Account)`, Go automatically populates `CorroborationSpec` by extracting any sender/merchant domain or name token and monetary/numeric ID tokens (e.g., `12.53`, `PR #42`) from `(IncidentKey, Title, Summary)` and defaulting `HoldWindow = "3m"`, `AfterTimestamp = item.CreatedAt` (or explicit `--after` message timestamp when provided). This guarantees reliable corroboration even when lightweight `quick` models omit optional flags.

```go
type CorroborationSpec struct {
    Mode           string   `json:"mode"`                     // "monitor_refetch" | "connector_corroborate"
    MonitorID      string   `json:"monitorId,omitempty"`
    Connector      string   `json:"connector,omitempty"`      // e.g. "gmail"
    Account        string   `json:"account,omitempty"`        // e.g. "primary"
    SenderDomain   string   `json:"senderDomain,omitempty"`   // e.g. "rideshare.example.com"
    EntityTokens   []string `json:"entityTokens,omitempty"`   // e.g. ["12.53", "trip"]
    AfterTimestamp string   `json:"afterTimestamp,omitempty"` // RFC3339 of original failure message
    HoldWindow     string   `json:"holdWindow,omitempty"`     // e.g. "3m" (evaluated independent of legacy coalesce_window=0s!)
    ResolutionCues []string `json:"resolutionCues,omitempty"` // Default: ["receipt","paid","succeeded","payment received","confirmed","passed","recovered","resolved"]
}
```

**Execution Flow (`0` LLM Tokens):**
1. **Smart Hold Calculation:** If `now - AfterTimestamp < HoldWindow` (fresh failure, e.g., arrived 30s ago), set `AvailableForDispatchAt = AfterTimestamp + HoldWindow` (default `3m`) — even if a legacy Vault still has `coalesce_window: "0s"`. If the failure email is *already* older than `HoldWindow` (e.g., 4 hours old after a quiet window), dispatch proceeds immediately to the corroboration query with `0` extra delay!
2. **Bounded Read-Only Connector Query (`max=10`, `timeout=5s`):** Queries the originating connector (`hq-connector-protocol/v1` `search` verb, e.g., `from:<SenderDomain> after:<unix_epoch>`).
3. **Deterministic Resolution Matching:** Inspects returned `NormalizedItem` headers/subjects (no LLM call!):
   - Must have `Timestamp > AfterTimestamp`.
   - Must match `SenderDomain` (exact or subdomain alignment) and at least one `ResolutionCue` in subject/snippet.
   - If `EntityTokens` includes a monetary amount or numeric ID (e.g., `12.53`, `PR #42`), require matching token when present in the candidate subject/snippet, and verify no *newer* failure message arrived after the resolution message.
4. **Verdict:**
   - **Matched resolution:** Return `VerifyDowngrade` (`downgradedBy: "corroborated_resolved"`, `ResolutionRef: threadKey`, logged to `silent_ledger` — or `brief_rollup` if `attention.corroboration.resolved_tier: brief_rollup`). Zero false interrupt!
   - **No match:** Return `VerifyDeliver` with `ProvenanceNote: "Verified unresolved as of HH:MM"`.
   - **Connector timeout/error:** Fail-open to `VerifyDeliver` with `ProvenanceNote: "Corroboration skipped (connector unavailable)"`.

### 5.4 Late-Arrival Auto-Reconciliation (`outbox.Add`)

When a positive-polarity item (e.g., `"Rideshare receipt (USD 12.53)"` or `"CI Build #42 passed"`) arrives **after** a matching negative-polarity interrupt was already delivered (within `2h`, same `(Connector, Account)` and matching `EntityKey` or overlapping merchant + numeric/amount token):
- `outbox.Add()` automatically annotates the prior delivered interrupt with `ReconciledByID = newItem.ID`, `Status = StatusActedHandled`, `DismissReason = "auto_resolved_by_followup"`, and updates `CONSOLE.md` / Living Spaces on the next sync — clearing stale alerts without firing a noisy second interrupt!

### 5.5 Legacy `coalesce_window: "0s"` Advisory

`hq doctor` and `hq migrate` emit a non-destructive tip when an existing `<vault>/config.yaml` still sets `coalesce_window: "0s"` (the pre-RFC-0027 migrated value), recommending `"3m"` while `CorroborationSpec.HoldWindow` ensures failure corroboration already works out-of-the-box regardless.

---

## 6. Gap 4 — Inbound Family & External Channel Boundaries

To catch urgent plan changes originating outside Gmail/GChat (WhatsApp, Messenger, iMessage, SMS, Telegram family groups) **without widening the Lethal Trifecta**, RFC-0031 establishes a strict **3-Layer Boundary Contract**:

### 6.1 Architectural Rule (Non-Negotiable Ring-0 Invariant)

- **RFC-0023 (`hq-channel-bridge/v1` — `HandleInbound`) is strictly 1:1 Owner-Only.** Companion channel bridges MUST authenticate the sender as the single Vault owner (`owner_chat_id` / `owner_user_id`) before calling `/api/channels/inbound`. Third-party or family group messages MUST NEVER enter `HandleInbound` (`IngressOwnerInteractive`).
- **Headless browser scraping of Messenger/WhatsApp in core is explicitly out-of-scope** (fragile DOM, ToS/2FA challenges, high credential blast radius).

### 6.2 Three Sanctioned Ingestion Paths

| Path | Best For | Security Profile (`internal/authz`) | What It Can Do | Infra Needed |
|---|---|---|---|---|
| **Path A: Zero-Infra Owner Forward + Read-Only `.ics` Monitor** *(Ships in Phase 2)* | Any messaging app today (Messenger, iMessage, WhatsApp, WeChat) | `IngressOwnerInteractive` (owner forward) or schema-only `ics_feed` monitor | Owner forwards a 1-line update via Mobile Remote Control or RFC-0023 Owner DM (`hq goals update-leg` / `hq space note`), OR shares a read-only family `.ics` calendar URL monitored by `internal/monitors/` at `0` tokens | **Zero** new credentials or bots |
| **Path B: Opt-In Tainted Family/Group Connector Pack (`hq-connector-protocol/v1`)** *(Existing Seam — Documented & Hardened)* | Passive monitoring of a dedicated family Telegram group, WhatsApp Business/Cloud webhook, Matrix/Signal bridge, or local macOS Messages DB reader | `quarantined_reader` (`IngressUntrustedInbox`, `tainted: true`, `allow_tainted_create: false`, `participant_lock: true`) | Watches allowlisted room/chat IDs only; triages schedule/disruption cues into **tainted passive alerts** or proposes a Goal/Living Space update for owner 1-tap confirmation. **Cannot** call tools, arm monitors, approve proposals, or read `private/` | Opt-in local companion pack (SHA-256 pinned in `private/trust.json`) |
| **Path C: Trusted Partner Scoped Delegate (`hq-delegate-transport/v1`, RFC-0026)** *(Existing Seam — Family Starter Template)* | Spouse / co-organizer querying live itinerary status or pushing structured leg updates | `scoped_delegate` (`IngressUntrustedPeer`, `caller_groups.family`, Ring-0 egress redaction) | Answers read-only trip queries from curated `Public Status` / Living Space snapshot; submits **CaMeL parameterized `queue_only` nudges** (e.g., `leg_id` enum + `new_time` regex) staged cleanly for owner review | RFC-0026 `private/delegates/policy.json` + companion transport |

**Concrete Phase 2 Addition for Path A (Zero-Infra Ergonomics):**
- Add `hq goals update-leg <goal-slug> <item-id> [--status=...] [--window-start=...] [--note=...]` and `hq space note <space-slug> --text="..."` so forwarding a family message into Front Desk updates the Goal, refreshes any linked Living Space, and retargets/re-arms linked live monitors in **1 deterministic command**.
- Add a clean anonymized starter example in `docs/` showing how to configure a family `caller_groups` policy under RFC-0026 without exposing any private Vault memory.

### 6.3 RSI v2 Synergy (`RFC-0029` Integration — Q6)

While Ring-0/Ring-1 primitives (Active TZ, `internal/monitors/`, Phase B `VerifyFunc`) must be built deterministically in Go rather than learned via prompt RSI, **RSI v2** ([RFC-0029](0029-rsi-v2-trace-eval-and-heldout-ratchet.md)) pairs cleanly with RFC-0031 to learn **policy and tuning** over time:
1. **Noise-to-Corroboration Tuning (`FrictionProactiveNoise`):** Repeated user dismissals on monitor alerts or failure interrupts emit non-destructive tuning advisories (e.g., raising `MinETADeltaMinutes`, extending `HoldWindow`, or adding a `ResolutionCue`).
2. **Manual Status-Check Compilation (`Stream 2` / `compile_script_or_route`):** When RSI v2 detects recurring multi-step manual status checks in chat across `>= 2` conversations (e.g., repeatedly checking a status page or flight/rail URL), its Morning Brief compilation card recommends promoting the check into a `0`-token `hq monitor`.

---

## 7. Engineering Invariants & File Layout

| Invariant | Verification |
|---|---|
| **Zero External Go Dependencies (`H4-A`)** | `go.mod` remains 100% pure Go stdlib (`net/http`, `net`, `time`, `encoding/json`, `crypto/sha256`). |
| **Strict Lock Hierarchy** | Add `RankMonitors Rank = 31` in `internal/store/store.go` (`RankWatcher(30) < RankMonitors(31) < RankPulse(32)`). Enforce snapshot-then-commit in `internal/monitors/` and Phase B unlocked verification in `internal/outbox/dispatcher.go`. |
| **Monolith LOC Ceilings (`H5-F`)** | New files only: `internal/kernel/timezone.go`, `internal/monitors/{monitors,http_json,http_text,ics,provider,ssrf,diff,monitors_test}.go`, `internal/outbox/corroborate.go`, `cmd/hq/monitors_cli.go`. Touch `kernel.go` / `main.go` by `< 15` lines total (dispatch wiring only). |
| **Ring-0 Non-Interference (`NI-1`–`NI-4`)** | `internal/authz` blocks `hq monitor add|edit|resume` in `quarantined_reader` & `scoped_delegate`; SSRF firewall blocks private/metadata IPs post-DNS; `Critical` quiet-hour bypass on monitors requires explicit owner-configured `CriticalStatuses`. |
| **PII & Vault Hygiene (`P1`–`P6`)** | All tests and docs use synthetic domains (`@example.com`, `127.0.0.1` loopback httptest servers under explicit test hook) and zero real routes/identities. |

---

## 8. Phased Implementation Plan (Phase 2 — Blocked Pending Review)

- **Phase 2A — Active Timezone Resolution (`CUJ-50`):**
  - `internal/kernel/timezone.go` (`ResolveActiveTimezone`, `hq tz set|clear|status`), `tz_mode: active | fixed` & `CRON_TZ=ACTIVE` in `gatekeeper.go`, `LastLocalDate` + `12h` daily anti-double-fire guard, `InQuietHoursAt` active-TZ wiring in `internal/outbox/`, `templates/triggers.yaml` defaults, and Living Space ↔ Goal TZ linkage.
- **Phase 2B — Ephemeral, Goal-Bound Live Monitors (`CUJ-51`):**
  - `store.RankMonitors = 31`, `internal/monitors/` (`http_json`, `http_text`, `ics_feed`, `hq-monitor-provider/v1`, SSRF post-DNS firewall, asymmetric `monitors.allowed_hosts`, `DeltaPolicy`, `freshness_wins` multi-source conflict resolver, baseline commit-after-delivery), `60s` `monitorTicker` in `cmd/hq/ops_cli.go`, `hq monitor` / `hq goals watch` CLIs, `internal/authz` taint lockdown, and Living Space `hq_builtin: "monitors"` binding.
- **Phase 2C — Verify-Before-Interrupt Corroboration (`CUJ-52`):**
  - `internal/outbox/corroborate.go` + Phase B `VerifyFunc` hook in `DispatchPendingInterrupts`, Mode A monitor re-verify, Mode B 0-token connector corroboration query (with Go auto-inference in `outbox.Add()`, explicit CLI overrides, independent `HoldWindow`, `silent_ledger` default, & fail-open provenance), `outbox.Add()` late-arrival auto-reconciliation, `inbox-triage` skill corroboration hints, and `hq doctor` / `hq migrate` advisories for legacy `coalesce_window: "0s"` and `CRON_TZ=ACTIVE`.
- **Phase 2D — Zero-Infra Family/External Intake, RSI Synergy & Docs (`CUJ-53`):**
  - `hq goals update-leg` + `hq space note` fast-paths, `ics_feed` monitor E2E coverage, RFC-0026 family delegate starter guide, RSI v2 synergy hooks (`FrictionProactiveNoise` → corroboration/threshold tuning advisories & recurring status-check → `hq monitor` compile suggestion), and full `./bin/hq ci pre-push` + `PRD-0003` CUJ index update.

---

## 9. Locked `/grill-me` Design Decisions (Q1–Q6)

| # | Decision | Locked Choice |
|---|---|---|
| **Q1** | Scope & Phasing | Ship all 4 pillars (`2A Active TZ` → `2B Live Monitors` → `2C Verify-Before-Interrupt` → `2D Zero-Infra Family Intake + RSI Synergy`) in one cohesive RFC-0031 build. |
| **Q2** | Active Timezone Scope | All three user-aligned OS schedules (`gmail-inbox-watch`, `morning-brief`, `sleep-consolidate`) + `quiet_hours` follow Active TZ; fixed-clock jobs (e.g. US market watches) stay `tz_mode: fixed`. |
| **Q3** | Live Monitor Host Gate | Allow any public `https://` host directly when armed by an untainted owner-interactive session (post-DNS SSRF private-IP firewall & tainted-session hard block always on; `monitors.allowed_hosts` remains optional lockdown & fail-closed for `unattended_auto`). |
| **Q4** | Built-In Monitor Kinds | Ship `http_json` + `http_text` (RE2 regex on stripped HTML/text) + `ics_feed` + SHA-256-pinned `hq-monitor-provider/v1` companion pack seam. |
| **Q5** | Mode B Corroboration | Auto-infer `CorroborationSpec` in Go on failure/payment/CI interrupts from `inbox-triage` (with explicit CLI flag overrides) and downgrade confirmed false alarms to `silent_ledger` by default. |
| **Q6** | Family Channels & RSI | Phase 2D ships Zero-Infra Owner Forward (`hq goals update-leg` / `hq space note`) + `.ics` monitor + RFC-0026 family delegate starter guide + RSI v2 synergy hooks; keep Telegram/WhatsApp connectors as separate opt-in private packs. |
