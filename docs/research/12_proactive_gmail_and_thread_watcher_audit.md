# 12 — Forensic Audit: Proactive Gmail Watching, Counterparty `WaitingOn` Threads & Outbox Front Desk Delivery

- **Date:** 2026-11-14
- **Related Design:** [RFC-0016: Muse-Style Proactive Gmail & Counterparty Thread Watcher](../design/0016-proactive-gmail-and-thread-watcher.md)
- **Prior Baseline:** [RFC-0005: Native Gmail Integration](../design/0005-gmail-integration.md), [RFC-0014: Proactive Engine v2](../design/0014-proactive-engine-and-living-context.md), [Research 10: Meta Muse Deep Dive](10_muse_ai_proactive_and_context_study.md)

---

## 1. Executive Summary

`RFC-0014` introduced the foundational primitives for `agy-hq`'s Proactive Engine v2: 0-token cursor gatekeepers (`internal/kernel/gatekeeper.go`), a 4-tier Attention Router (`internal/outbox/outbox.go`), and stateful multi-day `Goals` with counterparty `WaitingOn` SLAs (`internal/kernel/goals.go`).

However, a line-by-line audit of the live runtime reveals **four structural gaps** that prevent `agy-hq` from delivering a Muse-grade daytime proactive experience (`07:00–21:00 PT`):

1. **Zero Daytime 15–20m Schedule & Blind `WaitingOn` Reply Detection:** `templates/triggers.yaml` has no daytime Gmail gatekeeper schedule, `SweepWaitingOnObligations` (`internal/kernel/goals.go:369-430`) never checks Gmail threads for counterparty replies (relying solely on wall-clock expiry), and `DefaultPrimaryQuery` (`category:primary is:unread`) misses replies routed to `Updates` (school portals, recruiting ATS platforms, calendar invites, investor portals) as well as emails glanced at on mobile.
2. **Transport Auth Split-Brain & Silent Error Masking in `hq daemon`:** `gmail.RunPrecheck` (`internal/gmail/gmail.go:831-880`) silently swallows transport errors (`if err != nil { continue }`) and returns `"GMAIL_OK (0 new unread Primary messages; 0 LLM tokens)"` even when OAuth token refresh fails or the Antigravity `gmail` plugin CLI lacks session credentials outside `run_command`. Furthermore, `loadAccessTokenFromFile` (`gmail.go:1288-1290`) returns cached `access_token` values without checking `expiry`, and `RunPrecheck` marks messages as `SeenMessageIDs` *before* any triage worker succeeds.
3. **The Outbox `interrupt` Delivery Black Hole:** `outbox.Add` (`internal/outbox/outbox.go:383-430`) appends `status: "pending"` rows to `local/memory/outbox/items.jsonl`, **and nothing in `agy-hq` ever pushes pending `interrupt` items to `[HQ] Front Desk` (`front_desk.json`)**. Instead, legacy `RunHeartbeat` spawns orphaned top-level `[Auto] Gmail Triage` sidebar threads (`kernel.go:2235`) that write Markdown files nobody is notified about until the next day's 7:30 AM `/morning-brief`.
4. **Three Concrete Quiet-Hours & SLA Escalation Bugs:**
   - `DefaultAttentionConfig()` (`outbox.go:89`) sets `QuietHours.Enabled = false` and `templates/config.yaml` omits `enabled: true`, leaving `22:00–07:00 PT` quiet hours **disabled on every initialized Vault**.
   - `templates/config.yaml:48` writes `tz: "America/Los_Angeles"`, whereas `ParseAttentionConfig` (`outbox.go:180`) only parses `timezone:`, silently dropping the user's configured timezone.
   - `EvaluateDelivery` (`outbox.go:355-359`) irreversibly overwrites `TierInterrupt -> TierBriefRollup` during quiet hours instead of holding items until `07:00 PT`, and `SweepWaitingOnObligations` (`goals.go:385-401`) never escalates expired `WaitingOn` SLAs to `interrupt` unless the parent Goal happens to have a `TargetDate` within 24 hours.

---

## 2. Benchmark Comparison: Muse (`muse.ai`) vs. Current `agy-hq` Runtime

Drawing on [Research 10](10_muse_ai_proactive_and_context_study.md), Muse's proactive inbox loop succeeds because three subsystems work in lockstep: **Hatch** (connector watermark polling + counterparty correlation), **Do-the-Work-First Synthesis** (pre-checking calendar availability and staging ready-to-send replies/invites before interrupting), and **Chat Bubble + Sentinel** (calibrated, quiet-hours-aware delivery with 1-tap approval cards).

