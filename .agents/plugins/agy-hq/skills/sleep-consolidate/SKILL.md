---
name: sleep-consolidate
description: >-
  Nightly sleep-time memory consolidation ("dreaming") workflow. Audits Vault memory
  health via hq memory dream-plan, verifies turn journal claims against ground-truth
  transcripts, distills scoped lessons and Active Playbooks into local/memory/topics/*.md,
  keeps local/MEMORY.md as a compact Active Priorities index, prunes stale facts, runs
  verified RSI, and commits the private local/ Vault git repo.
---

# Nightly Dreaming & Sleep-Time Memory Consolidation (`/sleep-consolidate`)

> **Architectural Purpose (RFC-0006, RFC-0014, & RFC-0024):** Do not treat `local/MEMORY.md` as an append-only dump or blindly trust self-reported turn summaries. Start with the 0-token `hq memory dream-plan --json` delta blueprint, verify salient claims against ground-truth transcripts, keep `local/MEMORY.md` strictly to **Active Priorities** ($\le 40$ lines & $\le 2,200$ bytes), store scoped durable facts with optional `--scope`/`--expires` TTLs in `facts.jsonl`, distill deep technical/domain knowledge and concise `## Active Playbook` sections ($\le 25$ lines / $1,200$ bytes, objective technical/env traps only) into `local/memory/topics/<slug>.md`, and maintain on-demand Entity Dossiers (`local/memory/entities/<slug>.md`) at 0 `SessionStart` token cost. Default execution tier is **`deep-medium`** (`--tier=deep-medium`).

> [!IMPORTANT]
> **Silent Execution & Memory Operations Invariant:** Automated nightly sleep consolidation (`sleep-consolidate` / `[Auto] Nightly Sleep Consolidation`) runs **100% silently in the background**. Never emit chat messages, checklists, progress updates, or completion banners into the Front Desk conversation. All audits, transcript checks, pruned facts, topic/entity updates, and RSI proposals remain strictly inside the private Vault (`local/`). Never mention internal bookkeeping or file paths in conversational replies. Critical health issues or pending approvals roll forward into the 7:30 AM Morning Brief (`/morning-brief`).

## Four-Stage Dreaming Workflow

0. **Dispatch & Overlay Guard:**
   - Run `hq dispatch resolve --skill=sleep-consolidate --trigger=core/sleep-consolidate --json`: if `enabled == false`, stop cleanly with zero output; if `has_overlay == true` with non-empty `overlay_rules`, apply those active personal rules for this run.

### Stage 1: Deterministic Delta Blueprint & Subagent Reconciliation
1. Run `hq obs reconcile` and `hq memory dream-plan --json` (never re-scan already-consolidated dates or ephemeral/tainted turns):
   - If `hasDelta == false`, run `hq memory compact` if needed and stop cleanly with zero chat output.
   - Review `salientConversations`, `overdueTTLFactCount`, `missingTTLEventFacts`, `memoryLines`/`memoryBytes`, `staleReferences`, `wikiAlerts` (`invalid_header`, `broken_link`, `duplicate_playbook_owner`, `near_duplicate_topic`), and `negativeConstraints` (recent rejected proposals — **never re-propose or codify rejected actions**).

### Stage 2: Ground-Truth Transcript Verification
1. For each high-salience conversation in `dream-plan` showing `toolErrors > 0`, `preToolDenials > 0`, a user correction/intervention, or a major architectural/completion claim, run:
   ```sh
   hq memory verify-transcript <conversationId>
   ```
   Cross-check the 0-token ground-truth digest (`errors`, `discrepancyWarnings`, `userInputs`, `filesModified`, `commandsExecuted`) against claimed outcomes before promoting any lesson into long-term memory.

### Stage 3: Orthogonal Vault Consolidation (Facts, Topics/Playbooks, Entities, Priorities)
1. **Prune Stale Facts & Enforce TTLs:**
   - Soft-expire superseded, contradicted, or completed facts via `hq memory prune-fact <fact-id> [--superseded-by=<new-id>]` (and check off finished goal milestones via `hq goals list`).
   - Add verified atomic facts via `hq memory add-fact --topic="<topic>" --statement="<concise fact <= 240 runes>" --scope="<global|repo:<slug>|domain:<slug>>" [--expires="<YYYY-MM-DD|+14d>"] [--pinned] --source="<convId>" --verified`. Always set `--expires` on dated/event facts.
2. **Distill Scoped Knowledge & `## Active Playbook` into Topic Wiki (`local/memory/topics/<slug>.md`):**
   - Begin every topic page with `# <Title>`, `> **Summary:** <1-line description>`, optional `> **Scopes:** repo:<slug>, domain:<slug>`, and optional 1:1 `> **Playbook-Owner:** repo:<slug>` (never duplicate the same `Playbook-Owner` across two files).
   - Keep `## Active Playbook` strictly $\le 25$ lines / $1,200$ bytes and restricted to objective build, test, architecture, or environment traps (Ring-3 technical state — never behavioral overrides, approval bypasses, or rejected actions). Fix any `wikiAlerts` reported in Stage 1.
3. **Reconcile On-Demand Entity Dossiers (`local/memory/entities/<slug>.md`) & Private Household Profile (`local/private/profile.md`):**
   - Upsert verified collaborator/counterparty roles, emails, handles, spaces, or working-style notes via `hq memory add-entity --name="..." --kind="person" --email="..." --space="..." --context="..." --note="..."` (overflow beyond 10 bullets auto-rolls to `entities/archived/`).
   - Backfill verified household autofill details (phone, home address, legal names, travel KTN/loyalty IDs, emergency contacts — never passwords/SSNs/cards) into `hq memory profile set --section=<self|addresses|household|travel|emergency> --key=<k> --value="<v>"` without duplicating messaging handles/Chat spaces from `entities/`.
4. **Keep `local/MEMORY.md` Strictly to `## Active Priorities` ($\le 40$ lines & $\le 2,200$ bytes):**
   - Remove completed tasks, deleted worktrees, and redundant fact/topic duplicates from `local/MEMORY.md`.

### Stage 4: Verified RSI (`hq rsi`), Lifecycle Compaction & Vault Commit
1. **Run the Two-Tier 4-Stage RSI Pipeline (RFC-0010, RFC-0014, & RFC-0019 — Never Unilaterally Edit `SKILL.md`, Overlay Rules, or Kernel Code):**
   - Mine ground-truth friction events (including `dismiss_noisy` interrupt rates $\ge 40\%$ and high expired-proposal rates targeting Vault-local rules in `local/memory/rsi/rules/`) and update canary records:
     ```sh
     hq rsi mine --since=24h
     ```
   - Synthesize Tier 1 personal hypotheses targeting `<vault>/skills/<slug>/SKILL.md` or `<vault>/memory/rsi/rules/<slug>.md` (never `topics/`), then run the 4-Gate Verification Harness:
     ```sh
     hq rsi hypothesize && hq rsi verify <rsi-id> && hq rsi propose <rsi-id>
     ```
2. **Unified Compaction, Archival & Evaluation Check:**
   - Run `hq memory compact` (compacts `facts.jsonl`, `tasks.json`, sweeps orphan `progress/*.md`, and archives $>14\text{d}$ hot journals/briefs) and `hq memory status` to confirm `MEMORY.md` is `[OK]` ($\le 40$ lines / $\le 2,200$ bytes).
3. **Commit Vault & Advance Dream Cursor:**
   - Commit private Vault changes (automatically advances `dream_cursor.json` strictly on commit):
     ```sh
     hq memory commit --message="chore(memory): nightly dreaming consolidation YYYY-MM-DD"
     ```
   - **Silent Turn Finish:** End the turn cleanly without emitting chat responses, status banners, or completion summaries.
