# RFC-0031: Model-Agnostic Orchestrator Mode — `2 × 3` Workload Routing (`/boost` · `/swarm` · `/teamwork`), Future-Proof Model Catalog, Subagent Parity & Dynamic Throttling

- **Status:** Proposed (Phase 1 RFC — Rev 3, Awaiting User Review)
- **Created:** 2026-11-27
- **Builds On:** [ADR-0001 (Hybrid Option 1.5)](../decisions/0001-option-1.5-hybrid-on-antigravity.md), [RFC-0009 (Scalable Task Board)](./0009-task-board-redesign.md), [RFC-0012 (Token Observability)](./0012-token-observability.md), [RFC-0018 (Progress Checkpoints)](./0018-always-on-agent-runtime-hardening.md), [RFC-0019 (Vault Skills)](./0019-two-tier-rsi-vault-overlay.md), [RFC-0025 (Pluggable Workspaces)](./0025-pluggable-workspace-providers.md), [RFC-0030 (Token Calibration)](./0030-console-ux-token-audit.md)
- **Research:** [26 — Empirical `2 × 3` Audit of `{Neon, Orchestrator} × {/swarm, /teamwork, /boost}`, Subagent Parity, Dynamic Throttling & Future-Model Compatibility](../research/26_neon_orchestrator_mode_audit.md)

---

## 1. One-Page Summary (TL;DR)

Auditing `656` Vault conversations across `{Gemini 4 Neon, Orchestrator (Neon)} × {/boost, /swarm, /teamwork}` alongside `agy-hq`'s model-routing history (`3` model-generation rollouts in `19` days) surfaced both **the right `2 × 3` workflow choices** and **six hardcoded model chokepoints** that currently require `11`-file Go/Skill edits whenever upstream rolls out new models. This RFC proposes **four durable, model-agnostic upgrades**:

| Problem Today | What We Propose |
| :--- | :--- |
| **1. Unclear `2 × 3` fit; Phase-2 builds leak onto `deep` quota, and stacking `Orchestrator` + plugins thrashes KV cache** | **Capability-Scoped `orchestrator` Tier + `2 × 3` Playbook:** Add model-agnostic logical tier **`orchestrator`** (`CanonicalTier: "standard"`, **`0` `deep`-budget burn**; default today: `gemini-4-neon-high`, `--depth=medium` $\rightarrow$ `neon-medium`) + `hq spawn --orchestrator` / `hq subagent --orchestrator`. Pair `/boost`, `/swarm`, and `/teamwork` with **direct homogeneous `orchestrator`-tier models**, keeping dual-model host `Orchestrator` for solo `0`-plugin sessions unless a future release passes a `>= 85%` handoff-CHR health check. |
| **2. Every upstream model rollout (`3.8` $\rightarrow$ `Hydrogen` $\rightarrow$ `Neon`) breaks hardcoded whitelists, `config.yaml` pins, and `obs` pricing** | **Zero-Hardcoding Model Catalog & `auto` Tier Binding:** Default `templates/config.yaml` tiers to **`model: auto`** (preserving explicit user pins while auto-advancing uncustomized Vaults across rollouts), cache live non-secret `GetCascadeModelConfigData` models in `<vault>/memory/models_catalog.json` (`hq models` CLI) so new family slugs (`--model=xenon`) resolve on **Day 0 with `0` Go edits**, and derive `obs` family/tier/rates + `429` ladders from the **`routing.tiers` graph** + `budget.json` pattern rules. |
| **3. `/swarm` workers leak orphan `<SYSTEM_MESSAGE>` tasks into `tasks.json`/`obs` and default helpers to heavy models** | **`/swarm` Lineage Binding + Tier-Referenced `swe-pulse` Playbook:** Strip `<SYSTEM_MESSAGE>` blocks in `SanitizeTaskTitle`, bind `<swarm_worker>` / `SWARM_DIR` sessions as `nested_subagent` children under their `@coordinator` task, and update `swe-pulse/SKILL.md` with **tier-referenced** (`quick` / `standard` / `orchestrator` / `heavy`) rules for Fast Path, `/boost`, `/swarm` (`workspace:"share"`), and headless 3-wave CLI runs (`0` hardcoded model names in `SKILL.md`!). |
| **4. Native `invoke_subagent(Model="flash")` downgrades children, `SpawnSubagent` lacks `429` retry, and budgets cliff at `100%`** | **Family-Agnostic Subagent Parity + Tier-Graph `429` & Budget/Context Throttling:** Rewrite `invoke_subagent` `Model="flash"` $\rightarrow$ `"inherit"` dynamically whenever `SameModelFamily(callerModel, resolvedTierModel)` (and `"inherit"` $\rightarrow$ `"flash"`/`"flash_lite"` when `TierRank(caller) > TierRank(target)`), support `hq subagent --model=inherit`, share the tier-graph `429` ladder across `SpawnTask` & `SpawnSubagent`, soft-throttle at `80%/95%` budget, and add `200k/300k/380k` coordinator context guards. |

