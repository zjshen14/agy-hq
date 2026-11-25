# RFC-0030: Token Observability Calibration, Unified 4-Tab Console UX & Dual-Artifact Lifecycle Sync

- **Status:** Proposed (Phase 1 Research & RFC — awaiting review before Phase 2 code)
- **Created:** 2026-11-25
- **Builds On:** [RFC-0007 (Front Desk Init Hardening)](./0007-front-desk-initialization-hardening.md), [RFC-0009 (Scalable Task Board)](./0009-task-board-redesign.md), [RFC-0012 (Token Observability)](./0012-token-observability.md), [RFC-0027 (Living Spaces & Calm Console)](./0027-muse-proactive-actions.md)
- **Research:** [25 — Forensic Audit of Token Math, Console UX & Artifact Lifecycle](../research/25_console_ux_and_token_observability_audit.md)

---

## 1. One-Page Summary (TL;DR)

Auditing `639` real conversations (`2.93B` gross tokens, `88` compactions) against `internal/obs/`, `internal/webui/`, and Front Desk lifecycle hooks surfaced silent token/QBU math drift, broken `:4242` UI actions, duplicate console prose, and orphaned `hq_console_live.html` files on shift handovers. This RFC proposes **four tightly scoped fixes**:

| Problem Today | What We Propose |
| :--- | :--- |
| **1. Token, QBU, context ceiling & anomaly math have drifted from live reality (`internal/obs/`)** | **Per-Turn Model Pricing + `512k` Host Ceiling + `24h` Calibrated Anomalies:** Track `Model` **per turn** from journals (fixing `54.0%` multi-model misattribution); price QBU/USD strictly from the **executed model** (`1 QBU = $0.01`, fixing `28.9%` QBU inflation on `[Deep]` tasks switched to `Hydrogen`); update host auto-compaction ceiling from hardcoded `256k` to configurable **`512k`** while keeping `120k / 160k / 180k` as Front Desk cold-cache hygiene markers; unify midnight boundaries on `EndedAt`; rank disjoint root trees; and fix `A2`/`A3`/`A4` (`24h` live window instead of `306` permanent zombie alerts). |
| **2. `:4242` is a 14-section scroll with broken JS buttons, and neither UI has a Token Usage tab** | **Unified 4-Tab Console (`Needs Me` · `Work` · `Tokens` · `Ops`):** Give both `hq_console_live.html` (Mobile / Artifact Pane) and `console.html` (`:4242` / Sidecar) the same 4-tab layout, adding a dedicated **`Tokens` tab** with per-model, disjoint per-task tree, prefix-cache savings / cold-spike diagnostics, and pure-SVG `7d` daily + `24h` hourly trend charts—while fixing `console.html`'s `ReferenceError` button bugs, `x-sidecar-token` auth, and partial `5s` polling. |
| **3. `CONSOLE.md` / `hq_console.md` duplicates verbose tables and drifts from HTML views** | **One Snapshot Producer, Three Clear Roles:** Build one `ConsoleSnapshot` in Go (`0` LLM tokens) feeding: (1) **`CONSOLE.md` / `hq_console.md`** (`<= 120` lines executive digest & link hub, collapsing `19/19 PASS` Doctor to 1 line), (2) **`hq_console_live.html`** (`< 45 KB` offline/Remote-Control 4-tab micro-app with `Copy` command chips), and (3) **`:4242` / Sidecar** (`5s` live desktop control plane with `1`-click `POST` actions). |
| **4. `hq_console_live.html` is never UI-pinned on Turn 1 and is orphaned on shift handover** | **Zero-Token Dual Pin (`{{HQ_SYNC}}`) & Co-Equal Lifecycle:** Track `LiveConsolePinned` alongside `ConsolePinned`; hydrate `write_file(..., Content="{{HQ_SYNC}}")` inside `PreToolUse` (`0` `read_file` calls, `~60` output tokens instead of `~9,500`!); clean up `hq_console_live.html` in `removeConsoleArtifacts`; verify both in `hq doctor`; and sync `claim-desk`, `hq console`, and `AGENTS.md`. |

