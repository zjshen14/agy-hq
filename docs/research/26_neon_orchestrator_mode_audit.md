# 26 — Empirical `2 × 3` Audit of `{Gemini 4 Neon, Orchestrator (Neon)} × {/swarm, /teamwork, /boost}`, Subagent Parity & Dynamic Throttling in `agy-hq`

- **Date:** 2026-11-27 (Rev 2 — Full `2 × 3` Model × Harness Matrix Audit)
- **Task Scope:** `[Task:agy-hq] Neon Orchestrator Mode RFC` (`hq/neon-orchestrator-mode`)
- **Companion Specification:** [RFC-0031: Neon Orchestrator Mode — `2 × 3` Workload Routing, `/swarm` & `/boost` Integration, Subagent Parity & Dynamic Throttling](../design/0031-neon-orchestrator-mode.md)
- **Related Documents:** [08 — Empirical Token Telemetry Audit](08_token_observability_and_telemetry.md), [25 — Forensic Audit of Token Math & Console UX](25_console_ux_and_token_observability_audit.md), [ADR-0001 (Hybrid Option 1.5)](../decisions/0001-option-1.5-hybrid-on-antigravity.md), [RFC-0009 (Scalable Task Board)](../design/0009-task-board-redesign.md), [RFC-0012 (Token Observability)](../design/0012-token-observability.md), [RFC-0018 (Progress Checkpoints)](../design/0018-always-on-agent-runtime-hardening.md), [RFC-0025 (Pluggable Workspaces)](../design/0025-pluggable-workspace-providers.md), [RFC-0030 (Token Calibration)](../design/0030-console-ux-token-audit.md)

---

## 1. Executive Summary & The `2 × 3` Design Question

Antigravity's Gemini 4 stack gives `agy-hq` two Neon-tier base model options in the picker and CLI—**direct `Gemini 4 Neon`** (`gemini-4-neon-high` / `gemini-4-neon-medium`) vs. **host `Orchestrator (Neon)`** (`gemini-pro-orchestrator-neon`)—and three built-in multi-agent reasoning/execution plugins: **`/boost`**, **`/swarm`**, and **`/teamwork`** (alongside `agy-hq`'s own `swe-pulse` / `hq subagent` CLI). Auditing `656` live Vault conversations (`3,060` turns, `308.44M` uncached / `3.19B` gross tokens), builtin plugin specs (`native_swarm`, `swarm_monitor`, `swarm_narrator`), `.agents/teamwork/` (`87` directories), and real `agy-hq` production runs across `/swarm` (Round 2 cleanup), `/teamwork` (Round 3 compaction), and `/boost` (RSI v2 `570`-paper synthesis) yields **four decisive findings**:

1. **Never Stack `Orchestrator (Neon)` Under `/swarm`, `/teamwork`, or `/boost` (Double-Orchestration Penalty):**
   - `Orchestrator (Neon)` (`gemini-pro-orchestrator-neon`) is already an **intra-conversation** dual-model router that alternates planning turns (`gemini-pro-orchestrator-neon`) with fast executor turns (`gemini-flash-worker-agy`) inside a **single** transcript.
   - Pairing it with an **inter-conversation** multi-agent harness (`/swarm`, `/teamwork`, `/boost`) creates three measured regressions: (a) **KV prefix-cache ping-pong** (`249k–309k` uncached tokens burned on every planner↔worker switch; **`69.2%` CHR** vs. **`91.4%–95.9%` CHR** on direct `Gemini 4 Neon`), (b) **coordinator integration turns executing on `gemini-flash-worker-agy`** (which missed a Go 1.24 `gofmt -s` AST failure during Round 2 `/swarm`, breaking GitHub CI), and (c) **accidental child-model downgrade** (spawning `/teamwork` during a worker-routed turn caused all `85` `/teamwork` agents to inherit `gemini-4-hydrogen-tiered-high` instead of `Neon`!).
   - `Orchestrator (Neon)` fits **only solo single-thread sessions (`< 150k` context, `0` multi-agent plugins)** where hands-off planner→worker delegation saves manual model switching.