---

## 2. Canonical `2 × 3` Workload Matrix & Rollout Compatibility Examples

| Workload Shape | Recommended Mode (Model-Agnostic Tier $\rightarrow$ Current Default) | Empirical Proof & Future Rollout Behavior |
| :--- | :--- | :--- |
| **`1–3` Known Files (Bugfix / Small PR)** | **Direct `standard` tier** *(today: `gemini-4-neon-medium`; `0` plugins)* — or solo host `Orchestrator` (`< 150k` ctx) | **`95.9%` CHR**, zero subagent overhead. Auto-adopts future `standard` default when `config.yaml` is `auto`. |
| **Hard Logic Bug, Race, Deep Research, or RFC Critique** | **Direct `orchestrator` tier + `/boost`** *(today: `gemini-4-neon-high` + `/boost`; `deep` tier only for Ring-0 `authz`/`taint` or `KC-1`)* | **`~490k` uncached (`~$0.82`, `~7m`, `3` subagents, `0` `deep` burn).** `/boost` children inherit the active `orchestrator` model automatically across future rollouts. |
| **Interactive Parallel Multi-File Build (`3–10` Slices)** | **Direct `orchestrator` tier + `/swarm`** *(inside `hq spawn --orchestrator --repo=.`)* | **`~4.0M` uncached (`~28m`, `96.2%` worker CHR) vs. `/teamwork`'s `22.81M` (`3h10m`).** `SessionStart` injects active `quick`/`standard`/`heavy` model IDs so `/swarm` pins `monitor`/`narrator`/scouts to `quick` and `workspace:"share"` authors + `verifier` to `standard`/`heavy` dynamically. |
| **Unattended Overnight Repo Overhaul (`100+` Files)** | **Direct `orchestrator` tier + `/teamwork`** *(inside an isolated `hq worktree`)* | Full interview + milestone sub-orchestrators + Victory Audit when walk-away autonomy justifies `20M+` tokens. |
| **Headless / CLI / `[Auto]` Multi-Slice Pulse** | **`hq spawn --orchestrator` + 3-Wave `hq subagent --tier=...`** | Wave 1 `--tier=quick` scouts $\rightarrow$ `hq progress save` $\rightarrow$ Wave 2 parallel `--repo` `--tier=standard` builders $\rightarrow$ Wave 3 `hq ci pre-push` + `1` `--tier=heavy` challenger. |

### Example: Zero-Code-Change Rollout When Next-Gen Models Ship
- **Today:** Shipping `gemini-4-neon` (`1ca6e59`) required `280` LOC across `11` files—and left concrete `gemini-4-*` strings inside user `<vault>/config.yaml` files that silently block future built-in upgrades (`routing.go:603`).
- **With RFC-0031:** Suppose Antigravity rolls out `gemini-5-xenon-{low,medium,high}` next month (or ships `Orchestrator v2` with shared KV prefix caching):
  1. `GetCascadeModelConfigData` populates `<vault>/memory/models_catalog.json` automatically; `hq spawn --model=xenon --depth=high` works **immediately on Day 0** via catalog + structural `<vendor>-<gen>-<family>-<depth>` matching (`0` Go edits).
  2. Setting `orchestrator: gemini-5-xenon-high` (or `gemini-5-orchestrator` once `hq models` verifies handoff CHR $\ge 85\%$) in `<vault>/config.yaml` updates **`hq spawn --orchestrator`, `SessionStart` `/swarm` model hints, `PreToolUse` `invoke_subagent` family parity, `429` step-down ladders, and `obs` pricing/family charts in 1 line—with `0` `SKILL.md` or binary changes**.

