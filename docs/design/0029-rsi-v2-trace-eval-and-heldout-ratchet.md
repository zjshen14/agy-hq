# RFC-0029: RSI Engine v2 — Impact-Weighted Multi-Signal Mining, Compiled Skills, Anchored Safety Gating & Held-Out Ratchet

- **Status:** Draft v2 (Phase 1 — Aligned Holistic POV; awaiting final review before Phase 2 coding)
- **Created:** 2026-11-25 (Rev 2 — incorporates 570-paper holistic frontier + full impact-weighted 17-day Vault audit)
- **Author:** `[Task:rsi] Deep Research on @omarsar0 RSI Post` (`DeepInvestigator` + `gemini-4-neon-high`)
- **Builds On:** [RFC-0010 (Bounded Agent RSI)](./0010-agent-recursive-self-improvement.md), [RFC-0019 (Two-Tier RSI & Vault Overlays)](./0019-two-tier-rsi-vault-overlay.md), [RFC-0024 (Memory Engine v2 & Eval)](./0024-memory-v2-prediction-and-eval.md), [RFC-0026 (Scoped Delegates & Taint v2)](./0026-declarative-scoped-delegates.md), [RFC-0027 (1-Tap Proactive Decisions)](./0027-muse-proactive-actions.md)
- **Research:** [24 — The 2026 RSI Research Frontier (570+ Paper Holistic Audit) & 17-Day `agy-hq` Production Evidence](../research/24_rsi_2026_frontier_and_vault_eval_audit.md)

---

## 1. One-Page Summary (TL;DR)

`agy-hq` already has strong **outer** RSI safety walls: a frozen Ring-0 Trusted Computing Base (`tcb.go`), Two-Tier Vault skill overlays ([RFC-0019](./0019-two-tier-rsi-vault-overlay.md)), taint isolation ([RFC-0017b](./0017b-provenance-taint-and-offer-gateway.md)), and SHA-256 human approval (`hq propose`). However, auditing **570+ 2026 RSI papers** alongside **all `2,590` live Vault observations (`1,274` deduped instances, `1,888` wasted steps)** shows our **inner** loop is blind to the highest-impact signals, forces everything into broken `SKILL.md` templates, uses a rubber-stamp verifier, and has split-brain/ghost canary bugs. This RFC proposes **four clean, zero-Frankenstein upgrades**:

| Problem Today (17-Day Impact-Weighted Audit) | What We Propose (`RSI v2`) |
| :--- | :--- |
| **1. Fragmented crash-only mining (`55.9%` timestamp splits, `50%` duplicates, `0%` weight on user corrections & recurring clean runs):** `miner.go` hashes raw timestamps/IDs, clobbers parallel tools, blames `go test` on `gchat`, and misses user pushback + recurring high-token workflows. | **Impact-Weighted 4-Signal Mining (`miner.go`):** Normalize away timestamps/UUIDs/IDs/paths before fingerprinting, pair parallel tools `1:1`, honor `StepStartIdx` + step-local skill inference, drop TDD/`grep` exit-1 noise (`24.6%`), rank by **`distinct_convs × token_cost`** (`internal/obs`), and ingest **Conversational User Corrections** (*WASP-Mem*) + **Recurring High-Cost Workflows** (`A3` / clean repeats). |
| **2. Everything forced into broken `SKILL.md` templates:** Owned `hq` CLI bugs, `60–82`-step watchdogs (`25.4%` of wasted steps), and external connector flags (`90` failures across `50` convs) all get dumped into raw next-line string templates (`"When using edit_file, Worker 8fe788d4..."`). | **4-Substrate Router + Trace-Grounded Synthesis (`mutator.go` + `hq rsi context`):** Route each weighted cluster to its right target—**Ring-1 CLI/hook triage** for owned `hq` bugs, **executable script/CLI compilation** (*FlashScript*/*SkillCompiler*) or **model-tier routing** (*HarnessPro*) for `A3`/repeated workflows, and **on-demand trace context (`hq rsi context <obs-id> --json`, `0` new files)** for clean, sparse Ring-2 rules (`<= 2`/night) on external connectors (`gmail`/`gcalendar`/`xurl`) & user workflows. |
| **3. Rubber-stamp verifier (`27/27` pass on garbled rules), IPI/security erosion risk & no held-out check:** `Gate 2B` checks only an HTML comment (`0` human anchors); `Gate 2A` skips `go test`; no guard blocks TLS downgrades or external-content instruction overrides (*Evolve-IPI* `+14–49 pp` ASR!). | **3 Pre-Commit Critics, Human Anchors & `hq rsi eval` (`tcb.go`, `drawbacks.go`, `verifier.go`, `eval.go`):** Gate every candidate with (1) structural drawback ops locked to a $\ge 12$-item **human-graded anchor set** in CI (*Who Verifies the Verifier?*), (2) **Security & IPI Harmlessness** blocking TLS/hook downgrades (*Trusting Trust*) and external-content obedience rules (*Evolve-IPI* / *Safe Personal RSI*), and (3) duplicate/contradiction checks—plus **`hq rsi eval`** scoring `50/50` held-out conversation splits on token-normalized friction (*Winner's Curse* / *HDA* / *GUARD*). |
| **4. Ghost canary (`13/15` on a deleted file), 30-day wipeout bug, dormant rule-deletion bug & `66/70` stranded drafts:** `archive.jsonl` increments counts while `Ablate` reads `helpful="0"` in Markdown; `findTierCPromotionCandidates` deletes proven rules without a replacement; rejected ideas recur forever. | **Single-Source `SIM` Archive, Causal Canaries & 1-Tap Staging (`proposer.go`):** Make `archive.jsonl` the single source of counts, `modelScope`, `editRationale`, `predictedOutcome`, and **Rejection/Revert Tombstones** (*SIM* / *No Second Run*); require **on-disk rule existence + skill invocation** in canary tracking (fixing `D9` / *PAST-Bench*); wire `hq rsi ablate` into `/sleep-consolidate`; never delete a proven rule without a compiled replacement; and add `hq rsi stage-nightly --max=2` + `hq rsi compact`. |

---

## 2. What It Looks Like in Practice (Before vs. After)

### Example A — Recurring External Connector CLI Errors (`90` Hidden Failures Across `50` Convs)
- **Today:** Agents repeatedly try `gcalendar readonly events --from=...` or `gmail readonly message <id>`, failing across `50` conversations. Because every date/message ID differs, `miner.go` hashes each into a separate `N=1` row and ignores the pattern—or worse, blames `read_file` in `gchat`.
- **With `RSI v2`:**
  1. `miner.go` strips IDs/dates/timestamps, clusters all `gcalendar --from` misses into one high-impact cluster (`distinctConvs = 7`, weighted by wasted tokens), and tags it as an **external tool** (outside `agy-hq` Go code).
  2. During `/sleep-consolidate`, `hq rsi context <obs-id> --json` gives the agent the scrubbed $\pm 5$-step failure+recovery window and target skill; it writes one crisp, verified rule (or thin wrapper script) and surfaces a 1-tap Morning Brief card (`[Approve]` · `[Reject — Don't Suggest Again]`).

### Example B — `60–82`-Step Recurring Workflows (`25.4%` of All Wasted Steps)
- **Today:** An automated watchdog or nightly digest burns `65` steps and `1.8M` gross tokens with zero tool errors (`A3 RunawayTurnBurn`). `mutator.go` tries to append an `A3` prose bullet to `SKILL.md`, adding prompt weight without reducing runtime steps.
- **With `RSI v2`:** `RSI v2` classifies recurring `A3` / repeated multi-step runs into the **Workflow Compilation & Routing** bucket—recommending either compiling deterministic fetch/filter loops into a reusable script (`<vault>/skills/<slug>/scripts/` or `hq` subcommand, *FlashScript*/*SkillCache*) or adjusting `<vault>/config.yaml` model-tier routing (*HarnessPro*), and tracks post-change held-out token cost via `hq rsi eval`.

### Example C — Conversational User Corrections, Ghost Canaries & Safety Erosion
- **Today:** You correct an agent in chat (*"check the full paper corpus before writing an RFC"* or *"stop ignoring the other rows"*), and `miner.go` records **`0` observations** because every tool exited `0`. Meanwhile, a deleted pre-RFC-0019 rule still climbs toward `"promoted"` (`13/15` ghost canary), and an empty rejection passes all 4 gates.
- **With `RSI v2`:**
  1. `/sleep-consolidate` extracts explicit user corrections via `hq rsi record-correction` (`WASP-Mem` human diagnosis), linking the exact conversation and routing preferences to Ring-3 facts or sparse Ring-2 rules.
  2. `updateCanaryRecords` verifies the rule block actually exists on disk before crediting a session; empty rejections never create rules; and `Gate 1`/`2B` hard-blocks any rule weakening TLS/hooks or relaxing external-content quarantine (*Safe Personal RSI* / *Evolve-IPI*).

---

## 3. Framework (`agy-hq` Open Source) vs. Personal Vault (`<vault>/`)

