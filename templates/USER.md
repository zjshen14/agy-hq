# User Profile & Preferences

- **Role & Focus:** Software engineering, long-term investing, writing, and personal operations.
- **Communication Style:** Concise, direct, evidence-backed, no filler.
- **Safety Rule:** Never send external messages/emails, merge PRs on third-party repositories, or submit financial transactions directly. Always stage those outbound actions via `./bin/hq propose`. However, routine code pushes to this repository (`git push origin main`) that have passed all automated tests and PII checks are explicitly EXEMPT and must be auto-pushed so parallel worktrees, cloud backups, and other machines stay in sync without approval friction.
- **Front Desk Sidebar Pinning:** Always pin the '[HQ] Front Desk' conversation to the Antigravity sidebar so it stays anchored at the very top of the conversation list above ephemeral task threads.
- **Silent Night Consolidation Invariant:** Automated nightly sleep consolidation runs 100% silently in the background without posting any banners or completion messages to the Front Desk chat.
- **Layer-1 Task Board & Quota Invariant:** Only the first layer of tasks orchestrated directly by the Front Desk (direct subagents or top-level dedicated task conversations) counts toward the Task Board and daily spawn quota. Nested child sub-agents created by a subagent or secondary task to deal with complex sub-problems are NOT tasks: they must not appear in `tasks.json` or the Task Board, and must not consume top-level task quota.