---

## 2. What It Looks Like in Practice (Before vs. After)

### Example A — Multi-Model Task Attribution, Context Gauge & Cold-Wakeup Alerts
- **Today:** You write an RFC on `gemini-4-carbon-high` (`1.5M` uncached) and implement Phase 2 in the same thread on `gemini-4-neon-high` (`3.5M` uncached), or switch a `[Deep]` task to `gemini-4-hydrogen-xhigh` (`54.5M` uncached). `hq obs` attributes **100% of earlier turns** to the latest model, bills `Hydrogen` at `5×` `deep` QBU, clamps a `360k` context gauge at `360k / 256k (100%)`, and buries live Front Desk cold-cache spikes under `306` weeks-old `A3` subagent alerts (because `A3` counted raw JSONL tool lines instead of `PlannerSteps`, while `A2` was masked by warm intra-turn steps).
- **With RFC-0030:** Every turn carries its exact `Model` and `ModelTier`. Context gauges show **`360k / 512k (70.3%)`** for tasks, while `[HQ] Front Desk` shows a dual-marker bar (`57k / 512k` with `120k Warn` & `180k Crit` cold-wakeup ticks). Live anomalies show only active `24h` issues—e.g., *"`A2`: Front Desk woke cold 4 times (>50k input, <20% CHR), wasting 640k uncached tokens — run `/compact`."*

### Example B — Sidecar (`:4242`) & Mobile Remote Control (`hq_console_live.html`)
- **Today:** Opening `sidecar://agy-hq-console/` shows a 14-section scroll; clicking `[Re-open 24h Card]` or a Quick Decision button throws `ReferenceError: id is not defined`; and after 5 seconds, `refreshState()` erases task token badges and PII warnings while leaving token KPIs frozen. On mobile Remote Control, `hq_console_live.html` has no Token Usage tab at all.
- **With RFC-0030:** Both surfaces open on **`🔔 Needs Me (N)`** (or remember your active tab) with 4 clean tabs: **`Needs Me`**, **`Work`**, **`Tokens`**, and **`Ops`**. Tapping **`Tokens`** shows `Today / 7d / 30d / All` burn by model family, disjoint task trees (expandable to child subagents), prefix-cache `$` saved, and `7d` + `24h` SVG trend bars. Every `:4242` button works in 1 click, and `5s` polling updates all 4 tabs non-destructively.

### Example C — Turn-1 Front Desk Pinning & Shift Handover
- **Today:** On Turn 1, `[HQ] Front Desk` reads `CONSOLE.md` (`4.5 KB`) and regenerates all `4.5 KB` in `write_file(hq_console.md)`—leaving `hq_console_live.html` (`35 KB`) unpinned in Antigravity's artifact tray. Later, `hq release-desk` or `hq claim-desk` deletes `hq_console.md` from the old thread but leaves a frozen `hq_console_live.html` behind forever.
- **With RFC-0030:** On Turn 1, Front Desk calls `write_file` in parallel on `hq_console.md` and `hq_console_live.html` with `Content="{{HQ_SYNC}}"` (`0` `read_file` calls, `~60` output tokens total); `PreToolUse` injects the exact ground-truth bytes on disk. On handover/release/adopt, `removeConsoleArtifacts` retires both artifacts cleanly.

---

## 3. Pillar 1: Calibrated Token, Context, DAG & Anomaly Math (`internal/obs/`)

Full derivations and empirical proofs (`M-1`–`M-9`) are in [Research 25 §2](../research/25_console_ux_and_token_observability_audit.md). We preserve exact step identities ($P_s = I_s + C_s$, $U_s = I_s + O_s$, $G_s = I_s + C_s + O_s$, pooled $\text{CHR} = \frac{\sum C_s}{\sum (I_s + C_s)}$) and fix nine calculations:

