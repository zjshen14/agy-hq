# `agy-hq` — Personal Agent OS for Google Antigravity

**`agy-hq`** (Headquarter) is a lightweight, open-source **Personal Agent OS** built on top of **Google Antigravity**. It turns an always-on Mac running Antigravity into a 24/7 general-purpose, proactive personal assistant—while keeping **100% of your personal memory, files, and credentials isolated in a private `Vault` (`local/`)**.

---

## Why `agy-hq`?

Frontier personal agents (Meta Muse, OpenAI `dots`, Claude Duet/Cowork, OpenClaw, Nous Hermes) share six core capabilities:
1. **100% General-Purpose & Open-Ended** (handles any ad-hoc request—coding, writing a post, deep research, planning, or life admin—without boxing you into rigid categories)
2. **Always-On Execution** (runs 24/7 on your always-on Mac desktop)
3. **Silent-by-Default Heartbeat** (0-token background checks in Go that only wake the LLM when something is actionable)
4. **Event Triggers** (`fsnotify` drop-folder watching in `local/inbox/`, RSS/filing monitors, CI checks)
5. **Pocket Access & 1-Tap Approvals** (piggybacking on **Antigravity Remote Control** Web/PWA on Day 1, with pluggable Google Chat / Telegram adapters)
6. **Consolidated Memory & Compounding Skills** (0-token `Stop` turn capture, nightly "sleep-time" memory consolidation, and progressive-disclosure `.agents/skills/*/SKILL.md` workflows)

---

## Two-Layer Architecture: Open-Source Engine vs. Private Vault

```text
agy-hq/                                # LAYER 1: Open-Source Agent OS (This Git Repo)
├── .gitignore                         # Strictly ignores /local/, /bin/, and secrets
├── AGENTS.md                          # General-purpose personal assistant rules
├── cmd/hq/                            # Go CLI: init | hook | daemon | propose | notify | status
├── internal/                          # Go microkernel (hooks, 0-token trigger runner, approvals, Aux UI)
├── templates/                         # Starter blueprints copied by `hq init` into ./local/
├── docs/                              # Product specs, technical RFCs, ADRs & research
├── .agents/
│   ├── hooks.json                     # SessionStart (memory), PreToolUse (action guard), Stop (0-token log)
│   └── skills/                        # Additive, progressive-disclosure SKILL.md workflows
│
└── local/                             # LAYER 2: Your Private Vault (Separate Private Git Repo)
    ├── USER.md                        # Your profile, preferences, writing voice & goals
    ├── MEMORY.md                      # Consolidated long-term memory + topic notes
    ├── triggers.yaml                  # Your cron schedules, 0-token pre-checks & inbox/ drop rules
    ├── inbox/                         # Zero-prompt file drop folder (CSVs, PDFs, notes, receipts)
    ├── workspace/                     # Open workspace for any task (posts, research, projects, notes)
    ├── private/                       # High-PII records (beancount ledger, taxes) with extra guardrails
    └── memory/                        # facts.jsonl, journal/YYYY-MM-DD/<convId>.json, pending/, outbox/
```

---

## Documentation (`docs/`)

All product specs, technical RFCs, Architecture Decision Records (ADRs), and frontier research live in **[`docs/README.md`](docs/README.md)**:

- **System Overview:** [Architecture (`docs/architecture.md`)](docs/architecture.md) · [Roadmap (`docs/roadmap.md`)](docs/roadmap.md)
- **Product Designs (`docs/product/`):**
  - [PRD-0001: Vision & Day-in-the-Life UX](docs/product/0001-vision-and-ux.md)
  - [PRD-0002: Open-Ended Workflows & Additive Skills](docs/product/0002-open-workflows-and-skills.md)
- **Technical Designs (`docs/design/`):**
  - [RFC-0001: Two-Layer Vault Split & Per-Task Parallel Isolation](docs/design/0001-vault-and-task-isolation.md)
  - [RFC-0002: Zero-Token Journaling & Sleep-Time Memory Consolidation](docs/design/0002-memory-and-consolidation.md)
  - [RFC-0003: Action-Based Safety, Quarantined Readers & Approval-Bound Execution](docs/design/0003-approval-bound-execution.md)
  - [RFC-0004: Silent Gatekeeper, Quota Circuit Breaker & Channel Adapters](docs/design/0004-heartbeat-and-channels.md)
- **Key Decisions (`docs/decisions/`):**
  - [ADR-0001: Option 1.5 Hybrid on Antigravity](docs/decisions/0001-option-1.5-hybrid-on-antigravity.md)
  - [ADR-0002: Go Microkernel Binary (`bin/hq`)](docs/decisions/0002-go-microkernel-binary.md)
  - [ADR-0003: Two-Layer OSS vs. Private Vault Split](docs/decisions/0003-two-layer-oss-and-private-vault.md)
  - [ADR-0004: Remote Control First + Pluggable Channels](docs/decisions/0004-remote-control-first-pluggable-channels.md)
  - [ADR-0005: Zero-Token Stop Hook vs. Blocking Gate](docs/decisions/0005-zero-token-stop-hook-vs-blocking-gate.md)
  - [ADR-0006: General-Purpose Agent + Additive Skills Over Rigid Domain Packs](docs/decisions/0006-general-agent-and-skills-over-packs.md)
- **Research Archive (`docs/research/`):**
  - [01 — Product Landscape](docs/research/01_products.md) · [02 — Academic Frontier](docs/research/02_academic.md) · [03 — Technical Options](docs/research/03_technical.md) · [04 — Antigravity Audit](docs/research/04_antigravity_audit.md)