| Layer | Open-Source Framework (`internal/rsi/`, `cmd/hq/`) | Personal Vault (`<vault>/memory/rsi/`, `<vault>/skills/`) |
| :--- | :--- | :--- |
| **Miners & Normalizers** | Pure-Go stdlib fingerprint normalizer, `1:1` tool matcher, TDD/`grep` noise filter, `internal/obs` token-cost join, and `hq rsi record-correction`. | `<vault>/memory/rsi/observations.jsonl` (atomic `0600` under `store.RankMemory = 78`). |
| **Verifier & Safety Probes** | Frozen Ring-0 `tcb.go`, 3 pre-commit critics (`drawbacks.go`), `testdata/anchors.json` ($\ge 12$ good/bad rule pairs + IPI/TLS attack probes enforced in `go test`), and `hq rsi eval`. | Optional personal anchors (`<vault>/memory/rsi/anchors.jsonl`), `SIM` archive ledger + rejection tombstones (`archive.jsonl`). |
| **Evolution Targets** | Core OS skills stay immutable (`0` dirty files in `agy-hq`); owned `hq` CLI/hook bugs emit clean Tier-2 worktree/issue triage reports (`--upstream`). | `<vault>/skills/<slug>/{SKILL.md,scripts/}` (Tier 1A), `<vault>/memory/rsi/rules/<slug>.md` (Tier 1B), and `<vault>/config.yaml` routing hints. |

---

## 4. Design of the Four Organic Upgrades

### 4.1 Impact-Weighted 4-Signal Mining (`Phase 2A` — `internal/rsi/miner.go`)

1. **Fix Telemetry & Fingerprinting Defects (`D1`, `D2`, `D3`, `D10`, `D11b`, `D12`):**
   - **Canonical Normalization (`D12`):** Strip `Created At: ... Completed At: ...` headers, RFC3339 timestamps, UUIDs, hex IDs (`--thread=`, `prop-`), and `<HOME>`/worktree paths before hashing `computeFingerprint`—and skip timestamp prefixes in `firstLineExcerpt` (`D11b`).
   - **1:1 Parallel Tool Pairing & Delta Watermarks (`D1`, `D2`):** Pair each tool result strictly to its matching `ToolCall`, walk journals via `vault.WalkJournals` ([RFC-0024](./0024-memory-v2-prediction-and-eval.md)), and skip steps `< je.StepStartIdx` (eliminating `50.0%` duplicate inflation).
   - **Step-Local Skill Attribution & Noise Filter (`D10`):** Infer `SkillOrPkg` from the failing step's command/file path *first* (never letting `"gchat"` in a Front Desk summary swallow `go test` or `hq --help`), filter normal dev TDD (`go test`/`go build`/`hq ci pre-push`), `grep` exit-1 no-match, and empty-reason rejections, and wire `taint.IsTainted` ([RFC-0017b](./0017b-provenance-taint-and-offer-gateway.md), [RFC-0026](./0026-declarative-scoped-delegates.md)) + `hq watch read` (`D3`).
