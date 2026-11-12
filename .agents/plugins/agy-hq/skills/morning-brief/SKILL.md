---
name: morning-brief
description: >-
  Generate the daily Morning Brief across calendar events, inbox triage,
  pending approvals, active engineering worktrees/PRs, and investment watchlist alerts.
  Use when the user asks for a morning brief, daily digest, or status overview.
---

# Morning Brief (`/morning-brief`)

Produce a concise, actionable daily brief covering life admin, engineering projects, goals/SLAs, and investment watchlist updates.

## Steps

1. **Check Pending Approvals, Attention Outbox, Goals & Token/Context Health:**
   - Run `./bin/hq status` to inspect pending approval proposals (noting any `expires:` TTLs or `⚠️ PII WARNING` flags) and active git worktrees in `local/worktrees/`.
   - Run `./bin/hq outbox drain` to consume overnight `brief_rollup` items (quiet-hours DND holds, `INTACT` thesis checks, and SLA nudges) and `./bin/hq ideas list` to surface top pending `ideas_feed` suggestions.
   - Run `./bin/hq goals due --within=72h` to highlight approaching multi-day goal deadlines and overdue counterparty `WaitingOn` SLAs.
   - Run `./bin/hq obs tokens --since=24h` to check 24h uncached token burn, Prefix Cache Hit Ratio (`CHR %`), `[HQ] Front Desk` context watermark (recommending `/compact` if $\ge 120\text{k}$ tokens), and any active `A1`–`A5` anomaly alerts.
2. **Calendar, Gmail, Google Chat & Life Admin:**
   - If the `gcalendar` skill/plugin is available, check today's schedule and highlight time-sensitive meetings or conflicts.
   - Check Gmail via `./bin/hq gmail search` and Google Chat via `./bin/hq gchat search --query="is:unread"` (plus any overnight digests in `local/notes/inbox/`) to summarize high-priority unread threads, DMs, and any auto-drafted replies awaiting approval.
   - Check `local/inbox/` for any unprocessed files dropped overnight.
3. **Engineering Projects (`swe-pulse`):**
   - For each active worktree in `local/worktrees/` (or tracked repos in `local/notes/`), summarize open PR status or failing CI checks via `gh pr status`.
4. **Investments (`thesis-check`):**
   - Check `local/notes/investing/` for tracked tickers and highlight any material overnight 8-K/10-Q/10-K filings or thesis triggers.
5. **Write Brief & Present 1-Tap Mobile Approvals:**
   - Save a dated copy to `local/notes/general/brief-YYYY-MM-DD.md` and output the executive summary directly in chat (so it reads cleanly on Mobile Remote Control without needing to open local files or `localhost` links).
   - If `./bin/hq status` shows any pending proposals, call `ask_question` with `Approve (<id> · sha256:<prefix>)`, `Reject (<id>)`, and `Keep pending` options so the user can approve or reject them with 1 tap on their phone.
   - **Silent Memory & Ledger Operations:** Never reference internal bookkeeping files or ledger paths (`MEMORY.md`, `facts.jsonl`, `tasks.json`) in the brief; report only actionable executive insights.
