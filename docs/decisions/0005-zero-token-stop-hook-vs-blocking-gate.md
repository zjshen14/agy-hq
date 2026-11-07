# ADR-0005: Use Zero-Token `Stop` Hook Transcript Parsing Instead of Blocking Turns

- **Status:** Accepted
- **Date:** 2026-11-07
- **References:** [RFC-0002](../design/0002-memory-and-consolidation.md), [Second-Opinion Review](../../README.md)

---

## Context

Antigravity's `Stop` lifecycle hook executes a deterministic OS command when an agent turn finishes (`terminationReason: "NO_TOOL_CALL"`, `fullyIdle: true`). Because the hook binary (`bin/hq`) does not run an LLM itself, we considered two ways to capture episodic session journals:
1. **Option A (Turn-Blocking Gate):** Have `Stop` return `{"decision": "continue", "reason": "Write your journal entry first"}` whenever the agent hasn't written a summary file, forcing the agent to run an extra LLM turn and file-write tool call before stopping.
2. **Option B (Zero-Token Transcript Extraction + Nightly Consolidation):** Have `hq hook stop` read `transcriptPath` (passed by Antigravity on stdin) directly in Go, extract a structured turn record (`lastUserInput`, tools called, files modified, response snippet) into `local/memory/journal/YYYY-MM-DD/<convId>.json` in ~2ms with zero LLM calls, and let the 23:30 `sleep-consolidate` Flash job distill semantic insights once per night.

## Decision

Choose **Option B (Zero-Token Transcript Extraction + Nightly Consolidation)** and reject Option A.

## Consequences

- **Positive:** Zero extra LLM turns or tool calls during interactive chat; zero added latency when a turn completes; saves 5-hour/weekly plan quota; writes immutable per-turn JSON files that never collide across parallel sessions.
- **Negative:** Semantic abstraction into `local/MEMORY.md` happens asynchronously during the nightly consolidation run (or when `/learn` is invoked explicitly) rather than synchronously at the end of every turn, while raw turn records in `local/memory/journal/` are available immediately.
