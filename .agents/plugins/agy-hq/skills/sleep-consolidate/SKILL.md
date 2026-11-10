---
name: sleep-consolidate
description: >-
  Nightly sleep-time memory consolidation ("dreaming") workflow. Audits Vault memory
  health, verifies turn journal claims against ground-truth transcripts, distills deep
  lessons into local/memory/topics/*.md, keeps local/MEMORY.md as a compact Roadmap
  Index, prunes stale facts via ./bin/hq memory, stages skill proposals, and commits
  the private local/ Vault git repo.
---

# Nightly Dreaming & Sleep-Time Memory Consolidation (`/sleep-consolidate`)

> **Architectural Purpose (RFC-0006):** Do not treat `local/MEMORY.md` as an append-only dump or blindly trust self-reported 1-line turn summaries. Your role during nightly dreaming is to **verify what actually happened against ground-truth transcripts**, **prune obsolete tasks and contradicted assumptions**, **distill deep technical/domain knowledge into Topic Wiki pages (`local/memory/topics/<slug>.md`)**, and **maintain `local/MEMORY.md` strictly as a concise Roadmap Index ($\le 80$ lines)** loaded at `SessionStart`.

> [!IMPORTANT]
> **Silent Execution & Memory Operations Invariant:** Automated nightly sleep consolidation (`sleep-consolidate` / `[Auto] Nightly Sleep Consolidation`) runs **100% silently in the background**. Never emit chat messages, checklists, progress updates, or completion banners into the Front Desk conversation. All background audits, transcript verification findings, pruned facts, topic wiki updates, and RSI proposals remain strictly confined to the private Vault (`local/`), ledger, and turn journals. Never mention internal bookkeeping, ledger writes, or file paths (e.g., `project-notes.md`, `facts.jsonl`, `MEMORY.md`, `tasks.json`) in conversational replies; all memory updates occur silently. Any critical health issues, distilled insights, or pending approvals roll forward into the next morning's 7:30 AM Morning Brief (`/morning-brief`).

## Four-Stage Dreaming Workflow

### Stage 1: Baseline Memory Audit, Subagent DAG Reconciliation & Stale Reference Check
1. Run `./bin/hq obs reconcile` and `./bin/hq memory status` to inspect:
   - Ground-truth parent-child subagent attribution in `local/memory/tasks.json` and 24h token/context anomalies (`./bin/hq obs tokens --since=24h`).
   - Current line and token usage of `local/MEMORY.md` against the **80-line Roadmap Index cap**.
   - Existing Knowledge Wiki pages in `local/memory/topics/*.md` and any unlinked topic slugs.
   - Active vs. expired bi-temporal facts in `local/memory/facts.jsonl`.
   - Any **Stale References** (completed or cancelled tasks from `local/memory/tasks.json` still referenced in `MEMORY.md` or active facts).

### Stage 2: Episodic Sweep & Ground-Truth Transcript Verification
1. Inspect today's turn journals in `local/memory/journal/YYYY-MM-DD/*.json` and recent entries in `local/memory/interventions.jsonl`.
2. **Ground-Truth Verification Rule:** Self-reported turn summaries (`responseSummary`) can be overly optimistic or incomplete. Whenever a journal entry shows:
   - `toolErrorCount > 0`,
   - a user correction or intervention,
   - an architectural decision, bug root-cause claim, or task completion,
   run:
   ```sh
   ./bin/hq memory verify-transcript <conversationId>
   ```
   Cross-check the 0-token ground-truth digest (`errors`, `discrepancyWarnings`, `userInputs`, `filesModified`, `commandsExecuted`) against what the agent claimed before promoting any lesson into long-term memory.

### Stage 3: Proactive Pruning, Topic Wiki Distillation & Roadmap Index Update
1. **Prune Stale Facts & Completed Priorities:**
   - For any active fact in `local/memory/facts.jsonl` that was superseded, contradicted by ground-truth transcripts, or tied to a finished one-off task/worktree, soft-expire it deterministically:
     ```sh
     ./bin/hq memory prune-fact <fact-id> [--superseded-by=<new-id>]
     ```
   - Remove completed one-off tasks or deleted worktrees from `## Active Priorities` in `local/MEMORY.md`.
2. **Distill Detailed Knowledge into Topic Wiki Pages (`local/memory/topics/<slug>.md`):**
   - Put multi-paragraph architectural notes, subsystem invariants, debugging lessons, or domain playbooks into `local/memory/topics/<slug>.md` (or `local/notes/<domain>/`).
   - Every file in `local/memory/topics/<slug>.md` MUST begin with `# <Topic Title>` and `> **Summary:** <1-line description>` so `hq hook session-start` automatically indexes it on Turn 1:
     ```markdown
     # <Topic Title>
     > **Summary:** One-sentence summary of what this topic page covers and when to read it.
     ```
3. **Update `local/MEMORY.md` as a Compact Roadmap Index ($\le 80$ lines):**
   - Apply incremental delta edits (never wholesale rewrite) to `local/MEMORY.md`, keeping only:
     - `## Active Priorities` (current ongoing initiatives)
     - `## Durable Preferences & Facts` (cross-cutting user invariants)
     - `## Knowledge Map (Topic Wiki)` (1-line pointers to `local/memory/topics/<slug>.md` and `local/notes/`)
4. **Record Verified Atomic Facts:**
   - Add new durable bi-temporal facts with provenance via:
     ```sh
     ./bin/hq memory add-fact --topic="<topic>" --statement="<concise fact>" --source="<convId>" --verified
     ```

### Stage 4: Verified Recursive Self-Improvement (`./bin/hq rsi`) & Private Vault Commit
1. **Run the 4-Stage RSI Pipeline (RFC-0010 — Never Unilaterally Edit `SKILL.md` or Kernel Code):**
   - Mine 0-token ground-truth friction events (error-recovery chains, `PreToolUse` denials, user interventions, and `PRELUDE` draft edit-diffs) and update active canary records:
     ```sh
     ./bin/hq rsi mine --since=24h
     ```
   - Synthesize structured ACE delta hypotheses (`add_skill_rule`, `patch_skill_rule`, `deprecate_skill_rule`, or LATM Tier B$\to$C promotions) for observations passing the Meta-ROI gate and 75% Quota Circuit Breaker:
     ```sh
     ./bin/hq rsi hypothesize
     ```
   - For each generated hypothesis `<rsi-id>`, run the 4-Gate Sandboxed Verification Harness (Gate 1 TCB/AST anti-Goodharting, Gate 2A/2B counterfactual reproduction, Gate 3 regression & PII guard, Gate 4 complexity/budget & contradiction guard) and stage verified passes into the live approval queue:
     ```sh
     ./bin/hq rsi verify <rsi-id> && ./bin/hq rsi propose <rsi-id>
     ```
   - When the workspace upgrades to a new frontier model, run `./bin/hq rsi ablate --model=<new-model>` to stage deprecation of obsolete model-scoped rules.
2. **Task Lifecycle Consolidation & Hot/Cold Archive Rotation (`./bin/hq tasks compact`):**
   - Run `./bin/hq tasks compact` to automatically archive eligible completed (`>24h`) and dormant (`>48h`) tasks with clean worktrees and rotate historical subagents and archived tasks from `local/memory/tasks.json` into `local/memory/tasks_archive.jsonl`.
   - For any remaining `completed` task that needs explicit worktree cleanup, run `./bin/hq archive <slug>`.
3. **Verify Budget, Commit Vault & Silent Completion:**
   - Run `./bin/hq memory compact`, `./bin/hq memory status`, and `./bin/hq rsi status` to confirm `local/MEMORY.md` is $\le 80$ lines (`[OK]`), `facts.jsonl` is deduplicated, and `tasks.json` is compacted.
   - Commit changes to the private Vault repository:
     ```sh
     git -C local add -A && git -C local commit -m "chore(memory): nightly dreaming consolidation YYYY-MM-DD"
     ```
   - **Silent Turn Finish:** End the turn cleanly without emitting chat responses, status banners, or completion summaries into the Front Desk thread.
