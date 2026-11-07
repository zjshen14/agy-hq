# Implementation Roadmap

This document tracks the phased rollout of `agy-hq`.

---

## Milestone Overview

| Milestone | Scope | References | Status |
|---|---|---|---|
| **M0: Safety Setup & Runtime Spikes** | Switch Antigravity preset off `Turbo` to `Default`/`Auto` (Seatbelt sandbox active); verify `agentapi` CLI flags and `hooks.json` round-trip with `bin/hq`. | [RFC-0003](design/0003-approval-bound-execution.md) | Next |
| **M1: Microkernel, Vault & Built-In Skills** | Build `bin/hq` (`init`, `hook`, `propose`, `notify`, `daemon`, Aux Console); run `hq init` to scaffold `local/` Vault (`USER.md`, `MEMORY.md`, `triggers.yaml`, `workspace/`, `private/`); create built-in `.agents/skills/*/SKILL.md` workflows. | [RFC-0001](design/0001-vault-and-task-isolation.md), [RFC-0002](design/0002-memory-and-consolidation.md), [PRD-0002](product/0002-open-workflows-and-skills.md) | Planned |
| **M2: Scheduled Automations & Proactive Loop** | Register `morning-brief` (07:30), `edgar-close-watch` (13:30), and `sleep-consolidate` (23:30) pinned to `--model=flash`; enable 30m 0-token gatekeeper in `hq daemon`. | [RFC-0004](design/0004-heartbeat-and-channels.md) | Planned |
| **M3: External Chat Channel Adapters** | Add optional Google Chat (Desktop OAuth / PubSub) and Telegram drivers implementing `ChannelAdapter`. | [ADR-0004](decisions/0004-remote-control-first-pluggable-channels.md), [RFC-0004](design/0004-heartbeat-and-channels.md) | Future |
| **M4: Evaluation Harness & Search Index** | Add `evals/` private task replay suite (`pass^k`) and optional SQLite FTS5 (`sqlite-vec`) index over historical journals. | [RFC-0002](design/0002-memory-and-consolidation.md) | Future |
