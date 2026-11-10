<p align="center">
  <img src="../assets/branding/icons/icon-obsidian-128x128.png" width="96" height="96" alt="agy-hq logo" />
</p>

# `agy-hq` Documentation Hub

Welcome to the documentation for **`agy-hq` (Headquarter)** — a lightweight, open-source Personal Agent OS built on top of Google Antigravity.

To keep documentation maintainable as the project evolves, docs are organized into five categories:

1. **System Overview** (`architecture.md`, `roadmap.md`) — The 5-minute high-level map and current rollout plan.
2. **Product Designs (`product/`)** — User experience flows, touchpoints, and open-ended workflow + skill specs.
3. **Technical Designs (`design/`)** — Focused engineering RFCs for individual kernel subsystems and protocols.
4. **Key Decisions / ADRs (`decisions/`)** — Architecture Decision Records capturing *Context, Options Considered, Decision, and Consequences*.
5. **Research Archive (`research/`)** — Point-in-time landscape surveys, academic literature reviews, and platform audits.

---

## 1. System Overview

| Document | Purpose | Status |
|---|---|---|
| [architecture.md](architecture.md) | High-level system map, two-layer (`agy-hq` vs. `Vault`) layout, and component index | Living |
| [roadmap.md](roadmap.md) | Phased implementation milestones and pre-build verification checklist | Living |

---

## 2. Product Designs (`docs/product/`)

| ID | Title | Summary | Status |
|---|---|---|---|
| [PRD-0001](product/0001-vision-and-ux.md) | **Core Product Vision & Day-in-the-Life UX** | The four user touchpoints: Mobile/Web Remote Control, Desktop + Aux Console, Drop Folder, and Sleep-Time Consolidation | Approved |
| [PRD-0002](product/0002-open-workflows-and-skills.md) | **Open-Ended Workflows & Additive Skills** | How the agent handles any ad-hoc task out of the box (writing, coding, research, admin), boosted by progressive-disclosure `SKILL.md` skills | Approved |
| [PRD-0003](product/0003-critical-user-journeys.md) | **Critical User Journeys (CUJs 1–18) & E2E Acceptance Criteria** | Deterministic expected behaviors across all 18 core journeys enforced via automated E2E tests (`internal/e2e/cuj_e2e_test.go`) | Approved |

---

## 3. Technical Designs (`docs/design/`)

