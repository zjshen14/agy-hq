# `agy-hq` Documentation Hub

Welcome to the documentation for **`agy-hq` (Headquarter)** — a lightweight, open-source Personal Agent OS built on top of Google Antigravity.

To keep documentation maintainable as the project evolves, docs are organized into five distinct categories:

1. **System Overview** (`architecture.md`, `roadmap.md`) — The 5-minute high-level map and current rollout plan.
2. **Product Designs (`product/`)** — User experience flows, touchpoints, and Extension Pack product specs.
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
| [PRD-0002](product/0002-pack-swe.md) | **Starter Pack: Side-Project SWE (`packs/swe`)** | Multi-repo pulse, CI/PR triage, and isolated worktree feature builder | Approved |
| [PRD-0003](product/0003-pack-invest.md) | **Starter Pack: Investment Research (`packs/invest`)** | Per-ticker `thesis.md` memory, SEC EDGAR watchers, and citation-verified bull/bear checks | Approved |
| [PRD-0004](product/0004-pack-admin.md) | **Starter Pack: Life Admin, Ledger & Taxes (`packs/admin`)** | Quarantined inbox triage, plain-text `beancount` bookkeeping, and year-round tax prep | Approved |

---

## 3. Technical Designs (`docs/design/`)

| ID | Title | Summary | Status |
|---|---|---|---|
| [RFC-0001](design/0001-vault-and-pack-isolation.md) | **Two-Layer Vault Split & Parallel Pack Isolation** | `pack.yaml` contract, `local/` Vault overlay, and the 4 parallel isolation guarantees | Approved |
| [RFC-0002](design/0002-memory-and-consolidation.md) | **Zero-Token Journaling & Sleep-Time Memory Consolidation** | Scoped `SessionStart` injection, `Stop` hook JSONL transcript extraction, and nightly delta patching | Approved |
| [RFC-0003](design/0003-approval-bound-execution.md) | **Safety, Quarantined Readers & Approval-Bound Execution** | Breaking the Lethal Trifecta, `PreToolUse` hard blocklist, and `hq propose` SHA-256 deterministic execution | Approved |
| [RFC-0004](design/0004-heartbeat-and-channels.md) | **Silent Gatekeeper, Quota Circuit Breaker & Channel Adapters** | 0-token Go pre-checks, Flash-only spawn budgets, Remote Control default UX, and pluggable chat drivers | Approved |

---

## 4. Key Decisions / ADRs (`docs/decisions/`)

| ID | Decision | Status |
|---|---|---|
| [ADR-0001](decisions/0001-option-1.5-hybrid-on-antigravity.md) | Adopt "Option 1.5" (Antigravity App + Thin Local Sidecar/Hooks) over standalone frameworks or OpenClaw forks | Accepted |
| [ADR-0002](decisions/0002-go-microkernel-binary.md) | Build the `hq` microkernel as a single static Go binary (`bin/hq`) | Accepted |
| [ADR-0003](decisions/0003-two-layer-oss-and-private-vault.md) | Separate the open-source engine (`agy-hq`) from the git-ignored private user `Vault` (`local/`) | Accepted |
| [ADR-0004](decisions/0004-remote-control-first-pluggable-channels.md) | Piggyback on Antigravity Remote Control for Phase 1 mobile UX behind a pluggable `ChannelAdapter` | Accepted |
| [ADR-0005](decisions/0005-zero-token-stop-hook-vs-blocking-gate.md) | Use 0-token `Stop` hook transcript parsing instead of blocking turns with `decision: continue` | Accepted |

---

## 5. Research Archive (`docs/research/`)

- [01 — Frontier Personal Agent Product Landscape](research/01_products.md)
- [02 — Academic & Research Frontier](research/02_academic.md)
- [03 — Technical Build Options & Integrations](research/03_technical.md)
- [04 — Google Antigravity Capability Audit](research/04_antigravity_audit.md)
