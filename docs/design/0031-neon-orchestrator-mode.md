# RFC-0031: Neon Orchestrator Mode — First-Class Coordinator Routing, Multi-Wave `swe-pulse` Execution, Subagent Parity & Dynamic Budget/Context Throttling

- **Status:** Proposed (Phase 1 RFC — Awaiting User Review)
- **Created:** 2026-11-27
- **Builds On:** [RFC-0009 (Scalable Task Board)](./0009-task-board-redesign.md), [RFC-0012 (Token Observability)](./0012-token-observability.md), [RFC-0018 (Progress Checkpoints)](./0018-always-on-agent-runtime-hardening.md), [RFC-0019 (Vault Skills)](./0019-two-tier-rsi-vault-overlay.md), [RFC-0025 (Pluggable Workspaces)](./0025-pluggable-workspace-providers.md), [RFC-0030 (Token Calibration)](./0030-console-ux-token-audit.md)
- **Research:** [26 — Forensic Audit of Multi-Agent Orchestration, 3-Family Neon Routing, Subagent Model Parity & Dynamic Throttling](../research/26_neon_orchestrator_mode_audit.md)

---

## 1. One-Page Summary (TL;DR)

Auditing `656` production conversations (`308.4M` uncached / `3.19B` gross tokens) and the `85`-agent `.agents/teamwork/` Round 3 compaction trace (`22.81M` uncached, `3h10m`) surfaced three gaps: multi-file engineering prompts accidentally route to scarce `gemini-4-carbon-high` (`deep`) quota, `/teamwork` burns `6×–8×` excess tokens on `10m` polling crons and single-checkout serialization while `swe-pulse` lacks a multi-wave contract, and Antigravity's native `invoke_subagent(Model="flash")` silently downgrades `Neon` children to `Hydrogen`. This RFC proposes **three focused upgrades**:

| Problem Today | What We Propose |
| :--- | :--- |
| **1. No coordinator routing tier; Phase-2 prompts leak onto `Carbon` (`deep`) or bust cache on host `Orchestrator (Neon)`** | **First-Class `orchestrator` Profile (`gemini-4-neon-high` / `medium`) + `--orchestrator` Flag:** Add logical tier `orchestrator` (`CanonicalTier: "standard"`, **`0` `DailyDeepUncachedBudget` burn**) to `internal/routing/`, `templates/config.yaml`, `hq spawn --orchestrator`, and `hq subagent --orchestrator`. Classify Phase-2/multi-wave coordination prompts ahead of `reDeepTask` so `Carbon` stays strictly protected for Phase 1 RFCs, Ring-0 security audits, and `KC-1` thesis checks, and fix the `"pro"`-before-`"neon"` billing/fallback bug for `gemini-pro-orchestrator-neon`. |
| **2. `/teamwork` spawns `85` agents sequentially on 1 checkout with `10m` polling crons, while `swe-pulse` has no wave or file-fence contract** | **3-Wave `swe-pulse` Coordinator Contract + Mechanical File-Ownership Fences + `hq progress`:** Standardize multi-file engineering around **Wave 1** (`gemini-4-hydrogen-low` read-only scouts $\rightarrow$ disjoint slice map), **Wave 2** (parallel isolated-worktree `gemini-4-neon-{medium,high}` builders enforced by `--fence=<globs>` in `PreToolUse` and `hq worktree verify-fence` before merge), and **Wave 3** (`0`-token `./bin/hq ci pre-push` + **1** fresh-context challenger before `main` merge), with `0` polling crons and `hq progress save` checkpoints at every wave boundary. |
| **3. Native `invoke_subagent` `SubagentModel` disagrees with `hq subagent`, `SpawnSubagent` lacks `429` fallback, and budgets/contexts only act at `100%` / `120k`** | **Parent-Aware Subagent Parity + `80%`/`95%` Budget Step-Down + `200k`/`300k`/`380k` Context Guards:** Bridge native `invoke_subagent` in `PreToolUse` (`Neon` parent + `[standard\|heavy]` $\rightarrow$ `Model="inherit"`; `Carbon` parent + `[standard]` $\rightarrow$ `Model="flash"`), align `hq subagent --model=inherit|flash|flash_lite`, add the `Carbon -> Neon-High -> Neon-Medium -> Hydrogen-Medium` `429` ladder to `SpawnSubagent`, soft-throttle tier defaults at `80%`/`95%` daily budget, and add wave-aware `200k / 300k / 380k` orchestrator context guards. |

