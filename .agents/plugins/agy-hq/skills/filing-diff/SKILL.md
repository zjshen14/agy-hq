---
name: filing-diff
description: >-
  Section-by-section semantic diff of consecutive SEC filings (10-K vs prior 10-K,
  10-Q vs prior 10-Q) highlighting new risk factors, accounting policy shifts,
  footnote changes, and MD&A tone shifts.
---

# SEC Filing Section-by-Section Diff (`/filing-diff`)

Compare a newly released `10-K` or `10-Q` against the prior period's filing to surface material language changes.

## Steps

1. **Locate Current & Prior Filings:**
   - Retrieve the target sections (`Item 1A Risk Factors`, `Item 7 MD&A`, key accounting footnotes) for both the current and prior filing.
2. **Diff & Materiality Filter:**
   - Identify newly added paragraphs, deleted assurances, softened guidance language, or changed accounting policies (ignoring boilerplate date/number roll-forwards).
3. **Write Diff Report:**
   - Save a side-by-side summary with verbatim quotes to `local/notes/investing/tickers/<TICKER>/filing-diff-YYYY-MM-DD.md`.
