---
name: thesis-check
description: >-
  Investment thesis tracker and falsification monitor. Evaluates new earnings
  releases, 10-K/10-Q/8-K filings, or market developments against the user's
  written thesis and kill criteria in local/notes/investing/.
---

# Investment Thesis & Falsification Check (`/thesis-check`)

> **Model Routing Tiering (`local/config.yaml` `routing.tiers`):**
> - **High-frequency polling** (e.g., watchlist inbox scans): `--tier=quick` (`hq dispatch resolve --skill=thesis-check`).
> - **Routine close watch & filing extraction** (e.g., `edgar-close-watch`): `--tier=routine` (`hq dispatch resolve --skill=thesis-check`).
> - **High-stakes thesis falsification / `KC-1` breach audits:** `--tier=deep`.

Evaluate new filings or news against an explicit investment thesis and falsification ("kill") criteria.

## Steps

0. **Extension Dispatch & Overlay Check:** Run `hq dispatch resolve --skill=thesis-check --json`; if `overridden == true`, activate the resolved companion skill (`skill_path`) and stop following this file; if `has_overlay == true` with non-empty `overlay_rules`, apply those active personal rules for this run.
1. **Load Existing Thesis:**
   - Read `local/notes/investing/tickers/<TICKER>/thesis.md` (or create a structured starter thesis with core bull case, bear case, key KPIs, and explicit kill criteria if tracking a new ticker).
2. **Evidence Extraction with Exact Citations:**
   - Extract KPIs, segment revenue/margins, guidance changes, and risk disclosures with exact filing date and section/page references.
   - Use Python scripts for any valuation, margin, or growth-rate calculations.
3. **Kill-Criteria Audit & Attention-Budgeted Delivery (RFC-0014):**
   - Explicitly score each falsification criterion (`INTACT`, `WATCH`, or `BREACHED`) and append a dated entry to `local/notes/investing/tickers/<TICKER>/journal.md`.
   - **Routine `INTACT` Checks (Zero Interrupt):** When running from a scheduled gatekeeper (`edgar-close-watch`) and all kill criteria remain `INTACT`, do **not** interrupt the user; queue a 1-line summary for the next Morning Brief via:
     ```sh
     hq outbox add --tier=brief_rollup --scope=investing --skill=thesis-check --title="<TICKER> filing check: INTACT" --summary="..."
     ```
   - **`WATCH` or `BREACHED` Alerts:** Immediately surface via `hq outbox add --tier=interrupt --critical --scope=investing --skill=thesis-check --title="<TICKER> Thesis Alert" --summary="..."` (and stage any follow-up action via `hq propose`).

## Pre-IPO Secondary Opportunities & Deal-Flow Watchdog

1. **Automated Secondary Watchdog:**
   - Optional recurring watch (using Antigravity's native `schedule` tool, or a 0-token `daemon_gatekeeper` in `local/triggers.yaml`) to ingest inbound private-market or secondary deal flow.
2. **Autonomous Document Pulls (Zero Friction Invariant):**
   - Never ask the user whether to pull financials or term sheets. Autonomously pull all prospectuses, cap tables, valuation metrics, and transaction history proactively.
3. **Master Retrospective Ledger (`local/notes/investing/opportunities/INDEX.md`):**
   - Record every evaluated secondary deal with company name, implied valuation, discount/premium vs. last round, moat score (1–10), decision (`Buy`, `Watch`, `Pass`), and future review trigger dates.
