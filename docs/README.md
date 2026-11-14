<p align="center">
  <img src="../assets/branding/icons/icon-obsidian-128x128.png" width="96" height="96" alt="agy-hq logo" />
</p>

# `agy-hq` Documentation Hub

Welcome to the engineering and product documentation for **`agy-hq` (Headquarter)** — a lightweight, open-source Personal Agent OS built on top of Google Antigravity.

> **Where Should I Start?**
> - **New to `agy-hq` (5-minute tour):** Start with [PRD-0001 (Vision & Day-in-the-Life UX)](product/0001-vision-and-ux.md) → [PRD-0002 (Open Workflows & Skills)](product/0002-open-workflows-and-skills.md) → [System Architecture](architecture.md).
> - **Evaluating Architecture & Internals:** Read [System Architecture](architecture.md) → [ADR-0001 (Hybrid Antigravity Architecture)](decisions/0001-option-1.5-hybrid-on-antigravity.md) & [ADR-0003 (Two-Layer Vault)](decisions/0003-two-layer-oss-and-private-vault.md) → [RFC-0006 (4-Layer Memory)](design/0006-memory-engine-upgrade.md) & [RFC-0012 (Token Observability)](design/0012-token-observability.md).
> - **Reviewing Security, Privacy & CI Guarantees:** Read [SECURITY.md](../SECURITY.md) → [RFC-0003 (Approval-Bound Execution & Quarantine)](design/0003-approval-bound-execution.md) → [RFC-0011 (CI & PII Guard)](design/0011-ci-pipeline-and-regression-harness.md) → [CONTRIBUTING.md](../CONTRIBUTING.md).

---

## 1. System Overview

| Document | Purpose | Status |
|---|---|---|
| [architecture.md](architecture.md) | High-level system map, two-layer (`agy-hq` vs. `Vault`) layout, and component index | Living |
| [roadmap.md](roadmap.md) | Phased implementation milestones and upcoming `M4` extensions | Living |

---

## 2. Product Designs (`docs/product/`)

| ID | Title | Summary | Status |
|---|---|---|---|
| [PRD-0001](product/0001-vision-and-ux.md) | **Core Product Vision & Day-in-the-Life UX** | The four user touchpoints: Mobile/Web Remote Control, Desktop + Aux Console, Drop Folder, and Sleep-Time Consolidation | Approved |
| [PRD-0002](product/0002-open-workflows-and-skills.md) | **Open-Ended Workflows & Additive Skills** | How the agent handles any ad-hoc task out of the box (writing, coding, research, admin), boosted by progressive-disclosure `SKILL.md` skills | Approved |
| [PRD-0003](product/0003-critical-user-journeys.md) | **Critical User Journeys (CUJs 1–24) & E2E Acceptance Criteria** | Deterministic expected behaviors across all 24 core journeys enforced via automated E2E tests (`internal/e2e/cuj_e2e_test.go`) | Approved |

---

## 3. Technical Designs (`docs/design/`)

