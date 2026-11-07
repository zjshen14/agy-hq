# RFC-0001: Two-Layer Vault Split & Parallel Pack Isolation

- **Status:** Approved
- **Related Docs:** [ADR-0003](../decisions/0003-two-layer-oss-and-private-vault.md), [architecture.md](../architecture.md)

---

## 1. Objective

1. Keep the top-level `agy-hq` architecture **100% domain-agnostic** and **open-source safe** (zero personal data in tracked files).
2. Allow arbitrary **Extension Packs** (`swe`, `invest`, `admin`, or custom packs) to **run in parallel without interfering with each other**.

---

## 2. Two-Layer Filesystem Contract

- **Layer 1 (`agy-hq` Root Repo — Public-Safe):** Contains `cmd/hq/`, `internal/`, `packs/<id>/pack.yaml`, `packs/<id>/skills/*/SKILL.md`, and `templates/`.
- **Layer 2 (`local/` or `$HQ_VAULT_DIR` — Private Git Repo):** Ignored by the root `.gitignore` and protected by a root `.git/hooks/pre-commit` check. Holds `local/config.yaml`, `local/USER.md`, `local/MEMORY.md`, `local/inbox/`, `local/memory/`, and `local/packs/<id>/`.
- **Bootstrap (`hq init`):** Copies `templates/` into `local/`, initializes `local/.git`, and symlinks enabled pack skills into `.agents/skills/`.

---

## 3. Extension Pack Manifest (`packs/<id>/pack.yaml`)

```yaml
id: invest
name: "Investment Research"
description: "SEC filing watchers, thesis tracking, and citation-verified equity analysis"
concurrency:
  max_parallel_runs: 2          # Max concurrent sessions for this pack
  daily_spawn_budget: 5         # Independent daily spawn quota
  default_model: flash
triggers:
  schedules:
    - id: edgar-close-watch
      cron: "CRON_TZ=America/Los_Angeles 30 13 * * 1-5"
      precheck_cmd: ["hq", "check-rss", "--config", "local/packs/invest/watchlist.yaml"]
      skill: invest-thesis-check
  inbox_match:
    extensions: [".pdf", ".htm", ".html"]
    filename_regex: "(?i)(10-[kq]|8-k|earnings|prospectus)"
    skill: invest-filing-ingest
isolation:
  vault_subpath: "local/packs/invest"
  access_grants:
    - "read_file(<workspace>/agy-hq/local/packs/invest)"
    - "write_file(<workspace>/agy-hq/local/packs/invest)"
    - "mcp(sec-edgar/*)"
    - "read_url(data.sec.gov)"
```

---

## 4. Four Parallel Isolation Guarantees

1. **Context & Memory Compartmentalization (`SessionStart` Hook):**
   - Inspects the session's `[pack:<id>]` tag (or matches `lastUserInput` against pack descriptions) and injects **only** `local/USER.md` + `local/packs/<id>/MEMORY.md`. Private data in `admin` is never loaded into `swe` or `invest` contexts.
2. **Filesystem Boundary Enforcement (`PreToolUse` Hook):**
   - Verifies that file reads/writes stay within the active pack's `vault_subpath` (`local/packs/<id>/**`).
3. **Lock-Free, Immutable File Writes (Zero Race Conditions):**
   - Parallel sessions **never edit shared files in place**:
     - Turn journals write to `local/memory/journal/YYYY-MM-DD/<convId>-<stepIdx>.json`.
     - Staged approvals write to `local/memory/pending/<uuid>.json`.
     - Notifications write to `local/memory/outbox/<uuid>.json`.
     - Parallel coding tasks use isolated git worktrees (`Workspace: "share"` / `"branch"`).
   - Only the single-writer **Nightly Consolidation** job merges the day's immutable files into `local/MEMORY.md` and `local/packs/<id>/MEMORY.md`.
4. **Independent Concurrency & Quota Lanes:**
   - `hq daemon` maintains per-pack worker semaphores (`max_parallel_runs`) and `daily_spawn_budget` counters so a burst of work in one pack never starves another.
