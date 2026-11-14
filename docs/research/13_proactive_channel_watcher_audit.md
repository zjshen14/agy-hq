# 13 — General Watcher Mechanism, Quarantine Provenance & Pluggable Enterprise Connector Audit (Post-`RFC-0018`)

- **Date:** 2026-11-14
- **Baseline Audited:** `origin/main` (incorporating [RFC-0003](../design/0003-approval-bound-execution.md), [RFC-0005](../design/0005-gmail-integration.md), [RFC-0008](../design/0008-gchat-integration.md), [RFC-0014](../design/0014-proactive-engine-and-living-context.md), [RFC-0015](../design/0015-global-plugin-packaging.md), [RFC-0016](../design/0016-sidebar-conversation-archive-sync.md), and [RFC-0018](../design/0018-always-on-agent-runtime-hardening.md))
- **Companion Designs:**
  - [RFC-0017a — Modular Connector Protocol, Hash-Pinned Trust Registry & 0-Token Watcher Engine](../design/0017a-connector-protocol-and-watcher-engine.md)
  - [RFC-0017b — Provenance-Based Taint Propagation, Quarantined Context Queries & Offer Gateway](../design/0017b-provenance-taint-and-offer-gateway.md)
  - [RFC-0017c — Non-Destructive Lease-Based Morning Brief Collector & Structured Extensions](../design/0017c-lease-based-morning-brief-and-extensions.md)
- **Prior Research Referenced:** [10 — Meta Muse (`muse.ai`) Deep Dive](10_muse_ai_proactive_and_context_study.md), [14 — Anthropic Always-On Agents Gap Audit](14_anthropic_always_on_agents_gap_audit.md)

---

## 1. Executive Summary

High-frequency, low-noise proactive watching across communication channels, calendars, and counterparty threads—paired with concrete, 1-tap approval-bound offers—is a foundational capability of an always-on Personal Agent OS ([Research 10](10_muse_ai_proactive_and_context_study.md)).

When operating `agy-hq` across **both personal workstations and enterprise environments**, two non-negotiable architectural requirements emerge:
1. **Zero-Recompile Enterprise Modularity:** Even for the "same" vendor product (such as **Gmail** or **Google Calendar**), **Enterprise Workspace** deployments differ substantially from consumer setups: corporate IT often restricts self-serve OAuth token files on disk, routes authentication through Antigravity-managed Desktop SSO shims (`AGY_ACCOUNT=user@acme.com`) or proprietary corporate CLIs/proxies, enforces DLP policies (`draft_only` vs. direct API send), and pairs mail with internal tools (Slack, Jira / Atlassian `twg`, GitHub Enterprise, Gerrit, PagerDuty, or internal incident CLIs) whose code cannot live inside open-source `agy-hq`. Neither the watcher daemon, quarantine hooks, `/inbox-triage`, nor `/morning-brief` can hardcode consumer Gmail/Calendar/Chat silos.
2. **Provenance-Enforced Quarantine & Single-Authority State Integrity:** Proactive channel watching continuously ingests untrusted external inputs. As verified forensically against live code and empirical Antigravity plugin-CLI probes below, wrapping untrusted text in static XML tags (`<untrusted_content>`) is **not** a security boundary if untrusted strings reach untainted worker prompts, unfenced Front Desk user-message injections, or `front_desk_cron` `/morning-brief` turns. Similarly, overlapping "processed" markers across `Stop` hooks, unlocked JSONL stores, and read-destructive brief drains cause silent item loss or duplicate spawns under failure.

Auditing `origin/main` after [RFC-0018](../design/0018-always-on-agent-runtime-hardening.md) confirms that `agy-hq` already provides strong **generic runtime primitives** (`internal/events`, `internal/timers`, `CommandArgv` + `--resume-on-decide`, `lean_auto` `SessionStart`, `OutboxDispatcher`, and `hq outbox eval`), while exposing concrete gaps across **modularity, Desktop SSO shim lifecycle, untrusted-content laundering, offer-gateway enforcement, and store concurrency**—addressed cleanly across the three-part design suite ([RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md), [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md), and [RFC-0017c](../design/0017c-lease-based-morning-brief-and-extensions.md)).

---

## 2. Verified `RFC-0018` Baseline Primitives in `bin/hq`