---

## 3. Pillar 1: Model-Agnostic `orchestrator` Tier & Self-Updating Live Model Catalog

1. **Capability-Scoped Tier Graph (`internal/routing/routing.go`, `templates/config.yaml`, `cmd/hq/tasks_cli.go`):**
   - Add canonical logical tier **`orchestrator`** (aliases: `coordinator`, `coord`) between `standard` and `heavy`/`deep`:
     $$\texttt{quick} \prec \texttt{routine} \prec \texttt{standard} \prec \texttt{orchestrator} \equiv \texttt{heavy} \equiv \texttt{deep\_routine} \prec \texttt{deep}$$
   - Built-in defaults today: `orchestrator` = `gemini-4-neon-high` (`--depth=medium` $\rightarrow$ `gemini-4-neon-medium`), `CanonicalTier: "standard"` (**`0` `DailyDeepUncachedBudget` burn**). Move `deep_routine` (`Neon-High`) to `CanonicalTier: "standard"` so only true `deep`-tier models consume the `30M` `deep` cap.
   - Expose `--orchestrator` on `hq spawn` and `hq subagent` (`ActiveTask.LogicalTier = "orchestrator"`), and evaluate `reOrchestratorTask` (`phase\s*2\s+implement\w*|orchestrat\w*|multi[- ]?wave|swe[- ]?pulse|parallel\s+worktree\w*`) ahead of `reDeepTask`.
2. **`model: auto` Default & Live LS Catalog Cache (`internal/kernel/ls_client.go`, `internal/routing/catalog.go`):**
   - Ship `templates/config.yaml` with `model: auto` across `routing.tiers` (documenting current concrete defaults in comments), and treat unmodified `KnownHistoricalBuiltinDefaults` (`gemini-3.5-flash-lite`, `gemini-3.8-flash-*`, `gemini-4-argon-*`, and unmodified `v1` template defaults when retired from live LS) as `auto` so uncustomized Vaults advance cleanly across future model generations while explicit user/pack pins win unconditionally.
   - Persist a non-secret `<vault>/memory/models_catalog.json` (`store.WriteAtomic`) whenever `GetCascadeModelConfigData` runs, and add **`hq models [--json]`** listing live LS models, active tier bindings (`quick`..`deep`), retired-pin warnings, and `7d` CHR health (flagging any dual-backend `orchestrator` model with handoff CHR `< 75%`).
   - Replace `isMultiDepthBase` and `rePromptModelHint` whitelists with **live catalog lookup + structural family/depth parsing** (`<prefix>-<family>-(low|medium|high|xhigh)`), enabling Day-0 short aliases (`--model=<new-family>`) without Go edits.
3. **Config-Derived Pricing & Family Extraction (`internal/obs/obs.go`, `dag.go`, `console.html`):**
   - Fix `"neon"`-before-`"pro"` ordering + `gemini-flash-worker-agy` (`hydrogen` rates), replace hardcoded `gemini-4-*` strings in `dag.go` with `routing.Resolve`, support optional `modelRates` pattern overrides in `<vault>/memory/obs/budget.json`, inherit tier/rates automatically for any custom model referenced in `routing.tiers`, and extract normalized family slugs dynamically so future families render cleanly in `:4242` without falling into `"other"`.

---

## 4. Pillar 2: `/swarm` Lineage Hygiene & Tier-Referenced `swe-pulse` Playbook