| Capability | Meta Muse (`muse.ai`) Pattern | `agy-hq` Current State (`RFC-0014`) | Root-Cause Gap Identified in Audit |
|---|---|---|---|
| **Daytime Polling Cadence** | Continuous / 15m connector cursor watch during waking hours | Hardcoded 30m 24/7 `hbTicker` (`cmd/hq/main.go:1412`); 0 schedules in `triggers.yaml` | Wakes 24/7 on a fixed 30m loop; not declaratively scoped to `07:00–21:00 PT` (`15–20m`) in `triggers.yaml`. |
| **Inbound Scope Coverage** | Correlates VIPs, active threads, school/family logistics, recruiting, and calendar invites across tabs | `label:INBOX is:unread category:primary -label:promotions -label:social` (`gmail.go:28`) | Misses `CATEGORY_UPDATES` where school portals, camp/sports schedules, ATS recruiting messages, and `.ics` updates land. |
| **Counterparty `WaitingOn` Tracking** | Correlates inbound thread replies against open obligations; auto-closes timers on reply | `SweepWaitingOnObligations` (`goals.go:369`) only checks `now >= FollowUpAfter` | Never inspects Gmail threads! Replies read on mobile (`!is:unread`) or in `Updates` go undetected and trigger **false overdue SLA alerts**. |
| **Zero-Token Delta Fidelity** | Connector cursor tracks `historyId` + thread message watermarks + sender/list headers | `searchViaOAuthToken` (`gmail.go:1347-1364`) fetches only `{id, threadId}` | Stripped `From`, `Subject`, `Snippet`, and `List-Unsubscribe` headers prevent 0-token bulk filtering and adaptive model selection. |
| **Model Tier Calibration** | Cheap classifier on routine mail; capable synthesis only on high-signal threads | `SpawnFlashTask` (`kernel.go:2161`) always resolves `"flash"` to `gemini-4-hydrogen-high` | Wastes `standard` (`high`) thinking budget on newsletters/receipts instead of routing `gemini-4-hydrogen-low` (`quick`) / `gemini-4-hydrogen-medium` (`routine`). |
| **"Do the Work First" Offers** | Checks calendar free/busy first; presents 1-tap reply draft **or** calendar event creation/update | `inbox-triage` skill stages `hq gmail draft`, but tainted sessions cannot inspect/stage `gcalendar` | Missing quarantine-safe `hq calendar` read/propose bridge; calendar reschedules degrade to passive text notes. |
| **Front Desk Push Delivery** | Delivers coalesced action card directly into the single active assistant thread | `outbox.Add` writes JSONL to disk; zero Front Desk push (`outbox.go:383`) | `interrupt` items sit undelivered in `items.jsonl`; `[HQ] Front Desk` and Antigravity Remote Control are never notified. |
| **Quiet Hours (`22:00–07:00 PT`)** | Holds non-critical notifications overnight; flushes cleanly in the morning | `QuietHours.Enabled` defaults to `false`; `tz:` key ignored; irreversible tier overwrite | Quiet hours are inert out of the box, and enabling them permanently downgrades `interrupt` items to `brief_rollup`. |

---

## 3. Code-Level Forensic Findings

### 3.1 `internal/gmail/gmail.go`: Transport, Cursor & Auth Failure Modes

1. **Silent False-Green on Auth/Network Errors (`gmail.go:831-880`):**
   In `RunPrecheck`, each enabled account calls `msgs, err := SearchMessages(vaultDir, alias, cfg.Query, 20)`. When `err != nil`, line 834 executes `continue`, and line 878 returns:
   ```text
   GMAIL_OK (0 new unread Primary messages; 0 LLM tokens)
   ```
   Because `hq daemon` runs as a background OS process outside Antigravity's `run_command` tool sandbox, Antigravity's managed plugin credential shim (which injects OAuth tokens only when bare `gmail` is invoked inside an agent turn) is unavailable to `hq daemon` unless a standalone OAuth `TokenFile` (`local/private/gmail/tokens/<alias>.json`) or standalone CLI login exists. Any auth failure in `hq daemon` is silently reported as `GMAIL_OK`.