| ID | Title | Summary | Status |
|---|---|---|---|
| [RFC-0001](design/0001-vault-and-task-isolation.md) | **Two-Layer Vault Split & Per-Task Parallel Isolation** | Public engine vs. private `local/` Vault, lock-free per-conversation journals, and parallel task execution | Implemented |
| [RFC-0002](design/0002-memory-and-consolidation.md) | **Zero-Token Journaling & Sleep-Time Memory Consolidation** | `SessionStart` injection, `Stop` hook JSONL transcript extraction, and nightly delta patching + skill proposals | Implemented |
| [RFC-0003](design/0003-approval-bound-execution.md) | **Action-Based Safety, Quarantined Readers & Approval-Bound Execution** | Breaking the Lethal Trifecta, `PreToolUse` hard blocklist, and `hq propose` SHA-256 deterministic execution | Implemented |
| [RFC-0004](design/0004-heartbeat-and-channels.md) | **Silent Gatekeeper, Quota Circuit Breaker & Channel Adapters** | 0-token Go pre-checks (`local/triggers.yaml`), Flash-only spawn budgets, Remote Control default UX, and pluggable chat drivers | Implemented |
| [RFC-0005](design/0005-gmail-integration.md) | **Decoupled Native Gmail Integration (Layer 1 Infra & Layer 2 Workflows)** | Layer 1: extensible multi-account registry, pluggable transport, pure Go `hq daemon` sidecar watcher, and safe CRUD primitives (`search`, `read-quarantined`, `draft`, `approve`). Layer 2: Daily Brief, Auto-Draft Triage, and Verified Attachment Routing | Implemented |
| [RFC-0006](design/0006-memory-engine-upgrade.md) | **4-Layer Memory Hierarchy, Ground-Truth Dreaming & Proactive Fact Pruning** | Anthropic-grounded memory hierarchy (Roadmap Index, Topic Wiki, Bi-Temporal Facts, Raw Transcripts), offline dreaming with transcript verification, and proactive pruning | Implemented |
| [RFC-0007](design/0007-front-desk-initialization-hardening.md) | **Front Desk Initialization Hardening, Trigger Arming & Shift Handover** | Non-blocking Front Desk state machine, deterministic Turn-1 initialization, `hq_console.md` UI artifact pinning, automated `triggers.yaml` schedule arming, and shift handover coordination | Implemented |
| [RFC-0008](design/0008-gchat-integration.md) | **Decoupled Google Chat Integration (Layer 1 Infra, Hybrid Transports & Layer 2 Workflows)** | Multi-transport adapter (`oauth` REST API v1 for Workspace, isolated `--headless=new` Chrome `cdp` with DNS/URL network jail + `gmail-bridge` for personal `@gmail.com`, `webhook`), guided 1-time `hq gchat login`, fuzzy space name resolution, 1-step `hq gchat propose`, and 1-tap `gchat_send` approval | Implemented |
| [RFC-0009](design/0009-task-board-redesign.md) | **Scalable Task Board, Live Console & Hot/Cold Lifecycle Redesign** | Per-tier console visibility (100% active shown, capped idle/completed), two-tier hot/cold storage (`tasks.json` + `tasks_archive.jsonl`), seamless cold recall (`hq reopen`), bounded `SessionStart`, and `hq tasks` CLI | Implemented |
| [RFC-0010](design/0010-agent-recursive-self-improvement.md) | **Bounded Agent Recursive Self-Improvement (RSI) — 4-Stage Trace Mining, Verification & Human-Gated Evolution** | 0-token friction trace mining, ACE-compliant skill/code delta hypotheses, 4-gate sandboxed verification oracle (Ring 0 TCB AST check + counterfactual replay + E2E suite), and 1-tap SHA-256 approval | Implemented |
| [RFC-0011](design/0011-ci-pipeline-and-regression-harness.md) | **Automated CI/CD Pipeline, Mathematical PII/Vault Boundary Verification & Agent Harness Regression Gate** | Two-tier defense-in-depth (`pre-commit`/`pre-push` + GitHub Actions), 6-invariant mathematical PII/secret scanner (`hq ci check-pii`), Skill/Hook/Routing contract verifier (`hq ci check-harness`), and `sync.Once` E2E acceleration (`10.5s` → `1.8s`) | Implemented |
| [RFC-0012](design/0012-token-observability.md) | **End-to-End Token Usage, Latency & Cost/Quota Observability** | Stat-cached transcript token/latency ingestion, ground-truth parent-child subagent DAG reconciliation, `./bin/hq obs` CLI (`tokens`, `top`, `inspect`, `anomalies`, `budget`, `reconcile`), Web UI & Console gauges, and 5 deterministic anomaly/budget guards | Implemented |
| [RFC-0013](design/0013-open-source-launch-strategy.md) | **Open-Source Readiness, Forensic Privacy Sanitization, Governance & Launch Architecture** | Forensic Two-Layer Vault & Git history audit (`P6-ContextualPII` + `check-pii --history`), subprocess `GIT_DIR` hardening, licensing & governance (`Apache-2.0` vs. `MIT`, `SECURITY.md`, `CONTRIBUTING.md`), 5-minute `--demo` onboarding, and Show HN launch playbook | Implemented |
| [RFC-0014](design/0014-proactive-engine-and-living-context.md) | **Proactive Engine v2 — Event-Driven Gatekeepers, Calibrated Attention Routing, Stateful Goals, Scheduled Console Spaces & Living Context** | Muse-informed proactive upgrades: 0-token cursor gatekeepers & isolated workers (eliminating Front Desk cold-cache wakeups), 4-tier Attention Router + `Ideas` feed, stateful multi-day `Goals` & `WaitingOn` SLAs, context-scheduled Console `Spaces`, surgical `hq memory forget` + Entity Dossiers, and universal `local/private/` credential read-guards | Implemented |
| [RFC-0015](design/0015-global-plugin-packaging.md) | **Standalone Global Antigravity Plugin Packaging, Embedded Templates & Workspace-Agnostic Vault Resolution** | Package `agy-hq` (`0.1.0`) for global install (`~/.gemini/config/plugins/agy-hq/` + `~/.local/bin/hq`), eliminate `sidecars.json` & duplicate `.agents/hooks.json`, add `hq plugin` / `hq version` CLI & `//go:embed` templates, decouple `ResolveVaultDir()` via 5-step global pointer resolution, and scope Front Desk guardrails away from external repos | Implemented |
| [RFC-0016](design/0016-marketplace-distribution-and-binary-bootstrap.md) | **Antigravity Marketplace Distribution, Automated GitHub Release Matrix & Verified Binary Bootstrap** | Prepare `agy-hq` for 1-click Antigravity Marketplace & `agy plugin install` distribution without a Go toolchain: `./scripts/hq-hook.sh` (`0755`) 5-step SHA-256-verified binary bootstrapper, tag-driven GitHub Releases matrix + SLSA provenance + `plugin-release` branch sync, dual-mode `hq upgrade`, and `CUJ-26` hermetic E2E suite | Proposed |

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
- [08 — Empirical Audit of Antigravity Token Telemetry, Prefix Caching & Multi-Agent Attribution](research/08_token_observability_and_telemetry.md)
- [09 — Open-Source Personal Agent OS Strategy: Forensic Privacy Audit, Developer Onboarding & Launch Playbook](research/09_open_source_agent_os_strategy.md)
- [10 — Meta Muse (`muse.ai`) Deep Dive: Personal Context Architecture, Proactive Workflows & Comparative Audit Against `agy-hq`](research/10_muse_ai_proactive_and_context_study.md)
- [11 — Antigravity Global Plugin Packaging & Vault Isolation Audit](research/11_plugin_packaging_audit.md)
- [12 — Antigravity Marketplace Distribution, Repository Layout & Verified Binary Bootstrap Audit](research/12_marketplace_distribution_and_binary_bootstrap.md)
