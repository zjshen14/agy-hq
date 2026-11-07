# PRD-0002: Open-Ended Workflows & Additive Skills

- **Status:** Approved
- **Related Docs:** [ADR-0006](../decisions/0006-general-agent-and-skills-over-packs.md), [RFC-0001](../design/0001-vault-and-task-isolation.md), [RFC-0003](../design/0003-approval-bound-execution.md)

---

## 1. Core Principle: Zero Artificial Domain Walls

A personal assistant must never refuse or stumble on a new kind of request—such as *"Write a blog post for me"*, *"Compare two flight itineraries"*, or *"Help me debug this network issue"*—just because that category wasn't pre-registered.

Therefore, `agy-hq` operates on a two-tier capability model:
1. **Default Tier — 100% General-Purpose Agent:** Every conversation has full access to Antigravity's general reasoning, web search, browser automation, terminal sandbox, your `local/USER.md` profile, `local/MEMORY.md`, and your open `local/workspace/` folder.
2. **Booster Tier — Additive Skills (`.agents/skills/*/SKILL.md`):** When a request (or a scheduled/inbox trigger in `local/triggers.yaml`) matches a known workflow, Antigravity progressively loads that `SKILL.md` runbook to apply domain-specific verification rules and templates.

---

## 2. How New Capabilities Emerge Organically

When you ask the agent to do something brand new (e.g., *"Write a technical blog post in my voice and publish a draft"*):
1. **First Time (Zero Setup):** The agent executes the request immediately in `local/workspace/` using your `local/USER.md` preferences and general tools.
2. **Feedback & Capture:** If you give it feedback (*"Use shorter paragraphs, include a Mermaid diagram, and save drafts to `local/workspace/posts/`"*), `hq hook stop` captures the turn in `local/memory/journal/`.
3. **Nightly Promotion to a Skill:** During the 23:30 `sleep-consolidate` run (or on demand via `/learn`), the agent proposes a new `.agents/skills/write-post/SKILL.md` in `local/memory/proposals/` so future posts follow your exact workflow automatically.

---

## 3. Built-In Starter Skills (`.agents/skills/`)

While the agent can do anything out of the box, `agy-hq` ships with seven curated starter skills for high-frequency personal workflows:

| Skill (`.agents/skills/<name>/`) | What It Automates | Built-In Verification & Guardrails |
|---|---|---|
| **`morning-brief`** | Synthesizes today's calendar, quarantined inbox triage, repo CI status, and watchlist alerts at 07:30 AM. | Read-only aggregation; stages any drafted actions via `hq propose`. |
| **`sleep-consolidate`** | Runs at 23:30 PM to distill `local/memory/journal/YYYY-MM-DD/*.json` into delta patches on `local/MEMORY.md` and `facts.jsonl`. | Never overwrites `MEMORY.md` wholesale; stages new skill proposals in `local/memory/proposals/`. |
| **`repo-pulse`** | Checks registered git repositories (`gh pr list`, `gh run list`) for failing CI or stalled PRs, and builds features/fixes inside isolated `git worktree` checkouts. | Read-only explorer subagents + 1 writer per worktree; `git push` and `gh pr merge` require `hq propose` approval. |
| **`thesis-check`** | Maintains per-ticker `thesis.md` (*Core Thesis, Bull/Bear Case, Explicit Falsification Criteria*) and tests new SEC 10-K/10-Q/8-K filings via parallel Bull vs. Bear subagents. | Every financial number must cite `[Filing, Page/Section]` and pass script checks; zero brokerage trading tools. |
| **`inbox-triage`** | Summarizes unread emails into `local/workspace/inbox_digest.md` and stages draft replies. | Runs as a quarantined reader (zero direct outbound send permissions); sending requires `hq propose` approval. |
| **`ledger-reconcile`** | Triggered when a bank CSV/OFX drops into `local/inbox/`; categorizes entries into `local/private/ledger/2026.beancount`. | Enforces double-entry balance ($0 residual) via the deterministic `bean-check` CLI before staging the git diff. |
| **`tax-prep`** | Tracks `local/private/taxes/2026/checklist.md` (W-2s, 1099s, estimated deadlines) and extracts fields from PDFs dropped into `local/inbox/`. | Uses deterministic scripts for tax-bracket estimates (never bare LLM arithmetic); keeps `local/private/` isolated from untrusted web sessions. |