| Core OS Capability | Verified Live Code on `main` | Composition Across `RFC-0017a` / `0017b` / `0017c` |
|---|---|---|
| Per-scope worker serialization & lease recovery | `events.Enqueue` + `LeaseNextBatches` (`DefaultLeaseTTL = 15 * time.Minute` in `internal/events/events.go:31`, renewed in `PreToolUse` at `internal/hooks/hooks.go:693-695`) | `hq watch precheck` enqueues per-item events directly (`ScopeKey: "triage:<connector>:<account>"`, `Source: "connector:<id>"`) with **no** stdout `GatekeeperEnvelope`. |
| Step/range cron & `CRON_TZ=` scheduling | `ScheduleTrigger` (`internal/kernel/daemon.go:20`) and cron matcher (`internal/kernel/gatekeeper.go:62-113`) **already support** `*/n`, `a-b`, `a-b/n`, comma lists, and `CRON_TZ=` | Reused directly (`CRON_TZ=<tz> */15 7-20 * * *`) without adding a redundant `Window` struct field. |
| Lean `[Auto]` worker context & anti-nagging | `leanAuto` `SessionStart` (`internal/hooks/hooks.go:223-275`) + `outbox.FormatReportedDigest` (`internal/outbox/reported.go:95`) | Reused across connector triage workers, hardened so tainted titles are bounded and nonce-fenced in `SessionStart` (`hooks.go:440-442`). |
| Intra-day deadline & promise check-ins | `hq timers set --kind=follow_up` (`internal/timers/timers.go`) | Allowlisted under quarantine when scoped to `--scope=inbox-triage`. |
| Shell-free `CommandArgv` execution & revision loop | `Proposal.CommandArgv`, `OriginSkill`, `ResumeOnDecide`, `RejectProposal` (`internal/approvals/approvals.go:121-131, 240-355, 373, 381-393`), which **already preserve** `AGY_ACCOUNT=` and parent `PATH` | Extended with `ExecContext`, resolved `argv[0]` + binary hash/version binding, `IdempotencyKey`, `ThreadKey` `Supersede`, and apply-time thread-head preconditions (`RFC-0017b`). |
| Event-driven Front Desk push & policy harness | `OutboxDispatcher` (`internal/outbox/dispatcher.go:29-119`), `hq daemon` `wakeCh` (`cmd/hq/main.go:1606-1652`), and `hq outbox eval` (`internal/outbox/eval.go`) | Hardened with `flock` atomic persistence, `tz:`/`enabled` parser fix, and nonce-fenced system cards for tainted items. |

---

## 3. Empirical Audit of the Antigravity Plugin-CLI SSO Shim Lifecycle

Read-only black-box probes of Antigravity's managed plugin-CLI wrapper (`env -i` invocations of `gmail readonly labels --json`, capturing exit codes and redacted stderr only) established the exact runtime behavior of Desktop-managed SSO plugins:

| Environment Passed to `language_server plugin-cli <tool> ...` | Observed Behavior |
|---|---|
| `PATH` only (no `ANTIGRAVITY_CREDENTIAL_SCOPE`, no `ANTIGRAVITY_PLUGIN_SHIM_DIR`) | **Hangs indefinitely** (>25s timeout, `rc=124`) |
| `PATH` + `ANTIGRAVITY_CREDENTIAL_SCOPE` (no `ANTIGRAVITY_PLUGIN_SHIM_DIR`) | **Hangs indefinitely** (>25s timeout, `rc=124`) |
| `PATH` + `ANTIGRAVITY_CREDENTIAL_SCOPE` + live `ANTIGRAVITY_PLUGIN_SHIM_DIR` | Succeeds (`rc=0`) |
| `PATH` + `ANTIGRAVITY_CREDENTIAL_SCOPE` + `SHIM_DIR` + `LS_ADDRESS` / `CSRF_TOKEN` | Succeeds (`rc=0`; `LS_ADDRESS` and `CSRF_TOKEN` are **not required**) |
| `PATH` + `ANTIGRAVITY_CREDENTIAL_SCOPE` + **deleted** or `/nonexistent` `SHIM_DIR` | **Succeeds (`rc=0`)** |
| `PATH` + invalid `ANTIGRAVITY_CREDENTIAL_SCOPE` + `/nonexistent` `SHIM_DIR` | Fast-fails (`rc=1`) with `"needs authentication"` + LLM-targeted `<plugin-card ...>` stderr text |