2. **Among the `2 × 3` Matrix, Two Direct-`Neon` Combinations Dominate `95%` of Complex `agy-hq` Work:**
   - **Direct `Gemini 4 Neon (High)` + `/boost`** (`~490k` uncached / `~$0.82` / `~7m` per boost, `3` subagents: `DeepInvestigator` $\rightarrow$ `WorkerL0` + `ImprovementWorker`) is the **clear winner for hard localized bugs, tricky edge cases, deep research, and RFC design verification**—delivering adversarial multi-perspective rigor at `100%` faithful `neon-high` inheritance and `0` `Carbon` (`deep`) quota burn.
   - **Direct `Gemini 4 Neon (High/Medium)` + `/swarm`** (`~4.0M` uncached / `~28m` across `6` parallel workers, `96.2%` worker CHR, native `workspace:"share"` isolation, `0`-token `swarm_wait`) is the **clear winner for interactive parallel multi-file features and `3–10`-slice refactors** (`5.7×` cheaper and `6.8×` faster than `/teamwork`), **provided** the prompt overrides `/swarm`'s homogeneous-model default (pinning `monitor`/`narrator`/scouts to `gemini-4-hydrogen-low` and authors/verifiers to `neon-medium`/`high`) and `agy-hq` fixes `/swarm` worker lineage.
   - **Direct `Gemini 4 Neon (High)` + `/teamwork`** (`85` agents, `22.81M` uncached, `$35.88`, `3h10m`) is a **niche tool strictly for unattended overnight `100+`-file repo overhauls** inside an isolated `hq worktree`—never for routine features or interactive sessions.
3. **Four Live `agy-hq` Kernel/Hook Bugs Break `/swarm`, `Orchestrator (Neon)` & Native Subagent Parity Today:**
   - **`/swarm` Worker Lineage Leak:** All `6` live `/swarm` workers (`sw-worker-1..6`) bypassed `invoke_subagent` and started with `<SYSTEM_MESSAGE>...<swarm_worker>`, leaking into `tasks.json` and `hq obs` as **orphan top-level `[Task:general] The following is a <SYSTEM_MESSAGE>...` rows** (`Parent: ""`, `0` linked journals).
   - **`"pro"`-Before-`"neon"` Billing & Fallback Bug:** [`obs.go:308,323,358`](../../internal/obs/obs.go), [`routing.go:91,501`](../../internal/routing/routing.go), and [`ls_client.go:651`](../../internal/kernel/ls_client.go) match `"pro"` before `"neon"`, misbilling `gemini-pro-orchestrator-neon` at Carbon `$1.25/$10.00` (`deep` budget!) and lacking `gemini-flash-worker-agy` classification.
   - **Phase-2 `reDeepTask` `Carbon` Bleed:** `80.4%` (`123 / 153`) of flagship engineering turns were Phase-2 builds initially routed to `gemini-4-carbon-high` because [`routing.go:16`](../../internal/routing/routing.go) matched `rfc|audit|design|complex` with no `orchestrator` tier.
   - **Native `invoke_subagent(Model="flash")` Downgrade & Missing `SpawnSubagent` `429` Ladder:** Across `290` live `spawnEdges`, native `Model="flash"` **always** executes `gemini-4-hydrogen-tiered-high` (even from a `Neon` parent; only `Model="inherit"` preserves `Neon`), `hq subagent --model=flash` contradicts `--tier=standard` ([`routing.go:208`](../../internal/routing/routing.go)), `--model=inherit` errors out, and [`SpawnSubagent`](../../internal/kernel/spawn.go) (`spawn.go:484`) has zero HTTP `429` step-down retry.

---

## 2. Empirical Audit of Axis 1: `Gemini 4 Neon` vs. `Orchestrator (Neon)`

