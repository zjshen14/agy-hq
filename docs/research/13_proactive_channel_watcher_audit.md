# 13 — General Watcher Mechanism, Quarantine Provenance & Pluggable Enterprise Connector Audit (Post-`RFC-0018`)

- **Date:** 2026-11-14
- **Baseline Audited:** `origin/main` (incorporating [RFC-0003](../design/0003-approval-bound-execution.md), [RFC-0005](../design/0005-gmail-integration.md), [RFC-0008](../design/0008-gchat-integration.md), [RFC-0014](../design/0014-proactive-engine-and-living-context.md), [RFC-0015](../design/0015-global-plugin-packaging.md), [RFC-0016](../design/0016-sidebar-conversation-archive-sync.md), and [RFC-0018](../design/0018-always-on-agent-runtime-hardening.md))
- **Companion Designs:**
  - [RFC-0017a — Modular Connector Protocol, CLI Transport Profiles & 0-Token Watcher Engine](../design/0017a-connector-protocol-and-watcher-engine.md)
  - [RFC-0017b — Provenance-Based Taint Propagation, Human-Presence Approval Proof & Offer Gateway](../design/0017b-provenance-taint-and-offer-gateway.md)
  - [RFC-0017c — Non-Destructive Lease-Based Morning Brief Collector & Structured Extensions](../design/0017c-lease-based-morning-brief-and-extensions.md)
  - [RFC-0017d — Companion Extension Packs, Unified Trust Registry & Host Portability](../design/0017d-enterprise-extensibility-and-portability.md)
- **Prior Research Referenced:** [10 — Meta Muse (`muse.ai`) Deep Dive](10_muse_ai_proactive_and_context_study.md), [14 — Anthropic Always-On Agents Gap Audit](14_anthropic_always_on_agents_gap_audit.md)

---

## 1. Executive Summary

High-frequency, low-noise proactive watching across communication channels, calendars, and counterparty threads—paired with concrete, 1-tap approval-bound offers—is a foundational capability of an always-on Personal Agent OS ([Research 10](10_muse_ai_proactive_and_context_study.md)).

Operating `agy-hq` across **both personal workstations and enterprise environments** surfaces three non-negotiable architectural requirements:
1. **Zero-Recompile Enterprise Modularity & Host Portability:** Even for the same vendor product (such as **Gmail**, **Google Chat**, or **Google Calendar**), enterprise deployments diverge from consumer setups: corporate environments route authentication through Antigravity-managed Desktop SSO shims (`AGY_ACCOUNT=user@acme.com`) or custom corporate CLIs, enforce DLP policies (`draft_only` vs. direct send), remap model catalogs (`routing.tiers`), relocate `ANTIGRAVITY_APP_DATA_DIR`, run on hosts without `node`, and bundle companion tools (Slack, Jira / `twg`, GitHub Enterprise, Gerrit, or internal incident CLIs) via companion plugins whose code cannot live inside open-source `agy-hq`.
2. **Provenance-Enforced Quarantine & Human-Presence Approval Integrity:** Proactive watching continuously ingests untrusted external inputs. Two rounds of adversarial code and runtime auditing proved that static XML tags (`<untrusted_content>`) are **not** a security boundary, post-spawn taint binding races turn 1 (`N2`), agent-controlled `--session` / `--no-session` timers escape taint (`N3`), and unconditional CLI `hq approve` without human-presence proof (`N1`) allows prompt-injected untainted sessions to self-approve trust ceremonies or outbound mutations.
3. **Single-Authority State & Non-Destructive Collection:** Overlapping "processed" markers across `Stop` hooks, unlocked JSONL stores, and read-destructive `DrainBriefRollup` calls cause silent item loss or infinite poison retries unless unified behind locked stores (`internal/store`), batch-bound `hq watch ack`, and lease-based `hq brief collect --lease` / `commit`.

---

## 2. Verified `RFC-0018` Baseline Primitives in `bin/hq`

