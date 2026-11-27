# RFC-0031: Neon Orchestrator Mode — `2 × 3` Workload Routing (`/boost` · `/swarm` · `/teamwork`), Coordinator Tier, Subagent Parity & Dynamic Throttling

- **Status:** Proposed (Phase 1 RFC — Rev 2, Awaiting User Review)
- **Created:** 2026-11-27
- **Builds On:** [ADR-0001 (Hybrid Option 1.5)](../decisions/0001-option-1.5-hybrid-on-antigravity.md), [RFC-0009 (Scalable Task Board)](./0009-task-board-redesign.md), [RFC-0012 (Token Observability)](./0012-token-observability.md), [RFC-0018 (Progress Checkpoints)](./0018-always-on-agent-runtime-hardening.md), [RFC-0019 (Vault Skills)](./0019-two-tier-rsi-vault-overlay.md), [RFC-0025 (Pluggable Workspaces)](./0025-pluggable-workspace-providers.md), [RFC-0030 (Token Calibration)](./0030-console-ux-token-audit.md)
- **Research:** [26 — Empirical `2 × 3` Audit of `{Gemini 4 Neon, Orchestrator (Neon)} × {/swarm, /teamwork, /boost}`, Subagent Parity & Dynamic Throttling](../research/26_neon_orchestrator_mode_audit.md)

---

## 1. One-Page Summary (TL;DR)

Evaluating all **six combinations** of `{Gemini 4 Neon, Orchestrator (Neon)} × {/swarm, /teamwork, /boost}` across `656` live Vault conversations (`308.4M` uncached tokens) proved two core lessons: **(1)** stacking host `Orchestrator (Neon)` (`gemini-pro-orchestrator-neon`) under `/swarm`, `/teamwork`, or `/boost` causes `250k–309k` intra-turn KV-cache thrash (`69.2%` CHR), runs coordinator merges on `gemini-flash-worker-agy`, and downgrades child workers to `Hydrogen`; and **(2)** **direct `Gemini 4 Neon` + `/boost`** (`~$0.82`/`7m`) and **direct `Gemini 4 Neon` + `/swarm`** (`workspace:"share"`, `~6×` cheaper & `~7×` faster than `/teamwork`) cover `95%` of complex work cleanly—once `agy-hq` fixes four integration gaps:

| Problem Today | What We Propose |
| :--- | :--- |
| **1. Unclear `2 × 3` choice; Phase-2 tasks leak onto `Carbon` (`deep`), and `Orchestrator (Neon)` is misbilled as `Carbon`** | **First-Class `orchestrator` Profile (`gemini-4-neon-high` / `medium`) + `2 × 3` Policy:** Add logical tier `orchestrator` (`CanonicalTier: "standard"`, **`0` `Carbon` deep burn**) + `hq spawn --orchestrator` / `hq subagent --orchestrator`. Reserve `Orchestrator (Neon)` strictly for **solo `0`-plugin sessions (`< 150k` ctx)**, fix `"neon"`-before-`"pro"` billing/fallbacks (`gemini-pro-orchestrator-neon` $\rightarrow$ Neon rates; `gemini-flash-worker-agy` $\rightarrow$ Hydrogen rates), and route Phase-2 prompts ahead of `reDeepTask`. |
| **2. `/swarm` leaks orphan `<SYSTEM_MESSAGE>` tasks into `tasks.json`/`obs`, defaults all helpers to `Neon-High`, and `swe-pulse` lacks a mode selector** | **4-Mode `swe-pulse` Playbook (`/boost` · `/swarm` · Headless 3-Wave CLI · `/teamwork`) + `/swarm` Lineage Fix:** Teach `SessionStart`, `Stop`, `SanitizeTaskTitle`, and `obs/dag.go` to classify `<swarm_worker>` trajectories as `nested_subagent` children under their `@coordinator` task (`0` Task Board pollution). Update `swe-pulse/SKILL.md` with the `2 × 3` decision table, `/swarm` tiered-model + `workspace:"share"` brief contract, headless 3-wave CLI fallback (`hq subagent --repo`), and wave `hq progress save` checkpoints. |
| **3. Native `invoke_subagent(Model="flash")` downgrades `Neon` children to `Hydrogen`, `SpawnSubagent` lacks `429` retry, and budgets/contexts cliff at `100%` / `120k`** | **Subagent Parity Bridge + Symmetric `429` Ladder + `80%/95%` Budget & `200k/300k/380k` Context Throttling:** Rewrite `invoke_subagent` `Model="flash"` $\rightarrow$ `"inherit"` on `Neon` parents for `[standard\|heavy\|orchestrator]` (and `"inherit"` $\rightarrow$ `"flash"` on `Carbon` parents for `[routine\|standard]`), support `hq subagent --model=inherit`, add the `Carbon -> Neon-High -> Neon-Medium -> Hydrogen-Medium` `429` ladder to `SpawnSubagent`, soft-throttle tiers at `80%/95%` budget, and add `200k/300k/380k` coordinator context guards. |