**Architectural Implications (`RFC-0017a` & `RFC-0017b`):**
1. **Ephemeral Shim Directories:** Each `run_command` creates a fresh `0700` `/tmp/agy-plugin-cli-<rand>/` directory containing 2-line wrappers (`exec '<stable_path>/language_server' plugin-cli <tool> "$@"`) and **deletes** that directory when the command exits. Caching `/tmp/agy-plugin-cli-*` paths on disk is both ineffective and unsafe on multi-user hosts.
2. **`ANTIGRAVITY_CREDENTIAL_SCOPE` Is a Bearer Capability Token:** It grants read and mutate access to connected Workspace plugins while the Antigravity Language Server runs. It must **never** be cached in unguarded `<vault>/memory/` (`hooks.go:800-824` guards `private/`, not `memory/`) and must **never** be allowlisted globally in `SanitizedApprovalEnv` (which would hand Workspace bearer credentials to arbitrary `hq propose --cmd` commands).
3. **Missing `SHIM_DIR` Hang Hazard:** Omitting `ANTIGRAVITY_PLUGIN_SHIM_DIR` hangs `plugin-cli`. Every connector subprocess invocation must set a non-empty sentinel `ANTIGRAVITY_PLUGIN_SHIM_DIR=/nonexistent-hq-sentinel` alongside process-group deadlines (`Setpgid` + `cmd.WaitDelay`).
4. **Chat vs. `:4242` WebUI Approval Divergence:** Chat `hq approve` runs in a fresh Desktop tool environment, whereas `:4242` WebUI approvals execute inside `hq daemon`, which survives Desktop restarts with launch-time env (`sidecars/agy-hq-console/main.mjs:45-53, 144-146`), and `exec.Command(argv[0])` resolves `argv[0]` against the approving process's `PATH` rather than `cmd.Env`. Proposals therefore must bind `ExecContext` + resolved absolute `argv[0]` into SHA-256 and poll `requires_desktop_session: true` connectors from the Desktop-tied sidecar process.

---

## 4. Forensic Gap Catalog Against `origin/main`

### 4.1 Monolithic Coupling & Fail-Open Configuration (`RFC-0017a`)
1. **Hard-Compiled Silos (`kernel.go:2330-2410`, `cmd/hq/main.go:1611`, `hooks.go:889`):** `RunHeartbeat` hardcodes 30-minute 24/7 `gmail.RunPrecheck` and `gchat.RunPrecheck` loops; `morning-brief/SKILL.md` and `inbox-triage/SKILL.md` hardcode consumer `gmail`, `gcalendar`, `gchat`, and `gh` steps.
2. **Pure-Go YAML Fail-Open Hazard (`internal/kernel/daemon.go:142-283`):** The line-oriented YAML helper splits flow sequences on every comma (breaking `--fields=from,subject` or `from:(a,b)`) and silently ignores unknown keys—meaning a typo like `particpant_lock: true` fails open! Canonical manifests must use **`connector.json`** (`encoding/json` + `DisallowUnknownFields`).
3. **Project-Scoped Plugin Discovery Conflict (`RFC-0015`):** `RFC-0015` supports project-scoped `.agents/plugins/` installs alongside global `~/.gemini/config/plugins/`. Discovery requires explicit plugin enablement verification plus `<vault>/config.yaml` `connectors.trusted_plugins` and approval-gated SHA-256 pinning (`<vault>/private/connector_trust.json`) rather than regex path write-guards (which are shell-bypassable and collide with `hq upgrade`).
4. **Reference Driver & `WaitingOn` Defects:** `gmail.RunPrecheck` (`gmail.go:832-852`) and `gchat.RunPrecheck` (`gchat.go:1096-1116`) swallow `SearchMessages` errors and stamp `SeenMessageIDs` *before* triage finishes; Gmail direct OAuth REST (`gmail.go:1273-1439`) lacks `expiry` token refresh, metadata headers, full MIME decoding, and REST draft CRUD; `SweepWaitingOnObligations` (`goals.go:369-430`) cannot inspect connector threads before firing overdue alerts.

### 4.2 Untrusted-Content Laundering & Offer Gateway Bypasses (`RFC-0017b`)
- **Untrusted Laundering Paths (`A1`–`A5`):**
  - **`A1` (`gatekeeper.go:284-302`):** Precheck stdout (truncated to 300 runes) splices into worker prompts *before* taint is applied (`search_web`, `write_file`, `invoke_subagent`, `send_message` still permitted).
  - **`A2` (`dispatcher.go:29-48`, `events_dispatch.go:381-395`):** `OutboxDispatcher` delivers tainted worker summaries into `[HQ] Front Desk` via `SendUserCascadeMessage` as an **unfenced user turn**.
  - **`A3` (`hooks.go:440-442`, `reported.go:136`):** Delivered tainted titles inject into every `SessionStart`.
  - **`A4` & `A5` (`templates/triggers.yaml:3-8`):** `/morning-brief` runs as `mode: front_desk_cron` directly inside `[HQ] Front Desk`; feeding raw quarantined connector output or unvetted `hq-brief/*.md` instructions into Front Desk launders untrusted content into the highest-privilege session.
