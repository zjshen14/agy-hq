# 26 — Forensic Audit of Multi-Agent Orchestration (`.agents/teamwork/` vs. `swe-pulse`), 3-Family Neon Routing, Subagent Model Parity & Dynamic Budget/Context Throttling

- **Date:** 2026-11-27
- **Task Scope:** `[Task:agy-hq] Neon Orchestrator Mode RFC` (`hq/neon-orchestrator-mode`)
- **Companion Specification:** [RFC-0031: Neon Orchestrator Mode — First-Class Coordinator Routing, Multi-Wave `swe-pulse` Execution, Subagent Parity & Dynamic Budget/Context Throttling](../design/0031-neon-orchestrator-mode.md)
- **Related Documents:** [08 — Empirical Token Telemetry Audit](08_token_observability_and_telemetry.md), [25 — Forensic Audit of Token Math & Console UX](25_console_ux_and_token_observability_audit.md), [RFC-0009 (Scalable Task Board)](../design/0009-task-board-redesign.md), [RFC-0011 (CI & Harness Gate)](../design/0011-ci-pipeline-and-regression-harness.md), [RFC-0012 (Token Observability)](../design/0012-token-observability.md), [RFC-0018 (Progress Checkpoints)](../design/0018-always-on-agent-runtime-hardening.md), [RFC-0019 (Vault Skills)](../design/0019-two-tier-rsi-vault-overlay.md), [RFC-0025 (Pluggable Workspaces)](../design/0025-pluggable-workspace-providers.md), [RFC-0030 (Token Calibration)](../design/0030-console-ux-token-audit.md)

---

## 1. Executive Summary & Empirical Corpus Scope

Auditing the live `656`-conversation production telemetry index (`3,060` turns, `60,374` steps, `308.44M` uncached tokens, `3,189.62M` / `3.19B` gross tokens, `90.9%` pooled prefix cache hit ratio) alongside `.agents/teamwork/` (`87` orchestration directories from Round 3 codebase compaction), [`internal/routing/routing.go`](../../internal/routing/routing.go), [`internal/kernel/kernel.go`](../../internal/kernel/kernel.go), [`internal/kernel/ls_client.go`](../../internal/kernel/ls_client.go), [`internal/kernel/spawn.go`](../../internal/kernel/spawn.go), [`internal/obs/anomaly.go`](../../internal/obs/anomaly.go), [`internal/obs/obs.go`](../../internal/obs/obs.go), [`internal/hooks/pre_tool.go`](../../internal/hooks/pre_tool.go), [`internal/ci/harness.go`](../../internal/ci/harness.go), and [`templates/skills/swe-pulse/SKILL.md`](../../templates/skills/swe-pulse/SKILL.md) (plus `<vault>/skills/swe-pulse/SKILL.md`) surfaced **four structural gaps** in how `agy-hq` coordinates multi-file engineering work:

1. **No First-Class Coordinator Tier (`Carbon` Quota Bleed vs. Host `Orchestrator (Neon)` Cache Thrash):**
   - Prompts describing multi-package Phase 2 implementation, refactors, or audits match `reDeepTask` (`architect|design|security|audit|race condition|rfc|complex`) in [`routing.go:16`](../../internal/routing/routing.go) and route straight to `deep` (`gemini-4-carbon-high`). Across the 4 recent flagship engineering threads (`tk-mem-v2`, `tk-muse`, `tk-rsi-v2`, `tk-console`), Phase 1 RFC design used only **`30` turns (`8.30M` uncached)** on `gemini-4-carbon-high`, while **`123` of `153` turns (`80.4%`, `17.40M` uncached)** were Phase 2 implementation and subagent coordination manually switched onto `gemini-4-neon-high` to protect `Carbon` (`30M` daily deep cap).
   - Meanwhile, Antigravity's built-in `Orchestrator (Neon)` (`gemini-pro-orchestrator-neon`) alternates planner turns (`gemini-pro-orchestrator-neon`) and worker turns (`gemini-flash-worker-agy`) inside a **single** transcript, busting the KV prefix cache on every handoff (`265k–309k` uncached input per switch; **`69.2%` CHR** vs. **`95.9%` CHR** on `gemini-4-neon-medium` and **`91.4%` CHR** on `gemini-4-neon-high`)—and [`obs.go:308,323,358`](../../internal/obs/obs.go) misbills `gemini-pro-orchestrator-neon` as `Carbon` (`deep`, `$1.25/$10.00`) because `strings.Contains(m, "pro")` precedes `strings.Contains(m, "neon")`.