| Model Option | Runtime Execution Mechanics | 30d Convs / Turns | Uncached / Gross | Pooled CHR % | Subagent Inheritance (`Model` omitted) | Best Fit |
| :--- | :--- | ---: | ---: | ---: | :--- | :--- |
| **`Gemini 4 Neon (Medium)`** (`gemini-4-neon-medium`) | Single model across all steps; explicit depth (`medium`). | `14` / `94` | `6.44M` / `136.08M` | **`95.9%`** | Children inherit `gemini-4-neon-medium` deterministically. | `1–3` file coding, `/swarm` slice authors, routine PR fixes. |
| **`Gemini 4 Neon (High)`** (`gemini-4-neon-high`) | Single model across all steps; explicit depth (`high`). | `21` / `219` | `28.97M` / `318.50M` | **`91.4%`** | Children inherit `gemini-4-neon-high` deterministically (`6/6` in `/boost`). | **Top-level `/boost` & `/swarm` coordinators**, complex builders, verifiers. |
| **`Orchestrator (Neon)`** (`gemini-pro-orchestrator-neon` + `gemini-flash-worker-agy`) | Host meta-model alternating `pro-orchestrator-neon` planner turns and `flash-worker-agy` tool turns in **1** transcript; locks manual depth. | `5` / `135` | `16.70M` / `116.09M` | **`69.2%`** *(planner)* / **`86.8%`** *(worker)* | **Unstable:** children spawned on worker-routed turns inherit `hydrogen-tiered-high` (`tk-audit` $\rightarrow$ `/teamwork`). | **Solo single-thread coding (`< 150k` ctx, NO `/swarm`/`/teamwork`/`/boost`).** |

### Forensic Proof of Intra-Transcript KV Cache Ping-Pong (`tk-audit`, Nov 19–22)
In `tk-audit` (`83` turns), once conversation context reached `~285k–309k` tokens, every internal handoff between `gemini-pro-orchestrator-neon` and `gemini-flash-worker-agy` invalidated or halved prefix cache hits across the two model backends:
- `T#32` (`gemini-pro-orchestrator-neon`): `285k` uncached (`0.0%` CHR) $\rightarrow$ `T#34` (`gemini-flash-worker-agy`): `265k` uncached (`49.5%` CHR) $\rightarrow$ `T#36` (`pro-orchestrator-neon`): `299k` uncached (`49.5%` CHR) $\rightarrow$ `T#40` (`flash-worker-agy`): `274k` uncached $\rightarrow$ `T#43` (`pro-orchestrator-neon`): `306k` uncached $\rightarrow$ `T#44`: `309k` uncached $\rightarrow$ `T#48` (`flash-worker-agy`): `298k` uncached.
- By contrast, consecutive `gemini-4-neon-high` turns at identical `285k–480k` context in `tk-console` (`T#16..T#23`, `T#29..T#36`) sustained **`98.4%–99.5%` CHR** (`3.0k–7.6k` uncached per turn—**`40×–90×` less uncached input per turn**).

---

## 3. Empirical Audit of Axis 2: `/boost` vs. `/swarm` vs. `/teamwork` on `agy-hq`

All three Antigravity harnesses were exercised on real `agy-hq` engineering tasks between `2026-11-19` and `2026-11-26`:

| Dimension | **`/boost`** *(Live: `tk-rsi-v2` Research & Spine)* | **`/swarm`** *(Live: `tk-audit` Round 2 Anti-Slop)* | **`/teamwork`** *(Live: `tk-audit` Round 3 Compaction)* |
| :--- | :--- | :--- | :--- |
| **Internal Topology** | Parent $\rightarrow$ `DeepInvestigator` (`3` turns) $\rightarrow$ `WorkerL0` + `ImprovementWorker` (`1` turn each). | Coordinator (`swarm`, `post_message`, `read_board`, `swarm_wait`) + named `authors`, `verifier` (`1:2..4`), optional `narrator` & `monitor` (`3m` stall cron). | `sentinel` $\rightarrow$ `interviewer` $\rightarrow$ `orchestrator` $\rightarrow$ `sub_orch_m1..m5` $\rightarrow$ `3 Explorers + 1 Worker + 2 Reviewers + 2 Challengers + 1 Auditor` + `victory_auditor`. |
| **Agents Spawned** | **`3` per `/boost`** (`6` across 2 boosts) | **`6` parallel workers** (+ coordinator) | **`85` subagents** (`10–19` per milestone + relays) |
| **Workspace Isolation** | Shares parent's single isolated `hq worktree` (read/synthesize in children; edit in parent). | **Built-in `workspace:"share"`** (git worktree per author) or `"branch"` clone; `inherit` for verifiers. | **None by default:** all `85` agents shared **1 checkout**, forcing sequential `M1`$\rightarrow$`M5` execution. |
| **Idle / Waiting Cost** | **`0` tokens** (one-shot reactive `send_message`). | **`0` tokens** (`swarm_wait` parks workers; only `monitor` ticks every `3m` until dormant). | **High:** `10m` polling crons across sentinel (`48` turns) + orchestrator (`28` turns) + 5 sub-orchs (`171k–213k` cold spikes). |
| **Measured Burn & Time** | **`486k–491k` uncached / boost** (`~$0.82`, `~7m`, `90.3%` tree CHR). | **`3.97M` worker uncached** + coord (`103.2M` gross, `96.2%` worker CHR, **`~28m`**). | **`22.81M` tree uncached** + `1.8M` parent relay (`354M` gross, **`~$38`**, **`3h10m`**). |
| **Repo & `agy-hq` Hygiene** | **`0` repo clutter**; `100%` clean `nested_subagent` DAG in `hq obs`. | `0` repo clutter (`SWARM_DIR` in `<appData>/brain/`), **but** workers leaked into `tasks.json` as `[Task:general] The following is a <SYSTEM_MESSAGE>...` with `Parent: ""`! | Created **`87` `.agents/teamwork/` folders (`352+` files)** in repo root; final `git push` blocked by `PreToolUse` subagent rule. |

---

## 4. Full `2 × 3` Matrix Verdict & Workload Fit for `agy-hq`

| Combo (`Model × Plugin`) | Verdict | Quantitative & Architectural Rationale |
| :--- | :---: | :--- |
| **1. `Gemini 4 Neon` + `/boost`** | **🏆 Primary (`Hard Problems & RFCs`)** | **`~$0.82` / `~7m` per boost.** Best choice for hard concurrency/state bugs, tricky tests, paper/codebase synthesis, and challenging RFC designs before coding—preserving `100%` of `Carbon` (`deep`) quota unless Ring-0 `authz`/`taint` or `KC-1` rules trigger. |
| **2. `Gemini 4 Neon` + `/swarm`** | **🏆 Primary (`Parallel Multi-File Builds`)** | **`~6×` cheaper & `~7×` faster than `/teamwork`** for `3–10` independent file/package slices. Must pair with: (a) running inside an `hq spawn --orchestrator --repo=.` worktree, (b) prompt rule pinning `monitor`/`narrator`/scouts to `gemini-4-hydrogen-low` and `workspace:"share"` authors + `verifier` to `neon-medium`/`high`, and (c) `agy-hq` `/swarm` lineage binding. |
| **3. `Gemini 4 Neon` + `/teamwork`** | **🟡 Niche (`Overnight Repo Sweeps Only`)** | Viable **only** for unattended `100+`-file multi-milestone migrations inside an isolated `hq worktree` where formal interview + 5-gate auditing outweighs `20M+` uncached burn and `2–3h` serial latency. |
| **4. `Orchestrator (Neon)` + `/boost`** | **❌ Avoid** | Double orchestration: intra-turn `flash-worker-agy` switching busts prefix cache (`69%` CHR) and risks spawning `DeepInvestigator` off a worker turn. |
| **5. `Orchestrator (Neon)` + `/swarm`** | **❌ Avoid** | Tested in Round 2: coordinator board/merge turns ran on `gemini-flash-worker-agy`, suffered `250k–309k` cold handoff spikes, and pushed a Go 1.24 CI break (`8d0db20`). Direct `Neon` + `/swarm` is strictly superior. |
| **6. `Orchestrator (Neon)` + `/teamwork`** | **❌ Avoid** | Tested in Round 3: burned `24.6M+` uncached tokens (`3h10m`), woke parent `30` times on `flash-worker-agy`, and downgraded all `85` `/teamwork` agents to `hydrogen-tiered-high`. |
| *(Baseline)* **Solo `Neon (Medium)` or `Orchestrator (Neon)` (`0` plugins)** | **✅ Fast Path (`1–3` Files)** | For `<= 3`-file bug fixes or PR tweaks in `swe-pulse`, skip all 3 multi-agent plugins: run direct `gemini-4-neon-medium` (`95.9%` CHR) or solo `Orchestrator (Neon)` (`< 150k` ctx) in one worktree. |