1. **`/swarm` Worker Lineage & `<SYSTEM_MESSAGE>` Title Strip (`internal/hooks/`, `internal/kernel/`, `internal/obs/dag.go`):**
   - Strip `<SYSTEM_MESSAGE>...</SYSTEM_MESSAGE>` blocks in `SanitizeTaskTitle` so system-injected worker headers never become task titles.
   - Detect `<swarm_worker>` / `.system_generated/swarm` / LS `SourceMetadata` in `detectNestedDescendant` (`session_start.go`, `stop.go`) and `obs/dag.go`: classify `/swarm` workers (`author`, `verifier`, `narrator`, `monitor`) as `nested_subagent` children (`leanAuto = true`, barred from `claim-desk`) linked to their `@coordinator` UUID via `SWARM_DIR` (`.../brain/<coordinator-id>/.system_generated/swarm/`) or active spawner state.
2. **Model-Agnostic 4-Mode `swe-pulse` Contract (`templates/skills/swe-pulse/SKILL.md` `<= 95 / 150` lines, `0` hardcoded model IDs):**
   - In `HandleSessionStart`, inject a 1-line **Active Tier Map** (`quick=<id> · standard=<id> · orchestrator=<id> · heavy=<id> · deep=<id>`) on non-Front-Desk coding/orchestrator sessions (`~120` bytes) so skills and `/swarm` prompts always know the live model IDs without hardcoding them in Markdown:
     1. **Mode 1 — Fast Path (`<= 3` files):** Single `hq worktree add`, direct `standard`-tier edits + tests.
     2. **Mode 2 — Hard Bug / RFC Critique (`/boost`):** Run `/boost` on a direct `orchestrator`-tier session (`WorkerL0` + `ImprovementWorker`).
     3. **Mode 3 — Interactive Parallel Slices (`/swarm`):** Launch inside `hq spawn --orchestrator --repo=<repo> --task=<slug>`; pin `monitor`/`narrator`/scouts to the active `quick` model, `workspace:"share"` authors (with disjoint owned paths) to `standard`, and `1` `verifier` per `2–4` authors to `heavy`/`orchestrator`; merge into the task worktree before `swarm{action:"kill"}` and gate `main` merge on `./bin/hq ci pre-push`.
     4. **Mode 4 — Headless 3-Wave CLI (No UI Slash Command):** Wave 1 `hq subagent --tier=quick` scouts $\rightarrow$ `hq progress save` $\rightarrow$ Wave 2 parallel `hq subagent --repo=<repo> --task=<slug>-w2-<slice> --tier=standard` builders $\rightarrow$ `hq progress save` $\rightarrow$ Wave 3 `./bin/hq ci pre-push` + `1` fresh `hq subagent --tier=heavy` challenger (`0` polling crons).

---

## 5. Pillar 3: Family-Agnostic Subagent Parity & Tier-Graph Throttling

1. **Dynamic Subagent Parity (`internal/routing/`, `internal/kernel/spawn.go`, `internal/hooks/orchestrator_hooks.go`):**
   - Support `hq subagent --model=inherit` (inheriting parent model from `tasks.json` / journal / LS lineage) and align coarse `--model=flash` with `standard` (`routine` when `--tier=routine`).
   - In `PreToolUse` (`invoke_subagent`, `TypeName != "browser"`): compare `SameModelFamily(payload.ModelName, resolvedTier.ModelID)`—rewriting `Model="flash"` $\rightarrow$ **`"inherit"`** whenever the caller already runs on the target tier's model family (preserving `Neon` today and any future `Xenon` tomorrow!), and rewriting `"inherit"` $\rightarrow$ `resolvedTier.SubagentModel` when `TierRank(caller) > TierRank(resolvedTier)` (`deep` parent spawning `[quick|routine|standard]` worker).
