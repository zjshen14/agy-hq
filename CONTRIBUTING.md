# Contributing to `agy-hq`

Thank you for contributing to **`agy-hq`**! Our goal is to build a rock-solid, zero-bloat, privacy-first **Personal Agent OS** on Google Antigravity.

By participating in this project, you agree to abide by our [Code of Conduct](CODE_OF_CONDUCT.md), and any contributions you submit are licensed under the [Apache License 2.0](LICENSE).

---

## Core Architectural Invariants

Every pull request is verified mechanically by `./bin/hq ci pre-push` and GitHub Actions (`.github/workflows/ci.yml`). Before writing code or skills, keep these five rules in mind:

1. **Zero External Go Dependencies (`Gate H4-A`):**
   - [`go.mod`](go.mod) must contain **zero** `require` directives. The `hq` microkernel (`cmd/hq/`, `internal/`) relies strictly on the Go standard library (`1.24+`) so every user can compile the entire OS locally in `< 2 seconds` with zero supply-chain risk.
2. **Airtight Two-Layer Vault & Zero PII (`Invariants P1–P6`):**
   - Never include real workstation paths (`/Users/<realname>/`, `/home/<realname>/`), personal email addresses, real Google Chat `spaces/<id>` literals, or personal contact/family names in code, docs, or tests.
   - Always use synthetic fixtures: `/Users/username/`, `alice@example.com` / `user@gmail.com`, `spaces/AAAA_LAUNCH_01`, and `ACME` / `Sample Corp`.
   - Tip: Add your own personal names or project slugs to `local/private/pii-denylist.txt` (git-ignored) so `./bin/hq ci pre-commit` catches accidental mentions automatically on your machine.
3. **Progressive-Disclosure Skill Budget (`Gate H1`):**
   - `.agents/plugins/agy-hq/skills/` is strictly reserved for the 3 Core OS skills (`inbox-triage`, `morning-brief`, `sleep-consolidate`), while opt-in starter skills live in `templates/skills/<name>/SKILL.md` (installed into `<vault>/skills/` via `hq skill add` and discovered via `.agents/skills.json`).
   - Every `SKILL.md` file must have valid `name` + `description` YAML frontmatter, remain **`<= 150 lines`** (`rsi.MaxSkillMarkdownLines = 150`), and keep `0` tracked `rsi:rule` blocks (`<= 25` total skills, `<= 2,500` frontmatter tokens).
4. **Research → RFC → Code for Architectural Features (`Gate H4-B`):**
   - Non-trivial subsystems require a technical RFC in `docs/design/NNNN-<slug>.md` (and optional research note in `docs/research/`) indexed in [`docs/README.md`](docs/README.md).
   - All Markdown files must have balanced fenced code blocks and valid relative links pointing to git-tracked files.
5. **Ring 0 Trusted Computing Base (`internal/rsi/tcb.go`):**
   - Changes touching `internal/approvals/`, `internal/ci/`, `internal/events/`, `internal/plugin/`, `internal/rsi/{tcb,verifier,proposer}.go`, `internal/sandbox/`, `internal/timers/`, `internal/version/`, `internal/webui/`, `internal/vault/vault.go`, `.agents/plugins/agy-hq/{plugin.json,hooks.json,rules/,sidecars/}`, `.gemini/`, `.github/`, `.gitignore`, `.git/hooks/`, `LICENSE`, `SECURITY.md`, `local/.gitignore`, `scripts/bootstrap.sh`, or `templates/{embed.go,gitignore.tmpl,sidecars/}` are protected from automated self-modification and undergo extra maintainer review.

---

## Local Development Workflow

### 1. Clone & Bootstrap (with Demo Data)
```sh
git clone https://github.com/<your-fork>/agy-hq.git headquarter
cd headquarter
./scripts/bootstrap.sh --demo
```

### 2. Develop in an Isolated Worktree
Just as `[HQ] Front Desk` isolates tasks in git worktrees, we recommend developing features on scoped `hq/<slug>` branches:
```sh
./bin/hq worktree add . my-feature
cd local/worktrees/headquarter-my-feature
```

### 3. Build & Run the Full Local CI Gate
Always stage new Markdown/source files (`git add`) before running the CI gate so `Gate H4-B` recognizes new tracked link targets:
```sh
go build -o ./bin/hq ./cmd/hq
./bin/hq ci pre-push
```

What `./bin/hq ci pre-push` verifies in ~2 seconds:
- **Step 1:** Mathematical PII & Vault boundary audit (`Invariants P1–P6`) across all tracked files.
- **Step 2:** Harness contract gates (`H1` Skills `<= 150` lines & Core OS / `templates/skills/` boundary, `H2-A`/`H2-B`/`H2-C` Hook parity, `PreToolUse` security matrix, & SemVer/bundle parity, `H3` Model routing, inbox classifier & trigger integrity, `H4-A` `gofmt -s` + zero external `go.mod` deps, `H4-B` Markdown links & `docs/README.md` index completeness, `H5-A`–`H5-F` anti-slop, sidecar parity, CUJ doc parity & monolith LOC ceilings).
- **Step 3:** `go vet ./...` and the full unit + CUJ E2E suite (`CUJs 1–47`, `49` `TestCUJ*` tests via `go test ./...`).

### 4. Commit Message Convention
Follow Conventional Commits:
- `feat(<scope>): ...` (e.g., `feat(gchat): ...`, `feat(obs): ...`)
- `fix(<scope>): ...`
- `docs(<scope>): ...`
- `test(<scope>): ...`
- `chore(<scope>): ...`
