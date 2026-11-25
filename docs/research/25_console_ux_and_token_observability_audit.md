# 25 — Forensic Audit of Token Observability Math, Context Window Regimes, Multi-Surface Console UX & Front Desk Artifact Lifecycle

- **Date:** 2026-11-25
- **Task Scope:** `[Task:agy-hq] Console UX, Token Audit & MD Sync RFC` (`hq/console-ux-token-audit`)
- **Companion Specification:** [RFC-0030: Token Observability Calibration, Unified 4-Tab Console UX & Dual-Artifact Lifecycle Sync](../design/0030-console-ux-token-audit.md)
- **Related Documents:** [06 — Task Board & Console Audit](06_taskboard_and_console_audit.md), [08 — Empirical Token Telemetry Audit](08_token_observability_and_telemetry.md), [22 — Meta Muse Proactive Actions & Living Spaces Audit](22_muse_proactive_actions_and_living_artifacts_audit.md), [RFC-0007 (Front Desk Init Hardening)](../design/0007-front-desk-initialization-hardening.md), [RFC-0009 (Scalable Task Board)](../design/0009-task-board-redesign.md), [RFC-0012 (Token Observability)](../design/0012-token-observability.md), [RFC-0027 (Living Spaces & Calm Console)](../design/0027-muse-proactive-actions.md)

---

## 1. Executive Summary & Empirical Corpus Scope

Fifteen days after [RFC-0012](../design/0012-token-observability.md) shipped `internal/obs/`, the production Vault telemetry index has grown nearly **10×**—from the initial `68` transcripts (`4,894` steps, `505.8M` gross tokens) studied in [Research 08](08_token_observability_and_telemetry.md) to **`639` conversations, `2,883` turns, `57,075` steps, `88` `CHECKPOINT` compactions, `284.32M` uncached tokens (`266.24M` input + `18.08M` output), and `2,926.31M` (`2.93B`) gross tokens** (`90.85%` pooled prefix cache hit ratio). Over the same window, `agy-hq` migrated to 3-family model routing (`Hydrogen` / `Neon` / `Carbon`), shipped interactive Living Spaces (`hq_console_live.html` via [RFC-0027](../design/0027-muse-proactive-actions.md)), and experienced an upstream Antigravity context-window expansion.

Auditing the live `639`-conversation index line-by-line against `internal/obs/`, `internal/webui/`, `internal/living/`, `internal/kernel/front_desk.go`, `internal/hooks/`, `internal/doctor/doctor.go`, and `.agents/plugins/agy-hq/rules/AGENTS.md` uncovered **four systemic gaps**:

1. **Nine Mathematical & Attribution Distortions in `internal/obs/` (`M-1`–`M-9`):**
   - **Host Compaction Ceiling Regime Shift (`M-1`):** On `2026-11-16`, Antigravity doubled automatic `CHECKPOINT` compaction on Gemini 4 models from **`~256k`** (`256,819`–`276,364`) to **`~512k`** (`513,052`–`523,117`). Seven hardcoded `256k` constants across `obs`, `webui`, `doctor`, and CLI clamp active `360k`–`511k` tasks at `100%` while allowing `[HQ] Front Desk` to bloat to **`519,290` tokens** (`2×` cold-wakeup burn).
   - **Retroactive Single-Model Overwrite (`M-2`, `54.0%` of Corpus Burn):** Thirteen multi-phase conversations (`153.4M / 284.3M` uncached tokens) switched models mid-thread (e.g., Phase 1 RFC on `gemini-4-carbon-high` $\rightarrow$ Phase 2 coding on `gemini-4-neon-high`). Because `TurnMetrics` omits `Model`, `ConversationMetrics.Model` retroactively assigns **100% of prior turns** to the single last-read journal model.
   - **Role-Label Tier Overriding Executed Model Pricing (`M-3`, `28.9%` of Vault QBU):** `NormalizeTier(tier, model)` trusts the role/task label `tier` (`"deep"` / `"quick"`) ahead of the actual `model` string, billing `54.54M` `gemini-4-hydrogen-xhigh` and `5.74M` `gemini-3.8-flash-tiered` tokens at **`5.0×` `deep` weight (`534,303 QBU`)** and inflating `DailyDeepUncachedBudget` usage.
   - **Non-Monotonic QBU vs. USD Formula & Missing Neon/Sonnet Rates (`M-4`), Cross-Midnight `StartedAt` vs. `EndedAt` Split (`M-5`, `1.52M` uncached drift), Subtree Multi-Counting in `hq obs top --tree` (`M-6`), `306` Unbounded Zombie Anomalies + Simpson's Paradox in `A2` + `StepCount` Bug in `A3` (`M-7`), and Non-Front-Desk Cache Blindness (`M-8`–`M-9`).**
