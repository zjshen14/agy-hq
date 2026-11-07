---
name: thesis-check
description: >-
  Investment thesis tracker and falsification monitor. Evaluates new earnings
  releases, 10-K/10-Q/8-K filings, or market developments against the user's
  written thesis and kill criteria in local/notes/investing/.
---

# Investment Thesis & Falsification Check (`/thesis-check`)

Evaluate new filings or news against an explicit investment thesis and falsification ("kill") criteria.

## Steps

1. **Load Existing Thesis:**
   - Read `local/notes/investing/tickers/<TICKER>/thesis.md` (or create a structured starter thesis with core bull case, bear case, key KPIs, and explicit kill criteria if tracking a new ticker).
2. **Evidence Extraction with Exact Citations:**
   - Extract KPIs, segment revenue/margins, guidance changes, and risk disclosures with exact filing date and section/page references.
   - Use Python scripts for any valuation, margin, or growth-rate calculations.
3. **Kill-Criteria Audit:**
   - Explicitly score each falsification criterion (`INTACT`, `WATCH`, or `BREACHED`) and append a dated entry to `local/notes/investing/tickers/<TICKER>/journal.md`.
