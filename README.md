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

## Documentation

- **[Product & Architecture Design (`docs/architecture.md`)](docs/architecture.md)**
- **Research Reports (`docs/research/`):**
  - [01 — Frontier Personal Agent Product Landscape](docs/research/01_products.md)
  - [02 — Academic & Research Frontier](docs/research/02_academic.md)
  - [03 — Technical Build Options & Integrations](docs/research/03_technical.md)
  - [04 — Google Antigravity Capability Audit](docs/research/04_antigravity_audit.md)
