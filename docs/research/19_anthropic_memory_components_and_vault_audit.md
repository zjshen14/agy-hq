# 19 — Anthropic's *"The Components of Agent Memory"* & 12-Day Production Vault Telemetry Audit

- **Date:** 2026-11-18
- **Status:** Phase 1 Research Complete
- **Primary External Source:** Lance Martin (Anthropic Engineering), [*"The components of agent memory"*](https://www.anthropic.com/research/components-of-agent-memory) (Nov 6, 2026)
- **Empirical Corpus:** Read-only, PII-scrubbed telemetry audit across 12 days of live `agy-hq` Vault production usage (`226` conversations, `1,337` turn journals, `31,526` execution steps, `72` bi-temporal facts, `11` topic wiki pages, `6` entity dossiers, `19` progress checkpoints, `36` domain notes, and `1,754` RSI friction observations) + host audit of Google Antigravity `v2.29` / CLI `1.5.11`
- **Companion Design:** [RFC-0024: Memory Engine v2 — Prediction-Utility Fact Scoring, Dual-Cap Anti-Hoarding Budgets, Scoped Operational Playbooks, Pure-Go Hybrid Search & Quantitative Memory Eval](../design/0024-memory-v2-prediction-and-eval.md)
- **Related Prior Docs:** [RFC-0002](../design/0002-memory-and-consolidation.md), [RFC-0006](../design/0006-memory-engine-upgrade.md), [RFC-0012](../design/0012-token-observability.md), [RFC-0014](../design/0014-proactive-engine-and-living-context.md), [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md), [RFC-0018](../design/0018-always-on-agent-runtime-hardening.md), [RFC-0019](../design/0019-two-tier-rsi-vault-overlay.md), [Research 04](04_antigravity_audit.md), [Research 08](08_token_observability_and_telemetry.md)

---

## 1. Executive Summary

Anthropic's November 6, 2026 engineering study, ***"The components of agent memory"***, provides the most comprehensive empirical evaluation to date of cross-session file-based agent memory across continual database learning (*CL-Bench 1.0*), long-horizon state navigation (*Pokémon Red*), enterprise document synthesis (`350k` files / `420M` tokens at *Stainless*), and legal contract brief generation (*Harvey LAB*).

Auditing Anthropic's findings side-by-side against 12 days of real-world multi-domain `agy-hq` production telemetry reveals a striking duality: **`agy-hq`'s foundational 4-layer architecture ([RFC-0006](../design/0006-memory-engine-upgrade.md)) and security boundaries ([RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md)) are strongly validated—and on untrusted-channel taint isolation, decisively ahead—yet real-world usage has already triggered eight concrete memory pathologies that Anthropic's experiments predict:**

1. **`SessionStart` Layer-1 Bloat (`30,901` Bytes / `~7,725` Tokens vs. `< 1,500`-Token Target — `5.15×` Over Budget):**
   - Although `local/MEMORY.md` sits at `39 / 80` lines (`[OK]` in `hq memory status`), nightly dreaming gamed the line-only cap by packing `175–350` byte semicolon-chained paragraphs onto single lines (`6,844` bytes / `~1,711` tokens).
   - Worse, `MEMORY.md`'s `## Knowledge Map (Topic Wiki)` (`2,186` bytes) and `HandleSessionStart`'s auto-discovered `## HQ Knowledge Wiki` (`3,473` bytes) inject all `11` topic summaries **twice verbatim on every session start**, while `Active Vault Facts` adds another `8,834` bytes (`~2,208` tokens) and `USER.md` (`4,903` bytes) repeats rules already enforced in `.agents/plugins/agy-hq/rules/AGENTS.md`.
2. **Chronological LIFO Fact Starvation (`100%` of `architecture` & `ux` Facts Evicted in `lean_auto`):**
   - `vault.LoadActiveFacts` slices `active[len(active)-limit:]` (`25` in `full`, `8` in `lean_auto`). After just 12 days (`37` active facts, growing `+3.1/day` net), LIFO silently evicts `32.4%` (`12/37`) of active facts from `full` sessions and **`78.4%` (`29/37`) from `lean_auto` workers—evicting `8/8` (`100%`) of `architecture` invariants, `4/4` (`100%`) of `ux` invariants, and `1/1` (`100%`) of `gchat` facts** while injecting recent personal/calendar facts into unrelated software engineering subagents.
3. **Missing Bi-Temporal Expiry TTLs & Misplaced Procedural Rules in `facts.jsonl`:**
   - **`27.0%` (`10/37`)** of active facts encode transient date-bound events or deadlines with zero `expires_at` field (remaining active until an LLM manually runs `hq memory prune-fact`), while **`35.1%` (`13/37`)** encode procedural `"when X, always/never Y"` workflow rules that belong in progressive-disclosure skills or scoped playbooks rather than global atomic fact slots.
4. **Low Domain-Matched Topic Wiki Recall (`42.1%`) & Unlinked Island Topology (`1` Cross-Link Across `11` Pages):**
   - Across `172` untainted conversations where `SessionStart` injected the Topic Wiki index, agents called `read_file` on any topic page in `28.5%` (`49/172`) of sessions—and when the initial prompt clearly matched an existing topic domain (`N = 19`), agents read the matching `topics/<slug>.md` only **`42.1%` (`8/19`)** of the time. Inline summary leakage in `MEMORY.md`/`facts.jsonl` and dilution inside a `30.9 KB` `SessionStart` payload suppress on-demand retrieval, directly mirroring Anthropic's CL-Bench unsteered recall gap (`< 50%` on Sonnet 4.6 vs. `90%` on Sonnet 5). Meanwhile, the `11` topic pages form isolated islands (`1` inter-topic cross-reference total).
5. **Missing "Wiki + Search" Hybrid Retrieval (`hq memory search`):**
   - Anthropic's Stainless benchmark proved **Wiki + Search (`61%`) > Search alone (`53%`) > Wiki alone (`48%`)**, saving `~20,000` tokens/question. `agy-hq` currently provides `hq memory lookup` *exclusively* for `entities/*.md` (`6` calls across `219` transcripts) with zero lexical search CLI across `topics/`, `facts.jsonl`, `notes/`, or `journal/`.
6. **Rigid RSI Noise vs. Anthropic's "Purpose Over Rigid Rules" (`1,754` Observations $\rightarrow$ `0` Active Rules):**
   - Anthropic found that rigid extraction rules (`"record only failures"`) degraded memory gains by **`−0.234` on Opus 5** and **`−0.150` on Opus 4.7**. In `agy-hq`, `hq rsi mine` accumulated **`1,754` raw tool-error observations** (`50` rigid template hypotheses, `1` ever approved, **`0` active `<!-- rsi:rule -->` blocks** across `<vault>/memory/rsi/rules/` and `<vault>/skills/`), prompting nightly dreaming to bypass RSI and write global prose rules into `USER.md`, `MEMORY.md`, and `facts.jsonl` instead of repo/scope-bound playbooks.
7. **Unlocked Multi-Writer File Mutations & Orphaned Lifecycle Stores:**
   - Anthropic's Harvey LAB contract test showed concurrent shared-file writes dropped fact survival from `100%` to **`72%` (`41/57`)**. In `internal/vault/`, `AddFact`, `PruneFact`, `CompactFacts` (`0644` non-atomic write), and `UpsertEntity` bypass `internal/store` `flock` (`RankMemory` is absent). Meanwhile, `local/memory/progress/` holds **`19` active checkpoints and `0` archived** (`CompactTasks` never calls `ArchiveProgress`), and `journal/` holds `1,337` individual JSON files across 12 days (`~40,000` files/year uncompacted).
8. **Zero Quantitative Memory Evaluation (`hq memory eval`):**
   - `hq memory status` audits static line counts and dead task slugs, but tracks zero closed-loop quality metrics (Wiki recall, fact hit precision, scoped eviction starvation, repeat error recurrence, or user correction rates).

---

## 2. Deep Analysis of Anthropic's *"The Components of Agent Memory"* (Nov 6, 2026)

### 2.1 Scope Definition & Architectural Hierarchy

Anthropic defines **agent memory** strictly as *knowledge gained from experience across user interactions and assigned tasks that persists between independent sessions as readable context*—explicitly excluding:
- **Weight updates** (gradient training / fine-tuning),
- **Single-session in-context learning** (ephemeral within one trajectory), and
- **Context compaction** (lossy intra-session turn summarization when approaching context window limits).

Drawing on hippocampal rapid encoding versus offline neocortical replay during sleep, Anthropic organizes cross-session memory into **four layers** maintained by offline **dreaming** (sleep-time compute, building on Letta / UC Berkeley 2025):

| Layer (Bottom $\rightarrow$ Top) | Anthropic Formulation | Primary Role & Failure Mode if Misused |
| :--- | :--- | :--- |
| **1. Transcripts** *(Bottom)* | Searchable, immutable raw event log of what actually occurred in each session. | **Ground truth.** Expensive to scan raw at test time, but essential for verifying self-reported notes and searching unindexed details. |
| **2. Memory Notes** | Salient observations, tested rules, corrections, or failure traps tagged in-moment or extracted post-hoc. | **Prediction unit.** Fails when models hoard redundant observable state (`413` files in Pokémon) or record untested guesses (`Sonnet 4.6`). |
| **3. Memory Representations** | Structured, cross-linked markdown collections (such as self-maintained **wikis** or per-contract dossiers). | **Navigation map & multi-doc synthesis.** Fails under **Borges's Map Trap** (wiki `1.77×` larger than raw docs) or **multi-writer overwrite races** (`72%` survival). |
| **4. Injected Context** *(Top)* | Tightly curated header/index preemptively loaded into every session (e.g., Claude Code loads only the first **`200` lines or `25 KB`** of `MEMORY.md`). | **Zero-search orientation.** Fails when bloated: longer instruction files reduce adherence, waste prompt tokens, and propagate stale errors into every run. |

---

### 2.2 Quantitative Benchmark Synthesis Across Anthropic's Four Evaluations

#### A. *CL-Bench 1.0* Database Continual Learning (`30` Sequential Sessions)
Anthropic framed memory writing as **a prediction about what future sessions will need without re-deriving it**, running 30 sequential database QA sessions in *Claude Managed Agents* with a persistent memory folder:

| Model | Stateless Baseline | With Memory Folder | Gain ($\Delta$) | Observed Memory Behavior |
| :--- | :---: | :---: | :---: | :--- |
| **Claude Sonnet 4.6** | `0.330` | `0.364` *(unsteered)*<br>`0.641` *(steered)* | `+0.034`<br>**`+0.311`** | Scattered files of **untested guesses**; read memory `< 50%` of turns unless explicitly prompted to read memory first and test theories. |
| **Claude Sonnet 5** | `0.306` | `0.610` | `+0.304` | Autonomously read memory **`9/10` times**; marked grader-confirmed answers and wrote explicit error corrections. |
| **Claude Opus 4.7** | `0.383` | `0.700` | `+0.317` | Consolidated notes into a single reference document separating **confirmed facts** from **unconfirmed hypotheses**. |
| **Claude Opus 5** | `0.564` | `0.828` | `+0.264` | **Closed the loop:** entries combined **(1) validated rule**, **(2) verification evidence**, and **(3) explicit trap warnings**. |
| **Claude Sonnet 5.5** | `0.644` | `0.833` | `+0.189` | Closed the learning loop at lower inference cost, matching Opus 5's ceiling. |
| **Claude Opus 5.5** | `0.655` | `0.838` | `+0.183` | Highest absolute reward; concise validated rules. |

- **Critical Steering Finding (Purpose $>$ Rigid Prescriptive Rules):** Telling frontier models *what memory is for* outperformed rigid content constraints. Enforcing a prescriptive rule to **`"record only failures"`** reduced memory gain by **`−0.234` on Opus 5**, **`−0.150` on Opus 4.7**, and `−0.020` on Sonnet 4.6. Conversely, weaker/unfocused contexts benefited strongly from an explicit **retrieval + verification directive**.

#### B. *Pokémon Red* Long-Horizon Salience & Ground-Truth Dreaming (`10,000` Steps)
1. **Salience vs. Hoarding Collapse:**
   - **Sonnet 3.7:** Treated everything as salient, creating **`413` files across `225` folders (`977,930` chars)**—logging every battle, item, NPC, wallet balance, and map coordinate already visible on screen. Because game state mutated every turn, its memory went continuously stale; at step `10,000`, it created a *fourth* `"master"` folder, spending `17` consecutive steps creating `6` files and deleting `9` with **zero game actions** (`1` badge earned).
   - **Fable 5 & Opus 5.5:** Recorded **only non-obvious, un-displayed knowledge** (`10` files, `43,406` chars—**`22.5×` smaller**): `global.md` (current goal, gym plan, memory self-rules), `locations/` (route navigation plans), and `gameplay/` (empirically tested mechanics, e.g., varying input timing after 3 failed Poké Balls). Earned **`4` badges** (`Fable 5`) and **completed the game** (`Opus 5.5`).
2. **The Mt. Moon False-Memory Trap & Transcript Correction:**
   - An in-session note erroneously recorded the Mt. Moon PokéCenter on the *east* exit (`"Mt Moon DONE"`). Trusting that false note caused the raw-memory agent to jump a one-way ledge back to the west entrance **three times**, conclude `"I'm going to stop rather than keep looping"`, retreat to Pewter City, and never reach Cerulean City across `3,000` actions.
   - Running **offline dreaming against the raw session transcript** exposed the coordinate discrepancy, rewrote the note, and enabled the agent to re-enter Mt. Moon and reach Cerulean City at action **`1,935`**.

#### C. *Stainless* Enterprise Corpus (`350,000` Files, `~420M` Tokens) & *Harvey LAB* Legal Evaluations
1. **Wiki + Search Complementarity (`2` $\rightarrow$ `128` Dreaming Sessions / `2M` $\rightarrow$ `128M` Dreaming Tokens):**
   - **Wiki only:** `13%` $\rightarrow$ `48%` of graded criteria (`103` pages / `766` links $\rightarrow$ `222` pages / `12,893` links).
   - **Search tool only (no wiki):** Flat at **`53%`**.
   - **Wiki + Search tool together:** **`54%` $\rightarrow$ `61%`** (`+8 pp` over search alone, `+13 pp` over wiki alone) while **saving `~20,000` tokens per question**.
2. **Task Topology Determines Wiki ROI:**
   - **Multi-document synthesis + arithmetic/judgment (`24` merger questions):** Wiki scored **`87%` vs. `73%` search-only (`+14 pp`)**.
   - **Single-document text lookup (`57`-criterion LAB brief):** **`0%` net gain** over direct search.
   - **Borges's Map Trap:** On one `10`-document (`115 KB`) corpus, unconstrained wiki building generated **`68` pages, `255` links, and `203 KB` (`1.77×` raw corpus size)** including unlinked orphan pages—abstracting nothing.
3. **Goal-Directed Schema & Sharded Single-Writer Pages (`72%` $\rightarrow$ `100%`):**
   - Four parallel agents editing one shared `provisions.md` summary table overwrote each other's lines and over-summarized legal clauses, retaining only **`41/57` (`72%`)** of graded contract facts.
   - Reflecting on that failure, dreaming updated its wiki-building instructions to assign **one page per contract written by a single agent quoting primary terms verbatim**, raising brief accuracy to **`57/57` (`100%`)**.
4. **Second-Pass Cross-Session Link Repair:**
   - Single-session note writers only see one session's slice, leaving dangling references (`9` links across `6` pages pointing to non-existent `escrow-agreement`). A second dreaming pass repointed all `9` broken links to `buyer-form-escrow-agreement` and synthesized `index`, `instructions-email`, and `escalations` pages (`38` $\rightarrow$ `41` pages, `225` $\rightarrow$ `278` links).

---

## 3. Architectural Comparison: Anthropic Framework vs. `agy-hq` (`HEAD`)

| Memory Dimension | Anthropic (*Components of Agent Memory*, Nov 2026) | `agy-hq` v0.3.x (`RFC-0002`..`0019`) | Alignment & Gap Assessment |
| :--- | :--- | :--- | :--- |
| **Layer 4: Raw Ground Truth** | Immutable session transcripts (`transcript.jsonl`) | `~/.gemini/antigravity/brain/<convId>/.system_generated/logs/transcript.jsonl` | **Aligned.** `PreToolUse` additionally blocks direct LLM tampering with `.system_generated`. |
| **Layer 3: Episodic Capture** | In-session LLM note writes *or* post-hoc log extraction (notes trade-off: in-moment logging distracts task focus; dreaming delays availability) | **0-token Go `Stop` hook** (`journal/YYYY-MM-DD/<conv>-<exec>.json`) + `interventions.jsonl` + `reported.jsonl` + `progress/<slug>.md` + `facts.jsonl` | **`agy-hq` Leads on Capture Cost (`0` tokens, `0` task distraction); Lags on Fact Selection & TTLs** (chronological LIFO `last 25/8`, zero `expires_at`). |
| **Layer 2: Structured Representations** | Self-maintained cross-linked Markdown wikis + sharded per-item pages | `topics/<slug>.md` (Wiki), `entities/<slug>.md` (Dossiers), `skills/<slug>/SKILL.md` + `rsi/rules/<slug>.md` | **Partial Gap.** `topics/` has only `1` cross-link across `11` pages; no repo-scoped operational playbooks; unlocked concurrent file writes in `internal/vault`. |
| **Layer 1: Injected Header** | Small index (`<= 200` lines / `25 KB` in Claude Code) leaving links to deeper layers | `SessionStart` Zone 1 (`USER.md` + `MEMORY.md <= 80L` + Topic Index + `general.md`) + Zone 2 (Facts, Reported, Tasks, Worktrees, Triggers) | **Critical Bloat Regression (`30.9 KB` / `~7,725` tok).** Line-only cap gamed via `350 B` lines; duplicate Topic Map; unscoped verbose facts. |
| **Offline Dreaming & Transcript Verification** | Sleep-time replay checking claims against ground-truth transcripts, fixing broken links, merging duplicates | Nightly `/sleep-consolidate` (`23:30`) + 0-token `hq memory verify-transcript <convId>` | **Strong Foundation; Needs Salience Pre-Pass & Graph Hygiene.** Lacks 0-token quiet-night skip, broken-link/duplicate checker, and Borges ratio guard. |
| **Hybrid Retrieval (`Wiki + Search`)** | Wiki browsing **+** full-text search (`61%` vs `48%` wiki-only) | `read_file` on `topics/*.md` + `hq memory lookup` (**entities only**) | **Major Gap.** Domain Wiki recall is only `42.1%`; no `hq memory search` across `topics/`, `facts.jsonl`, `notes/`, and `journal/`. |
| **Procedural Learning Loop** | Validated rule + verification provenance + trap warning; avoid rigid `"record only failures"` rules | 4-stage `hq rsi` (`mine` $\rightarrow$ `hypothesize` $\rightarrow$ `verify` $\rightarrow$ `propose`) | **Broken Funnel (`1,754` obs $\rightarrow$ `0` active rules).** Over-noisy raw error mining + rigid mechanical templates bypassed by LLM prose edits. |
| **Untrusted Input & Security Boundary** | Not addressed (assumes trusted benchmark corpora) | Monotonic provenance taint ([RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md)), withheld Vault memory under taint, `hq memory forget --block-pii` | **`agy-hq` Decisively Ahead.** Prevents prompt-injected emails/chats from reading or poisoning durable memory. |

---

## 4. Empirical 12-Day Production Vault Audit (`2026-11-07` – `2026-11-18`)

All metrics below were collected strictly read-only across the live `local/` Vault (`90` private Vault git commits, `226` unique conversations, `1,337` turn journal files, `31,526` turn steps) with zero PII or private content exported.

### 4.1 `SessionStart` Token Footprint & Redundancy Breakdown (`full_task` Profile)

| Injected `SessionStart` Section | Zone | Lines | Bytes | Est. Tokens (`~B/4`) | Share of Total | Structural Pathology Observed |
| :--- | :---: | ---: | ---: | ---: | :---: | :--- |
| `## HQ Vault: USER.md` | Zone 1 | `15` | `4,903` | `~1,225` | `15.9%` | `349 B/line` density; ~45% duplicates `.agents/plugins/agy-hq/rules/AGENTS.md` (Front Desk delegation, Layer-1 quota, model routing, outbound preview). |
| `## HQ Vault: MEMORY.md` *(Header + Priorities + Durable Prefs)* | Zone 1 | `25` | `4,662` | `~1,165` | `15.1%` | `39` total lines passes `<= 80L` check, yet packs `250 B/line` semicolon-chained paragraphs; repeats `USER.md` & `facts.jsonl` rules. |
| `MEMORY.md` $\rightarrow$ `## Knowledge Map (Topic Wiki)` | Zone 1 | `12` | `2,186` | `~546` | `7.1%` | **100% redundant duplicate** of the auto-discovered `## HQ Knowledge Wiki` section injected immediately below it. |
| `## HQ Knowledge Wiki (Topic Index)` | Zone 1 | `13` | `3,473` | `~868` | `11.2%` | Auto-generated by `vault.LoadTopicIndex`; buried at byte `11,800`, with no active-scope highlighting. |
| `## Active Vault Facts` *(Last `25` LIFO)* | Zone 2 | `26` | `8,834` | `~2,208` | **`28.6%`** | Single largest section (`353 B/fact` avg); mixes global invariants, repo trivia, and un-expired personal calendar dates. |
| `## Recently Reported to User` | Zone 2 | `10` | `2,016` | `~504` | `6.5%` | Bounded `8`-item anti-repetition digest ([RFC-0018](../design/0018-always-on-agent-runtime-hardening.md)). |
| `## Tracked Task Conversations` + `Worktrees` + `Triggers` + `Policy` + `Notice` | Zone 2 | `37` | `4,796` | `~1,199` | `15.5%` | Dynamic operational tail for interactive coordination. |
| **Total `SessionStart` (`full_task`)** | **Z1 + Z2** | **`153`** | **`30,901`** | **`~7,725`** | **`100.0%`** | **`5.15×` over RFC-0006's `< 1,500`-token design target.** |

### 4.2 Bi-Temporal `facts.jsonl` Churn, Domain Skew & LIFO Starvation

Across 12 days, `local/memory/facts.jsonl` recorded **`72` canonical entries**: **`37` active** (`100%` marked `verified: true`, `91.9%` carrying `source_conv` provenance, `86.5%` using semantic slug IDs) and **`35` expired** (`31` superseded via `superseded_by`, confirming healthy bi-temporal turnover on fast-moving domains like `investing` [`11` expired] and `personal_family` [`9` expired]).

However, evaluating `vault.LoadActiveFacts(vaultDir, limit)` (`active[len(active)-limit:]`) exposes severe **domain eviction starvation**:

| Topic Category *(Anonymized)* | Total Active (`N = 37`) | Retained in `full` (`limit = 25`) | Evicted in `full` (`12` Evicted) | Retained in `lean_auto` (`limit = 8`) | Evicted in `lean_auto` (`29` Evicted) | `lean_auto` Eviction Rate |
| :--- | ---: | ---: | ---: | ---: | ---: | ---: |
| `investing` | `9` | `6` | `3` | `1` | `8` | `88.9%` |
| `architecture` | `8` | `4` | `4` | **`0`** | **`8`** | **`100.0%`** |
| `personal_family` | `5` | `4` | `1` | `1` | `4` | `80.0%` |
| `ux` | `4` | `1` | `3` | **`0`** | **`4`** | **`100.0%`** |
| `workflow` | `4` | `4` | `0` | `2` | `2` | `50.0%` |
| `admin` | `2` | `2` | `0` | `1` | `1` | `50.0%` |
| `platform` | `2` | `2` | `0` | `1` | `1` | `50.0%` |
| `gchat` | `1` | **`0`** | `1` | **`0`** | **`1`** | **`100.0%`** |
| `travel` | `1` | `1` | `0` | `1` | `0` | `0.0%` |
| `career` | `1` | `1` | `0` | `1` | `0` | `0.0%` |
| **Total** | **`37`** | **`25`** | **`12` (`32.4%`)** | **`8`** | **`29` (`78.4%`)** | **`78.4%`** |

Semantic audit of the `37` active statements further explains *why* the active pool grows by `+3.1` facts/day:
- **Transient Event/Window Facts Without TTL (`10 / 37 = 27.0%`):** Concrete calendar events, deadlines, and bookings carry `valid_to: null` and no `expires_at` timestamp, occupying hot injection slots even after their event window passes unless manually pruned at night.
- **Procedural Rules Disguised as Atomic Facts (`13 / 37 = 35.1%`):** Imperative instructions (`"must"`, `"never"`, `"when forwarding..."`, `"on social networks, write..."`) sit inside `facts.jsonl` instead of target skills (`gchat`, `morning-brief`) or scoped repo playbooks (`repo:blogs`, `repo:agy-hq`).

### 4.3 Layer-2 Storage Footprint, Retrieval Recall & Orphaned Lifecycle Stores

| Vault Store | Active Count | Volume (Lines / Bytes / Est. Tokens) | Empirical Usage & Health Telemetry (`219` Transcripts on Disk) |
| :--- | ---: | :--- | :--- |
| **Topic Wiki (`memory/topics/*.md`)** | `11` pages | `227` lines / `36,877 B` (`~9,219` tok)<br>*(min `7L` / med `19L` / max `35L`)* | `0` orphans, `0` broken links, **only `1` inter-topic cross-link**. Read in `49/172` (`28.5%`) untainted sessions overall; **`8/19` (`42.1%`) recall when prompt matched topic domain**. |
| **Entity Dossiers (`memory/entities/*.md`)** | `6` files | `56` lines / `4,587 B` (`~1,146` tok)<br>*(`4` person, `2` org; `1–5` notes)* | `0` `SessionStart` token overhead; `hq memory lookup` invoked **`6` times** across `219` transcripts (under-utilized during outbound drafting & triage). |
| **Task Progress (`memory/progress/*.md`)** | `19` active<br>**`0` archived** | `<= 60L` per file | **`0` explicit `hq progress save` calls** by agents; all `19` auto-generated by `HandleStop` `>= 120k`-token guard and **never archived** when tasks compacted to `tasks_archive.jsonl`. |
| **Domain Notes (`notes/**`)** | `36` files | `417,083 B` (`~104,270` tok)<br>*(`19` investing `281 KB`, `11` general `92 KB`, `2` briefs `17 KB`)* | Read in `27/174` untainted sessions. Current Wiki-to-Notes **Borges Ratio** = `36.9 KB / 417.1 KB = 0.088` (well below `1.0×` anti-abstraction danger zone, but `notes/investing/` grows `+23 KB/day`). |
| **Turn Journals (`memory/journal/`)** | `1,337` files | `12` date dirs (`31,526` steps; `326` error turns; `104` `PreToolUse` denial turns; `64` compactions) | High-value 0-token telemetry, but uncompacted older daily directories accumulate `~3,300` small JSON files/month. |
| **Interventions & RSI (`interventions.jsonl`, `memory/rsi/`)** | `70` interventions<br>`1,754` RSI obs | `48` approved, `16` rejected, `3` expired, `3` `user_feedback`; `26` follow-up user correction turns in transcripts | **`1,754` RSI observations** (`1,335` raw `error_recovery_chain`, `318` `pretool_friction`, `77` `quota_anomaly`, `24` `user_intervention`; only `28` have `occurrenceCount > 1`) $\rightarrow$ `50` hypotheses (`26` draft, `20` verified, `3` proposed, `1` approved) $\rightarrow$ **`0` active RSI rules**. |

---

## 5. Host Audit: Google Antigravity Native Memory Surfaces (`v2.29` / CLI `1.5.11`)

Inspecting the live Antigravity installation (`~/.gemini/antigravity/`, builtin skills `antigravity-guide` and `agy-customizations`, and `agy changelog` through `1.5.11`) identifies six host-level persistence/context mechanisms and clarifies how `agy-hq` should coexist with each:

1. **Hierarchical Workspace & Plugin Rules (`AGENTS.md` / `GEMINI.md` / `.agents/rules/*.md`):**
   - Loaded walking up from CWD to repo root, deduplicated by resolved path, capped at **`24,000` bytes per file** and a **`20,000`-token aggregate `defaultRulesBudget`** (beyond which over-budget rules are demoted to on-demand file path pointers).
   - ** Prefix Cache Implication:** Because Antigravity places `<user_rules>` **before** Turn 1 `<USER_REQUEST>` (which contains per-second `<ADDITIONAL_METADATA>` timestamps) and before `<SYSTEM_MESSAGE>` (`SessionStart`), tracked `AGENTS.md` rules and `<skills>` frontmatter form the **only cross-conversation shared KV-cache prefix** across subagents and parallel tasks. Meanwhile, `SessionStart` (`Zone 1 + Zone 2`) is cached across turns `2..N` *within* each conversation (`92.1%–97.9%` CHR per [Research 08](08_token_observability_and_telemetry.md)).
2. **Progressive-Disclosure Skills (`SKILL.md` + `.agents/skills.json`):**
   - Only YAML frontmatter (`name`, `description`) enters idle prompt context; full procedural markdown loads only when activated. [RFC-0019](../design/0019-two-tier-rsi-vault-overlay.md) already wires `<vault>/skills/` via `.agents/skills.json`.
3. **Interactive `/learn` Command:**
   - User-invoked session reflection that proposes rule/skill additions after explicit user approval, defaulting to `~/.gemini/GEMINI.md`, `~/.gemini/config/`, or tracked workspace `.agents/`—none of which are inside the git-versioned, PII-guarded private Vault (`local/`).
4. **Intra-Session Compaction (`/compact` & `CHECKPOINT` at `~256k` Tokens):**
   - Replaces older turns inside one trajectory (`80%–89%` context reduction), orthogonal to cross-session memory except at the compaction handoff boundary (`120k` `hq progress` flush).
5. **Host Conversation Summary & FTS5 Database (`~/.gemini/antigravity/conversation_summaries.db`):**
   - Since our initial [Research 04](04_antigravity_audit.md) audit, Antigravity maintains a `5.9 MB` SQLite database (`236` rows in `conversation_summaries` tracking `parent_conversation_id` and `nesting_depth`, plus `252` chunk rows in **`conversations_fts`**, an `fts5` virtual table indexing `title`, `workspace`, `project`, `user_body`, and `agent_body` with step ranges and byte offsets into `transcript.jsonl`). Meanwhile, `~/.gemini/antigravity/knowledge/` still contains only an empty `knowledge.lock`.
   - **Architectural Decision for `agy-hq`:**
     1. Importing a CGo or third-party Go SQLite library would violate `agy-hq`'s zero-dependency Ring-0 TCB (`H4-A`), and coupling directly to an undocumented internal SQLite schema risks silent breakage across Antigravity App updates.
     2. More importantly, `conversations_fts` indexes **raw unverified transcripts (including tainted quarantined sessions and unredacted pre-`forget` text!)**.
     3. Therefore, `agy-hq` should implement **`hq memory search` in 100% pure Go stdlib** over Vault-canonical Layer 2 & Layer 3 files (`topics/`, `playbooks/`, `entities/`, `facts.jsonl`, `notes/`, and compacted untainted `journal/`), guaranteeing `0` external dependencies, `<15ms` latency, strict `taint.IsTainted` enforcement, and 100% compliance with `hq memory forget` redactions—while documenting host transcript deletion in `hq memory forget` output when a user scrubs sensitive text that originally entered via chat.

---

## 6. Design Synthesis: Five Architectural Pillars for [RFC-0024](../design/0024-memory-v2-prediction-and-eval.md)

1. **Replace LIFO Fact Slicing with Prediction-Utility Scoring, Scoped Filtering & Bi-Temporal TTLs:** Add `kind`, `scopes`, `salience`, `expires_at`, and `promoted_to` to `Fact` (keeping volatile hit counters in git-ignored `fact_telemetry.json`), enforce `<= 240`-char atomic statements, auto-expire `expires_at <= now` in 0 tokens, and rank facts per session by Scope Affinity $\times$ (Prior Salience + Kind-Stratified Half-Life Recency + Empirical Hit Precision + Provenance).
2. **Enforce Dual Line + Byte Budgets, Eliminate Duplicate Sections & Automate Lifecycle Archival:** Cut `SessionStart` (`full`) from `30.9 KB (~7,725 tok)` to `<= 12.0 KB (<= 3,000 tok)` and `lean_auto` to `<= 4.8 KB (<= 1,200 tok)`; drop the duplicate `## Knowledge Map` section from `MEMORY.md`; enforce `store.RankMemory` (`Rank = 78`) `0600` atomic locking; auto-archive `progress/<slug>.md` in `CompactTasks`; compact `> 14d` journals into `journal/archive/YYYY-MM.jsonl`; and enforce Topic Wiki merge/Borges/staleness thresholds.
3. **Introduce Scope/Repo-Bound Operational Playbooks (`<vault>/memory/playbooks/<scope>.md`):** Separate universal cross-domain persona (`USER.md`, `MEMORY.md`, `rsi/rules/general.md` in Zone 1) from repo- and domain-specific operational rules & trap warnings (`playbooks/repo-<slug>.md`, `playbooks/domain-<slug>.md` injected in Zone 2 *only* when scope matches, `0` bytes on Front Desk and `tainted` sessions), plus a top-of-Zone-2 **Matched Context Hint** to lift domain Wiki recall from `42.1%` to `>= 80%`.
4. **Ship Pure-Go Hybrid Memory Search (`hq memory search`) & Salience-Gated Dreaming (`hq memory dream-plan`):** Combine structured Wiki/Playbook representations with `<15ms` pure-Go BM25 lexical search across Layer 2 + Layer 3, filter `hq rsi mine` noise (`occurrenceCount >= 2` for raw tool errors), and add second-pass wiki link/duplicate verification to `/sleep-consolidate`.
5. **Close the Observability Loop with `hq memory eval`:** Measure all 6 North-Star metrics (`SessionStart` budget/redundancy, `WikiRecall@Domain`, `FactUtilityRate` & `ScopedStarvationRate`, `RepeatFrictionRate`, `CorrectionRate`, and `WikiHealth / BorgesRatio`) over live Vault telemetry and deterministic CI benchmark fixtures.