---

## 2. What It Looks Like in Practice (Before vs. After)

### Example A — Launching Phase 2 of an Approved RFC Without Draining `Carbon` Quota
- **Today:** From `[HQ] Front Desk`, running `hq spawn --repo=. --task=rfc0030-impl --prompt="Coordinate Phase 2 implementation and E2E audit for RFC-0030"` matches `rfc` and `audit` in `reDeepTask` and spawns on **`gemini-4-carbon-high` (`deep`)**. Across our 4 recent flagship tasks, `80.4%` of turns (`123 / 153`) were Phase 2 implementation where the user had to manually switch the UI picker down to `gemini-4-neon-high` mid-run—or tried host `Orchestrator (Neon)` (`gemini-pro-orchestrator-neon`), which alternated `flash-worker-agy` turns inside one transcript (`69.2%` CHR) and was misbilled at `$1.25/$10.00` Carbon rates.
- **With RFC-0031:** Running `hq spawn --orchestrator --repo=. --task=rfc0030-impl --prompt="..."` (or matching `Phase 2 implementation` automatically) launches `[Task:agy-hq] rfc0030-impl (orchestrator/gemini-4-neon-high)` with **`91%+` single-model prefix cache hit ratio** and **`0` tokens billed against the `30M` daily deep cap** (`--depth=medium` selects `gemini-4-neon-medium` for lighter `<= 2`-slice runs).

### Example B — 4-Package Refactor Sweep (`swe-pulse` 3-Wave Contract vs. `/teamwork`)
- **Today (`/teamwork`):** Spawns `85` homogeneous `hydrogen-tiered-high` subagents across 5 sequential milestones on the root checkout (`87` `.agents/teamwork/` folders), wakes `259k–319k` orchestrators every `10m` via cron (`76+` turns), runs `5` LLM review/audit agents per milestone before `hq ci pre-push`, burns **`22.81M` uncached tokens (`$35.88`, `3h10m`)**, and fails final `git push` under subagent `PreToolUse` rules.
- **With RFC-0031 (`swe-pulse` Orchestrator Mode):**
  1. **Wave 1 (`~1m`, `~$0.02`):** Coordinator dispatches `2` parallel `hq subagent --tier=quick` (`gemini-4-hydrogen-low`) scouts, sleeps at `0` tokens until `send_message` callbacks arrive, partitions files into 3 disjoint slices, and saves `hq progress save --task=refactor-yaml --done="Wave 1: 3 disjoint slices fenced" --next="Wave 2 parallel builders"`.
  2. **Wave 2 (`~8m`, parallel):** Dispatches `3` concurrent builders via `hq subagent --repo=. --task=refactor-yaml-w2-s1 --tier=standard --fence="internal/yamlmini/**,internal/routing/**"`, each editing and running `go test -race` in its own `<vault>/worktrees/` checkout while `PreToolUse` mechanically blocks out-of-fence edits. Coordinator verifies `hq worktree verify-fence`, merges the 3 clean branches into `hq/refactor-yaml`, prunes `-w2-*` worktrees, and saves Wave-2 progress.
  3. **Wave 3 (`~4m`):** Runs `./bin/hq ci pre-commit && ./bin/hq ci pre-push` (`0` LLM tokens) + **1** fresh-context challenger (`hq subagent --tier=heavy` on `gemini-4-neon-high` inspecting `ORIGINAL_REQUEST` vs. `git diff origin/main...HEAD`), rebases and squash-merges onto `main`, and reports a concise summary (`~7` subagents total, `~3M` uncached, `0` repo clutter).