2. **Fragmented Console UX & Three Live Action Bugs (`console.html` & `console_space.go`):**
   - `:4242` / Sidecar (`console.html`) stacks **14 vertical sections** in a narrow side pane with no tabs, throws `ReferenceError: id is not defined` on all three [RFC-0027](../design/0027-muse-proactive-actions.md) POST actions (`outbox-act`, `restage-proposal`, `dismiss-expired`), bypasses `window.sidecar.fetch` (`x-sidecar-token`), never refreshes `#obs-card` during 5s polling while wiping task token badges and PII/expiry warnings, and leaves `DailyTrend` / `TierBreakdown` unrendered. Meanwhile, `hq_console_live.html` has only 3 tabs and **no Token Usage view**.
3. **Blurred Overlap Between `CONSOLE.md` / `hq_console.md` and HTML Views:**
   - `CONSOLE.md` repeats 19 static `PASS` rows from `hq doctor` and raw turn logs on every sync while lacking a single canonical view-model shared with `console_space.go` and `console.html`.
4. **Incomplete `hq_console_live.html` Lifecycle Sync:**
   - `removeConsoleArtifacts` (`internal/kernel/front_desk.go:149`) deletes `hq_console.md` on shift handover/release/adopt **but leaves stale `hq_console_live.html` (+ `.metadata.json`) orphaned in the retired thread**. `FrontDeskLease`, `SessionStart`, `PreToolUse`, `doctor.go`, `claim-desk`, and `AGENTS.md` only track and pin `hq_console.md`—and Turn-1 pinning still wastes a full `read_file` + multi-KB LLM `write_file` regeneration instead of `0`-output-token `PreToolUse` hydration.

---

## 2. Mathematical Audit of `internal/obs/` (`M-1` through `M-9`)

### 2.1 Verified Step-Level & Tree Conservation Laws

In Antigravity's `transcript.jsonl`, every `PLANNER_RESPONSE` and `CHECKPOINT` step $s$ emits three non-negative integer counters:
- $I_s = \texttt{input\_tokens}$: **strictly uncached prompt tokens** (newly appended user/tool messages + prefix-cache creation).
- $C_s = \texttt{cache\_read\_tokens}$: **prefix-cached prompt tokens read**.
- $O_s = \texttt{output\_tokens}$: **generated tokens** (visible response + reasoning/`thinking` + tool-call JSON).

For any step $s$ and any step sequence $S = (s_1, \dots, s_m)$:

$$P_s = I_s + C_s \quad \text{(Prompt Context)}, \qquad U_s = I_s + O_s \quad \text{(Uncached Burn)}, \qquad G_s = I_s + C_s + O_s \quad \text{(Gross Tokens)}$$

$$\text{CHR}(S) = \begin{cases} \dfrac{\sum_{s \in S} C_s}{\sum_{s \in S} (I_s + C_s)} & \text{if } \sum_{s \in S}(I_s + C_s) > 0 \\ 0 & \text{otherwise} \end{cases}$$

Across all `639` indexed conversations $\mathcal{C}$ (`362` root conversations $\mathcal{R}$ with `ParentConversationID == ""` and `277` child/descendant subagents, `0` missing parent references), our audit verified **exact conservation**:

$$\sum_{c \in \mathcal{C}} U(\text{Self}_c) = 266{,}243{,}413 + 18{,}076{,}067 = 284{,}319{,}480 = \sum_{r \in \mathcal{R}} U(\text{Tree}_r)$$

$$\sum_{c \in \mathcal{C}} G(\text{Self}_c) = 266{,}243{,}413 + 2{,}641{,}990{,}490 + 18{,}076{,}067 = 2{,}926{,}309{,}970, \qquad \text{CHR}(\mathcal{C}) = \frac{2{,}641{,}990{,}490}{2{,}908{,}233{,}903} = 90.845\%$$

While raw summation and `AddRollup` ratio-of-sums pooling are exact, **nine higher-level calculations in `internal/obs/` introduce severe quantitative errors**:

---

### 2.2 `M-1`: The `256k` $\rightarrow$ `512k` Compaction Regime Shift & Dual Context Thresholds

Inspecting all **`88` turns with `CompactionCount > 0`** across the `639`-conversation corpus reveals an exact step-function change on **`2026-11-16`**:

| Window | Compaction Turns | Auto-Compaction Peak ($P_s = I_s + C_s$) | Post-Checkpoint Context | Observed Models |
| :--- | ---: | ---: | ---: | :--- |
| **`2026-11-08` – `2026-11-15`** | `71` | **`256,819` – `276,364`** (`~256k`) *(plus 9 subagent early ckpts at `67k–115k`)* | `20,568` – `221,919` (median `~77k`) | `gemini-4-argon-high`, `gemini-4-carbon-high`, `gemini-4-hydrogen-high/xhigh`, `gemini-3.8-flash-*` |
| **`2026-11-16` – `2026-11-25`** | `17` | **`513,052` – `523,117`** (`~512k`) *(plus 3 manual `/compact` runs on Front Desk at `206k`, `276k`, `364k`)* | `22,554` – `280,324` (median `~50k`) | `gemini-4-carbon-high`, `gemini-4-neon-medium/high`, `gemini-4-hydrogen-high/xhigh` |

**Two bugs result from hardcoding `256,000` in 7 places** ([`webui.go:1078`](../../internal/webui/webui.go), [`console.html:445-464`](../../internal/webui/assets/console.html), [`console_md.go:324`](../../internal/webui/console_md.go), [`doctor.go:366-378`](../../internal/doctor/doctor.go), [`anomaly.go:162`](../../internal/obs/anomaly.go), [`obs.go:1419`](../../internal/obs/obs.go), [`ops_cli.go:687`](../../cmd/hq/ops_cli.go)):
1. **Broken Fill Gauge:** `ctxPct(cur)` computes $\min(100, \frac{\text{cur}}{256000}\times 100)$, displaying `511k / 256k` (`100.0%`) on active tasks (`tk000080` at `511,137` tokens, `tk000108` at `360,078` tokens) when the host ceiling is actually **`512,000`** (`99.8%` and `70.3%`).
2. **Doubled Cold-Wakeup Hazard on `[HQ] Front Desk`:** Under the old `256k` ceiling, an un-compacted Front Desk auto-compacted at `~257k`. Under the `512k` host ceiling, `[HQ] Front Desk` (`fd000001`) grew to **`519,290` tokens** (`2026-11-18T04:29Z`), doubling the uncached input penalty of every post-idle wakeup (`> 5m` prefix cache TTL). Thus, **Host Auto-Compaction Ceiling (`ContextCeilingTokens = 512,000`)** and **Front Desk Cold-Cache Hygiene Thresholds (`Warn = 120k`, `Nudge = 160k`, `Crit = 180k`)** are distinct physical quantities that must be modeled and rendered separately.

---

### 2.3 `M-2` & `M-3`: Single-Model Conversation Overwrite (`54.0%` Drift) & Role-Tier Pricing Bug (`28.9%` QBU Drift)