---

## 5. Required `agy-hq` Engineering Seams (`Gate H1` & `Gate H5-F` Audited)

To make the winning combinations (`Direct Neon + /boost`, `Direct Neon + /swarm`, `Direct Neon` fast-path, and `hq subagent` headless 3-wave CLI fallback) seamless without reinventing Antigravity's `/swarm` or `/boost` engines ([ADR-0001](../decisions/0001-option-1.5-hybrid-on-antigravity.md)):
1. **`/swarm` Worker Lineage & Title Hygiene (`hooks`, `kernel`, `obs/dag.go`):** Detect `<swarm_worker>` / `.system_generated/swarm` / LS `SourceMetadata` so `/swarm` workers (`author`, `verifier`, `narrator`, `monitor`) register as `nested_subagent` children under their `@coordinator` task instead of orphan `[Task:general] The following is a <SYSTEM_MESSAGE>...` entries, and strip `<SYSTEM_MESSAGE>` blocks in `SanitizeTaskTitle`.
2. **First-Class `orchestrator` Routing Profile & `Orchestrator (Neon)` Billing Fix (`routing`, `obs`, `ls_client`):** Add `orchestrator` tier (`gemini-4-neon-high` default, `--depth=medium` $\rightarrow$ `neon-medium`, `CanonicalTier: "standard"`, `0` `Carbon` deep burn) + `hq spawn --orchestrator`, match `"neon"` before `"pro"` so `gemini-pro-orchestrator-neon` bills at Neon `$0.50/$3.00` (and `gemini-flash-worker-agy` at Hydrogen `$0.30/$2.50`), and move `deep_routine` (`Neon`) out of the `Carbon` `deep` budget bucket.
3. **Subagent Parity (`invoke_subagent` ↔ `hq subagent`) & Dynamic Throttling (`routing`, `spawn.go`, `hooks`):** Rewrite native `invoke_subagent` `Model="flash"` $\rightarrow$ `"inherit"` on `Neon` parents for `[standard|heavy|orchestrator]` roles (and `"inherit"` $\rightarrow$ `"flash"` on `Carbon` parents for `[routine|standard]`), support `hq subagent --model=inherit`, add the `Carbon -> Neon-High -> Neon-Medium -> Hydrogen-Medium` `429` ladder to `SpawnSubagent`, soft-throttle tier defaults at `80%`/`95%` daily budget, and add `200k/300k/380k` coordinator context watermarks—placing new code outside `Gate H5-F` monolith ceilings (`hooks.go` `405/415`, `main.go` `573/615`, `kernel.go` `1,949/2,015`) and keeping `templates/skills/swe-pulse/SKILL.md` `<= 95 / 150` lines (`Gate H1`).
