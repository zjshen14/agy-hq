---
name: morning-brief
description: >-
  Generate the daily Morning Brief across calendar events, inbox triage,
  pending approvals, active engineering worktrees/PRs, and investment watchlist alerts.
  Use when the user asks for a morning brief, daily digest, or status overview.
---

# Morning Brief (`/morning-brief`)

Produce a concise, actionable daily brief covering life admin, engineering projects, and investment watchlist updates.

## Steps

1. **Check Pending Approvals & Active Worktrees:**
   - Run `./bin/hq status` to inspect any pending approval proposals in `local/memory/pending/` and active git worktrees in `local/worktrees/`.
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