2. **`.agents/teamwork/` Ceremony, Polling-Cron Thrash & Single-Checkout Serialization:**
   - Executing Round 3 compaction (`169` files, `+3,946 / -3,607` LOC) via generic `/teamwork` spawned **`85` subagents** (`87` `.agents/teamwork/` directories, `352+` markdown files), burned **`22.81M` uncached tokens (`329.62M` gross, `~$35.88`)**, and took **`3h10m`** because Milestones `M1`–`M5` ran **sequentially in one shared checkout**, armed recurring `10m` polling crons (`48` sentinel turns + `28` orchestrator turns, suffering `171k–213k` uncached cold-cache spikes), ran **5 verification agents per milestone iteration** (`2` Reviewers + `2` Challengers + `1` Auditor) before deterministic `./bin/hq ci pre-push`, and hit a `PreToolUse` `forge_git` push block at completion because the orchestrator itself ran as a nested subagent.
   - Conversely, `swe-pulse` (`32` template lines) knows how to create `hq worktree add` directories, but defines **no wave structure, no scout/builder/verifier model tiering, no file-ownership fences, and no `hq progress` wave checkpoints**.
3. **Silent Divergence Between Native `invoke_subagent(Model=...)` and `hq subagent` Resolution:**
   - Joining `290` live `spawnEdges` against executed journal models proves that Antigravity's native `invoke_subagent(Model="flash")` **never** resolves to `Neon`—it resolves to **`gemini-4-hydrogen-tiered-high`** even when the parent is running on `gemini-4-neon-high`! Native `Model="flash_lite"` resolves to legacy **`gemini-3.5-flash-lite`**, while `Model="inherit"` is the **only** native enum that preserves `gemini-4-neon-{medium,high}` from a `Neon` parent.
   - Inside [`routing.go`](../../internal/routing/routing.go), `--tier=standard` resolves to `gemini-4-neon-medium` (`SubagentModel: "flash"`), yet `hq subagent --model=flash` falls through [`routing.go:208`](../../internal/routing/routing.go) to `gemini-4-hydrogen-high`, and `--model=inherit` fails with `unknown Antigravity model ID "inherit"`.
4. **Binary `100%` Budget Cliff, Missing `SpawnSubagent` `429` Fallback & Flat `120k` Context Nudge:**
   - `DailyUncachedBudget` (`50M`) and `DailyDeepUncachedBudget` (`30M`) apply zero routing backpressure below `100%` (where only `kind: auto` triggers pause). [`SpawnSubagent`](../../internal/kernel/spawn.go) (`spawn.go:484`) lacks the `429` (`RESOURCE_EXHAUSTED`) retry fallback that `SpawnTask` (`spawn.go:212`) has—and `SpawnTask` cliff-drops from `neon-high` straight to `hydrogen-medium`, skipping `neon-medium`. Finally, [`PreCompactionFlushTokens = 120,000`](../../internal/hooks/hooks.go) fires once at `23%` of the `512k` ceiling with a `2h` mute file, leaving `250k–400k` coordinators unthrottled (`2.33M–7.20M` cold-wakeup burn per task).

---

## 2. Forensic Audit of `.agents/teamwork/` vs. `agy-hq` Native Primitives

Inspecting `.agents/teamwork/` (`ORIGINAL_REQUEST.md`, `sentinel/`, `teamwork_orchestrator_r3/`, `teamwork_sub_orch_m1`..`m4_gen2`..`m5/`, and `teamwork_victory_auditor_r3/`) alongside `hq obs inspect` on the sentinel root (`sub-teamwork-sentinel`) and orchestrator (`sub-teamwork-orch`) quantifies where every token and minute went:

