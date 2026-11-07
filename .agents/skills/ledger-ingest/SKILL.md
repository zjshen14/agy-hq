---
name: ledger-ingest
description: >-
  Plain-text accounting (Beancount / hledger) ingestion and reconciliation workflow.
  Parses bank CSV/OFX statements from local/inbox/ into double-entry postings in
  local/private/ledger/ and verifies zero-balance integrity.
---

# Plain-Text Accounting & Ledger Ingestion (`/ledger-ingest`)

Maintain a verifiable double-entry financial ledger inside `local/private/ledger/`.

## Steps

1. **Locate Statement & Ledger:**
   - Read the statement file from `local/inbox/` and inspect existing account definitions in `local/private/ledger/main.bean` (create starter chart of accounts if not yet initialized).
2. **Generate Double-Entry Postings:**
   - Convert each row deterministically into a dated Beancount transaction with explicit narration and category account.
   - Flag any ambiguous or high-value anomalous transactions for user review.
3. **Deterministic Validation:**
   - If `bean-check` or `hledger check` is installed, run it against `local/private/ledger/main.bean` to verify zero balance errors before finalizing.
