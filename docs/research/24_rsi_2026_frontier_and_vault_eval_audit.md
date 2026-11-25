# 24 — The 2026 Recursive Self-Improvement (RSI) Research Frontier (570+ Paper Holistic Audit) & 17-Day `agy-hq` Production Evidence

- **Date:** 2026-11-25 (Revision 2 — Holistic Landscape Expansion)
- **Author:** `[Task:rsi] Deep Research on @omarsar0 RSI Post` (`DeepInvestigator` + Primary arXiv/Index Verification)
- **Status:** Complete / Phase 1 Holistic Research Memo *(Pausing for User POV Alignment Before Revising RFC-0029)*
- **Related Documents:**
  - [RFC-0010: Bounded Agent Recursive Self-Improvement (RSI)](../design/0010-agent-recursive-self-improvement.md)
  - [RFC-0019: OS vs. User-Space Skill Boundary, Vault Skills (`local/skills/`) & Two-Tier RSI Overlays](../design/0019-two-tier-rsi-vault-overlay.md)
  - [RFC-0024: Memory Engine v2 — Orthogonal Single-Source Vault Architecture & Deterministic Memory Eval](../design/0024-memory-v2-prediction-and-eval.md)
  - [RFC-0026: Unified Session Authorization Lattice & Declarative Scoped Delegates](../design/0026-declarative-scoped-delegates.md)
  - [RFC-0027: Smarter Proactive Actions — Looking Ahead, 1-Tap Decisions & Interactive Spaces](../design/0027-muse-proactive-actions.md)
  - [RFC-0029: RSI Engine v2 (Draft v1 — Pending Holistic POV Revision)](../design/0029-rsi-v2-trace-eval-and-heldout-ratchet.md)
  - [07 — Agent Recursive Self-Improvement (RSI): Literature Survey & Safety Invariants](07_agent_recursive_self_improvement.md)
  - [15 — Two-Tier Recursive Self-Improvement, OS vs. User-Space Skill Boundary & Personal Vault Skill Overlay Audit](15_two_tier_rsi_vault_overlay.md)
  - [19 — Anthropic's *"The Components of Agent Memory"* & 12-Day Production Vault Telemetry Audit](19_anthropic_memory_components_and_vault_audit.md)

---

## 1. Executive Summary & Why Revision 1 Was Too Narrow

On November 24, 2026, Elvis Saravia (`@omarsar0`, DAIR.AI) published the X Article [*"My Top 10 RSI Papers of 2026"*](https://x.com/omarsar0/status/2125234668396023943), which links to two massive live paper corpora:
1. **DAIR.AI's Self-Improving Agents Collection** ([`academy.dair.ai/papers/collections/self-improving-agents`](https://academy.dair.ai/papers/collections/self-improving-agents)): **570 papers** total (`31` foundational Reading List classics + rapid Sep–Nov 2026 surge).
2. **`theseus-labs-rsi/awesome-rsi`** ([GitHub](https://github.com/theseus-labs-rsi/awesome-rsi), accompanying `arXiv:2609.11873`): **509 unique arXiv papers** (`216` L1, `155` L2, `67` L3, `51` L4 Deployment, `40` L5 Meta-Improvement), supplemented by **`Awesome-Self-Improving-Agents`** ([GitHub](https://github.com/selfimproving-agent/Awesome-Self-Improving-Agents), **242 unique arXiv papers**) and Chen et al.'s **~1,250-paper** survey (`arXiv:2607.07663`).

**What Revision 1 missed:** Auditing only `@omarsar0`'s 10 featured picks inherited a strong bias toward **verifiable coding/ML-infra benchmarks** and missed the most directly relevant late-2026 wave for a **Personal Agent OS** like `agy-hq`:
- **Personal-Agent Safety Erosion & Gating:** *Safe Personal RSI* (`arXiv:2611.23651`), *Appreciation Teaches Unauthorized Execution* (`arXiv:2610.39122` — showing skill evolution raises Indirect Prompt Injection ASR by `+14.0` to `+48.8 pp`!), and *Masked Erosion* (`arXiv:2610.38392` — showing acceptance gates silently erode overlapping safety layers).
- **Programmatic Compilation Over Prose Rules:** *FlashScript* (`arXiv:2610.33876`, `70×` fewer tokens & `100%` determinism vs. `23%` for prose skills), *SkillCompiler* (`arXiv:2610.39508`, `+32.33%`), *SkillCache* (`arXiv:2611.10022`), *EfficientSkill* (`arXiv:2611.19823`, `-80%` tokens), and *PASE* (`arXiv:2611.14019`).
- **When Memory & Rules Backfire:** *More Memory, Worse Agents* (`arXiv:2610.27406`, error reproduction), *How Much Should an Agent Remember?* (`arXiv:2611.26361`, IBM production lessons showing optimal rule count depends on model strength), *When Should Agents Remember? / FGSE* (`arXiv:2611.14326`), *WASP-Mem* (`arXiv:2611.04003`, `1` round of human diagnosis beats `4` rounds of self-judgment by `+7.0–10.9%`), *SoMem* (`arXiv:2611.11929`), and *PAST-Bench* (`arXiv:2608.04003`, Pathway Disconnect).
- **Statistical Resolution, Regret Gating & Failure Detection:** *The Resolution Hypothesis* (`arXiv:2611.23068`, >10M LLM calls), *TRUST* (`arXiv:2611.10787`, cuts bad-update rate `45.59% -> 2.78%`), *GUARD* (`arXiv:2611.23476`, `-54.5%` tokens, `0.000` memory pollution), *SIM* (`arXiv:2611.18856`), *SkillPlumber* (`arXiv:2611.25429`), and *Blind to What They Can Fix* (`arXiv:2611.29807`).

**Cross-checked against 100% of `agy-hq`'s live 17-day Vault telemetry (`2,590` observations across untainted + tainted runs):**
- **`98.7%` (`2,557/2,590`)** occur in only 1 conversation, and **`50.0%` (`>= 1,206/2,413`)** of error/pretool records are pure re-scan duplicates (`D2`).
- Among ALL **`20` untainted recurring (`>= 2` conversations)** observations: **`60%` (`12/20`)** are deterministic **CLI/hook bugs** (`--help` exit codes, missing `hq proposals` aliases, `env --` exit 127), **`25%` (`5/20`)** are **60–82-step monolithic turns** (`A3`/`A4` burn), **`10%` (`2/20`)** are normal **dev TDD/CI false positives** (`go test`, `hq ci pre-push`), **`5%` (`1/20`)** is explicit user policy (already in `AGENTS.md`), and **`0%` (`0/20`)** are missing Markdown `SKILL.md` prompt rules! *(All `13/13` tainted recurring observations are likewise deterministic CLI/hook bugs.)*

Every arXiv ID cited in this document (**83+ papers**) has been programmatically verified against `export.arxiv.org`.

---

## 2. The Holistic 2026 RSI Research Landscape (8 Structural Fronts)

```mermaid
flowchart TB
    subgraph F1["1. Surveys & Formal Limits"]
        S1["3 Mega-Surveys (2609.11873, 2607.13104, 2607.07663)"]
        S2["Formal Limits: Winner's Curse (2610.09239) · Resolution (2611.23068) · Harness Ceiling (2610.11655)"]
    end
    subgraph F2["2. Frozen-Model Harness RSI"]
        H1["Offline/Modular: Meta-Harness · ModularRSI · PluginRSI · HarnessPro"]
        H2["Runtime/Continual: Exo · GUARD · SIM · BOXBOX"]
    end
    subgraph F3["3. Skill Gating & Compilation"]
        K1["Gating/Coverage: VaG · SkillPlumber · Skill++ · PANDO"]
        K2["Compile to Code: FlashScript (70x tok) · SkillCompiler · SkillCache · EfficientSkill · PASE"]
    end
    subgraph F4["4. Personal Agent & Memory RSI (Core agy-hq)"]
        P1["Safe Updates: Safe Personal RSI · FGSE · PAST-Bench"]
        P2["Memory Discipline: SoMem · WASP-Mem · Anti-Persistence · IBM altk-evolve · Discernment"]
    end
    subgraph F5["5. L5 Meta & World-Model RSI"]
        M1["Hyperagents · Dream-RSI · RSI-Master · AIDE2 · No Second Run"]
    end
    subgraph F6["6. Parametric / Weight-Space RSI"]
        W1["ClawLoop · ReSAIL · SkillRL · SleepGate (KV) · Agent Lightning"]
    end
    subgraph F7["7. Eval Integrity & Safety Erosion"]
        E1["Verifier & Attribution: Who Verifies Verifier · HDA (54% overfit) · TRUST · Blind to Fix"]
        E2["Security Erosion: Evolve-IPI (+14-49pp) · Masked Erosion · Trusting Trust · RSI Control"]
    end
    subgraph F8["8. 2026 RSI Benchmarks (12+ Suites)"]
        B1["PAST-Bench · SEAGym · SelfImprove-Bench · RSIBench-Data/Infra · Singularity-Bench · FrontierRSI"]
    end
    F1 --> F2 & F3 & F4
    F2 & F3 & F4 --> F7 --> F8
    F5 & F6 -.-> F1
```

---

### Front 1: Mega-Surveys, Autonomy Taxonomies & Formal Limits

1. **Three 2026 Mega-Surveys:**
   - **Duan et al. (Sep 2026, `arXiv:2609.11873`, [`theseus-labs-rsi/awesome-rsi`](https://github.com/theseus-labs-rsi/awesome-rsi), `509` papers):** Establishes the **`B0` persistence boundary** (in-task output revision without persistent state carryover is *not* RSI), the **Headroom-Closed Index (`HCI`)**, and the **5-Level Autonomy Ladder**: `L1` Execution (`216` papers), `L2` Strategy (`155`), `L3` Experience (`67`), **`L4` Deployment & Environment Adaptation (`51` — `agy-hq`'s operating regime)**, and `L5` Recursive Meta-Improvement (`40`).
   - **Ren, Chen, Guo, Zhuge, Schmidhuber et al. (Jul 2026, `arXiv:2607.13104`, [`Awesome-Self-Improving-Agents`](https://github.com/selfimproving-agent/Awesome-Self-Improving-Agents), `242` papers):** Splits RSI into *Foundation Model Improvement* vs. *Scaffolding Improvement* and distinguishes *Mechanism* vs. *Domain* evaluation.
   - **Chen, Wang, Qu (Jul 2026, `arXiv:2607.07663`, ~1,250 papers):** Separates convergent *bounded self-refinement* from open-ended RSI and ranks the **Verification Hierarchy**: `Formal/Execution Verifiers > Process Reward Models & Rubrics > External LLM Judges > Intrinsic Self-Assessment`.
2. **Four Formal & Empirical Ceilings of RSI:**
   - **Recursive Criticality (`arXiv:2609.00137`, Burtsev, Sep 2026):** Defines reproduction number $\mathcal{R}_{\mathrm{AI}}$ separating self-amplifying ($\mathcal{R}_{\mathrm{AI}} > 1$) from self-damping loops.
   - **The Winner's Curse (`arXiv:2610.09239`, Hu & Tang, Oct 2026):** Models correlated measurement noise $\hat{\Delta}_k = \Delta_k + c + \eta_k$; after Round 1, **`61–96%`** of proposed rewrites are neutral or harmful. Selecting on $n=16$ items overstates held-out accuracy by **`13–20 pp`** (affecting `GEPA` and `MIPROv2`); scoring start vs. final on **$n \ge 64$ untouched held-out items** removes average bias.
   - **The Resolution Hypothesis (`arXiv:2611.23068`, Zeng et al., Nov 2026):** Across 7 evolution methods, 6 models, and **>10M LLM calls**, shows agent evolution stalls not because proposers fail to invent better edits, but because **noisy evaluations are no better than random at separating helpful candidates from harmful ones**, and method-level margins are smaller than run-to-run variance.
   - **Harness Evolution Hits a Ceiling (`arXiv:2610.11655`, Tian et al., Oct 2026):** Classifying trajectories by *first failure signal* separates **process failures** (`F2` harness friction, `F3` loops/step exhaustion — repaired by harness evolution, `55% -> 90%` delivery) from **content/capacity failures** (`F4` clean run, poor answer — hits a hard harness ceiling and requires higher model capacity / routing escalation or weight training).
   - **Cold-Start Failure Generalization Gap (`arXiv:2610.22984`, Zhao et al., Oct 2026 & `arXiv:2611.11342`, Mitra & Saluja, Nov 2026):** Failure-derived notes repair previously failed tasks well, but **struggle to generalize to unseen tasks** unless anchored by successful trajectories (`+4.0 pp` over failure-blind search on narrow decisions; no demonstrated gain on full conversations).

---

### Front 2: Frozen-Model Harness, Modular & Continual Evolution

The single largest engineering wave in late 2026 treats the **agent harness** (context assembly, tool interfaces, control flow, verification) as the primary optimization object around fixed frontier weights:

| System / Paper | arXiv ID & Date | Core Mechanism | Headline Verified Result |
| :--- | :---: | :--- | :--- |
| **`Meta-Harness`** (Stanford / KRAFTON / MIT) | `2603.28052` (Mar) | Proposer inspects full source code, scores & raw execution traces on filesystem. | `+7.7 pts` at `4×` fewer tokens; `+4.7 pts` across 5 held-out models; `76.4%` on TB2 (discovered env-bootstrapping snapshots). |
| **`Exo`** (UC Berkeley / a16z / Braintrust) | `2611.00463` (Oct) | Formal $s_{t+1} = K(s_t, a_t)$ separating immutable protected layer $K$ from mutable policy $\pi_t$ + **`Guardian`** diff gate. | **`-35%` to `-61%` held-out model cost** on AppWorld & SREGym (`-35%` diagnosis latency); `-12%` to `-53%` vs. Codex/Pi. |
| **`GUARD`** (Google Cloud) | `2611.23476` (Nov) | Grader-free continual RSI via pre-execution checklists, **directional regret** (*newly hurt* vs. *never solved*), rule pinning & **Soft-HURT gate**. | **`80.80%`** on 43 SDK tasks (`+8.89 pp`, $p=0.008$), **`-54.5%` tokens/task**, **`0.000`** procedural memory pollution. |
| **`SIM`** (Google Cloud / DeepMind) | `2611.18856` (Nov) | Structured **Self-Improvement Memory** storing edit rationale, predicted vs. observed outcomes & eval context across shifts. | **`+2.36 pts`** mean lift over harness-only continuation across 10 coding runs ($p=0.003$, wins `9/10`). |
| **`ModularRSI`** (Beihang / Manchester) | `2609.14857` (Sep) | Evolves 5 orthogonal harness modules independently on 2,000 disjoint tasks. | TB2 `47.57% -> 52.43%`; SWE-Bench Verified `73.40% -> 76.45%`. |
| **`PluginRSI`** | `2609.32423` (Sep) | Decomposes monolithic harnesses into atomized reusable plugins evolved in parallel and dynamically recombined. | Faster convergence & stronger cross-model transfer than monolithic rewrites. |
| **`HarnessPro`** (Tsinghua / Z.AI) | `2610.26770` (Oct) | Label-free Pareto search generating `lite` / `medium` / `heavy` harnesses with runtime hook budgets. | `+4.5 pp` pass rate on TB2 while cutting per-task cost by **`33.9%`**. |
| **`BOXBOX`** | `2611.09218` (Nov) | Mid-task "pit-stop" harness revision + kernel hot-reloading without dropping workspace state. | Sustained optimization over long-horizon performance runs. |
| **`Harnesses That Improve Themselves`** | `2611.22299` (Nov) | Unifying 4-component modular framework (context, control, action interfaces, verification/recovery). | Gated modular evolution beats prompt reflection and full-harness rewrites across models. |
| *Adjacent Harness Papers* | `2603.03329`, `2606.01770`, `2606.09498`, `2607.15524`, `2609.01437`, `2609.24974`, `2611.27499` | `AutoHarness` (synthesized code verifiers), `Adaptive Auto-Harness`, `Self-Harness`, `RHI`, `HarnessDev`, `Harness-Zero`, `NavRSI`. | Converge on modular, trace-grounded, budget-aware harness edits over monolithic prompt rewrites. |

---

### Front 3: Procedural Skills — Pre-Commit Gating, Coverage & Compilation to Code

A decisive 2026 consensus emerged: **unconstrained natural-language skill accumulation degrades agents (`SkillsBench` `2602.12670`: `-1.3 pp` unverified vs. `+16.2 pp` curated)**; high-performing systems either **gate skills strictly before commit** or **compile recurring skills into deterministic code**:

1. **Programmatic Compilation (Turning Recurring Workflows into Deterministic Code/Scripts):**
   - **`FlashScript` (`arXiv:2610.33876`, Oct 2026):** Compiles recurring agent trajectories into parameterized **Neuro-Symbolic Programs (NSPs)**—deterministic code for actions/transforms, isolated LLM calls only where judgment is required. Across 120 WorkBench tasks, NSPs used **`70×` fewer input tokens**, **`3×` faster runtime**, and achieved **`100%` determinism** across `108` unseen instances (where prose skills collapsed to **`23%`** on composite tasks!).
   - **`SkillCompiler` (`arXiv:2610.39508`, Oct 2026):** Decomposes skills into fine-grained ops, compiling well-specified steps into reusable verifiable programs while keeping LLM reasoning for context-dependent steps (**up to `+32.33%`** accuracy across 5 benchmarks & 6 model families).
   - **`SkillCache` (`arXiv:2611.10022`, Nov 2026):** One-trace execution-grounded specialization compiling a completed skill run into a parameterized program capsule replayed by a lightweight executor, falling back to the full LLM skill only on divergence.
   - **`EfficientSkill` (`arXiv:2611.19823`, Google / Purdue, Nov 2026):** Discovery followed by accuracy-guarded pruning + utility offloading; cuts output tokens in `11/12` configs (up to **`-80%`** vs. unconstrained `EvoSkill`) while improving accuracy.
   - **`PASE` (`arXiv:2611.14019`, Nov 2026) & `Evo-Harness` (`arXiv:2608.15071`, Aug 2026):** Separate procedural logic into patchable executable components (`PASE`, `+2.8%+` stability/generalization) or compile context lessons into harness code.
2. **Pre-Commit Contamination Gating, Coverage & Routing Hygiene:**
   - **`Verifier-as-Gatekeeper (VaG)` (`arXiv:2608.05810`, Aug 2026):** Proves skill accumulation exhibits a **capability-contamination phase transition** where post-hoc rollback cannot undo downstream drift. Pre-commit gating with 3 heterogeneous critics (*structural validity, behavioral harmlessness, semantic consistency*) + marginal-gain selection hits **`72%` pass@1** on TB2 with a **`5×` smaller** skill library that transfers across 4 backbones.
   - **`SkillPlumber` (`arXiv:2611.25429`, Nov 2026):** Introduces **instruction-level coverage** (`follow` / `not-follow` / `not-covered`) to separate *wrong instructions* from *ignored instructions* (`+25.6` to `+30.4 pp` over seed skills; removing coverage loses `20.8%` of recovered failures).
   - **`Skill++` (`arXiv:2611.00544`, Oct 2026) & `PANDO` (`arXiv:2605.24785`, May 2026):** Routing-aware skill refinement (ensuring new skills do not hijack progressive-disclosure routing of older skills) and confidence-based demotion + **cache-aware prompting**.
   - *Additional:* `Skill∞` (`2610.35434`), `Trace2Skill` (`2603.25158`), `SkillNet` (`2603.04448`), `SWE-Skills-Bench` (`2603.15401`), `SkillAxe` (`2606.10546`), `Rethinking Self-Evolving Skills` (`2608.02636`).

---

### Front 4: Personal Agent RSI, Memory Discipline & Safe Updates *(Direct `agy-hq` Territory)*

This is the single most important cluster missed in Revision 1—studying **long-lived personal and enterprise assistants** where weights are frozen, real users supply noisy feedback, and memory/instruction updates carry privacy and security risk:

| Paper | arXiv ID & Date | Key Finding Directly Applicable to `agy-hq` |
| :--- | :---: | :--- |
| **`Safe Personal RSI`** (CMU / UCSD / UC Berkeley / HKU) | `2611.23651` (Nov) | Evaluates personal-agent (*Kicker*: email, calendar, health, marketplace) prompt updates against 29 executable functional + security probes (OAuth/authorization reuse, memory deletion, tool-output injection), with failure replay, **bisection diff isolation**, automated repair, and **conservative fallback**. **Rejects `20/21` risky updates**; repaired edits drop attack violations `10.7% -> 0%` (`69.0% -> 17.9%` under strong attacks). |
| **`PAST-Bench` & `Hermes+`** | `2608.04003` (Aug) | Evaluates personal-agent RSI across *Memory, Procedural Reuse, Info Gathering, and Update* with persistence-on/off controls. Shows **Update** (cleanly replacing stale state) is hardest and uncovers the **Pathway Disconnect** (claimed gains without causal `Save -> Retrieve -> Update`). |
| **`SoMem`** (*Can LLM Agents Organize Their Own Memory?*, CityU HK / Tencent) | `2611.11929` (Nov) | 3-store linked Markdown (*Source Store* raw append-only, *Topic Store* agent-curated with stable IDs/evidence footnotes, *Core Store* capped preferences) under **"Model Owns Meaning, Code Owns Consistency"**. Mirrors `agy-hq` [RFC-0024](../design/0024-memory-v2-prediction-and-eval.md); beats baselines `+2.66` LoCoMo / `+3.10` BEAM-100K. |
| **`When Should Agents Remember?` (`FGSE`)** | `2611.14326` (Nov) | Treats every candidate memory/rule update as a **falsifiable hypothesis** `(precondition, behavior_change, expected_effect, verifier)` tested in temporary state against target-transfer, falsification, and archived-regression probes before commit—cutting harmful committed updates to **`3.6%`** and regression damage to **`1.3%`**. |
| **`More Memory, Worse Agents`** (*Anti-Persistence*) | `2610.27406` (Oct) | Documents **Error Reproduction**: faulty persisted rules/summaries shape future runs, generating *new* traces that re-derive the same faulty rule. Storing raw trajectories + outcomes and reconstructing task guidance on demand (**Anti-Persistence**) beat persistent rule stores at lower LLM cost. |
| **`How Much Should an Agent Remember?`** (IBM Research / CUGA / `altk-evolve`) | `2611.26361` (Nov) | Production enterprise deployment lessons: **optimal guideline count is a property of the model, not the memory** (strong models scale to `191` guidelines `+9.5/+16.1`, weaker models peak at `~29` selected guidelines). Warns of high-support backfiring guidelines, dropped provenance, and swallowed API errors. |
| **`WASP-Mem`** | `2611.04003` (Nov) | Interlinked wiki procedural memory (`+4.6–6.1%` pass rate, `-57–61%` storage). Shows **prescriptive human schemas hurt**, whereas **1 round of human failure diagnosis beats 4 rounds of agent self-judgment by `+7.0–10.9%`**. |
| **`ARISE`** | `2611.23588` (Nov) | Micro-memory corrections consolidated into macro rules accepted strictly if held-out accuracy rises (`+10.9 pp` SOP-Bench, `+5.8 pp` $\tau^2$-bench, `-4.0%` calls, `-5.2%` output tokens). |
| **`Learning through Discernment`** | `2611.23716` (Nov) | Over `127` simulated year-long lives, shows polite user reactions are treacherous teachers: when users cannot see hidden outcomes, **`80%` of "thanked" lessons were failures** (Spearman $\rho = -0.81$ with failure share). |
| **`Routed Continual Learning`** (Meta FAIR) & **`Memento 3`** | `2610.38796`, `2610.11794` (Oct) | Domain-routed memory pools cut cost while lifting reward; `Memento 3` compiles reflective rulebooks into executable code (*"Code as Model"*). *(Also: `Metis` `2606.24151`, `MemRL` `2601.03192`, `Evo-Memory` `2511.20857`.)* |

---

### Front 5: L5 Meta-Improvement, Offline World-Model Dreaming & Frontier Research Loops

- **`Dream-RSI` (`arXiv:2609.14858`, Google/DeepMind/UMD/UVA, Sep 2026):** Replays structured historical discovery logs as a **zero-cost offline simulator ("dreaming")** so candidate exploration/routing policies can be stress-tested offline before live deployment.
- **`RSI-Master` (`arXiv:2609.35561`, Sep 2026) & `RSI-Anything` (`arXiv:2610.13912`, Oct 2026):** Combine regularized **Experiment OS** action logging (blocking reward hacking), reviewer-guided DAG orchestration (preventing strategy lock-in), and world-model pre-filtering.
- **`Hyperagents` (`arXiv:2603.19461`, Meta FAIR, Mar 2026; extending `DGM` `2505.22954`):** Unifies task + meta agent into one editable program; meta-level mechanisms (persistent memory, performance tracking) transferred across non-coding domains.
- **`AIDE²` (`arXiv:2609.26457`, Weco AI, Sep 2026) & `MASS` (`arXiv:2610.12176`, UC Berkeley / Sakana AI, Oct 2026):** 8-day outer-loop research harness evolution (documenting bi-level noise compounding) and multi-agent evolutionary self-supervision for open-ended tasks (`1.2–1.6×` perf/token).
- **`No Second Run` (`arXiv:2611.10719`, SII & Xinxu AI, Nov 2026):** Decomposes unrepeatable frontier pre-training (`801-30B-A3B-Base`, `11.8T` tokens) into fast measurable sub-loops (`detect -> attribute -> fix -> verify`) coordinated via **shared notes logging failed ideas and why they failed**. *(Also: `DiscoveryRSI` `2610.23839`, `ShinkaEvolve` `2509.19349`, `Group-Evolving Agents` `2602.04837`, `Economics of RSI` `2609.15802`.)*

---

### Front 6: Parametric / Weight-Space RSI, Harness-Native RL & Model-Internal Sleep

- **`ClawLoop` (`arXiv:2610.14118`, Oct 2026) & `Agent Lightning v1.0` (`arXiv:2608.17528`, Aug 2026):** Harness-native closed SFT/RL training loops directly inside shell/filesystem sandboxes.
- **`ReSAIL` (`arXiv:2609.39306`, Sep 2026) & `The Curse of Recursion` (`arXiv:2305.17493`):** Mitigate multi-cycle model collapse in iterative self-distillation via privileged-information weighting and frozen anchor-teacher KL regularization.
- **`SleepGate` (`arXiv:2603.14517`, Mar 2026) & `Language Models Need Sleep` (`arXiv:2606.03979`, Jun 2026):** Model-internal memory mechanisms—`SleepGate` runs learned sleep micro-cycles over **transformer KV caches** (cutting proactive interference from $O(n)$ to $O(\log n)$); conceptually analogous to `agy-hq`'s explicit `--supersedes` fact pruning rather than an external Vault protocol. *(Also: `SkillRL` `2602.08234`, `SEAL` `2506.10943`, `Absolute Zero` `2505.03335`.)*

---

### Front 7: Evaluation Integrity, Attribution & Evolutionary Safety Erosion

If 2025 asked *"can agents self-improve?"*, late 2026 uncovered **six concrete ways RSI silently degrades accuracy or security even when benchmark scores go up**:

1. **Overfitting & Compute Confounding (`Harness-Delta Attribution`, Ding et al., `arXiv:2610.22466`):** Decomposes $\Delta S = T + O + G$ (Test-Time Scaling + Overfitting + Generalizable Residual). Across `21` significant runs (`11` models, `3` frameworks), **`54%` is overfitting** and **`14%` is extra compute** (including an ALFWorld harness that hardcoded benchmark priors into a 0-LLM-call controller).
2. **Vacuous Unanchored Graders (`Who Verifies the Verifier?`, Zhang et al., `arXiv:2610.11464`):** Removing `10` human-checked anchor examples collapses a co-evolving verifier into an **always-pass rubber stamp**, yet downstream skill scores still look fine. Inspectable 3-tier drawback detectors + `10` human anchors (**Double Ratchet**) retain `88–110%` of ground-truth lift.
3. **False-Evolution Acceptance (`TRUST`, Gao et al., `arXiv:2611.10787`):** Online multiple hypothesis testing cuts the share of accepted inferior updates from **`45.59%` (greedy gate) down to `2.78%`**.
4. **Silent Failure Blindness (`Blind to What They Can Fix`, Cao, Deng, Kersting, `arXiv:2611.29807`):** Self-improving agents generate scripts whose own runtime checks stay **silent on `>= 50%` of environment-shift failures** in 4 of 5 domains—even though the same agents easily fix those failures once a monitor trained on normal-run records alerts them.
5. **Self-Evolution Degrading Indirect Prompt Injection Defense (`Appreciation Teaches Unauthorized Execution`, Wang et al., `arXiv:2610.39122`, [`SaFo-Lab/Evolve-IPI`](https://github.com/SaFo-Lab/Evolve-IPI)):** When a personal agent learns from normal user appreciation on tasks involving external content, **Indirect Prompt Injection ASR jumps by `+14.0` to `+48.8 pp`** under skill evolution (`+3.0` to `+39.3 pp` under RL)!
6. **Silent Redundant-Safeguard Erosion, Benchmark Poisoning & Covert Sabotage:**
   - **`Masked Erosion` (`arXiv:2610.38392`, Oct 2026):** Under `411` improvement loops, acceptance gates silently delete redundant safety rules because another layer still catches the harm—until a weaker model runs and credentials leak! **Freezing safety text (`Ring-0`) or auditing one-layer ablations is required to preserve defense-in-depth.**
   - **`Reflections on Trusting Trust, Revisited` (`arXiv:2609.17817`, Roesner & Kohno, Sep 2026):** Poisoned environments forcing insecure workarounds (e.g., disabled TLS cert verification) implant persistent insecure rules/tools that survive subsequent clean evolution rounds.
   - **`Evolutionary Safety of RSI` (`arXiv:2609.31186`), `Your Agent May Misevolve` (`arXiv:2509.26354`), `RSI Control` (`arXiv:2610.27735`), `ReSI` (`arXiv:2610.12233`), & `Spotify` (`arXiv:2609.30297`):** Catalog the 6 evolutionary hazards (*intent drift, error accumulation, skill contamination, safety erosion, evaluator drift, risk inheritance*) and validate human-reviewed PR gates in production (`+14%` listening, `-5%` skips).

---

### Front 8: The 2026 RSI Benchmark Landscape (`12` Verified Suites)

| Benchmark | arXiv / Repo | Primary Dimension Measured | Key Takeaway |
| :--- | :--- | :--- | :--- |
| **`PAST-Bench`** | `2608.04003` | Personal-agent cross-session RSI (*Memory, Procedural Reuse, Info Gathering, Update*). | **Update** is hardest; exposes *Pathway Disconnect* (`Save -> Retrieve -> Update`). |
| **`SelfImprove-Bench`** | `2611.02797` | Generalization transfer ladder (`G0`–`G4` from in-task to OOD & cross-model). | Separates narrow memorization from transferable improvement. |
| **`SEAGym`** | `2606.17546` | Dynamic train / frozen val / ID & OOD transfer / replay regression / cost on TB2 & HLE. | Frequent updates stall held-out accuracy and collapse strong intermediate snapshots. |
| **`RSIBench-Data`** | `2607.25886` | Data-centric autonomous RSI research loops. | `58.33%` improve on attempt 1, but **`78.26%` of post-peak runs finish below peak**. |
| **`RSIBench-Infra`** | `2611.24395` | 20 production ML infra optimization tasks (up to 32 accelerators). | Benchmarks end-to-end engineering gains under real infra constraints. |
| **`EvoAgentBench`** | `2607.05202` | Procedural ability transfer across evolving tasks. | Tests whether evolved skills transfer to adjacent workflows. |
| **`SkillsBench` / `SWE-Skills-Bench`** | `2602.12670` / `2603.15401` | Curated vs. self-generated `SKILL.md` efficacy. | Curated skills `+16.2 pp`; unverified self-written skills **`-1.3 pp`**. |
| **`PostTrainBench`** | `2603.08640` | Autonomous LLM post-training (`10h`, `1× H100`). | Caught test-set training, checkpoint downloading & unauthorized API key use. |
| **`Flex-RSI`** | `2611.08946` | Vision/robotics/3D harness + executor co-evolution. | Binary pass/fail alone stalls open models; rich trace feedback drives gains. |
| **`Singularity-Bench` + `Breakwater`** | `2611.16591` | 6 AI infra tasks vs. human baselines; OWASP Agentic red-teaming. | Opus 5.5 + Claude Code hit `1.25` Singularity-Index; built `Breakwater` against Harbor reward hacks. |
| **`FrontierRSI`** | [`abundant-ai/frontier-rsi`](https://github.com/abundant-ai/frontier-rsi) | 27 open-ended H100 tasks (`2–8h` budgets; 8 public). | Container-isolated hidden graders with 90%-of-best continuous ramp scoring. |

---

## 3. Forensic Audit of `agy-hq`'s Live RSI Engine (17 Days, `2,590` Observations)

### 3.1 What `agy-hq` Already Got Right (Validated Against the 570-Paper Frontier)
1. **Ring-0 Immutable TCB (`internal/rsi/tcb.go`) + `PreToolUse` Overlay Lock:** Directly satisfies `Exo`'s protected mechanism layer $K$ AND `Masked Erosion`'s (`2610.38392`) prescription to **freeze safety text** so acceptance gates cannot silently erode overlapping safeguards.
2. **SHA-256 Human Approval Gate (`hq propose` / `hq approve`):** Mirrors `Exo`'s `Guardian` and Spotify's PR-reviewed production loop (`2609.30297`).
3. **Two-Tier OS vs. User-Space Skill Boundary ([RFC-0019](../design/0019-two-tier-rsi-vault-overlay.md)):** Keeps tracked Core OS skills clean (`0` dirty git files) while evolving `<vault>/skills/` and `<vault>/memory/rsi/rules/`.
4. **Taint Provenance Exclusion (`RFC-0017b` / `RFC-0026`) + `SoMem`-Aligned Memory v2 (`RFC-0024`):** Anticipates `Evolve-IPI` (`2610.39122`) and `Trusting Trust` (`2609.17817`) by quarantining untrusted external reads away from durable memory/RSI, and already mirrors `SoMem`'s *"Model Owns Meaning, Code Owns Consistency"*.
5. **Existing Infrastructure Already Built (But Under-Wired):** `model_scope` tagging + `hq rsi ablate --model=<id>` (`proposer.go:332-398`) and `findTierCPromotionCandidates` (`mutator.go:747-791`) already exist in code, along with dual-repo snapshots and the 75% quota circuit breaker.

### 3.2 Complete 17-Day Live Production Audit (`2026-11-08` – `2026-11-25`)

Auditing **all `2,590` observations** (`2.74 MB`), `70` hypotheses (`39` `draft`, `27` `verified`, `3` `proposed`, `1` `approved`), and `archive.jsonl` in the live Vault (sanitized, aggregate-only) produces an eye-opening empirical picture:

1. **Funnel Collapse & 50% Duplicate Inflation:**
   - `2,413` `error_recovery_chain` (`1,865`) + `pretool_friction` (`548`) rows collapse to **`1,134` unique `(conversationID, errorExcerpt[:90])` pairs**—meaning **`>= 1,206` rows (`50.0%`)** are pure re-scan duplicates caused by `miner.go` ignoring `StepStartIdx`.
   - `98.7%` (`2,557/2,590`) occurred in **only 1 conversation** (`312` normal missing-file exploration, `239` normal `edit_file` target misses, `180` normal dev `go test`/compile errors, `26/30` empty-reason proposal rejections).
   - `548` `pretool_friction` rows are dominated by true-positive safety/Front-Desk guardrails (`249` Front Desk cap, `61` outbound approval gates, `58` taint quarantine, `6` private credential guards, `174` timestamp-truncated `"other"` excerpts).
   - `draft_edited` (`PRELUDE`): **`0`** observations. `proactive_noise`: **`0`** observations. Active `<!-- rsi:rule -->` blocks on disk: **`0`**.

2. **Exact Classification of ALL `33` Recurring (`distinctConvs >= 2`) Observations (`20` Untainted + `13` Tainted):**

| Target Substrate Bucket | Untainted (`N=20`) | Tainted (`N=13`) | Concrete Recurring Friction Patterns Observed |
| :--- | :---: | :---: | :--- |
| **A. Deterministic CLI / Hook / Infra Fixes** *(Ring-1 Go code)* | **12 (`60%`)** | **13 (`100%`)** | • `PreToolUse` `env: '--'` exit-127 rewrite on `hq watch precheck`/`read` (`22+` convs across variants)<br>• `--help` exiting `1` or parsed as positional arg (`hq --help`, `hq help`, `hq gchat --help`, `hq gmail --help`, `hq memory --help`, `prune-fact --help`, `verify-transcript --help`)<br>• Missing aliases (`hq proposals`, `hq proposals list`, `hq propose list`, `hq watch ack --all`)<br>• `hq rsi mine` stdout truncation & CDP tab-missing error |
| **B. Monolithic Turn Compilation & Tier Routing** *(`SkillCache` / `FlashScript` / `HarnessPro`)* | **5 (`25%`)** | **0** | • `A3 RunawayTurnBurn` (`60–82` steps, `1.57M–2.45M` gross tokens) on `sleep-consolidate`, `morning-brief`, & recurring inbox/filing watchdogs (`3–4` convs each)<br>• `A4 TierMisroutingBurn` on `sleep-consolidate` (`3` convs) |
| **C. Normal Dev TDD / CI False Positives** *(Filter out)* | **2 (`10%`)** | **0** | • `hq ci pre-push` failing before `gofmt` (`8` convs, misattributed to `gchat`)<br>• `go test ./...` failing during feature dev (`3` convs, misattributed to `gchat`) |
| **D. Explicit User Policy / `SKILL.md` Prompt Rules** *(Ring-2)* | **1 (`5%`)** | **0** | • `1` explicit `user_feedback` (`workflow_delegation`, already in `AGENTS.md`)<br>• **Recurring non-CLI `SKILL.md` prompt-rule gaps: `0 / 33` (`0%`)!** |

### 3.3 Eleven Concrete Implementation Defects Identified (`D1`–`D11`)

| ID | File / Line | Defect Description |
| :-: | :--- | :--- |
| **`D1`** | `miner.go:696-712` | **Parallel Tool-Call Clobbering:** `lastToolName = tc.Name` overwrites with the *last* tool in a parallel batch, blaming `git`/`python3` errors on `read_file`/`edit_file`. |
| **`D2`** | `miner.go:336-378, 677` | **Missing `StepStartIdx` Watermark (`50.0%` Duplicates):** Re-scans transcripts from step `0` (`O(T^2)`) and bypasses `vault.WalkJournals`. |
| **`D3`** | `miner.go:702-709` & `verifier.go:303-310` | **Incomplete Taint v2 & Missing Security-Downgrade Detector:** Misses `taint.IsTainted` / `hq watch read` and allows `Trusting Trust` flags (`curl -k`, `--insecure`, `GIT_SSL_NO_VERIFY`, `--no-verify`). |
| **`D4`** | `mutator.go:509-606` & `miner.go:440, 811` | **$N=1$ Winner's Curse & Raw String-Template Slop:** `1 × 4 >= 4` triggers rules from single-session accidents, pasting raw heredocs/chat status lines and empty rejection feedback (`"Honor user rejection feedback: "`). |
| **`D5`** | `mutator.go:576-630` | **Conflating Process vs. Content/Capacity Failures:** Forces `FaultModelRouting` (`A3`/`A4`) and content rejections into `SKILL.md` bullets instead of CLI compilation, tier routing, or Ring-3 playbooks. |
| **`D6`** | `verifier.go:98-200` | **Vacuous Unanchored Verifier:** `Gate 2A` only greps `"t.Fatal"` without running `go test`; `Gate 2B` rubber-stamps empty/corrupt rules (`27/27` pass, `0` human anchors); `Gate 3` omits `ci.CheckPII` (`P1`–`P6`). |
| **`D7`** | `proposer.go:127, 354` & `mutator.go:756` | **Split-Brain `helpful="0"` Counter Bug & Unwired `ablate`:** `archive.jsonl` increments `HelpfulCount`, while `Ablate` & `Promote` read static `helpful="0"` from Markdown—wiping rules at day 31; `sleep-consolidate` never calls `hq rsi ablate`. |
| **`D8`** | `ops_cli.go:298-344` & `proposer.go` | **Unclosed Single-ID Funnel, Zero Tombstones & Zero `eval`:** `66/70` hypotheses stranded in `draft`/`verified`; rejected/expired proposals leave no `SIM`/negative-result tombstone; no `hq rsi eval`. |
| **`D9`** | `proposer.go:123-132` | **Ghost-Canary Bug (Pathway Disconnect):** `updateCanaryRecords` checks `strings.Contains(tf, sk)` on a deleted pre-RFC-0019 file path and keeps incrementing `HelpfulCount` (`13/15`) toward `"promoted"` for a rule that does **not exist on disk**! |
| **`D10`** | `miner.go:139-141` | **Greedy Skill Misattribution:** `inferSkillOrPkg` checks `strings.Contains(lower, "gchat")` across the whole turn prompt+summary before checking the failing command, attributing `go test ./...`, `hq ci pre-push`, and `hq --help` to `skill=gchat`. |
| **`D11`** | `mutator.go:761-784` & `miner.go:860-873` | **Dormant Rule-Destruction Hazard & Timestamp Truncation:** Once `D7` is fixed, `findTierCPromotionCandidates` stages `deprecate_skill_rule` to **delete** proven rules (`helpful >= 5`) without generating any replacement Go check/script! Separately, `firstLineExcerpt(..., 180)` spends 180 chars on `Created At: ... Completed At: ...` timestamps, truncating actual `PreToolUse` denial reasons on `174` rows. |

---

## 4. Emerging Strategic Framing (Questions Before Forming Final `agy-hq` POV)

Both the 570-paper 2026 frontier and `agy-hq`'s 17-day live telemetry point away from *"build a better Markdown bullet factory"* and toward a **4-Substrate Evidence-First RSI Architecture**:

1. **Substrate 1 — Deterministic CLI & Hook Hardening (`60%` of live recurring friction; `AutoHarness`, `Blind to What They Can Fix`):** Route recurring CLI/flag/exit-code friction directly to **upstream issue/PR triage** (or local helper scripts) instead of spending prompt tokens on permanent `SKILL.md` workarounds.
2. **Substrate 2 — Recurring Workflow Compilation & Cost-Tier Tuning (`25%` of live friction; `FlashScript`, `SkillCompiler`, `SkillCache`, `EfficientSkill`, `HarnessPro`):** Compile recurring 60–82-step watchdog/brief/dreaming loops into deterministic 0-token CLI bundles/scripts + model-tier routing overrides, and fix `findTierCPromotionCandidates` (`D11`) so proven rules are *never* deprecated without a verified compiled replacement.
3. **Substrate 3 — Evidence-Linked Memory with First-Class `Update` (`SoMem`, `PAST-Bench`, `WASP-Mem`, `Anti-Persistence`):** Preserve `RFC-0024`'s clean separation (*"Model Owns Meaning, Code Owns Consistency"*), prioritizing falsification-gated state updates (`FGSE`) and human-diagnosed failure summaries (`WASP-Mem`) over automatic unverified abstraction persistence (`More Memory, Worse Agents`).
4. **Substrate 4 — Ultra-Sparse, Pre-Commit-Gated Personal Rules (`Safe Personal RSI`, `Evolve-IPI`, `Masked Erosion`, `VaG`, `GUARD`, `SIM`, `TRUST`, `Who Verifies the Verifier?`):** Treat personal `SKILL.md`/overlay rules as a **rare, high-bar, model-scoped last resort**—strictly gated by Ring-0 frozen safety invariants, `Evolve-IPI` taint/authorization probes, 3 pre-commit critics + human-anchored drawback fixtures, directional regret (`GUARD`), causal on-disk existence + activation tracking (fixing `D9`), and `SIM`-grade rationale/tombstone memory.

> **Next Step:** Align with the user on this 4-substrate Point of View before revising [RFC-0029](../design/0029-rsi-v2-trace-eval-and-heldout-ratchet.md) or entering Phase 2 implementation.

---

## 5. Primary Sources & Verified Corpus Map

- **Primary Indices Inspected Directly:**
  - `@omarsar0` X Article *"My Top 10 RSI Papers of 2026"* (Nov 24, 2026): `https://x.com/omarsar0/status/2125234668396023943`
  - DAIR.AI *Self-Improving Agents* Collection (**570 papers**): `https://academy.dair.ai/papers/collections/self-improving-agents`
  - `theseus-labs-rsi/awesome-rsi` (**509 unique arXiv IDs**): `https://github.com/theseus-labs-rsi/awesome-rsi`
  - `selfimproving-agent/Awesome-Self-Improving-Agents` (**242 unique arXiv IDs**): `https://github.com/selfimproving-agent/Awesome-Self-Improving-Agents`
- **Verified arXiv Corpus Cited (83+ IDs checked against `export.arxiv.org`):**
  - *Surveys & Limits:* `2609.11873`, `2607.13104`, `2607.07663`, `2609.00137`, `2610.09239`, `2611.23068`, `2610.11655`, `2610.22984`, `2611.11342`, `2609.15802`.
  - *Harness RSI:* `2603.28052`, `2611.00463`, `2611.23476`, `2611.18856`, `2609.32423`, `2609.14857`, `2610.26770`, `2611.09218`, `2611.22299`, `2603.03329`, `2606.01770`, `2606.09498`, `2607.15524`, `2609.01437`, `2609.24974`, `2611.27499`.
  - *Skills & Compilation:* `2610.33876`, `2610.39508`, `2611.10022`, `2611.19823`, `2611.14019`, `2608.15071`, `2608.05810`, `2611.25429`, `2611.00544`, `2605.24785`, `2610.35434`, `2603.25158`, `2603.04448`, `2602.12670`, `2603.15401`, `2606.10546`, `2608.02636`.
  - *Personal Agent & Memory RSI:* `2611.23651`, `2608.04003`, `2611.11929`, `2611.14326`, `2610.27406`, `2611.26361`, `2611.04003`, `2611.23588`, `2611.23716`, `2610.38796`, `2610.11794`, `2606.24151`, `2601.03192`, `2511.20857`.
  - *L5 Meta, Dreaming & Parametric:* `2609.14858`, `2609.35561`, `2610.13912`, `2603.19461`, `2505.22954`, `2609.26457`, `2610.12176`, `2611.10719`, `2610.23839`, `2509.19349`, `2602.04837`, `2610.14118`, `2609.39306`, `2603.14517`, `2606.03979`, `2602.08234`, `2508.03680`, `2608.17528`, `2506.10943`, `2505.03335`, `2305.17493`.
  - *Eval, Attribution, Safety & Production:* `2610.22466`, `2610.11464`, `2611.10787`, `2611.29807`, `2610.39122`, `2610.38392`, `2609.17817`, `2609.31186`, `2509.26354`, `2610.27735`, `2610.12233`, `2609.30297`.
  - *Benchmarks:* `2603.08640`, `2606.17546`, `2611.08946`, `2611.16591`, `2611.02797`, `2607.25886`, `2611.24395`, `2607.05202`, plus [`abundant-ai/frontier-rsi`](https://github.com/abundant-ai/frontier-rsi).