---

## 2. Canonical `2 × 3` Decision Matrix & Concrete Before/After Examples

| Task Shape in `agy-hq` | Winning Combo | Why It Wins (Empirical Vault Proof) |
| :--- | :--- | :--- |
| **`1–3` Known Files (Bugfix / Small Feature / PR Triage)** | **Direct `Gemini 4 Neon (Medium)`** *(or solo `Orchestrator (Neon)` `< 150k` ctx; `0` plugins)* | **`95.9%` CHR**, zero subagent startup tax, clean single `hq worktree`. |
| **Hard Logic Bug, Flaky Race, Deep Research, or RFC Design Critique** | **Direct `Gemini 4 Neon (High)` + `/boost`** | **`~490k` uncached (`~$0.82`, `~7m`, `3` subagents):** `DeepInvestigator` runs `WorkerL0` + adversarial `ImprovementWorker` on `neon-high` (`0` `Carbon` burn; escalate to `Carbon + /boost` only for Ring-0 `authz`/`taint` or `KC-1` breaches). |
| **Parallel Multi-File Feature or Refactor (`3–10` Disjoint Slices)** | **Direct `Gemini 4 Neon (High)` + `/swarm`** *(inside an `hq spawn --orchestrator --repo=.` worktree)* | **`~4.0M` uncached (`~28m`, `96.2%` worker CHR) vs. `/teamwork`'s `22.81M` (`3h10m`):** `0`-token `swarm_wait`, native `workspace:"share"` worktrees per author, `1` fresh `verifier` per `2–4` authors, and explicit `hydrogen-low` pinning for `monitor`/`narrator`/scouts. |
| **Unattended Overnight Repo-Wide Overhaul (`100+` Files)** | **Direct `Gemini 4 Neon (High)` + `/teamwork`** *(inside an isolated `hq worktree`)* | Full scoping interview + milestone sub-orchestrators + Victory Audit when walk-away autonomy matters more than `20M+` token cost; **never** pair with `Orchestrator (Neon)` or run in the root checkout. |
| **Non-Interactive / CLI / `[Auto]` Multi-Slice Pulse (`0` UI Slash Cmd)** | **`hq spawn --orchestrator` + Headless 3-Wave `hq subagent`** | Wave 1 `hydrogen-low` scouts $\rightarrow$ Wave 2 parallel `hq subagent --repo` `neon-medium` builders with disjoint file fences $\rightarrow$ Wave 3 `./bin/hq ci pre-push` + `1` fresh `neon-high` challenger. |

### Concrete Before vs. After
- **Before (`Orchestrator (Neon)` + `/swarm` on Round 2 / `/teamwork` on Round 3):** Selecting `Orchestrator (Neon)` + `/swarm` bounced turns between `pro-orchestrator-neon` and `flash-worker-agy` (`250k–309k` uncached per switch), leaked `6` orphan `[Task:general] The following is a <SYSTEM_MESSAGE>...` rows onto the Task Board, and broke Go 1.24 CI on `flash-worker-agy`. Selecting `Orchestrator (Neon)` + `/teamwork` downgraded all `85` agents to `hydrogen-tiered-high`, woke the parent `30` times, created `87` `.agents/teamwork/` folders in the root repo, and burned `24.6M+` uncached tokens (`$38`, `3h10m`).
- **After (`RFC-0031`):** `hq spawn --orchestrator --repo=. --task=<slug>` opens a clean `gemini-4-neon-high` coordinator worktree (`0` `Carbon` deep burn). Running `/boost` challenges tricky designs in `7m` (`~$0.82`); running `/swarm` with the `swe-pulse` brief template pins `monitor`/`narrator`/scouts to `hydrogen-low` and `workspace:"share"` authors + `verifier` to `neon-medium`/`high`, attributes all `/swarm` workers cleanly under the coordinator tree in `hq obs` (`0` orphan tasks), runs `./bin/hq ci pre-push` before squash-merge, and saves wave checkpoints via `hq progress save`.