2. **Tier-Graph `429` Step-Down, `80%/95%` Budget Backpressure & `200k/300k/380k` Context Guards:**
   - **Tier-Graph `429` & Fallback Ladder (`spawn.go`, `ls_client.go`):** Replace hardcoded model slices in **both** `SpawnTask` and `SpawnSubagent` with `ResolveStepDownLadder(vaultDir, requestedTierOrModel)` traversing `deep -> orchestrator -> standard -> fallback_on_429 (routine) -> quick` filtered against live `GetCascadeModelConfigData`.
   - **`80%/95%` Daily Budget Soft-Throttle (`internal/routing/budget_throttle.go`):** Step non-forced, non-explicit tiers down 1 rung along the tier graph at `>= 80%` daily uncached (`>= 80%` deep cap steps `deep` subagents to `orchestrator`) and to `routine` at `>= 95%`.
   - **Coordinator Context Watermarks (`internal/hooks/orchestrator_hooks.go`):** Keep Front Desk at `120k/160k/180k`; add watermark-keyed stages resetting on `CHECKPOINT`: **`200k`** (prefer diffs/summaries), **`300k`** (auto-save `hq progress save` on `Stop` + wave-boundary `/compact` prompt), and **`380k`** (`LogicalTier == "orchestrator"` `3`-lookup/turn cap until `/compact` or delegation; regular coding tasks stay `100%` unrestricted).

---

## 6. Guardrails, Implementation Phases & Open Review Questions

- **LOC & Hygiene Invariants (`Gate H1`, `H4-A`, `H5-F`):** `0` external Go deps; new logic lives in `internal/routing/{catalog,budget_throttle}.go`, `internal/kernel/orchestrator.go`, `internal/hooks/orchestrator_hooks.go`, and `cmd/hq/tasks_cli.go` so `hooks.go` (`405/415`), `main.go` (`573/615`), `workspace_lifecycle.go` (`1,056/1,075`), and `kernel.go` (`1,949/2,015`) remain strictly below `Gate H5-F` ceilings.

| Phase | Scope | Verification |
| :--- | :--- | :--- |
| **Phase 2A (`routing`, `obs`, `ls_client`)** | `orchestrator` tier, `model: auto` + live `models_catalog.json` + structural family parser + `hq models`, tier-derived `obs` pricing/family + `"neon"`-before-`"pro"` fix, & `80%/95%` tier-graph budget throttle. | Unit tests proving a synthetic future model (`gemini-5-xenon-{low,medium,high}`) works across routing, fallback, and `obs` with `0` code changes + `Gate H3`. |
| **Phase 2B (`kernel`, `hooks`, `obs/dag`, `cmd/hq`)** | `--orchestrator` CLI flag, `<swarm_worker>`/`SWARM_DIR` lineage & `<SYSTEM_MESSAGE>` strip, tier-graph `429` ladder in `SpawnTask`/`SpawnSubagent`, family-equivalence `invoke_subagent` bridge, `SessionStart` tier map, & `200k/300k/380k` watermarks. | Unit tests for `/swarm` parent binding, future-family `invoke_subagent` parity, `429` step-down, & post-`CHECKPOINT` watermark reset. |
| **Phase 2C (`swe-pulse`, `AGENTS.md`, `CUJ-50`)** | Model-agnostic 4-mode `templates/skills/swe-pulse/SKILL.md` (`hq skill sync`), `AGENTS.md` capability-tier rules, & `0003-critical-user-journeys.md` (`CUJ-50`). | **`TestCUJ50_ModelAgnosticOrchestratorSwarmLineageAndFutureModelRollout`** E2E test + `100%` `./bin/hq ci pre-push` & `go test -race ./...`. |

### Open Review Questions for Sign-Off
1. **`model: auto` Default in `templates/config.yaml`:** Confirm switching `templates/config.yaml` `routing.tiers` to `model: auto` (with concrete overrides documented in comments and unmodified legacy template strings treated as `auto` when retired upstream) so future model rollouts upgrade unpinned Vaults automatically while honoring explicit user pins?
2. **`2 × 3` Default Policy + Empirical `Orchestrator` Gate:** Confirm adopting **direct `orchestrator`-tier + `/boost`** and **direct `orchestrator`-tier + `/swarm`** (`workspace:"share"` + dynamic `quick`/`standard` pinning) as default multi-agent modes today, with `hq models` tracking rolling handoff CHR so we can flip `routing.tiers.orchestrator` to a future host `Orchestrator v2` in 1 config line if upstream adds shared KV caching?
3. **`380k` Coordinator Context Guard:** Above `380k` context on `--orchestrator` threads, do you prefer the **`3`-lookup/turn mechanical `PreToolUse` cap** (forcing `/compact` or delegation) or a **non-blocking `PreToolUse` warning nudge**?
