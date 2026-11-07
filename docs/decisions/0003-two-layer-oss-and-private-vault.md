# ADR-0003: Separate Open-Source Engine (`agy-hq`) from Private User Vault (`local/`)

- **Status:** Accepted
- **Date:** 2026-11-07
- **References:** [RFC-0001](../design/0001-vault-and-task-isolation.md)

---

## Context

`agy-hq` is designed to be open-sourced and shared so others can use the framework and contribute generic `.agents/skills/` workflows. At the same time, running a personal assistant produces sensitive personal state (user profile, emails, investment theses, bank ledgers, tax workpapers, and conversation journals) that must never leak into the public git history.

## Decision

Split the workspace into two strictly separated git layers:
1. **Layer 1 (`agy-hq` Root Git Repository — Public-Safe):** Contains only the domain-agnostic Go microkernel (`cmd/hq/`, `internal/`), generic `.agents/skills/*/SKILL.md` workflows, starter templates (`templates/`), and documentation (`docs/`).
2. **Layer 2 (`local/` or `$HQ_VAULT_DIR` — Private User Vault):** Ignored by the root `.gitignore`, enforced by a root `.git/hooks/pre-commit` guard, and initialized by `hq init` as its own independent private git repository. All personal config (`config.yaml`, `triggers.yaml`, `USER.md`, `MEMORY.md`), runtime logs (`memory/`), drop files (`inbox/`), open-ended project workspace (`workspace/`), and high-PII files (`private/`) live exclusively inside `local/`.

## Consequences

- **Positive:** The root `agy-hq` repository can be pushed to public GitHub at any time with zero risk of leaking personal data; any user can clone `agy-hq` and run `bin/hq init` to bootstrap their own private Vault; personal memories, project files, and ledgers remain version-controlled in their own private git history.
- **Negative:** User data writes must target `local/` rather than the root engine directories (enforced automatically by `hq hook pre-tool`).