### Example C — Subagent Parity & Dynamic Budget / Context Backpressure
- **Today:** When a `gemini-4-neon-high` task calls `invoke_subagent(TypeName="self", Role="[standard] agy-hq: Parser Fixer", Model="flash")`, Antigravity's LS ignores `Neon` and runs `gemini-4-hydrogen-tiered-high`. If `neon-high` hits HTTP `429` inside `hq subagent`, the call errors out instead of stepping down to `neon-medium`. And once coordinator context passes `120k`, hooks go silent until `512k` auto-compaction.
- **With RFC-0031:** `PreToolUse` rewrites `Model="flash"` $\rightarrow$ `"inherit"` when a `Neon` parent spawns a `[standard|heavy|orchestrator]` non-browser subagent (keeping it on `Neon`), while rewriting unintentional `"inherit"` $\rightarrow$ `"flash"` when a `Carbon` parent spawns a `[routine|standard]` worker. At `80%` daily budget (`40M / 50M`), `orchestrator` / `heavy` auto-steps from `neon-high` to `neon-medium`; on HTTP `429`, `SpawnSubagent` steps smoothly down `neon-high -> neon-medium -> hydrogen-medium`; and at `300k` / `380k` context, the coordinator auto-checkpoints and compacts at the wave boundary.

---

## 3. Pillar 1: First-Class `orchestrator` Routing Profile & CLI Surface

1. **Routing Matrix Extension (`internal/routing/routing.go`, `templates/config.yaml`):**
   - Add canonical logical tier **`orchestrator`** (aliases: `coordinator`, `coord`) to `NormalizeLogicalTier`, `builtinDefaultModelForLogical`, and `templates/config.yaml`:

   | Logical Tier | Built-In Default Model | `--depth=medium` Override | `CanonicalTier` (Budget Bucket) | Native `SubagentModel` (Standalone / From `Neon` Parent) | Primary Workload |
   | :--- | :--- | :--- | :---: | :---: | :--- |
   | `quick` | `gemini-4-hydrogen-low` | `gemini-4-hydrogen-low` | `quick` | `flash_lite` / `flash_lite` | Wave-1 repo scouts, status checks, high-freq polling |
   | `routine` | `gemini-4-hydrogen-medium` | `gemini-4-hydrogen-medium` | `standard` | `flash` / `flash` | `[HQ] Front Desk`, Morning Brief, routine digests |
   | `standard` | `gemini-4-neon-medium` | `gemini-4-neon-medium` | `standard` | `flash` / `inherit` | Wave-2 bounded single-slice coding & unit tests |
   | **`orchestrator`** | **`gemini-4-neon-high`** | **`gemini-4-neon-medium`** | **`standard`** | **`flash` / `inherit`** | **Multi-wave `swe-pulse` coordinators & Phase-2 RFC builds (`0` `deep` burn)** |
   | `heavy` | `gemini-4-neon-high` | `gemini-4-neon-medium` | `standard` | `flash` / `inherit` | Wave-2 cross-file refactors & Wave-3 fresh-context challenger |
   | `deep_routine` | `gemini-4-neon-high` | `gemini-4-neon-medium` | `standard` *(fix)* | `flash` / `inherit` | Nightly `sleep-consolidate` & research memos *(stops mis-eating `deep` cap)* |
   | `deep` | `gemini-4-carbon-high` | `gemini-4-carbon-medium` | `deep` | `pro` / `pro` | Phase 1 RFCs, Ring-0 `authz`/`taint` security audits, `KC-1` breaches |

   - Add `reOrchestratorTask` (`phase\s*2\s+implement\w*|orchestrat\w*|multi[- ]?wave|swe[- ]?pulse|parallel\s+worktree\w*|coordinate\s+(?:builders|subagents|implementation)`) evaluated **before** `reDeepTask` whenever explicit Phase-1 RFC authoring or `KC-1` falsification is not requested, preventing `"Implement RFC-0030 Phase 2"` from leaking onto `gemini-4-carbon-high`.
   - Fix `"pro"`-before-`"neon"` ordering in `obs.go` (`ModelCanonicalTier`, `ModelFamily`, `ModelRatesPerMillion`), `routing.go` (`inferSubagentAlias`, `CustomFallbackCandidates`), and `ls_client.go` (`selectLiveFallbackModelConfig`) so `gemini-pro-orchestrator-neon` maps to `Family: "neon"`, `CanonicalTier: "standard"`, Neon rates (`$0.50 / $0.125 / $3.00`), and Neon live fallbacks.
