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
| [RFC-0016](design/0016-sidebar-conversation-archive-sync.md) | **Antigravity Sidebar Conversation Archive Synchronization** | Synchronize `hq archive`, `hq tasks compact`, `/sleep-consolidate`, and `hq reopen` with Antigravity's sidebar via ConnectRPC (`GetAllCascadeTrajectories` + `UpdateConversationAnnotations`), 7-gate Front Desk/pin/activity safety guards, durable offline retry queue, and `hq tasks sync-sidebar --backfill` | Implemented |
| [RFC-0017a](design/0017a-connector-protocol-and-watcher-engine.md) | **Modular Connector Protocol, CLI Transport Profiles & 0-Token Watcher Engine** | Split `connector.json` (pinned code) vs. `instance.json` (account data), shared `transport.kind: "cli"` unifying `hq gmail` & 1-step `hq gchat propose` (`auto_install_plugin: false`), `hq-connector-protocol/v1` + `jsonmap`, scrubbed subprocess pool, locked `internal/store`, paginated `events.Enqueue` (`Metadata["tier"]`), and single-authority `hq watch ack` + `PoisonedItems` | Implemented |
| [RFC-0017b](design/0017b-provenance-taint-and-offer-gateway.md) | **Provenance-Based Taint Propagation, Human-Presence Approval Proof & Offer Gateway** | Lease-time `[hq-spawn:<token>]` `SessionStart` taint binding, fail-closed `O_EXCL` taint files, transcript-verified HMAC human-presence capability on `hq approve` + all-session `private/trust.json` guard, tainted `hq timers set` lockdown, nonce-fenced `QuarantinedThread`, hardened `hq offer stage` (RFC 5322 authenticated-participant lock, `Supersede`, `inspect_thread` precondition), and nonce-fenced system cards | Implemented |
| [RFC-0017c](design/0017c-lease-based-morning-brief-and-extensions.md) | **Non-Destructive Lease-Based Morning Brief Collector & Structured Extensions** | Idempotent `hq brief collect` peek + two-phase `--lease` / `hq brief commit` after file persistence, `< 4h` expiring-proposal warnings, provenance-safe tainted reader JSON summarization (zero raw quarantined text in Front Desk), and structured `brief.d` / pack `hq-brief` extensions (`quarantined: true` default) | Implemented |
| [RFC-0017d](design/0017d-enterprise-extensibility-and-portability.md) | **Companion Extension Packs, Unified Trust Registry & Host Portability** | Companion plugin `hq-extension.json` (`plugin.json` untouched for `--strict`), two-lattice virtual merge + `<vault>/extensions.json` (`hq triggers pause|resume`), unified `private/trust.json` (`hq` pinned by `(version, commit)`), `hq dispatch resolve` + `SKILL.md` trampolines + role-scoped `front_desk_overlays`, `routing.ResolveTier`, `internal/hostpaths` + canonical `BrainDir` guard, Go-native `hq sidecar console`, and hook-piggybacked Desktop-SSO polling | Implemented |
| [RFC-0018](design/0018-always-on-agent-runtime-hardening.md) | **Always-On Agent Runtime Hardening — Durable Event Inbox, Self-Scheduled Follow-Up Timers, Deferred Approval Resumption, Cross-Run Progress/Reported Memory & OS Credential Sandboxing** | Anthropic always-on benchmark upgrades: unified durable Event Inbox (`pending` → `leased` → `done`/`dropped`) + single-worker scope locks, agent-set `Follow-up` timers (`hq timers`), closed-loop deferred approval/rejection resumption + scrubbed `CommandArgv`, `reported.jsonl` + `progress/<slug>.md` + lean static-first `SessionStart`, optional `bwrap`/`sandbox-exec` credential jail, and `hq outbox eval` | Implemented |
| [RFC-0019](design/0019-two-tier-rsi-vault-overlay.md) | **OS vs. User-Space Skill Boundary, Vault Skills (`local/skills/`) & Two-Tier RSI Overlays** | Slim `.agents/plugins/agy-hq/skills/` to 3 Core OS skills (`inbox-triage`, `morning-brief`, `sleep-consolidate`), move 6 domain/channel starters (`gchat`, `swe-pulse`, `ledger-ingest`, `tax-prep`, `thesis-check`, `filing-diff`) to `templates/skills/` → `<vault>/skills/` via `.agents/skills.json`, `hq skill`, & `hq gchat login` auto-seeding, add inline `hq dispatch resolve` overlays (`<vault>/memory/rsi/rules/`), fix `"swe-pulse"` fallback to `general.md`, and migrate tracked rules cleanly | Implemented |
| [RFC-0020](design/0020-proactive-ambient-synthesis-and-mobile-push.md) | **Cross-Source Ambient Synthesis Loop, Pre-Meeting / Deadline Nudges & Optional Zero-Body Mobile Push** | Muse-style 0-token `hq pulse precheck` joining upcoming `gcalendar` meetings (`T-30m`), expiring `proposals` (`<2h` TTL), unsent drafts, overdue `WaitingOn` SLAs, and approaching Goal `targetDate` deadlines into actionable Front Desk `interrupt` cards, plus opt-in zero-body mobile push | Proposed |
| [RFC-0021](design/0021-customizable-approval-policy-and-ux.md) | **Customizable Outbound Approval Policy, Native Coexistence & Lifecycle Hardening** | Uniform data-driven `approvals.rules` in `<vault>/config.yaml` + `hq-extension.json` `approval_rules`, `third_party_only` vs. `owned_repos` resolution, deterministic non-no-op CLI guard (no dummy `echo`), `24h` `ask_ttl` + Issue `#28` idempotent `VerifyAskAndMintCap` + `15s` hook timeout, surgical multi-question `ask_question` options, `(ConversationID, ActionType)` auto-supersede, `--dismiss` wakeup suppression, and 0-token `Reject` / offline git ref reconciliation | Implemented |
| [RFC-0022](design/0022-outgoing-phone-call.md) | **Outgoing Customer-Service Phone Calls — Sovereign PSTN Voice Bridge, Approval-Bound Call Plans, Vault Secret Isolation & Human Takeover** | Pure-stdlib Ring-0 PSTN media bridge (`Telnyx` / `Twilio` + `Gemini Live` / `OpenAI Realtime` over RFC 6455 WSS + ephemeral capability ingress), `mode: always` `hq call propose` SHA-256 `CallPlan` gate, E.164 emergency/toll-fraud firewall, 3-tier `local/private/calls/` DTMF/redacted secret isolation, 0-token `HOLD_WAIT` music gatekeeper (`3s` pre-roll buffer), and `:4242` live whisper + mobile PSTN conference takeover | Proposed |

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
- [12 — Antigravity Sidebar Conversation Archive Synchronization Audit](research/12_conversation_archive_sync_audit.md)
- [13 — General Watcher Mechanism, Quarantine Provenance & Pluggable Enterprise Connector Audit](research/13_proactive_channel_watcher_audit.md)
- [14 — Anthropic Always-On Agents Architecture & Forensic Gap Audit Against `agy-hq`](research/14_anthropic_always_on_agents_gap_audit.md)
- [15 — Two-Tier Recursive Self-Improvement, OS vs. User-Space Skill Boundary & Personal Vault Skill Overlay Audit](research/15_two_tier_rsi_vault_overlay.md)
- [16 — Empirical Audit of Outbound Approval UX Friction, Native Antigravity Coexistence & Customizable Policy Design](research/16_approval_ux_and_customizable_policy_audit.md)
- [17 — Outgoing Customer-Service Phone Call Architecture: External Telephony, Real-Time Voice Pipelines, Sovereign Vault Privacy & Safety Audit](research/17_outgoing_phone_call_audit.md)