| Core OS Capability | Verified Live Code on `main` | Composition Across `RFC-0017a`–`0017d` |
|---|---|---|
| Per-scope worker serialization & lease recovery | `events.Enqueue` + `LeaseNextBatches` (`DefaultLeaseTTL = 15 * time.Minute` in `internal/events/events.go:31`, renewed in `PreToolUse` at `internal/hooks/hooks.go:693-695`) | `hq watch precheck` enqueues per-item events directly (`ScopeKey: "triage:<connector>:<account>"`, `Source: "connector:<id>"`, `Metadata["tier"]`) with lease-time `[hq-spawn:<token>]` taint binding. |
| Step/range cron & `CRON_TZ=` scheduling | `ScheduleTrigger` (`internal/kernel/daemon.go:20`) and cron matcher (`internal/kernel/gatekeeper.go:62-113`) **already support** `*/n`, `a-b`, `a-b/n`, comma lists, and `CRON_TZ=` | Extended with virtual merge (`core < packs < vault`), `Enabled *bool`, `Tier`, `CatchUp`, and `hq triggers pause|resume` (`RFC-0017d`). |
| Lean `[Auto]` worker context & anti-nagging | `leanAuto` `SessionStart` (`internal/hooks/hooks.go:223-275`) + `outbox.FormatReportedDigest` (`internal/outbox/reported.go:95`) | Reused across connector triage workers, binding `[hq-spawn:<token>]` taint before turn 1 and nonce-fencing tainted titles in `SessionStart`. |
| Intra-day deadline & promise check-ins | `hq timers set --kind=follow_up` (`internal/timers/timers.go`) | Locked under taint to caller `--session`, denying `--no-session`, enforcing enum prompts, and inheriting session taint (`N3`). |
| Shell-free `CommandArgv` execution & revision loop | `Proposal.CommandArgv`, `OriginSkill`, `ResumeOnDecide`, `RejectProposal` (`internal/approvals/approvals.go:121-131, 240-355, 373, 381-393`) | Hardened with `N1` human-presence HMAC capability, `ExecContext` + resolved `argv[0]` + `(version, commit)` self-pinning, `ThreadKey` `Supersede`, and apply-time `inspect_thread` preconditions (`RFC-0017b`). |
| Event-driven Front Desk push & policy harness | `OutboxDispatcher` (`internal/outbox/dispatcher.go:29-119`), `hq daemon` `wakeCh` (`cmd/hq/main.go:1606-1652`), and `hq outbox eval` (`internal/outbox/eval.go`) | Hardened with `flock` atomic persistence, `tz:`/`enabled` parser fix, and nonce-fenced system cards for tainted items. |

---

## 3. Empirical Runtime Probes (Plugin-CLI SSO Shim, Sidecar Env & Strict Plugin Validator)

### 3.1 Antigravity Plugin-CLI SSO Shim Lifecycle (Round 1 Probes)
Read-only black-box `env -i` invocations of `gmail readonly labels --json` (capturing exit codes and redacted stderr only) established:

| Environment Passed to `language_server plugin-cli <tool> ...` | Observed Behavior |
|---|---|
| `PATH` only (no `ANTIGRAVITY_CREDENTIAL_SCOPE`, no `ANTIGRAVITY_PLUGIN_SHIM_DIR`) | **Hangs indefinitely** (>25s timeout, `rc=124`) |
| `PATH` + `ANTIGRAVITY_CREDENTIAL_SCOPE` (no `ANTIGRAVITY_PLUGIN_SHIM_DIR`) | **Hangs indefinitely** (>25s timeout, `rc=124`) |
| `PATH` + `ANTIGRAVITY_CREDENTIAL_SCOPE` + live `ANTIGRAVITY_PLUGIN_SHIM_DIR` | Succeeds (`rc=0`) |
| `PATH` + `ANTIGRAVITY_CREDENTIAL_SCOPE` + `SHIM_DIR` + `LS_ADDRESS` / `CSRF_TOKEN` | Succeeds (`rc=0`; `LS_ADDRESS` and `CSRF_TOKEN` are **not required**) |
| `PATH` + `ANTIGRAVITY_CREDENTIAL_SCOPE` + **deleted** or `/nonexistent` `SHIM_DIR` | **Succeeds (`rc=0`)** |
| `PATH` + invalid `ANTIGRAVITY_CREDENTIAL_SCOPE` + `/nonexistent` `SHIM_DIR` | Fast-fails (`rc=1`) with `"needs authentication"` + LLM-targeted `<plugin-card ...>` stderr text |

### 3.2 Sidecar Environment & Strict Plugin Validator (Round 2 Probes)

| Empirical Probe | Measured Result | Architectural Consequence |
|---|---|---|
| Live `agy-hq-console` sidecar & `hq daemon` `/proc/<pid>/environ` (key names only) | Exports `ANTIGRAVITY_{AGENTAPI_EXE,APP_DATA_DIR,CSRF_TOKEN,LS_ADDRESS,SIDECAR_UI_TOKEN,SIDECAR_WEB_PORT,...}`, `ELECTRON_RUN_AS_NODE`, `PLUGIN_ROOT`. **Zero `ANTIGRAVITY_CREDENTIAL_SCOPE` and zero `ANTIGRAVITY_PLUGIN_SHIM_DIR`** (built-in swarm sidecar identical). | **`N4`:** Sidecars **cannot** poll `requires_desktop_session: true` connectors. Desktop-SSO polling must piggyback on conversation-scoped hooks (detached singleflight `hq watch precheck --desktop-only`) or `builtin: "schedule"` triggers (`RFC-0017d §5.4`). |
| Desktop sidecar process argv | Runs under Antigravity's bundled runtime (`antigravity --import=.../register_loader.js main.mjs`, `ELECTRON_RUN_AS_NODE=1`), not `/usr/bin/node`. | Headless/CLI `node`-less support (`#5`) is cleanly solved by Go-native `hq sidecar console` + updating `plugin.ValidateBundle` (`internal/plugin/plugin.go:289-303`) + `builtin: "schedule"` sidecars (`RFC-0017d §5.3`). |
| `agy plugin validate --strict` (`agy 1.5.8`) | **Passes** extra root files/dirs (`hq-extension.json`, `hq-connectors/`, `hq-brief/`) and custom sidecar `command` / `builtin: "schedule"`. **Fails `--strict`** on any unknown key inside `plugin.json`. | Companion extension packs must leave `plugin.json` untouched and index components via root `hq-extension.json` (`RFC-0017d §2.1`). |

