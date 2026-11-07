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
2. **Calendar & Life Admin:**
   - If the `gcalendar` skill/plugin is available, check today's schedule and highlight time-sensitive meetings or conflicts.
   - Check `local/inbox/` for any unprocessed files dropped overnight.
3. **Engineering Projects (`swe-pulse`):**
   - For each active worktree in `local/worktrees/` (or tracked repos in `local/notes/`), summarize open PR status or failing CI checks via `gh pr status`.
4. **Investments (`thesis-check`):**
   - Check `local/notes/investing/` for tracked tickers and highlight any material overnight 8-K/10-Q/10-K filings or thesis triggers.
5. **Write Brief:**
   - Save a dated copy to `local/notes/general/brief-YYYY-MM-DD.md` and present an executive summary with links to any staged approvals (`./bin/hq status`).
