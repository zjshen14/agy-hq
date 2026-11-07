# ADR-0006: Use One General-Purpose Agent + Additive Skills Over Rigid Domain Packs

- **Status:** Accepted
- **Date:** 2026-11-07
- **References:** [PRD-0002](../product/0002-open-workflows-and-skills.md), [RFC-0001](../design/0001-vault-and-task-isolation.md)

---

## Context

In earlier drafts, we considered organizing capabilities into rigid "Domain Packs" (`packs/swe`, `packs/invest`, `packs/admin`), where each session was assigned to a pack and restricted by `PreToolUse` to that pack's directory.

However, a personal assistant must handle arbitrary, open-ended requests on the fly (e.g., *"Write a blog post for me"*, *"Plan a weekend itinerary"*, or tasks that span multiple domains at once). Requiring a pre-registered Pack for a task to succeed—or jailing each conversation inside a single domain directory—artificially restricts a general-purpose agent.

## Decision

1. **Drop rigid runtime "Packs":** The assistant is **one unified, general-purpose agent** that can handle any open-ended task in `local/workspace/` out of the box.
2. **Use native Antigravity `Skills` (`.agents/skills/*/SKILL.md`) as purely additive boosters:** Skills provide progressive-disclosure runbooks and verification rules when a task matches them, and new skills can be proposed organically from repeated workflows during nightly consolidation.
3. **Isolate at the Task (`conversationId`) and Sensitivity (`local/private/`) levels:**
   - Parallel tasks are isolated from each other via immutable per-conversation files (`<convId>.json`) and git worktrees—so *any* combination of tasks can run concurrently.
   - Security is enforced by **Action Tier** (`hq propose` for irreversible external actions) and a dedicated **`local/private/`** boundary for high-PII files (taxes, bank ledgers), leaving `local/workspace/` unrestricted for general work.

## Consequences

- **Positive:** Zero artificial walls when asking the agent to do something new; ~40% simpler Go microkernel (no Pack Registry or per-pack folder jail); parallel execution works even when running multiple tasks in the same domain.
- **Negative:** None—skills remain modular and shareable as standard `.agents/skills/<name>/SKILL.md` directories.
