# RFC-0024: Memory Engine v2 — Orthogonal Single-Source Vault Architecture, Dual-Cap Budgets, Scoped Topic Playbooks, Pure-Go Hybrid Search & Deterministic Memory Eval

- **Status:** Implemented (Draft B+ — Phase 2A–2D complete, CI + CUJ-39 + offline A/B replay verified)
- **Date:** 2026-11-18
- **Author:** `[Task:agy-hq] Memory Architecture Deep Dive & RFC` (`gemini-4-carbon-high` / `gemini-4-neon-high`)
- **Companion Research:** [19 — Anthropic's *"The Components of Agent Memory"* & 12-Day Production Vault Telemetry Audit](../research/19_anthropic_memory_components_and_vault_audit.md)
- **Extends / Updates:** [RFC-0002 (Zero-Token Journaling & Consolidation)](0002-memory-and-consolidation.md), [RFC-0006 (4-Layer Memory Hierarchy & Dreaming)](0006-memory-engine-upgrade.md), [RFC-0010 (Bounded Agent RSI)](0010-agent-recursive-self-improvement.md), [RFC-0012 (Token Observability)](0012-token-observability.md), [RFC-0014 (Proactive Engine v2 & Living Context)](0014-proactive-engine-and-living-context.md), [RFC-0017b (Provenance Taint & Offer Gateway)](0017b-provenance-taint-and-offer-gateway.md), [RFC-0018 (Always-On Runtime Hardening)](0018-always-on-agent-runtime-hardening.md), [RFC-0019 (Two-Tier RSI & Vault Skill Overlay)](0019-two-tier-rsi-vault-overlay.md)

---

## 1. Motivation & Design Philosophy ("Organic Synthesis, Zero Frankenstein")

Synthesizing Anthropic's [*"The components of agent memory"*](https://www.anthropic.com/research/components-of-agent-memory) (Nov 6, 2026), cross-industry benchmarks of **Meta Muse**, **OpenAI `dots`**, **xAI `Grok Bot`/`Grok Build`**, and **Google `Gemini`** ([Research 19 §6](../research/19_anthropic_memory_components_and_vault_audit.md)), and a 12-day empirical audit of `agy-hq`'s live Vault (`226` conversations, `1,337` turns, `31,526` steps — [Research 19 §4](../research/19_anthropic_memory_components_and_vault_audit.md)) revealed eight production regressions alongside four real paradigm tensions:

1. **`SessionStart` Bloat & Double-Entry Bookkeeping (`30,901 B` / `~7,725` tok — `5.15×` Over Target):** `MEMORY.md` gamed its `80`-line cap via `250 B` semicolon lines (`39L = 6,844 B`), injected the 11-page Topic Wiki index **twice verbatim** (`2,186 B` + `3,473 B`), and repeated `USER.md`/`AGENTS.md` rules and `facts.jsonl` priorities (`8,834 B`).
2. **Chronological LIFO Eviction Starvation (`100%` `architecture` & `ux` Loss in `lean_auto`):** `active[len-limit:]` evicted `32.4%` (`12/37`) of active facts in `full` sessions and **`78.4%` (`29/37`) in `lean_auto` — starving `8/8` (`100%`) of `architecture` and `4/4` (`100%`) of `ux` facts** while injecting recent personal/calendar items into SWE subagents.
3. **Missing Fact TTLs (`27.0%`) & Misplaced Procedural Rules (`35.1%`):** Date-bound events sit active indefinitely without `expires_at`, while imperative workflow rules duplicate `AGENTS.md`/`SKILL.md`.
4. **Low Domain Topic Wiki Recall (`42.1%`), Island Graph (`1` Cross-Link) & No Hybrid Search:** `hq memory lookup` searches `entities/` only; there is no pure-Go BM25 `hq memory search` to capture Anthropic's *Wiki + Search* gain (`48%` $\rightarrow$ `61%`).
5. **Broken RSI Noise Funnel (`1,754` Obs $\rightarrow$ `0` Active Rules):** Single-step TDD/compile errors flooded `observations.jsonl`, while rigid template mutators collided with Anthropic's Figure 5 finding that prescriptive `"record only failures"` rules degrade frontier models (`−0.234` on Opus 5).
6. **Unlocked Multi-Writer Mutations & Orphaned Lifecycle Stores:** `internal/vault` bypasses `internal/store` locking (`facts.jsonl` written `0644` non-atomically), `19` auto-generated `progress/*.md` files sit unarchived (`0` archived), and `1,337` daily journal JSON files accumulate uncompacted.

### Core Architectural Principle: Resolving Paradigm Tensions Organically
Rather than bolting on extra directories (`memory/playbooks/`), secondary archive files (`facts_cold.jsonl`), or brittle lexical hit-counting formulas (`fact_telemetry.json` — which suffer from the **Negative Guardrail Paradox**, where obeying *"Never place market orders at open"* produces `0` output-token hits and auto-demotes critical safety rules), **RFC-0024 enforces four clean architectural invariants**:
1. **Single Source of Truth (Mutually Exclusive Store Taxonomy):** Every piece of knowledge has **exactly one** canonical home determined by `(Abstraction × Scope × Loading Mode × Governance Ring)`.
2. **Simple Deterministic Physics in Go; Semantic Salience in Dreaming:** Pure Go enforces hard byte/line budgets, 0-token `expires_at` expiry, deterministic scope filtering, and atomic locking; frontier nightly dreaming judges semantic consolidation, promotion, and pruning.
3. **Epistemic Source Split (Fast Human Truth vs. Slow Verified Experience):** Direct user-stated preferences/corrections write synchronously (`hq memory add-fact`, `0s` cross-tab latency); agent-discovered lessons consolidate strictly offline after `VerifyTranscript` checks.
4. **Strict Ring-2 vs. Ring-3 & Provenance Taint Boundaries:** Unattended Ring-3 dreaming never bypasses Ring-2 human approval (`hq propose`) for behavioral policy changes, and tainted sessions ([RFC-0017b](0017b-provenance-taint-and-offer-gateway.md)) have `0` read/write access to durable Vault memory.

---

## 2. Orthogonal Store Taxonomy & System Dataflow

### 2.1 Single Source of Truth Matrix (Eliminating Store Proliferation)

| Store | Canonical Role (Exclusive Ownership) | Loading Mode & Hard Budget | Governance Ring & Writer |
| :--- | :--- | :--- | :--- |
| **Plugin & Repo `AGENTS.md`** | Universal `agy-hq` OS invariants (plugin) & public team repo rules (repo). | Antigravity `<user_rules>` (**cross-session KV-cached prefix**; never duplicated in `USER.md`/`MEMORY.md`). | **Ring 0/1** (Git PR only) |
| **`<vault>/USER.md`** | User identity, communication style, personal risk/autonomy constitution. | Zone 1 (`<= 2,000 B` advisory; compact slice `<= 1,800 B` in `lean_auto`). | **Explicit User Only** (never auto-edited by dreaming) |
| **`<vault>/MEMORY.md`** | **Active Priorities Roadmap ONLY** (`5–8` active threads reconciled with `goals.json` & `tasks.json`; **zero** `Knowledge Map`, **zero** `Durable Preferences & Facts`). | Zone 1 on `front_desk` & `full_task` (`<= 40L & <= 2,200 B`); **omitted in `lean_auto`**. | **Ring 3** (nightly dreaming) |
| **`<vault>/memory/topics/<slug>.md`** | Domain/repo reference synthesis (`<= 120L / 6 KB`). Optional top **`## Active Playbook`** (`<= 25L / 1.2 KB`, **at most `1` owner per `repo:X`/`domain:Y`**) for build/test commands & verified technical traps. | 1-line summary in Zone 1 Index (filtered to matched scopes in `lean_auto`); `## Active Playbook` slice in Zone 2 **only** on scope match (`0 B` on Front Desk & tainted); full page via `read_file` / `hq memory search`. | **Ring 3** (transcript-verified technical/env facts only) |
| **`<vault>/skills/<slug>/SKILL.md` & `rsi/rules/<core>.md`** | Multi-step `"how to run workflow X"` runbooks + personal behavioral/style overlays on Core OS skills (`morning-brief`, `sleep-consolidate`, `general.md`). | Progressive disclosure (`SKILL.md` frontmatter; `hq dispatch resolve` inline overlay; `general.md <= 5` rules in Zone 1). | **Ring 2** (User or `hq rsi` + `hq propose` / `hq approve`) |
| **`<vault>/memory/facts.jsonl`** | Single canonical bi-temporal file for atomic (`<= 240` runes) state, user constraints & `expires_at`-bounded commitments (`valid_to != nil` / `now > expires_at` is cold storage; **no `facts_cold.jsonl`, no `fact_telemetry.json`**). | Zone 2 deterministic scope selection (`<= 12` / `2,400 B` `full`, `<= 6` / `1,000 B` `lean_auto`). | **Fast Path:** Sync `add-fact` on direct user input<br>**Slow Path:** Dreaming |
| **`entities/*.md`, `goals.json`, `progress/*.md`, `notes/**`** | Counterparty routing (`entities`), SLA/milestone state (`goals`), `1:1` active-task 120k handoff (`progress`), long-form memos (`notes`). | `0 B` idle (`progress` loaded only for matching `TaskSlug`; queried via `lookup`/`search`/`goals`). | Deterministic `hq` CLI |

> **Where User Preferences Live (Preferences Are First-Class Memory, Partitioned by Scope):**
> - **Global Persona & Style Constitution** *(rare-change defaults applying to ~100% of turns)* $\rightarrow$ **`<vault>/USER.md`**.
> - **Domain / Scoped Preferences & Dynamic Constraints** *(e.g. travel cabin class, trading guards, social tone, time-bound overrides)* $\rightarrow$ **`<vault>/memory/facts.jsonl`** (written synchronously from natural language, isolated by `Scopes` so unrelated SWE subagents never carry travel/family preferences).
> - **Per-Counterparty Style & Channel Preferences** *(e.g. "prefer GChat bullets before 10 AM for Alice")* $\rightarrow$ **`<vault>/memory/entities/<slug>.md`** (`0 B` idle; fetched via `hq memory lookup`).
> - **Multi-Step Procedural Workflow Preferences** *(e.g. Morning Brief layout or recurring forwarding checklists)* $\rightarrow$ **`<vault>/skills/<slug>/SKILL.md`** or **`rsi/rules/<core>.md`** (progressive disclosure, Ring-2 gated).

### 2.2 Architecture Dataflow

```mermaid
flowchart TB
    subgraph L4["Layer 4: Immutable Host Ground Truth (Antigravity Brain)"]
        TR["transcript.jsonl &amp; transcript_full.jsonl\n(Read-only via hq surrogate; PreToolUse blocks LLM tampering)"]
    end

    subgraph L3["Layer 3: Leaf-Locked Episodic &amp; Bi-Temporal Stores (store.RankMemory = 78, 0600)"]
        JL["journal/YYYY-MM-DD/*.json (14d hot)\n-&gt; journal/archive/YYYY-MM.jsonl (cold)\nShared vault.WalkJournals + HandleStop archive fallback"]
        FC["Single Canonical facts.jsonl (&lt;= 240c/fact)\n• scope | expires_at | pinned | valid_from | valid_to\n(Soft-expired rows stay in-file at 0 SessionStart cost)"]
        PR["progress/&lt;slug&gt;.md (1:1 active tasks)\n-&gt; progress/archived/ via cmd/hq + CompactMemory"]
    end

    subgraph L2["Layer 2: Structured Representations + Lock-Free Pure-Go BM25 Search"]
        TP["topics/&lt;slug&gt;.md (&lt;= 25 active, &lt;= 6 KB)\n• Optional Scopes + 1:1 ## Active Playbook (&lt;= 25L / 1.2 KB)\n• Cross-linked Reference &amp; Deep Context"]
        EN["entities/&lt;slug&gt;.md (&lt;= 100 active, &lt;= 10 notes)"]
        SRCH["hq memory search &lt;query&gt;\n• Pure-Go stdlib Okapi BM25 (&lt;15ms, 0 CGo/SQLite)\n• Excludes all private/**, outbox/**, symlinks &amp; tainted logs"]
    end

    subgraph L1["Layer 1: Role-Aware Dual-Cap SessionStart (Lock-Free, &lt;10ms)"]
        Z1["Zone 1 (Global Core)\n• full/FD: USER.md + MEMORY.md (&lt;= 2.2 KB) + Topic Index + general.md\n• lean_auto: USER.md (&lt;= 1.8 KB) + general.md + Filtered Topic Index\n  (omits MEMORY.md so Zone 2 is never starved!)"]
        Z2["Zone 2 (Scope-Matched Tail)\n• Matched ## Active Playbook (&lt;= 1.2 KB total; 0 on FD/taint)\n• Matched Context Hint (Task/Subagent only; 0 on FD)\n• Scope-Filtered Facts (&lt;= 12 full / &lt;= 6 lean; temporal precedence)"]
    end

    subgraph Loop["Salience-Gated Dreaming &amp; 4-Metric Deterministic Eval"]
        DP["hq memory dream-plan\n• --precheck: 0 bytes on idle (exit_zero_nonempty)\n• Stage 1 --json: VerifyTranscript batch + rejected negative constraints\n• dream_cursor.json updated ONLY on Stage 4 hq memory commit"]
        EV["hq memory eval --since=14d [--fixtures]\n• M1 Budget &amp; 5-Gram Overlap · M2 Starvation &amp; TTL Hygiene\n• M3 Ground-Truth Interventions &amp; Denials · M4 Wiki Graph Integrity"]
    end

    TR --&gt;|"0-token HandleStop"| JL
    JL &amp; TR --&gt; DP &amp; EV
    DP --&gt;|"Non-empty delta"| TP &amp; EN &amp; FC
    Z1 &amp; Z2 --&gt;|"Lock-free read-only"| SESS["Active Agent Session"]
    SESS -.-&gt;|"read_file / hq memory search / lookup"| TP &amp; EN &amp; SRCH
```

---

## 3. Pillar 1: Bi-Temporal `Fact` v2, Deterministic Scope Physics & Epistemic Source Split

### 3.1 Minimal Backward-Compatible `Fact` v2 Schema (`internal/vault/vault.go`)

Existing `facts.jsonl` lines deserialize unchanged. Rather than introducing a 4-kind decay taxonomy, `fact_telemetry.json`, or `facts_cold.jsonl`, `Fact` gains **only three orthogonal, deterministic metadata fields**:

```go
type Fact struct {
	ID           string   `json:"id"`
	Topic        string   `json:"topic"`
	Statement    string   `json:"statement"`              // Enforced <= 240 runes on AddFact
	SourceConv   string   `json:"source_conv,omitempty"`
	Verified     bool     `json:"verified,omitempty"`
	ValidFrom    string   `json:"valid_from"`
	ValidTo      *string  `json:"valid_to"`
	SupersededBy string   `json:"superseded_by,omitempty"`

	// RFC-0024 Additions (100% backward-compatible omitempty):
	Scopes       []string `json:"scopes,omitempty"`       // e.g. ["global"], ["repo:agy-hq"], ["domain:investing"], ["skill:gchat"]
	ExpiresAt    string   `json:"expires_at,omitempty"`   // Optional RFC3339 0-token auto-expiry timestamp
	Pinned       bool     `json:"pinned,omitempty"`       // Always eligible for matching scope ahead of unpinned items
}
```

- **Single Canonical File (`facts.jsonl`):** `LoadFactsStateAt(vaultDir, now)` already partitions `active` (`valid_to == nil && (expires_at == "" || now <= expires_at)`) from `expired` (`valid_to != nil || now > expires_at`) in `< 1.2ms` over `2,000+` lines. Keeping expired facts in canonical `facts.jsonl` preserves `AddFact`'s ID uniqueness check (`memory.go:170`), `PruneFact` idempotency, and `hq memory search --include-archive` with zero split-brain storage. `CompactFacts` materializes `ValidTo = &ExpiresAt` on newly elapsed TTL rows.

### 3.2 Epistemic Source Split, Live-Connector Grounding & Anti-Tamper Guards

1. **Fast Path (Natural-Language User-Asserted Truth $\rightarrow$ Silent Sync `hq memory add-fact`):**
   - **100% Natural-Language UX for the User:** The user never needs to invoke a slash command or CLI flag manually—simply saying *"Never book Basic Economy; minimum Economy Plus"* or *"Raise my limit from \$82 to \$85"* in normal chat triggers the fast path. Governed by a 1-line directive in `.agents/plugins/agy-hq/rules/AGENTS.md`, the agent **silently** runs `hq memory add-fact --verified` under the hood (`< 5ms`, `0` Front Desk lookup quota, zero chat bookkeeping chatter), making the preference or override live across all sibling tabs in `0s`.
   - **Auto-Session Guard:** `cmd/hq` blocks `hq memory add-fact` in all unattended `[Auto]` sessions (`isAutoConversation`) **except** `sleep-consolidate` (and blocks tainted callers unconditionally), and `AGENTS.md` forbids Front Desk from calling `add-fact` in response to proactive alert or timer wakeup text.
   - **Normative vs. Hypothesis Rule:** User *preferences, constraints, and decisions* write `--verified=true`; unverified debugging guesses do not.
2. **Slow Path (Agent-Discovered Experience $\rightarrow$ Offline Transcript-Verified Dreaming):**
   - Agents never pause mid-coding/research to write speculative notes; `HandleStop` writes 0-token turn journals and `/sleep-consolidate` verifies outcomes against `transcript.jsonl` before updating `topics/` or `facts.jsonl`.
   - **Ephemeral Scratch Opt-Out (`hq spawn --ephemeral`):** Sets `Ephemeral: true` on `kernel.ActiveTask` (*Gemini Temporary Chats* pattern), excluding throwaway brainstorm threads from `dream-plan` and `hq rsi mine`.
3. **Temporal Precedence Directive (Resolving Same-Day Fast-Path Overrides):**
   - Zone 2's `## Active Vault Facts` header includes one explicit sentence:
     > *"`verified` facts carry `valid_from` timestamps; if a newer `verified` fact contradicts older summary prose in `MEMORY.md` or `topics/`, honor the newer fact until nightly dreaming reconciles the wiki."*
4. **Refined Live-Connector Grounding Rule:**
   - Prohibit copying **raw queryable connector snapshots** (daily Calendar agendas, unread Gmail lists, open GitHub PR/CI tables) into `facts.jsonl` or `MEMORY.md`.
   - Explicitly **preserve** non-derivable human commitments, unbooked travel/planning gaps (which do not exist on Calendar), cross-source synthesis, and standing routing rules—requiring `--expires=<RFC3339|YYYY-MM-DD|+dur>` on every date-bounded commitment (`AddFact` rejects statements `> 240` runes and warns when a calendar date appears without `--expires`).

### 3.3 Deterministic Go Fact Selection (`SelectFactsForSession`)

Zero ML hyperparameters, zero lexical hit-counting, zero Negative Guardrail Paradox:

1. **Eligibility Filter by Role & Scope:**
   - **Scoped Task / `lean_auto` Subagent / Worktree Session:** Eligible set = `{ f | f.Scopes matches active repo:<X> or domain:<Y> or skill:<Z> }` $\cup$ `{ f | f.Scopes contains "global" && f.Pinned }` $\cup$ `{ f | lexical stem match >= 2 against Turn-1 prompt }`. **Unrelated personal/domain facts are excluded (`0%` leakage).**
   - **`[HQ] Front Desk` (`Role == "front_desk"`):** All active non-expired facts are eligible, subject to a **per-topic diversity cap of `<= 3` facts per `Topic`** (preventing any single noisy domain from crowding out other domains).
2. **Deterministic Priority Sort (Exact Tiered Comparator):**
   - **Tier 1:** Scope-matched `Pinned` facts & imminent TTL facts (`0 < ExpiresAt - now <= 72h`).
   - **Tier 2:** Scope-matched `Verified` facts (or `global` `Pinned` invariants).
   - **Tier 3:** Remaining eligible `Verified` facts, then unverified facts.
   - **Tie-breaker within each tier:** Newest `ValidFrom` descending, then `ID` ascending (100% deterministic).
3. **Hard Dual-Cap Packing:** Pack sorted facts until hitting `MaxFacts` (`12` in `full`, `6` in `lean_auto`) **or** `MaxFactBytes` (`2,400 B` in `full`, `1,000 B` in `lean_auto`). Semantic pruning of excess hot facts (`> 50` active) is flagged to nightly dreaming rather than auto-evicted by lexical heuristics.

---

## 4. Pillar 2: Role-Aware Dual-Cap Budgets, `MEMORY.md` Cleanup & Automated Lifecycle Archival

### 4.1 Fixing `lean_auto` Budget Starvation & `MEMORY.md` Double-Entry Bookkeeping

Auditing `internal/hooks/hooks.go:431-480` revealed that naive Zone 1 $\rightarrow$ Zone 2 priority allocation under a `4,800 B` `lean_auto` cap would exhaust the entire budget inside `MEMORY.md` (`9–10 KB` Zone 1), starving `100%` of Zone 2 (`0` playbooks, `0/6` facts). We fix both root causes cleanly:

1. **Make `local/MEMORY.md` Exclusively the `Active Priorities` Roadmap (`<= 40` Lines & `<= 2,200 B`):**
   - Remove **both** `## Knowledge Map (Topic Wiki)` (100% duplicated by `LoadTopicIndex`) **and** `## Durable Preferences & Facts` (triple-duplicated across `AGENTS.md`, `USER.md`, and `facts.jsonl`).
   - Update `templates/MEMORY.md` and `sleep-consolidate/SKILL.md` Stage 3 so `MEMORY.md` holds only `5–8` crisp active priority bullets reconciled with `goals.json` and `tasks.json` (`<= 180 B/line` lint).
2. **Role-Aware `SessionStart` Zone Allocation (Guaranteed Zero Zone-2 Starvation):**

| Profile | Total Hard Cap | Zone 1 Allocation | Zone 2 Allocation |
| :--- | :---: | :--- | :--- |
| **`front_desk`** | **`<= 12,000 B`** (`~3,000` tok) | `USER.md` (`<= 2,000 B`) + `MEMORY.md` (`<= 2,200 B`) + Canonical Topic Index (`<= 2,200 B`) + `general.md` (`<= 500 B`) | `0 B` Playbooks + `0 B` Context Hint + Diverse Facts (`<= 12` / `2,400 B`) + Reported/Tasks/Worktrees/Triggers (`<= 2,700 B`) |
| **`full_task`** | **`<= 12,000 B`** (`~3,000` tok) | Same compact Zone 1 (`<= 6,900 B`) | Matched `## Active Playbook` (`<= 1,200 B`) + Matched Context Hint (`<= 250 B`) + Scoped Facts (`<= 12` / `2,400 B`) + Active `progress/<slug>.md` / Dynamic Tail |
| **`lean_auto`** *(Subagent / `[Auto]`)* | **`<= 4,800 B`** (`~1,200` tok) | **`USER.md` (`<= 1,800 B`) + `general.md` (`<= 400 B`) + Scope-Filtered Topic Index (`<= 400 B` + 1-line `hq memory search` pointer); `MEMORY.md` OMITTED!** | Matched `## Active Playbook` (`<= 1,200 B`) + Scoped Facts (`<= 6` / `1,000 B`) |
| **`tainted`** | **`<= 800 B`** (`~200` tok) | Quarantine role card only (`0 B` Vault memory) | `0 B` (`0` facts, `0` playbooks, `0` hints) |

### 4.2 Anti-Hoarding Store Caps & Safe Lifecycle Archival

| Store | Hard Caps | Automated Archival & Integrity Rules |
| :--- | :--- | :--- |
| **`memory/topics/*.md`** | `<= 25` active; `<= 120L` & `<= 6,000 B`/page; `<= 100 KB` total | Pairwise token Jaccard `>= 0.45` flagged as merge candidate; pages with `0` reads & `0` edits for `> 60d` move to `topics/archived/<slug>.md`; **max `1` `## Active Playbook` owner per scope**; `DomainCompressionRatio` checked only when `Bytes(notes/) >= 20 KB`. |
| **`memory/entities/*.md`** | `<= 100` active; `<= 25L` & `<= 10` bullets/dossier | `UpsertEntity` rolls oldest bullets beyond `10` into `entities/archived/<slug>-history.md`; inactive `> 180d` entities with `0` `WaitingOn` SLAs archive to `entities/archived/`. |
| **`memory/progress/*.md`** | `<= 60L` (`<= 2,400 B`); **1:1 active tasks** | **No circular import:** `internal/kernel` does **not** import `internal/vault` (`vault/migrate.go:11` already imports `kernel`). Instead, `cmd/hq` (`tasks compact`, `memory compact`, `archive`) calls `vault.ArchiveProgressBatch(vaultDir, res.RotatedColdSlugs, now)`, backed by an idempotent orphan sweep in `vault.CompactMemory(vaultDir, activeTaskSlugs)`. |
| **`notes/briefs/`** | `14d` hot window | `hq memory compact` moves briefs `> 14d` to `notes/briefs/archive/YYYY-MM/`. |
| **`memory/journal/`** | `14d` hot JSON dirs $\rightarrow$ `journal/archive/YYYY-MM.jsonl` | 1. Shared **`vault.WalkJournals(vaultDir, since, includeArchive, fn)`** skips `d.Name() == "archive"` across `hooks.go`, `obs.go`, `rsi/miner.go`, `VerifyTranscript`, `dream-plan`, and `eval`.<br>2. **`HandleStop` Step-Delta Guard (`hooks.go:2426`):** If `priorJournalStepCountWithDate` finds `0` hot entries for an existing multi-turn `transcript.jsonl`, it falls back to `journal/archive/` (or the final `USER_INPUT` step index) so `> 14d` conversations (e.g. pinned Front Desk) never reset `stepStartIdx` to `0` or trip false `A3` budget breakers. |

---

## 5. Pillar 3: Scoped Topic Playbooks (`topics/<slug>.md`), Host-Contract Scope Resolution & Ring-2/Ring-3 Governance

### 5.1 Folding Scoped Playbooks into Canonical `topics/<slug>.md` (Zero Extra Directory)

Rather than creating a redundant `<vault>/memory/playbooks/` store alongside `topics/`, every repo or domain maintains **one canonical file** under `local/memory/topics/<slug>.md` with an optional structured header and top `## Active Playbook` slice:

```markdown
# agy-hq Architecture & Harness
> **Summary:** Two-layer Vault boundaries, Front Desk invariants, pure-Go stdlib contract, and build/test gates.
> **Scopes:** `repo:agy-hq`
> **Playbook-Owner:** `repo:agy-hq`

## Active Playbook
- **Build & Test:** `go build -o ./bin/hq ./cmd/hq && ./bin/hq ci pre-push` (`go.mod` must stay `0` external deps, `H4-A`).
- **Verified Trap** (`verified:1412e19`): Approval subprocesses must inherit `SanitizedApprovalEnv` with `ANTIGRAVITY_PLUGIN_SHIM_DIR` preserved so plugin shims resolve.

## Reference & Deep Context
*(Read on demand via `read_file` or `hq memory search`; never auto-injected at `SessionStart`...)*
```

- **`1:1` Primary Playbook-Scope Owner Rule (Solving `N:1` Domain Explosion):** Multiple topics may tag `> **Scopes:** domain:investing` (for filtered `Topic Index` and `hq memory search`), **but at most ONE topic per `repo:<X>` or `domain:<Y>`** may declare `> **Playbook-Owner:** <scope>` and an auto-injected `## Active Playbook` section (`<= 25` lines / `<= 1,200 B` aggregate Zone 2 cap across all matched scopes). `hq memory status` and `hq doctor` flag duplicate owners.

### 5.2 Host-Contract-Verified `ResolveSessionScopes` & Turn-1 Prompt Extraction

Auditing Antigravity's `hooks.md §3` and [RFC-0017b §1.2](0017b-provenance-taint-and-offer-gateway.md) proved two host realities that Phase 2 must handle explicitly:
1. **`SessionStart` Stdin Omits `lastUserInput`:** When `payload.LastUserInput == ""`, `HandleSessionStart` performs a shared, bounded `< 16 KB` read of `payload.TranscriptPath`'s initial `USER_INPUT` step (falling back to `matchedTask.Title`).
2. **Umbrella `headquarter` `WorkspacePaths` on Subagents:** Subagents spawned from `headquarter` inherit `WorkspacePaths = [".../headquarter"]` even when targeting `<vault>/worktrees/blogs-personal-blog/`. `ResolveSessionScopes` therefore resolves `repo:<slug>` in strict specificity order:
   1. `<vault>/worktrees/<repo>-<task>` parsed from `matchedTask.WorktreeDir` or Turn-1 prompt path,
   2. `matchedTask.Scope` / `matchedTask.RepoRoot`,
   3. `payload.WorkspacePaths[0]` basename / git root.
3. **Matched Context Hint (Task & Subagent Sessions Only):** Suppressed on `[HQ] Front Desk` (whose single Turn-1 `SessionStart` spans 100+ diverse turns). On Task/Subagent sessions, when the Turn-1 prompt or scope matches `1–2` `topics/<slug>.md` pages by exact slug or $\ge 2$ high-IDF domain tokens, Zone 2 prepends a 2-line callout (`## 📌 Matched Vault Context`) pointing to `read_file` on those pages.

### 5.3 Strict Ring-2 RSI vs. Ring-3 `## Active Playbook` Governance Boundary

- **Ring 3 (`topics/<slug>.md` `## Active Playbook`, Auto-Consolidated by Dreaming):** Restricted strictly to **objective technical/environmental context** (build/test commands, codebase invariants, external platform constraints, and `VerifyTranscript`-confirmed technical traps). Never encodes behavioral policy, approval thresholds, communication style, or `thesis-check` kill criteria.
- **Ring 2 (`<vault>/skills/*/SKILL.md` & `<vault>/memory/rsi/rules/*.md`, Human-Approved via `hq propose`):** Owns all behavioral policy, workflow procedures, and Core OS skill overlays.
- **Eliminating RSI vs. Dreaming Collision:**
  - `hq rsi` does **not** route robotic template strings into `topics/` or `## Active Playbook`.
  - `internal/rsi/miner.go` requires **`occurrenceCount >= 2` across distinct conversations** before persisting raw `FrictionErrorRecovery` observations (eliminating the `1,335`-row TDD noise flood) and compacts observations `> 90d` to `rsi/archive.jsonl`.
  - Automatic `hq rsi hypothesize` targets **explicit human friction signals** (`FrictionUserIntervention`, `FrictionDraftEdited`, `FrictionProactiveNoise`) and opt-in `--upstream` maintainers.
  - `dream-plan` feeds recent `rejected` entries from `interventions.jsonl` as **Negative Constraints** so nightly dreaming never re-encodes a human-rejected rule into Ring-3 memory.

---

## 6. Pillar 4: Lock-Free Pure-Go Hybrid Search (`hq memory search`) & Safe `dream-plan` Gatekeeper

### 6.1 Pure-Go Okapi BM25 Search (`hq memory search`)

```sh
hq memory search "<query>" [--scope=<scope>] [--layers=topics,entities,facts,notes,journal] \
  [--include-archive] [--limit=8] [--json]
```

- **100% Pure Go Stdlib (`internal/vault/search.go`, `< 15ms`, Lock-Free):** Tokenizes with Unicode case-folding, identifier splitting (`camelCase`, `kebab-case`, `snake_case`), and lightweight suffix stemming; ranks chunks with Okapi BM25 ($k_1 = 1.2, b = 0.75$) × layer authority (`topics` `1.25×`, `entities` `1.15×`, active `facts` `1.10×`, `notes` `1.00×`, untainted `journal` `0.80×`, `archived` `0.65×`). Every result prints glass-box provenance (`file://<absPath>#L<start>-L<end>`, `valid_from`, `source_conv`, `verified`).
- **Hard Security & Symlink Exclusions:**
  - Server-side `cmd/hq` blocks `memory search` under taint (`taint.IsTainted`).
  - Unconditionally excludes **ALL** of `<vault>/private/**` (including `private/ledger/` and `private/taxes/`), `<vault>/approvals/**`, `<vault>/inbox/**`, `<vault>/worktrees/**`, `<vault>/repos/**`, `<vault>/memory/{channels,outbox,events*,taint,pending}/**`, and any journal entry where `TaintedSession == true`.
  - Every walked path checks `d.Type()&os.ModeSymlink == 0` (`Lstat`) to prevent symlink escapes.

### 6.2 Gatekeeper-Safe `hq memory dream-plan` (`gatekeeper.go:586–605` Verified)

To integrate cleanly with `internal/kernel/gatekeeper.go` (which spawns on any non-empty stdout under `exit_zero_nonempty` and truncates precheck stdout to `300` runes):
1. **`triggers.yaml` Pre-Check Mode (`dedup_mode: exit_zero_nonempty`):**
   - `precheck_cmd: ["hq", "memory", "dream-plan", "--precheck"]` checks `<vault>/memory/dream_cursor.json`.
   - If `0` untainted non-ephemeral journals, `0` new `interventions.jsonl` items, `0` elapsed `ExpiresAt` facts, and `!OverBudget` occurred since the cursor, prints **0 bytes (`""`)** and exits `0` (**`0` Carbon LLM tokens burned on every idle night, including Night 1**).
   - If non-empty delta exists, prints a `< 200`-char 1-line summary (`"delta: 7 untainted convs, 2 expired TTL facts, 1 broken wiki link"`).
2. **Untruncated Stage-1 Execution & Commit-Bound Cursor:**
   - Inside `sleep-consolidate/SKILL.md` Stage 1, the worker runs `hq memory dream-plan --json` directly (bypassing `gatekeeper.go`'s `300`-rune cap) to receive batched `VerifyTranscript` digests for salient turns (`TurnErrorCount > 0`, `PreToolDenials > 0`, `>= 5` steps), elapsed TTL facts, stale `MEMORY.md` slugs vs. `goals.json`/`tasks.json`, recent `rejected` `interventions.jsonl` **Negative Constraints**, and wiki graph hygiene alerts (broken links, Jaccard `>= 0.45` near-duplicates, duplicate `Playbook-Owner` claims).
   - `dream_cursor.json` advances **exclusively inside `hq memory commit`** at Stage 4 completion (never during `--precheck`), guaranteeing a failed or quota-blocked run never drops unprocessed journals.

---

## 7. Pillar 5: Three-Layer Memory Quality Evaluation & Offline Pre-Merge A/B Gate

Deterministic hygiene metrics (`M1`–`M4`) verify memory *plumbing*, but proving that Memory v2 delivers strictly higher **end-to-end answer quality** than v1 requires a three-layer evaluation ladder that cleanly separates **offline pre-merge verification** (on cloned `/tmp` Vault snapshots) from **post-merge live production telemetry**:

### 7.1 Layer 1 — Offline 0-Token Historical Replay (Pre-Merge, `< 5s`)
- Snapshot the live Vault read-only into two throwaway directories: `/tmp/vault-v1` (untouched v1, driven by `main`'s `~/.local/bin/hq`) and `/tmp/vault-v2` (migrated via the PR branch's `./bin/hq migrate`).
- Run `hq memory eval --replay --baseline-bin=~/.local/bin/hq --baseline-vault=/tmp/vault-v1` across historical untainted `local/memory/journal/` turns (`~170` real prompts/workspaces) in **0 LLM tokens** to quantify exact before/after `SessionStart` byte reduction, `0%` Zone-2 starvation in `lean_auto`, and scope-matching accuracy on real user history before merging.

### 7.2 Layer 2 — Offline 20–30 Task Blinded `v1` vs. `v2` A/B Quality Benchmark (Pre-Merge Ship Gate, `~5–10m`)
- Executed strictly **offline on the PR branch** against the cloned `/tmp/vault-v1` vs. `/tmp/vault-v2` snapshots (`0` mutation to live `local/` Vault; `0` waiting on production traffic):
  - **5 Balanced Task Categories (`20–30` curated prompts; private personal eval file in `<vault>/memory/eval_suite.jsonl` + PII-free synthetic set in `internal/vault/testdata/`):**
    1. *Constraint & Negative-Guardrail Adherence* (e.g., social reply style, opening-bell trading guard, counterparty preferences);
    2. *Scoped Engineering Recall (`lean_auto` Subagent)* (does `## Active Playbook` + scoped facts cut failed commands and tool steps?);
    3. *Multi-Hop Deep Synthesis* (Wiki + `hq memory search` vs. v1 baseline);
    4. *Temporal Freshness & Contradiction Override* (newer `verified` fast-path fact overriding older `MEMORY.md` prose; elapsed `expires_at` event ignored);
    5. *Anti-Distraction & Taint Isolation* (`0` unrelated personal leakage into SWE tasks; `0` Vault leakage in `tainted` mode).
  - **Execution & Blinded Grading:** Run identical fresh isolated headless sessions (`agy -p`, same model) under **Arm A (`main` v1 `SessionStart`, no `hq memory search`)** vs. **Arm B (PR v2 `SessionStart` + `hq memory search`)**, grading both arms via deterministic checklist assertions plus a **blinded LLM-as-Judge** (higher-tier model unaware of Arm A/B labels).
  - **Pre-Merge Ship Criteria:** Arm B must strictly beat Arm A on overall Rubric Pass Rate with **zero category regressions** (catching any unintended side effect of omitting `MEMORY.md` in `lean_auto`), equal-or-lower median tool steps/tokens, and `>= 50%` lower `SessionStart` uncached tokens.

### 7.3 Layer 3 — Post-Merge Live Production Telemetry (`hq memory eval`, 7–14d Canary)
After merging to `main` and running `hq upgrade` (which automatically commits a `local/.git` pre-migration restore point), compare `hq obs` cold-start uncached tokens and **`hq memory eval [--since=14d] [--fixtures] [--json]`** against the 12-day v1 baseline recorded in [Research 19 §4](../research/19_anthropic_memory_components_and_vault_audit.md). Replaces noisy heuristics (`fact_telemetry.json`, regex `TranscriptCorrectionTurns`, raw command-hash `RepeatFrictionRate`) with **four 100% deterministic, high-signal metrics**:

| Metric | Exact 0-Token Definition | Target |
| :--- | :--- | :---: |
| **`M1`: `SessionStart` Budgets & 5-Gram Overlap** | Exact bytes & tokens across `front_desk`, `full_task`, `lean_auto` vs. caps (`12 KB` / `4.8 KB`), plus 5-gram Jaccard overlap across `{AGENTS.md, USER.md, MEMORY.md, active facts}`. | `<= 100%` budget;<br>`< 10%` overlap |
| **`M2`: Scoped Starvation & TTL Hygiene** | (a) `%` of `pinned` or scope-matched active facts evicted in `lean_auto`; (b) `%` of active date-bearing event facts missing `expires_at` or past deadline. | **`0%` starvation** *(baseline `100%`)*;<br>`0%` overdue TTL |
| **`M3`: Ground-Truth Friction & Interventions** | Ground-truth `interventions.jsonl` (`rejected` excluding `--dismiss`, `user_feedback`, edited Gmail/GChat drafts) + `PreToolDenials` per `100` interactive untainted turns (7d vs. prior 7d). | Monotonic downward trend |
| **`M4`: Wiki Graph Integrity & Guarded Compression** | `0` broken local markdown links, `0` missing `# Title`/`> **Summary:**`, `0` duplicate `Playbook-Owner` claims, `0` near-duplicate topic pairs (`Jaccard >= 0.45`), `<= 6 KB`/page & `<= 100 KB` total. Computes `DomainCompressionRatio = Bytes(topics/) / Bytes(notes/)` **only when `Bytes(notes/) >= 20 KB`** (`<= 0.35`). *(Fixes `AuditMemory` `OrphanTopicSlugs` in `memory.go:374` so stripping `## Knowledge Map` from `MEMORY.md` no longer marks `100%` of topics as orphans!)* | `0` structural errors;<br>ratio `<= 0.35` when applicable |

- **Observational Diagnostic (Non-Gating):** Reports `(topic_reads / hints_shown, N)` as an informational counter without a brittle small-$N$ pass/fail gate.
- **Golden CI Regression Suite (`hq memory eval --fixtures`):** Deterministic synthetic Vault fixtures in `internal/vault/testdata/memory_eval_fixtures.json` asserting: (F1) `100%` scoped `repo:agy-hq` invariant recall & `0%` personal leakage in `lean_auto` within `<= 4,800 B` (verifying `0%` Zone-2 starvation); (F2) `expires_at` `72h` boost and `T+1s` 0-token auto-expiry; (F3) `MEMORY.md` byte-cap defense against `350 B` lines; (F4) broken link, duplicate `Playbook-Owner`, and Jaccard `>= 0.45` detection.

---

## 8. Pillar 6: Antigravity Native Memory Coexistence, Security & Lock Safety

### 8.1 Coexistence with Antigravity Native Surfaces (`v2.29` / CLI `1.5.11`)
- **`AGENTS.md` / `GEMINI.md` (`<= 24 KB` / `20k` host budget):** Public team & OS invariants only (cross-session KV-cache prefix).
- **`.agents/skills.json` $\rightarrow$ `<vault>/skills/`:** Multi-step procedural runbooks (`0` idle tokens).
- **`/learn` & `/compact`:** Personal workflows route to `<vault>/skills/` or scoped `topics/`; `/compact` owns intra-session compression while `120k` `progress/<slug>.md` bridges active task handoffs and auto-archives on task completion.
- **Host `conversation_summaries.db` (`conversations_fts`) & `transcript.jsonl`:** Pure-Go `hq memory search` stays independent of undocumented host SQLite (`0` CGo/external deps, `H4-A`), enforcing taint & `forget`; `hq memory forget` reports originating `source_conv` IDs so users know if raw host transcripts in `~/.gemini/antigravity/brain/<convId>` should also be deleted in Antigravity UI.

### 8.2 Security, Taint, Concurrency (`store.RankMemory = 78`) & Complete `forget` Cascade
1. **Uniform Server-Side Taint Gate (`cmd/hq/main.go`):** Place `if sub != "lookup" { denyIfCallerTainted(vaultDir, "memory "+sub) }` at the top of `case "memory":` (closing gaps on `status`, `topics`, `compact`, `verify-transcript`, `search`, `dream-plan`, `eval`).
2. **Deadlock-Free `store.RankMemory = 78` Discipline (`internal/store/store.go`):**
   - `RankExtensions (10) < RankGoals (50) < RankOutbox (60) < RankTaint (70) < RankTasks (75) < RankMemory (78) < RankSubagentQueue (80) < RankQuota (85)`.
   - **All readers are 100% lock-free** (atomic `os.Rename` via `store.WriteAtomic(..., 0o600)`).
   - **Writers acquire `RankMemory` strictly around leaf file writes** (`AddFact`, `PruneFact`, `CompactFacts`, `UpsertEntity`, `SaveProgress`, `ArchiveProgress`, and the file-rewrite step of `ForgetMemory`), **never** calling `kernel.*`, `taint.*`, `extension.*`, or `SnapshotVault` (`git commit`) inside the lock.
3. **Complete `hq memory forget` Cascade (`Lstat` Symlink-Safe):**
   - Enforces `Lstat` non-symlink checks across every walked file and cascades across `facts.jsonl`, `MEMORY.md`, `topics/{*.md,archived/*.md}`, `entities/{*.md,archived/*.md}`, `notes/**/*.md`, **hot `memory/journal/YYYY-MM-DD/*.json`**, `memory/journal/archive/*.jsonl`, `memory/progress/{*.md,archived/*.md}`, `memory/obs/index.json` (`PromptPreview`), and `memory/rsi/{observations.jsonl,hypotheses/*.json,rules/*.md}`—guaranteeing a forgotten phrase never reappears in `hq memory search` 5 seconds later.

---

## 9. Vault Schema Migration (`v9 -> v10`) & Phased Implementation Plan

### 9.1 Non-Destructive Vault Migration (`CurrentSchemaVersion = 10`)
1. `SnapshotVault(vaultDir, "migrate v9->v10")`.
2. Create `memory/topics/archived/`, `memory/entities/archived/`, `memory/journal/archive/`, and `notes/briefs/archive/` (`0700`).
3. Rewrite `memory/facts.jsonl` atomically at `0600` under `store.RankMemory` (preserving all existing rows; inferring default `Scopes` in-memory when omitted).
4. Strip redundant `## Knowledge Map (Topic Wiki)` from `local/MEMORY.md` when all slugs exist on disk in `local/memory/topics/*.md`, and archive orphaned `memory/progress/<slug>.md` files whose task slugs are already completed/archived.

### 9.2 Phased Implementation & Verification Matrix (Phase 2 — After User Approval)

| Sub-Phase | Target Packages & Files | Key Deliverables & Tests | Est. Scope |
| :--- | :--- | :--- | :---: |
| **Phase 2A: Locked `Fact` v2, Dual-Cap Role-Aware `SessionStart` & `MEMORY.md` Slimming** | `internal/store/store.go`, `internal/vault/{vault,memory,migrate}.go`, `internal/hooks/hooks.go`, `templates/MEMORY.md`, `cmd/hq/main.go` | `store.RankMemory = 78` (`0600` leaf atomic writes, lock-free readers); `Fact` v2 (`Scopes`, `ExpiresAt`, `Pinned`, `<= 240c`); `SelectFactsForSession`; `MEMORY.md` `Active Priorities`-only (`<= 40L / 2.2 KB`); `lean_auto` Zone-1 projection (`0%` Zone-2 starvation); Turn-1 `< 16 KB` `TranscriptPath` + `WorktreeDir` `ResolveSessionScopes`; `[Auto]` fast-path & `cmd/hq` uniform taint gate. | `~380` LOC |
| **Phase 2B: Scoped Topic `## Active Playbook`, Lifecycle Archival & Complete `forget`** | `internal/vault/{memory,progress,entities_and_forget,journal}.go`, `internal/hooks/hooks.go`, `internal/obs/obs.go`, `internal/rsi/{miner,mutator}.go`, `cmd/hq/main.go` | `> **Scopes:**` + `1:1` `> **Playbook-Owner:**` `## Active Playbook` extraction (`<= 25L / 1.2 KB`); `Matched Context Hint` (Task/Subagent only); `vault.WalkJournals` + `14d` journal/brief archival + `HandleStop` archive fallback; `cmd/hq` + `CompactMemory` `progress/` archival (zero import cycle); `--ephemeral` task flag; RSI `occ >= 2` & human-signal filter; `Lstat`-safe full `forget` cascade. | `~400` LOC |
| **Phase 2C: Pure-Go BM25 `hq memory search` & Gatekeeper-Safe `dream-plan`** | `internal/vault/{search,dream}.go`, `cmd/hq/main.go`, `.agents/plugins/agy-hq/skills/sleep-consolidate/SKILL.md`, `templates/triggers.yaml` | Lock-free `< 15ms` Okapi BM25 `hq memory search` (full `private/**` + `outbox/**` + symlink exclusion); `dream-plan --precheck` (`""` idle under `exit_zero_nonempty`) + Stage-1 `--json` + `dream_cursor.json` updated only on `hq memory commit`; updated `sleep-consolidate/SKILL.md` (`<= 150L`, `H1`-clean). | `~360` LOC |
| **Phase 2D: 3-Layer `hq memory eval` (`--replay`, A/B Runner), `hq doctor` & `CUJ-39` E2E** | `internal/vault/eval.go`, `internal/vault/testdata/memory_eval_fixtures.json`, `cmd/hq/main.go`, `internal/e2e/cuj_e2e_test.go`, `docs/product/0003-critical-user-journeys.md` | 4-metric `hq memory eval` (`M1`–`M4`, `--fixtures`, `--replay` v1-vs-v2 historical replay) + offline pre-merge blinded A/B benchmark runner over cloned `/tmp` Vault snapshots; fixed `AuditMemory` integrity checks; `hq doctor` checks; **`TestCUJ39_MemoryV2ScopedTopicsSearchDreamPlanAndEval`** black-box E2E test. | `~400` LOC |

---

## 10. Resolved Design Decisions & Review Gate

1. **D1 — Hard Dual Line + Byte Budgets [Confirmed 2026-11-18, Hardened by Review]:** `12,000 B` (`full` / `front_desk`), `4,800 B` (`lean_auto` with `MEMORY.md` omitted + filtered `Topic Index` to prevent Zone-2 starvation), `MEMORY.md` `Active Priorities`-only (`<= 40L & <= 2,200 B`, stripping both `Knowledge Map` and `Durable Preferences & Facts`).
2. **D2 — Scoped `topics/<slug>.md` `## Active Playbook` Over Separate `playbooks/` Directory [Updated per Zero-Frankenstein Simplification & Carbon High Review]:** Collapse operational repo/domain playbooks into canonical `topics/<slug>.md` (`1:1` `Playbook-Owner` cap per scope, `<= 25L / 1,200 B` total in Zone 2, Ring-3 technical/env facts only), eliminating the redundant 7th rule store while keeping `0` dirty files in external repositories.
3. **D3 — Deterministic Go Physics Over Lexical Hit-Count Scoring [Updated per Carbon High Review]:** Drop `fact_telemetry.json`, `facts_cold.jsonl`, and 5-term lexical hit-count auto-demotion (preventing the Negative Guardrail Paradox); use single-file `facts.jsonl` + deterministic scope/TTL/pinned/recency physics in Go + frontier dreaming for semantic pruning.
4. **D4 — Zero-Token `dream-plan --precheck` Gatekeeper [Confirmed 2026-11-18, Fixed for `gatekeeper.go`]:** Emit `""` on idle under `exit_zero_nonempty`, invoke untruncated `dream-plan --json` in Stage 1, and advance `dream_cursor.json` only on Stage 4 `hq memory commit`.
5. **D5 — Phase 2 Implementation Gate [Pending Final User Review]:** Holding strictly in Phase 1 (`0` code modifications) until your final review sign-off.
