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

> **Architectural Purpose (RFC-0006 & RFC-0014):** Do not treat `local/MEMORY.md` as an append-only dump or blindly trust self-reported 1-line turn summaries. Verify what actually happened against ground-truth transcripts, prune obsolete tasks and contradicted assumptions, distill deep technical/domain knowledge into Topic Wiki pages (`local/memory/topics/<slug>.md`), maintain on-demand Entity Dossiers (`local/memory/entities/<slug>.md`) at 0 `SessionStart` token cost, and keep `local/MEMORY.md` strictly as a concise Roadmap Index ($\le 80$ lines).

> [!IMPORTANT]
> **Silent Execution & Memory Operations Invariant:** Automated nightly sleep consolidation (`sleep-consolidate` / `[Auto] Nightly Sleep Consolidation`) runs **100% silently in the background**. Never emit chat messages, checklists, progress updates, or completion banners into the Front Desk conversation. All background audits, transcript verification findings, pruned facts, topic/entity updates, and RSI proposals remain strictly confined to the private Vault (`local/`), ledger, and turn journals. Never mention internal bookkeeping, ledger writes, or file paths (e.g., `project-notes.md`, `facts.jsonl`, `MEMORY.md`, `tasks.json`) in conversational replies. Any critical health issues, distilled insights, or pending approvals roll forward into the next morning's 7:30 AM Morning Brief (`/morning-brief`).

## Four-Stage Dreaming Workflow

### Stage 1: Baseline Memory Audit, Subagent DAG Reconciliation & Stale Reference Check
1. Run `./bin/hq obs reconcile` and `./bin/hq memory status` to inspect:
   - Ground-truth parent-child subagent attribution in `local/memory/tasks.json` and 24h token/context anomalies (`./bin/hq obs tokens --since=24h`).
   - Current line and token usage of `local/MEMORY.md` against the **80-line Roadmap Index cap**.
   - Existing Knowledge Wiki pages (`local/memory/topics/*.md`), Entity Dossiers (`local/memory/entities/*.md`), active vs. expired bi-temporal facts, and any **Stale References**.

### Stage 2: Episodic Sweep & Ground-Truth Transcript Verification
1. Inspect today's turn journals in `local/memory/journal/YYYY-MM-DD/*.json` and recent entries in `local/memory/interventions.jsonl`.
2. **Ground-Truth Verification Rule:** Whenever a journal entry shows `toolErrorCount > 0`, a user correction/intervention, or an architectural/root-cause/completion claim, run:
   ```sh
   ./bin/hq memory verify-transcript <conversationId>
   ```
   Cross-check the 0-token ground-truth digest (`errors`, `discrepancyWarnings`, `userInputs`, `filesModified`, `commandsExecuted`) against what the agent claimed before promoting any lesson into long-term memory.

### Stage 3: Proactive Pruning, Topic Wiki, Entity Dossiers & Roadmap Index Update
1. **Prune Stale Facts & Completed Priorities:**
   - Soft-expire superseded, contradicted, or completed one-off facts via `./bin/hq memory prune-fact <fact-id> [--superseded-by=<new-id>]` (and check off finished goal milestones via `./bin/hq goals list`).
   - Remove completed one-off tasks or deleted worktrees from `## Active Priorities` in `local/MEMORY.md`.
2. **Distill Detailed Knowledge into Topic Wiki Pages (`local/memory/topics/<slug>.md`):**
   - Put multi-paragraph architectural notes, subsystem invariants, debugging lessons, or domain playbooks into `local/memory/topics/<slug>.md` (beginning with `# <Topic Title>` and `> **Summary:** <1-line description>`).
3. **Reconcile On-Demand Entity & Counterparty Dossiers (`local/memory/entities/<slug>.md`):**
   - When verified turns reveal durable collaborator/counterparty roles, handles, spaces, or working-style preferences, upsert them via:
     ```sh
     ./bin/hq memory add-entity --name="<Name>" --kind="person" --email="..." --space="..." --context="..." --note="..."
     ```
   - Entity dossiers carry **0 bytes of `SessionStart` token overhead** and are retrieved on-demand via `./bin/hq memory lookup <query>`.
4. **Update `local/MEMORY.md` ($\le 80$ lines) & Record Verified Atomic Facts:**
   - Apply incremental delta edits to `local/MEMORY.md` (`## Active Priorities`, `## Durable Preferences & Facts`, `## Knowledge Map (Topic Wiki)`).
   - Add verified atomic facts via `./bin/hq memory add-fact --topic="<topic>" --statement="<concise fact>" --source="<convId>" --verified`.

### Stage 4: Verified Recursive Self-Improvement (`./bin/hq rsi`) & Private Vault Commit
1. **Run the 4-Stage RSI Pipeline (RFC-0010 & RFC-0014 — Never Unilaterally Edit `SKILL.md` or Kernel Code):**
   - Mine 0-token ground-truth friction events (error-recovery chains, `PreToolUse` denials, user interventions, `PRELUDE` draft edit-diffs, and `proactive_noise` idea dismissal rates $>70\%$) and update active canary records:
     ```sh
     ./bin/hq rsi mine --since=24h
     ```
   - Synthesize structured ACE delta hypotheses (`./bin/hq rsi hypothesize`) and run the 4-Gate Sandboxed Verification Harness + stage verified passes:
     ```sh
     ./bin/hq rsi verify <rsi-id> && ./bin/hq rsi propose <rsi-id>
     ```
   - When upgrading to a new frontier model, run `./bin/hq rsi ablate --model=<new-model>`.
2. **Task Lifecycle Consolidation & Hot/Cold Archive Rotation:**
   - Run `./bin/hq tasks compact` (and `./bin/hq archive <slug>` if explicit worktree cleanup is needed).
3. **Verify Budget, Commit Vault & Silent Completion:**
   - Run `./bin/hq memory compact`, `./bin/hq memory status`, and `./bin/hq rsi status` to confirm `local/MEMORY.md` is $\le 80$ lines (`[OK]`).
   - Commit changes to the private Vault repository:
     ```sh
     git -C local add -A && git -C local commit -m "chore(memory): nightly dreaming consolidation YYYY-MM-DD"
     ```
   - **Silent Turn Finish:** End the turn cleanly without emitting chat responses, status banners, or completion summaries into the Front Desk thread.
