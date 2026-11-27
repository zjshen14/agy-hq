# 26 — Empirical `2 × 3` Audit of `{Neon, Orchestrator} × {/swarm, /teamwork, /boost}`, Subagent Parity, Dynamic Throttling & Future-Model Compatibility

- **Date:** 2026-11-27 (Rev 3 — Full `2 × 3` Matrix + Future-Model Rollout Architecture)
- **Task Scope:** `[Task:agy-hq] Neon Orchestrator Mode RFC` (`hq/neon-orchestrator-mode`)
- **Companion Specification:** [RFC-0031: Model-Agnostic Orchestrator Mode — `2 × 3` Workload Routing, Future-Proof Model Catalog, Subagent Parity & Dynamic Throttling](../design/0031-neon-orchestrator-mode.md)
- **Related Documents:** [08 — Empirical Token Telemetry Audit](08_token_observability_and_telemetry.md), [25 — Forensic Audit of Token Math & Console UX](25_console_ux_and_token_observability_audit.md), [ADR-0001 (Hybrid Option 1.5)](../decisions/0001-option-1.5-hybrid-on-antigravity.md), [RFC-0009 (Scalable Task Board)](../design/0009-task-board-redesign.md), [RFC-0012 (Token Observability)](../design/0012-token-observability.md), [RFC-0018 (Progress Checkpoints)](../design/0018-always-on-agent-runtime-hardening.md), [RFC-0025 (Pluggable Workspaces)](../design/0025-pluggable-workspace-providers.md), [RFC-0030 (Token Calibration)](../design/0030-console-ux-token-audit.md)

---

## 1. Executive Summary

Antigravity's rapid Gemini rollout (`3.5-flash-lite` $\rightarrow$ `3.8-flash` $\rightarrow$ `4-argon` $\rightarrow$ `4-hydrogen` / `4-carbon` $\rightarrow$ `4-neon` + `Orchestrator` in just three weeks) poses two linked architectural questions for `agy-hq`:
1. **Today's `2 × 3` Choice:** Between **direct single-family models** (`Gemini 4 Neon`) and **host dual-model `Orchestrator`** (`gemini-pro-orchestrator-neon`), paired across Antigravity's three multi-agent harnesses (**`/boost`**, **`/swarm`**, **`/teamwork`**), which combinations fit which workloads?
2. **Continuous Rollout Resilience:** How do we design `agy-hq`'s routing, skills, observability, and subagent bridges so **next month's model families, generation bumps (`gemini-4.5` / `5`), or next-gen host `Orchestrator` upgrades work on Day 0 without multi-file Go/Skill string surgery**?

Auditing `656` Vault conversations (`3,060` turns, `308.44M` uncached / `3.19B` gross tokens), `.agents/teamwork/` (`87` folders), builtin `/swarm` skills, and every model string across `internal/` & `cmd/` yields **four core findings**:

1. **Double-Orchestration Physics (`Orchestrator (Neon)` + Any Multi-Agent Plugin Loses Today, for Measurable Reasons):**
   - Current `Orchestrator (Neon)` (`gemini-pro-orchestrator-neon`) alternates planner turns with `gemini-flash-worker-agy` inside **one** transcript. Stacking `/boost`, `/swarm`, or `/teamwork` on top caused: (a) **KV prefix-cache ping-pong** (`250k–309k` uncached tokens per switch; **`69.2%` CHR** vs. **`91.4%–95.9%`** on direct `Neon`), (b) coordinator merges executing on `flash-worker-agy` (breaking Go 1.24 CI in Round 2 `/swarm`), and (c) child agents inheriting `hydrogen-tiered-high` (`85/85` agents in Round 3 `/teamwork`).
   - Because future host `Orchestrator` releases could add shared KV caching and planner-preserved inheritance, `agy-hq` should treat this as an **empirical capability check** (rolling handoff CHR $\ge 85\%$ + preserved child inheritance) rather than permanently banning `"orchestrator"` strings.
