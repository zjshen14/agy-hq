# Headquarter (`hq`): Product & Architecture Design

> **One-Line Summary:** **`Headquarter` (`hq`)** is a lightweight, open-source "Personal Agent OS" built on top of **Google Antigravity**. It turns an always-on Mac running Antigravity into a 24/7 proactive personal assistant—while keeping **100% of your personal memory, files, and credentials isolated in a private `Vault` (`local/`)**.

**Supporting Research & Reviews:**
- [01 — Frontier Product Landscape](research/01_products.md) · [02 — Academic & Research Frontier](research/02_academic.md) · [03 — Technical Build Options](research/03_technical.md) · [04 — Antigravity Capability Audit](research/04_antigravity_audit.md)

---

## 1. The Big Picture: Why "Option 1.5"?

The newest generation of personal agents—**Meta Muse**, **Manus**, **OpenAI `dots`**, **Anthropic Claude Duet / Cowork**, **OpenClaw**, and **Nous Hermes**—all feel powerful for the same six reasons:
1. **They are always on** (they don't die when you close your laptop).
2. **They have a silent heartbeat** (they check on things in the background, but stay quiet unless something actually needs your attention).
3. **They react to events** (a file dropped in a folder, an urgent email, a new SEC filing, a broken build).
4. **They live in your pocket** (you can read briefs, delegate tasks, and tap **Approve / Reject** from your phone).
5. **They remember and consolidate** (they don't start from scratch every conversation).
6. **They compound skills** (when you teach them a workflow once, it becomes a reusable skill).

### How We Get All Six Without Building a Massive Framework

Antigravity *already* gives us best-in-class "hands" (coding, terminal sandbox, real-Chrome browser automation, subagents, MCP servers, and scheduled cron jobs) plus **Remote Control** (a mobile web/PWA interface with push notifications).

Instead of building a heavy standalone agent server from scratch, we use **"Option 1.5"**:
- We run the **Antigravity Desktop App** on your **always-on Mac desktop** (with *"Keep computer awake"* and *"Run in background"* turned on).
- We add **one small, dependency-free Go binary (`bin/hq`)** and a structured **Markdown memory vault** inside `<workspace>/agy-hq`.
- `bin/hq` plugs into Antigravity's native **Lifecycle Hooks** and **Daemon Sidecars** to handle the four things Antigravity doesn't do out of the box:
  1. **Automatic memory injection & journaling** across conversations.
  2. **Zero-token background pre-checks** (so we only wake up the LLM when something actually changed).
  3. **Parallel, isolated Extension Packs** so different domains (coding, investing, life admin) never step on each other.
  4. **Deterministic approval gates** so the LLM can never send an email, merge a PR, or touch money without your click.

---

## 2. Day-in-the-Life UX: How You Interact With `Headquarter`

You interact with one unified system across **four natural touchpoints**:

### 📱 1. From Your Phone or Anywhere (`Antigravity Remote Control` First, Chat Apps Later)
Because Antigravity's built-in **Remote Control (`antigravity.google`)** is already enabled on your account (`user@gmail.com`), you can install it to your phone's home screen as a web app (PWA) on Day 1—with **zero custom tunnel setup**:
- **07:30 AM — Morning Brief:** You get a push notification on your phone. Opening it shows a clean Morning Brief artifact summarizing:
  - **Admin:** Today's calendar + 1 urgent email from your CPA (with a drafted reply ready to approve).
  - **Investments:** `NVDA` filed an 8-K after hours; 3-bullet thesis impact summary with page citations.
  - **Side Projects:** Nightly CI passed on `project-alpha`; PR #18 is ready.
- **Silent by Default All Day:** Every 30 minutes, `hq daemon` runs lightweight checks in Go (costing **0 LLM tokens**). If nothing urgent happened, your phone stays completely quiet.
- **On-the-Go Delegation & Approvals:** From the Remote Control web UI on your phone, you can start or continue any conversation in `headquarter`, use slash commands (`/thesis-check`, `/swe-pulse`), and tap native **Approve / Proceed** buttons when an agent stages an action.
- **Future Chat Tunnels (Phase 2):** When you want messages inside **Google Chat** or **Telegram**, `bin/hq` has a pluggable `ChannelAdapter` ready to mirror briefs and approval cards there without changing anything else.

### 🖥️ 2. At Your Desk (`Antigravity App` + Right-Hand `HQ Console`)
When you sit at your Mac desktop, you use the standard Antigravity App:
- **Left Sidebar:** Every background run (Morning Briefs, filing watchers, inbox file drops) shows up as a normal conversation in your history.
- **Center Chat:** Every time you chat, `hq` automatically loads your `USER.md` profile and the relevant pack's memory. When you finish, `hq` logs a structured summary of the turn in the background.
- **Right Side Pane (`[Open HQ Console](sidecar://hq/dashboard)`):** Clicking the pill opens a clean, single-page local web panel inside Antigravity's right-hand pane showing:
  - **Pending Approvals Queue** (exact diffs/payloads with 1-click `[Approve]` / `[Reject]`).
  - **Active Watchers & Daily Quota Meter** (how many background runs have fired today).

### 📂 3. Zero-Prompt Drop Folder (`local/inbox/`)
Whenever you download a bank CSV, PDF receipt, W-2, or 10-K on your Mac, you just drag it into `local/inbox/`—no prompting needed:
- `hq daemon` spots the new file instantly, matches it against your installed packs, and spawns the right workflow (e.g., categorizes bank transactions into your `beancount` ledger, verifies the math with `bean-check`, and queues the diff for your approval).

### 🌙 4. Nightly "Sleep-Time" Consolidation (11:30 PM)
While you sleep, a scheduled Flash job reviews the day's turn logs:
- Updates `local/MEMORY.md` and per-pack memories with new durable facts (using small delta patches, never overwriting the whole file).
- If you corrected the agent or repeated a workflow during the day, it drafts a proposed `SKILL.md` improvement into `local/memory/proposals/` for you to approve the next morning.
- Commits your private `local/` vault to git.

---

## 3. Two-Layer Architecture: Open-Source `Headquarter` vs. Private `Vault`

You want to be able to **open-source the framework and share it with others**, while keeping **your personal setup, memories, holdings, and tax/ledger data 100% private**.

To make that effortless, the workspace is split into two strictly separated git layers—just like Obsidian (app + plugins vs. your personal vault):

```mermaid
flowchart LR
  subgraph OSS["Layer 1: Headquarter (Public-Safe Root Git Repo)"]
    direction TB
    K["cmd/hq/ & internal/\nDomain-agnostic Go microkernel:\nDispatcher, Hooks, Approvals, Aux UI, Channels"]
    P["packs/{swe,invest,admin}/\nOpen-source Pack Specs:\npack.yaml schemas & generic SKILL.md workflows"]
    T["templates/\nStarter blueprints copied by `hq init`"]
  end

  subgraph Vault["Layer 2: The Vault (`local/` — Separate Private Git Repo)"]
    direction TB
    CFG["local/config.yaml & local/USER.md\nYour account, enabled packs, personal profile"]
    MEM["local/MEMORY.md & local/memory/\nYour facts.jsonl, journals, pending approvals, outbox"]
    PDATA["local/packs/{swe,invest,admin}/\nYour repos, watchlist & theses, beancount ledger, taxes"]
    PCUST["local/packs/<custom-pack>/\nAny private pack you build just for yourself"]
  end

  OSS -- "`hq init` scaffolds &\nmounts Vault at runtime" --> Vault
```

### Directory Layout (`<workspace>/agy-hq`)

```text
headquarter/                           # LAYER 1: Open-Source Agent OS (Public Git Repo)
├── .gitignore                         # Ignores /local/, /bin/, and *.secret*
├── AGENTS.md                          # Generic system rules (dynamically mounts ./local/)
├── go.mod                             # Go module for bin/hq
├── cmd/hq/                            # CLI: init | hook | daemon | propose | notify | status
├── internal/                          # Domain-agnostic Go packages (zero personal data)
│   ├── kernel/                        # Pack registry, parallel dispatcher, quota circuit breaker
│   ├── hooks/                         # SessionStart, PreToolUse guard, Stop transcript parser
│   ├── approvals/                     # SHA-256 pending queue & deterministic CLI executor
│   ├── channels/                      # Pluggable adapter (Remote Control default; GChat/Telegram ready)
│   └── webui/                         # Single-page Aux Console (sidecar://hq/dashboard)
├── packs/                             # Open-Source Extension Packs (reusable skills + schemas only)
│   ├── swe/                           # pack.yaml + skills/swe-project-pulse/SKILL.md
│   ├── invest/                        # pack.yaml + skills/invest-thesis-check/SKILL.md
│   └── admin/                         # pack.yaml + skills/admin-{inbox,ledger,tax}/SKILL.md
├── templates/                         # Starter files copied into ./local/ when someone runs `hq init`
├── .agents/
│   ├── hooks.json                     # Connects Antigravity lifecycle hooks to ./bin/hq
│   └── skills/                        # Core skills (morning-brief, sleep-consolidate) + pack symlinks
│
└── local/                             # LAYER 2: Your Private Vault (Independent Private Git Repo)
    ├── .git/                          # Backed up to your private git remote (never public)
    ├── config.yaml                    # Your settings, enabled packs, daily quota budgets
    ├── USER.md                        # Your profile, preferences, timezone
    ├── MEMORY.md                      # Your consolidated global memory (<2,500 tokens)
    ├── inbox/                         # Your drag-and-drop folder (CSVs, PDFs, receipts)
    ├── memory/
    │   ├── facts.jsonl                # Time-stamped facts: {"entity","fact","valid_from","valid_to"}
    │   ├── interventions.jsonl        # Tracks which proactive pings you accepted vs. dismissed
    │   ├── journal/YYYY-MM-DD/        # Immutable per-turn logs (<convId>.json)
    │   ├── pending/                   # Staged actions awaiting your approval (<uuid>.json)
    │   ├── outbox/                    # Outbound briefs & notifications (<uuid>.json)
    │   └── proposals/                 # Nightly proposed SKILL.md updates
    └── packs/                         # Your Private Per-Pack State & Custom Packs
        ├── swe/                       # projects.yaml, repo notes, worktree pointers
        ├── invest/                    # watchlist.yaml, tickers/<TICKER>/thesis.md, filing notes
        ├── admin/                     # ledger/2026.beancount, taxes/2026/checklist.md, inbox_digest.md
        └── <my-private-pack>/         # Any personal pack you don't want in the open-source repo
```

> [!TIP]
> **Why this split works so well:** Anyone can clone `headquarter`, run `bin/hq init`, and get a fresh `local/` vault of their own. Meanwhile, a `pre-commit` check in the root repo blocks any commit containing personal emails or `local/` paths, so you can push improvements to the open-source repo at any time without worrying about leaking private data.

---

## 4. Agnostic Kernel & Parallel Pack Isolation

The top-level `hq` kernel knows **nothing** about software engineering, stocks, or taxes. Every specialty is an **Extension Pack** defined by a `pack.yaml` manifest.

### 4.1 What a Pack Looks Like (`packs/<id>/pack.yaml`)

```yaml
id: invest
name: "Investment Research"
description: "SEC filing watchers, thesis tracking, and citation-verified equity analysis"
concurrency:
  max_parallel_runs: 2          # Up to 2 concurrent research jobs
  daily_spawn_budget: 5         # Dedicated quota lane; cannot starve other packs
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

### 4.2 How Multiple Packs Run in Parallel Without Interfering

When `swe`, `invest`, and `admin` tasks run at the same time, four guardrails keep them completely isolated:

1. **Compartmentalized Context (`SessionStart` Hook):**
   - When a session starts for `[pack:invest]`, `hq hook session-start` injects `local/USER.md` + `local/packs/invest/MEMORY.md` **only**. Your tax data in `local/packs/admin/` is never loaded into an investment or coding conversation.
2. **Filesystem Sandbox Enforcement (`PreToolUse` Hook):**
   - `hq hook pre-tool` checks the active pack's `vault_subpath` and blocks the agent from reading or modifying another pack's private folder.
3. **Lock-Free, Immutable File Writes (Zero Race Conditions):**
   - Parallel tasks **never edit shared files in place**:
     - Turn logs write to `local/memory/journal/YYYY-MM-DD/<convId>.json`.
     - Approval requests write to `local/memory/pending/<uuid>.json`.
     - Parallel coding tasks in `swe` use separate `git worktree` directories (`Workspace: "share"` / `"branch"`).
   - Only the single-threaded **Nightly Consolidation** job merges the day's immutable files into `local/MEMORY.md`.
4. **Independent Quota & Concurrency Lanes:**
   - Each pack has its own `max_parallel_runs` limit and `daily_spawn_budget`. A busy afternoon of coding in `swe` cannot block or exhaust the budget for an urgent `admin` or `invest` task.

---

## 5. Under the Hood: The 4 Core Engine Loops

### Loop 1: Zero-Token Memory & Sleep-Time Consolidation
*Grounded in Letta's LoCoMo filesystem study, UC Berkeley's Sleep-Time Compute paper, and Stanford's ACE paper:*
- **Before a session (`SessionStart`):** `bin/hq` injects your profile and active pack memory in `<10ms`.
- **After a turn finishes (`Stop`):** Instead of calling an LLM or forcing an extra turn, `hq hook stop` reads the `transcriptPath` JSONL file passed by Antigravity on stdin (**0 LLM tokens, ~2ms**) and saves a structured record of the turn (`lastUserInput`, tools used, files touched, final response snippet) to `local/memory/journal/YYYY-MM-DD/<convId>.json`.
- **At 11:30 PM (`sleep-consolidate`):** One scheduled Flash run distills the day's journal files into small delta updates on `local/MEMORY.md` and `local/memory/facts.jsonl`.

### Loop 2: Silent Heartbeat & Quota Protection
*Grounded in Horvitz's Mixed-Initiative utility gating:*
- **0-Token Go Pre-Checks:** `hq daemon` wakes up every 30 minutes and checks lightweight endpoints (RSS feeds, file drops, GitHub status) in pure Go. If nothing requires action, **it spawns zero LLM conversations**.
- **Flash-Only Background Runs:** All scheduled and event-triggered runs explicitly use `--model=flash` (or `flash_lite`), keeping your Pro / Opus / Argon quota free for interactive work.
- **Circuit Breaker & Dead-Man's Switch:** `hq daemon` enforces a global daily cap (`max_daily_spawns: 15`) and alerts you if quota runs low or if the 07:30 Morning Brief fails to run by 08:00.

### Loop 3: Safety & Approval-Bound Execution
*Grounded in DeepMind's CaMeL pattern and Willison's "Lethal Trifecta" (Private Data + Untrusted Input + External Action):*
- **Quarantined Readers:** Any background job that reads untrusted content (emails, web pages, external PDFs) has **read-only + local summary write permissions only**.
- **Hard Block on Direct External Writes:** `hq hook pre-tool` blocks the LLM from directly running `gws gmail send`, `gh pr merge`, `git push`, or payment commands.
- **Stage → Approve → Deterministic Execute:**
  1. The agent stages a proposed action via `hq propose` (writing `local/memory/pending/<uuid>.json` with a `sha256` checksum of the exact recipient/body/command).
  2. You review the exact payload in Remote Control or the `HQ Console` side pane and tap **Approve**.
  3. **`hq daemon` verifies the `sha256` checksum and executes the CLI command directly in Go—with zero LLM involved after your approval.**

---

## 6. The Three Starter Packs (Included Out of the Box)

All three packs ship in `packs/` as generic, open-source templates and mount your personal data in `local/packs/`:

| Pack | What It Does for You | Key Tools & Guardrails |
|---|---|---|
| **1. `packs/swe`<br>Side-Project Engineering** | Tracks your active repos in `local/packs/swe/projects.yaml`; monitors open PRs, failing CI, and stale branches; turns short feature specs or bug notes into tested branches inside isolated git worktrees. | **Guardrail:** Subagents are read-only researchers; at most one writer per git worktree (`Workspace: "share"`). Never pushes or merges without `hq propose` approval. |
| **2. `packs/invest`<br>Investment Research** | Watches your holdings/watchlist in `local/packs/invest/watchlist.yaml`; maintains a living `tickers/<TICKER>/thesis.md` (*Core Thesis, Bull/Bear Case, Explicit Falsification Criteria*); alerts on new 10-K/10-Q/8-K filings and runs a Bull vs. Bear subagent check against your thesis. | **Guardrail:** Connects to `sec-edgar-mcp` / `OpenBB`. Every financial figure must cite `[Filing, Page/Section]` and pass deterministic script checks. **Zero brokerage trading tools.** |
| **3. `packs/admin`<br>Life Admin, Ledger & Taxes** | **Inbox & Calendar:** Quarantined read-only triage into `inbox_digest.md` + draft replies.<br>**Bookkeeping:** Watches `local/inbox/` for bank CSVs, categorizes into `2026.beancount`, and validates double-entry balance with `bean-check`.<br>**Taxes:** Tracks `taxes/2026/checklist.md` (W-2s, 1099s, estimated deadlines) and extracts fields into workpapers.<br>**Bookings:** Navigates via Chrome DevTools MCP up to final confirmation, then pauses for your approval. | **Guardrail:** All accounting and tax math runs in deterministic tools (`bean-check` / scripts), never mental LLM math. Secrets stay in Keychain/1Password, never in `local/MEMORY.md`. |

---

## 7. Reference Comparisons: Why This Stack Wins

### 7.1 How Option 1.5 (`Headquarter`) Closes Pure Antigravity's Gaps

| Frontier Capability | Pure Antigravity (Option 1) | `Headquarter` (Option 1.5) Solution |
|---|---|---|
| **Scheduled Cron Jobs** | ✅ Native (`"builtin": "schedule"`) | Uses native Automations pinned to `--model=flash`. |
| **Execution (Code, Browser, MCP)** | ✅ Native (Sandbox, `/browser`, Subagents) | Uses native capabilities + per-pack path sandboxing. |
| **Mobile & Remote Web Access** | ✅ Native (**Remote Control** PWA + push) | Piggybacks on Remote Control in Phase 1; pluggable Chat adapters in Phase 2. |
| **Silent 30m Heartbeat** | ❌ Missing (cron always spawns a chat) | `hq daemon` runs 0-token Go pre-checks and only spawns when triggered. |
| **Event Triggers (File drop / RSS)** | ❌ Missing in App | `hq daemon` watches `local/inbox/` (`fsnotify`) and polls feeds. |
| **Cross-Session Memory** | ❌ Stateless automations | `SessionStart` injection + 0-token `Stop` transcript parser + nightly consolidation. |
| **Parallel Domain Isolation** | ❌ Flat workspace | Per-pack `pack.yaml` sandboxes, lock-free UUID journals, per-pack quota lanes. |

### 7.2 Comparing All 6 Architectural Options Evaluated

| Option | Effort | Power | Security | Est. Cost | Verdict |
|---|---|---|---|---|---|
| **Option 1: Pure Native Antigravity** | 1–2 days | Medium | High (on `Default`/`Auto`) | Existing sub | Good starting point, but lacks silent heartbeat, event triggers & automatic memory. |
| **Option 1.5: `Headquarter` (App + `bin/hq` Go Kernel)** | **3–4 days** | **High** | **Very High** | **Existing sub** | **Selected Architecture.** Runs 24/7 on your always-on Mac; open-sourceable core + private Vault. |
| **Option 2: Antigravity Python SDK Daemon** | 5–7 days | Very High | Very High | \$10–60/mo API + VM | Optional future path if you ever move off macOS to a headless Linux VM. |
| **Option 3: OpenClaw (394k★) or Hermes (261k★)** | 1d install / weeks to harden | Very High | Low–Fair (OpenClaw CVEs & skill malware) | \$50–250+/mo API | Skip the monolith codebase; we already borrowed their best ideas (`USER.md`, `MEMORY.md`, silent heartbeat). |
| **Option 4: Micro-Claw (`NanoClaw` / `nanobot`)** | 2–4 days | Med–High | Very High | API + host | Useful reference if we ever add Docker-per-task sandboxing. |
| **Option 5: Cloud Durable Stack (Cloudflare / ADK)** | 1–3 weeks | Highest | High | \$5–40/mo + API | Unnecessary when you already have an always-on Mac desktop. |

### 7.3 Why Go (`golang`) for `bin/hq`?
Antigravity invokes hooks (`hooks.json`) and sidecars (`sidecar.json`) as standard OS processes over JSON stdin/stdout. Writing `bin/hq` as **a single compiled Go binary** gives us:
- **Instant hook execution (`<10ms` estimate)** on every session start and tool call, with **zero Python `venv` or `pip` breakage**.
- **Built-in concurrency (`goroutines` + `fsnotify`)** for watching `local/inbox/`, running 30m pre-checks across packs, and serving the right-hand `HQ Console` web UI in a tiny memory footprint.
- Meanwhile, domain-specific Python CLIs (`bean-check` for accounting, `sec-edgar-mcp` for filings) still work seamlessly as external tools called by the agent.

---

## 8. Implementation Plan (3 Clean Steps)

| Step | What We Do | Outcome |
|---|---|---|
| **Step 0: Safety Setup & 30-Min Verification** | 1. Switch Antigravity global permission preset from `Turbo` to `Default` or `Auto` (enabling the macOS Seatbelt sandbox).<br>2. Verify `agentapi` CLI flags and `hooks.json` round-trip with a minimal `bin/hq` binary. | Sandbox is active and hook/sidecar contracts are verified on your machine. |
| **Step 1: Scaffold `Headquarter` + `Vault` + Starter Packs (Day 1–2)** | 1. Create the open-source `headquarter/` structure (`cmd/hq/`, `internal/`, `packs/{swe,invest,admin}/`, `templates/`, `.agents/hooks.json`).<br>2. Compile `bin/hq` (`init`, `hook`, `propose`, `notify`, `daemon`, and the Aux Pane `HQ Console`).<br>3. Run `bin/hq init` to create your private git-ignored `local/` Vault (`USER.md`, `MEMORY.md`, `inbox/`, and per-pack folders). | You can use all 3 packs (`swe`, `invest`, `admin`), drop files into `local/inbox/`, view the `HQ Console` side pane, and access everything from your phone via **Antigravity Remote Control**. |
| **Step 2: Scheduled Automations & Optional Chat Adapters (Day 2–3)** | 1. Register the Scheduled Automations (`morning-brief` at 07:30, `edgar-close-watch` at 13:30, `sleep-consolidate` at 23:30) pinned to `--model=flash`.<br>2. *(Optional Phase 2)* Add the Google Chat / Telegram adapter in `internal/channels/`. | Full 24/7 proactive personal assistant running on your always-on Mac. |
