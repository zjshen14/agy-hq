# PRD-0003: Starter Pack — Investment Research (`packs/invest`)

- **Status:** Approved
- **Pack Path:** `packs/invest/` (Open-Source Spec) · `local/packs/invest/` (Private Vault State)
- **Related Docs:** [02 — Academic Frontier §7](../research/02_academic.md), [RFC-0001 (Pack Isolation)](../design/0001-vault-and-pack-isolation.md)

---

## 1. Problem Statement

General chatbots summarize market news but forget your original investment thesis, hallucinate numbers from SEC filings (~35%+ error rate on Finance Agent Benchmark without verification), and suffer from look-ahead bias.

---

## 2. Capabilities & Workflows

1. **Per-Ticker Thesis Memory (`local/packs/invest/tickers/<TICKER>/thesis.md`):**
   - Every holding or watchlist company has a structured markdown file recording:
     - *Core Thesis & Variant Perception*
     - *Key Operating Drivers & Valuation Guardrails*
     - *Bull Case vs. Bear Case*
     - *Explicit Falsification Criteria ("What evidence would make me exit?")*
2. **SEC Filing & Earnings Watcher (`edgar-close-watch` Schedule & `inbox/` Trigger):**
   - Monitors `local/packs/invest/watchlist.yaml` against SEC EDGAR RSS feeds (10-K, 10-Q, 8-K) and earnings releases.
3. **Bull vs. Bear Thesis Check (`invest-thesis-check` Skill):**
   - When a new filing drops, extracts financial tables via XBRL/deterministic scripts (never LLM mental math).
   - Spawns parallel read-only **Bull** and **Bear** subagents to test the new data against your `thesis.md` falsification criteria.
   - Produces a concise impact brief where every number carries a `[Filing, Section/Page]` citation.

---

## 3. Guardrails & Isolation Rules

- **Citation & Script Verification Mandatory:** No uncited financial figures are permitted in thesis updates.
- **Zero Trading Capability:** This pack has **no brokerage execution tools or permissions**—research, thesis tracking, and alerting only.
