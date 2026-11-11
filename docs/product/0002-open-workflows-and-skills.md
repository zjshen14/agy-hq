# PRD-0002: Open-Ended Workflows & Additive Skills

- **Status:** Approved
- **Related Docs:** [ADR-0006](../decisions/0006-general-agent-and-skills-over-packs.md), [RFC-0001](../design/0001-vault-and-task-isolation.md), [RFC-0003](../design/0003-approval-bound-execution.md)

---

## 1. Core Principle: Zero Artificial Domain Walls

A personal assistant must never refuse or stumble on a new kind of request—such as *"Write a blog post for me"*, *"Compare two flight itineraries"*, or *"Help me debug this network issue"*—just because that category wasn't pre-registered.

Therefore, `agy-hq` operates on a two-tier capability model:
1. **Default Tier — 100% General-Purpose Agent:** Every conversation has full access to Antigravity's general reasoning, web search, browser automation, terminal sandbox, your `local/USER.md` profile, `local/MEMORY.md`, and your open `local/notes/` and `local/worktrees/` folders.
2. **Booster Tier — Additive Skills ([`.agents/plugins/agy-hq/skills/*/SKILL.md`](../../.agents/plugins/agy-hq/skills/morning-brief/SKILL.md)):** When a request (or a scheduled/inbox trigger in `local/triggers.yaml`) matches a known workflow, Antigravity progressively loads that `SKILL.md` runbook (`<= 150` lines) to apply domain-specific verification rules and templates.

---

## 2. How New Capabilities Emerge Organically

When you ask the agent to do something brand new (e.g., *"Write a technical blog post in my voice and stage a draft"*):
1. **First Time (Zero Setup):** The agent executes the request immediately in `local/notes/` using your `local/USER.md` preferences and general tools.
2. **Feedback & Capture:** If you give it feedback (*"Use shorter paragraphs, include a Mermaid diagram, and save drafts to `local/notes/posts/`"*), `hq hook stop` captures the turn in `local/memory/journal/`.
3. **Nightly Promotion to a Skill:** During the 23:30 `sleep-consolidate` run (or via `./bin/hq rsi`), the agent distills recurring workflows or friction into verified `SKILL.md` deltas staged via `hq propose` for 1-tap approval.

---

## 3. Built-In Starter Skills (`.agents/plugins/agy-hq/skills/`)

While the agent can handle any request out of the box, `agy-hq` ships with **nine curated starter skills** for high-frequency personal workflows:

| Skill (`.agents/plugins/agy-hq/skills/<name>/`) | What It Automates | Built-In Verification & Guardrails |
| :--- | :--- | :--- |
| **[`morning-brief`](../../.agents/plugins/agy-hq/skills/morning-brief/SKILL.md)** | Synthesizes today's calendar, quarantined inbox triage, active worktrees/PRs, and watchlist alerts at 07:30 AM. | Read-only aggregation; stages any drafted replies via `hq propose`. |
| **[`sleep-consolidate`](../../.agents/plugins/agy-hq/skills/sleep-consolidate/SKILL.md)** | Runs silently at 23:30 PM to verify turn journals against raw transcripts, update Topic Wiki pages, compact `MEMORY.md`, and commit `local/.git`. | Zero Front Desk chat output; enforces `hq memory verify-transcript` before promoting facts. |
| **[`swe-pulse`](../../.agents/plugins/agy-hq/skills/swe-pulse/SKILL.md)** | Orchestrates multi-repo software engineering inside isolated `local/worktrees/<repo>-<task>/` checkouts and checks PR/CI status. | Read-only explorers + isolated per-task worktrees; third-party PR merges and external pushes require `hq propose`. |
| **[`inbox-triage`](../../.agents/plugins/agy-hq/skills/inbox-triage/SKILL.md)** | Classifies files dropped into `local/inbox/` or unread messages, writes structured summaries to `local/notes/`, and stages replies. | Quarantined reader session (blocks web egress, file writes, subagents, and private Vault reads); sending requires `hq propose`. |
| **[`gchat`](../../.agents/plugins/agy-hq/skills/gchat/SKILL.md)** | Searches, reads in quarantine, and stages 1-step outbound Google Chat messages (`./bin/hq gchat propose`) across Spaces and DMs. | Network-jailed headless Chrome CDP / Workspace OAuth transport + SHA-256 1-tap approval. |
| **[`thesis-check`](../../.agents/plugins/agy-hq/skills/thesis-check/SKILL.md)** | Maintains per-ticker `thesis.md` (*Core Thesis, Bull/Bear Case, Kill Criteria*) and evaluates new earnings or SEC 10-K/10-Q/8-K filings. | Every financial figure must cite `[Filing, Page/Section]`; zero brokerage trading tools. |
| **[`filing-diff`](../../.agents/plugins/agy-hq/skills/filing-diff/SKILL.md)** | Performs section-by-section semantic diffs of consecutive SEC filings (10-K vs. prior 10-K, 10-Q vs. prior 10-Q). | Highlights risk-factor additions, accounting policy shifts, footnote changes, and MD&A tone shifts with verbatim quotes. |
| **[`ledger-ingest`](../../.agents/plugins/agy-hq/skills/ledger-ingest/SKILL.md)** | Triggered when a bank CSV/OFX drops into `local/inbox/`; parses entries into double-entry postings in `local/private/ledger/`. | Enforces double-entry zero-balance integrity (`0.00` residual) via `bean-check` / `hledger` before staging changes. |
| **[`tax-prep`](../../.agents/plugins/agy-hq/skills/tax-prep/SKILL.md)** | Tracks annual checklists in `local/private/taxes/` (W-2, 1099, 1098, K-1) and extracts fields from PDFs dropped into `local/inbox/`. | Uses deterministic scripts for bracket/estimate calculations; isolates `local/private/` from tainted sessions. |