1. **Lack of Per-Turn Model Attribution (`M-2`):**
   - Antigravity's `transcript.jsonl` does not embed `model` on `PLANNER_RESPONSE` lines; `modelName` is recorded per turn in `local/memory/journal/YYYY-MM-DD/<convId>-<execNum>.json` alongside `(stepStartIdx, stepCount)`.
   - `discoverTranscriptsCached` ([`obs.go:617-649`](../../internal/obs/obs.go)) iterates `os.ReadDir(datePath)` in lexicographical filename order (where `-10.json` sorts *before* `-2.json`!) and collapses all journal entries for a conversation into a **single** `modelHint` string.
   - Across the live Vault, **`13` conversations totaling `153,412,584` uncached tokens (`54.0%` of the entire `284.32M` corpus!)** switched models across turns:
     - `tk000088` (`54.54M` uncached, `209` turns): executed across **7 models** (`claude-sonnet-5-5-medium`, `gemini-4-argon-high`, `gemini-4-carbon-medium`, `gemini-4-neon-high`, `gemini-4-hydrogen-{medium,high,xhigh}`), yet `100%` of its `54.54M` tokens were attributed to `gemini-4-hydrogen-xhigh`.
     - `fd000001` (`[HQ] Front Desk`, `45.40M` uncached, `893` turns): spanned 5 models across upgrades (`gemini-3.8-flash-high` $\rightarrow$ `gemini-4-hydrogen-high` $\rightarrow$ `gemini-4-hydrogen-medium`), yet all `45.40M` tokens remained pinned to `gemini-4-hydrogen-high`.
     - Phase 1 $\rightarrow$ Phase 2 Task Threads (`tk000057` `10.11M`, `tk000108` `8.95M`, `tk000080` `5.01M`): ran Phase 1 RFCs on `gemini-4-carbon-high` and Phase 2 implementation on `gemini-4-neon-high`, erasing all Phase 1 `carbon-high` token history once Phase 2 ran.
   - Additionally, when `LoadOrRefreshIndex` indexes an in-flight transcript before `hq hook stop` writes its first journal entry, [`dag.go:242-251`](../../internal/obs/dag.go) writes a synthetic default (`"gemini-4-neon-medium"`) into `cm.Model`, preventing [`obs.go:754`](../../internal/obs/obs.go) (`if cand.modelHint != "" && cached.Model == ""`) from ever adopting the real journal `modelName` unless file size changes again.

2. **Role/Task `Tier` Overriding Ground-Truth Model Pricing (`M-3`):**
   - In [`obs.go:265-278`](../../internal/obs/obs.go), `NormalizeTier(tier, model)` checks `if t == "quick" || t == "standard" || t == "deep" { return t }` **before** inspecting `model`.
   - Consequently, `tk000088` (created with `Tier: "deep"`, later switched to `gemini-4-hydrogen-xhigh`) and `21` subagents with `[Deep]` role prefixes running on `gemini-3.8-flash-tiered` were priced at **`5.0×` `deep` weight**:

     | Mismatched `(cm.Tier, cm.Model)` Pair in Live Index | Convs | Uncached Tokens | Inflated / Distorted QBU | True Model Class | True QBU (`1x` / `5x`) |
     | :--- | ---: | ---: | ---: | :---: | ---: |
     | `("deep", "gemini-4-hydrogen-xhigh")` | `1` | `54,542,586` | `434,642.5` (`5.0×`) | `standard` (`Hydrogen`) | `86,928.5` |
     | `("deep", "gemini-3.8-flash-tiered")` | `21` | `5,736,018` | `99,660.4` (`5.0×`) | `standard` (`Flash`) | `19,932.1` |
     | `("quick", "gemini-4-hydrogen-tiered-high")` | `11` | `2,138,126` | `1,148.0` (`0.25×`) | `standard` (`Hydrogen`) | `4,592.0` |
     | `("quick", "gemini-4-carbon-medium")` | `1` | `68,916` | `17.5` (`0.25×`) | `deep` (`Carbon`) | `350.0` |

   - **Impact:** `426,563` phantom QBU (`23.0%` net distortion, `28.9%` gross misclassified QBU) and false `TodayDeepUncachedTokens` accumulation toward the `30M` daily deep circuit breaker. **Fix:** Derive `ModelTier` and `ComputeQuotaAndCost` strictly from the turn's executed `Model`, while preserving `RequestedTier` from task/role metadata solely for `A4` routing-drift inspection.

