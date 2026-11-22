# RFC-0027: Smarter Proactive Actions — Looking Ahead, 1-Tap Decisions & Interactive Spaces

- **Status:** Proposed (Phase 1 — Awaiting User Review)
- **Created:** 2026-11-21 · **Updated:** 2026-11-22
- **Supersedes:** [RFC-0020 (Ambient Synthesis & Mobile Push)](./0020-proactive-ambient-synthesis-and-mobile-push.md)
- **Builds On:** [RFC-0014 (Proactive Engine v2)](./0014-proactive-engine-and-living-context.md), [RFC-0023 (Channel Bridges)](./0023-front-desk-channel-bridge.md), [RFC-0025 (Workspaces)](./0025-pluggable-workspace-providers.md), [RFC-0026 (Scoped Delegates)](./0026-declarative-scoped-delegates.md)
- **Research:** [22 — Meta Muse Proactive Actions & Living Spaces Audit](../research/22_muse_proactive_actions_and_living_artifacts_audit.md)

---

## 1. One-Page Summary (TL;DR)

Today, `agy-hq` has strong background plumbing, **but it mostly reacts to single new unread messages and replies with plain text.** Inspired by what works best in Meta Muse (`https://muse.ai/`)—and adapted for both **engineering work** and **personal life**—this RFC proposes **four practical improvements**:

| Problem Today | What We Propose |
| :--- | :--- |
| **1. Only reacts to *new* messages; misses *time passing* & conflicts** | **Look-Ahead Check (`hq pulse`) + 3-State Checklists:** Every 15 minutes (at **0 LLM tokens** when nothing changed), check upcoming meetings/1:1s, approaching deadlines, tasks due *while* you'll be away, PRs/workspaces needing your action, and overlapping schedules. Track multi-step goals as **`confirmed` / `open` / `skipped`** (e.g., `11 of 16 ready`, never re-nagging on `skipped` items). |
| **2. Alerts are passive text walls, and timed-out approvals vanish** | **1-Tap Decision Buttons + Burst Grouping + `restage`:** Group back-to-back alerts from the same sender within **3 minutes** into one message. Attach **2–4 clear action buttons** (`[Mark Handled]`, `[Run Fix/Check]`, `[Park Until...]`, `[Re-open Timed-Out Card]`) that work in chat, `:4242`, and mobile bridges—and always tell you if a staged approval timed out unsent. |
| **3. Complex plans & war-rooms are dumped as long Markdown text** | **Living Spaces (Local Interactive Micro-Apps):** For multi-part plans (release war-rooms, trade-off tables, thesis trackers, itineraries), compile a structured JSON file (`space.json`) into a local HTML page with countdowns, progress bars, **role/audience tabs** (`Exec / Eng` or `Adult / Kids`), **0-token live CLI refresh**, and **instant client-side language toggle** (when configured in your Vault). |
| **4. Console leads with token/cron plumbing; hard to defer or mute noise** | **Human-First Console + "Park Until" + Noise Tuning:** Put pending decisions, countdowns, and Spaces at the top of `:4242` and `hq_console.md`; move token/cron/health stats into a collapsible section (showing *"Next run: Tomorrow 7:30 AM"* instead of raw cron syntax); support `hq goals park --until=...` and `hq cancel`; and automatically dial down noisy triggers during nightly consolidation. |

---

## 2. What It Looks Like in Practice (Before vs. After)

### Example A — Engineering Work (1:1 Prep & Release Cutover)
- **Today:** You have a 1:1 with a teammate in 25 minutes while a PR in `headquarter-foo` has reviewer comments (`NeedsOwner: 1`) and a staged reply from yesterday expired unsent. Nothing nudges you unless a brand-new email arrives right now, and your release checklist is static prose.
- **With RFC-0027:**
  1. **25m before the 1:1**, `hq pulse` spots the calendar invite + open workspace item + expired draft and sends one concise prep card with buttons: `[Re-open Draft]` · `[Open PR Workspace]` · `[Dismiss]`.
  2. Your **Release War-Room Space** refreshes PR/CI counts in **0 LLM tokens** and switches cleanly between `Stakeholder TL;DR`, `PRs & Gates (12/15)`, and `Rollback Runbook` tabs.