| Subtree / Role Layer in `.agents/teamwork/` | Agents | Turns | Uncached Burn | Gross Tokens | CHR % | API-Equiv `$` | Observed Structural Overhead |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: | :--- |
| **Sentinel Relay (`sub-teamwork-sentinel`)** | `1` | `48` | `1.21M` | `24.57M` | `95.3%` | `$2.23` | Pure relay + `10m` cron polling (`T#24` `171k` & `T#32` `204k` cold spikes). |
| **Project Orchestrator (`sub-teamwork-orch`)** | `1` | `28` | `814k` | `15.11M` | `95.0%` | `$1.45` | Sequential `M1`→`M5` dispatch + `10m` cron (`T#17` `179k` & `T#22` `213k` cold spikes). |
| **Step-0 Survey Explorers (`survey_1..3`)** | `3` | `4` | `1.46M` | `23.56M` | `94.2%` | `$2.37` | Ran on `hydrogen-tiered-high` (`307k–689k` uncached each) instead of `hydrogen-low`. |
| **Milestone Sub-Orchestrators (`M1`–`M5` + `m4_gen2`)** | `6` | `99` | `2.28M` | `48.20M` | `95.7%` | `$4.84` | Extra middle-management layer + `16`-spawn succession (`m4_gen2`) + cron timers. |
| **Milestone Explorers, Workers & Redundant Gate Agents** | `72` | `~145` | `16.73M` | `213.00M` | `93.1%` | `$24.44` | `3` Explorers + `1` Worker + **`2` Reviewers + `2` Challengers + `1` Auditor** per iter (`M2`/`M3`/`M4` ran 2 iters = `19` agents/milestone) on **1 shared git checkout**. |
| **Interviewer + Victory Auditor** | `2` | `6` | `319k` | `5.18M` | `94.6%` | `$0.55` | Post-merge check against `ORIGINAL_REQUEST.md` (`VICTORY CONFIRMED`). |
| **Total `/teamwork` Tree** | **`85`** | **`~330`** | **`22.81M`** | **`329.62M`** | **`93.9%`** | **`$35.88`** | **`3h10m` wall-clock (`04:25Z`–`07:35Z`); `87` `.agents/teamwork/` folders; final `git push` blocked.** |

### Six Takeaways for `agy-hq` Orchestrator Design
1. **Keep What Worked:** Upfront read-only repo scouting (`Step 0`), explicit milestone contracts (`SCOPE.md`), and a **fresh-context adversarial verifier** (`Victory Auditor` / `Challenger` checking `ORIGINAL_REQUEST.md` against `git diff` without builder sunk-cost bias) caught real doc-sync and edge-case omissions in `M2`–`M4`.
2. **Eliminate Middle-Management & Redundant Prose Gatekeepers (`10` Agents/Iter $\rightarrow$ `1` Builder + `0`-Token `hq ci pre-push` + `1` Fresh Challenger):** `agy-hq` already enforces formatting (`H4-A`), relative links & `docs/README.md` (`H4-B`), dead Go symbols (`H5-B`), unlocked store writes (`H5-C`), sidecar/SemVer parity (`H2-C`/`H5-D`), CUJ parity (`H5-E`), monolith LOC caps (`H5-F`), PII (`P1`–`P6`), and `-race` tests in **`./bin/hq ci pre-push`** (`0` LLM tokens). Running `5` LLM review/audit subagents per milestone before `ci pre-push` wastes `~60%` of subtree tokens.
3. **Ban Polling Crons (`0` Heartbeat Timers):** Antigravity wakes an idle parent reactively the instant a child calls `send_message` (`0` tokens while asleep). Eliminating `10m` status crons removes `70+` empty turns and `>750k` uncached cold-wakeup tokens per run.
4. **Parallelize Builders Across Isolated `hq worktree` / `hq workspace` Checkouts with File-Ownership Fences:** Because `/teamwork` mutated the root checkout in place, `M1`–`M5` could not run concurrently. Giving each Wave-2 builder an isolated `<vault>/worktrees/<repo>-<slug>-w2-<slice>/` checkout plus disjoint file-ownership globs lets `2–4` builders code and run `go test` simultaneously without colliding on shared index files (`cmd/hq/main.go`, `docs/README.md`).
5. **Replace `87` `.agents/teamwork/` Repo Folders with Vault `hq progress` Checkpoints:** Writing `352+` orchestration files inside the target repo risks git dirtiness and `P3` PII leaks; `local/memory/progress/<slug>.md` (`<= 60` lines, auto-injected into `SessionStart` Zone 2 and archived by `hq archive` / `hq tasks compact`) already provides lock-safe Vault persistence.
6. **Run Top-Level Coordinators as `[Task:<scope>]` (`hq spawn --orchestrator`), Not Nested Subagents:** Top-level `[Task:<scope>]` sessions hold `authz.ProfileInteractiveTask` (`⊤`), allowing direct owned-repo `git push` after green CI and 1-tap `hq propose` + `ask_question` when human sign-off is required.

