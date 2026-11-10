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

## Pre-IPO Secondary Opportunities & Deal-Flow Watchdog

1. **Automated Secondary Watchdog (`secondary-deal-watch`):**
   - Optional scheduled trigger (configured in `local/triggers.yaml`) to ingest inbound private-market or secondary deal flow.
2. **Autonomous Document Pulls (Zero Friction Invariant):**
   - Never ask the user whether to pull financials or term sheets. Autonomously pull all prospectuses, cap tables, valuation metrics, and transaction history proactively.
3. **Master Retrospective Ledger (`local/notes/investing/opportunities/INDEX.md`):**
   - Record every evaluated secondary deal with company name, implied valuation, discount/premium vs. last round, moat score (1–10), decision (`Buy`, `Watch`, `Pass`), and future review trigger dates.