2. **Impact Weighting & 2 New Signal Streams:**
   - Join each cluster against `internal/obs` to compute **`ImpactScore = DistinctConvs × MeanWastedTokens`** (falling back to `DistinctConvs × WastedSteps` when token metrics are absent), requiring `DistinctConvs >= 2` for automated mechanical rules (*Winner's Curse* guard).
   - **Stream 2 (Recurring Clean-Exit High-Cost Runs):** Flag repeated skill/task slugs exceeding step or token budgets across $\ge 2$ conversations—even with `0` tool errors—as **Compilation / Routing** candidates (*FlashScript* / *SkillCache*).
   - **Stream 3 (Human Corrections — *WASP-Mem*):** Add `hq rsi record-correction --skill=<slug> --conv=<id> --summary="..." --diagnosis="..."` invoked during `/sleep-consolidate` when transcript review finds explicit user pushback or rework.

---

### 4.2 4-Substrate Router & Trace-Grounded Synthesis (`Phase 2B` — `internal/rsi/mutator.go`)

1. **Right-Sized Target Classification (`TargetSubstrate`):**
   - **`ring1_cli_triage`** (owned `hq` subcommand/flag/hook bugs): Emit an actionable triage summary in `hq rsi status` / Morning Brief (or Tier-2 `--upstream` worktree patch) instead of a `SKILL.md` workaround.
   - **`compile_script_or_route`** (`A3`/`A4` or recurring multi-step workflows): Recommend a `<vault>/skills/<slug>/scripts/` helper or `<vault>/config.yaml` tier adjustment—never prose rule bloat.
   - **`ring2_skill_or_overlay`** (external connector quirks like `gmail`/`gcalendar`/`xurl`, noisy proactive triggers, or recurring workflow corrections): Eligible for sparse, model-scoped `ACEDeltaOp` synthesis.
   - **`ring3_memory`** (factual/env discoveries & user preferences): Handled by Memory v2 ([RFC-0024](./0024-memory-v2-prediction-and-eval.md)) `facts.jsonl` / `topics/<slug>.md`, never duplicated in `SKILL.md`.
2. **Two-Speed Trace Synthesis (`hq rsi context` + Strict Fast-Path):**
   - Pure-Go 0-token templates fire **only** on `FrictionProactiveNoise` ([RFC-0027](./0027-muse-proactive-actions.md)) or clean single-line same-binary CLI flag repairs (`argv[0]` matches, zero heredocs, zero prose, `len <= 140`).
   - Add **`hq rsi context <obs-id> --json`** (`0` new files on disk): extracts the PII-scrubbed, taint-free $\pm 5$-step failure+recovery window from Layer-4 transcripts, current target skill/overlay text, and prior **Tombstones** for that skill. `/sleep-consolidate` inspects the top $\le 2$ clusters by `ImpactScore` and submits clean `ACEDeltaOp` JSON via `hq rsi hypothesize --from-json`.

---

### 4.3 Three Pre-Commit Critics, Human Anchors & `hq rsi eval` (`Phase 2C` — `tcb.go`, `drawbacks.go`, `verifier.go`, `eval.go`)

1. **Pre-Commit Critics (*VaG*, *Safe Personal RSI*, *Evolve-IPI*, *Trusting Trust*, *Masked Erosion*):**
   - **Critic 1 — Structural & Semantic Drawback Detectors (`drawbacks.go`):** Reject empty/`< 24`-char guidance, heredocs (`<<`), multi-line script dumps, conversational status prefixes (`"Dispatched "`, `"Worker "`), and near-duplicate rules (token Jaccard $\ge 0.70$ against active rules or tombstones). Ship **`internal/rsi/testdata/anchors.json`** ($\ge 12$ human-graded good/bad rules + security probes) requiring **`100%` pass/fail agreement in `go test`** (*Who Verifies the Verifier?*).
   - **Critic 2 — Security & Indirect Prompt Injection Harmlessness (`tcb.go`):** Hard-block security-downgrade flags (`curl -k`, `--insecure`, `GIT_SSL_NO_VERIFY`, `NODE_TLS_REJECT_UNAUTHORIZED=0`, `StrictHostKeyChecking=no`, `ssl._create_unverified_context`, `--no-verify`, `chmod 777`) AND rules instructing the agent to obey commands/actions embedded inside external untrusted emails, chats, or web pages (*Evolve-IPI* `2610.39122`). Keep Ring-0 safety rules strictly frozen (*Masked Erosion* `2610.38392`).
   - **Critic 3 — Real Tier-2 Counterfactuals & PII Guard:** Execute real `go test -run <ReplayTestName>` (`FAIL` on unpatched `HEAD`, `PASS` on candidate) in Tier-2 worktrees (`Gate 2A`) and run full `ci.CheckPII` (`P1`–`P6`) + `go vet` (`Gate 3`).
2. **Held-Out Compute-Normalized Ratchet (`hq rsi eval [--since=14d] [--json]`):**
   - Deterministically partitions untainted conversations (`SHA-256(convId) mod 2`) into **Selection (`50%`)** vs. **Held-Out (`50%`)** splits (*Winner's Curse*, *HDA*), reporting: (1) Held-out friction rate (wasted steps per 100 tool calls, pre vs. post `AppliedAt`), (2) Compute-normalized tokens/turn per skill (`internal/obs`), (3) **Directional regret** (*GUARD*: newly regressed skills flagged immediately), and (4) Proposal precision, orphan count & anchor agreement.

---

### 4.4 Single-Source `SIM` Archive, Causal Canaries & 1-Tap Staging (`Phase 2D` — `proposer.go`)

1. **Single-Source `SIM` Archive (`archive.jsonl`):**
   - Make `archive.jsonl` the single canonical store for `ruleId`, `helpfulCount`, `harmfulCount`, `sessionsObserved`, `modelScope`, `editRationale`, `predictedOutcome`, and terminal statuses (`canary`, `promoted`, `reverted`, `rejected`, `expired`).
   - **Fix `D9` (Ghost Canary / *PAST-Bench* Pathway Disconnect):** `updateCanaryRecords` verifies the exact `<!-- rsi:rule id="..." -->` block **still exists on disk** before incrementing `helpfulCount`; deleted/migrated files transition cleanly to `status: "archived"`.
   - **Fix `D7` & `D11a`:** Join `Ablate` and `Promote` against `archive.jsonl` (zero per-turn Markdown churn), wire `hq rsi ablate --model=<active>` into `/sleep-consolidate` Stage 4, and change `findTierCPromotionCandidates` (`helpful >= 5`) to emit a **Compilation Advisory** rather than a destructive `deprecate_skill_rule` diff (never deleting a proven rule until its compiled script/CLI passes tests).
   - **Negative-Result Tombstones:** Proposal rejection (`hq reject`) or canary revert writes a tombstone to `archive.jsonl` so rejected patterns are never re-proposed (*No Second Run*).
2. **Closed-Loop Nightly Batching & Backlog Cleanup:**
   - Add **`hq rsi stage-nightly [--max=2]`** (verify + stage top $\le 2$ candidates with [RFC-0027](./0027-muse-proactive-actions.md) 1-tap Morning Brief buttons) and **`hq rsi compact`** (auto-expiring `> 7d` orphaned `draft`/`verified` files so pre-v2 backlog clears cleanly).

---

## 5. Safety & Engineering Guardrails

1. **Frozen Ring-0 TCB & Human Guardian:** `tcb.go`, `verifier.go`, `proposer.go`, `internal/approvals/`, `hooks.json`, and `inbox-triage` remain strictly Ring-0 immutable (*Masked Erosion* defense); every Ring-1 and Ring-2 apply remains gated by SHA-256 `hq approve`.
2. **Atomic Locking, `0` External Deps & Compact Modules:** All Vault RSI writes (`observations.jsonl`, `archive.jsonl`, `hypotheses/*.json`) use atomic `0600` locked writes under `store.RankMemory = 78` (`Gate H5-C`); `go.mod` stays at **`0` external deps** (`Gate H4-A`); new logic splits cleanly into `internal/rsi/drawbacks.go` and `internal/rsi/eval.go` (`< 650` LOC each, `Gate H5-F`).

---

## 6. Phased Implementation Plan (Awaiting Final Review)

| Phase | Scope | Key Verification |
| :--- | :--- | :--- |
| **Phase 2A (`P0`)** | `miner.go`: canonical fingerprint normalization (`D12`), `1:1` parallel tool pairing (`D1`), `StepStartIdx` + `vault.WalkJournals` (`D2`), step-local skill inference + TDD/`grep` filter (`D10`), `taint.IsTainted` v2 (`D3`), `obs` token-cost weighting, recurring clean-run detection, `hq rsi record-correction`, & atomic `store` writes. | Unit tests proving timestamp/ID normalization clusters external CLI errors, `O(Δsteps)` zero-duplicate mining, zero `gchat` misattribution, and taint v2 blocking. |
| **Phase 2B (`P0`)** | `mutator.go` + `cmd/hq`: 4-substrate router (`ring1_cli_triage`, `compile_script_or_route`, `ring2_skill_or_overlay`, `ring3_memory`), strict same-binary fast-path guard, `hq rsi context <obs-id> --json`, and `hypothesize --from-json`. | Unit tests proving `hq` bugs & `A3` runs route off `SKILL.md`, heredocs/prose/empty rejections never interpolate, and `context` scrubs PII. |
| **Phase 2C (`P0`)** | `drawbacks.go`, `tcb.go`, `verifier.go`, `eval.go`: 3 pre-commit critics, `testdata/anchors.json` (`100%` CI agreement), `Trusting Trust` + `Evolve-IPI` blockers, real Tier-2 `go test` + `ci.CheckPII`, and `hq rsi eval` (`50/50` held-out + directional regret). | Anchor suite `100%` pass/fail agreement, TLS + IPI rejection tests, Tier-2 fail-then-pass replay, and held-out `eval` unit tests. |
| **Phase 2D (`P1`)** | `proposer.go` + `sleep-consolidate/SKILL.md`: single-source `SIM` `archive.jsonl`, causal on-disk canary check (`D9`), `Ablate` counter join + nightly wiring (`D7`), safe non-destructive `Tier B->C` advisory (`D11a`), rejection tombstones, `hq rsi stage-nightly --max=2`, and `hq rsi compact`. | E2E journey verifying `mine -> context -> hypothesize -> verify -> stage-nightly -> approve/reject` with causal canary, tombstones, `compact`, and `0` dirty repo files. |
