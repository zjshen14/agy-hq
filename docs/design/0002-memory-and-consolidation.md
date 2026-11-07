# RFC-0002: Zero-Token Journaling & Sleep-Time Memory Consolidation

- **Status:** Approved
- **Related Docs:** [ADR-0005](../decisions/0005-zero-token-stop-hook-vs-blocking-gate.md), [02 — Academic Frontier §1 & §3](../research/02_academic.md)

---

## 1. Research Grounding

1. **Filesystem Memory Competes with Specialized DBs (Letta LoCoMo Study):** Structured markdown/JSONL files with search score ~74% on LoCoMo while remaining human-inspectable and git-diffable.
2. **Sleep-Time Compute (Letta / UC Berkeley, 2025):** Consolidating raw logs offline cuts test-time compute by 5× and boosts accuracy by +13–18% on predictable recurring queries.
3. **Agentic Context Engineering (ACE, 2025):** Never allow an LLM to rewrite `MEMORY.md` or `SKILL.md` wholesale (which causes context collapse); always apply incremental delta patches.
4. **Verified Procedural Memory (SkillsBench, 2026):** Curated skills add +16 percentage points to pass rates, whereas unverified self-generated skills add −1.3 pp. All self-proposed skill updates must be staged for verification/approval.

---

## 2. Three-Phase Memory Lifecycle

### Phase A: Scoped Context Injection (`hq hook session-start`)
- Triggered on `SessionStart` via `.agents/hooks.json`.
- Reads `local/USER.md`, `local/MEMORY.md`, the active pack's `local/packs/<id>/MEMORY.md`, and matching active facts from `local/memory/facts.jsonl` (`valid_to == null`).
- Returns an `injectSteps` `systemMessage` payload on stdout in `<10ms`.

### Phase B: Zero-Token Turn Journaling (`hq hook stop`)
- Triggered on `Stop` when `fullyIdle == true` and `terminationReason == "NO_TOOL_CALL"`.
- Reads `transcriptPath` (passed by Antigravity in the hook stdin JSON) directly in Go (**0 LLM tokens, ~2ms, zero turn blocking**).
- Writes an immutable JSON record to `local/memory/journal/YYYY-MM-DD/<convId>-<stepIdx>.json`:
  ```json
  {
    "timestamp": "2026-11-07T14:20:00-08:00",
    "conversationId": "ec33ebf9-...",
    "pack": "invest",
    "userPrompt": "Check NVDA 8-K impact",
    "toolsUsed": ["read_file", "search_web"],
    "filesModified": ["local/packs/invest/tickers/NVDA/notes.md"],
    "responseSummary": "First/last paragraph extracted from assistant response..."
  }
  ```

### Phase C: Nightly Sleep-Time Consolidation (`sleep-consolidate` Skill at 23:30)
- Runs once per night as a scheduled Flash automation:
  1. Reads all `local/memory/journal/YYYY-MM-DD/*.json` and `local/memory/interventions.jsonl` files.
  2. Applies incremental delta patches to `local/MEMORY.md` and `local/packs/<id>/MEMORY.md`.
  3. Appends/supersedes bi-temporal entries in `local/memory/facts.jsonl` (`valid_from`, `valid_to`).
  4. Stages any proposed `SKILL.md` patches in `local/memory/proposals/` for user approval the next morning.
  5. Commits `local/` Vault changes to the private git repository.