---

## 3. Pillar 1: `orchestrator` Routing Profile & `Orchestrator (Neon)` Telemetry Fix

1. **Routing Matrix (`internal/routing/routing.go`, `templates/config.yaml`, `cmd/hq/tasks_cli.go`):**
   - Add canonical logical tier **`orchestrator`** (aliases: `coordinator`, `coord`) defaulting to **`gemini-4-neon-high`** (`--depth=medium` $\rightarrow$ **`gemini-4-neon-medium`**, `CanonicalTier: "standard"`, **`0` `DailyDeepUncachedBudget` burn**), exposed via `hq spawn --orchestrator` and `hq subagent --orchestrator` (`ActiveTask.LogicalTier = "orchestrator"`).
   - Move `deep_routine` (`sleep-consolidate` / research memos on `gemini-4-neon-high`) from `CanonicalTier: "deep"` (`SubagentModel: "pro"`) to `CanonicalTier: "standard"` (`SubagentModel: "flash"` / `"inherit"`) so only true `Carbon` / `Argon` / `Claude` models consume the `30M` daily `deep` cap.
   - Evaluate `reOrchestratorTask` (`phase\s*2\s+implement\w*|orchestrat\w*|multi[- ]?wave|swe[- ]?pulse|parallel\s+worktree\w*`) **ahead of `reDeepTask`** unless Phase-1 RFC authoring or `KC-1` falsification is requested.
2. **`Orchestrator (Neon)` & `flash-worker-agy` Billing/Fallback Fix (`internal/obs/obs.go`, `internal/kernel/ls_client.go`):**
   - Match `"neon"` **before** generic `"pro"` in `ModelCanonicalTier`, `ModelFamily`, `ModelRatesPerMillion`, `inferSubagentAlias`, `CustomFallbackCandidates`, and `selectLiveFallbackModelConfig`, and classify `gemini-flash-worker-agy` under `Family: "hydrogen"` (`$0.30 / $0.075 / $2.50`, `standard`) so `Orchestrator (Neon)` sessions report exact per-turn family/cost telemetry and fall back within `Neon`/`Hydrogen` instead of `Carbon`.

---

## 4. Pillar 2: `/swarm` Lineage Integration & 4-Mode `swe-pulse` Coordinator Contract

1. **`/swarm` Worker Lineage & `<SYSTEM_MESSAGE>` Title Sanitization (`internal/hooks/`, `internal/kernel/`, `internal/obs/dag.go`):**
   - Strip leading `<SYSTEM_MESSAGE>...</SYSTEM_MESSAGE>` blocks in `SanitizeTaskTitle` (alongside existing `<USER_REQUEST>` stripping in [`kernel.go:30`](../../internal/kernel/kernel.go)) so system-injected prompts never appear as literal task titles.
   - Detect `/swarm` worker sessions (`<swarm_worker>` header in first user input, `.system_generated/swarm` path reference, or LS `SourceMetadata`) in `detectNestedDescendant` (`stop.go` / `session_start.go`) and `obs/dag.go`: record them in `nested_conversations.json` (`IsSubagent = true`, `leanAuto = true` Zone-1 budget, barred from `claim-desk`) and link their `ParentConversationID` to the owning `@coordinator` conversation via `SWARM_DIR` (`<appData>/brain/<coordinator-conv-id>/.system_generated/swarm/`) or active swarm spawner lookup!