---

## 4. Forensic Gap Catalog Against `origin/main`

### 4.1 Monolithic Coupling, Dual-Config Drift & Fail-Open YAML (`RFC-0017a`)
1. **Hard-Compiled Silos & Dual Account Stores:** `RunHeartbeat` (`kernel.go:2330-2410`) hardcodes 30m `gmail`/`gchat` loops; `private/{gmail,gchat}/accounts.json` duplicates account configuration separately from connector specs; `hq gmail init` (`gmail.go:248, 291-330`) downloads an unpinned zip from a public URL without consent (`#2`); and `gchat` lacks a generic `cli` transport (`#3`).
2. **Pure-Go YAML Fail-Open Hazard (`internal/kernel/daemon.go:142-283`):** Splits flow lists on commas and silently ignores unknown keys (turning `particpant_lock: true` or `enabeld: false` into silent bypasses).
3. **Reference Driver & `WaitingOn` Defects:** `gmail.RunPrecheck` (`gmail.go:832-852`) and `gchat.RunPrecheck` (`gchat.go:1096-1116`) swallow errors and stamp `SeenMessageIDs` before triage completes; Gmail direct OAuth REST lacks `expiry` refresh, metadata headers, MIME decode, and REST draft CRUD; `SweepWaitingOnObligations` (`goals.go:369-430`) cannot run `inspect_thread`.

### 4.2 Untrusted Laundering, Taint Races & Self-Approval Bypasses (`RFC-0017b`)
- **Untrusted Laundering & Taint Escapes (`A1`–`A5`, `N2`, `N3`):**
  - **`A1` & `N2` (`gatekeeper.go:284-302`, `events_dispatch.go:227-247`):** Splicing stdout into worker prompts before taint (`A1`) and calling `BindBatchConversation` *after* `SpawnTask` returns (`N2`) leaves turn 1 untainted.
  - **`A2`, `A3`, `A4`, `A5`:** Unfenced Front Desk user-turn outbox delivery (`A2`), unfenced `reported.jsonl` in `SessionStart` (`A3`), raw quarantined text or default-unquarantined `argv` brief extensions entering `front_desk_cron` `/morning-brief` (`A4`), and unpinned `hq-brief/*.md` prompts (`A5`).
  - **`N3` (`cmd/hq/proactive_cli.go:924-969`):** `hq timers set` accepts `--session=<any_conv>` (injecting text into Front Desk) and `--no-session` (spawning a fresh untainted worker with attacker text).
- **Offer, Subprocess & Self-Approval Bypasses (`B1`–`B12`, `N1`):**
  - **`N1` (`hooks.go:531, 536, 804-810`, `cmd/hq/main.go:503-521`):** `PreToolUse` blocks `/api/approve` but leaves CLI `hq approve` unconditional across untainted, `[Auto]`, and subagent sessions, and omits `private/trust.json` from the credential write guard—allowing prompt-injected sessions to self-approve proposals or rewrite trust pins directly.
  - **`B1`–`B11`:** Raw `hq propose` under taint (`B1`), regex `From`/`Cc` parsing (`From: "ceo@acme.com" <x@evil.com>`, `B2`), pre-approval remote drafts (`B4`), standalone `"{{param}}"` flag injection (`B5`), agent-chosen `--account` steering (`B6`), unbound `ack` / fuzzy `mark-replied` (`B8`), unlocked fail-open `tainted_sessions.json` (`B10`), and subprocess `CSRF_TOKEN` / `LS_ADDRESS` inheritance (`B11`).