2. **Missing Token Expiry Check & Truncated OAuth Bodies (`gmail.go:1273-1438`):**
   - `loadAccessTokenFromFile` (`gmail.go:1288-1290`) immediately returns `tok.AccessToken` whenever non-empty without checking token expiration or retrying on HTTP `401`, causing standalone OAuth polling to fail ~1 hour after login even when a valid `refresh_token` is present on disk.
   - `searchViaOAuthToken` (`gmail.go:1328-1364`) calls `/gmail/v1/users/me/messages` and returns `MessageSummary` structs with empty `From`, `Subject`, `Snippet`, and `Labels`.
   - `readThreadViaOAuthToken` (`gmail.go:1391-1416`) discards `payload.parts` / `payload.body.data` and populates `PlainBody: rm.Snippet` (~160 chars), starving the Quarantined Reader of actual message content when OAuth transport is active.
   - `CreateDraft` (`gmail.go:708`) and `SendApprovedDraft` (`gmail.go:762`) invoke `createDraftViaPluginCLI` / `sendDraftViaPluginCLI` exclusively, with no OAuth REST fallback (`users.drafts.create` / `users.drafts.send`) when approved via `:4242` WebUI.

3. **At-Most-Once Cursor Loss Before Triage (`gmail.go:841-853`):**
   `RunPrecheck` mutates `st.SeenMessageIDs[m.ID] = true` and writes `state-<alias>.json` *before* spawning the triage worker. If the spawn is blocked by `QuotaState.CircuitOpen`, coalesced away, or fails mid-turn, those messages are permanently skipped on all future ticks. Additionally, `SeenMessageIDs` grows unboundedly over months without TTL compaction.

### 3.2 `internal/kernel/goals.go`: Counterparty `WaitingOn` Blind Spots

1. **No Thread Inspection in `SweepWaitingOnObligations` (`goals.go:369-430`):**
   `WaitingOnItem` stores `Channel` (`"gmail"`), `Counterparty` (`"Bob Rivera <bob@example.com>"`), and `ThreadOrSpace` (`"1934a8f0c2e1b001"`), yet `SweepWaitingOnObligations` never queries Gmail. If Bob replies within 20 minutes:
   - If the reply lands in `CATEGORY_UPDATES` or the user reads it on their phone before the next poll, `WaitingOnItem.Status` stays `"waiting"` until `FollowUpAfter` expires, at which point `SweepWaitingOnObligations` emits a **false-positive overdue SLA alert** (`status = "nudge_queued"`).
   - Even if the reply lands in `Primary` unread, `RunPrecheck` does not annotate `TriageBatch` with matching `WaitingOn` obligations, forcing the LLM worker to guess whether an inbound thread corresponds to an active Goal.

2. **Overly Restrictive SLA Interrupt Escalation (`goals.go:385-401`):**
   Lines 385–396 set `tier = outbox.TierBriefRollup` by default and only upgrade to `outbox.TierInterrupt` if `g.TargetDate` parses cleanly and `untilTarget <= 24*time.Hour`. Goals created without `--target` (or multi-week goals like recruiting loops, school enrollment, or tax prep) **never** escalate an overdue `48h` counterparty reply SLA to `interrupt` during the workday, burying time-sensitive follow-ups until the next morning's brief.

### 3.3 `internal/outbox/outbox.go`, `internal/kernel/kernel.go` & `internal/webui/webui.go`: Delivery & Quiet-Hours Bugs

1. **Undelivered `interrupt` Queue (`outbox.go:383-430`):**
   `outbox.Add` writes items to `local/memory/outbox/items.jsonl` and calls `SyncConsoleHook(vaultDir)`. Neither `outbox.Add`, `RunHeartbeat` (`kernel.go:2195-2258`), nor `runDaemonLoop` (`cmd/hq/main.go:1405-1465`) ever dispatches `TierInterrupt` items to the active `[HQ] Front Desk` conversation (`front_desk.json`).
   Meanwhile, `SyncMarkdownConsole` (`webui.go:1739`) and `webui.loadState` (`webui.go:675`) filter exclusively by `outbox.TierIdeasFeed`—so pending `interrupt` items do not even appear on `hq_console.md` or `http://127.0.0.1:4242`!

