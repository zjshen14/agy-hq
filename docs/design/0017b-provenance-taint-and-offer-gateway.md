# RFC-0017b: Provenance-Based Taint Propagation, Nonce-Rewritten Approval Proof & Offer Gateway

- **Status:** Proposed (Phase 1 Review — Opus 5.5 Security & Systems Hardened)
- **Date:** 2026-11-14
- **Suite:** Part 2 of 4 — see also [RFC-0017a (Connector Protocol & Watcher Engine)](0017a-connector-protocol-and-watcher-engine.md), [RFC-0017c (Lease-Based Morning Brief & Extensions)](0017c-lease-based-morning-brief-and-extensions.md), and [RFC-0017d (Companion Extension Packs, Unified Trust & Host Portability)](0017d-enterprise-extensibility-and-portability.md)
- **Depends On:** [RFC-0003 (Approval-Bound Execution)](0003-approval-bound-execution.md), [RFC-0017a](0017a-connector-protocol-and-watcher-engine.md), [RFC-0017d](0017d-enterprise-extensibility-and-portability.md), [RFC-0018 (Always-On Agent Runtime Hardening)](0018-always-on-agent-runtime-hardening.md)
- **Companion Research:** [13 — General Watcher Mechanism, Quarantine Provenance & Pluggable Enterprise Connector Audit](../research/13_proactive_channel_watcher_audit.md)

---

## 1. Security Threat Model & Explicit Trust Boundary

Proactive watching processes untrusted third-party emails, chats, tickets, and calendar invites continuously without human initiation. Forensic host and code audits ([Research 13 §4.2](../research/13_proactive_channel_watcher_audit.md)) established three core facts:
1. **Explicit Threat Boundary (Tainted vs. Same-UID Untainted Sessions):**
   - An **untainted** developer session with unrestricted `run_command` executes as the local Unix user—its shell holds live `ANTIGRAVITY_*` IPC credentials and can access local files. Against an untainted session, `PreToolUse` approval capabilities (`N1`) defend against **model-level / prompt-injected tool invocations** (`hq approve`, `hq propose`, `write_file`), not arbitrary same-UID C/shell code.
   - The **hard security boundary** is the **provenance-tainted session** (`tainted == true`), which has **no general shell** (`strictArgv` token allowlist + server-side `cmd/hq` flag enforcement + `matcher: ".*"` deny-by-default tool allowlist + `bwrap` `private/` mask + `env -u` IPC credential scrub).
2. **Host Hook Contract Realities (`S1`–`S5`):**
   - Antigravity records `ask_question` user taps as `source=MODEL, type=GENERIC` tool results (whereas `USER_EXPLICIT` steps can be machine-injected via `SendUserCascadeMessage` timers, continuations, or `hq spawn`).
   - `PreToolUse` runs as a separate child process (cannot export env vars) but natively supports JSON **`"overwrite"`** to rewrite `ask_question` options and `run_command` `CommandLine`.
   - `SessionStart` does not carry `lastUserInput`, and `hooks.json` explicit tool matchers previously missed `webview_browser`, `swarm`, and `manage_task`.

---

## 2. Taint-on-Ingress, First-`PreToolUse` Token Binding & Append-Only Taint Store

### 2.1 CAS Spawn-Token Binding (`SpawnTokenHash`) & Taint-on-Ingress (`A1`, `N2`, `S5` Closed)
1. **Hash-Only Spawn Token at Lease Time:** `LeaseNextBatches` mints a 128-bit single-use `spawn_token`, stores **only `SpawnTokenHash = sha256(spawn_token)`** in readable `events.json`, excludes `EventIDs`-bearing tasks from heuristic `LinkPendingSpawnTask` (`kernel.go:616-641`), and prefixes the opaque-ID worker prompt with `[hq-spawn:<spawn_token>]` (zero `Sender`/`Subject` text).
2. **Primary Guarantee — Taint-on-Ingress (`hq watch read` & Legacy Readers):**
   - Caller identity comes **strictly from `os.Getenv("ANTIGRAVITY_CONVERSATION_ID")`** (never `--conv` overrides outside hermetic temp-vault tests; `P0-3`).
   - Before emitting a single untrusted byte, `hq watch read` CAS-binds the batch via `events.BindBatchByToken` (failing closed on `ErrTokenBoundElsewhere`), writes `<vault>/private/taint/<sha256(convID)>.json` + append-only `<vault>/private/taint/ledger.jsonl` (`fsync`), and only then formats output.
