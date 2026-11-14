# `agy-hq` (Headquarter) — Personal Agent OS Plugin for Google Antigravity

**`agy-hq`** turns Google Antigravity into a 24/7 general-purpose, proactive Personal Agent OS across **every** workspace on your machine—while keeping **100% of your personal memory, files, and credentials isolated in a private `Vault`** (`~/.gemini/antigravity/hq/vault` or `<checkout>/local/`).

## Installation

From an `agy-hq` checkout, install `~/.local/bin/hq` on `PATH` and sync the global Antigravity plugin bundle (`~/.gemini/config/plugins/agy-hq/`):

```sh
./scripts/bootstrap.sh --global
# or directly:
./bin/hq plugin install
```

Verify installation and bundle/binary version sync at any time:

```sh
hq version
hq plugin status
hq doctor
```

## Bundled Customizations

- **Global OS Rules (`rules/AGENTS.md`):** Role-scoped `[HQ] Front Desk` coordinator vs. unrestricted repository coding sessions, private Vault boundary, model routing, and mandatory `hq propose` payload previews.
- **Zero-Token Lifecycle Hooks (`hooks.json`):** Automatic `SessionStart` Vault context injection, `PreToolUse` approval/credential/quarantine guardrails, and `Stop` ground-truth turn journaling.
- **9 Progressive-Disclosure Skills (`skills/*/SKILL.md`):** `morning-brief`, `inbox-triage`, `gchat`, `swe-pulse`, `sleep-consolidate`, `thesis-check`, `filing-diff`, `ledger-ingest`, and `tax-prep`.
- **Auto-Discovered HQ Console Sidecar (`sidecars/agy-hq-console/sidecar.json`):** Live `:4242` Aux Pane control console for approvals, Task Board, token observability, and schedule-aware Spaces.
