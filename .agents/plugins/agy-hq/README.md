# `agy-hq` (Headquarter) — Personal Agent OS for Google Antigravity

**`agy-hq`** turns an always-on machine running Google Antigravity into a 24/7 general-purpose, proactive personal assistant—while keeping **100% of your personal memory, files, and credentials isolated in a private `Vault` (`local/`)**.

## Capabilities

- **Single `[HQ] Front Desk` Coordinator:** Non-blocking intake desk with a live 0-token `hq_console.md` Task Board & Quota Ledger.
- **Zero-Token Lifecycle Hooks (`.agents/hooks.json`):** Automatic `SessionStart` context injection, `PreToolUse` safety & non-blocking guardrails, and `Stop` turn journaling.
- **Progressive-Disclosure Workflows (`.agents/skills/`):** `morning-brief`, `inbox-triage`, `swe-pulse`, `sleep-consolidate`, `thesis-check`, `filing-diff`, `ledger-ingest`, and `tax-prep`.
- **Approval-Bound Outbound Execution (`./bin/hq propose`):** Cryptographic SHA-256 1-tap approvals for external mutations (Gmail, Google Chat, PR merges).
