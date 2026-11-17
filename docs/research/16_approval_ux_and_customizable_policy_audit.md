# Research 16: Empirical Audit of Outbound Approval UX Friction, Native Antigravity Coexistence & Customizable Policy Design

- **Status:** Complete
- **Date:** 2026-11-16
- **Companion RFC:** [RFC-0021: Customizable Outbound Approval Policy, Native Coexistence & Lifecycle Hardening](../design/0021-customizable-approval-policy-and-ux.md)
- **Prior Specs Reviewed:** [RFC-0003](../design/0003-approval-bound-execution.md), [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md), [RFC-0017d](../design/0017d-enterprise-extensibility-and-portability.md), [RFC-0018](../design/0018-always-on-agent-runtime-hardening.md)

---

## 1. Executive Summary & Audit Scope

Following several weeks of live multi-agent operation under [RFC-0003](../design/0003-approval-bound-execution.md) and [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md), two concrete operational failure modes emerged in the Vault proposal queue (`Pending Approvals`):

1. **Duplicate & Conflicting Confirmation Surfaces:** Agents operating under Antigravity's `AGENT_PERMISSION_PRESET_TURBO` preset routinely bypassed `hq propose`—either intentionally via alternative commands/browser automation not covered by `PreToolUse`, or after `hq approve` failures—leaving orphaned pending proposals behind even after the underlying action had shipped. Meanwhile, native `ask_question` UI prompts were silently overwritten by `PreToolUse`, destroying multi-question forms and custom options.
2. **Stale & Non-Executable Queue Entries:** Non-CLI browser/social tasks produced synthetic "dummy `echo`" proposals that could not execute on approval (`0` security enforcement and dead Web Console buttons), while 30-minute human-presence TTLs rejected real user approvals submitted 40–69 minutes after staging.

This audit examines the full approval pipeline across:
- **Core approval & presence engine:** [`internal/approvals/approvals.go`](../../internal/approvals/approvals.go), [`internal/approvals/presence.go`](../../internal/approvals/presence.go), [`internal/offer/offer.go`](../../internal/offer/offer.go)
- **Hook enforcement & quarantine:** [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go), [`internal/hooks/quarantine.go`](../../internal/hooks/quarantine.go), [`.agents/plugins/agy-hq/hooks.json`](../../.agents/plugins/agy-hq/hooks.json)
- **CLI, Console & Agent Rules:** [`cmd/hq/main.go`](../../cmd/hq/main.go), [`internal/webui/webui.go`](../../internal/webui/webui.go), [`.agents/plugins/agy-hq/rules/AGENTS.md`](../../.agents/plugins/agy-hq/rules/AGENTS.md)
- **Configuration & Extensibility:** [`templates/config.yaml`](../../templates/config.yaml), [`internal/extension/pack.go`](../../internal/extension/pack.go)

---

## 2. Empirical Failure Taxonomy (`F1`–`F6`)

Forensic inspection of two distinct waves of stuck Vault proposals (`Wave 1`: 4 multi-category proposals; `Wave 2`: 3 evening browser social-post proposals), workspace transcripts, `interventions.jsonl`, and GitHub Issue `#28` surfaced six systemic root causes (`F1`–`F6`). All examples below use synthetic identifiers.