---

## 3. Empirical Audit of 3-Family Routing, `Orchestrator (Neon)` & Subagent Model Parity

### 3.1 `gemini-4-neon-{medium,high}` vs. `gemini-pro-orchestrator-neon` & `Carbon` Drift

Across the `30`-day model breakdown (`hq obs tokens --by=model --since=30d`), the three Gemini 4 families and host orchestrator meta-models exhibit starkly different cost and cache profiles:

| Executed Model ID | Convs | Turns | Uncached | Gross | Pooled CHR % | Rates (`In / Cache / Out` per `1M`) | Empirical Role Observation |
| :--- | ---: | ---: | ---: | ---: | ---: | :---: | :--- |
| `gemini-4-hydrogen-low` | `94` | `99` | `2.91M` | `8.17M` | `65.1%` | `$0.10 / $0.025 / $0.40` | `31k` avg uncached/run (`$0.005`/scout) — ideal Wave-1 scout. |
| `gemini-4-hydrogen-medium` | `185` | `663` | `34.77M` | `263.69M` | `87.1%` | `$0.25 / $0.0625 / $1.50` | Fast `[HQ] Front Desk` & routine digest synthesis. |
| **`gemini-4-neon-medium`** | **`14`** | **`94`** | **`6.44M`** | **`136.08M`** | **`95.9%`** | **`$0.50 / $0.125 / $3.00`** | **Highest prefix CHR in corpus; crisp single-slice builder / light coordinator.** |
| **`gemini-4-neon-high`** | **`21`** | **`219`** | **`28.97M`** | **`318.50M`** | **`91.4%`** | **`$0.50 / $0.125 / $3.00`** | **Workhorse for all 4 flagship Phase-2 multi-file builds (`0` deep-cap burn).** |
| `gemini-pro-orchestrator-neon` + `gemini-flash-worker-agy` | `5` | `135` | `16.70M` | `116.09M` | `69.2%` / `86.8%` | Misbilled as `$1.25` (`pro` bug!) | Host alternating planner/worker busts KV cache (`265k–309k` uncached per switch in `tk-audit` `T#32..T#49`). |
| `gemini-4-carbon-high` | `73` | `331` | `46.80M` | `597.14M` | `92.8%` | `$1.25 / $0.3125 / $10.00` | `2.5×–3.3×` pricier + `30M` daily deep cap; best reserved for Phase 1 RFCs & `KC-1` / Ring-0 security audits. |

