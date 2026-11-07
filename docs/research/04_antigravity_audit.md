# 04 — Google Antigravity as a foundation for a proactive personal agent (audit, 2026-11-07)

**Scope:** what Antigravity (App 2.0, CLI `agy`, IDE, Python SDK) can and cannot do for a "Claude/OpenClaw-like" proactive personal assistant.
**Method:** I read the local built-in skills and docs, read the SDK plugin docs, ran read-only commands (`agy --help`, `agy help <sub>`, `agy changelog`, `agentapi get-conversation-metadata <own id>`), and fetched the live pages under https://antigravity.google/docs/*. No config was changed and no sidecar or automation was enabled.

**Environment observed:** Antigravity App language server **2.29.0** (`~/.gemini/antigravity/daemon/hub_*.json`), CLI **1.5.3** (`agy changelog`), SDK plugin **0.0.9** (`~/.gemini/config/plugins/google-antigravity-sdk/plugin.json`).

---

## TL;DR

- **Proactivity: yes, but only on your own machine.** Persistent cron "Automations" (sidecars with `"builtin": "schedule"`) start a fresh agent conversation on each run. Generic daemon sidecars can run any long-lived script (a watcher, a webhook server, a bot) and call `agentapi`. Both depend on the Antigravity App running on an awake, online machine. There are **no native event triggers** (file, webhook, email). You build them as sidecars, or use SDK triggers.
- **Memory: nothing automatic.** You get per-conversation transcripts, `/learn` (which proposes rules and skills and writes them only after you approve), and always-on `GEMINI.md`/`AGENTS.md` rules. "Knowledge items" are mentioned in the docs but are effectively absent here: `~/.gemini/antigravity/knowledge/` holds only `knowledge.lock`. Scheduled runs keep no memory between runs. Memory has to be file-based and wired in with rules, hooks, and skills.
- **Channels: no native mobile app and no inbound messaging.** The closest thing to mobile is **Remote Control**, a browser/PWA dashboard that can drive local sessions and sends **push notifications**. **Remote Agents** run in Google's cloud while the laptop sleeps, but they can't touch local files and need a Pro or Ultra plan. A Telegram bridge is possible as a sidecar (`agentapi new-conversation` / `send-message`), but `agentapi` **does not return the agent's reply**. Replies have to come back through files or transcripts, or by having the agent post to Telegram itself.
- **Execution is strong:** terminal with an OS sandbox (Seatbelt on macOS), real-Chrome browser automation via the Chrome DevTools MCP, MCP, subagents, and dynamic workflows. In unattended runs, **anything that isn't pre-approved is auto-denied** (automations), or **soft-denied** with exit 0 (`agy -p`).
- **SDK = same harness, headless, scriptable.** Triggers (`every`, `on_file_change`, custom), persistence (`conversation_id` + `save_dir`), custom Python tools, declarative policies with `ask_user` handlers, 9+ hook points, local models (LiteRT Gemma 4 26B, Ollama). It **bills a Gemini API key or Vertex**, not your Antigravity subscription quota.
- **Policy blocker to keep in mind:** the FAQ says third-party wrappers (it names OpenClaw) on an Antigravity login violate the ToS, and wrapping `agy -p` inside third-party apps "is not supported under consumer plans". For a custom bot, the safest route is the **SDK with an API key**, or sidecars plus `agentapi` inside the App.

---

## 1. Proactivity

### 1.1 Persistent cron: Automations (scheduled sidecars)