| ID | Failure Mode | Empirical Incident Pattern | Codebase Root Cause | Operational Consequence |
| :--- | :--- | :--- | :--- | :--- |
| **`F1`** | **Policy / Hook Asymmetry & Native Bypass** | Wave 1 proposals for a version tag/release (`prop-1794800001-a1b2c3d4e5f6`), static blog publish (`prop-1794800002-b2c3d4e5f6a1`), and GitHub issue+reply (`prop-1794800004-d4e5f6a1b2c3`) stayed `pending` even though the release tag landed via native CLI/git push and the other actions executed through unblocked paths. | [`.agents/plugins/agy-hq/rules/AGENTS.md`](../../.agents/plugins/agy-hq/rules/AGENTS.md) §4 broadly requires `hq propose` across publishes, releases, deploys, and PR merges, whereas [`outboundCommandPatterns`](../../internal/hooks/hooks.go#L619-L632) only intercepts a narrow hardcoded CLI regex list (`gh pr merge`, `gh release create`, `vercel --prod`, `gmail`/`gchat`, `hq rsi apply`, etc.). Reversible own-repo pushes (`git push`), blog deploys via `git`/`scp`, and interactive `gh issue create` fall through unblocked, creating split-brain state. | Users approve once natively in Antigravity or watch the agent finish cleanly, only to find "ghost" pending items cluttering `CONSOLE.md` and `:4242` Web Console. |
| **`F2`** | **Dummy No-Op `--cmd` Placeholders for Browser/MCP Actions** | Wave 1 (`prop-1794800003-c3d4e5f6a1b2`) and Wave 2 browser social-post proposals contained commands like `echo 'Approved: post announcement to community board via browser'` (or quoted prose blobs). | [`cmd/hq/main.go`](../../cmd/hq/main.go#L508) rejects `hq propose` without `--cmd` (unless `--draft` or `--rsi-id` is set). When rules direct agents to stage browser/MCP actions via `hq propose`, models fabricate a harmless `echo`/`printf`/`true` no-op so CLI validation succeeds. | Zero cryptographic binding (`ApproveProposal` executes `echo` instead of the real post), zero deterministic completion signal, and clicking **Approve** on `:4242` silently runs a no-op with 0 follow-through. |
| **`F3`** | **30-Minute `AskTTL` vs. 24-Hour `ProposalTTL` Mismatch** | All three Wave 2 social-post proposals (`prop-1794800005-e5f6a1b2c3d4`, `prop-1794800006-f6a1b2c3d4e5`, `prop-1794800007-0718293a4b5c`) had genuine user taps on `Approve (code:...)` in `ask_question` 40–69 minutes after the prompt rendered. `VerifyAskAndMintCap` rejected every single tap as expired, prompting agents to proceed directly via browser/alternative paths and abandon all three proposals in `pending`. | [`DefaultAskTTL`](../../internal/approvals/presence.go#L25) is hardcoded to `30 * time.Minute` from `ask_question` render time (`RecordAsk`), whereas [`DefaultProposalTTL`](../../internal/approvals/approvals.go#L25) is `24 * time.Hour`. Async human review habits (replying to mobile/desktop prompts an hour later) routinely cross the 30-minute cliff while proposals remain valid for 23 more hours. | Authentic human approvals fail closed with an error, training users and agents to route around `hq approve` and leaving valid proposals stranded in `Pending Approvals`. |
| **`F4`** | **Blanket `ask_question` Overwrite & Missed Rejection Reconciliation** | Multi-question `ask_question` calls mentioning any pending `prop-*` ID lost secondary questions and custom steering options; when users selected `Reject (<id>)` in `ask_question` and moved on cleanly, the model sometimes skipped calling `hq reject`, leaving the proposal `pending`. | [`HandlePreToolUse`](../../internal/hooks/hooks.go#L1269-L1408) runs `strings.Contains(rawStr, prop.ID)` across the entire serialized `ask_question` argument payload (L1280) and overwrites `questions` (L1388) with a single synthetic question holding 3 rigid options (`Approve`, `Reject`, `Defer`), stripping all sibling questions and custom choices. Reject taps are logged only for ask verification, not auto-applied on turn end. | Broken multi-question UX forces sequential round-trips, loses domain-specific options, and drops user rejections onto the floor unless manually cleaned up. |
| **`F5`** | **Missing Thread Auto-Supersede, Spurious Reject Wakeups & Stale Git Refs** | Wave 2 generated 3 separate pending proposals from the same conversation thread for the same task; calling `hq reject --reason="already shipped"` risked triggering an LLM follow-up wakeup; and pushed git tags/branches remained pending for up to 24h. | `CreateWithOptions` ([`approvals.go`](../../internal/approvals/approvals.go)) only calls `SupersedeByThread` when `SupersedePrior && TargetThreadKey != ""`; generic `hq propose` lacks a default conversation/type dedupe key. Meanwhile [`RejectProposal`](../../internal/approvals/approvals.go#L872) treats any custom `--reason` (`hasCustomSteering`) as an instruction to enqueue an LLM resumption event; `hq propose` stdout lacks explicit `ask_question` pairing hints; and no 0-token offline reconciler checks local git refs. | Queue bloat from repeated drafts (`3x` duplicates), wasted LLM turns when cleaning stale items, and chronic distrust of `CONSOLE.md` counts. |
| **`F6`** | **GitHub Issue `#28` — 5s Hook Timeout & Non-Idempotent `askPath` Deletion** | Slow Vault disk scans or concurrent `hq` checks risk hitting the 5-second hook deadline right after `VerifyAskAndMintCap` has invalidated the ask record. | [`.agents/plugins/agy-hq/hooks.json`](../../.agents/plugins/agy-hq/hooks.json#L17) caps `PreToolUse` (`".*"`) at `"timeout": 5` seconds. Worse, [`VerifyAskAndMintCap`](../../internal/approvals/presence.go#L356-L366) deletes `askPath` (`L366`) and writes `.used-*` (`L356-L363`) *before* `ApproveFromCLI` consumes `approval_caps/<id>.json`. If `hq approve` fails mid-flight or the hook times out right after minting, retrying `hq approve` fails permanently because the `AskRecord` is already gone. | Single transient timeout or downstream command pre-check failure irreversibly bricks an approved one-shot ask, forcing manual re-staging or Web Console fallback. |

---

## 3. Native Antigravity Coexistence: Where `hq propose` Adds Real Value vs. Duplicates Prompts

In Antigravity, the runtime already enforces workspace and shell safety using native permission presets (`AGENT_PERMISSION_PRESET_TURBO`, interactive terminal prompts, and `ask_question`). Layering `hq propose` blindly on top of every external side effect creates double prompting without improving security.

Evaluating every action class from first principles yields three boundary criteria and a crisp separation of duties:

### The Three Boundary Criteria

1. **Session Autonomy (`Auto` / `Tainted` vs. Interactive User-Directed):**
   - **Unattended (`[Auto]`) & Tainted (`tainted`) sessions:** Antigravity's interactive chat prompt is absent (or context is untrusted per [RFC-0017b](../design/0017b-provenance-taint-and-offer-gateway.md)). *Every* external mutation — including own-repo git pushes, package publishes, and outbound offers — **must** gate asynchronously via `hq offer stage` / `hq propose` so the user reviews safely from `[HQ] Front Desk`, `CONSOLE.md`, or `:4242` Web Console.
   - **Interactive user-directed sessions:** The user is present in chat and often runs `AGENT_PERMISSION_PRESET_TURBO` precisely to avoid rubber-stamping routine development steps.
2. **Blast Radius & Reversibility:**
   - **High / Public / Counterparty Blast Radius (`always` gated):** Sending messages/emails on behalf of the user (`messaging`: Gmail, GChat, Slack, customer email), publishing public/org artifacts (`release_publish`: `gh release create`, `npm publish`, `vercel --prod`, production deploys), merging third-party PRs (`pr_merge` on non-owned repos), and Ring-0 TCB self-modification (`rsi_apply`). Once executed, counterparties read it or public registries/production shift irreversibly.
   - **Single-Player / Owned-Repo Reversible Actions (`auto_only` under interactive sessions):** `git push`, `git tag`, and own-repo `gh issue` / `gh pr create` / `gh pr comment` inside `owned_repos` (e.g., `alice/*`) are reversible single-player version-control actions. Under interactive sessions, requiring a separate `hq propose` round-trip duplicates Antigravity's terminal/tool model cleanly for zero real blast-radius gain; `AGENTS.md` §3 already relies on local CI pre-push gates for these.
3. **Deterministic Zero-Token Execution:**
   - `hq propose` only earns its keep over native `ask_question` when approving at `:4242` or in chat triggers **deterministic, zero-LLM-token execution by the Go binary** with scrubbed environment variables (`SanitizedApprovalEnv`), canonical SHA-256 verification (`ComputeProposalHashV2`), and `CommandArgv` pipe binding (`/dev/fd/3` in [`offer.go`](../../internal/offer/offer.go)).
   - If an action *cannot* execute cleanly inside Go (e.g., an interactive multi-step Chrome DevTools MCP flow on a social web UI), forcing it into `hq propose` produces fake `echo` commands (`F2`). Those workflows must instead use native `ask_question` + markdown blockquote preview in-turn (for interactive chats) or `hq outbox add` escalation cards (for unattended `[Auto]` runs).

### Side-by-Side Coexistence Matrix

| Action Category | Interactive Session (`trusted`, live user) | Unattended / `[Auto]` / `tainted` Session | Primary Enforcement / Execution Path |
| :--- | :--- | :--- | :--- |
| **Outbound Human Messaging** (Gmail send, GChat post, external counterparty comms) | **`hq propose`** + `ask_question` | **`hq propose`** / `hq offer stage` | `PreToolUse` hard gate + Go deterministic connector execution (`hq approve`) |
| **Public / Production Releases** (`gh release create`, `npm publish`, `vercel --prod`, `fly deploy`) | **`hq propose`** + `ask_question` | **`hq propose`** | `PreToolUse` hard gate + deterministic CLI execution (`hq approve`) |
| **Third-Party PR Merges** (`gh pr merge` outside `owned_repos`) | **`hq propose`** + `ask_question` | **`hq propose`** | `PreToolUse` (`third_party_only` target resolver) + deterministic `gh pr merge` |
| **Owned-Repo PR Merges & Git Tags/Pushes** (`alice/*` own repos) | **Exempt** (native Antigravity / CI gates) | **`hq propose`** (`auto_only` default) | Unblocked in interactive mode; blocked by `PreToolUse` in `[Auto]` / `tainted` |
| **Ring-0 Self-Modification** (`hq rsi apply`, rule/kernel mutations) | **`hq propose`** | **`hq propose`** (or blocked if tainted) | `PreToolUse` hard gate + deterministic `hq approve` |
| **Interactive Browser / GUI Social Posts** (Chrome DevTools / Web UI actions with no deterministic CLI) | **Native `ask_question`** + markdown blockquote preview (NO `hq propose`) | **`hq outbox add`** / escalation (never dummy `echo`) | Rejected if attempted via `echo`/`printf`/`true`/`:` in `hq propose` |
| **User-Added Categories** (e.g., `cloud_infra`, `payments` via `config.yaml` or extension pack) | **Configured `mode`** (`always` / `third_party_only` / `auto_only` / `never`) | **Configured `mode`** (`auto_only` still gates in `[Auto]`/`tainted`) | Uniform `approvals.rules` evaluator in `PreToolUse` (0 Go changes) |

### Why `--defer-to-session` Fails the `YAGNI` Test

During design exploration, we considered adding a non-executable `hq propose --defer-to-session` "audit ticket" mode so browser tasks could still log intent into `Pending Approvals`. We explicitly **reject (`YAGNI`)** `--defer-to-session`:
- **Dual state machines without deterministic completion:** A non-executable proposal requires a human to click Approve in one UI (`:4242` or `ask_question`), only for an LLM still to wake up later, inspect browser DOM state, click buttons manually, and remember to close the ticket. Every gap between click and completion re-creates `F1`–`F2` stale entries.
- **Better primitives already exist:** Interactive chats already have synchronous `ask_question` + blockquote previews right before clicking **Post**; background `[Auto]` runs already have `hq outbox add --tier=interrupt` to escalate drafts to `[HQ] Front Desk` with 0 stale proposal state. Keeping `hq propose` 100% deterministic preserves its security invariant: *every row in `Pending Approvals` has an exact SHA-256-bound binary/connector payload that Go executes immediately on approval in 0 LLM tokens.*

---

## 4. Architectural Rationale for Uniform Data-Driven Policy (`approvals.rules`)

Previously, adding or tuning an approval rule required coordinated edits across Go regexes in [`hooks.go`](../../internal/hooks/hooks.go#L619-L651), classification helpers (`classifyUnattendedAutoCommand`), CLI helpers in [`cmd/hq/main.go`](../../cmd/hq/main.go), and prose instructions in [`rules/AGENTS.md`](../../.agents/plugins/agy-hq/rules/AGENTS.md). Because Go was hardcoded and `AGENTS.md` drifted wider than `PreToolUse`, `F1` was inevitable.

Moving to a single **uniform schema** (`approvals.rules` in `<vault>/config.yaml` + optional `approval_rules` in [`hq-extension.json`](../../internal/extension/pack.go)) eliminates harness coupling:

1. **Zero Harness Rebuilds for User Customizations & New Categories:** Both built-in defaults (`messaging`, `release_publish`, `pr_merge`, `forge_git`, `rsi_apply`, plus concrete browser/no-op guard guidance) and arbitrary user-defined categories (`cloud_infra`, `payments`, `db_migrations`, etc.) are represented as identical structs in one ordered slice:
   - Each rule binds `{id, description, mode, commands, target_resolver, auto_categories}` directly in YAML.
   - Users can switch a built-in rule's `mode` (`always`, `third_party_only`, `auto_only`, `never`), tune `owned_repos: ["alice/*", "org/*"]`, or drop in a brand-new `cloud_infra` rule (`commands: ["^(terraform|pulumi)\\s+apply\\b"]`, `mode: "always"`) by editing `<vault>/config.yaml` or installing a companion pack—**with zero Go code changes and zero `hq upgrade` rebuilds**.
2. **Single Source of Truth Across Enforcement & Prompting:** Instead of duplicating brittle tables in `AGENTS.md`, `HandleSessionStart` compiles a concise `Active Outbound Policy` summary directly from the resolved rule set and injects it into session context alongside the rule-bound `PreToolUse` evaluator. What `PreToolUse` enforces and what the model is instructed to propose are guaranteed identical on every turn.
3. **Preserving Zero-External-Dependency TCB Purity:** All `commands` regexes compile cleanly with Go stdlib `regexp` on rule load (`0` new `go.mod` dependencies), and config integrity is protected against untrusted prompt-injection edits by keeping `<vault>/config.yaml` write-guarded in `PreToolUse` (especially rejecting self-weakening mutations to `approvals:` from `tainted` or `[Auto]` sessions).

---

## 5. Summary of Recommendations (Implemented in RFC-0021)

1. **Data-Driven `approvals` Config & Extension Schema:** Introduce unified `approvals.rules`, `approvals.owned_repos`, `approvals.ask_ttl` (`24h` default), and `hq-extension.json` `approval_rules` merging, backed by `SessionStart` dynamic policy injection, `hq doctor` validation, and `approvals:` self-tamper protection in `PreToolUse`.
2. **Strict Deterministic Command Boundary (`Reject No-Ops`):** Reject dummy no-op commands (`echo`, `printf`, `true`, `:`, quote-wrapped prose) at `hq propose` creation and in `PreToolUse`, keeping interactive browser/GUI actions strictly on native `ask_question` + blockquote preview and background drafts on `hq outbox add`.
3. **Human-Presence & Hook Reliability Hardening (Issue `#28` + `F3`/`F4`):** Align `DefaultAskTTL` to `24h` (`ask_ttl`), make `VerifyAskAndMintCap` idempotent/crash-safe (defer `.used-*` mark and `askPath` removal until `ConsumeCap`), bump `PreToolUse` timeout (`5s -> 15s`), and make `ask_question` matching/injection surgical per matching question index without clobbering sibling questions or custom options.
4. **Zero-Token Lifecycle & Stale Queue Reconciliation (`F4`/`F5`):** Auto-supersede prior pending proposals per `(ConversationID, ActionType)` (with `--keep-prior` opt-out), add `hq reject --dismiss` / `--no-resume` to suppress unnecessary LLM wakeups, print crisp `ask_question` usage guidance on `hq propose`, auto-reconcile `Reject (<id>)` transcript selections, and add `<2ms` offline local git ref/tag reconciliation in `Stop` / `SyncMarkdownConsole`.
