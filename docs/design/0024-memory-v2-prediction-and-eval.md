# RFC-0024: Memory Engine v2 — Prediction-Utility Fact Scoring, Dual-Cap Anti-Hoarding Budgets, Scoped Operational Playbooks, Pure-Go Hybrid Search & Quantitative Memory Eval

- **Status:** Proposed
- **Date:** 2026-11-18
- **Author:** `[Task:agy-hq] Memory Architecture Deep Dive & RFC` (`gemini-4-carbon-high`)
- **Companion Research:** [19 — Anthropic's *"The Components of Agent Memory"* & 12-Day Production Vault Telemetry Audit](../research/19_anthropic_memory_components_and_vault_audit.md)
- **Extends / Updates:** [RFC-0002 (Zero-Token Journaling & Consolidation)](0002-memory-and-consolidation.md), [RFC-0006 (4-Layer Memory Hierarchy & Dreaming)](0006-memory-engine-upgrade.md), [RFC-0010 (Bounded Agent RSI)](0010-agent-recursive-self-improvement.md), [RFC-0012 (Token Observability)](0012-token-observability.md), [RFC-0014 (Proactive Engine v2 & Living Context)](0014-proactive-engine-and-living-context.md), [RFC-0017b (Provenance Taint & Offer Gateway)](0017b-provenance-taint-and-offer-gateway.md), [RFC-0018 (Always-On Runtime Hardening)](0018-always-on-agent-runtime-hardening.md), [RFC-0019 (Two-Tier RSI & Vault Skill Overlay)](0019-two-tier-rsi-vault-overlay.md)

---

## 1. Motivation & Quantitative Problem Statement

Anthropic's engineering research, [*"The components of agent memory"*](https://www.anthropic.com/research/components-of-agent-memory) (Nov 6, 2026), establishes five empirical laws for cross-session agent memory:
1. **Memory is a prediction about future utility**, closing the learning loop only when entries capture *(validated rule + verification provenance + failure trap warnings)* and surface when relevant.
2. **Top-layer injected context must remain compact**, because unpruned instruction/memory headers degrade adherence and stall execution (*Pokémon Red* `413` files / `1` badge vs. `10` files / `4` badges).
3. **Structured wikis and search are complementary** (*Stainless* `420M`-token benchmark: `48%` wiki-only vs. `53%` search-only vs. **`61%` wiki + search**, saving `~20,000` tokens/question), provided wikis avoid **Borges's Map Trap** (`1.77×` raw corpus bloat) and **multi-writer overwrite races** (`72%` $\rightarrow$ `100%` survival via sharded single-writer pages).
4. **Purpose-driven dreaming with ground-truth transcript checks outperforms rigid extraction checklists** (rigid `"record only failures"` rules cut Opus 5 memory gain by `−0.234`, while second-pass dreaming repairs broken cross-links and Mt. Moon false-completion traps).

Auditing 12 days of real-world `agy-hq` Vault usage (`226` conversations, `1,337` journaled turns, `31,526` steps — [Research 19 §4](../research/19_anthropic_memory_components_and_vault_audit.md)) revealed eight concrete regressions against [RFC-0006](0006-memory-engine-upgrade.md):

| Problem | Measured 12-Day Production Baseline (`HEAD`) | RFC-0024 Target |
| :--- | :--- | :--- |
| **1. `SessionStart` Bloat & Line-Cap Gaming** | **`30,901 B` (`~7,725` tok)** (`full`); `MEMORY.md` passes `39/80L` check via `250 B` semicolon lines (`6,844 B`); Topic Wiki index injected **twice** (`2,186 B` + `3,473 B`). | **`<= 12,000 B` (`<= 3,000` tok)** (`full`), **`<= 4,800 B` (`<= 1,200` tok)** (`lean_auto`); `0` duplicate sections. |
| **2. LIFO Fact Eviction Starvation** | `LoadActiveFacts` (`active[len-limit:]`) evicts `32.4%` in `full` and **`78.4%` (`29/37`) in `lean_auto` — starving `100%` (`8/8`) of `architecture` & `100%` (`4/4`) of `ux` facts**. | **`100%` scoped invariant recall** via Scope Affinity $\times$ Prediction-Utility scoring; `0` unrelated personal facts in SWE subagents. |
| **3. Missing Fact TTLs & Misplaced Rules** | `27.0%` (`10/37`) of active facts are date-bound events with `0` TTL; `35.1%` (`13/37`) are procedural rules duplicating `AGENTS.md`/`SKILL.md`. | Deterministic `expires_at` 0-token auto-expiry + automatic promotion of procedural clusters to Playbooks/Skills. |
| **4. Low Topic Wiki Recall & Island Graph** | **`42.1%` (`8/19`)** `read_file` recall on domain-matched prompts; only `1` cross-link across `11` topic pages; no hybrid lexical search CLI. | **`>= 80%` domain Wiki recall** via Matched Context Hints + `<15ms` pure-Go BM25 `hq memory search`. |
| **5. Global Rule Pollution vs. Repo Scope** | Repo-specific build/posting rules leak into global `MEMORY.md` / `facts.jsonl`; `hq rsi` (`1,754` noisy obs $\rightarrow$ `0` active rules) bypassed by nightly dreaming. | Scope-bound `<vault>/memory/playbooks/<scope>.md` injected in Zone 2 *only* on matching repo/domain (`0 B` on Front Desk & tainted). |
| **6. Unlocked Writes & Orphaned Lifecycles** | `internal/vault` bypasses `internal/store` `flock` (`facts.jsonl` written `0644` non-atomically); `19` active `progress/*.md` (`0` archived); `1,337` raw journals. | `store.RankMemory` (`Rank = 78`) `0600` atomic writes; auto-archive `progress/` on `CompactTasks`; `14d` rolling journal/brief archival. |
| **7. Zero Memory Quality Observability** | `hq memory status` checks only static line count & dead task slugs. | Deterministic 0-token `hq memory eval` computing Metrics `M1`–`M6` + CI fixture benchmark suite. |

---

## 2. Architecture Overview

```mermaid
flowchart TB
    subgraph L4["Layer 4: Immutable Host Ground Truth (Antigravity Brain)"]
        TR["transcript.jsonl &amp; transcript_full.jsonl\n(Read-only via hq surrogate; PreToolUse blocks LLM tampering)"]
    end

    subgraph L3["Layer 3: Locked Episodic &amp; Bi-Temporal Stores (store.RankMemory = 78, 0600)"]
        JL["journal/YYYY-MM-DD/*.json (14d hot)\n-&gt; journal/archive/YYYY-MM.jsonl (cold)"]
        FC["facts.jsonl (Hot &lt;= 50; &lt;= 240 chars)\n• kind | scopes | salience | expires_at\n-&gt; facts_cold.jsonl"]
        FT["fact_telemetry.json (Git-ignored)\n• injected_count | hit_count | last_hit_at"]
        PR["progress/&lt;slug&gt;.md (Active tasks only)\n-&gt; progress/archived/ on CompactTasks"]
    end

    subgraph L2["Layer 2: Structured On-Demand Representations + Pure-Go BM25 Search"]
        TP["topics/&lt;slug&gt;.md (&lt;= 25 active, &lt;= 6 KB)\n• Cross-linked domain synthesis"]
        PB["playbooks/&lt;scope&gt;.md (&lt;= 60L / 2.4 KB)\n• repo:&lt;slug&gt; &amp; domain:&lt;slug&gt;\n• Rule + Verified Evidence + Trap Warning"]
        EN["entities/&lt;slug&gt;.md (&lt;= 100 active, &lt;= 10 notes)\n• Counterparty &amp; channel dossiers"]
        SRCH["hq memory search &lt;query&gt;\n• Pure-Go stdlib BM25 (&lt;15ms, 0 CGo/SQLite)\n• Spans L2 + L3; denied under taint"]
    end

    subgraph L1["Layer 1: Dual-Cap SessionStart Assembly (&lt;= 12 KB full | &lt;= 4.8 KB lean_auto)"]
        Z1["Zone 1: Byte-Stable Global Prefix\n• USER.md (&lt;= 2.4 KB)\n• MEMORY.md (&lt;= 60L &amp; &lt;= 3.6 KB, 0 dup Topic Map)\n• Canonical Topic Wiki Index + general.md (&lt;= 5)"]
        Z2["Zone 2: Role &amp; Scope-Matched Tail\n• Matched Context Hint (Top 1-2 Topic/Entity links)\n• Matching playbooks/&lt;scope&gt;.md (0 on FD/tainted)\n• Utility-Scored Scoped Facts (&lt;= 15 full / &lt;= 6 lean)"]
    end

    subgraph Loop["Offline Salience-Gated Dreaming &amp; Closed-Loop Eval"]
        DP["hq memory dream-plan --since=24h\n(0-token delta gatekeeper + VerifyTranscript batch)"]
        EV["hq memory eval --since=14d\n(M1 Budget · M2 WikiRecall · M3 FactUtility ·\n M4 RepeatFriction · M5 Corrections · M6 BorgesRatio)"]
    end

    TR --&gt;|"0-token HandleStop"| JL &amp; FT
    JL &amp; TR --&gt; DP &amp; EV
    DP --&gt;|"Non-empty delta"| TP &amp; PB &amp; EN &amp; FC
    EV --&gt;|"Promote / Demote / Merge signals"| DP
    Z1 &amp; Z2 --&gt;|"&lt;10ms read-only"| SESS["Active Agent Session"]
    SESS -.-&gt;|"read_file / hq memory search"| TP &amp; SRCH
```

---

## 3. Pillar 1: Bi-Temporal `Fact` v2, Auto-Expiry TTLs & Prediction-Utility Scoring

### 3.1 Backward-Compatible `Fact` v2 Schema & Telemetry Split (`internal/vault/memory.go`)

Existing `facts.jsonl` lines deserialize unchanged. Optional fields capture prediction kind, target scope, prior salience, deterministic expiry, and promotion lineage:

```go
type FactKind string

const (
	FactKindInvariant  FactKind = "invariant"  // Durable rule/constraint (half-life = 90d, default salience = 0.85)
	FactKindPrediction FactKind = "prediction" // Verified domain heuristic/playbook pointer (half-life = 45d, default = 0.70)
	FactKindState      FactKind = "state"      // Evolving project/deal/system state (half-life = 21d, default = 0.55)
	FactKindEvent      FactKind = "event"      // Time-bound milestone/booking/deadline (half-life = 7d, default = 0.45)
)

type Fact struct {
	ID           string   `json:"id"`
	Topic        string   `json:"topic"`
	Statement    string   `json:"statement"`              // Enforced <= 240 runes on AddFact
	SourceConv   string   `json:"source_conv,omitempty"`
	Verified     bool     `json:"verified,omitempty"`
	ValidFrom    string   `json:"valid_from"`
	ValidTo      *string  `json:"valid_to"`
	SupersededBy string   `json:"superseded_by,omitempty"`

	// RFC-0024 Additions (backward-compatible omitempty):
	Kind         FactKind `json:"kind,omitempty"`         // Inferred if empty during LoadFactsState
	Scopes       []string `json:"scopes,omitempty"`       // e.g. ["global"], ["repo:agy-hq"], ["domain:investing"], ["skill:gchat"]
	Salience     float64  `json:"salience,omitempty"`     // Explicit override in [0.0, 1.0]; 0 uses Kind default
	ExpiresAt    string   `json:"expires_at,omitempty"`   // Optional RFC3339 auto-expiration timestamp
	Pinned       bool     `json:"pinned,omitempty"`       // Exempt from cold demotion within matching scope
	PromotedTo   string   `json:"promoted_to,omitempty"`  // Target topic/playbook/skill path when graduated
}
```

**Why Telemetry Lives Outside `facts.jsonl` (`local/memory/fact_telemetry.json`):**
- Recording `InjectedCount` inside `facts.jsonl` during `SessionStart` would turn a `<2ms` read-only hook into a mutating disk write and dirty `local/.git` on every conversation turn.
- Instead, git-ignored `local/memory/fact_telemetry.json` (`0600`, guarded by `store.RankMemory`) stores per-`Fact.ID` counters (`InjectedCount`, `HitCount`, `LastInjectedAt`, `LastHitAt`), updated asynchronously in **0 LLM tokens** during `HandleStop` and `hq memory eval` by scanning turn transcripts.

### 3.2 Zero-Token `ExpiresAt` Auto-Expiration & Statement Guardrails

1. **`hq memory add-fact` Enhancements:**
   ```sh
   hq memory add-fact --topic=<topic> --statement="<concise <=240c fact>" \
     [--kind=invariant|prediction|state|event] [--scope=global|repo:<name>|domain:<name>|skill:<name>] \
     [--salience=0.0..1.0] [--expires=<RFC3339|YYYY-MM-DD|+7d>] [--pinned] [--source=<convId>] [--verified]
   ```
   - Rejects `--statement` exceeding **`240` runes** (`~60` tokens) with an actionable error instructing the caller to place multi-sentence details in `topics/<slug>.md` or `playbooks/<scope>.md` and link it.
   - If `--statement` contains an explicit calendar date (`YYYY-MM-DD` or `Month DD`) and both `--expires` and `--kind` are omitted, `AddFact` emits a stderr hint recommending `--expires=<date+24h>` so transient calendar facts self-clean.
2. **Automatic Expiration on Read & Compaction:**
   - `LoadFactsStateAt(vaultDir, now)` treats any entry with `ExpiresAt != "" && now.After(parsedExpiresAt)` as **expired** immediately in `0` LLM tokens (`SessionStart` stops injecting it the second the deadline passes).
   - `CompactFacts` materializes `ValidTo = &ExpiresAt` on newly elapsed TTL facts.

### 3.3 Deterministic Prediction-Utility Scoring (`SelectFactsForSession`)

Replacing `active[len(active)-limit:]`, `SelectFactsForSession(active, telemetry, ctx, opts)` computes a deterministic utility score $U(f, ctx, t)$ in `<0.5ms` (`O(N)` pure Go arithmetic, `0` embeddings or network calls):

$$U(f, ctx, t) = w_{\text{scope}}(f, ctx) \cdot \Big( 0.35 \cdot S(f, t) + 0.25 \cdot R(f, t) + 0.25 \cdot H(f) + 0.15 \cdot V(f) \Big)$$

1. **Scope Affinity Multiplier $w_{\text{scope}}(f, ctx)$:**
   - `SessionContext` derives `RepoSlug` (from `WorkspacePaths` / enclosing worktree), `TaskScope` (`matchedTask.Scope`), `ActiveSkill`, `Role` (`front_desk` | `task` | `subagent` | `auto`), and normalized `PromptTokens` (`payload.LastUserInput`).
   - **Exact Scope or Lexical Prompt Match:** $w_{\text{scope}} = 1.35$ (e.g., `scopes` contains `repo:agy-hq` or `topic == "architecture"` inside an `agy-hq` worktree, or `PromptTokens` shares $\ge 2$ non-stopword stems with `Statement`).
   - **Global Cross-Cutting Scope (`global`, `ux`, `workflow`):** $w_{\text{scope}} = 1.00$.
   - **Unmatched Domain in `[HQ] Front Desk` (`Role == "front_desk"`):** $w_{\text{scope}} = 0.75$ (kept eligible because Front Desk handles arbitrary cross-life questions, subject to a **per-topic diversity cap of `<= 4` facts/topic** so one noisy topic cannot monopolize the slice).
   - **Unmatched Personal/Domain Fact in `lean_auto` / Scoped Repo Session:** $w_{\text{scope}} = 0.0$ (strictly filtered out unless lexically matched in `PromptTokens` — eliminating irrelevant personal/travel/calendar injection into SWE subagents and `[Auto]` workers).
2. **Prior Salience & Upcoming-Window Boost $S(f, t) \in [0, 1]$:**
   - Uses explicit `f.Salience` if $> 0$, else `Kind` default (`0.85` `invariant`, `0.70` `prediction`, `0.55` `state`, `0.45` `event`).
   - **Imminent Event Boost:** If `f.ExpiresAt` is within the next `72h` (`0 < ExpiresAt - t <= 72h`), $S(f, t) = \max(S(f, t), 0.92)$ so upcoming deadlines float to the top right when actionable and vanish at `ExpiresAt`.
3. **Kind-Stratified Half-Life Recency $R(f, t) = 2^{-\Delta t / \tau_{\text{kind}}}$:**
   - $\Delta t = \min(t - t_{\text{valid\_from}}, t - t_{\text{last\_hit}})$ (hippocampal replay stabilization: facts that prove useful in transcripts refresh their recency clock).
   - Half-lives $\tau_{\text{kind}}$: `90d` (`invariant`), `45d` (`prediction`), `21d` (`state`), `7d` (`event`). `Pinned` facts clamp $R(f, t) = 1.0$.
4. **Laplace-Smoothed Empirical Utility $H(f) \in [0, 1]$:**
   - $H(f) = \min\left(1.0,\; 2.0 \cdot \frac{\text{HitCount} + 1}{\text{InjectedCount} + 4}\right)$, giving unseen facts a neutral `0.50` prior while decaying facts injected `>= 20` times with `0` hits toward `0.08`.
5. **Verification Bonus $V(f)$:** `1.0` if `f.Verified && f.SourceConv != ""`, else `0.30`.

Selection greedily packs highest-$U$ facts until hitting **either** `MaxFacts` (`15` in `full`, `6` in `lean_auto`) **or** `MaxFactBytes` (`3,000 B` in `full`, `1,200 B` in `lean_auto`), then renders selected facts in deterministic `ID`-sorted order within `Zone 2`.

### 3.4 Promotion, Consolidation & Cold Demotion Ladder

During `hq memory compact` and nightly `/sleep-consolidate`:
- **Cluster Promotion (`>= 3` Active Facts on Same Topic/Repo or Procedural `Kind == "invariant"`):** Flagged in `hq memory dream-plan` for distillation into `topics/<slug>.md`, `playbooks/<scope>.md`, or `<vault>/skills/<slug>/SKILL.md`, soft-expiring the raw facts with `promoted_to: "<target>"`.
- **Cold Demotion (`facts_cold.jsonl`):** Unpinned facts with `InjectedCount >= 20`, `HitCount == 0`, and age `> 21d` (or overflow beyond the `50`-fact hot pool cap) move cleanly to `local/memory/facts_cold.jsonl` (`0` `SessionStart` cost, fully searchable via `hq memory search --include-archive`).

### 3.5 Multi-Writer Lock Safety (`store.RankMemory = 78`)

Inspired by Anthropic's Harvey LAB `provisions.md` multi-writer finding (`72%` $\rightarrow$ `100%` survival):
- Add **`RankMemory Rank = 78`** in `internal/store/store.go` (strictly above `RankTasks = 75` so `kernel.CompactTasks` can acquire `RankTasks` $\rightarrow$ `RankMemory` without lock-order inversion, and below `RankSubagentQueue = 80` / `RankQuota = 85`).
- Wrap `AddFact`, `PruneFact`, `CompactFacts`, `UpsertEntity`, `SaveProgress`, `ArchiveProgress`, `SavePlaybook`, and `ForgetMemory` in `store.With(ctx, store.RankMemory, lockPath, ...)` using `store.WriteAtomic(..., 0o600)` (closing the legacy `0644` non-atomic `os.WriteFile` in `writeCanonicalFacts`).

---

## 4. Pillar 2: Dual Line/Byte Anti-Hoarding Caps & Automated Lifecycle Archival

To prevent line-count gaming (`350 B` semicolon lines) and Borges's Map Trap, Memory v2 enforces **both line and byte budgets** across every memory layer:

| Store / Surface | Hard Limits (Line + Byte) | Enforcement & Automatic Compaction / Archival Rule |
| :--- | :--- | :--- |
| **`SessionStart` Total** | `full`: **`<= 12,000 B`** (`~3,000` tok)<br>`lean_auto`: **`<= 4,800 B`** (`~1,200` tok)<br>`tainted`: **`<= 800 B`** (`~200` tok) | Enforced by priority-ordered section allocator in `HandleSessionStart` (Zone 1 core $\rightarrow$ Matched Context Hint $\rightarrow$ Scoped Playbook $\rightarrow$ Scored Facts $\rightarrow$ Dynamic Tail). |
| **`local/MEMORY.md`** | **`<= 60` lines AND `<= 3,600 B`** (`~900` tok); max `180 B/line` lint | **Drop `## Knowledge Map (Topic Wiki)`** from `MEMORY.md` (`templates/MEMORY.md` + `v10` migration strip) since `LoadTopicIndex` auto-generates the canonical Topic Index. `HandleSessionStart` truncates on both `60` lines and `3,600` bytes. |
| **`local/USER.md`** | Advisory **`<= 2,400 B`** (`~600` tok) | `hq memory status` & `hq doctor` warn when `USER.md > 2,400 B` or shares high 5-gram overlap (`>= 20%`) with `.agents/plugins/agy-hq/rules/AGENTS.md`. |
| **Hot `facts.jsonl`** | Injected: `<= 15` (`3,000 B`) / `<= 6` (`1,200 B`); Hot pool: **`<= 50` active**; `<= 240` chars/fact | `AddFact` enforces `<= 240` chars; `CompactFacts` auto-expires `ExpiresAt <= now` and demotes lowest-utility overflow (`> 50`) to `facts_cold.jsonl`. |
| **`memory/topics/*.md`** | **`<= 25` active pages**; **`<= 120` lines & `<= 6,000 B` per page**; **`<= 100 KB` total** | 1. **Near-Duplicate Detector:** Pairwise non-stopword token Jaccard `>= 0.45` flagged for merge.<br>2. **Borges Ratio Guard:** Warn if `bytes(topics/) / max(1, bytes(notes/)) > 0.35`.<br>3. **Cold Archival:** Pages with `0` `read_file` hits and `0` mtime updates for `> 60d` move to `memory/topics/archived/<slug>.md` (omitted from `SessionStart` index; searchable via `--include-archive`). |
| **`memory/playbooks/*.md`** | **`<= 20` active scopes**; **`<= 60` lines & `<= 2,400 B` per playbook** | Loaded in Zone 2 **only** when session scope matches (`0 B` on Front Desk & tainted). |
| **`memory/entities/*.md`** | **`<= 100` active dossiers**; **`<= 25` lines & `<= 10` bullets per dossier** | `UpsertEntity` caps `Notes` at `10` bullets (rolling oldest notes to `entities/archived/<slug>-history.md`); inactive `> 180d` entities with `0` `WaitingOn` goals archive to `entities/archived/`. |
| **`memory/progress/*.md`** | `<= 60` lines (`<= 2,400 B`); **1:1 with active/idle tasks** | `kernel.CompactTasks` and `hq archive <slug>` automatically invoke `vault.ArchiveProgress` for every task rotated to `tasks_archive.jsonl` (fixing the `19 active / 0 archived` leak). |
| **`notes/briefs/` & `journal/`** | `14d` hot window | `hq memory compact` moves `notes/briefs/*.md` older than `14d` to `notes/briefs/archive/YYYY-MM/` and rolls `memory/journal/YYYY-MM-DD/*.json` older than `14d` into `memory/journal/archive/YYYY-MM.jsonl` (`1` line/turn, preserving 100% `hq obs` & `hq memory eval` history while cutting inode count by `99%`). |

---

## 5. Pillar 3: Scope/Repo-Bound Operational Playbooks & Cache/Taint-Safe `SessionStart`

### 5.1 Prompt Prefix Cache Mechanics in Antigravity

As established in [Research 08 §2.2](../research/08_token_observability_and_telemetry.md) and [Research 19 §5](../research/19_anthropic_memory_components_and_vault_audit.md):
1. **Cross-Conversation Prefix Cache:** Antigravity places `<identity>`, `<tools>`, `<skills>` frontmatter, and `<user_rules>` (`AGENTS.md` / `GEMINI.md`) **ahead** of Turn 1 `<USER_REQUEST>` (which embeds per-second `<ADDITIONAL_METADATA>` timestamps) and `<SYSTEM_MESSAGE>` (`SessionStart`). Keeping `.agents/plugins/agy-hq/rules/AGENTS.md` and skill frontmatter compact and static maximizes cross-session prefix cache sharing across parallel subagents.
2. **Intra-Conversation Prefix Cache (`92.1%–97.9%` CHR Across Steps `2..K`):** `SessionStart` runs once at Turn 1; every subsequent step within the conversation caches **both Zone 1 and Zone 2**.
3. **Why Zone 1 Purity Still Matters:** Keeping Zone 1 strictly global (`USER.md` + compact `MEMORY.md` + sorted Topic Index + `rsi/rules/general.md`) with zero volatile timestamps and zero repo-specific clutter guarantees (a) clean cross-run caching in headless/SDK harnesses that hoist system messages, and (b) high instruction adherence without cross-repo rule interference.

### 5.2 Scope-Bound Operational Playbooks (`<vault>/memory/playbooks/<scope>.md`)

To stop repo-specific quirks (e.g., `repo:blogs` Reddit title guidance, `repo:agy-hq` `SanitizedApprovalEnv` rules, `domain:investing` secondary SPV rubrics) from polluting global `MEMORY.md`, `USER.md`, or `facts.jsonl`, we introduce **Scoped Playbooks**:

1. **Naming & Triplet Entry Format (Anthropic Opus 5 Closed-Loop Schema):**
   - Path: `<vault>/memory/playbooks/<scope-slug>.md` (`0600`), where `<scope-slug>` is `repo-<repo-name>` (e.g. `repo-agy-hq`, `repo-blogs`, `repo-acme-api`) or `domain-<scope>` (e.g. `domain-investing`, `domain-career`).
   - Entries follow the compact **Rule + Provenance + Trap** triplet (`<= 3` lines per rule):
     ```markdown
     # Scoped Playbook: `repo:blogs`
     > **Scope:** `repo:blogs` | **Budget:** <= 60 lines / 2,400 bytes

     - **[Social Post Framing]** (`verified:640edff`): Lead casual developer posts with a concrete builder story hook rather than RFC headers; **Trap:** `r/google_antigravity` Post Guidance blocks the word `"quota"` in titles—use `"token usage"` instead.
     ```
2. **Deterministic Scope Resolution (`ResolveSessionScopes`):**
   - Inspects `payload.WorkspacePaths` / CWD git remote or directory basename (resolving `<vault>/worktrees/<repo>-<task>` $\rightarrow$ `repo:<repo>`) and `matchedTask.Scope` (`domain:<scope>`).
   - **`[HQ] Front Desk`:** Injects **`0` playbooks inline** (Front Desk stays an ultra-lean router).
   - **Tainted Sessions (`taint.IsTainted == true`):** Injects **`0` playbooks inline** and blocks `read_file` / CLI access to `<vault>/memory/playbooks/`.
   - **Untainted Scoped Task / Subagent:** Appends matching `<vault>/memory/playbooks/<scope>.md` (`<= 2,400 B`, max `2` scopes: `repo:<X>` + `domain:<Y>`) at the top of **Zone 2**.
3. **RSI Scope Routing Fix (`internal/rsi/miner.go` & `mutator.go`):**
   - When an untainted friction event occurs inside repository `<repo>` without matching an installed skill, `inferSkillOrPkg` returns **`"repo:" + repoSlug`** (targeting `<vault>/memory/playbooks/repo-<repoSlug>.md`) instead of dumping repo-specific errors into global `general.md` or `swe-pulse`.
   - `<vault>/memory/playbooks` is protected from unverified direct LLM `write_file`/`edit_file` overwrite when managed via `hq memory playbook upsert` / `hq rsi`, keeping **`0` dirty files** in any tracked git repository.

### 5.3 Top-of-Zone-2 "Matched Context Hint" (Lifting Wiki Recall `42.1%` $\rightarrow$ `>= 80%`)

In [Research 19 §4.3](../research/19_anthropic_memory_components_and_vault_audit.md), agents skipped `read_file` on `topics/<slug>.md` `57.9%` of the time even when the user's prompt directly matched that topic's domain. In `HandleSessionStart`, when `payload.LastUserInput` or `matchedTask` matches `1–2` `TopicPage`s or `EntityDossier`s by keyword/slug overlap:
- Zone 2 prepends a high-salience **2-line callout** directly above `Active Vault Facts`:
  ```markdown
  ## 📌 Matched Vault Context (Read Before Investigating)
  - Topic Wiki match: **[Google Chat Chrome CDP Integration](file://<vault>/memory/topics/gchat-cdp-integration.md)** — call `read_file` on this page first before probing from scratch.
  ```
- Cost: `~45` tokens when matched (`0` tokens when unmatched), directly replicating the targeted retrieval steering that nearly doubled Sonnet 4.6's CL-Bench score (`0.364` $\rightarrow$ `0.641`).

---

## 6. Pillar 4: Pure-Go Hybrid Memory Search (`hq memory search`) & Salience-Gated Dreaming

### 6.1 Pure-Go BM25 Lexical Search (`hq memory search`)

Closing Anthropic's *Wiki + Search* gap (`48%` $\rightarrow$ `61%`) without breaking `H4-A` (`0` external Go dependencies; no CGo SQLite):

```sh
hq memory search "<query>" [--scope=<scope>] [--layers=topics,playbooks,entities,facts,notes,journal] \
  [--include-archive] [--limit=8] [--json]
```

- **Implementation (`internal/vault/search.go`):**
  - Tokenizes query and local Vault documents with Unicode case-folding, identifier splitting (`camelCase`, `kebab-case`, `snake_case`), and lightweight English suffix stemming.
  - Scores paragraph/record chunks with standard **Okapi BM25** ($k_1 = 1.2, b = 0.75$) multiplied by layer authority weights (`topics`/`playbooks` `1.25×`, `entities` `1.15×`, active `facts` `1.10×`, `notes` `1.00×`, untainted `journal` `0.80×`, `archived` `0.65×`).
  - Executes in **`< 15ms`** over a `1–5 MB` local Vault corpus with zero persistent daemon index to corrupt, returning ranked hits with `file://<absPath>#L<start>-L<end>` links and `<= 200`-char context snippets.
- **Security & Quarantine:** `denyIfCallerTainted(vaultDir, "memory search")` blocks tainted sessions unconditionally (tainted callers remain restricted to narrowed `hq memory lookup` routing fields per [RFC-0017b §3.2](0017b-provenance-taint-and-offer-gateway.md)), and `protectedPrivateSubs` (`local/private/**`) are never indexed or scanned.

### 6.2 Salience-Gated Dreaming Pre-Pass (`hq memory dream-plan`) & Second-Pass Wiki Hygiene

To address Anthropic's open question on dreaming compute efficiency (*"focus dreaming on information that's likely to change or carefully tune the cadence"*) and fix the `1,754`-observation RSI noise flood:

1. **Zero-Token Delta Planner (`hq memory dream-plan --since=24h [--json]`):**
   - Executed as `precheck_cmd` (or Step 0) of `sleep-consolidate`:
     - If `0` untainted journal entries, `0` new `interventions.jsonl` entries, `0` elapsed `ExpiresAt` facts, and `!OverBudget` occurred since the last consolidation cursor, exits cleanly with `DREAM_NOOP` (**`0` Carbon LLM tokens burned on idle days**).
     - Otherwise outputs a single compact JSON digest containing:
       1. Pre-computed `VerifyTranscript` digests **only** for salient conversations (`TurnErrorCount > 0`, `PreToolDenials > 0`, follow-up user corrections, or `>= 5` steps);
       2. Auto-expired `ExpiresAt` facts + cluster promotion & cold-demotion candidates;
       3. Completed `goals.json` / `tasks.json` slugs still lingering in `MEMORY.md`;
       4. **Wiki Graph Hygiene Alerts:** Broken markdown links (Anthropic Figure 15 second-pass link repair), orphan topics, near-duplicate topic pairs (`Jaccard >= 0.45`), and pages exceeding `6 KB` / Borges ratio `> 0.35`.
2. **RSI Noise Filter (`internal/rsi/miner.go`):**
   - Raw single-occurrence `FrictionErrorRecovery` steps from normal TDD/compile iterations (`occurrenceCount == 1 && wastedSteps < 4`) remain local turn telemetry and are **not** persisted to `observations.jsonl` unless the normalized command/error fingerprint recurs across **`>= 2` distinct conversations** (while `FrictionUserIntervention`, `FrictionDraftEdited`, and repeated `FrictionPreTool` remain immediate). Observations older than `90d` compact to `rsi/archive.jsonl`.

---

## 7. Pillar 5: Quantitative Memory Eval & Observability (`hq memory eval`)

We introduce a deterministic **0-LLM-token evaluation & telemetry command**:

```sh
hq memory eval [--since=14d] [--fixtures] [--json]
```

### 7.1 Six North-Star Metrics (`MemoryEvalReport`)

| Metric ID | Metric Name | Exact 0-Token Formula Over Vault & Transcripts | Healthy Target |
| :--- | :--- | :--- | :---: |
| **`M1`** | **`SessionStart` Budget & Cross-File Redundancy** | `Bytes(full)` / `12,000 B`, `Bytes(lean_auto)` / `4,800 B`, and 5-gram Jaccard overlap across `{AGENTS.md, USER.md, MEMORY.md, active facts}` | Budget `<= 100%`<br>Overlap `< 10%` |
| **`M2`** | **Domain-Matched Topic Wiki Recall (`WikiRecall@Domain`)** | $\frac{\lvert \{ c \in C_{\text{matched}} \mid c \text{ read/searched matching } \text{topic} \text{ in steps } 1..6 \} \rvert}{\lvert C_{\text{matched}} \rvert}$ across untainted sessions where `topic.CreatedAt < c.StartedAt` | **`>= 80%`**<br>*(baseline `42.1%`)* |
| **`M3`** | **Fact Utility Precision & Scoped Starvation** | Mean $\frac{\text{HitCount}}{\max(1, \text{InjectedCount})}$ for facts with `InjectedCount >= 5`, plus `%` of active scope-matched invariants evicted in `lean_auto` | Starvation **`0%`**<br>*(baseline `100%`)* |
| **`M4`** | **Post-Rule Repeat Friction Rate (`LoopClosure`)** | Fraction of non-transient error fingerprints recurring in conversations started *after* a matching `playbook`, `rsi/rules`, or `topic` trap rule was committed | **`<= 10%`** |
| **`M5`** | **User Correction & Rejection Rate (`Corrections/100t`)** | $\frac{\text{RejectedProposals} + \text{EditedDrafts} + \text{UserFeedback} + \text{TranscriptCorrectionTurns}}{\text{InteractiveUntaintedTurns}} \times 100$ (rolling 7d vs. prior 7d trend) | Monotonic downward trend |
| **`M6`** | **Wiki Structural Integrity & Borges Ratio** | `BrokenLinks`, `OrphanTopics`, `NearDuplicatePairs` (`J >= 0.45`), and `BorgesRatio = Bytes(topics/) / max(1, Bytes(notes/))` | `0` broken/dup;<br>Ratio `<= 0.35` |

### 7.2 Golden CI Regression Fixtures (`hq memory eval --fixtures`)

Similar to `hq outbox eval` ([RFC-0018](0018-always-on-agent-runtime-hardening.md)), `internal/vault/testdata/memory_eval_fixtures.json` ships deterministic synthetic Vaults and multi-session transcripts evaluated in `go test ./internal/vault/...`:
1. **Fixture F1 (LIFO vs. Scoped Prediction-Utility):** `40` active facts (`8` older `repo:agy-hq` invariants + `32` newer personal/calendar/investing facts) $\rightarrow$ asserts `SelectFactsForSession` achieves **`100%` recall** of top `repo:agy-hq` invariants in a `lean_auto` coding subagent and **`0%` leakage** of personal facts.
2. **Fixture F2 (Bi-Temporal `ExpiresAt` Window):** Event fact `expires_at = T` boosts into top-3 at `T - 24h` (`S >= 0.90`) and drops to `0%` injection at `T + 1s` without LLM pruning.
3. **Fixture F3 (Line-Count Gaming & Duplicate Section Defense):** `MEMORY.md` with `35` lines of `300`-byte semicolon text (`10.5 KB`) is deterministically capped at `<= 3,600 B`, keeping `SessionStart` `<= 12,000 B`.
4. **Fixture F4 (Wiki Graph & Borges Guard):** Detects a dangling markdown link (`escrow-agreement.md`), a `0.58`-Jaccard duplicate topic pair, and a `1.77×` Borges bloat regression.

---

## 8. Pillar 6: Coexistence Contract with Google Antigravity Native Memory

| Antigravity Native Surface | Ownership & Coexistence Rule in `agy-hq` |
| :--- | :--- |
| **Repo `AGENTS.md` / `GEMINI.md`** | **Public / Team Invariants Only** (`<= 24 KB` file / `20k`-token host budget). Forms the cross-conversation KV-cache prefix. Never write personal habits or Vault state here. |
| **Plugin `.agents/plugins/agy-hq/rules/AGENTS.md`** | **Core `agy-hq` OS Kernel Rules.** `USER.md` and `MEMORY.md` must not copy rules already enforced here (`hq memory eval` `M1` lints overlap). |
| **Progressive-Disclosure Skills (`.agents/skills.json` $\rightarrow$ `<vault>/skills/`)** | **Canonical Home for Multi-Step Workflows.** Procedural `"when doing X, run steps 1..3"` notes graduate from `facts.jsonl` into `<vault>/skills/<slug>/SKILL.md` (`0` idle token cost). |
| **Native `/learn` Slash Command** | Machine-generic coding style preferences may use `~/.gemini/config/rules/`; personal/Vault domain lessons target `<vault>/skills/` or `<vault>/memory/playbooks/<scope>.md` so they remain versioned in `local/.git` and scanned by `P1`–`P6`. |
| **Native `/compact` (`CHECKPOINT` at `~256k`)** | Owns intra-session context compression; `agy-hq`'s `120k` pre-compaction nudge + `progress/<slug>.md` preserves active task handoff state and auto-archives on task completion. |
| **Host `conversation_summaries.db` (`conversations_fts` SQLite FTS5) & `transcript.jsonl`** | Treated as **read-only host telemetry (`transcript.jsonl`) / opaque internal app index (`conversation_summaries.db`)**. `hq memory search` uses pure-Go stdlib BM25 over Vault Layer 2/3 files (`0` SQLite/CGo dep, taint-safe, respects `forget`). When `hq memory forget` scrubs a phrase with `source_conv` provenance, CLI output lists the originating `conversationId`(s) so the user knows if raw host transcripts in `~/.gemini/antigravity/brain/<convId>` should also be deleted via the Antigravity UI. |

---

## 9. Security, Privacy & Ring-0 TCB Invariants

1. **Zero External Go Dependencies (`Gate H4-A`):** `SelectFactsForSession`, Okapi BM25 `hq memory search`, `dream-plan`, and `hq memory eval` use 100% Go standard library (`unicode`, `math`, `sort`, `bufio`, `encoding/json`).
2. **Strict Monotonic Taint Isolation ([RFC-0017b](0017b-provenance-taint-and-offer-gateway.md)):**
   - When `taint.IsTainted(vaultDir, convID) == true`, `HandleSessionStart` withholds `MEMORY.md`, `topics/`, `playbooks/`, `facts.jsonl`, `rsi/rules/general.md`, and Matched Context Hints (`<= 800 B` quarantine role card only).
   - `cmd/hq` server-side gates block `memory search`, `memory add-fact`, `memory prune-fact`, `memory playbook`, `memory forget`, and `memory commit` under taint.
   - Tainted turn journals (`TaintedSession == true`) are excluded from `hq memory search`, `fact_telemetry.json` hit counting, and `dream-plan` promotion so untrusted emails/chats can never bias utility scores or inject false memories.
3. **Private Vault & Permission Hardening (`P1`–`P6` & `0600`):**
   - All new files (`playbooks/*.md`, `facts_cold.jsonl`, `fact_telemetry.json`, `journal/archive/YYYY-MM.jsonl`) reside strictly inside git-ignored `<vault>/memory/` (`0700` dirs, `0600` regular non-symlink files via `store.WriteAtomic`).
   - `hq memory forget` cascades across `playbooks/*.md`, `facts_cold.jsonl`, `topics/archived/`, `entities/archived/`, and `journal/archive/*.jsonl` in addition to active stores.

---

## 10. Vault Schema Migration (`v9 -> v10`) & Phased Implementation Plan

### 10.1 Non-Destructive Vault Migration (`CurrentSchemaVersion = 10` in `internal/vault/migrate.go`)

1. Snapshot `local/.git` (`SnapshotVault(vaultDir, "migrate v9->v10")`).
2. Create `<vault>/memory/playbooks/`, `<vault>/memory/topics/archived/`, `<vault>/memory/entities/archived/`, and `<vault>/memory/journal/archive/` (`0700`).
3. Tighten `local/memory/facts.jsonl` permissions to `0600`, infer missing `Kind` / `Scopes` metadata non-destructively in-memory (rewriting canonical deduplicated lines under `store.RankMemory`), and strip any duplicate `## Knowledge Map (Topic Wiki)` block from `local/MEMORY.md` if all listed slugs already exist in `local/memory/topics/*.md`.
4. Archive orphaned `local/memory/progress/<slug>.md` checkpoints whose `slug` is already `completed`/`cancelled` in `tasks.json` or rotated to `tasks_archive.jsonl`.

### 10.2 Phased Implementation & Verification Matrix (Phase 2 — After RFC Approval)

| Sub-Phase | Target Packages & Files | Key Deliverables & Tests | Est. Scope |
| :--- | :--- | :--- | :---: |
| **Phase 2A: Locked `Fact` v2, TTLs & Dual-Cap `SessionStart`** | `internal/store/store.go`, `internal/vault/{vault,memory,migrate}.go`, `internal/hooks/hooks.go`, `templates/MEMORY.md` | `store.RankMemory` (`Rank = 78`) + `0600` atomic writes; `Fact` v2 (`Kind`, `Scopes`, `Salience`, `ExpiresAt`, `PromotedTo`) + `fact_telemetry.json`; `SelectFactsForSession` scoring; `<= 12 KB` / `<= 4.8 KB` `SessionStart` cap + duplicate Topic Map removal + Matched Context Hint. | `~450` LOC |
| **Phase 2B: Scoped Playbooks & Lifecycle Archival** | `internal/vault/{playbooks,progress,entities_and_forget}.go`, `internal/kernel/tasks.go`, `internal/rsi/{miner,mutator}.go`, `internal/hooks/hooks.go` | `<vault>/memory/playbooks/<scope>.md` + `hq memory playbook <list\|show\|upsert>`; Zone 2 scope injection (`0` on FD/taint); `CompactTasks` $\rightarrow$ `ArchiveProgress` hook; `14d` rolling archival for `journal/` & `notes/briefs/`; `forget` cascade update; RSI `repo:<slug>` routing & `occurrenceCount >= 2` noise filter. | `~480` LOC |
| **Phase 2C: Pure-Go Hybrid Search & Salience-Gated Dreaming** | `internal/vault/search.go`, `cmd/hq/main.go`, `.agents/plugins/agy-hq/skills/sleep-consolidate/SKILL.md` | Okapi BM25 `hq memory search` (`<15ms`, `--include-archive`, taint-denied); `hq memory dream-plan --since=24h` (`DREAM_NOOP` quiet-night skip + batch `VerifyTranscript` + broken-link/duplicate/Borges checks); updated `sleep-consolidate/SKILL.md` within `H1` line cap. | `~420` LOC |
| **Phase 2D: Quantitative `hq memory eval`, `hq doctor` & `CUJ-39` E2E** | `internal/vault/eval.go`, `internal/vault/testdata/memory_eval_fixtures.json`, `cmd/hq/main.go`, `internal/e2e/cuj_e2e_test.go`, `docs/product/0003-critical-user-journeys.md` | `hq memory eval` (`M1`–`M6` live + `--fixtures`); `hq doctor` dual-cap & overlap checks; **`TestCUJ39_MemoryV2PredictionScoringPlaybooksSearchAndEval`** black-box E2E test. | `~450` LOC |

---

## 11. Open Design Decisions for User Review

Before beginning Phase 2 implementation, please review and confirm the following design choices:

1. **D1 — `SessionStart` Byte/Token Budgets (`12,000 B` / `~3,000` tok `full`, `4,800 B` / `~1,200` tok `lean_auto`):**
   - **Recommended:** Enforce hard dual caps (`MEMORY.md <= 60L & 3,600 B` with duplicate `## Knowledge Map` removed; `Active Vault Facts <= 15 & 3,000 B` in `full` / `<= 6 & 1,200 B` in `lean_auto`), cutting `SessionStart` by **`~66%`** (`30.9 KB` $\rightarrow$ `<= 12.0 KB`) immediately on migration.
2. **D2 — Dedicated `<vault>/memory/playbooks/<scope>.md` vs. Frontmatter-Tagged `topics/<slug>.md`:**
   - **Recommended (`playbooks/<scope>.md`):** Keep `playbooks/<scope>.md` (`<= 60L / 2.4 KB` succinct operational rules + trap warnings, auto-injected in Zone 2 when working in `repo:<slug>` or `domain:<slug>`) distinct from `topics/<slug>.md` (`<= 6 KB` deep architectural/investment reference pages read on-demand via `read_file` / `hq memory search`). *Alternative:* Merge both into `topics/<slug>.md` using an `auto_inject_scope:` header field.
3. **D3 — Unattended `sleep-consolidate` Zero-Token `DREAM_NOOP` Gatekeeper:**
   - **Recommended:** Enable `hq memory dream-plan --since=24h` as a 0-token precheck so quiet days (`0` untainted sessions, `0` expired TTLs, `!OverBudget`) skip spawning the `23:30` `gemini-4-carbon-medium` worker completely, while active days receive pre-verified transcript digests and wiki link/duplicate checks.
4. **D4 — Rollout Sequencing:**
   - **Recommended:** Ship all four sub-phases (`2A` $\rightarrow$ `2D`) cleanly on this branch (or split `2A+2B` first, then `2C+2D`) after RFC sign-off.