**Substring-Ordering Bug Across 3 Packages:** Because `gemini-pro-orchestrator-neon` contains both `"pro"` and `"neon"`, checking `"pro"` before `"neon"` in [`obs.go:308,323,358`](../../internal/obs/obs.go), [`routing.go:91,501`](../../internal/routing/routing.go), and [`ls_client.go:651`](../../internal/kernel/ls_client.go) misclassifies it into `Family: "carbon"`, `CanonicalTier: "deep"` (eating `DailyDeepUncachedBudget`), `$1.25/$10.00` Carbon rates, and `gemini-4-carbon-high` live LS fallback! Matching `neon` **before** generic `pro` fixes all three packages cleanly, and adding `gemini-pro-orchestrator*` handling to `ls_client.go` keeps live `GetCascadeModelConfigData` resolution deterministic.

### 3.2 Ground-Truth Audit: Native `invoke_subagent(Model=...)` vs. `hq subagent`

Joining parent `spawnEdges` (`edge.Model`) against child `journal` `ModelName` post-`2026-11-19` exposes a critical mismatch between `routing.go`'s `SubagentModel` hints and Antigravity's native `invoke_subagent` enum (`inherit | flash_lite | flash | pro`):

| Caller Context & Intention | Native `invoke_subagent(Model=...)` | Actual Executed Model in Host LS | `hq subagent` (`StartCascade` RPC) | Parity Verdict Today |
| :--- | :--- | :--- | :--- | :--- |
| **Wave-1 Scout (`quick`)** | `Model="flash_lite"` (`12×`) | `gemini-3.5-flash-lite` *(legacy 3.5!)* | `--tier=quick` / `--model=flash_lite` $\rightarrow$ **`gemini-4-hydrogen-low`** (`M527`) | **Drifted** (native runs 3.5; `hq subagent` runs 4-Hydrogen-Low). |
| **Builder (`standard` / `heavy`) from `Hydrogen` parent** | `Model="flash"` (`16×`) | `gemini-4-hydrogen-tiered-high` *(not Neon!)* | `--tier=standard` $\rightarrow$ **`gemini-4-neon-medium`** (`M533`); `--model=flash` $\rightarrow$ `gemini-4-hydrogen-high`! | **Broken** (`routing.go:208` contradicts `--tier=standard`, and native `flash` never reaches `Neon`). |
| **Builder (`standard` / `heavy`) from `Neon` Coordinator** | `Model="flash"` (`2×`) vs `Model="inherit"` (`6×`) | `"flash"` **downgrades to `hydrogen-tiered-high`**; `"inherit"` **stays `gemini-4-neon-high`!** | `--tier=standard\|heavy` $\rightarrow$ **`gemini-4-neon-{medium,high}`**; `--model=inherit` **errors!** | **Inverted** (`routing.go` recommends `"flash"` to Neon parents, causing silent downgrade; `hq subagent` rejects `"inherit"`). |
| **Challenger (`deep` / cross-family)** | `Model="pro"` (`4×`) | `pro` / `hydrogen-tiered-xhigh` | `--model=carbon\|opus` $\rightarrow$ **`gemini-4-carbon-high` (`M420`) / `claude-opus-5-5-high` (`M402`)** | **`hq subagent` required** for exact `Carbon` / `Opus` / worktree binding. |

---

## 4. Audit of Budget Cliff, `SpawnSubagent` `429` Gap, Context Watermarks & CI Headrooms

