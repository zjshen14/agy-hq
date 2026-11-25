# RFC-0029: RSI Engine v2 — Trace-Grounded Synthesis, Anchored Drawback Verifier & Held-Out Ratchet

- **Status:** Draft v1 (Paused — Pending Holistic POV Alignment from [Research 24 Revision 2](../research/24_rsi_2026_frontier_and_vault_eval_audit.md); zero production code modified)
- **Created:** 2026-11-25
- **Author:** `[Task:rsi] Deep Research on @omarsar0 RSI Post` (`gemini-4-carbon-high`)
- **Builds On:** [RFC-0010 (Bounded Agent RSI)](./0010-agent-recursive-self-improvement.md), [RFC-0019 (Two-Tier RSI & Vault Overlays)](./0019-two-tier-rsi-vault-overlay.md), [RFC-0024 (Memory Engine v2 & Eval)](./0024-memory-v2-prediction-and-eval.md), [RFC-0026 (Scoped Delegates & Taint v2)](./0026-declarative-scoped-delegates.md), [RFC-0027 (1-Tap Proactive Decisions)](./0027-muse-proactive-actions.md)
- **Research:** [24 — 2026 RSI Frontier (`@omarsar0` Survey) & 17-Day Production Audit](../research/24_rsi_2026_frontier_and_vault_eval_audit.md)

---

## 1. One-Page Summary (TL;DR)

`agy-hq` already has strong **outer** RSI safety boundaries: a Ring-0 Immutable Trusted Computing Base (`tcb.go`), Two-Tier Vault Skill Overlays ([RFC-0019](./0019-two-tier-rsi-vault-overlay.md)), and SHA-256 human approval (`hq propose`). However, auditing **17 days of live Vault telemetry** (`2,590` observations $\rightarrow$ `70` hypotheses $\rightarrow$ **`0` active overlay files**) alongside the 2026 RSI literature reveals that the **inner** loop suffers from single-session noise, broken string templates, a rubber-stamp verifier, and split-brain counters. This RFC proposes **four clean, zero-Frankenstein fixes**:

| Problem Today (17-Day Live Audit) | What We Propose (`RSI v2`) |
| :--- | :--- |
| **1. Noisy $N=1$ mining & misattributed tool errors:** Parallel tool calls clobber `lastToolName` (`git` errors blamed on `read_file`), multi-turn transcripts are re-scanned from step `0`, one-off retries (`1 × 4 >= 4`) trigger rules, and model-capacity gaps are forced into `SKILL.md` text. | **Clean Trace Mining + Process-vs.-Content Routing (`miner.go`):** Pair tool calls `1:1` with tool results, honor `StepStartIdx` via `vault.WalkJournals`, require **$\ge 2$ distinct conversations** before proposing a rule (*Winner's Curse* guard), wire `taint.IsTainted` v2, and separate **Process Failures** (skill/CLI fix) from **Content/Capacity Failures** (model-tier routing or Ring-3 playbooks). |
| **2. Raw string-template slop:** `mutator.go` pastes whatever shell heredoc or chat sentence came next (`"When using edit_file, Worker 8fe788d4..."`) and turns empty proposal rejections into `"Honor user rejection feedback: "`. | **Two-Speed Trace-Grounded Synthesis (`mutator.go` + `hq rsi context`):** Restrict 0-token Go templates to clean same-binary flag repairs ($C_{\text{fail}} \rightarrow C_{\text{ok}}$) and noise thresholds; for everything else, `hq rsi context <obs-id> --json` gives `/sleep-consolidate` the scrubbed $\pm 5$-step failure+recovery trace window (`0` extra files on disk) to write a clean rule (`<= 2`/night). |
| **3. Rubber-stamp verifier (`27/27` pass on broken rules) & no held-out check:** `Gate 2B` only checks for an HTML comment—passing empty/garbled rules `100%`—while `Gate 2A` skips `go test` and `Gate 4` misses TLS/security-downgrade flags (`curl -k`, `--no-verify`). | **Anchored Drawback Verifier + "Trusting Trust" Guard + `hq rsi eval`:** Check candidate rules with deterministic drawback detectors tested in CI against a $\ge 12$-item **human-graded golden anchor set** (*Who Verifies the Verifier?*), block security-weakening flags (*Roesner & Kohno*), run real `go test` + `ci.CheckPII` on Tier-2 patches, and add **`hq rsi eval`** on a held-out conversation split. |
| **4. Orphaned drafts (`66/70`) & 30-day counter wipeout bug:** `archive.jsonl` increments `helpfulCount`, while `Ablate` and `Promote` read static `helpful="0"` from Markdown—breaking `Tier B -> C` promotion and wiping all rules at day 31. Rejected ideas also recur forever. | **Single-Source Canary Ledger, Tombstones & 1-Tap Staging (`proposer.go`):** Read canonical `helpfulCount`/`harmfulCount` from `archive.jsonl` (zero per-turn Markdown diffs), record rejected/expired/reverted rules as **Tombstones** so dead ideas never return (*No Second Run*), and add `hq rsi stage-nightly --max=2` surfacing 1-tap Morning Brief cards. |

---

## 2. What It Looks Like in Practice (Before vs. After)

### Example A — Parallel Tool Call & Multi-Step Recovery
- **Today:** In one session, an agent runs a failing `git commit` (`Author identity unknown`) alongside `read_file`, then posts *"Worker `8fe788d4` finished."* `miner.go` blames `read_file`, `mutator.go` emits `- When using read_file in gchat, Worker 8fe788d4...` from $N=1$ occurrence, `verifier.go` marks `Gate 1–4 = PASS`, and the file sits orphaned in `hypotheses/` forever.
- **With `RSI v2`:**
  1. `miner.go` pairs the exit-128 error strictly to `run_command (git)` and waits until the same friction recurs across **$\ge 2$ distinct conversations** (ignoring one-off $N=1$ noise).
  2. Once recurring, `hq rsi context <obs-id> --json` hands `/sleep-consolidate` the exact $\pm 5$-step failure + recovery trace and target skill; the authored rule passes all anchored drawback checks (`Gate 2B`) and appears in Morning Brief as a single 1-tap card (`[Approve Rule]` · `[Reject — Never Suggest Again]`).

### Example B — Empty Proposal Rejection vs. Canary Survival
- **Today:** You click `Reject` on an outdated chat draft without typing a reason. `mutator.go` generates `"When using hq_propose in general, Honor user rejection feedback: "` (`Gate 1–4 = PASS`). Meanwhile, a genuinely useful rule that succeeded `13` times in `archive.jsonl` still says `helpful="0"` in Markdown—so `hq rsi ablate` deletes it on day 31.
- **With `RSI v2`:**
  1. Reason-free rejections cancel the single action cleanly without inventing a global rule; rejections *with* feedback log a **Tombstone** in `archive.jsonl` so that pattern is never re-proposed.
  2. `Ablate` and `Promote` join directly against `archive.jsonl`, preserving proven rules (`helpfulCount = 13`) and graduating `helpfulCount >= 5` candidates cleanly without rewriting `SKILL.md` on every turn.

---

## 3. Framework (`agy-hq` Open Source) vs. Personal Vault (`<vault>/`)

We keep general-purpose verification and evaluation physics in `agy-hq` while storing all personal telemetry, learned rules, and tombstones strictly inside git-ignored `<vault>/`:

| Layer | Open-Source Framework (`internal/rsi/`, `cmd/hq/`) | Personal Vault (`<vault>/memory/rsi/`, `<vault>/skills/`) |
| :--- | :--- | :--- |
| **Verifier Anchors** | Generic golden anchor fixture (`internal/rsi/testdata/anchors.json`, $\ge 12$ good/bad generic rule pairs enforced in `go test`). | Optional user-specific anchor overrides (`<vault>/memory/rsi/anchors.jsonl`) and personal rejection tombstones. |
| **Rule Targets** | Immutable Core OS skills (`.agents/plugins/agy-hq/skills/`; Tier-2 `--upstream` uses isolated git worktrees). | `<vault>/skills/<slug>/SKILL.md` (Tier 1A) and `<vault>/memory/rsi/rules/<slug>.md` (Tier 1B; `0` dirty files in `agy-hq`). |
| **State & Eval** | Pure-Go stdlib miner, drawback detectors, security-downgrade AST/flag scanner, and `hq rsi eval` math (`0` external deps). | `<vault>/memory/rsi/{observations,archive}.jsonl` (locked under `store.RankMemory = 78`, `0600` permissions). |

---

## 4. Design of the Four Organic Upgrades

### 4.1 Accurate Trace Mining, Taint v2 & Process-vs.-Content Routing (`Phase 2A`)

1. **Four Deterministic Telemetry Fixes in `internal/rsi/miner.go`:**
   - **1:1 Tool-Call Pairing:** Match each tool result step to its exact originating `ToolCall` entry rather than overwriting a single `lastToolName` variable across parallel calls.
   - **Per-Turn Delta Watermarks & Shared Journal Walk:** Replace `miner.go`'s ad-hoc directory scan with `vault.WalkJournals` ([RFC-0024](./0024-memory-v2-prediction-and-eval.md)) and skip transcript steps `< je.StepStartIdx` so pinned multi-turn threads (`[HQ] Front Desk`) are mined in $O(\Delta\text{steps})$ instead of $O(T^2)$.
   - **Unified Taint v2 Exclusion:** Check `taint.IsTainted(vaultDir, je.ConversationID)` ([RFC-0017b](./0017b-provenance-taint-and-offer-gateway.md), [RFC-0026](./0026-declarative-scoped-delegates.md)) and `hq watch read` in addition to `je.TaintedSession` so quarantined or untrusted sessions can never seed Ring-1/Ring-2 observations.
   - **Cross-Session Recurrence Gate ($\text{DistinctConvs} \ge 2$):** Require `len(ev.ConversationIDs) >= 2` (or explicit `user_feedback` with non-empty `lesson`/`reason`) before an observation becomes eligible for rule synthesis—eliminating $N=1$ selection noise (*Hu & Tang, 2026*).
2. **First-Signal Failure Class (`failureClass`: `process` vs. `content` — *Tian et al., 2026*):**
   - **`process`** (`error_recovery_chain`, `pretool_friction`, `proactive_noise` across $\ge 2$ conversations) $\rightarrow$ eligible for Ring-2 `SKILL.md` / overlay rules (or Ring-1 CLI/hook fixes in `--upstream` mode).
   - **`content`** (`quota_anomaly` / `FaultModelRouting` or clean-execution quality interventions) $\rightarrow$ surfaced in `hq rsi status` / `/sleep-consolidate` as **Model-Tier Routing recommendations** (`<vault>/config.yaml`) or **Ring-3 Topic Playbook** candidates—never stuffed into procedural `SKILL.md` rules.

---

### 4.2 Two-Speed Trace-Grounded Synthesis (`Phase 2B`)

1. **Strict Fast-Path Guard (`0` LLM Tokens):** Pure-Go rule templates in `mutator.go` fire **only** when:
   - `Category == FrictionProactiveNoise` (deterministic threshold/tier adjustment from [RFC-0027](./0027-muse-proactive-actions.md)), **or**
   - `Category == FrictionErrorRecovery` where both failed command $C_{\text{fail}}$ and recovery command $C_{\text{ok}}$ are single-line CLI invocations sharing the same base executable (`argv[0]`, zero heredocs, zero prose, `len <= 140`).
2. **On-Demand Trace Context (`hq rsi context <obs-id> --json` + `--from-json`):**
   - For multi-step or nuanced `process` friction, `hq rsi context <obs-id> --json` extracts a PII-scrubbed, taint-verified context packet directly from Layer-4 `transcript.jsonl` (`0` new files or directories on disk): the $\pm 5$-step failure-and-recovery window, current target `SKILL.md`/overlay text, and any prior **Tombstones** for that skill.
   - During `/sleep-consolidate` (`deep-medium`), the dreaming agent inspects at most the **top `2` eligible observations** by `ExpectedSavedSteps` and submits clean `ACEDeltaOp` JSON via `hq rsi hypothesize --from-json`. Stale unproposed `draft` files older than `7d` are automatically pruned during `hq memory compact`.

---

### 4.3 Anchored Drawback Verifier, "Trusting Trust" Guard & `hq rsi eval` (`Phase 2C`)

1. **Inspectable Drawback Detectors + Human Anchor Set (`Gate 2B` — *Zhang et al., 2026*):**
   - Refactor `Gate 2B` into deterministic drawback detectors that reject any rule exhibiting: (a) empty or `< 24`-char actionable guidance after stripping boilerplate, (b) raw shell heredocs (`<<`), multi-line script dumps, or conversational status prefixes (`"Dispatched "`, `"Worker "`, `"Launched "`), (c) duplicate or near-duplicate token Jaccard ($\ge 0.70$) against existing active rules or Tombstones, or (d) missing falsification/kill-criteria terms on investing skills.
   - Ship `internal/rsi/testdata/anchors.json` ($\ge 12$ human-labeled good and bad candidate rules derived from real failure classes); `go test ./internal/rsi/...` enforces **`100%` pass/fail agreement** so the verifier can never silently regress into a rubber stamp.
2. **"Trusting Trust" Security-Downgrade Blocker (`Gate 1` — *Roesner & Kohno, 2026*) & Real Tier-2 `Gate 2A`/`3`:**
   - Extend `tcb.go` to hard-reject any rule or diff introducing security-weakening flags or patterns (`curl -k`, `--insecure`, `GIT_SSL_NO_VERIFY`, `NODE_TLS_REJECT_UNAUTHORIZED=0`, `StrictHostKeyChecking=no`, `ssl._create_unverified_context`, `--no-verify`, `chmod 777`).
   - In Tier-2 (`--upstream`) worktrees, `Gate 2A` executes real `go test -run <ReplayTestName>` (`FAIL` on unpatched `HEAD`, `PASS` with patch) and `Gate 3` runs full `ci.CheckPII` (`P1`–`P6`) + `go vet`.
3. **Held-Out & Compute-Normalized Evaluation (`hq rsi eval [--since=14d] [--json]`):**
   - Deterministically hashes untainted `conversationId` (`SHA-256 mod 2`) into **Selection (`50%`)** vs. **Held-Out (`50%`)** splits (*Hu & Tang, 2026*; *Ding et al., 2026*), reporting: (1) **Held-Out Friction Rate** (wasted error-recovery steps per 100 tool calls, before vs. after each rule's `AppliedAt`), (2) **Compute-Normalized Token Delta** (mean tokens/turn on matched skills via `internal/obs`), (3) **Proposal Funnel Health** (precision, orphan count, canary survival rate), and (4) **Anchor Verifier Agreement (`100%`)**.

---

### 4.4 Single-Source Canary Counters, Tombstones & 1-Tap Nightly Staging (`Phase 2D`)

1. **Single-Source Counters & Tombstones in `archive.jsonl`:**
   - Make `archive.jsonl` the **single canonical source** of `ruleId`, `helpfulCount`, `harmfulCount`, `sessionsObserved`, and terminal status (`canary`, `promoted`, `reverted`, `rejected`, `expired`).
   - Remove static `helpful="0" harmful="0"` reliance in `Ablate` and `findTierCPromotionCandidates` by joining against `archive.jsonl`—fixing the 30-day wipeout bug and unlocking `Tier B -> Tier C` promotion with **zero per-turn edits** to `SKILL.md` or `local/.git`.
   - When `hq reject <prop-id>` or proposal expiry runs on an RSI proposal, write a `status: "rejected"` **Tombstone** (with `sourceFingerprints` and `rejectionReason`) to `archive.jsonl` so `miner.go` and `mutator.go` never re-propose rejected ideas.
2. **Closed-Loop Nightly Staging (`hq rsi stage-nightly [--max=2]`):**
   - Verifies, deduplicates, and stages at most `2` top-ROI verified hypotheses per night, attaching [RFC-0027](./0027-muse-proactive-actions.md) 1-tap `DecisionOption` buttons (`approve_proposal`, `reject_proposal`, `dismiss_noisy`) for Morning Brief and `:4242`.

---

## 5. Safety & Engineering Guardrails

1. **Ring-0 TCB & Human Approval Unchanged:** `internal/rsi/{tcb,verifier,proposer}.go`, `internal/approvals/`, `hooks.json`, and `inbox-triage` remain strictly Ring-0 immutable; every Ring-1 and Ring-2 apply remains gated by SHA-256 `hq approve`.
2. **Atomic Locking & Zero External Dependencies:** All Vault writes in `internal/rsi/` (`observations.jsonl`, `archive.jsonl`, `hypotheses/*.json`) use atomic `0600` locked writes under `store.RankMemory = 78` (`Gate H5-C`), and `go.mod` remains at **`0` external dependencies** (`Gate H4-A`).
3. **Strict LOC Discipline (`Gate H5-F`):** Drawback detectors and held-out `eval` live in focused files (`internal/rsi/eval.go`, `internal/rsi/drawbacks.go`) so every file in `internal/rsi/` stays compact (`< 650` LOC).

---

## 6. Phased Implementation Plan (Pending Phase 1 Review)

| Phase | Scope | Key Verification |
| :--- | :--- | :--- |
| **Phase 2A (`P0`)** | `miner.go`: 1:1 parallel tool pairing, `StepStartIdx` + `vault.WalkJournals`, `taint.IsTainted` v2, `DistinctConvs >= 2` gate, `process` vs. `content` classification, & atomic `store` writes. | Unit tests for parallel tool attribution, `O(Δsteps)` watermark skip, taint v2 blocking, and $N=1$ noise rejection. |
| **Phase 2B (`P0`)** | `mutator.go` + `cmd/hq`: strict same-binary fast-path guard, `hq rsi context <obs-id> --json`, `hypothesize --from-json`, & `> 7d` stale draft compaction. | Unit tests proving heredocs/chat prose/empty rejections never template-interpolate, plus `context` PII scrubbing. |
| **Phase 2C (`P0`)** | `drawbacks.go`, `tcb.go`, `verifier.go`, `eval.go`: anchored drawback detectors + `testdata/anchors.json`, "Trusting Trust" flag blocker, real Tier-2 `go test` + `ci.CheckPII`, & `hq rsi eval`. | `100%` agreement on `testdata/anchors.json`, TLS-downgrade block tests, Tier-2 fail-then-pass replay test, & held-out `eval` test. |
| **Phase 2D (`P1`)** | `proposer.go` + `sleep-consolidate/SKILL.md`: `archive.jsonl` counter join (fixing 30d wipeout & `Tier B->C`), rejection tombstones, & `hq rsi stage-nightly --max=2`. | E2E journey verifying `mine -> context -> hypothesize -> verify -> stage-nightly -> approve/reject` with tombstones & `0` dirty repo files. |
