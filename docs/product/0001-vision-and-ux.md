# PRD-0001: Core Product Vision & Day-in-the-Life UX

- **Status:** Approved
- **Related Docs:** [architecture.md](../architecture.md), [RFC-0001](../design/0001-vault-and-task-isolation.md), [ADR-0004](../decisions/0004-remote-control-first-pluggable-channels.md), [ADR-0006](../decisions/0006-general-agent-and-skills-over-packs.md)

---

## 1. Product Vision

The newest generation of personal agents—Meta Muse, Manus, OpenAI `dots`, Anthropic Claude Duet/Cowork, OpenClaw, and Nous Hermes—all share six product pillars:
1. **Always-On Presence** (continues working when your laptop lid is closed).
2. **Silent-by-Default Heartbeat** (monitors in the background, interrupts only when high-value and urgent).
3. **Event-Driven Reactions** (file drops, new SEC filings, urgent emails, CI failures).
4. **Pocket Access & 1-Tap Approvals** (read briefs, delegate tasks, and approve actions from your phone).
5. **Consolidated Long-Term Memory** (never starts from a blank slate).
6. **Compounding Skills** (distills repeated workflows into verified skills).

`agy-hq` delivers all six pillars on top of an always-on Mac running Google Antigravity, while keeping 100% of personal data inside a git-ignored private `Vault` (`local/`).

---

## 2. The Four User Touchpoints (Day-in-the-Life UX)

### Touchpoint 1: Mobile & Remote Web (`Antigravity Remote Control` PWA First, Chat Apps Later)
By piggybacking on Antigravity's built-in **Remote Control (`antigravity.google`)**, you install the web app to your phone's home screen on Day 1 with zero external bot setup:
- **07:30 AM Morning Brief:** A single push notification links to a structured Morning Brief artifact summarizing:
  - **Life Admin:** Today's calendar + quarantined inbox triage highlights (with drafted replies staged for approval).
  - **Investments:** Overnight 10-K/10-Q/8-K filing alerts and thesis impact summaries with page citations.
  - **Side Projects:** Repo CI status, open PR checks, and background branch progress.
- **Silent All Day:** Every 30 minutes, `hq daemon` runs 0-token Go pre-checks. If nothing crosses your urgency threshold, zero LLM sessions are spawned and your phone stays quiet.
- **On-the-Go Delegation & Approvals:** Start or resume any `agy-hq` conversation from your phone's browser, invoke `/thesis-check` or `/swe-pulse`, and tap native **Approve / Proceed** buttons on staged actions.
- **Phase 2 Chat Mirroring:** When enabled, `hq`'s `ChannelAdapter` mirrors briefs and interactive approval cards to Google Chat or Telegram.

### Touchpoint 2: At Your Desk (`Antigravity App` + Right-Hand `HQ Console`)
- **Left Conversation Sidebar:** Every scheduled and event-triggered run appears in your normal Antigravity history.
- **Center Chat Canvas:** `hq hook session-start` automatically injects your `local/USER.md` profile and `local/MEMORY.md`; `hq hook stop` captures a 0-token turn log when you finish.
- **Right Auxiliary Side Pane (`[Open HQ Console](sidecar://hq/dashboard)`):** A single-page local web panel inside Antigravity's side pane showing:
  - **Pending Approvals Queue** (exact diffs/payloads with 1-click `[Approve]` / `[Reject]`).
  - **Active Watchers & Daily Quota Meter** (daily spawn counts and circuit-breaker status).

### Touchpoint 3: Zero-Prompt Drop Folder (`local/inbox/`)
Drag any bank CSV, PDF receipt, W-2, 10-K, or notes file into `local/inbox/`:
- `hq daemon` detects the file via `fsnotify`, matches it against `local/triggers.yaml` (or passes it to a general triage prompt if unmatched), and automatically dispatches the task.

### Touchpoint 4: Nightly "Sleep-Time" Consolidation (11:30 PM)
While you sleep, the `sleep-consolidate` automation reviews the day's turn logs (`local/memory/journal/YYYY-MM-DD/*.json`):
- Applies incremental delta updates to `local/MEMORY.md` and `local/memory/facts.jsonl`.
- Stages any proposed `SKILL.md` refinements in `local/memory/proposals/` for your approval the next morning.
- Commits the private `local/` Vault to git.