---

### 2.4 `M-4` & `M-5`: Non-Proportional QBU Formula, Missing 3-Family Rates & Cross-Midnight Drift

1. **QBU vs. `EstCostUSD` Non-Proportionality & Missing Models (`M-4`):**
   - [RFC-0012 §3.1](../design/0012-token-observability.md) defined **`1 QBU = $0.01` API-equivalent compute**, yet [`obs.go:281-311`](../../internal/obs/obs.go) implemented two incompatible linear forms:
     $$\text{QBU}_{\text{code}} = w_{\text{tier}} \cdot \frac{I + 0.1 C + 3.0 O}{1000}, \qquad \text{Cost}_{\text{USD}} = \frac{r_{\text{in}} I + r_{\text{cache}} C + r_{\text{out}} O}{10^6}$$
     In `standard`, input/cache/output weights are `1 : 0.10 : 3.0` in QBU vs. `1 : 0.25 : 8.33` in USD (`$0.30 / $0.075 / $2.50`), so two models with identical `$` cost report up to **`2.8×` different QBU** depending on output/cache ratio!
   - Furthermore, `ComputeQuotaAndCost` lacks entries for **`gemini-4-neon-*`** (treating `neon-high` identically to `hydrogen-medium` when `standard`, or identically to `carbon-high` when `deep`) and **`claude-sonnet-5-5-*`** (falling into Gemini `deep` at `$1.25/$10.00` instead of `$3.00/$0.30/$15.00`).
   - **Canonical Calibration:** Anchor `QuotaUnits = EstCostUSD * 100.0` (`1 QBU = $0.01`) over a complete 3-family + Claude rate table configurable in `<vault>/memory/obs/budget.json`.

2. **Cross-Midnight `StartedAt` vs. `EndedAt` Boundary Contradiction (`M-5`):**
   - `turnMatchesCutoff` ([`obs.go:816-828`](../../internal/obs/obs.go)) evaluates `EndedAt >= cutoff`, whereas `localDateStr` in `BuildSummaryReport` ([`obs.go:958`](../../internal/obs/obs.go)), `BuildDashboardSummary` ([`obs.go:1304,1312`](../../internal/obs/obs.go)), and `EvaluateAnomalies` / `SyncDailyBudgetCircuitBreaker` ([`anomaly.go:203,377`](../../internal/obs/anomaly.go)) buckets by `StartedAt`.
   - Across `6` cross-midnight turns in the corpus (`1,516,386` uncached tokens—e.g., `23:30` `sleep-consolidate` runs finishing after `00:00`), `hq obs tokens --since=today` includes them in `TodayUncached` (`EndedAt`) while bucketing them into **yesterday's date row** (`StartedAt`), disagreeing with `:4242` and the budget circuit breaker. Standardizing on `turnEffectiveTime(t)` (`EndedAt`, fallback `StartedAt`) eliminates all cross-midnight drift.

---

### 2.5 `M-6`: Subagent DAG Multi-Counting in Ranked Tables & Orphaned Retry Attempts

1. **Overlapping Ancestor + Descendant Rows in `hq obs top --tree`:**
   - Although root trees partition the Vault cleanly ($\sum_{r \in \mathcal{R}} \text{Tree}_r = \sum_{c \in \mathcal{C}} \text{Self}_c$), `BuildTopReport(..., treeMode=true)` ([`obs.go:1109-1132`](../../internal/obs/obs.go)) ranks *every* node (`task`, `subagent`, `nested_subagent`) together. In live output, **one codebase-audit root task (`31.76M` tree) occupied 5 of the Top 10 rows** (`#3` root `31.76M`, `#4` child `22.81M`, `#5` grandchild `21.29M`, `#9` & `#10` great-grandchildren `5.92M` & `5.31M`).
   - Similarly, `[HQ] Front Desk`'s `Tree` (`81.30M`) combines coordinator `Self` (`45.40M`) + `167` direct `invoke_subagent` trees (`35.90M`), whereas standalone `hq spawn` `[Task:*]` conversations have `ParentConversationID == ""`. A per-task UI table must rank **disjoint roots** (splitting `[HQ] Front Desk` into *Coordinator Self* vs. *Direct Subagents*, alongside top-level `[Task:*]` and `[Auto]` trees) with expandable child drill-down so percentages never sum above `100%`.