### Example B — Personal / Family Logistics (Away Window & Multi-Item Plan)
- **Today:** Two security emails from the same airline arrive 15 minutes apart and wake Front Desk twice with *"Let me know if you want me to check."* Meanwhile, a form due `Dec 2` goes unnoticed until `Dec 1`—while you're away `Nov 22 – Dec 1`.
- **With RFC-0027:**
  1. Related alerts within `3m` merge into one card (and follow-ups within `30m` link back as `↪ Update #2`), with 1-tap buttons: `[My Action — Dismiss]` · `[Check Auth Emails]`.
  2. Because the trip Goal marks `Nov 22 – Dec 1` as an **unavailable window (`blocksAvailability: true`)**, `hq pulse` flags the `Dec 2` form **before departure**, and your local **Trip Space** shows a live countdown, `11 of 16 booked` progress bar, `Skipped` items (never re-suggested), and instant `English / 中文` + `Adult / Kids` toggles.

---

## 3. Framework (`agy-hq` Open Source) vs. Personal Vault (`local/`)

We strictly separate **general-purpose OS mechanisms** (tracked in `agy-hq`) from **your personal preferences** (stored only in git-ignored `<vault>/`):

| Area | Open-Source Framework (`cmd/`, `internal/`, `templates/config.yaml`) | Personal Vault (`<vault>/config.yaml`, `USER.md`, `<vault>/skills/`) |
| :--- | :--- | :--- |
| **Languages** | Locale-neutral dictionary engine. **Default:** `["en"]` (language switch hidden when single-language). | Set `living_spaces.default_locales: ["en", "zh"]` in `<vault>/config.yaml` when you want bilingual toggles by default. |
| **View Tabs** | Generic `perspectives` (`0..N` tabs) and `groups` (`0..N` filters), hidden when unused. | Choose tabs per Space/skill (`Exec / Eng / On-Call`, `Bull / Kill Criteria`, or `In-Depth / Kids`). |
| **Space Layouts** | **5 reusable UI building blocks:** KPI banner, collapsible timeline, comparison table, readiness checklist, and notes/callouts. | Domain skills and actual Spaces live in `<vault>/skills/` and `<vault>/living_spaces/`. |
| **Goals & Pulse** | Generic time windows, buffer/overlap checks, `blocksAvailability` deadline shifts, workspace `NeedsOwner` checks, and `confirmed / open / skipped` status. | Your actual goals, custom lead times (`pulse.*`), and weekday work-focus vs. weekend personal `attention.scope_rules`. |

```yaml
# Optional settings in <vault>/config.yaml (showing neutral framework defaults):
attention:
  coalesce_window: "3m"              # Hold non-critical interrupts 3m to merge bursts (Critical = immediate)
  incident_correlation_window: "30m" # Link related follow-ups within 30m
  # scope_rules:                     # Optional: quiet non-critical personal alerts during weekday coding blocks
  #   - scopes: ["personal"]
  #     schedule: "weekdays 09:00-17:30 America/Los_Angeles"
  #     downgrade_non_critical_interrupt_to: "brief_rollup"

pulse:
  pre_meeting_lead: "30m"
  countdown_horizons: ["168h", "72h", "24h"]
  cutoff_lead: "6h"

living_spaces:
  default_locales: ["en"]                 # Override to ["en", "zh"] in personal Vault
  default_surfaces: ["webui", "artifact"] # Render on :4242 and/or Antigravity side pane
```

---

## 4. Design of the Four Improvements

### 4.1 Look-Ahead Check (`hq pulse`) & 3-State Goal Checklists (`Phase 2A`)

1. **Goal Upgrades (`internal/kernel/goals_readiness.go`):**
   - Add optional `windowStartAt`, `windowEndAt`, `timezone`, `blocksAvailability` (for travel, offsites, or release freezes), `requiredCoverageTag` (verifies every day/slot in a window has a confirmed item with that tag), and `parkedUntil` (`YYYY-MM-DD` or `"after:<goal-slug>"`).
   - Add `checklist` items with **3 states**: `confirmed`, `open`, and `skipped` (with `skipReason`, acting as a "do not re-suggest" tombstone), plus optional `cutoffAt` (cancellation/freeze deadline) and `mustCompleteBeforeWindow`.