2. **Winning `2 × 3` Playbook (Expressed Over Capability Tiers, Not Hardcoded Model Names):**
   - **Hard Bugs, Deep Research & RFC Critique $\rightarrow$ Direct `orchestrator`/`heavy`-tier model (`Neon-High` today) + `/boost`** (`~490k` uncached / `~$0.82` / `~7m`, `3` subagents, `0` `deep`-cap burn).
   - **Parallel Multi-File Features & `3–10`-Slice Refactors $\rightarrow$ Direct `orchestrator`-tier model + `/swarm`** (`workspace:"share"`, `~4.0M` uncached / `~28m` vs. `/teamwork`'s `22.81M` / `3h10m`), pinning `monitor`/`narrator`/scouts to the active `quick` tier and authors/verifiers to `standard`/`heavy`.
   - **Unattended Overnight `100+`-File Sweeps $\rightarrow$ Direct `orchestrator`-tier model + `/teamwork`** inside an isolated `hq worktree`.
   - **Fast Path (`1–3` files) $\rightarrow$ Direct `standard`-tier model** (`95.9%` CHR, `0` plugins) or solo host `Orchestrator` (`< 150k` ctx).
3. **Four Live `agy-hq` Integration Bugs Today:** (a) `/swarm` workers (`<swarm_worker>`) leak into `tasks.json` and `hq obs` as orphan `[Task:general] The following is a <SYSTEM_MESSAGE>...` roots (`Parent: ""`); (b) `"pro"`-before-`"neon"` ordering misbills `gemini-pro-orchestrator-neon` as `Carbon` (`deep`); (c) `reDeepTask` hijacks `80.4%` of Phase-2 implementation turns onto `Carbon`; and (d) native `invoke_subagent(Model="flash")` downgrades `Neon` children to `Hydrogen`, while `SpawnSubagent` lacks HTTP `429` retry.
4. **Six Hardcoded Model Chokepoints Blocking Future Rollouts (Audited Across `37` Packages):** Commit `1ca6e59` (adding `Neon`) required editing **`11` files (`280` LOC)** because `agy-hq` hardcodes point-in-time codenames in six places (§4 below).

---

## 2. Empirical `2 × 3` Matrix Audit (`tk-audit` Round 2 `/swarm` & Round 3 `/teamwork`, `tk-rsi-v2` `/boost`)

| Combo (`Base Model × Harness`) | Live Vault Evidence (`2026-11-19` – `2026-11-26`) | Agents | Uncached Burn | Wall-Clock & CHR | Verdict |
| :--- | :--- | ---: | ---: | :---: | :---: |
| **`Direct Neon` + `/boost`** | `tk-rsi-v2`: `DeepInvestigator` $\rightarrow$ `WorkerL0` + `ImprovementWorker` (`2` runs). | `3` / boost | **`486k–491k`** (`~$0.82`) | `~7m` · `90.3%` CHR | **🏆 Tier-1 (`Hard Logic / Research / RFCs`)** |
| **`Direct Neon` + `/swarm`** | Worker profile (`sw-worker-1..6`, `workspace:"share"`, `swarm_wait`) + `Neon` coord (`91.4%` CHR). | `4–8` | **`~4.0M`** (`~$5–$6`) | `~28m` · `96.2%` CHR | **🏆 Tier-1 (`Parallel 3–10 Slice Builds`)** |
| **`Direct Neon` + `/teamwork`** | Avoids `30` parent `flash-worker` wakeups & `Hydrogen` downgrade, but keeps `10`-agent/iter serial loop. | `~85` | `~20M+` | `2–3h+` (1 checkout) | **🟡 Niche (`Overnight 100+ File Sweeps`)** |
| **`Orchestrator (Neon)` + `/boost`** | Intra-turn `pro-orchestrator-neon` $\leftrightarrow$ `flash-worker-agy` switching busts `300k` cache. | `3` / boost | `+250k/switch` | `~69%` coord CHR | **❌ Avoid (Double Orchestration)** |
| **`Orchestrator (Neon)` + `/swarm`** | Round 2 (`tk-audit` `T#15..35`): coord ran on `flash-worker-agy`, broke Go 1.24 CI (`8d0db20`), leaked `6` orphan tasks. | `6` + coord | `3.97M` + coord spikes | `~28m` · `49–69%` switches | **❌ Avoid (Coord Downgrade + Cache Thrash)** |
| **`Orchestrator (Neon)` + `/teamwork`** | Round 3 (`tk-audit` $\rightarrow$ `sub-teamwork-sentinel`): downgraded all `85` agents to `hydrogen-tiered-high` + `87` repo folders. | `85` | **`24.6M+`** (`~$38`) | **`3h10m`** · `76+` cron turns | **❌ Avoid (Worst Burn in Corpus)** |

---

## 3. Native `invoke_subagent(Model=...)` vs. `hq subagent` Ground-Truth Audit (`290` Edges)

- Native `invoke_subagent` accepts only `inherit | flash_lite | flash | pro`. In live journal telemetry, **`Model="flash"` always executes `gemini-4-hydrogen-tiered-high`** (even when the parent is `gemini-4-neon-high`), `Model="flash_lite"` executes `gemini-3.5-flash-lite`, and **`Model="inherit"`** is the only enum that preserves a parent's `standard`/`orchestrator`/`heavy` family.
- Meanwhile, `hq subagent` calls `LanguageServerService/StartCascade` with exact `GetCascadeModelConfigData` enums, but [`routing.go:208`](../../internal/routing/routing.go) maps `--model=flash` to `hydrogen-high` (contradicting `--tier=standard`) and rejects `--model=inherit`.

---

## 4. Six Hardcoded Model Chokepoints Auditing Future Rollout Compatibility

Grep-auditing all non-test Go, HTML, YAML, and Markdown files surfaced **six places where a new model rollout (e.g., `gemini-5-xenon-*`, `claude-opus-6-*`, or `Orchestrator v2`) currently forces manual code changes**:

| Chokepoint in Codebase Today | Exact Failure When a New Model Ships | Future-Proof Mechanism in [RFC-0031](../design/0031-neon-orchestrator-mode.md) |
| :--- | :--- | :--- |
| **1. Concrete pins in [`templates/config.yaml`](../../templates/config.yaml) + [`routing.go:603`](../../internal/routing/routing.go)** | `hq init` writes concrete `gemini-4-*` strings into `<vault>/config.yaml`; when built-in defaults later advance, `rawVaultModel != builtinDefault` mistakes old template strings for intentional user pins! | Support `model: auto` in `templates/config.yaml` (defaulting to built-in + live LS catalog) and auto-upgrade unmodified `KnownHistoricalBuiltinDefaults` in `routing.go` / `hq migrate`. |
| **2. Closed whitelists `isMultiDepthBase` & `rePromptModelHint` ([`routing.go:21,191`](../../internal/routing/routing.go))** | `--model=xenon` or `--model=gemini-5-xenon` (without `-high`) fails to append depth or match live LS configs until added to Go `switch` cases. | Structural `<vendor>-<gen>-<family>[-<depth>]` parser + passive non-secret **Live LS Model Catalog cache** (`<vault>/memory/models_catalog.json` + `hq models` CLI) matching short family slugs (`xenon`) on Day 0. |
| **3. Hardcoded ladders in [`ls_client.go:654`](../../internal/kernel/ls_client.go) & [`spawn.go:212`](../../internal/kernel/spawn.go)** | `429` retry checks `!strings.Contains(usedModel, "hydrogen")` and `ls_client.go` hardcodes `gemini-4-*` candidate slices. | **Tier-Graph Step-Down Ladder** (`deep -> orchestrator -> standard -> routine -> quick`) resolved dynamically via `routing.Resolve(vaultDir, ...)` against live `clientModelConfigs`. |
| **4. Hardcoded `switch` rates & `console.html` family palette ([`obs.go:306`](../../internal/obs/obs.go), `dag.go:236`)** | Unknown models fall into generic `$0.30` / `"other"`, `dag.go` hardcodes `gemini-4-*` fallbacks, and `"pro"` precedes `"neon"`. | Derive tier/rates for configured models from `routing.tiers` + optional `budget.json` `modelRates` pattern rules + structural family extraction (`xenon`, `claude`, etc.) & dynamic palette. |
| **5. Model-literal checks in `PreToolUse` subagent bridge** | Checking `HasPrefix(model, "gemini-4-neon-")` breaks as soon as `orchestrator` / `standard` moves to a new model family. | **Family-Equivalence & Tier-Rank Comparison:** compare `SameModelFamily(callerModel, tierTarget.ModelID)` and `TierRank(callerModel) > TierRank(tierTarget.ModelID)` dynamically. |
| **6. Model names inside `SKILL.md` & `AGENTS.md` prose** | Hardcoding `gemini-4-hydrogen-low` / `gemini-4-neon-medium` in `swe-pulse` `/swarm` instructions goes stale on every rollout. | Skills reference **capability tiers only** (`quick`, `standard`, `orchestrator`, `heavy`, `deep`); `SessionStart` & `hq models` surface the live tier$\rightarrow$`modelId` map (`Gate H1` `<= 95/150` lines; `Gate H5-F` monoliths untouched). |