- **Offer & Subprocess Boundary Bypasses (`B1`–`B12`):**
  - **`B1` (`hooks.go:535`):** `reAllowedQuarantineHQCmd` allows raw `hq propose`, bypassing any offer-level `participant_lock`.
  - **`B2` (`gmail.go:369, 571-586`):** Regex extraction across sender-authored `From`/`To`/`Cc` headers allows arbitrary `Cc:` additions and display-name smuggling (`From: "ceo@acme.com" <x@evil.com>`).
  - **`B4`, `B5`, `B6`, `B8`:** Pre-approval remote draft creation mutates external servers prematurely; standalone `"{{param}}"` argv elements allow `--flag` injection; agent-chosen `--account`/`--connector` enables cross-account confused-deputy attacks; unbound `ack` and fuzzy `goals.go:258` `strings.Contains` `mark-replied` allow alert suppression.
  - **`B10` (`gmail.go:421-456`):** `tainted_sessions.json` is unlocked and returns an empty map on JSON parse error (torn write untaints **every** session!), and `convID == ""` is a silent no-op.
  - **`B11` (`gatekeeper.go:224`, `gmail.go:1052`):** Subprocesses inherit `ANTIGRAVITY_CSRF_TOKEN` and `ANTIGRAVITY_LS_ADDRESS`, letting any connector child call `SendUserCascadeMessage` to forge Front Desk user turns.

### 4.3 State Machine, Concurrency & Destructive Drain Bugs (`RFC-0017a` & `RFC-0017c`)
1. **No Single Commit Authority (`events.go:832-867`, `hooks.go:1478`):** `CompleteByConversation` marks *all* leased events `done` on any `Stop` (even 429/partial-batch stops), while `HasActiveOrCompletedDedup` ignores `StatusDropped` (`events.go:237, 384`), causing poison items to re-enqueue infinitely.
2. **Broken `GatekeeperEnvelope` Transport (`gatekeeper.go:225, 284-311`):** 300-rune truncation, `CombinedOutput()` stderr mixing, single `gatekeeper:<id>` `ScopeKey` serialization, and all-or-nothing exit-1 failure make stdout envelopes unsuitable for multi-connector polling.
3. **Single-Goroutine Head-of-Line Blocking & Unlocked Stores (`cmd/hq/main.go:1606-1652`, `outbox.go:320-335`, `goals.go:72-82`, `gatekeeper.go:50-60`):** Sequential subprocess polls stall fast timers/outbox delivery; `items.jsonl`, `goals.json`, and `trigger_cursors.json` use unlocked `os.WriteFile`.
4. **Quiet-Hours Config Bug & Destructive `DrainBriefRollup` (`outbox.go:103, 193, 578-604`, `templates/config.yaml:45-50`):** `ParseAttentionConfig` ignores `tz:` and leaves `QuietHours.Enabled == false` on every new Vault; `DrainBriefRollup` mutates items to `delivered` on read, losing overnight items if `collect` runs twice or a brief turn fails; `ThreadKey` burst superseding leaves superseded `Proposal` files approvable for 24h.

---

## 5. Summary of the Split Architecture (`RFC-0017a` / `0017b` / `0017c`)

1. **[RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md) — Connector Protocol, Trust Registry & Watcher Engine:** Canonical fail-closed `connector.json` (`DisallowUnknownFields` + `hq connector lint`), approval-gated SHA-256 `connector_trust.json`, `hq-connector-protocol/v1` + pure-Go `jsonmap` driver, scrubbed subprocess env + bounded process-group worker pool, sidecar-polled `requires_desktop_session` model, shared `flock` `internal/store`, direct per-item `events.Enqueue`, single-authority `hq watch ack` + `poisoned` set, quiet-hours/quota alignment, and reference `gmail`/`gchat`/`gcalendar` connectors.
2. **[RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md) — Provenance Taint, Context Queries & Offer Gateway:** Bind-time auto-taint (`Source: "connector:*"`) with opaque-ID prompts, fail-closed per-conversation taint files, nonce-delimited normalized `QuarantinedThread`, `low`-sensitivity `hq watch query`, batch-bound side-effect-free `hq offer stage` (removing raw `hq propose` under taint, RFC 5322 authenticated-participant lock, `ExecContext` + resolved `argv[0]` hash binding, `approvals.Supersede`, apply-time `inspect_thread` precondition), and nonce-fenced system outbox cards.
3. **[RFC-0017c](../design/0017c-lease-based-morning-brief-and-extensions.md) — Lease-Based Morning Brief & Structured Extensions:** Idempotent `hq brief collect` + two-phase `--lease` / `hq brief commit`, provenance-safe tainted reader JSON summarization (zero raw quarantined text in Front Desk), and structured `argv:` / explicitly trusted `brief.d` & `hq-brief` extensions.