| ID | Title | Summary | Status |
|---|---|---|---|
| [RFC-0001](design/0001-vault-and-task-isolation.md) | **Two-Layer Vault Split & Per-Task Parallel Isolation** | Public engine vs. private `local/` Vault, lock-free per-conversation journals, and parallel task execution | Approved |
| [RFC-0002](design/0002-memory-and-consolidation.md) | **Zero-Token Journaling & Sleep-Time Memory Consolidation** | `SessionStart` injection, `Stop` hook JSONL transcript extraction, and nightly delta patching + skill proposals | Approved |
| [RFC-0003](design/0003-approval-bound-execution.md) | **Action-Based Safety, Quarantined Readers & Approval-Bound Execution** | Breaking the Lethal Trifecta, `PreToolUse` hard blocklist, and `hq propose` SHA-256 deterministic execution | Approved |
| [RFC-0004](design/0004-heartbeat-and-channels.md) | **Silent Gatekeeper, Quota Circuit Breaker & Channel Adapters** | 0-token Go pre-checks (`local/triggers.yaml`), Flash-only spawn budgets, Remote Control default UX, and pluggable chat drivers | Approved |
| [RFC-0005](design/0005-gmail-integration.md) | **Decoupled Native Gmail Integration (Layer 1 Infra & Layer 2 Workflows)** | Layer 1: extensible $N$-account registry, pluggable transport, pure Go `hq daemon` sidecar watcher, and safe CRUD primitives (`search`, `read-quarantined`, `draft`, `approve`). Layer 2: Daily Brief, Auto-Draft Triage, and Verified Attachment Routing | Approved |
| [RFC-0006](design/0006-memory-engine-upgrade.md) | **4-Layer Memory Hierarchy, Ground-Truth Dreaming & Proactive Fact Pruning** | Anthropic-grounded memory hierarchy (Roadmap Index, Topic Wiki, Bi-Temporal Facts, Raw Transcripts), offline dreaming with transcript verification, and proactive pruning | Approved |
| [RFC-0007](design/0007-front-desk-initialization-hardening.md) | **Front Desk Initialization Hardening, Trigger Arming & Shift Handover** | Non-blocking Front Desk state machine, deterministic Turn-1 initialization, `hq_console.md` UI artifact pinning, automated `triggers.yaml` schedule arming, and shift handover coordination | Approved |
| [RFC-0008](design/0008-gchat-integration.md) | **Decoupled Google Chat Integration (Layer 1 Infra, Hybrid Transports & Layer 2 Workflows)** | Multi-transport adapter (`oauth` REST API v1 for Workspace, isolated `--headless=new` Chrome `cdp` with DNS/URL network jail + `gmail-bridge` for personal `@gmail.com`, `webhook`), guided 1-time `hq gchat login`, fuzzy space name resolution, 1-step `hq gchat propose`, and 1-tap `gchat_send` approval | Approved |
| [RFC-0009](design/0009-task-board-redesign.md) | **Scalable Task Board, Live Console & Hot/Cold Lifecycle Redesign** | Per-tier console visibility (100% active shown, capped idle/completed), two-tier hot/cold storage (`tasks.json` + `tasks_archive.jsonl`), seamless cold recall (`hq reopen`), bounded `SessionStart`, and `hq tasks` CLI | Approved |
| [RFC-0010](design/0010-agent-recursive-self-improvement.md) | **Bounded Agent Recursive Self-Improvement (RSI) — 4-Stage Trace Mining, Verification & Human-Gated Evolution** | 0-token friction trace mining, ACE-compliant skill/code delta hypotheses, 4-gate sandboxed verification oracle (Ring 0 TCB AST check + counterfactual replay + E2E suite), and 1-tap SHA-256 approval | Approved |
| [RFC-0011](design/0011-ci-pipeline-and-regression-harness.md) | **Automated CI/CD Pipeline, Mathematical PII/Vault Boundary Verification & Agent Harness Regression Gate** | Two-tier defense-in-depth (`pre-commit`/`pre-push` + GitHub Actions), 5-invariant mathematical PII/secret scanner (`hq ci check-pii`), Skill/Hook/Routing contract verifier (`hq ci check-harness`), and `sync.Once` E2E acceleration (`10.5s` -> `1.8s`) | Approved |

---

## 4. Key Decisions / ADRs (`docs/decisions/`)

| ID | Decision | Status |
|---|---|---|
| [ADR-0001](decisions/0001-option-1.5-hybrid-on-antigravity.md) | Adopt "Option 1.5" (Antigravity App + Thin Local Sidecar/Hooks) over standalone frameworks or OpenClaw forks | Accepted |
| [ADR-0002](decisions/0002-go-microkernel-binary.md) | Build the `hq` microkernel as a single static Go binary (`bin/hq`) | Accepted |
| [ADR-0003](decisions/0003-two-layer-oss-and-private-vault.md) | Separate the open-source engine (`agy-hq`) from the git-ignored private user `Vault` (`local/`) | Accepted |
| [ADR-0004](decisions/0004-remote-control-first-pluggable-channels.md) | Piggyback on Antigravity Remote Control for Phase 1 mobile UX behind a pluggable `ChannelAdapter` | Accepted |
| [ADR-0005](decisions/0005-zero-token-stop-hook-vs-blocking-gate.md) | Use 0-token `Stop` hook transcript parsing instead of blocking turns with `decision: continue` | Accepted |
| [ADR-0006](decisions/0006-general-agent-and-skills-over-packs.md) | Use one general-purpose agent + additive `SKILL.md` skills and per-task isolation instead of rigid domain packs | Accepted |

---

## 5. Research Archive (`docs/research/`)

- [01 — Frontier Personal Agent Product Landscape](research/01_products.md)
- [02 — Academic & Research Frontier](research/02_academic.md)
- [03 — Technical Build Options & Integrations](research/03_technical.md)
- [04 — Google Antigravity Capability Audit](research/04_antigravity_audit.md)
- [05 — Google Chat Integration Pathways & Account Architecture Audit](research/05_gchat_integration.md)
- [06 — Task Board, Console Layout & Conversation Lifecycle Audit](research/06_taskboard_and_console_audit.md)
- [07 — Agent Recursive Self-Improvement (RSI): Literature Survey, Production Trace Loops & Safety Invariants](research/07_agent_recursive_self_improvement.md)