2. **4-Mode `swe-pulse` Coordinator Contract (`templates/skills/swe-pulse/SKILL.md` `<= 95 / 150` lines, synced via `hq skill sync`):**
   - **Mode 1 — Fast Path (`<= 3` files):** Single `hq worktree add`, direct `gemini-4-neon-medium` edits + repo tests + clean `git status -s`.
   - **Mode 2 — Hard Bug / Design Verification (`/boost` on `Neon-High`):** Run `/boost` inside the task worktree to challenge root causes or edge cases via `WorkerL0` + `ImprovementWorker` before editing.
   - **Mode 3 — Interactive Parallel Slices (`/swarm` on `Neon-High` Coordinator):** Always launch from an isolated task worktree (`hq spawn --orchestrator --repo=<repo> --task=<slug>`), pass explicit `/swarm` model & isolation rules (*pin `monitor`/`narrator`/scouts to `gemini-4-hydrogen-low`; give each author `workspace:"share"`, explicit owned file paths, and `model:"gemini-4-neon-medium"` or `"high"`; add `1` `verifier` on `gemini-4-neon-high` per `2–4` authors; land branches into the coordinator worktree before `swarm{action:"kill"}`*), and run `./bin/hq ci pre-commit && ./bin/hq ci pre-push` before squash-merging to `main`.
   - **Mode 4 — Headless / CLI 3-Wave Coordinator (No UI Slash Command):** Wave 1 `hq subagent --tier=quick` (`hydrogen-low`) scouts $\rightarrow$ `hq progress save` $\rightarrow$ Wave 2 parallel `hq subagent --repo=<repo> --task=<slug>-w2-<slice> --tier=standard` (`neon-medium`) builders with disjoint file-ownership lists verified via `git diff --name-only` $\rightarrow$ `hq progress save` $\rightarrow$ Wave 3 `./bin/hq ci pre-push` + `1` fresh-context challenger (`hq subagent --tier=heavy` on `neon-high`) before `main` merge (`0` polling crons; reactive `send_message` wakeup only).

---

## 5. Pillar 3: Subagent Parity & Dynamic Budget/Context Throttling

