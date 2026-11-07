# `agy-hq` — Personal Agent OS for Google Antigravity

**`agy-hq`** (Headquarter) is a lightweight, open-source **Personal Agent OS** built on top of **Google Antigravity**. It turns an always-on Mac running Antigravity into a 24/7 proactive personal assistant while keeping **100% of your personal memory, files, and credentials isolated in a private `Vault` (`local/`)**.

---

## Why `agy-hq`?

Frontier personal agents (Meta Muse, OpenAI `dots`, Claude Duet/Cowork, OpenClaw, Nous Hermes) share six core capabilities:
1. **Always-On Execution**
2. **Silent-by-Default Heartbeat** (0-token background checks that only wake the LLM when actionable)
3. **Event Triggers** (`fsnotify` drop-folder watching, RSS/filing monitors, CI checks)
4. **Pocket Access & 1-Tap Approvals** (piggybacking on **Antigravity Remote Control** Web/PWA on Day 1, with pluggable Google Chat / Telegram adapters)
5. **Consolidated Long-Term Memory** (scoped `SessionStart` injection, 0-token `Stop` transcript capture, and nightly "sleep-time" consolidation)
6. **Parallel, Isolated Extension Packs** (`packs/<id>/pack.yaml`)

`agy-hq` delivers all six using a single dependency-free Go microkernel (`bin/hq`) wired into Antigravity's native **Lifecycle Hooks**, **Daemon Sidecars**, and **Skills**.

---

## Two-Layer Architecture: Open-Source Engine vs. Private Vault

```text
agy-hq/                                # LAYER 1: Open-Source Agent OS (This Git Repo)
├── .gitignore                         # Strictly ignores /local/, /bin/, and secrets
├── AGENTS.md                          # System rules (dynamically mounts ./local/ Vault)
├── cmd/hq/                            # Go CLI: init | hook | daemon | propose | notify | status
├── internal/                          # Domain-agnostic Go microkernel packages
├── packs/                             # Open-Source Extension Packs (schemas + SKILL.md only)
│   ├── swe/                           # Side-project engineering & PR/CI pulse
│   ├── invest/                        # SEC EDGAR watchers & thesis-driven equity research
│   └── admin/                         # Quarantined inbox triage, Beancount ledger & tax prep
├── templates/                         # Blueprints copied by `hq init` into ./local/
├── docs/                              # Architecture & frontier research reports
│
└── local/                             # LAYER 2: Your Private Vault (Separate Private Git Repo)
    ├── config.yaml                    # Enabled packs, channel settings, quota budgets
    ├── USER.md                        # Personal profile & preferences
    ├── MEMORY.md                      # Consolidated long-term memory
    ├── inbox/                         # Zero-prompt file drop folder (CSVs, PDFs, receipts)
    ├── memory/                        # facts.jsonl, journal/, pending/ approvals, outbox/
    └── packs/{swe,invest,admin}/      # Your private repos, watchlist/theses, ledger & tax files
```

---

## Documentation (`docs/`)

All product specs, technical RFCs, Architecture Decision Records (ADRs), and frontier research live in **[`docs/README.md`](docs/README.md)**:

- **System Overview:** [Architecture (`docs/architecture.md`)](docs/architecture.md) · [Roadmap (`docs/roadmap.md`)](docs/roadmap.md)
- **Product Designs (`docs/product/`):**
  - [PRD-0001: Vision & Day-in-the-Life UX](docs/product/0001-vision-and-ux.md)
  - [PRD-0002: SWE Starter Pack](docs/product/0002-pack-swe.md) · [PRD-0003: Investment Research Pack](docs/product/0003-pack-invest.md) · [PRD-0004: Life Admin, Ledger & Tax Pack](docs/product/0004-pack-admin.md)
- **Technical Designs (`docs/design/`):**
  - [RFC-0001: Two-Layer Vault Split & Parallel Pack Isolation](docs/design/0001-vault-and-pack-isolation.md)
  - [RFC-0002: Zero-Token Journaling & Sleep-Time Memory Consolidation](docs/design/0002-memory-and-consolidation.md)
  - [RFC-0003: Safety, Quarantined Readers & Approval-Bound Execution](docs/design/0003-approval-bound-execution.md)
  - [RFC-0004: Silent Gatekeeper, Quota Circuit Breaker & Channel Adapters](docs/design/0004-heartbeat-and-channels.md)
- **Key Decisions (`docs/decisions/`):**
  - [ADR-0001: Option 1.5 Hybrid on Antigravity](docs/decisions/0001-option-1.5-hybrid-on-antigravity.md)
  - [ADR-0002: Go Microkernel Binary (`bin/hq`)](docs/decisions/0002-go-microkernel-binary.md)
  - [ADR-0003: Two-Layer OSS vs. Private Vault Split](docs/decisions/0003-two-layer-oss-and-private-vault.md)
  - [ADR-0004: Remote Control First + Pluggable Channels](docs/decisions/0004-remote-control-first-pluggable-channels.md)
  - [ADR-0005: Zero-Token Stop Hook vs. Blocking Gate](docs/decisions/0005-zero-token-stop-hook-vs-blocking-gate.md)
- **Research Archive (`docs/research/`):**
  - [01 — Product Landscape](docs/research/01_products.md) · [02 — Academic Frontier](docs/research/02_academic.md) · [03 — Technical Options](docs/research/03_technical.md) · [04 — Antigravity Audit](docs/research/04_antigravity_audit.md)