1. **Passive `0%..99%` Budget Window (`internal/obs/anomaly.go`):** `EvaluateAnomalies` emits `A5` `WARN` at `80%` of `DailyUncachedBudget` (`40M / 50M`) and `DailyDeepUncachedBudget` (`24M / 30M`), and `SyncDailyBudgetCircuitBreaker` opens `QuotaState.CircuitOpen` at `100%`—which only halts unattended `kind: auto` spawns (`spawn.go:305`). Neither `routing.Resolve` nor `swe-pulse` reads live budget utilization to step down `neon-high` $\rightarrow$ `neon-medium` $\rightarrow$ `hydrogen-medium` or cap parallel builder fan-out when burn crosses `80%` / `95%`.
2. **Missing `429` Quota Step-Down in `SpawnSubagent` (`internal/kernel/spawn.go:484`):** `spawnTaskViaLS` (`spawn.go:212`) retries HTTP `429` (`RESOURCE_EXHAUSTED`) once on `resolveRoutineFallbackModel` (`gemini-4-hydrogen-medium`), skipping `gemini-4-neon-medium` when `neon-high` is throttled. Worse, `SpawnSubagent` (`spawn.go:484`) performs **zero `429` fallback**, aborting a coordinator's Wave 2 immediately on transient model rate limits.
3. **Single `120k` Context Nudge on `512k` Coordinators (`internal/hooks/pre_tool.go:294`):** `PreCompactionFlushTokens = 120,000` fires once per `2h` and goes silent from `121k` to `512k`. Across `30d` telemetry, top `[Task:*]` coordinators (`tk-mem-v2`, `tk-console`, `tk-audit`, `tk-muse`, `tk-rsi-v2`) accumulated `6–26` cold-cache wakeups (`A2`) at `250k–400k` context, burning **`2.33M–7.20M` uncached cold-start tokens each**.
4. **Harness & Monolith LOC Ceilings (`internal/ci/harness.go`, `internal/ci/slop.go`):**
   - **Gate `H1` (`SKILL.md` Cap):** `rsi.MaxSkillMarkdownLines = 150` (`templates/skills/swe-pulse/SKILL.md` is `32` lines; keeping it `<= 95` lines leaves `55+` lines of Ring-2 RSI headroom in `<vault>/skills/swe-pulse/SKILL.md`). Tracked `templates/skills/` must contain `0` `rsi:rule` blocks.
   - **Gate `H5-F` (Monolith LOC Ceilings):** `internal/hooks/hooks.go` is at **`405 / 415` LOC (`10` LOC headroom!)**, `internal/kernel/workspace_lifecycle.go` is `1,056 / 1,075` (`19` LOC), `cmd/hq/main.go` is `573 / 615` (`42` LOC), and `internal/kernel/kernel.go` is `1,949 / 2,015` (`66` LOC). Phase 2 code must place new coordinator/fence/throttling logic in dedicated sibling files (`internal/routing/budget_throttle.go`, `internal/kernel/orchestrator.go`, `internal/hooks/orchestrator_hooks.go`, and `cmd/hq/tasks_cli.go`) so all five monolith ratchets stay untouched.

---

## 5. Expected Quantitative Impact of Neon Orchestrator Mode ([RFC-0031](../design/0031-neon-orchestrator-mode.md))

| Dimension | Generic `/teamwork` Baseline (`Round 3` Audit) | Ad-Hoc Single-Thread `Carbon` Task | Proposed **Neon Orchestrator Mode (`RFC-0031`)** |
| :--- | :--- | :--- | :--- |
| **Coordinator Model & Deep-Cap Impact** | `hydrogen-tiered-high` (`0` deep, weak synthesis) | `gemini-4-carbon-high` (**`100%` counts against `30M` deep cap**) | **`gemini-4-neon-high` / `medium`** (`91.4%–95.9%` CHR, **`0` Carbon deep burn**) |
| **Subagents per 4-Slice Feature/Sweep** | `85` (`10–19` per milestone + sentinel) | `0–2` (monolithic `350k+` context) | **`6–8`** (`2–3` `hydrogen-low` scouts + `3–4` `neon` worktree builders + `1` fresh challenger) |
| **Status Polling Turns** | `76+` (`10m` cron wakeups + `200k` cold spikes) | `0` | **`0`** (`100%` reactive `send_message` wakeup + wave-boundary `hq progress`) |
| **Workspace Parallelism & Edit Safety** | `1` shared checkout (strictly sequential `3h10m`) | `1` worktree (serial edits) | **Parallel `<vault>/worktrees/<repo>-<slug>-w2-*`** + mechanical `PreToolUse` & `git diff` path fences |
| **Estimated Uncached Burn (4-Slice Sweep)** | `22.81M` (`$35.88`, `329.62M` gross) | `6.0M–10.1M` (`$15–$35` on Carbon) | **`~2.5M–4.0M` uncached (`~$2.20–$4.50`, `~6×–8×` lower burn, `~15–25m` wall-clock)** |
