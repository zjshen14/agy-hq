---
name: sleep-consolidate
description: >-
  Nightly sleep-time memory consolidation workflow. Reviews the day's immutable
  turn journals in local/memory/journal/, applies incremental delta updates to
  local/MEMORY.md and local/memory/facts.jsonl, stages skill improvement proposals,
  and commits the private local/ Vault git repo.
---

# Sleep-Time Memory Consolidation (`/sleep-consolidate`)

Consolidate raw daily turn journals into durable, structured long-term memory without context collapse.

## Steps

1. **Read Today's Turn Journals & Interventions:**
   - Inspect all JSON files in `local/memory/journal/YYYY-MM-DD/*.json` and recent entries in `local/memory/interventions.jsonl`.
2. **Apply Incremental Delta Updates (Never Wholesale Rewrite):**
   - Update `local/MEMORY.md` with new durable priorities or preferences learned today, keeping total size under 2,500 tokens.
   - Append new bi-temporal facts to `local/memory/facts.jsonl` (`id`, `topic`, `statement`, `valid_from`, `valid_to: null`). If an existing fact was superseded, set its `valid_to` timestamp rather than deleting history.
3. **Stage Skill Proposals (Do Not Auto-Edit `SKILL.md`):**
   - If a new repeated workflow or user correction occurred today, write a proposed skill diff or new draft skill to `local/memory/proposals/YYYY-MM-DD-<slug>.md` for user review.
4. **Commit the Private Vault:**
   - Run `git -C local add -A && git -C local commit -m "chore(memory): nightly consolidation YYYY-MM-DD"` (if there are changes).