3. **First-`PreToolUse` Bounded Transcript Binding:** On a conversation's first `PreToolUse` (cached via `.checked` marker), `hooks.go` performs a bounded `< 64 KB` read of `payload.TranscriptPath`'s initial `USER_INPUT` (or `payload.LastUserInput`) and idempotently calls `BindBatchByToken` + taint persistence before tool 1 executes. Lease renewal (`RenewLeaseByConversation`) runs **after** writing the hook decision using non-blocking `LOCK_EX|LOCK_NB` (`P0-4`).

### 2.2 Protected Monotonic Taint Store (`<vault>/private/taint/`, `B10`, `S8` Closed)
Replaces unlocked `memory/tainted_sessions.json` with all-session-guarded `<vault>/private/taint/` (masked in `bwrap`):
- Per-conversation `<sha256(convID)>.json` (`O_EXCL`, `0600`, `fsync`) + append-only `<vault>/private/taint/ledger.jsonl`.
- **`IsTainted(convID)`** evaluates `true` if the per-conversation file exists, `ledger.jsonl` records `convID`, legacy `tainted_sessions.json` contains `convID` (1-release compatibility), or any I/O/JSON error occurs on an existing taint record (fail-closed). Taint is monotonic across lease expiry so zombie workers remain locked down.

### 2.3 Wildcard Hook Matcher, Deny-by-Default Tools & `strictArgv` (`S4`, `S7`, `S14`, `S20` Closed)
- **`hooks.json` `PreToolUse` `matcher: ".*"` + Fail-Closed Shell Fallback:** Gates every tool (`webview_browser`, `swarm`, `manage_task`, `read_resource`, `mcp_*`, and future tools), returning `{"decision":"deny"}` if `bin/hq` is unavailable.
- **Tainted Tool Allowlist:** Only `run_command` and `read_file` (restricted strictly to the active plugin `skills/` directory and the conversation's own canonical artifact directory) are permitted under taint; all other tools are denied.
- **Batch-Bound `hq watch read` Nonce Fence:** Strips `<script>`, `<style>`, HTML comments, hidden spans, and control characters, rendering `QuarantinedThread` inside a fresh 128-bit `<<<HQ_UNTRUSTED_NONCE_<32hex>>>` fence.

---

## 3. Data-Minimized Context Queries Under Taint (`B3` Mitigation)

1. **`hq watch query --connector=<id> --query=<name>` Sensitivity Classes:** Tainted sessions may invoke **only** `"sensitivity": "low"` queries on locked accounts (e.g., `gcalendar` `freebusy` `[start, end, busy]` windows only).
2. **Server-Side Narrowed `hq memory lookup`:** Inspects `taint.IsTainted` inside `cmd/hq` and projects **routing metadata only** (`entity_slug`, `display_name`, `relationship_tag`, `known_channels`).

---

## 4. Hardened Offer Gateway (`hq offer stage`) & Nonce-Rewritten `hq approve`

### 4.1 `strictArgv` Shell Tokenizer, Env Scrub & `cmd/hq` Tainted Enforcement (`B1`, `N3`, `S7`, `S9`, `S14` Closed)
1. **`strictArgv` + IPC Env Scrub in `PreToolUse`:** Under taint, `run_command` requires every whitespace token to match `^[A-Za-z0-9._:/=@,+-]{1,256}$` with no leading `@` and zero shell metacharacters/redirections (`< > ' " \ * ? ~ { } # ; | & $`), targeting only `(watch read | watch query | offer stage | outbox add | watch ack | timers set | goals mark-replied)`. Both tainted and `[Auto]` `run_command` calls are rewritten via `PreToolUse` `"overwrite"` with `env -u ANTIGRAVITY_CSRF_TOKEN -u ANTIGRAVITY_LS_ADDRESS -u ANTIGRAVITY_AGENTAPI_EXE ANTIGRAVITY_PLUGIN_SHIM_DIR=/nonexistent-hq-sentinel -- ...`.
2. **Server-Side `cmd/hq` Flag Gate (`S9`):** Every `hq` subcommand checks `taint.IsTainted(vaultDir, envConvID)` directly in Go (`flag.Visit`): under taint, `propose`/`approve`/`spawn`/`subagent`/`exec-sandboxed` exit `1`, and `hq timers set` permits **only** `--kind=follow_up --reason=<enum> --item=<id> --in=<dur>` (forcing `SessionID = envConvID`, rejecting `--no-session` / `--kind=schedule` / `cancel`, and inheriting taint on fire).

### 4.2 Batch-Bound Scope, RFC 5322 Participant Lock & Zero Pre-Approval Side Effects
1. **Server-Derived Target (`B6`):** `(connector, account, thread_key, staged_head_item_id)` load strictly from the caller's leased batch state (`--conv` overrides removed on `propose`/`read-quarantined`, `S6`).
2. **RFC 5322 Authenticated Participant Lock (`B2`, `B7`):** `"email"` recipients parse via `net/mail.ParseAddressList` (`addr.Address` only, rejecting display-name smuggling like `From: "ceo@acme.com" <x@evil.com>`); when `participant_lock == true`, every recipient must belong to `AuthenticatedParticipants` computed by a hash-pinned driver:
   $$\text{AuthenticatedParticipants} = \text{AccountOwnerSentRecipients}(\text{thread}) \cup \{ m.\text{From} \mid m.\text{DKIMOrSPFAligned} \}$$
3. **Side-Effect-Free Staging (`B4`):** Writes immutable payload JSON to `<vault>/approvals/payloads/<proposal_id>.json` (`0600`, guarded in `hooks.go`), passed at approve time via `/dev/fd/3` `ExtraFiles` pipe (`S11`).

### 4.3 Nonce-Rewritten `ask_question` Capability (`S1`, `S2`), `HashVersion: 2` & Superseding
- **All-Session Path Guards (`S3`, `P0-10`):** Blocks direct LLM tool/shell access to `<vault>/private/{gmail,gchat,credentials,connectors,taint,approval_asks,approval_caps,trust.json,pii-denylist.txt,daemon_token}`, `<vault>/approvals/`, `$XDG_CONFIG_HOME/agy-hq/roots.json`, and `**/.system_generated/**`; scopes Front Desk brain writes strictly to `canonical(Roots.BrainDir/<convID>)` with `Lstat` no-symlink verification.
- **Hook-Rewritten `ask_question` Nonce + `CommandLine` `--cap` Overwrite (`N1`, `S1`, `S2` Closed):**
  1. When `PreToolUse` intercepts `ask_question` referencing pending proposal `<id>`, it rewrites `questions[].options` via `"overwrite"` to include a fresh 6-hex nonce (`✅ Approve <id> · code <NONCE6>`) and records an `O_EXCL` `0600` `AskRecord` (`ConvID`, `AskStepIdx`, `ProposalID`, `FullSHA256`, `NonceHash`) in `<vault>/private/approval_asks/`.
  2. When `PreToolUse` intercepts canonical `hq approve <id> --sha256=<pfx>` ($\ge 12$ hex; denied in `[Auto]`/subagent/tainted sessions), it reverse-scans the bounded tail ($\le 2\text{ MiB}$) of `payload.TranscriptPath` (`transcript_full.jsonl` on `truncated_fields`) for the `DONE` `GENERIC` step matching `AskStepIdx` and exact label `✅ Approve <id> · code <NONCE6>`, claims an `O_EXCL` per-`(convID, AskStepIdx)` used marker, writes `<vault>/private/approval_caps/<id>.json` (`O_EXCL`, `0600`), and returns `"overwrite": {"CommandLine": "<os.Executable()> approve <id> --sha256=<FullSHA256> --cap=<nonce>"}`.
  3. `ApproveProposal` atomically consumes `<id>.json` via `os.Rename`, constant-time-verifying `NonceHash`, `FullSHA256` (re-read post-claim), and `VaultCanon`. (`:4242` WebUI calls `ApproveFromTrustedDaemon` in-process; isolated unit/CUJ tests with `vault.IsTempPath(EvalSymlinks(vaultDir)) && ANTIGRAVITY_CONVERSATION_ID == ""` retain direct CLI testability unless `HQ_FORCE_CAP_CHECK=1` is set.)
- **Versioned Proposal Hash (`HashVersion: 2`, `S10`, `P0-7`), Idempotency & Superseding:**
  - Existing `HashVersion == 0` proposals verify via the legacy hash (zero upgrade breakage); new `HashVersion: 2` proposals bind `(ActionType, Command, Argv, WorkDir, ConvID, ResumeOnDecide, Continuation, ExpiresAt, ExecContext, ResolvedBinaryPath, BinaryIdentity, PayloadSHA256, StagedHeadItemID)`.
  - `IdempotencyKey` uses an `O_EXCL` `.idem/<sha>` marker; `approvals.Supersede(ThreadKey)` atomically renames prior pending proposals to `superseded/`; and `hq approve` runs `inspect_thread --after=<StagedHeadItemID>` before executing offers.

---

## 5. Nonce-Fenced System Cards & Anti-Cue Sanitization (`A2`, `A3`, `S21` Closed)

1. **Provenance-Tagged `OutboxItem` & Dedup Isolation (`P1-14`):** `hq outbox add` derives `Tainted = true` server-side from `taint.IsTainted` (never a CLI flag) and deduplicates tainted items strictly by `(Connector, Account, ThreadKey)` so attacker-crafted titles cannot suppress unrelated alerts.
2. **Nonce-Fenced System Card (`OutboxDispatcher` & `FormatReportedDigest`):** Strips proposal/command cue tokens (`prop-...`, `<<<`, `HQ_`, `[hq-`) from `BoundedSubject` ($\le 120$ runes) and encloses untrusted previews inside a fresh 128-bit `<<<HQ_CARD_NONCE_<32hex>>>` fence under `[HQ SYSTEM NOTIFICATION — QUARANTINED CHANNEL ALERT]`.
3. **Vendor-Neutral `.agents/plugins/agy-hq/skills/inbox-triage/SKILL.md`:** Uses the single quarantined loop, non-overridable by companion packs.

---

## 6. Verification Plan (`CUJ-31`)

- **`TestCUJ31_ProvenanceTaintAndHardenedOfferGateway`:** Verifies `ask_question` nonce `overwrite` + `--cap` single-use atomic consume (blocking timer/continuation/`spawn` `USER_EXPLICIT` forgeries, tap replay, `hq status && hq approve`, and `.system_generated` writes under `HQ_FORCE_CAP_CHECK=1`), taint-on-read + first-`PreToolUse` `BindBatchByToken` + `<vault>/private/taint/ledger.jsonl`, `matcher: ".*"` blocking `webview_browser`/`swarm`/`manage_task`, `strictArgv` blocking `>` redirection, `cmd/hq` blocking tainted `timers set --kind=schedule`, RFC 5322 `AuthenticatedParticipants` lock, `HashVersion: 2` + v0 upgrade compatibility, `/dev/fd/3` payload pipe, `Supersede`, `inspect_thread` precondition, and nonce-fenced system cards.