2. **Unindexed `SupersededConversationIDs` & Direct-Only `ChildCount`:**
   - `dag.go:90-107` indexes `t.ConversationID` but ignores `t.SupersededConversationIDs` (orphaning retried `[Auto]` attempts as generic `[Task:general] Conversation ...` roots), and `TopItem.ChildCount` counts only depth-1 children (`1` on `tk000108`) rather than recursive `DescendantCount` (`22`).

---

### 2.6 `M-7`, `M-8` & `M-9`: Anomaly False Positives, Simpson's Paradox in `A2` & Cache Stale Windows

1. **Forensic Breakdown of the `306` Live Anomalies (`M-7`):**
   - Running `hq obs anomalies` on the live Vault emitted **`306` alerts**: `200` `A3 WARN`, `86` `A3 CRITICAL`, `20` `A4 WARN`, **`0` `A1`**, **`0` `A2`**, **`0` `A5`**.
   - **Bug 7a (Unbounded All-Time Scan):** `EvaluateAnomalies` ([`anomaly.go:142`](../../internal/obs/anomaly.go)) evaluates every conversation since day 1 with no recency filter, pinning weeks-old `CRITICAL` `A3` subagent turns permanently in `DashboardSummary.Anomalies` (`MaxDashboardAnomalies = 20`).
   - **Bug 7b (`A3` Uses Raw JSONL `StepCount` Instead of `PlannerSteps`):** [RFC-0012 §6](../design/0012-token-observability.md) specified `PlannerSteps > 60`, but [`anomaly.go:210`](../../internal/obs/anomaly.go) wrote `t.StepCount >= 60`. Because a single parallel 4-file `read_file` turn step writes `1` `PLANNER_RESPONSE` + `4` `GENERIC` lines (`5` JSONL steps), normal 15–25 step research subagents hit `StepCount >= 120` and fire `CRITICAL` runaway alerts (`286` false positives).
   - **Bug 7c (Simpson's Paradox in `A2 ColdCacheThrash`):** [`anomaly.go:173`](../../internal/obs/anomaly.go) checks cumulative `cm.Self.CacheHitRatio < 0.70`. On `[HQ] Front Desk`, suppose Turn $k$ wakes cold after a 30m gap at context $P = 250\text{k}$ (`Step 1`: $I_1 = 250\text{k}, C_1 = 0, \text{CHR}_1 = 0\%$) and executes 3 quick tool calls (`Steps 2–4`: $I_j = 2\text{k}, C_j \approx 252\text{k}, \text{CHR}_j = 99.2\%$). Turn CHR is:
     $$\text{CHR}_{\text{turn}} = \frac{0 + 3 \times 252\text{k}}{250\text{k} + 3 \times 254\text{k}} = \frac{756\text{k}}{1{,}012\text{k}} = 74.7\% > 70\%$$
     Warm intra-turn steps mask Step 1's `250k` cold-start tax (`fd000001` cumulative CHR = `86.6%`, so `A2` fired **`0` times** despite dozens of `150k–364k` cold wakeups!). Recording **`ColdStartInputTokens`** on each turn's *first* `PLANNER_RESPONSE` ($I_{s_1} \ge 50{,}000$ with $\text{CHR}_{s_1} < 0.20$) directly measures true cold-wakeup waste.
   - **Bug 7d (Inverted `A4 TierMisroutingBurn`):** [`anomaly.go:265`](../../internal/obs/anomaly.go) flags legitimate heavy `[Deep]` subagents (`>= 300k` uncached) while missing `[Auto]` / `[Quick]` tasks accidentally routed to `deep` models or 3-step lookup-only `deep` subagents.

2. **Non-Front-Desk Cache Blindness, `0`-Token Phantom Turns & Scope Mismatch (`M-8` & `M-9`):**
   - `loadSummaryCache` ([`summary_cache.go:125`](../../internal/obs/summary_cache.go)) and `computeVaultStateFingerprint` ([`webui.go:686`](../../internal/webui/webui.go)) only stat `[HQ] Front Desk`'s `transcript.jsonl`, serving up to `5m`-stale `TodayUncachedTokens` and `TaskBadges` when work or `/compact` happens in `[Task:*]` conversations.
   - `ParseTranscriptFile` emits a `TurnMetrics` with `PlannerSteps == 0, GrossTokens == 0` when a thread has only `USER_INPUT` (`7` conversations in corpus), skewing `ConvCount`, `TurnCount`, and `P50UncachedPerRun`.
   - `DashboardSummary.TierBreakdown` ([`obs.go:1277`](../../internal/obs/obs.go)) sums **all-time** tokens inside a card headered by **Today's** KPIs (`M-9`).

---

## 3. Forensic UX Audit of Console Surfaces (`console.html` & `console_space.go`)

| Surface / File | Defects Identified in Current Implementation | User Impact |
| :--- | :--- | :--- |
| **Sidecar & `:4242` WebUI** ([`internal/webui/assets/console.html`](../../internal/webui/assets/console.html)) | **1.** 14 vertically stacked sections in a single ~360px side-pane scroll with zero tabs.<br>**2.** **Live JS `ReferenceError` ([`L815, L822, L828`](../../internal/webui/assets/console.html)):** `const id` is scoped inside `else if (act === 'trigger-action')` (`L808`), so `outbox-act`, `restage-proposal`, and `dismiss-expired` throw `ReferenceError: id is not defined`.<br>**3.** **Sidecar Token Bypass:** Calls raw `fetch()` instead of `window.sidecar.fetch()`, omitting `x-sidecar-token` when proxied via `agy-hq-console` (`main.mjs:251`).<br>**4.** **Partial/Destructive 5s Polling (`refreshState`, `L840–965`):** Never updates `#obs-card`, Quick Decisions, Expired Proposals, Workspaces, Ideas, or Health; overwrites `#tasks-list` (erasing per-task token badges!) and `#pending-list` (erasing `ExpiresAt` & `RiskFlags` PII warnings!).<br>**5.** **Unrendered Telemetry:** `DailyTrend` and `TierBreakdown` exist in `/api/state` JSON but have zero HTML/SVG rendering; no per-model, per-task tree, or hourly trend views exist. | 1-tap decision/restage buttons fail silently; token stats freeze after initial page load; task token & PII badges vanish after 5 seconds; narrow sidecar requires endless scrolling. |
| **Live Console Space** ([`internal/webui/console_space.go`](../../internal/webui/console_space.go) & [`internal/living/render.go`](../../internal/living/render.go)) | **1.** Only 3 tabs (`Needs Me`, `Work & Spaces`, `Ops & Health`)—no `Tokens` tab (token telemetry is reduced to one `5.98M (12%) · 81%` KPI string).<br>**2.** `render.go:133` prepends an `"All Views"` pill before `Needs Me`, dumping every tab into one long page when tapped.<br>**3.** `internal/living` lacks inline bar/sparkline rendering for visual token share, context fill markers, and 7-day/24h burn trends under strict `default-src 'none'` CSP. | Mobile Remote Control and Antigravity side-pane users have zero visibility into model/task burn, cache efficiency, or Front Desk context health. |
| **Markdown Console** ([`internal/webui/console_md.go`](../../internal/webui/console_md.go)) | Prints all 19 `hq doctor` rows verbatim (`~2.5 KB`) even when `19/19 PASS`, mixes numbered (`📋 2.`, `📊 1.`, `🕒 4.`, `🩺 5.`) and unnumbered headings from organic RFC growth, and formats badges independently of `console_space.go`. | Visual noise in Markdown view; inconsistent numbering and drift across surfaces. |

---

## 4. Lifecycle Audit: `hq_console.md` vs. `hq_console_live.html` Across Front Desk Shifts

Tracing every Front Desk state transition from [RFC-0007](../design/0007-front-desk-initialization-hardening.md) against [RFC-0027](../design/0027-muse-proactive-actions.md)'s `hq_console_live.html` reveals **7 asymmetric lifecycle touchpoints**:

| Lifecycle Stage | Code Location | `hq_console.md` Today | `hq_console_live.html` Today (Gap) | Required Fix |
| :--- | :--- | :--- | :--- | :--- |
| **1. Lease State** | [`internal/kernel/front_desk.go:41`](../../internal/kernel/front_desk.go) | Tracks `ConsolePinned bool` | **Untracked** (no `LiveConsolePinned`) | Add `LiveConsolePinned bool` to `FrontDeskLease`; preserve on same-conv, reset on shift change. |
| **2. Shift Handover / Release / Adopt Cleanup** | [`internal/kernel/front_desk.go:149`](../../internal/kernel/front_desk.go) (`removeConsoleArtifacts`) | Removes `hq_console.md` & `.metadata.json` (+ legacy `hq_console_mobile.md`) | **Orphaned!** Left behind in outgoing conversation's `<artifactDir>/` forever | Add `hq_console_live.html` and `hq_console_live.html.metadata.json` to `removeConsoleArtifacts`. |
| **3. Turn-1 `SessionStart` Init** | [`internal/hooks/session_start.go:251`](../../internal/hooks/session_start.go) | Checks `!fd.ConsolePinned`; asks LLM to `read_file(CONSOLE.md)` + `write_file(hq_console.md)` (~4.5 KB output) | **Never pinned via `write_file`**, so Antigravity UI never registers its artifact card | Pre-sync both on `SessionStart`; instruct parallel `write_file` with `Content="{{HQ_SYNC}}"` for any unpinned artifact (`hq_console.md` & `hq_console_live.html`). |
| **4. `PreToolUse` Pin & Hydration** | [`internal/hooks/pre_tool.go:766,793`](../../internal/hooks/pre_tool.go) | Marks `ConsolePinned`, **forgets `*mutatedConsole = true`**, requires full LLM text | Not matched for pin tracking; not exempted from 3-lookup cap; `0600` vs `0644` metadata drift | Hydrate `Content="{{HQ_SYNC}}"` via `writePreToolAllowOverwrite` from disk (`0` LLM output tokens!), mark respective pin flag, set `*mutatedConsole = true`, exempt both from lookup cap. |
| **5. Transcript Backfill** | [`internal/kernel/front_desk.go:680`](../../internal/kernel/front_desk.go) | Scans `write_file`/`edit_file` for `hq_console.md` | **Ignored** during transcript backfill | Backfill `LiveConsolePinned = true` when `transcript.jsonl` contains `write_file`/`edit_file` to `hq_console_live.html`. |
| **6. `hq doctor` Checks `#4` & `#10`** | [`internal/doctor/doctor.go:161,346`](../../internal/doctor/doctor.go) | Check `#4` stats `hq_console.md` + `.metadata.json` only | **Unchecked** in `hq doctor`; Check `#10` hardcodes `/ 256k` | Verify both `hq_console.md` and `hq_console_live.html` (+ metadata & pin flags) in Check `#4`; use dynamic `ContextCeilingTokens` (`512k`) in Check `#10`. |
| **7. CLI & `AGENTS.md` Rules** | [`cmd/hq/tasks_cli.go:246`](../../cmd/hq/tasks_cli.go), [`cmd/hq/main.go:159`](../../cmd/hq/main.go), [`.agents/plugins/agy-hq/rules/AGENTS.md:26,33`](../../.agents/plugins/agy-hq/rules/AGENTS.md) | Mentions only `hq_console.md` | Omitted from `claim-desk`, `hq console`, and Turn-1 rule instructions | Sync `claim-desk`, `hq console`, and `AGENTS.md` to reference dual `{{HQ_SYNC}}` pinning and shared retirement. |