| Aspect | Finding | Evidence |
|---|---|---|
| Mechanism | A sidecar with `"builtin": "schedule"`. `args[0]` is the cron expression, followed by `agentapi new-conversation -- "<prompt>"`. Each run starts a **fresh conversation** with no memory of earlier runs. | `builtin/skills/automation/SKILL.md` L10-12, L112-114, L158-189; https://antigravity.google/docs/sidecars |
| Location | `~/.gemini/config/sidecars/<id>/sidecar.json` or `~/.gemini/config/plugins/<plugin>/sidecars/<id>/sidecar.json`. (The automation skill also mentions `~/.gemini/jetski/sidecars/`. That folder doesn't exist here, so this looks like stale text.) | docs/sidecars; `ls ~/.gemini/config/sidecars` → empty |
| Enablement | Off until enabled in `config.json → sidecars.<id>.enabled`. The UI path is the Automations dashboard (`sidecar://dashboard`). `new-conversation` needs a `projectId`. | docs/sidecars, "User configuration" |
| Granularity | Standard 5-field cron, so **1 minute** at the finest. The App adds jitter: "spreads them out by up to 10% of the interval, and by no more than 15 minutes". | https://antigravity.google/docs/features ("Automations") |
| Timezone | Prefix the expression with `CRON_TZ=<IANA zone>`, e.g. `CRON_TZ=America/Los_Angeles 0 9 * * *`. | automation SKILL.md L91-111 |
| Model tier | Per automation: Flash Lite, Flash (default), or Pro. | docs/features |
| Permissions | Runs "in a project that inherits global permissions and presets, and **all other non-approved commands will be auto-denied**". Per-sidecar `agent_permissions.access_grants` (e.g. `command(gh pr list)`, `read_file(/abs)`, `mcp(server/tool)`, `read_url(domain)`) plus `workspace_uris`. | automation SKILL.md L203-251 |
| Runtime data | `~/.gemini/antigravity/sidecar_data/<id>/{data,logs,events}`. `data/` is exposed as `$ANTIGRAVITY_EXECUTABLE_DATA_DIR`, and `events/` records each `agentapi` call. | docs/sidecars, "Runtime data" |
| App must run? | **Yes.** Sidecars are "background processes that run alongside Antigravity. Antigravity manages the lifecycle". The docs mark them "Available on: Antigravity App". Settings include "Run in background" (keeps the app alive after its window closes) and "Keep computer awake". | docs/sidecars; `antigravity_guide/references/app.md` L63-66 |
| Machine awake? | **Yes.** Local execution needs the machine powered on and online. The FAQ says "If an agent is running, Antigravity prevents your computer from sleeping", but nothing says sleep is prevented *between* runs. **What happens to runs missed during sleep is undocumented** (treat them as skipped). The fallback is `caffeinate`/pmset, or Remote Agents (cloud). | https://antigravity.google/docs/faq |

> [!NOTE]
> Your current global preset is **Turbo** (`config.json → userSettings.permissionPreset = AGENT_PERMISSION_PRESET_TURBO`, terminal sandbox off, 75 global allow grants). Automations inherit global presets, so today they would effectively run **unrestricted**. That is convenient, but risky for a proactive agent that reads untrusted email or web content.

### 1.2 Event triggers (file, webhook, email)

- **App and CLI: there are none built in.** The only sidecar builtin is `schedule` ("Currently supports schedule"; docs/sidecars).
- **Workaround (sanctioned):** a `"command"` sidecar (`restart_policy: "always"`) running your own script: an fsevents watcher, an HTTP webhook listener, an IMAP or Gmail poller, a Telegram long-poller. The script calls `agentapi new-conversation` or `agentapi send-message` when its event fires. Antigravity puts `agentapi` on the sidecar's PATH automatically.
- **SDK:** native `triggers=[every(60, fn), on_file_change(path, fn), custom_async_fn]`, and `ctx.send()` pushes a message into the running agent (`.../examples/getting_started/periodic_trigger.md`).

### 1.3 Daemon sidecars and UI sidecars

- **Daemon sidecar:** `"command": "python3", "args": ["worker.py"], "restart_policy": "always|on-failure|never"`. Its working directory is the sidecar folder, and `env` is supported (docs/sidecars). The changelog notes that the CLI also runs plugin sidecars during sessions (1.4.8 fixed a "memory leak in long sessions with plugins that run sidecars").
- **UI sidecar (UI plugin pane):** a sidecar with `has_web_ui: true` and `ui_config.views[]`, where the view's `entrypoint` is `SIDECAR_UI_ENTRYPOINT_AUX_PANE`. It renders in the App's side pane. The agent can surface a link pill to it: `[Label](sidecar://<id>/<path>)` (`builtin/skills/ui-plugin-navigation/SKILL.md`). This works well as a personal-agent "dashboard" (inbox, reminders, memory viewer) inside the App.
- Plugins can template sidecars with `${HOME}` (CLI 1.5.0 changelog), and plugin hooks and commands get `PLUGIN_ROOT` and `PLUGIN_DATA` (CLI 1.5.3).

### 1.4 In-session `schedule` tool (`/schedule`)

- One-shot timers (`DurationSeconds`, plus an optional `TimerCondition` to cancel early when a subagent or task reports back) and recurring schedules (`CronExpression`, `MaxIterations`, `IsDaemon`, `ExactTiming`).
- It wakes the **same** conversation with its full history and is **scoped to that conversation**. "For standing jobs that outlive a session, use a persistent sidecar or Automation" (https://antigravity.google/docs/commands/schedule).
- Recurring schedules get a per-schedule jitter unless exact timing is requested (CLI 1.4.5 changelog).
- This gives you "proactive within a live session" (for example, poll every 5 minutes while I'm away), but nothing survives an app restart.

---

## 2. Memory

| Mechanism | What it is | Long-term? |
|---|---|---|
| Transcripts | `~/.gemini/antigravity/brain/<conv-id>/.system_generated/logs/transcript.jsonl` and `transcript_full.jsonl`, plus `conversations/*.db|pb` and `conversation_summaries.db`. In the App you can attach a previous conversation with `@` (app.md L28-30). CLI: `--continue` / `--conversation <id>`. | Raw history only. No automatic recall. |
| `/learn` | Reflects on the session and proposes a **rule** or **skill** in `learning_proposal.md`, then writes only after you approve. Targets: `~/.gemini/GEMINI.md`, `~/.gemini/config/AGENTS.md`, `~/.gemini/config/rules/<n>.md`, `~/.gemini/config/skills/<n>/SKILL.md`, or workspace equivalents. The docs state "Agents don't remember conversations." | Yes, but manual and approval-gated. https://antigravity.google/docs/commands/learn |
| Rules | `GEMINI.md`/`AGENTS.md` are always on. Limits: 24 KB per file, and a 20k-token aggregate budget, beyond which rules are demoted to file pointers. | Yes, always injected. `agy-customizations/docs/rules.md` |
| Knowledge items | The docs mention them in passing ("app data directory … contains artifacts, knowledge items" — docs/agent-settings, docs/settings). `/docs/knowledge` returns 404, and locally `~/.gemini/antigravity/knowledge/` holds only `knowledge.lock`. | **Not usable or not documented.** Treat as absent. |
| Automations | "Each scheduled run starts a brand-new conversation with no memory of prior runs." | No |
| SDK | `conversation_id` + `save_dir` resume a full trajectory. `app_data_dir` relocates artifacts. Compaction can be configured. | Per-thread persistence, not semantic memory. |

**How file-based memory could work (cheap and native):**
1. Keep a memory folder, e.g. `~/agent-memory/{profile.md, facts.jsonl, journal/YYYY-MM-DD.md}`.
2. Put a short always-on **global rule** in `~/.gemini/config/AGENTS.md` (or `~/.gemini/GEMINI.md`, which exists and is empty). It holds the profile summary and says: "read `~/agent-memory/profile.md`; append new durable facts to `facts.jsonl`". Stay under 24 KB.
3. Add a **`SessionStart` / `PreInvocation` hook** that greps the memory files for terms in `lastUserInput` and injects matches as `ephemeralMessage` or `systemMessage` steps (`agy-customizations/docs/hooks.md` L254-331).
4. Add a **`Stop` hook** (or have the automation prompt end with an instruction) that appends a summary of the run to `journal/`. Hooks receive `transcriptPath`, so a script can summarize the transcript after the fact.
5. Optionally add an MCP memory server (stdio, e.g. SQLite FTS) registered in `mcp_config.json` or inside a plugin, using `PLUGIN_DATA` for storage.
6. Grant automations `read_file`/`write_file` on `~/agent-memory` in `agent_permissions`.

---

## 3. Interaction channels

| Channel | Status | Evidence |
|---|---|---|
| Desktop App 2.0 (Electron), IDE (VS Code fork), CLI `agy` | Yes | references/app.md, ide.md, cli.md |
| **Remote Control** (web/PWA on phone or tablet) | **Yes.** Drives local App and CLI sessions from any browser at antigravity.google; "install the web app to your home screen to receive **push notifications** when agents complete tasks or request input". The CLI daemon is `agy remote-control start [--name] [--session]`. On macOS it starts at login, not boot, and stops at logout. It's already on here (`remoteControlEnabled: true`). It doesn't work with ADC sign-in. | https://antigravity.google/docs/remote-control; `agy help remote-control`; CLI changelog 1.3.x-1.5.x |
| **Remote Agents** (Google-managed cloud) | Yes, for **Google AI Pro/Ultra personal accounts only**, rolling out gradually. Not on Free, Plus, or Workspace/Cloud accounts. They keep running while the laptop sleeps and use GitHub, Drive, and uploaded files, but **can't access local files**. `agy --remote-agent`. | https://antigravity.google/docs/remote-agents |
| Native mobile app | **No** (only the PWA above) | — |
| Desktop notifications | Yes. App setting "Notifications" (task completion), and CLI `notifications` (bell plus desktop notification on approval, question, or turn end). | app.md L63; https://antigravity.google/docs/settings |
| Voice | App voice mode (Gemini Live), CLI dictation, and `agy mic-serve` | docs/voice-mode; `agy help mic-serve` |
| **Inbound messaging (Telegram, Slack, email, SMS)** | **No native support.** The marketplace has no Telegram, Slack, or Gmail plugin; Google Calendar exists (`antigravity-plugins-official/plugin/gcalendar`). | `search_marketplace` |

### Can an external process start or continue a conversation, and get the reply?

1. **`agentapi`** (App). `~/.gemini/antigravity/bin/agentapi` is a two-line shell wrapper: `exec /Applications/Antigravity.app/Contents/Resources/bin/language_server agentapi "$@"`.
   - Subcommands: `new-conversation [--model=flash_lite|flash|pro|id] [--title] [--profile] <prompt>`, `send-message [--title] <recipient_id> <content>`, `get-conversation-metadata <id>`.
   - It talks to the **running** App language server through env vars (`ANTIGRAVITY_LS_ADDRESS`, `ANTIGRAVITY_CSRF_TOKEN`, `ANTIGRAVITY_PROJECT_ID`, …), which Antigravity injects into sidecars and agent shells. A random launchd process doesn't have them, so the **sanctioned path is to run the bot as a sidecar**.
   - **Replies:** there is **no "read messages / wait for reply" command**. `get-conversation-metadata` returns workspace and config, not message content (verified on this conversation). Ways to get the answer back out:
     - (a) The prompt tells the agent to write its answer to a file the bot watches, e.g. `$ANTIGRAVITY_EXECUTABLE_DATA_DIR/outbox/<id>.md`.
     - (b) The agent posts to Telegram itself with `curl` or a small `tg-send` script, allowed through a `command(...)` grant and a `read_url(api.telegram.org)` grant.
     - (c) The bot tails `brain/<id>/.system_generated/logs/transcript.jsonl`. This file format isn't a documented API and may change.
   - Whether `new-conversation` prints the new conversation id isn't documented, and I didn't run it (it starts a conversation). Check `sidecar_data/<id>/events/`, which records agentapi calls, once you test.
2. **`agy -p` headless (CLI).** Prints the answer to stdout (`--output-format json|stream-json`), continues threads with `--conversation <id>` or `--continue`, and `--input-format stream-json` keeps one process open for many turns (https://antigravity.google/docs/cli/headless). This is the **cleanest request/response path**. Caveats: CLI conversations live in `~/.gemini/antigravity-cli/`, separate from the App. Approvals are soft-denied unless pre-granted or `--dangerously-skip-permissions` is set. And see the ToS note in §7.
3. **`agy acp`.** An Agent Client Protocol JSON-RPC stdio server (sessions, resume, modes, permission requests forwarded to the client). A bridge could be an ACP client that relays permission prompts to Telegram.
4. **SDK.** A Python process owns the agent fully: `await agent.chat()` streams the reply, and `conversation_id` resumes the thread. This is the most robust bot backend (§6).

---

## 4. Execution

- **Terminal:** `run_command`, persistent terminals, and background tasks.
- **Sandbox:** on by default under the Default and Auto presets. macOS uses `sandbox-exec` with Seatbelt (SBPL) profiles; Linux uses namespaces; Windows uses AppContainer (Win 11 24H2+). There's no network by default, and `read_url` grants become the outbound allowlist (https://antigravity.google/docs/sandbox).
- **Presets** (https://antigravity.google/docs/permissions):

  | Preset | Sandbox | Terminal | Files | MCP/Web |
  |---|---|---|---|---|
  | Request Review | Off | Always ask | Workspace only | Ask |
  | **Default** | On | Allowed in sandbox, ask outside | Workspace + temp | Ask |
  | **Auto** | On | Sandbox; outside it, an AI reviewer checks the command | Workspace + temp | Ask |
  | **Turbo** (yours) | Off | Unrestricted | Full filesystem | Allowed |

  Grant syntax: `command(prefix|regex:…)`, `read_file(path)`, `write_file(path)`, `read_url(domain)`, `execute_url(domain)` (browser), `mcp(server/tool)`, `unsandboxed(...)` (business accounts only). Grants can be global, per project, or per sidecar.
- **Unattended approval handling:**
  - Automations: non-approved actions are **auto-denied** (automation SKILL.md L203-205).
  - `agy -p`: an action needing approval is **soft-denied**. The run continues, exits 0, and prints a stderr notice. With `--dangerously-skip-permissions` everything is approved, except denies your Gemini Enterprise admin enforces. Subagent questions are skipped in headless mode.
  - SDK: `run_command` is denied by default, and `ask_user(handler=…)` lets *your code* decide (for example, ask on Telegram).
  - Hooks: a `PreToolUse` hook can return `allow`, `deny`, `ask`, or `force_ask`, add `permissionOverrides`, or rewrite arguments. That makes it a programmable approval gate. Hooks are synchronous with a 30 s default timeout (configurable).
- **Browser:**
  - (1) The App's **built-in browser** in the side panel is for the user. It has its own cookies, separate from Chrome.
  - (2) **`/browser` subagent** drives *your real Chrome* through the **Chrome DevTools MCP**: screenshots, console, network, form filling, and WebM recordings. Chrome must already be running with remote debugging. It's only used when invoked, and access is controlled by `execute_url` grants and JS-execution policy.
  - (3) You also have `chrome-devtools` configured directly in `~/.gemini/config/mcp_config.json` (alongside `github` and `context7`). A browser profile exists at `~/.gemini/antigravity-browser-profile/`.
  - Sources: https://antigravity.google/docs/commands/browser, https://antigravity.google/docs/ide/browser
- **Other built-ins:** `search_web`, `read_url_content`, image generation, `generate_image`, and dynamic **`run_workflow`** Python orchestration scripts (`builtin/skills/run-workflow/SKILL.md`). `/goal` runs long tasks to completion; `/teamwork` and `/boost` need a paid plan.

---

## 5. Extensibility

| Feature | Details | Evidence |
|---|---|---|
| Skills | `skills/<name>/SKILL.md` (frontmatter `name` and `description`), loaded progressively. They become `/<name>` slash commands. Locations: `.agents/skills/` or `~/.gemini/config/skills/`. Workflows are deprecated. | `agy-customizations/docs/skills.md`; https://antigravity.google/docs/skills |
| Rules | `GEMINI.md`/`AGENTS.md`, hierarchical. `.agents/rules/*.md` with triggers (`always_on`, `model_decision`). | docs/rules.md |
| Plugins | A bundle of skills, `agents/*.md` (custom subagents), `rules/AGENTS.md`, `mcp_config.json`, `hooks.json`, `sidecars/`, `bin/` (CLIs the agent can call by name), `auth.json` (API keys and OAuth), and `PLUGIN_DATA`. Includes a marketplace, `agy plugin install/validate`, and import from Claude Code and Gemini CLI. | `builtin/skills/plugin/SKILL.md`; docs/plugins.md; https://antigravity.google/docs/build-plugins; CLI changelog |
| Hooks (App/CLI) | **6 events:** `SessionStart`, `PreToolUse`, `PostToolUse`, `PreInvocation`, `PostInvocation`, `Stop`. Command-type only, JSON over stdin and stdout. They can inject steps (`systemMessage`, `userMessage`, `ephemeralMessage`, `errorMessage`), terminate a turn, force it to continue, block a stop, or allow, deny, or rewrite tool calls. There are **no** HTTP or prompt hooks and **no** async hooks. | `agy-customizations/docs/hooks.md` L65-81, L404-407; https://antigravity.google/docs/hooks |
| MCP | stdio and Streamable HTTP (legacy SSE goes through `mcp-remote`). Global or per plugin. `agy mcp add/list/...`. OAuth sign-in for partner plugins. | docs/mcp_servers.md; https://antigravity.google/docs/mcp |
| Subagents | Background and async, nesting depth ≤10, workspace modes `inherit`/`branch`/`share`, custom agents in `agents/*.md` with a model tier and tool allowlist. | https://antigravity.google/docs/subagents |
| UI plugins / sidecar panes | Web UI in the aux pane via `sidecar://` pills (§1.3). | ui-plugin-navigation SKILL.md |
| Generative UI | Self-contained HTML artifacts, inline `<agent-embed>` (500 px viewport) or side pane. CSP blocks every external CDN except a gstatic Tailwind script, so widgets **can't fetch live external data**. Good for briefings and digests, not live dashboards (use a UI sidecar for that). | `builtin/skills/generative_ui/SKILL.md` |

---

## 6. SDK (`pip install google-antigravity`, preview)

- **Same harness?** **Yes.** "The SDK gives you the same agent runtime that powers Antigravity 2.0 and the Antigravity CLI" (https://antigravity.google/blog/introducing-google-antigravity-sdk). It ships a compiled runtime binary in the wheel (`references/sdk.md`). It does **not** auto-load your `~/.gemini/config` customizations; skills are passed with `skills_paths=[...]`.
- **Triggers:** `every(seconds, fn)`, `on_file_change(path, fn)` (needs `watchfiles`), and arbitrary async pollers that call `ctx.send(...)` (`examples/getting_started/periodic_trigger.md`).
- **Persistence:** `LocalAgentConfig(conversation_id=..., save_dir=...)` resumes a conversation (`persistence.md`). `app_data_dir` relocates artifacts.
- **Custom tools:** plain Python functions, which can also override built-ins by name (`custom_tool.md`), plus MCP (stdio, SSE, HTTP) and skills.
- **Policies:** `allow`, `deny`, `ask_user(handler)`, `allow_all`, `deny_all`, `workspace_only`, and argument predicates that fail closed. The default is `confirm_run_command()`, which denies shell. `RunCommandConfig(enable_sandbox=True)` adds OS sandboxing, but **silently no-ops** where the sandbox is unavailable (`references/safety_policies.md`).
- **Hooks:** `on_session_start/end`, `pre_turn`, `post_turn`, `pre_tool_call_decide`, `post_tool_call`, `on_tool_error`, `stop`, `on_interaction` (answer `ask_question` programmatically, e.g. from Telegram), and `on_compaction` (`examples/getting_started/hooks.md`).
- **Behavior:** `AgentBehavior.AUTONOMOUS` is the default (no clarifying questions); use `INTERACTIVE` for human-in-the-loop. `BudgetConfig` caps model calls, tool calls, and tokens. `CompactionConfig`. Structured output via Pydantic. Multimodal input. Subagents with `max_subagent_depth`.
- **Models and auth:** the default is `gemini-3.8-flash`. Auth is `GEMINI_API_KEY` (AI Studio) or Vertex (`vertex=True`, ADC or Express key), billed **per token**. Priority tier is available. **Local:** `LiteRTAgentConfig` (Gemma 4 26B on-device, 24 GB+ unified memory recommended, 64k KV cache) and `LocalOpenAIAgentConfig` (Ollama, LM Studio).
- **Headless daemon?** **Yes.** It's an ordinary asyncio Python program: run it under launchd or systemd, keep `async with Agent(...)` open, and let triggers or a Telegram poller push messages in. It doesn't depend on the App. The blog says a remotely hosted loop is coming ("soon … same agent deploys to the cloud").
- There's also a **managed cloud "Antigravity agent" in the Gemini API** (Interactions API, Google-hosted sandbox), which docs/sdk/overview links to. I didn't verify it in depth; the web search summaries about it were unreliable.

---

## 7. Limits, gaps, and blockers

1. **Terms of service.** FAQ: "Why can't I use third-party software (such as Claude Code, **OpenClaw**, or OpenCode) with my Antigravity login? … violation of our Terms of Service … can result in suspension". Also: "agy -p is intended for direct scripting and CI workflows. Wrapping agy inside third-party coding agents or applications … is **not supported under consumer plans**" (https://antigravity.google/docs/faq). → Don't build an OpenClaw-style bot on the consumer login. Use App-native sidecars plus `agentapi`, or the SDK with a Gemini API key or Vertex.
2. **Must stay powered on.** Automations and sidecars need the App running and the machine awake and online. Sleep behavior between runs is undocumented. Remote Agents avoid this but lose local access.
3. **No inbound messaging and no native mobile app.** Remote Control's PWA and push notifications are the native mobile channel. Telegram and similar have to be built.
4. **No automatic long-term memory.** Automations are stateless. Knowledge items are effectively absent.
5. **No native event triggers** (webhook, email, file) in the App. You write sidecar scripts or use the SDK.
6. **`agentapi` can't read replies.** You need an outbox file, agent-initiated posting, or transcript tailing.
7. **Unattended approvals:** everything not pre-granted is auto-denied (automations) or soft-denied (`-p`). Under Turbo everything is allowed, which is dangerous with untrusted input (prompt injection from email or web pages).
8. **Quotas and cost** (https://antigravity.google/docs/plans): Free gets a weekly quota. Pro gets a "high" quota refreshed every 5 hours until a weekly cap. Ultra gets the highest quota plus third-party models. AI-credit overage (Never/Always) is available on Pro and Ultra. **No BYOK** in the App or CLI. Quota scales with the amount of agent work, so frequent automations eat into it. The SDK uses separate pay-per-token billing.
9. **Models** (https://antigravity.google/docs/models): Gemini 4 Argon, Gemini 3.8/3.7/3.6 Flash, Gemini 3.1 Pro, Claude Sonnet/Opus 5.5 and 4.6 (Thinking), GPT-OSS 120B. Availability depends on the plan. Automations offer Flash Lite, Flash, or Pro tiers.
10. **Hook constraints:** synchronous, command-only, default 30 s timeout. That's awkward for "wait for my approval on Telegram" (raise `timeout`, or use the SDK's `ask_user` instead).
11. **Generative UI can't fetch live data** (CSP). Use a UI sidecar for live panes.
12. **Remote Control and Remote Agents restrictions:** Remote Control doesn't work with ADC sign-in. Remote Agents are personal Pro/Ultra only. Auto mode isn't available for business accounts.

---

## 8. GAP TABLE

| Personal-agent capability | Antigravity native | Cheapest way to fill the gap |
|---|---|---|
| Scheduled proactive runs (daily brief, nightly triage) | **Yes.** Automations: cron + `CRON_TZ`, ±10% jitter capped at 15 min, choice of model tier. | Use them as-is. Pre-grant exact `access_grants`. Have each prompt read and write the memory folder. |
| Runs while laptop asleep or off | **Partial.** Remote Agents (cloud, Pro/Ultra, no local files); the App prevents sleep only while an agent is running. | App "Keep computer awake" + "Run in background", `caffeinate`/`pmset`, or an always-on Mac mini or Linux box (CLI `agy remote-control start` + App). Use Remote Agents for repo- and Drive-only tasks. |
| Event triggers (file, webhook, email) | **No** (App); **Yes** (SDK `on_file_change` / custom). | A `"command"` sidecar running a small watcher or poller (fswatch, IMAP/Gmail API, Flask webhook) that calls `agentapi new-conversation`. |
| In-session follow-ups and reminders | **Yes.** `schedule` tool (timers, cron, `IsDaemon`). | Use it for "check back in N minutes" while a conversation is open; promote standing jobs to Automations. |
| Long-term memory across conversations | **Partial.** Rules, `/learn` (approval-gated), transcripts; no automatic memory. | `~/agent-memory/*.md` + a global `AGENTS.md` pointer + a `SessionStart`/`PreInvocation` hook that injects relevant snippets + a `Stop` hook that appends a journal entry. Optional SQLite-FTS MCP. |
| Mobile access | **Partial.** Remote Control PWA (drive sessions, approve, push notifications); Remote Agents on the web. | Install the Remote Control PWA on your phone. No custom code needed for basic mobile use. |
| Push notifications | **Yes.** Desktop notifications + Remote Control web push. | Turn on Notifications. For Telegram-style pushes, have the agent run a whitelisted `tg-send` script. |
| Inbound chat (Telegram, Slack, iMessage) | **No** | Option A (App-native): a Telegram long-poll bot as a `restart_policy: always` sidecar → `agentapi send-message` / `new-conversation`; replies via an outbox file or the agent calling `tg-send`. Option B (more robust, ToS-safe): an SDK daemon with a Gemini API key, where the Telegram handler calls `agent.chat()`. |
| External process receives the agent's reply | **Partial.** `agy -p --output-format json` returns it; `agentapi` doesn't. | SDK `await response.text()`, or a convention that the agent writes `outbox/<id>.md`. Avoid relying on `transcript.jsonl`. |
| Remote approval of risky actions | **Partial.** Remote Control approves from your phone; SDK `ask_user(handler)`; `PreToolUse` hooks. | Remote Control for App sessions. In the SDK, an `ask_user` handler that sends Telegram inline buttons and awaits the reply. |
| Unattended safety | **Yes** (granular grants, sandbox, Auto preset, per-sidecar grants). Currently set to Turbo. | Switch the global preset to Default or Auto, keep per-automation narrow grants, and add a `PreToolUse` deny-list hook for outbound sends and deletes. |
| Terminal and code execution | **Yes** (sandboxed) | — |
| Web browsing and automation | **Yes.** `search_web`, `read_url`, `/browser` subagent driving real Chrome through the DevTools MCP. | Keep Chrome running with remote debugging. Grant `execute_url` per domain. |
| Email | **No** native plugin (no Gmail plugin found) | A Gmail MCP server (stdio) or a small `bin/` CLI inside a personal plugin, using `auth.json` for the OAuth key. |
| Calendar, Docs, Drive | **Yes** via marketplace plugins (Google Calendar, Docs, Sheets, Slides, Drive) | Install from the marketplace. |
| Custom tools and integrations | **Yes.** MCP, plugins with `bin/`, skills, SDK Python tools. | Package everything into one personal plugin (`~/.gemini/config/plugins/personal-agent/`) with skills, hooks, sidecars, and MCP. |
| Personal dashboard UI | **Partial.** UI sidecar panes (live) and generative-UI artifacts (static, no external fetch). | A UI sidecar serving a small local web page (reminders, memory, run log) plus `sidecar://` pills. |
| Multi-agent delegation | **Yes.** Subagents (depth ≤10), `run_workflow`, `/teamwork` (paid). | — |
| Local or private models | **Partial.** Only through the SDK (LiteRT Gemma 4 26B, Ollama); the App and CLI are cloud-only. | SDK + `LocalOpenAIAgentConfig` for private or offline tasks. |
| Cost control | **Partial.** Plan quotas (5-hour and weekly) and AI-credit overage toggle in the App; `BudgetConfig` + token usage in the SDK. | Use Flash or Flash Lite tiers for automations and batch checks; keep Pro for on-demand work. Monitor quota in Settings. |
| Use as an OpenClaw-style backend on a consumer login | **No** (forbidden by the ToS FAQ) | SDK with an AI Studio or Vertex API key, or stay App-native (sidecars + `agentapi`). |

---

### Key sources

- Local: `~/.gemini/antigravity/builtin/skills/{automation,agy-customizations/docs/*,antigravity_guide/references/*,plugin,ui-plugin-navigation,generative_ui,run-workflow}/`, `~/.gemini/config/plugins/google-antigravity-sdk/skills/google-antigravity-sdk/{references,examples}/`, `~/.gemini/config/{config.json (keys only),mcp_config.json,sidecars/ (empty)}`, `~/.gemini/antigravity/bin/agentapi`, `~/.gemini/antigravity/knowledge/` (lock only).
- CLI: `agy --help`, `agy help {remote-control,acp,plugin,mcp,mic-serve}`, `agy changelog` (1.3.2 to 1.5.3), `agentapi --help`.
- Live: https://antigravity.google/docs/{sidecars,commands/schedule,commands/learn,commands/browser,hooks,plugins,build-plugins,mcp,permissions,sandbox,ide/browser,cli/headless,cli/reference,remote-control,remote-agents,features,faq,plans,models,settings,agent-settings,subagents,sdk/overview}, https://antigravity.google/blog/introducing-google-antigravity-sdk, plus the docs changelog. (`/docs/rules-workflows`, `/docs/cli/features` and `/changelog` returned stub pages; I used `/docs/rules`, `/docs/features` and `/docs/changelog` instead.)