1. **Dual Context Thresholds (`M-1` — `512k` Host Ceiling vs. Front Desk Hygiene):**
   - Add `ContextCeilingTokens int64` to `BudgetConfig` (`default: 512,000`, matching Antigravity's post-`2026-11-16` auto-compaction ceiling) and replace all 7 hardcoded `256k` strings/divisors.
   - Keep `WarnContextTokens = 120,000`, `NudgeContextTokens = 160,000`, and `CritContextTokens = 180,000` explicitly scoped to **`[HQ] Front Desk` Cold-Cache Hygiene** (preventing `500k` cold-wakeup spikes between idle gaps).
   - Track `PostCompactContextTokens` (peak since latest `CHECKPOINT`) alongside all-time `PeakContextTokens` so post-`/compact` health reflects current shift state.
2. **Per-Turn Model Mapping & Strict Model-Derived Pricing (`M-2`, `M-3`, `M-4`):**
   - Add `Model`, `ModelTier`, `ColdStartInputTokens`, and `FirstStepCHR` to `TurnMetrics`.
   - Sort journal files numerically by `(date, execNum)` (fixing `-10.json < -2.json`), map journal `(stepStartIdx, stepCount) -> modelName` onto turns, and record `ModelSource` (`"journal" | "spawn_edge" | "task" | "fallback"`) so real journal models always overwrite synthetic fallbacks.
   - Split **`RequestedTier`** (role/task intent) from **`ModelTier(model)`** (computed strictly from `model`) and anchor **`1 QBU = $0.01` (`QuotaUnits = EstCostUSD * 100.0`)** with configurable overrides in `budget.json`:

   | `ModelTier` | Model Matchers (3-Family + Legacy + Claude) | Input (`$/1M`) | Cache Read (`$/1M`) | Output (`$/1M`) |
   | :--- | :--- | :---: | :---: | :---: |
   | `quick` | `gemini-4-hydrogen-low`, `*-low`, `flash-lite`, `flash_lite` | `$0.10` | `$0.025` | `$0.40` |
   | `routine` | `gemini-4-hydrogen-medium`, `flash` | `$0.25` | `$0.0625` | `$1.50` |
   | `standard` | `gemini-4-neon-{medium,high}`, `gemini-4-hydrogen-{high,xhigh,tiered*}`, `gemini-3.8-flash-*` | `$0.50` | `$0.125` | `$3.00` |
   | `deep` (Gemini) | `gemini-4-carbon-*`, `gemini-4-argon-*`, `gemini-pro-*`, `pro` | `$1.25` | `$0.3125` | `$10.00` |
   | `deep` (Claude) | `claude-sonnet-*` / `claude-opus-*` | `$3.00` / `$5.00` | `$0.30` / `$0.50` | `$15.00` / `$25.00` |

   - `TodayDeepUncachedTokens` sums turns where **`ModelTier(turn.Model) == "deep"`** (never role labels), keeping `DailyUncachedBudget` (`50M`) and `DailyDeepUncachedBudget` (`30M`) accurate.
3. **Canonical `EndedAt` Boundary & Phantom-Turn Filtering (`M-5`, `M-8`):**
   - Use `turnEffectiveTime(t)` (`EndedAt`, fallback `StartedAt`) everywhere (`turnMatchesCutoff`, daily/hourly buckets, `DashboardSummary`, and `SyncDailyBudgetCircuitBreaker`).
   - Omit trailing `0`-planner-step / `0`-token turns from rollups while preserving `CompactedPendingNextTurn` if a standalone `CHECKPOINT` sits at EOF.
   - Include `stat(journal/<today>)` and `stat(obs/index.json)` in `summary_cache.json` and `webui.go`'s state fingerprint so non-Front-Desk task turns and `/compact` events refresh immediately.
4. **Disjoint Root-Tree Ranking & Calibrated `24h` Anomalies (`M-6`, `M-7`, `M-9`):**
   - Link `ActiveTask.SupersededConversationIDs` into their owning task in `dag.go`, add `DescendantCount`, and default `hq obs top --tree` (plus UI task tables) to **disjoint roots** (`--all-nodes` opt-in for inner subtrees).
   - Scope live Dashboard/Doctor anomalies to a **`24h` active window** (`hq obs anomalies --since=all` for historical audits), fix **`A3`** to check `t.PlannerSteps >= cfg.MaxTurnSteps` (`60`), fix **`A2`** to trigger on $\ge 3$ turn-opening cold spikes (`ColdStartInputTokens >= 50,000` & `FirstStepCHR < 0.20` in `24h`, reporting exact wasted tokens), and fix **`A4`** to flag `auto`/`quick` runs on `deep` models or $\le 5$-step lookup-only `deep` subagents with `0` file edits.

---

## 4. Pillar 2: Unified 4-Tab Console UX & Dedicated `Tokens` Tab (`internal/webui/`, `internal/living/`)

Both `hq_console_live.html` (`/living-spaces/hq-console`) and `console.html` (`sidecar://agy-hq-console/` & `:4242`) share **one sticky 4-KPI banner + 4 top-level tabs** (setting `HideAllPerspective: true` on `hq-console` so `"All Views"` no longer dumps all tabs together):

1. **`🔔 Needs Me (N)`:** Pending Approvals (preserving `ExpiresAt` + `RiskFlags` PII badges), Timed-Out Unsent Approvals (`Re-open 24h` / `Dismiss`), Quick Decisions, Active Goals (`confirmed / open / skipped`), Overdue SLAs, and Proactive Ideas.
2. **`🛠️ Work (M)`:** Tiered Live Task Board (`Self` & `Tree` token badge, workspace state, `conversation://` link, `Complete`/`Archive`), Active Workspaces (`NeedsOwner` badges), Living Spaces index, and Scheduled Console Spaces.
3. **`🪙 Tokens` (New Dedicated Tab on Both Surfaces):**
   - **Window Toggle:** `Today` · `7d` · `30d` · `All` (`0ms` client-side switch on `:4242`; `Today` vs `7d` group pills in offline `hq_console_live.html`).
   - **Gauges & Cache Savings:** Daily Uncached & Deep Budget bars, `QBU` / API-equiv `$`, Prefix Cache `CHR %` + **estimated uncached tokens/`$` saved by caching**, and the **Dual-Marker `512k` Front Desk Context Gauge** (`120k Warn` / `180k Crit` hygiene ticks + `/compact` chip).
   - **Per-Model & Family Breakdown:** Inline SVG stacked bar (`Carbon` · `Neon` · `Hydrogen` · `Claude`) + table (`Model`, `Tier`, `Turns`, `Uncached`, `Cache Read`, `CHR %`, `QBU Share`, `P50/Run`, `Latency`).
   - **Per-Task Attribution (Disjoint Roots):** `[HQ] Front Desk` (*Coordinator Self* vs. *Direct Subagents*) + top `[Task:*]` / `[Auto]` trees (`Self`, `Tree`, `Descendants`, `CHR %`, `Peak Ctx`, expandable child subagents on `:4242`).
   - **Trends & `24h` Anomalies:** Zero-JS-library inline SVG **`7d` Daily Burn + CHR** chart, **`24h` Hourly Burn** strip, and active `24h` `A1`–`A5` alerts.
4. **`🩺 Ops`:** Schedules (`NextRunHuman`, `0`-token skip counts, `Pause`/`Resume`/`Run Now`), Event Inbox & Timers, Channel Bridges, Scoped Delegates, Today's Turn Journal, `hq cancel` Reversals, and `hq doctor`.
5. **`console.html` & `internal/living` Hardening:**
   - Hoist `const id = btn.getAttribute('data-id') || ''` above action branches (`console.html:778`), route all `fetch` calls through a `hqFetch()` helper honoring `window.sidecar.fetch` + `X-HQ-CSRF-Token`, and update `refreshState()` to render all 4 tabs non-destructively without full-page reloads.
   - Add lightweight inline bar/sparkline cell support (`BlockMetricBars` / `TableRow` bar metadata) to `internal/living` (`< 45 KB`, strict `default-src 'none'` CSP, locale-neutral `["en"]` default with Vault `default_locales` i18n support).

---

## 5. Pillar 3: Single-Source-of-Truth Synergy (`CONSOLE.md` / `hq_console.md` vs. HTML Views)

All three console surfaces compile from a single `BuildConsoleSnapshot(vaultDir, opts)` call (`internal/webui/obs_view.go`, `0` LLM tokens), eliminating duplicated badge/metric logic while giving each surface a distinct job:

| Surface | Primary Audience & Medium | Exact Scope & Size Discipline |
| :--- | :--- | :--- |
| **`local/CONSOLE.md` & `<artDir>/hq_console.md`** | Quick Markdown glance, git-tracked Vault snapshot, universal `file://` & `conversation://` index | **Compact Executive Digest (`<= 120` lines, `< 6 KB`):** Top links to `hq_console_live.html`, Sidecar, & `:4242`; Approvals/Decisions; Goals; Active/Idle Task table; Workspaces; and a **6-line Ops & Token Health summary** (collapsing `hq doctor` to `✅ 19/19 PASS` unless checks `WARN`/`FAIL`, and removing legacy out-of-order `2.`, `1.`, `4.`, `5.` section numbers). |
| **`<artDir>/hq_console_live.html` & `/living-spaces/hq-console`** | Antigravity Mobile Remote Control & Desktop Artifact Side Pane (`0` localhost port needed) | **Portable Self-Contained 4-Tab Micro-App (`< 45 KB`):** Full `Needs Me`, `Work`, `Tokens` (`Today`/`7d` charts & model/task tables), and `Ops` tabs with `1`-tap **`📋 Copy` command chips** and `conversation://` links. |
| **`sidecar://agy-hq-console/` & `http://127.0.0.1:4242`** | Desktop Aux Pane & local browser control center | **Live Mutating 4-Tab Control Plane (`5s` live poll):** Direct authenticated `POST` execution (`Approve`, `Reject`, `Restage`, `Outbox Act`, Task/Trigger controls), `Today / 7d / 30d / All` interactive token explorer, and subagent tree drill-down. |

---

## 6. Pillar 4: Turn-1 Initialization, Handover Cleanup, `doctor.go` & `AGENTS.md` Sync

1. **Co-Equal Lease State & Handover Retirement (`internal/kernel/front_desk.go`):**
   - Add `LiveConsolePinned bool` (`json:"liveConsolePinned,omitempty"`) to `FrontDeskLease`. Preserve on same-conversation updates; reset to `false` on rotation.
   - Update `removeConsoleArtifacts(artifactDir)` to remove **`hq_console_live.html`** and **`hq_console_live.html.metadata.json`** alongside `hq_console.md` (+ `.metadata.json` and legacy `hq_console_mobile.md`) on `SetFrontDesk` rotation, `ReleaseFrontDesk`, and `hq adopt`.
   - Standardize `.metadata.json` (`userFacing: true, requestFeedback: false, summary, updatedAt`) and `0o644` artifact-dir permissions across both files; backfill `LiveConsolePinned` in `ReconcileFrontDeskFromTranscript`.
2. **Zero-Output-Token `{{HQ_SYNC}}` Pinning (`internal/hooks/session_start.go`, `pre_tool.go`):**
   - Pre-sync `CONSOLE.md` and `living_spaces/hq-console/index.html` inside `HandleSessionStart` when `isFrontDesk && (!fd.ConsolePinned || !fd.LiveConsolePinned)`.
   - Instruct `[HQ] Front Desk` on Turn 1 to call `write_file` in parallel on unpinned `<artDir>/hq_console.md` and/or `<artDir>/hq_console_live.html` with **`Content="{{HQ_SYNC}}"`** (`0` `read_file` calls needed!).
   - In `enforceFrontDeskGuardrails` (`pre_tool.go`), intercept `write_file` to `hq_console.md` or `hq_console_live.html`, mark `ConsolePinned` / `LiveConsolePinned`, set `*mutatedConsole = true`, exempt both filenames from the 3-lookup cap, and return `writePreToolAllowOverwrite` hydrating `Content` with the exact ground-truth bytes from disk when `Content == "{{HQ_SYNC}}"` (or short stub).
3. **`hq doctor`, CLI & `AGENTS.md` Alignment:**
   - **`internal/doctor/doctor.go`:** Check `#4` (`Front Desk Console Artifacts`) verifies both `hq_console.md` and `hq_console_live.html` (+ metadata files, `<vault>/living_spaces/hq-console/index.html`, and `mdPinned`/`livePinned`); Check `#10` uses dynamic `ContextCeilingTokens` (`512k`).
   - **`cmd/hq/tasks_cli.go` & `cmd/hq/main.go`:** Sync `hq claim-desk` Step 1 and `hq console` status output to list both artifacts.
   - **`.agents/plugins/agy-hq/rules/AGENTS.md` (§2.B):** Update Turn-1 Front Desk Initialization to specify parallel `write_file(..., Content="{{HQ_SYNC}}")` on `hq_console.md` and `hq_console_live.html` (`0` `read_file` steps) and automatic dual retirement on handover.

---

## 7. Guardrails, Implementation Phases & Open Review Questions

- **Repo Hygiene & LOC Ceilings:** Keeps `go.mod` at `0` external dependencies (`Gate H4-A`). Because `internal/webui/webui.go` is at `1,098 / 1,115` LOC (`Gate H5-F`), new snapshot and token view-model builders live in `internal/webui/obs_view.go`, keeping `webui.go` below ceiling.

| Phase | Scope | Verification |
| :--- | :--- | :--- |
| **Phase 2A (`internal/obs`)** | `512k` ceiling + Front Desk hygiene split, per-turn journal `Model`/`ModelTier`, 3-family + Claude `1 QBU = $0.01` pricing, `EndedAt` boundary, disjoint root trees + `SupersededConversationIDs`, `24h` `A1`–`A5` (`ColdStartInputTokens`, `PlannerSteps`), & cache stamps (`currentObsIndexVersion = 6`). | Unit tests covering multi-model turns, cross-midnight conservation, `A2` cold spikes, `A3` parallel tool lines, & cache invalidation. |
| **Phase 2B (`webui`, `living`)** | `BuildConsoleSnapshot` (`obs_view.go`), 4-tab `console.html` + `hq_console_live.html` with `Tokens` tab & SVG charts, `hqFetch` + `id` bugfixes, non-destructive `5s` polling, & compact `<= 120L` `CONSOLE.md`. | `webui` + `living` unit tests (`default-src 'none'` CSP, `< 45 KB` HTML size, single- vs multi-locale, `0` JS errors). |
| **Phase 2C (`kernel`, `hooks`, `doctor`, `AGENTS.md`)** | `LiveConsolePinned`, 6-file `removeConsoleArtifacts`, `PreToolUse` `{{HQ_SYNC}}` hydration, `SessionStart` dual-pin prompt, `doctor` `#4`/`#10`, CLI, & `AGENTS.md`. | **`CUJ-48`** black-box E2E test + `100%` `./bin/hq ci pre-push` pass. |

### Open Questions for Review
1. **QBU Calibration (`1 QBU = $0.01` vs. Legacy Token Multipliers):** Confirm anchoring `1 QBU = $0.01` over the 3-family + Claude rate table in §3.2 (while keeping `DailyUncachedBudget = 50M` and `DailyDeepUncachedBudget = 30M` denominated in raw uncached tokens so existing `budget.json` thresholds remain unchanged).
2. **Default Tab When `Needs Me == 0`:** Should `:4242` and `hq_console_live.html` always open on `Needs Me` (showing the green *"All Clear"* card + Goals), or auto-select `Work` when `Needs Me == 0` and no previous tab is saved in `sessionStorage`? *(Recommended: `Needs Me` when `ActionRequired > 0`, `Work` when `ActionRequired == 0` and `ActiveTasks > 0`, preserving user-clicked tab in `sessionStorage`.)*
3. **Turn-1 `{{HQ_SYNC}}` Hydration:** Confirm replacing the Turn-1 `read_file(CONSOLE.md)` + full-text `write_file` step with `0`-read parallel `write_file(..., Content="{{HQ_SYNC}}")` for both `hq_console.md` and `hq_console_live.html`.
