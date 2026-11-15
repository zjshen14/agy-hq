# 15 — Two-Tier Recursive Self-Improvement, OS vs. User-Space Skill Boundary & Personal Vault Skill Overlay Audit

- **Date:** 2026-11-15
- **Author:** `[Task:agy-hq] Two-Tier RSI & Vault Skill Overlay` (`gemini-4-carbon-high`)
- **Status:** Complete / Research Memo (Revision 2)
- **Related Documents:**
  - [RFC-0010: Bounded Agent Recursive Self-Improvement (RSI)](../design/0010-agent-recursive-self-improvement.md)
  - [RFC-0015: Standalone Global Antigravity Plugin Packaging](../design/0015-global-plugin-packaging.md)
  - [RFC-0017d: Companion Extension Packs, Unified Trust Registry & Host Portability](../design/0017d-enterprise-extensibility-and-portability.md)
  - [RFC-0019: OS vs. User-Space Skill Boundary, Vault Skills (`local/skills/`) & Two-Tier RSI Overlays](../design/0019-two-tier-rsi-vault-overlay.md)
  - [04 — Google Antigravity Capability Audit](04_antigravity_audit.md)
  - [07 — Agent Recursive Self-Improvement (RSI): Literature Survey & Safety Invariants](07_agent_recursive_self_improvement.md)

---

## 1. Executive Summary & Two Root Architectural Collisions

### 1.1 Collision 1: The RSI Divergence & Catch-All Attribution Problem
In [RFC-0010](../design/0010-agent-recursive-self-improvement.md), `agy-hq` established a 4-stage bounded Recursive Self-Improvement (RSI) engine (`internal/rsi/`). Production usage across developer checkouts exposed **five concrete failure modes** caused by mutating tracked plugin skills directly:

1. **Dirty Working Tree Friction:** Applying an approved RSI rule via `hq approve` dirties tracked source files (`M .agents/plugins/agy-hq/skills/swe-pulse/SKILL.md`), violating the clean-tree development invariant.
2. **Upgrade Abort & Merge Conflicts (`hq upgrade` / `git pull`):** Upstream skill updates collide with local `<!-- rsi:rule -->` blocks during `git pull`.
3. **PII & Workstation Quirk Leakage:** Local flags, internal project conventions, and personal habits risk leaking into public GitHub PRs.
4. **Forensic Catch-All Bug in `miner.go` & `mutator.go`:** Auditing `internal/rsi/miner.go:162` and `internal/rsi/mutator.go:359-366` revealed that `"swe-pulse"` was hardcoded as the fallback bucket whenever an observation did not match a specific skill. Consequently, `rsi-rule-swe-pulse-2c42ca` (committed on 2026-11-09)—actually a general Front Desk non-blocking lesson—was written into tracked `swe-pulse/SKILL.md`.
5. **Contributor Rule Leakage in `swe-pulse/SKILL.md`:** Step 3 of `swe-pulse/SKILL.md` hardcoded `agy-hq`-maintainer CI rules (`go build -o bin/hq ./cmd/hq && hq ci pre-push`), even though contributor rules already reside in root `AGENTS.md`.

### 1.2 Collision 2: Why Do We Need Harness-Level Skills at All?
Stepping back to first principles, procedural instructions in `agy-hq` can live in three places:
1. **Always-On Rules (`rules/AGENTS.md`):** Paid in prompt tokens on **100% of turns**.
2. **Core OS Harness Skills (`.agents/plugins/agy-hq/skills/`):** Progressive disclosure (~30 tokens of frontmatter; body loaded only when invoked) **and** upgraded atomically alongside the `hq` Go binary via `hq upgrade`.
3. **User-Space Vault Skills (`local/skills/`):** Progressive disclosure, **100% user-owned** inside the git-ignored private Vault (`local/.git`), never overwritten by `hq upgrade`.

Applying both tests (**progressive disclosure** + **lockstep Go binary coupling**) cleanly separates the 9 existing skills into **3 Core OS Harness Skills** and **6 User-Space Domain/Channel Starter Playbooks**:

| Skill | Calls Internal `hq` Kernel Protocols? | Needed by Every Persona (e.g. Living-Expense User)? | Proper Architectural Layer |
| :--- | :--- | :--- | :--- |
| `inbox-triage` | Yes (`hq watch read`, `hq offer stage`, `hq outbox add` taint boundary) | Yes | **Core OS Harness** (`.agents/plugins/agy-hq/skills/`) |
| `morning-brief` | Yes (`hq brief collect --lease`, `hq brief commit`) | Yes | **Core OS Harness** (`.agents/plugins/agy-hq/skills/`) |
| `sleep-consolidate` | Yes (`hq obs`, `hq memory verify-transcript`, `hq rsi`, `hq tasks`) | Yes | **Core OS Harness** (`.agents/plugins/agy-hq/skills/`) |
| `gchat` | Optional channel driver (`hq gchat` headless CDP); auto-seeded by `hq gchat login` | No (Channel opt-in) | **User-Space Starter** (`templates/skills/` → `local/skills/`) |
| `swe-pulse` | Only `hq worktree`/`hq spawn` (already in `rules/AGENTS.md`); assumes GitHub `gh` CLI | No (SWE-only) | **User-Space Starter** (`templates/skills/` → `local/skills/`) |
| `ledger-ingest` | Zero `hq` commands; assumes Beancount/`hledger` | Opt-in Finance/Expense | **User-Space Starter** (`templates/skills/` → `local/skills/`) |
| `tax-prep` | Zero `hq` commands; assumes US `W-2`/`1099`/`K-1` | Opt-in US Tax | **User-Space Starter** (`templates/skills/` → `local/skills/`) |
| `thesis-check` | Generic `hq outbox add` only; personal equity/secondary workflow | Opt-in Investing | **User-Space Starter** (`templates/skills/` → `local/skills/`) |
| `filing-diff` | Zero `hq` commands; SEC `10-K`/`10-Q` diffing | Opt-in Investing | **User-Space Starter** (`templates/skills/` → `local/skills/`) |

---

## 2. Antigravity Skill Discovery & Why `.agents/skills.json` Unlocks `local/skills/`

### 2.1 Native Discovery via `.agents/skills.json`
Per Antigravity's customization loader (`agy-customizations/docs/json_configs.md`), a workspace `.agents/skills.json` manifest registers external or git-ignored directories for native one-level-deep skill discovery:
```json
{
  "entries": [
    { "path": "local/skills" }
  ]
}
```
This solves the user-authored skill problem that `~/.gemini/config/skills/` and `.agents/skills/` could not:
- Putting personal skills in `headquarter/.agents/skills/` dirties the public git repository.
- Putting personal skills in `~/.gemini/config/skills/` strands them outside the private `local/.git` Vault backup and injects them into unrelated repositories on the workstation.
- Putting domain starter skills (`hq skill add ledger-ingest`) and custom user skills (`local/skills/living-budget/SKILL.md`) in **`local/skills/`** gives native `/<name>` slash-command discovery, full `scripts/` support, **`0` dirty files** in `headquarter`, private `local/.git` versioning across machines, and **direct in-place evolution by `hq rsi`**!

### 2.2 Why Core OS Skills Still Use Inline `hq dispatch resolve` Overlays
For the **3 Core OS skills** (`inbox-triage`, `morning-brief`, `sleep-consolidate`) that remain in `.agents/plugins/agy-hq/skills/` (Priority #1 in Antigravity's loader, higher than Priority #2 `.agents/skills.json`), wholesale shadowing in `local/skills/` would freeze the skill and miss future `hq upgrade` CLI flag updates.

Instead, extending `hq dispatch resolve --skill=<name> --json` to return `has_overlay`, `overlay_path`, and **inline `overlay_rules`** from `<vault>/memory/rsi/rules/<name>.md` composes personal operational tweaks in **one `0`-cap `hq` call** during Step 0—zero extra `read_file` round-trips, zero Front Desk lookup-cap burn, and zero merge conflicts on `hq upgrade`.

---

## 3. Summary of the Three Learning & Customization Substrates

| Substrate | Storage in Private Vault / Repo | Managed By | Git Footprint in `headquarter` |
| :--- | :--- | :--- | :--- |
| **1. Declarative & Episodic Memory** | `<vault>/MEMORY.md`, `memory/topics/`, `memory/entities/`, `facts.jsonl` | Nightly `/sleep-consolidate` (`hq memory`) | `0` dirty files (committed to `local/.git`) |
| **2A. User-Space Vault Skills** *(Starters + Custom)* | `<vault>/skills/<name>/SKILL.md` (+ `scripts/`) | User authoring, `hq skill add`, & in-place Tier 1A `hq rsi` | `0` dirty files (committed to `local/.git`) |
| **2B. Core OS Skill & General Overlays** | `<vault>/memory/rsi/rules/<core-skill\|general>.md` | Tier 1B `hq rsi` + inline `hq dispatch resolve` | `0` dirty files (committed to `local/.git`) |
| **3. Upstream Harness Evolution** | `.agents/plugins/agy-hq/skills/` (3 OS skills), `internal/*` | Tier 2 `hq rsi --upstream` in isolated worktree PR | Clean feature branch `hq/rsi-<id>` |