### 4.3 State Machine, Concurrency & Destructive Drain Bugs (`RFC-0017a` & `RFC-0017c`)
- `CompleteByConversation` (`events.go:832-867`) marks all leased items `done` on mid-batch stops; `HasActiveOrCompletedDedup` ignores `StatusDropped` (`events.go:237, 384`); `items.jsonl`, `goals.json`, and `trigger_cursors.json` lack `flock`; `ParseAttentionConfig` (`outbox.go:103, 193`) ignores `tz:` leaving quiet hours disabled; and `DrainBriefRollup` (`outbox.go:578-604`) mutates items to `delivered` on read.

### 4.4 Enterprise Extensibility & Host Portability Gaps (`RFC-0017d`, Issues `#1`–`#7`)
- **`#1` (Tier Routing Unwired):** `config.yaml` `routing.tiers` is unread (`ResolveModel` in `kernel.go:1945-2060`, `timers.go:197`, `events_dispatch.go:359`, `templates/triggers.yaml`, sidecars, and `migrate.go:419` hardcode concrete model IDs).
- **`#4` (No Companion Pack Standard):** Companion plugins need one strict `hq-extension.json` with monotonic security merging (`packs` can only tighten; Vault loosens with justification), `hq dispatch resolve`, `SKILL.md` step-0 trampolines, and role-scoped `<= 4 KB` `front_desk_overlays`.
- **`#5` & `#6` (`node` Dependency & Relocated Roots):** CLI hosts may lack `node`; `vault.go`, `plugin.go`, `doctor.go`, and `gmail.go` hardcode `~/.gemini`; and **`hooks.go:1105`** checks Front Desk brain writes via substring `"/.gemini/antigravity/brain/"` instead of a canonical prefix check against `Roots.BrainDir`.
- **`#7` (Schedule Pause/Resume):** Needs locked JSON `<vault>/extensions.json` overrides (`hq triggers pause|resume <fqid>`), untouched FQID cursors, and `front_desk_cron` disarm lists + `[hq-trigger:<fqid>]` step-0 guards.

---

## 5. Summary of the Four-Part Design Suite (`RFC-0017a`–`0017d`) & Pre-Implementation Audit Refinements

1. **[RFC-0017a](../design/0017a-connector-protocol-and-watcher-engine.md) — Connector Protocol, `cli` Transports & Watcher Engine:** Manifest (`connector.json`) vs. private account data (`<vault>/private/connectors/<id>/instance.json`, `token_refs: {read, send, webhook}`) split, `transport.kind: "cli"` unifying `hq gmail` (`#2`, `auto_install_plugin: false`) and 1-step `hq gchat propose` (`#3`), `/dev/fd/3` `ExtraFiles` payload pipe, `hq-connector-protocol/v1` + `jsonmap`, scrubbed worker pool, ranked `internal/store`, paginated `events.EnqueueBatch` (`MaxBatchEvents=25`, `PollCursor` vs. `AckedItemIDs`), single-lock `hq watch ack` (`Metadata["acked"]="1"`) + `PoisonedItems`, and quiet-hours alignment.
2. **[RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md) — Provenance Taint, Human-Presence Approval & Offer Gateway:** `SpawnTokenHash` + taint-on-ingress (`hq watch read`) + first-`PreToolUse` `< 64 KB` transcript binding (`N2`), `<vault>/private/taint/` fail-closed `O_EXCL` taint files, `PreToolUse` `matcher: ".*"` deny-by-default tainted tool allowlist (`run_command`, `read_file`) + `strictArgv` + `env -u` shim/credential scrub, `ask_question` nonce rewrite + `PreToolUse` `CommandLine` `--cap` capability (`N1`), server-side tainted `cmd/hq` flag lockdown (`N3`), `HashVersion: 2` + `approvals.Supersede` + apply-time `inspect_thread` check, and anti-cue sanitized nonce-fenced system cards.
3. **[RFC-0017c](../design/0017c-lease-based-morning-brief-and-extensions.md) — Lease-Based Morning Brief & Structured Extensions:** Zero-write `hq brief collect` peek + `LeaseTokenHash`/`LeaseConvID` `--lease` / `<vault>/notes/`-confined `hq brief commit`, `< 4h` proposal TTL alerts, provenance-safe tainted reader JSON summarization, and structured `brief.d` / pack `hq-brief` extensions (`quarantined: true` default).
4. **[RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md) — Companion Extension Packs, Unified Trust & Host Portability:** `hq-extension.json` (`plugin.json` untouched), two-lattice virtual merge + `<vault>/extensions.json` (`#4`, `#7`), unified `<vault>/private/trust.json` (`hq` pinned by `(version, commit)` via `os.Executable()`), `hq dispatch resolve` + `SKILL.md` trampolines + `front_desk_overlays`, `internal/routing.Resolve` (`#1`), hermetic `internal/hostpaths` + own-conversation `hooks.go` brain/transcript guard (`#6`), Go-native `hq sidecar console` (`#5`), and `Stop`-only detached singleflight Desktop-SSO polling (`N4`).
