# Headquarter (`agy-hq`) Engine Repository Instructions

Welcome to the **`agy-hq` (Headquarter)** engine and plugin source repository.

> Core Personal Agent OS operating rules (Vault boundary, `[HQ] Front Desk` vs. regular conversation scoping, silent memory operations, model routing, and `hq propose` 1-tap outbound approvals) are loaded automatically from `.agents/plugins/agy-hq/rules/AGENTS.md`. The rules below govern engineering work on `agy-hq` itself.

## `agy-hq` Contributor & Architecture Invariants

1. **Mandatory `Research -> RFC -> Approval -> Code` Gate for Large Changes:**
   - When adding a major capability, external integration, or architectural subsystem to `agy-hq`, **never jump straight into coding** (even inside a subagent).
   - Start with **Phase 1: Deep Research & Technical RFC (`docs/design/NNNN-<slug>.md` + `docs/research/` + `docs/README.md` index row)** on `deep` tier (`pro` / `gemini-4-carbon-high`) inside an isolated worktree (`hq spawn --repo=. --task=<slug> --depth=high` or `[deep] <Scope>: Research & RFC Architect` subagent).
   - Commit and push Phase 1 RFC/research docs cleanly on `hq/<slug>` before pausing for user review; begin Phase 2 code implementation only after user approval.

2. **Two-Layer Repo Hygiene & Ring-0 Trusted Computing Base:**
   - Keep `go.mod` 100% pure Go stdlib (`0` external dependencies, enforced by CI Gate `H4-A`).
   - Never commit personal identity information (PII), workstation home paths, or `local/` Vault state to this repository (enforced by mathematical PII scanner `P1`–`P6` in `hq ci check-pii`).
   - Maintain SemVer parity between `internal/version/version.go` (`Version`) and `.agents/plugins/agy-hq/plugin.json` (`"version"`), and byte-parity between `.agents/plugins/agy-hq/sidecars/agy-hq-console/main.mjs` and `templates/sidecars/agy-hq-console/main.mjs` (enforced by CI Gate `H2-C`).
   - Keep `.agents/plugins/agy-hq/hooks.json` as the single hook registration file (never recreate a duplicate root `.agents/hooks.json` or `.agents/plugins/agy-hq/sidecars.json`, enforced by `H2-A` and `H2-C`).

3. **Pre-Submit Local CI Gate & Zero-Dirty-Tree Auto-Push Invariant:**
   - Routine git commits and pushes to this repository (`git push origin main` or `git push -u origin hq/<task>`) are **exempt** from `hq propose` approval once:
     1. `go build -o ./bin/hq ./cmd/hq && ./bin/hq ci pre-push` passes `100%` (`P1`–`P6` PII checks, `H1`–`H4B` skill/hook/plugin/Go/Markdown contracts, `agy plugin validate --strict .agents/plugins/agy-hq`, `go vet ./...`, and all unit + CUJ E2E tests).
     2. Pre-commit checks (`./bin/hq ci pre-commit`) pass cleanly.
     3. Feature branches are cleanly rebased onto `origin/main` before merging.
   - Every subagent and task conversation modifying `agy-hq` must ensure `git status -s` is completely clean and pushed to `origin` before ending a turn.
   - After merging plugin or kernel changes into `main`, run `hq upgrade` (or `./bin/hq plugin install`) to keep `~/.local/bin/hq` and `~/.gemini/config/plugins/agy-hq/` synchronized across workspaces.