2. **Ten 0-Token Checks in `hq pulse precheck` (`internal/pulse/`):**
   - Runs every 15 minutes inside `hq daemon`. Consumes **0 LLM tokens** when nothing triggers, and emits direct Go cards for simple reminders (spawning an LLM worker only when summarizing a pre-meeting brief or drafting a follow-up reply):
     1. **Pre-Meeting / 1:1 Prep:** Upcoming event (`10–35m`) matched against contact dossiers, open `WaitingOn` threads, unsent drafts, active `Tasks`/`Workspaces`, and peer `Delegates`.
     2. **Horizon Countdowns:** `7d` (rolled into Morning Brief), `72h` & `24h` (interrupt if open blockers remain, with *"ignore if already handled"*), and `6h` before any `cutoffAt`.
     3. **Unavailable-Window Ripple:** Flags deadlines or `mustCompleteBeforeWindow` items that collide with an upcoming `blocksAvailability` Goal (`<= 72h` out).
     4. **Transition & Settlement Pings:** Arrival/start-of-window reminders (in target `timezone`) and follow-up timers for offline handoffs invisible to APIs.
     5. **Schedule Overlap & Coverage Gaps:** Flags confirmed slots with `< 45m` transit buffer/overlap or uncovered dates for `requiredCoverageTag`.
     6. **Expiring & Timed-Out Approvals:** Warns `< 2h` before `ExpiresAt`, and **always** notifies when a proposal expires unclicked (fixing today's `> 12h` Morning Brief blind spot).
     7. **Overdue `WaitingOn` SLAs:** Respects `parkedUntil` holds and notes if an earlier staged reply on that thread expired unsent.
     8. **Parked Wakeup:** Resurfaces `"Park until..."` items into Morning Brief once their target date or `"after:<goal>"` window passes.
     9. **Workspace & Review Pulse (Work-Native):** Nudges when an owned workspace flips to `NeedsOwner > 0` (review comments / CI blocker) or drifts into merge conflict ahead of a `cutoffAt`.
     10. **Stale Unsent Drafts (from `RFC-0020`):** Surfaces standalone unsent drafts older than `4h` into the non-interruptive `Ideas` feed.

---

### 4.2 1-Tap Decision Buttons & Timeout Recovery (`Phase 2B`)

1. **Structured `DecisionOption` Buttons (`internal/outbox/decisions.go`):**
   - Each Outbox alert can attach up to **4 buttons** drawn from a **fixed safe action list** (never raw shell commands):
     - `approve_proposal` / `reject_proposal` / `restage_proposal`
     - `park_until` (e.g., `24h`, `2026-12-02`, or `after:<goal-slug>`)
     - `dismiss_handled` (*"Good catch, I already did this"*) / `dismiss_noisy` (*"Don't alert me for things like this"*)
     - `skip_checklist` / `promote_goal` / `spawn_investigation` (runs a registered skill in an isolated background worker)
   - Clicking a button in `[HQ] Front Desk` (`ask_question`), `:4242` WebUI, or a mobile Channel Bridge ([RFC-0023](./0023-front-desk-channel-bridge.md)) runs `hq outbox act <item-id> --option=<opt-id>` in **0 LLM tokens**. *(Stateless zero-body push notifications like `ntfy`/Shortcuts also plug directly into `RFC-0023` as `notify_only` bridges rather than a separate push subsystem.)*
2. **1-Command Timeout Recovery (`hq approvals restage <id>`):**
   - Re-creates a fresh `24h` approval card from an expired proposal in **0 LLM tokens** (checking first whether the email/chat thread received new replies since it was originally drafted).
   - If a counterparty follows up on a thread where an earlier offer expired unsent, `hq watch` automatically notes *"Prior staged reply expired unsent on YYYY-MM-DD"* so the agent has accurate context.

---

### 4.3 Living Spaces — Composable Local Micro-Apps (`Phase 2C`)

- **Canonical Data + Deterministic HTML (`<vault>/living_spaces/<slug>/space.json` $\rightarrow$ `index.html`):**
  - Instead of asking an LLM to rewrite 500 lines of HTML on every update, agents update structured JSON (`space.json`) and `hq space build` compiles a clean, self-contained `< 45 KB` HTML page in **0 LLM tokens**.
  - **5 Composable Building Blocks:** `kpi_strip` (live JS countdown + `11/16` readiness bar), `timeline_accordion`, `comparison_table`, `checklist_ledger` (auto-synced when Goal checklist items change), and `callout_notes`.
  - **0-Token Live Work Data (`data_sources`):** Work Spaces (Release War-Rooms, PR Cockpits, Cost/Token Views) can bind tables/badges to built-in `hq` state (`workspaces`, `goals`, `delegates`, `obs`) or `trust.json`-verified `argv` scripts so metrics refresh without LLM calls.
  - **0-Token Client-Side Toggles:** Instant tab switching for `perspectives` (`Exec / Eng`, `Adult / Kids`), `groups`, and `locales` (`English / 中文` via `<script id="hq-i18n" type="application/json">` when multiple locales are configured).
  - **Where You View It:** Both inside Antigravity's native side pane (`generative_ui` HTML artifact matching light/dark theme) and at `http://127.0.0.1:4242/living-spaces/<slug>`.

---

### 4.4 Calm Console Layout, `hq cancel` & Noise Learning (`Phase 2D`)

1. **Re-Ordered Console (`hq_console.md` & `:4242`):**
   - **Top:** Action Required (Approvals, Decision Cards, `< 48h` Countdowns & Workspace Blockers).
   - **Middle:** Active Living Spaces + Active Goals (`confirmed / open / skipped`) + Active Tasks.
   - **Bottom (Collapsible Inspector):** Token burn, prefix cache %, watermarks, `hq doctor`, and Schedules with **human-readable next-run times** (`Tomorrow 7:30 AM · in 18h`).
2. **Instant Reversal (`hq cancel <target>`):** Cancels queued/running background events, follow-up timers, and pending proposals in one step when you change your mind mid-flight, logging the reversal in Activity.
3. **Automatic Noise Reduction (`hq rsi mine`):** Tracks `dismiss_handled` (useful) vs. `dismiss_noisy` and `expired` proposals per skill; when a trigger has $\ge 40\%$ noisy dismissals during `/sleep-consolidate`, `hq rsi` proposes a Vault-local rule (`<vault>/memory/rsi/rules/`) to downgrade it from `interrupt` to `brief_rollup` or `ideas_feed`.

---

## 5. Safety & Engineering Guardrails

1. **100% Local Privacy:** Living Spaces never leave your machine (`<vault>/living_spaces/` + local `:4242` loopback + local Antigravity artifact folder).
2. **Quarantine-Safe Buttons & Scripts:** Untrusted (`tainted`) email/chat triage workers can only attach fixed-enum buttons referencing their own staged offers/items (never shell commands); `:4242/living-spaces/<slug>` runs inside `<iframe sandbox="allow-scripts">` (no `allow-same-origin`, `default-src 'none'` CSP) so generated UI cannot touch `:4242` APIs or CSRF tokens; and custom `data_sources` require `trust.json` SHA-256 binary pins.
3. **Deadlock-Free Lock Order & Zero External Deps:** Adds `RankPulse = 32` and leaf `RankLivingSpaces = 77` in `internal/store`, keeps `go.mod` at `0` external dependencies (`Gate H4-A`), and places new code in modular packages (`internal/pulse`, `internal/living`) so monolith files stay well under `Gate H5` LOC limits.

---

## 6. Phased Implementation Plan (Paused Until Approved)

| Phase | Scope | Key Verification |
| :--- | :--- | :--- |
| **Phase 2A (`P0`)** | `Goal` 3-state checklists + windows + `parkedUntil`; `internal/pulse` (`D1`–`D10` checks); `3m` Outbox burst coalescing + `scope_rules`. | **`CUJ-45`:** Pre-1:1 prep, unavailable-window deadline shift, workspace `NeedsOwner` nudge, & `3m` burst merge (`0` tokens when idle). |
| **Phase 2B (`P0`)** | `DecisionOption` 1-tap buttons (`hq outbox act` across chat, `:4242`, & Channel Bridges) + `hq approvals restage` + expired-proposal alerts. | **`CUJ-46`:** Taint safety on buttons, 0-token button execution, `> 12h` expired proposal catch, & 1-tap `restage`. |
| **Phase 2C (`P1`)** | `internal/living` (`space.json` $\rightarrow$ HTML compiler, 5 block types, `hq-i18n`, `perspectives`, 0-token `data_sources`, `:4242` sandbox & `generative_ui`). | **`CUJ-47`:** Single- vs. multi-locale build, 0-token workspace/Goal sync, & strict iframe CSP isolation. |
| **Phase 2D (`P1/P2`)** | Re-ordered human-first Console + collapsible Ops Inspector (`NextRunAt`), `hq cancel`, and `dismiss_noisy` RSI mining. | Unit + E2E tests for `parkedUntil`, `NextRunAt`, `hq cancel`, and Vault-local RSI noise tuning. |

> **Current Status:** `RFC-0027` is **Proposed (Awaiting Review)**; [RFC-0020](./0020-proactive-ambient-synthesis-and-mobile-push.md) is marked `Superseded by RFC-0027`. No implementation code will be written until you approve.