2. **Front Desk Delivery Transport Mechanics in Antigravity:**
   We audited both available message injection mechanisms in the host environment:
   - **LanguageServer ConnectRPC (`SendUserCascadeMessage` in `kernel.go:3055-3091`):** Discovers the live Antigravity LanguageServer process port + CSRF token and posts a user-turn cascade message directly into an existing `cascadeId` (`front_desk.json`'s `ConversationID`).
   - **`agentapi send-message <recipient_id> <content>` (`~/.gemini/antigravity/bin/agentapi`):** CLI wrapper that delivers a message to an existing conversation ID, queuing reactively if the target conversation is mid-turn and waking it automatically when ready.
   - **Prefix Cache Preservation:** Unlike `SpawnFlashTask` (which starts a cold-cache top-level conversation on every batch and fragments user attention across dozens of `[Auto] Gmail Triage` threads in the sidebar), running quarantined triage in an isolated ephemeral worker (`gemini-4-hydrogen-low` / `gemini-4-hydrogen-medium`) and pushing **only** verified `interrupt` takeaways + pre-staged proposal previews into `[HQ] Front Desk` keeps untrusted email bodies 100% isolated from `[HQ] Front Desk` while preserving `[HQ] Front Desk`'s warm prefix cache (~93%+ cache hit rate per [Research 08](08_token_observability_and_telemetry.md))!

3. **Quiet-Hours Configuration & State Transition Defects (`outbox.go:84-96, 166-195, 349-363`):**
   - `DefaultAttentionConfig` sets `QuietHours.Enabled: false`.
   - `ParseAttentionConfig` ignores `tz:` in `templates/config.yaml`.
   - `EvaluateDelivery` mutates `TierInterrupt -> TierBriefRollup` during `22:00–07:00 PT` instead of holding the item (`status: "held_quiet_hours"`, `deliverAfter: 07:00 PT`), losing its `interrupt` priority and 1-tap approval workflow once morning arrives.

### 3.4 `internal/hooks/hooks.go`: Quarantine Boundary for Concrete Calendar Offers

Under [RFC-0003](../design/0003-approval-bound-execution.md), calling `hq gmail read-quarantined` taints the worker conversation (`tainted_<convID>.json`). `PreToolUse` (`hooks.go:492, 715-722`) restricts tainted `run_command` calls to `reAllowedQuarantineHQCmd` (`hq (gmail|propose|gchat|memory|outbox|goals|ideas|spaces|tasks|status)` with zero shell metacharacters).

Consequently:
- **Gmail Draft Replies Work Today:** A tainted worker can already run `hq gmail draft` + `hq propose --type=gmail_send --draft=<id>`.
- **Calendar Conflict Checks & Staged Event Updates Are Currently Blocked:** A tainted worker cannot invoke `gcalendar readonly events` (to check whether Tuesday 2:00 PM PT is free before drafting an interview reply) nor stage a `gcalendar mutate create|update|rsvp` command without shell quoting hazards or confused-deputy attendee exfiltration risks.
- **Architectural Implication:** Adding a narrow, schema-validated `hq calendar` bridge (`hq calendar events`, `hq calendar freebusy` scoped strictly to the user's own calendar when tainted, and `hq calendar propose` staging deterministic `calendar_update` proposals with attendee allowlisting and `--send-updates=none` safe defaults) unlocks Muse-style concrete calendar offers while keeping the `RFC-0003` Lethal Trifecta completely broken.

---

## 4. Token & Quota Budget Modeling (`07:00–21:00 PT`, 15-Minute Cadence)

At a 15-minute daytime cadence from `07:00` through `21:00 PT` inclusive (`57` gatekeeper ticks/day):

| Day Profile | Gatekeeper Ticks (`0` Tokens) | Delta Batches (`Delta != 0`) | Worker Invocations (`hydrogen-low` / `medium`) | `[HQ] Front Desk` Wakeups (`interrupt` Only) | Est. Uncached Daily Token Burn |
|---|---:|---:|---:|---:|---:|
| **Quiet Day** (8 newsletters/receipts, 1 actionable reply) | 57 | 5 | 4 `hydrogen-low` + 1 `hydrogen-medium` | **1** | ~35k–55k (`<0.4%` of 15M budget) |
| **Typical Active Day** (20 newsletters/updates, 4 actionable threads, 1 SLA nudge) | 57 | 11 | 7 `hydrogen-low` + 4 `hydrogen-medium` | **3–4** (coalesced) | ~95k–145k (`<1.0%` of 15M budget) |
| **Heavy Burst Day** (50 inbound emails, active recruiting + school + investing threads) | 57 | 16 (burst-coalesced) | 9 `hydrogen-low` + 7 `hydrogen-medium` | **5–6** (coalesced) | ~180k–260k (`~1.5%` of 15M budget) |

Compared to waking `[HQ] Front Desk` on a 15-minute cron (`57 × ~120k = ~6.8M` cached/uncached tokens/day and rapid context window exhaustion), the 3-stage gatekeeper $\rightarrow$ adaptive Hydrogen delta worker $\rightarrow$ coalesced Front Desk `interrupt` push uses **>96% fewer Front Desk turns** and keeps `0` untrusted newsletter/HTML bytes out of the coordinator context.