1. **Native `invoke_subagent` ↔ `hq subagent` Parity (`internal/routing/`, `internal/kernel/spawn.go`, `internal/hooks/orchestrator_hooks.go`):**
   - Support `hq subagent --model=inherit` (resolving the parent conversation's live model from `tasks.json` / latest journal / LS lineage) and align coarse `--model=flash` with `standard` (`gemini-4-neon-medium`, or `gemini-4-hydrogen-medium` when `--tier=routine`).
   - In `PreToolUse` (`invoke_subagent` with `TypeName != "browser"`): rewrite `Model="flash"` (or empty) $\rightarrow$ **`"inherit"`** when caller `payload.ModelName` is `gemini-4-neon-*` and role tier is `standard|heavy|orchestrator` (preventing silent downgrade to `Hydrogen`), and rewrite `"inherit"` $\rightarrow$ `"flash"` / `"flash_lite"` when a `Carbon`/`Claude` parent spawns a `[routine|standard]` or `[quick]` helper (preventing accidental `A4` deep burn).
2. **Dynamic `80%/95%` Budget Step-Down, Symmetric `429` Ladder & `200k/300k/380k` Context Watermarks:**
   - **Budget Soft-Throttling (`internal/routing/budget_throttle.go`):** At `>= 80%` `DailyDeepUncachedBudget` (`24M / 30M`), step non-forced, non-explicit `deep` subagents down to `gemini-4-neon-high`. At `>= 80%` `DailyUncachedBudget` (`40M / 50M`), step `orchestrator`/`heavy` `neon-high` $\rightarrow$ `neon-medium` and `standard` `neon-medium` $\rightarrow$ `hydrogen-high`; at `>= 95%` (`47.5M / 50M`), step non-forced tiers to `hydrogen-medium` (`ExplicitModel` and `--force` always win).
   - **Symmetric `429` Ladder (`internal/kernel/spawn.go`):** Give `SpawnSubagent` the same automatic HTTP `429` (`RESOURCE_EXHAUSTED`) retry as `SpawnTask`, stepping smoothly down `carbon-high -> neon-high -> neon-medium -> fallback_on_429 (hydrogen-medium)`.
   - **Coordinator Context Watermarks (`internal/hooks/orchestrator_hooks.go`):** Keep Front Desk hygiene at `120k/160k/180k`; replace the single `120k` `2h`-muted task nudge with watermark-keyed stages resetting on `CHECKPOINT`: **`200k`** (prefer diffs/summaries), **`300k`** (auto-save `hq progress save` on `Stop` + wave-boundary `/compact` prompt), and **`380k`** (`LogicalTier == "orchestrator"` guard capping inline `read_file`/`grep_search` at `3`/turn until `/compact` or delegation; regular coding tasks stay `100%` unrestricted).

---

## 6. Guardrails, Implementation Phases & Open Review Questions

- **Repo Hygiene & LOC Ceilings (`Gate H1`, `H4-A`, `H5-F`):** Keeps `go.mod` at `0` external deps and places new logic in `internal/routing/budget_throttle.go`, `internal/kernel/orchestrator.go`, `internal/hooks/orchestrator_hooks.go`, and `cmd/hq/tasks_cli.go` so `hooks.go` (`405/415`), `main.go` (`573/615`), `workspace_lifecycle.go` (`1,056/1,075`), and `kernel.go` (`1,949/2,015`) stay strictly below `Gate H5-F` ceilings.

| Phase | Scope | Verification |
| :--- | :--- | :--- |
| **Phase 2A (`routing`, `obs`, `ls_client`)** | `orchestrator` tier + `reOrchestratorTask`, `deep_routine` `standard` bucket fix, `"neon"`-before-`"pro"` + `flash-worker-agy` pricing/fallback, `--model=inherit|flash` parity, & `80%/95%` `ResolveWithBudget`. | Unit tests in `internal/routing` & `internal/obs` + updated `Gate H3` golden routing table. |
| **Phase 2B (`kernel`, `hooks`, `obs/dag`, `cmd/hq`)** | `--orchestrator` CLI flag, `<swarm_worker>` / `SWARM_DIR` nested lineage & `<SYSTEM_MESSAGE>` title strip, `SpawnSubagent` `429` ladder, `PreToolUse` `invoke_subagent` parity bridge, & `200k/300k/380k` watermarks. | Unit tests covering `/swarm` worker parent binding, `429` step-down, `invoke_subagent` rewrites, & post-`CHECKPOINT` watermark reset. |
| **Phase 2C (`swe-pulse`, `AGENTS.md`, `CUJ-50`)** | 4-mode `templates/skills/swe-pulse/SKILL.md` (`hq skill sync`), `.agents/plugins/agy-hq/rules/AGENTS.md` `2 × 3` guidance, & `docs/product/0003-critical-user-journeys.md` (`CUJ-50`). | **`TestCUJ50_NeonOrchestratorSwarmLineageParityAndThrottling`** E2E test + `100%` `./bin/hq ci pre-push` & `go test -race ./...`. |

### Open Review Questions for Sign-Off
1. **`2 × 3` Default Policy Confirmation:** Do you agree with codifying **`Direct Gemini 4 Neon + /boost`** (hard bugs/research/RFCs) and **`Direct Gemini 4 Neon + /swarm`** (parallel `workspace:"share"` multi-file builds with `hydrogen-low` helper pinning) as our two primary multi-agent modes—while avoiding `Orchestrator (Neon)` whenever `/boost`, `/swarm`, or `/teamwork` is active?
2. **Default `orchestrator` Depth (`gemini-4-neon-high` vs. `gemini-4-neon-medium`):** Confirm defaulting `--orchestrator` to **`gemini-4-neon-high`** (stepping down to `neon-medium` on `--depth=medium` or at `>= 80%` daily budget)?
3. **`380k` Coordinator Context Guard (`Hard 3-Lookup Cap` vs. `Soft Nudge`):** Above `380k` context on `--orchestrator` sessions, do you prefer the **`3`-lookup/turn mechanical `PreToolUse` cap** (forcing `/compact` or delegation) or a **non-blocking `PreToolUse` warning nudge**?
