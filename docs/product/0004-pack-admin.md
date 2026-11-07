# PRD-0004: Starter Pack — Life Admin, Ledger & Taxes (`packs/admin`)

- **Status:** Approved
- **Pack Path:** `packs/admin/` (Open-Source Spec) · `local/packs/admin/` (Private Vault State)
- **Related Docs:** [RFC-0001 (Pack Isolation)](../design/0001-vault-and-pack-isolation.md), [RFC-0003 (Approval Execution)](../design/0003-approval-bound-execution.md)

---

## 1. Problem Statement

Life admin tasks (triaging emails, reconciling expenses, tracking tax documents, booking reservations) carry the highest exposure to **indirect prompt injection** and **irreversible mistakes** (sending the wrong email, bad tax arithmetic, or leaking PII).

---

## 2. Four Core Workflows

1. **Quarantined Inbox & Calendar Triage (`admin-inbox-triage` Skill):**
   - Reads unread emails and calendar events via `gws` / Google Calendar plugin using a **read-only + local file write** grant (`local/packs/admin/inbox_digest.md`).
   - Drafts replies and stages them via `hq propose` for 1-click user approval.
2. **Plain-Text Double-Entry Bookkeeping (`admin-ledger-reconcile` Skill):**
   - Watches `local/inbox/` for bank/card CSV or OFX files.
   - Categorizes transactions into `local/packs/admin/ledger/2026.beancount` using your rules, runs the deterministic `bean-check` validator to verify double-entry balance, and stages the git diff for approval.
3. **Year-Round Tax Prep (`admin-tax-prep` Skill):**
   - Maintains `local/packs/admin/taxes/2026/checklist.md` (expected W-2s, 1099s, K-1s, deductible receipts, quarterly estimated deadlines).
   - Ingests tax PDFs dropped into `local/inbox/`, extracts fields into `workpaper.csv`, and uses deterministic scripts for tax-bracket estimates (never bare LLM calculation).
4. **Assisted Reservations & Bookings (`admin-booking` Skill):**
   - Uses Chrome DevTools MCP (`/browser`) to check availability and navigate up to the final confirmation screen, then pauses and requests explicit user confirmation with a screenshot and exact terms (`Venue, Time, Party Size, Total $, Cancellation Policy`).

---

## 3. Guardrails & Isolation Rules

- **Strict Data Compartmentalization:** `local/packs/admin/` (containing tax and financial records) is isolated from all other packs (`swe`, `invest`) by `hq hook session-start` and `hq hook pre-tool`.
- **Secrets Out of Context:** Passwords and payment credentials stay in macOS Keychain / 1Password and merchant-locked virtual cards, never in `MEMORY.md` or git.