2. **CLI Surface (`cmd/hq/tasks_cli.go`, `internal/kernel/kernel.go`):**
   - Add `--orchestrator` boolean flag to **`hq spawn`** and **`hq subagent`** (setting `tier = "orchestrator"` when `--tier` is unset and recording `LogicalTier: "orchestrator"` on `ActiveTask`).
   - Preserve `[Task:<scope>] <slug> (orchestrator/<model>)` title formatting so `IsExplicitTopLevelTask`, `SanitizeTaskTitle`, and `CUJ-40` sidebar/board filters work with zero regressions.
   - In `HandleSessionStart`, inject a compact (`<= 14` lines) **`## 🎼 Active Role: Neon Wave Orchestrator`** Zone-2 protocol card **only** when `matchedTask.LogicalTier == "orchestrator"` (`0` tokens added to regular tasks or Front Desk).

---

## 4. Pillar 2: Multi-Wave `swe-pulse` Coordinator Contract, File Fences & `hq progress`

Update [`templates/skills/swe-pulse/SKILL.md`](../../templates/skills/swe-pulse/SKILL.md) (`<= 95` lines, well within Gate `H1`'s `150`-line cap, `0` tracked `rsi:rule` blocks; synced to `<vault>/skills/swe-pulse/SKILL.md` via `hq skill sync` while preserving personal RSI rules):

1. **Fast-Path vs. Multi-Wave Gate:** Single-slice tasks touching `<= 3` known files execute directly in one isolated worktree without subagent ceremony. Multi-package features, refactors, or sweeps (`>= 4` files or `>= 2` subsystems) run the **3-Wave Coordinator Contract** with **zero polling crons** (sleep after dispatch; wake reactively on child `send_message`):
   - **Wave 1 — Scout & Slice (`hq subagent --tier=quick` on `gemini-4-hydrogen-low`):** `1–3` parallel read-only scouts map target files, tests, and `Gate H5-F` monolith LOC headrooms (`hooks.go <= 415`, `main.go <= 615`, `workspace_lifecycle.go <= 1075`, `webui.go <= 1115`, `kernel.go <= 2015`). Coordinator defines `2–4` disjoint slices (`S1..Sk`), reserves shared index files (`cmd/hq/main.go`, `docs/README.md`) for coordinator integration or one designated slice, and checkpoints **`hq progress save --task=<slug> --goal="..." --done="Wave 1 slices fenced" --next="Wave 2: S1..Sk"`**.
   - **Wave 2 — Fenced Parallel Worktree Builders (`hq subagent --repo=<repo> --task=<slug>-w2-<si> --tier=standard|heavy --fence="<globs>"`):**
     - Each builder works in `<vault>/worktrees/<repo>-<slug>-w2-<si>/` (or `--workspace=<provider> --share-from=<base>` per RFC-0025).
     - **Mechanical File-Ownership Fence (`internal/kernel/orchestrator.go` + `internal/hooks/orchestrator_hooks.go`):** `--fence="<comma-separated-repo-relative-globs>"` persists an entry in `<vault>/memory/subagent_fences.json` (`store.WriteAtomic` under `RankSubagentQueue`). `PreToolUse` denies `write_file`/`edit_file` outside the builder's worktree or matching fence globs, and **`hq worktree verify-fence <worktree> [--base=origin/main]`** (`0` LLM tokens) verifies `git diff --name-only` + clean `git status -s` before the coordinator merges `hq/<slug>-w2-<si>` into `hq/<slug>` and removes `-w2-<si>`. Coordinator saves Wave-2 `hq progress save`.
   - **Wave 3 — Deterministic `ci pre-push` + Fresh-Context Challenger Gate:**
     - **Step 3A (`0` LLM tokens):** Run `go build -o ./bin/hq ./cmd/hq && ./bin/hq ci pre-commit && ./bin/hq ci pre-push && go test -race ./...` in `hq/<slug>`.
     - **Step 3B (1 Fresh-Context Challenger):** Spawn **one** fresh subagent (`hq subagent --tier=heavy` on `gemini-4-neon-high`; escalate to `--tier=deep` `gemini-4-carbon-high` / `opus` only when modifying Ring-0 `authz`, `taint`, `approvals`, or `sandbox`) given **only** the original user request/RFC criteria and `git diff origin/main...HEAD`. Cap fix-and-reverify loops at **`2` rounds** before checkpointing and escalating to the user.
     - **Step 3C (Delivery):** Rebase onto `origin/main`, re-run `./bin/hq ci pre-push`, squash-merge/push (or stage third-party `gh pr merge` via `hq propose`), run `hq complete <slug>`, and deliver a concise summary.

---

## 5. Pillar 3: Subagent Parity & Dynamic Budget/Context Throttling

1. **Native `invoke_subagent` ↔ `hq subagent` Model Resolution Parity:**
   - **`hq subagent` (`internal/routing/routing.go`, `internal/kernel/spawn.go`):**
     - Support `--model=inherit` by resolving the parent conversation's active model (`tasks.json` $\rightarrow$ latest journal span $\rightarrow$ `InspectLSConversationLineage` $\rightarrow$ `standard` default).
     - Fix coarse `--model=flash` in `ResolveBuiltin` / `Resolve` to map to `standard` (`gemini-4-neon-medium`) unless `--tier=routine` (`gemini-4-hydrogen-medium`), matching `SubagentModel` round-trip semantics.
   - **Native `invoke_subagent` Bridge (`internal/hooks/orchestrator_hooks.go` in `PreToolUse`):**
     - When `TypeName != "browser"` and `Model` is `"flash"` or empty: if caller `payload.ModelName` is `gemini-4-neon-*` and role tier is `standard`, `heavy`, or `orchestrator`, rewrite `Model` to **`"inherit"`** via `writePreToolAllowOverwrite` so the child stays on `Neon` instead of downgrading to `Hydrogen`.
     - When `TypeName != "browser"` and `Model` is `"inherit"` or empty on a `Carbon`/`Argon`/`Opus`/`Sonnet` parent with `[quick|routine|standard]` role prefix, rewrite `Model` to `"flash_lite"` (`quick`) or `"flash"` (`routine`/`standard`) to prevent accidental `A4` deep-quota burn.
2. **Dynamic Budget Soft-Throttling & Symmetric `429` Ladder (`internal/routing/budget_throttle.go`, `internal/kernel/spawn.go`):**
   - Add optional `SoftThrottlePct` (`default: 0.80`) and `HardThrottlePct` (`default: 0.95`) to `obs.BudgetConfig`:
     - **`>= 80%` `DailyDeepUncachedBudget` (`24M / 30M`):** Non-forced, non-explicit `deep` subagents step down to `gemini-4-neon-high` (`orchestrator` is already `0`-deep-burn).
     - **`>= 80%` `DailyUncachedBudget` (`40M / 50M`):** Non-explicit `orchestrator`/`heavy` step down `gemini-4-neon-high` $\rightarrow$ `gemini-4-neon-medium`; `standard` steps down `gemini-4-neon-medium` $\rightarrow$ `gemini-4-hydrogen-high`; `SessionStart` / `hq status` advises `<= 2` parallel Wave-2 builders.
     - **`>= 95%` `DailyUncachedBudget` (`47.5M / 50M`):** Non-forced `orchestrator`/`standard` step down to `gemini-4-hydrogen-medium` (`routine`). Explicit caller `--model=<id>` and `--force` always bypass soft-throttling.
   - Unify HTTP `429` (`RESOURCE_EXHAUSTED`) retry across **both** `SpawnTask` and `SpawnSubagent` using a multi-rung family ladder: `gemini-4-carbon-high` $\rightarrow$ `gemini-4-neon-high` $\rightarrow$ `gemini-4-neon-medium` $\rightarrow$ `fallback_on_429` (`gemini-4-hydrogen-medium`).
3. **Wave-Aware Orchestrator Context Watermarks (`200k` / `300k` / `380k` on `512k` Ceiling):**
   - Keep `[HQ] Front Desk` cold-cache markers (`120k / 160k / 180k`) unchanged.
   - Replace the flat `120k` `2h`-muted task nudge with watermark-keyed stages (resetting automatically on `/compact` `CHECKPOINT`):
     - **`200k` (`Warn`):** One-time `PreToolUse` nudge to prefer diffs/scout digests over raw file dumps.
     - **`300k` (`Checkpoint`):** Auto-update `hq progress save` on `Stop` and prompt wave-boundary `/compact` before dispatching the next wave.
     - **`380k` (`Orchestrator Guard`, `LogicalTier == "orchestrator"` only):** Cap inline coordinator research calls (`read_file`/`grep_search`/`find_by_name`) at `3` per turn until `/compact` runs or work is delegated to a fresh subagent, keeping regular non-orchestrator coding sessions `100%` unrestricted.

---

## 6. Guardrails, Implementation Phases & Open Review Questions

- **Monolith LOC & Skill Ceilings (`Gate H1` & `Gate H5-F`):** Because `internal/hooks/hooks.go` (`405 / 415` LOC), `workspace_lifecycle.go` (`1,056 / 1,075`), `cmd/hq/main.go` (`573 / 615`), and `kernel.go` (`1,949 / 2,015`) sit near `H5-F` ceilings, new code lives in `internal/routing/budget_throttle.go`, `internal/kernel/orchestrator.go`, `internal/hooks/orchestrator_hooks.go`, and `cmd/hq/tasks_cli.go` (`0` LOC added to `hooks.go` or `main.go`). `templates/skills/swe-pulse/SKILL.md` stays `<= 95 / 150` lines with `0` external dependencies (`Gate H4-A`) and `0` PII (`P1`–`P6`).

| Phase | Scope | Verification |
| :--- | :--- | :--- |
| **Phase 2A (`routing`, `obs`, `ls_client`)** | `orchestrator` tier + `reOrchestratorTask`, `deep_routine` `standard` bucket fix, `"neon"`-before-`"pro"` billing/fallback fix, `--model=inherit|flash` parity, & `80%/95%` `ResolveWithBudget`. | Unit tests in `internal/routing` & `internal/obs` + updated `Gate H3` golden routing matrix. |
| **Phase 2B (`kernel`, `hooks`, `cmd/hq`)** | `--orchestrator` & `--fence` on `hq spawn`/`hq subagent`, `hq worktree verify-fence`, `SpawnSubagent` `429` ladder, `PreToolUse` native `invoke_subagent` parity bridge, & `200k/300k/380k` orchestrator watermarks + `SessionStart` role card. | Unit tests for fence denials, `git diff` fence verification, `429` step-down, `invoke_subagent` `Model` rewrites, & post-`CHECKPOINT` watermark reset. |
| **Phase 2C (`swe-pulse`, `AGENTS.md`, `CUJ-50`)** | 3-wave `templates/skills/swe-pulse/SKILL.md` (`hq skill sync`), `.agents/plugins/agy-hq/rules/AGENTS.md` routing matrix update, & `docs/product/0003-critical-user-journeys.md` (`CUJ-50`). | **`TestCUJ50_NeonOrchestratorMultiWaveFenceAndThrottling`** E2E test + `100%` `./bin/hq ci pre-push` & `go test -race ./...`. |

### Open Review Questions for Sign-Off
1. **Default `orchestrator` Depth (`gemini-4-neon-high` vs. `gemini-4-neon-medium`):** Should `--orchestrator` / `routing.tiers.orchestrator` default to **`gemini-4-neon-high`** (recommended—matches all 4 flagship Phase-2 runs, stepping down automatically to `neon-medium` on `--depth=medium` or at `>= 80%` daily budget), or default to **`gemini-4-neon-medium`** and require `--depth=high` for `neon-high`?
2. **`deep_routine` (`sleep-consolidate`) Budget Bucket:** Confirm moving `deep_routine` (`gemini-4-neon-high`) from `CanonicalTier: "deep"` (`SubagentModel: "pro"`) to `CanonicalTier: "standard"` (`SubagentModel: "flash"` / `"inherit"`) so `Neon` consolidation/research runs never falsely suggest `Carbon` (`"pro"`) subagents or consume the `30M` `Carbon`/`Claude` deep cap?
3. **`380k` Orchestrator Context Guard (`Hard Cap` vs. `Soft Nudge`):** At `>= 380k` context on `--orchestrator` threads, do you prefer the proposed **`3`-lookup/turn mechanical `PreToolUse` cap** (forcing `/compact` or subagent delegation before `512k` cold-cache thrash) or a **non-blocking `PreToolUse` warning nudge** on every turn above `380k`?
