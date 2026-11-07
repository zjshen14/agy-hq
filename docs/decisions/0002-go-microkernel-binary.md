# ADR-0002: Build the `hq` Microkernel as a Single Static Go Binary (`bin/hq`)

- **Status:** Accepted
- **Date:** 2026-11-07
- **References:** [RFC-0001](../design/0001-vault-and-task-isolation.md), [RFC-0002](../design/0002-memory-and-consolidation.md)

---

## Context

Antigravity invokes lifecycle hooks (`SessionStart`, `PreToolUse`, `Stop` in `.agents/hooks.json`) and daemon sidecars (`sidecar.json`) as external OS commands communicating over JSON stdin/stdout, files, and HTTP. We must choose the implementation language for the `hq` kernel binary.

## Options Considered

1. **Go (`golang`) Single Static Binary (`bin/hq`):**
   - Compiles `hq init`, `hq hook ...`, `hq daemon`, `hq propose`, `hq notify`, and the `HQ Console` HTTP server into one dependency-free executable.
2. **Python CLI + Daemon:**
   - Matches the language of the standalone `google-antigravity` SDK and tools like `beancount`.

## Decision

Build the `agy-hq` kernel (`cmd/hq/` and `internal/`) in **Go (`golang`)**, while allowing individual `.agents/skills/` workflows to invoke prebuilt external CLIs or MCP servers in any language (such as `bean-check` or `sec-edgar-mcp`).

## Consequences

- **Positive:**
  - **Zero `venv`/`pip` Fragility in Hooks:** Shell hooks invoked by Antigravity never fail due to broken Python virtual environments or missing packages.
  - **Fast Synchronous Hook Startup:** Compiled Go hooks start in `<10ms` (estimated) vs. `150–400ms` for Python interpreter initialization.
  - **Native Concurrency:** Goroutines cleanly handle concurrent `fsnotify` inbox watching, 30-minute trigger pre-checks, and the embedded `HQ Console` web server in a small memory footprint.
- **Negative / Boundary:** The standalone `google-antigravity` SDK library (Option 2) is Python-only; if Option 2 is ever used on a headless Linux VM, a ~300-line Python SDK runner will call into the same `bin/hq` CLI tools and `local/` Vault.
