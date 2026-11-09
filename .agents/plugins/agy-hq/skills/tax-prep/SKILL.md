---
name: tax-prep
description: >-
  Tax document extraction, checklist tracking, and deterministic workpaper
  preparation inside local/private/taxes/. Use when processing W-2, 1099, 1098,
  K-1 forms, or estimating quarterly/annual taxes.
---

# Tax Document Checklist & Workpaper Prep (`/tax-prep`)

Organize tax documents and build source-traceable tax workpapers inside `local/private/taxes/` without using LLM mental math.

## Steps

1. **Checklist & Document Inventory:**
   - Organize forms under `local/private/taxes/<tax-year>/docs/` and update `local/private/taxes/<tax-year>/checklist.md` comparing received forms (`W-2`, `1099-INT`, `1099-DIV`, `1099-B`, `1098`, `K-1`) against prior-year issuers.
2. **Structured Box Extraction:**
   - Extract each numbered box value into `local/private/taxes/<tax-year>/extracted_boxes.json` with exact document filename and page citation.
3. **Code-Calculated Workpaper (Zero LLM Arithmetic):**
   - Run a deterministic Python script over `extracted_boxes.json` to compute totals, capital gains summaries, and estimated federal/state tax liability, writing the audit trail to `local/private/taxes/<tax-year>/workpaper.md`.
