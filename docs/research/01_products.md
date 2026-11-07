# Frontier Personal-Assistant Agents: Product Landscape (as of 2026-11-07)

> [!NOTE]
> **Method and caveats.** Everything here comes from about 35 web searches run on 2026-11-07. The search tool returns summaries backed by grounding redirect links, so I cite each claim by the source domain or canonical page it pointed to (for example openai.com, meta.com, claude.com). Specific numbers (prices, star counts, model names such as "GPT-6 Astra" or "Opus 5.5") come from secondary write-ups. Check them on the vendor page before relying on them. Confidence tags: **[H]** = several independent sources agree, **[M]** = one or two sources, **[L]** = a single secondary source or inferred.

---

## 0. TL;DR

1. **Fall 2026 is the "always-on agent" season.** All three frontier labs plus Meta and Google now ship a persistent agent. Each runs on its own **cloud VM with a browser**, can **message you first**, keeps **long-term memory**, and **asks for approval** before risky actions:
   - **Meta Muse** (Sep 8 2026)
   - **OpenAI dots** (DevDay, Sep 29 2026)
   - **Anthropic Claude Duet** (research preview, Oct 21 2026)
   - **Google Gemini Spark** (announced at I/O in May 2026, reached Pro subscribers in July; built on the **Antigravity agent harness**)
   - **xAI Grok Bot** (beta)
2. **The open-source, self-hosted branch** is **OpenClaw** (formerly Clawdbot, then Moltbot) and **Nous Research's Hermes Agent**. Both are local daemons you talk to over Telegram, WhatsApp or iMessage, with markdown memory, cron/heartbeat proactivity and skills; Hermes also writes its own skills. They are the most capable for power users and also the riskiest (ClawHub supply-chain malware).
3. **Messaging-native startups** focus on inbox, calendar and proactive nudges. Poke (reportedly acquired by Cognition in July 2026), Lindy and Martin all work over iMessage or SMS.
4. **Most likely meaning of the user's references:** "Muse" = **Meta Muse** (high confidence). "Dostoevsky" = most likely **OpenAI "dots"** (medium). "Anthropic's Claude" = the Claude agent stack, most likely **Claude Duet** plus Cowork and Claude Code. Muse, dots and Duet are the three marquee always-on agents launched within six weeks of each other, which makes them a coherent trio. See §1.
5. **Relevance to building on Antigravity:** Google's own consumer agent (Gemini Spark) is reported to run on the **Antigravity harness** [M]. The core frontier features fall into two groups:
   - Already in Antigravity: scheduled automations, skills, MCP, sub-agents, memory files.
   - Missing, and these are what make the products above feel magical: an **always-on host**, a **messaging channel**, **event triggers**, **self-written skills**, **credential vaulting**, **payments**, and an **approval gate**.

---

## 1. Decoding the voice-transcribed references

| Transcribed | Best guess | Confidence | Evidence | Alternatives |
|---|---|---|---|---|
| **"Muse"** | **Meta Muse**, Meta's personal agent launched Sep 8 2026 | **High (~85%)** | Exact name match. It is a top-tier personal agent launched two months ago, built on Meta's "Muse Spark" model family, available in WhatsApp, iOS/Android, macOS and the web (meta.com, about.fb.com, datastudios.org, mashable.com). The user's framing (personal assistant, life admin) fits Muse's positioning exactly. | **Manus** (~12%): "Manus" spoken quickly can be heard as "Muse". Manus is a well-known general agent. Some other "Muse" (≈3%). |
| **"Dostoevsky"** | **OpenAI "dots"**, always-on ChatGPT agents announced at DevDay on Sep 29 2026 | **Medium (~50%)** | "Dots" is a brand-new coined word that is unlikely to be in a speech-to-text vocabulary. "Dots…" followed by more speech can plausibly be rendered as "Dost-oy-ev-sky". dots, Muse and Claude Duet are the three headline always-on agents of Sep–Oct 2026 and are compared head to head in press (reviewramble.com, every.to, mindstudio.ai). A user naming three reference products would very likely pick these three. No real agent product called "Dostoevsky" exists; searches return only literary or anime chatbots (character.ai, thepromptindex.com). | **OpenClaw** (~15%): the most famous personal agent of 2026, but phonetically weak. **Devin** (~5%). **Hermes** (~5%). Other (~25%). **Worth confirming with the user.** |
| **"Anthropic's Claude"** | The **Claude agent stack**, most specifically **Claude Duet** (proactive cloud agent, research preview Oct 21 2026), plus **Claude Cowork** (desktop agent with scheduled tasks and Dispatch) and **Claude Code** (CLI with channels, routines and background agents) | High that it means Anthropic; medium on which product | Duet is Anthropic's direct answer to Muse and dots (claude.com, engadget.com, cbsnews.com). Cowork and Claude Code are what power users actually run as personal agents today. | Claude Code used as a personal agent via Telegram channels; Cowork. |

**Takeaway:** treat the reference set as **{Meta Muse, OpenAI dots, Claude Duet/Cowork/Code}**, with **OpenClaw and Hermes** as the open-source architectural references. Confirm "Dostoevsky" with the user.

---

## 2. Product profiles

### 2.1 Meta Muse (consumer, cloud VM) **[H]**
- **What:** A personal agent that takes real-world actions: email, calendar, forms, shopping, deal tracking, travel and restaurant booking, budgeting. It keeps working toward long-running goals after you close the app. It can build "**Spaces**", interactive hubs and trackers that update in the background (for example a wedding planner or a small-business order tracker). It also offers voice conversation with avatars (meta.com, about.fb.com, agileitt.com, forbesmiddleeast.com).
- **Proactivity:** Background goals. Monitors email, calendar and timelines and alerts you unprompted when deadlines or conflicts appear (almcorp.com, techrepublic.com).
- **Memory:** Persistent memory of preferences and habits. You can wipe learned data. Interactions are used for training by default and you must opt out (cnet.com).
- **Channels:** iOS and Android apps, macOS app, web (muse.ai per secondary sources), **natively inside WhatsApp**. Ray-Ban/Meta glasses support is planned.
- **Execution:** A **Muse Secure VM** per user: a persistent, isolated Linux and Chromium environment with browser, filesystem and terminal. It can build micro-tools for itself on the fly.
- **Integrations:** "Utility connectors" for Gmail, Google Calendar and Outlook. "Action connectors" such as **OpenTable**. **Stripe Link "wallet for agents"** for checkout, with single-use virtual cards scoped to the approved amount (stripe.com, fintech.garden).
- **Safety:** **Sentinel**, an out-of-band permission gate that requires human approval for checkouts, sending email and similar actions. A **Secure Credentials Store** lets the model use passwords and cards without seeing the plaintext (meta.com, chatmaxima.com, margrop.net).
- **Pricing:** Free tier; **Power \$20/mo**; **Max \$100/mo**; Muse for Small Business (extremetech.com, gohuman.ai).
- **Weaknesses:**
  - Large privacy backlash: Alex Stamos called it "totally insane" to grant it access to sensitive data (moneywise.com, futurism.com).
  - Reports of it overreaching on local-data permissions (cnet.com, appleinsider).
  - A widely covered incident in which an autonomous Facebook Marketplace deal leaked a reviewer's home address (apnews.com).
  - **Amazon blocks it** (geekwire.com), and banks resist it.
  - The Muse model trails GPT and Claude on deep technical or coding work. No media generation.

### 2.2 OpenAI dots (ChatGPT always-on agents) **[H on existence, M on details]**
- **What:** "Always-on" persistent agents. Each dot has its own **cloud computer and browser** in OpenAI's data centers. It works toward ongoing goals 24/7, catches discrepancies across apps, and notifies you or finishes the work before you ask (openai.com, mindstudio.ai, zdnet.com). Heavy work is delegated to **Codex** (coding) and "ChatGPT Work".
- **Proactivity:** Background monitoring and research with read-only access to connected apps, scheduled recurring checks, and it **texts you first**. ChatGPT **Pulse was retired** and folded into explicit scheduled tasks and web-monitoring jobs (prowlo.com, aiweekly.co).
- **Memory:** A dot-specific memory and notes store, separate from chat memory, that carries across all channels and is formed proactively from connected apps.
- **Channels:** ChatGPT web, desktop and mobile; voice calls; **Slack and Teams**; **iMessage and RCS texting** for US Pro users, routed via the messaging-infra provider Linq. Texting is 1:1 and not end-to-end encrypted (securityonline.info, techstrong.ai).
- **Integrations:** Around 4,000 apps via the ChatGPT apps/plugin ecosystem (Zapier, Slack, Teams, Drive, email, calendar).
- **Safety:** User-defined autonomy versus approval rules, an automated "auto-review" of account-affecting actions, and pause/redirect at any time.
- **Pricing:** Pro tiers only: **Pro \$100** (first dot included), **Pro \$200**, **Pro \$500**; Business Premium \$100–125/user. Extra dots will cost more. Not available on personal plans in the EEA, UK or Switzerland (benchlm.ai, mindstudio.ai, openai.com).
- **Weaknesses:** Expensive entry point. Silent quota burn from background work. Privacy and audit overhead. Texting is not E2E encrypted.
- **Related OpenAI pieces:**
  - **Atlas** browser: agent mode and browser memories. Reportedly phased out as a standalone app and folded into the ChatGPT desktop app and Chrome extension [M].
  - **Agent mode** blocks extension installs, filesystem access and banking sites (humansecurity.com, intuitionlabs.ai).
  - OpenAI hired OpenClaw creator **Peter Steinberger** (Feb 2026) and is a major donor to the OpenClaw Foundation (forbes.com, steipete.me, openclaw.ai).

### 2.3 Anthropic: Claude Duet, Cowork, Claude Code, Claude in Chrome **[H]**
**Claude Duet** (research preview, Oct 21 2026)
- A standalone iOS/Android app plus claude.ai.
- Runs on a **cloud VM with a browser** and keeps working while your devices are off.
- Sends a **morning brief** (Gmail, Calendar, project tools) with ready-to-review drafts. Checks in every few hours and pings you when something has gone quiet (for example an unanswered client email) or a task finishes.
- **Inherits Claude memory, skills and connectors** from the Marketplace's 6,000+ connectors (Slack, QuickBooks, Stripe, Gusto).
- Can build financial models, market briefs, decks and web-app code.
- **Requires approval** before sending, paying, signing, posting or deleting. **Blocks money transfers and securities trading** outright.
- Included free for US Pro, Max and Ultra subscribers via waitlist, but counts against plan usage (claude.com, engadget.com, cbsnews.com, substack).

**Claude Cowork** (Claude Desktop)
- An agent that works on local files, desktop apps, Claude in Chrome, plugins and MCP.
- **Scheduled tasks** (hourly, daily, weekdays, weekly) via `/schedule`. Each run starts a fresh session. The machine must be awake; missed runs fire on the next launch (claude.com, claudecowork.im).
- **Dispatch:** the phone app sends tasks to the paired desktop session, which runs them locally and sends results back. Requires Keep-Awake (claude.com, helpnetsecurity.com).
- **Unified memory:** chat and Cowork memory are merged, written in real time, organized by topic, and editable under Settings › Memory. On by default for consumer plans (enterprisedna.co, cnet.com).

**Claude Code as a personal agent**
- **Channels** plugins connect a running local Claude Code session to **Telegram and Discord**, so you can text your dev box (claudefa.st).
- **Cloud Scheduled Tasks (formerly "Routines")**: triggered by cron, by an API webhook (`/fire`), or by **GitHub events**. They run headless in Anthropic's cloud, so the laptop can be off (claude.com).
- **Agent View / background agents** (`claude --bg`): parallel sessions in git worktrees under a supervisor daemon.
- **Remote control** from mobile or web.

**Claude in Chrome:** a side-panel agent that shares your logged-in browser state. It can click, type, fill forms and run multi-tab workflows, and it doubles as the browser-testing loop for Claude Code (claude.com).

**Weaknesses:**
- Duet is US-only and waitlisted.
- Cowork depends on your machine being awake.
- Heavy agent use eats into usage caps.
- Claude Code's personal-agent setup is DIY.

### 2.4 Google: Gemini Spark / Gemini Agent / Gemini in Chrome / Antigravity **[H on Spark, M on details]**
- **Gemini Spark** (I/O, May 2026)
  - A 24/7 personal agent on dedicated **Google Cloud VMs**, built on Gemini 3.5 Flash and the **Antigravity agent harness** (hbs.edu, blog.google, scalevise.com).
  - **Email-native**: you can delegate by emailing the agent's own address.
  - Deep Workspace integration (Gmail, Calendar, Docs, Sheets, Drive) and **MCP** connections to 30+ third-party services (Salesforce, WhatsApp, GitHub, Dropbox and others).
  - **Skills** (reusable instructions) and **Schedules** (time- or condition-based triggers, e.g. "if my flight is delayed, update my calendar and notify me").
  - Pauses for approval before sends, money movements and sensitive edits.
  - Rollout: Ultra in May, **Pro in July** (US). Now surfaced as **"Chat" / "Task"** buttons in the Gemini app (9to5google.com).
  - Weaknesses: hard to debug when a long task fails midway, caps on concurrent tasks, staggered international rollout, over-permissioning risk (datafloq.com).
- **Gemini in Chrome / Agent Mode / "auto browse":** agentic browsing across tabs, form filling, checkout preparation with human handoff on payments and logins, and the WebMCP standard (chrome.com, pcmag.com).
- **Scheduled Actions** (recurring prompts using Workspace data) and **Personal Intelligence** (a context engine across Gmail, Photos, YouTube and Search history) (gemini.google).
- **Project Mariner** was **shut down on May 4 2026** and folded into Gemini Agent and Chrome. **Project Astra** remains a research prototype, with its capabilities shipped in **Gemini Live** (androidauthority.com, techspot.com, deepmind.google).
- **Antigravity 2.0:** a standalone agent-first desktop app plus CLI and SDK. Scheduled tasks via cron and `/schedule`, dynamic sub-agents, async background tasks, **Remote Agents** that run in Google cloud while the laptop sleeps, and personal-account sign-in on the Free, Pro and Ultra tiers (antigravity.google).
- **Pricing (post-I/O):** AI Pro \$19.99; **new Ultra \$100** (5× Pro limits); **Ultra \$200** (cut from \$250; 20× limits in Gemini and Antigravity) (blog.google).

### 2.5 OpenClaw (formerly Clawdbot, then Moltbot) **[H]**
- **What:** An MIT-licensed, local-first "AI with hands" by Peter Steinberger, renamed twice over trademark concerns around "Claude". It is now run by the **OpenClaw Foundation** (chaired by Dave Morin, backed by OpenAI) (openclaw.ai, thenewstack.io, time.com).
- **Architecture:**
  - A single **Gateway daemon** (WebSocket control plane on 127.0.0.1:18789). It owns every channel connection: **WhatsApp (Baileys), Telegram (grammY), Slack, Discord, Signal, iMessage**.
  - Companion apps, web UI, and paired device "Nodes" (camera, screen).
  - A file-first workspace in `~/.openclaw/workspace/`:
    - `SOUL.md`: persona and rules
    - `MEMORY.md` plus daily `memory/YYYY-MM-DD.md` logs
    - `HEARTBEAT.md`: proactive checklist
  - Sources: openclaw.ai docs, clawdocs.org, openclaw.com.au.
- **Proactivity:**
  - **Heartbeat**: a periodic turn (for example every 30 minutes) in the main session where the agent *decides* whether anything needs doing and stays quiet if not.
  - **Cron jobs**: isolated sessions for deterministic schedules.
  - The heartbeat is being migrated into editable cron jobs.
- **Capabilities:** Shell, files, browser automation, email, calendar, flight booking. Sub-agents for coding that open GitHub PRs. A Twilio voice plugin can make phone reservations and handle IVR menus, escalating mid-call over Telegram ("7 PM is taken, is 8 OK?").
- **Models:** Any model you choose: Claude, GPT, Gemini, local models, or cheap rotations such as Flash or Kimi.
- **Skills:** The **ClawHub** marketplace, using a portable SKILL.md format.
- **Execution:** Your own Mac mini or VPS (the recommended setup) or your laptop (discouraged).
- **Pricing:** Free; you pay for the model API and hosting.
- **Weaknesses:**
  - **Security crisis:** hundreds of malicious ClawHub skills carried prompt or "guidance" injection and AMOS info-stealers through fake prerequisites. Download counts were botted (snyk.io, paloaltonetworks.com Unit 42, arXiv).
  - Exposed servers.
  - Gmail bans automation accounts, so users route through AgentMail.
  - A setup "rabbit hole" with constant API-key and cost juggling (reddit r/openclaw).
- **Culture:** It spawned **Moltbook**, a social network for agents only (ibm.com).

### 2.6 Hermes Agent (Nous Research) **[H on existence, M on numbers]**
- **What:** An open-source (MIT) long-lived agent, first released **Feb 25 2026**. Reportedly past 200k GitHub stars; v0.23.0 "Hearth" shipped in Nov 2026 (github.com/NousResearch/hermes-agent, nousresearch.com, startupfortune.com).
- **Signature feature, a closed learning loop:** after each session a **background reviewer agent** extracts techniques, fixes and user corrections and **writes or patches skills** in `~/.hermes/skills/` (agentskills.io SKILL.md standard) using a `skill_manage` tool. It prefers patching over rewriting. **Progressive disclosure** keeps prompt cost flat: only skill names and one-line summaries load until a skill is fetched with `skill_view` (fp8.co).
- **Memory layers:** `MEMORY.md` and `USER.md`; session search via SQLite FTS5; user modeling via **Honcho**.
- **Channels:** One gateway serves 20+ platforms (Telegram, Discord, Slack and others) plus CLI, TUI, desktop and web, with no duplicated agent instances.
- **Proactivity:** Natural-language or cron schedules that route output to a chat thread or file. Strong at "watcher" jobs that text you only when something matters.
- **Tools:** 40+ built-in (web search, browser, vision, sub-agents).
- **Models:** Nous Portal or OpenRouter (Claude, DeepSeek and others).
- **Weaknesses:** Self-hosting effort (one-click templates help). Self-written skills risk drift; reviewer bias toward "most sessions yield an insight" can bloat the skill library. Same prompt-injection exposure as OpenClaw.

### 2.7 Manus **[H]**
- **What:** A cloud "execution-layer" general agent (Butterfly Effect, Singapore). Plans and executes in a **cloud sandbox VM**: browsing, forms, code, full-stack web apps, Wide/Deep Research, slides, video. Model modes: Manus 2.0 / 2.0 Max / 2.0 Lite (manus.im, nocode.mba, venturebeat.com).
- **Proactivity:** Scheduled and recurring tasks on paid tiers.
- **Channels:** Web and apps, **Telegram** (via a QR link), Slack, WhatsApp, **Mail Manus** (forward email to it).
- **Browser Operator** extension: runs inside your local Chrome or Edge using your logins and cookies.
- **Corporate:** Meta announced an approximately **\$2B acquisition in Dec 2025**. Regulatory problems tied to its Chinese roots reportedly unwound it by mid-2026 and Manus now operates independently [M] (apnews.com, decodingdiscontinuity.com).
- **Pricing:** Credit-based. Free (~300 credits/day); \$20 (~4k credits); \$40 (~8k); \$200 (~40k); Teams from \$20/seat.
- **Weaknesses:** **Runaway credit drain** in error loops, unpredictable cost, no refunds. Trustpilot reviews are harsh compared with G2. Multi-step dead ends (zemith.com, trustpilot.com).

### 2.8 Poke (The Interaction Company of California) **[H on product, M on acquisition]**
- **What:** An assistant that lives in messaging: **iMessage**, WhatsApp, Telegram, RCS. Watches Gmail/Outlook and calendar and **texts you first** about flight changes, important email and conflicts. Drafts email replies, schedules, sets reminders. "Recipes" and integrations (Notion, GitHub, Linear). A strong personality, plus the viral "haggle over your price" onboarding (poke.com, observer.com, saner.ai).
- **Corporate:** Seed \$15M (General Catalyst) plus \$10M (Spark). **Reportedly acquired by Cognition (makers of Devin) in July 2026** [M] (notis.ai).
- **Pricing:** Free / **Pro \$19** / Ultra \$199.
- **Weaknesses:** Noisy email triage, timezone and date errors, slow integrations. **No searchable hub or structured task list**: everything lives in the chat thread.

### 2.9 Lindy **[H]**
- An executive assistant over **iMessage/SMS**, which also gives Siri, CarPlay and Shortcuts access.
- Morning brief, inbox triage with drafts in your voice, autonomous rescheduling.
- **Meeting bot** (joins, transcribes, writes action items and pre-briefs).
- Thousands of integrations plus MCP. **Computer Use** on Pro.
- Pricing: Plus ~\$30–50, Pro ~\$100, Max ~\$200. Credit-metered, 7-day trial, no free tier (lindy.ai, zapier.com, cloudtalk.io).
- Weakness: credit burn on heavy workflows. Business and executive tilt.

### 2.10 Martin **[M]**
- A voice-first "Jarvis" reachable by **iOS app, phone call, SMS, WhatsApp, email and Slack**.
- Gmail/Outlook triage, multi-calendar management, to-dos, reminders set over a phone call, proactive nudges and daily briefings. Learns preferences.
- About \$21/mo (annual). Android is waitlist-only (trymartin.com, apps.apple.com).
- Weakness: shallow beyond inbox, calendar and reminders. No real computer use or coding.

### 2.11 Perplexity: Comet, Assistant, Computer **[H]**
- **Comet** browser with an Assistant sidecar. **Background Assistants** dashboard runs parallel async web jobs and pauses for confirmation on checkouts, logins and form submits (perplexity.ai).
- **Perplexity Computer:** a long-running cloud agent system. **Max \$200/mo** (10k Computer credits); Pro \$20 (4k one-time credits); Enterprise Max \$325/seat.
- **Portable Computer** runs the stack locally on DGX Spark or RTX hardware without consuming credits (perplexity.ai, engadget.com).
- **Strength:** the best citation-grounded research. **Perplexity Finance** combines SEC EDGAR, Quartr transcripts, Morningstar and FactSet [M] (moneyflock.com).
- **Weakness:** weaker at life-admin execution and deep memory. Heavy use requires Max.

### 2.12 xAI Grok Bot **[M]**
- Beta. A team of persistent bots, each with its **own cloud desktop, filesystem, terminal and browser**.
- A **"Chief of Staff" bot coordinates sub-bots** in separate lanes (inbox, research, scheduling).
- An encrypted **Vault** for credentials and payments (X Money, Stripe Link, Shop Pay). Approval cards on desktop and mobile. A bot template marketplace (x.ai, withwillow.ai, ryzlabs.com).

### 2.13 Microsoft Copilot (Tasks / Actions → Cowork / Autopilot) **[M]**
- **Copilot Tasks:** goal-driven browser, file and service actions, run once or on a schedule. Approval required for payments, sharing personal info and communications. A Tasks view with pause, edit and clear-memory controls.
- In late 2026 this was restructured into **Copilot Home**, **Copilot Cowork** (delegated work plus Copilot Studio skills) and **Autopilot** (persistent proactive background agents) (microsoft.com, windowsforum.com).
- Strength: Microsoft 365 data. Weakness: consumer and personal features lag the enterprise ones, and the product line is confusing.

### 2.14 Apple Intelligence / new Siri **[H]**
- Previewed at WWDC (Jun 8 2026) and shipped with **iOS 27 on Sep 14 2026**.
- Personal context across Messages, Mail, Photos, Calendar and Notes. On-screen awareness. **Multi-app action chaining via App Intents** (for example: find the restaurant in a text, check the calendar, book, log it). A chat-style Siri app synced through iCloud.
- On-device and Private Cloud Compute, plus a **custom Gemini model** for heavy reasoning (apple.com, wikipedia).
- Strength: privacy, ambient presence on every Apple device. Weakness: no persistent cloud VM, no long-running autonomy, and only as capable as the App Intents developers expose.

### 2.15 Rabbit **[M]**
- **rabbitOS 2** (Sep 2025): "Creations" (apps you vibe-code by voice), later Claude Code integration and a terminal mode.
- **rabbit intern** (Jun 2025): a cloud general agent for research, sites and decks.
- **rabbit OS3** (Sep 2026): an agentic OS spanning the r1, browser, desktop and messaging, with continuous memory, up to 5 linked devices and computer control (rabbit.tech).
- Niche. The r1 is now mostly a controller node.

### 2.16 Devin (Cognition) **[H]**
- An autonomous software engineer in a **cloud sandbox** (shell, editor, browser). **Devin Desktop** (formerly Windsurf), CLI, DeepWiki, Devin Review.
- Integrations: GitHub, GitLab, Linear, Jira, Slack.
- Pricing: Free / **Pro \$20** / Max \$200 (devin.ai, cognition.com).
- Now also owns **Poke** [M], which suggests a convergence of coding agent and personal agent.
- Coding only; no life admin.

### 2.17 Other notable 2026 entrants (brief, **[L]**)
- **Sai by Simular**: a computer-use personal agent (simular.ai).
- **Gravity**, **Fyxer** (email), **Instinct**: appear in "personal agent" roundups.
- **AgentMail**: an email-inbox service built for agents, popular with OpenClaw users.
- **Linq**: iMessage/RCS infrastructure used by OpenAI dots.
- Finance-specific: **FinChat / Fiscal.ai**, and the open-source multi-agent frameworks **TradingAgents**, **AI Hedge Fund** and **FinRobot**.
- Tax and bookkeeping: **Keeper**, **Digits**, **Puzzle**, **Intuit Assist**. Agents prepare the work; licensed software and humans still file.

---

## 3. Feature taxonomy: what makes a frontier personal agent powerful

| # | Feature | What "frontier" looks like | Why it matters |
|---|---|---|---|
| F1 | **Always-on host** | A persistent cloud VM or home daemon that runs while your devices sleep | Removes "the laptop must be awake" failures (Cowork's main weakness) |
| F2 | **Proactive heartbeat** | Periodic *judgment* turns ("anything worth telling him?") that stay silent if not | The difference between a scheduler and an assistant (OpenClaw heartbeat, Duet check-ins, Poke nudges) |
| F3 | **Scheduled jobs (cron)** | Deterministic, isolated runs with output routed to a channel | Briefs, digests, monitors |
| F4 | **Event triggers** | Webhooks, new email, GitHub events, flight delays, price thresholds | Real-time reaction instead of polling (Claude routines `/fire` and GitHub triggers, Spark condition schedules) |
| F5 | **Messaging-native interface** | iMessage, WhatsApp, Telegram, SMS; the agent texts you first; rich approval replies | Zero-friction, always in your pocket (Poke, Lindy, OpenClaw, dots, Muse) |
| F6 | **Multi-channel continuity** | One agent identity and memory across chat, voice, email, Slack and desktop | No context loss (Hermes gateway, dots) |
| F7 | **Voice / phone** | Live voice, outbound phone calls, IVR navigation | Reservations and customer-service calls (OpenClaw Twilio plugin, Martin, dots voice) |
| F8 | **Persistent long-term memory** | Curated facts, episodic logs, user model; editable and auditable | Personalization without repeated explaining |
| F9 | **Self-improving skills** | The agent writes and patches its own SKILL.md after sessions | Compounding capability (Hermes is the clearest example; Muse builds micro-tools) |
| F10 | **Skills / plugin ecosystem** | A portable SKILL.md, MCP, a marketplace | Breadth, but also a supply-chain risk |
| F11 | **Computer / browser use** | Its own browser plus the option to use *your* logged-in browser | Reaches sites that have no API |
| F12 | **Code execution sandbox** | Shell, filesystem, git, test runners | Software engineering, data analysis |
| F13 | **Sub-agents / teams** | An orchestrator plus parallel specialists ("Chief of Staff") | Long-horizon, parallel work |
| F14 | **Credential vault** | The model uses secrets without seeing them | Safe logins (Muse Secure Credentials Store, Grok Vault) |
| F15 | **Payments** | Agent wallets, single-use scoped virtual cards (Stripe Link, Shop Pay, network tokens) | Bookings and purchases |
| F16 | **Approval gate / policy engine** | Out-of-band gating of irreversible actions; hard blocks (no transfers or trades) | Trust and safety (Sentinel, Duet, Spark) |
| F17 | **Audit trail / observability** | Step logs, a task dashboard, cost meter | Debugging and trust (Comet dashboard, Copilot Tasks view) |
| F18 | **Artifacts / living dashboards** | Persistent trackers that update in the background | Muse Spaces, Manus apps; fixes Poke's "no hub" complaint |
| F19 | **Model flexibility / cost control** | Bring any model, route cheap vs smart, budgets | OpenClaw and Hermes; stops runaway credit burn (Manus complaint) |
| F20 | **Local / private option** | Self-hosted or on-device | Privacy for finances and taxes (OpenClaw, Hermes, Perplexity Portable, Apple PCC) |

### 3.1 Feature × product matrix
Legend: ● strong / native · ◐ partial / beta / DIY · ○ absent or unknown

| Feature | Muse | dots | Claude Duet | Cowork | Claude Code | Gemini Spark | OpenClaw | Hermes | Manus | Poke | Lindy | Perplexity | Grok Bot | Siri |
|---|---|---|---|---|---|---|---|---|---|---|---|---|---|---|
| F1 Always-on host | ● | ● | ● | ○ | ◐ (cloud tasks) | ● | ● (self-host) | ● (self-host) | ◐ | ● | ● | ◐ | ● | ○ |
| F2 Proactive heartbeat | ● | ● | ● | ○ | ○ | ● | ● | ◐ | ○ | ● | ● | ○ | ◐ | ◐ |
| F3 Cron schedules | ● | ● | ◐ | ● | ● | ● | ● | ● | ● | ◐ | ● | ◐ | ● | ○ |
| F4 Event triggers | ◐ | ◐ | ◐ | ○ | ● (GitHub, webhook) | ● | ◐ | ◐ | ○ | ● (email) | ● | ○ | ◐ | ◐ |
| F5 Messaging-native | ● (WhatsApp) | ● (iMessage/RCS) | ○ (app push) | ○ | ◐ (Telegram) | ◐ (email) | ● | ● | ◐ (Telegram) | ● | ● | ○ | ○ | ● |
| F6 Multi-channel | ● | ● | ◐ | ◐ | ◐ | ● | ● | ● | ◐ | ● | ● | ◐ | ◐ | ● |
| F7 Voice / phone | ● | ● | ◐ | ○ | ○ | ● (Live) | ◐ (plugin) | ◐ | ○ | ○ | ◐ | ◐ | ○ | ● |
| F8 Long-term memory | ● | ● | ● | ● | ◐ (CLAUDE.md) | ● | ● (md files) | ● | ◐ | ● | ● | ◐ | ● | ● |
| F9 Self-improving skills | ◐ (micro-tools) | ○ | ○ | ○ | ○ | ○ | ◐ | ● | ○ | ○ | ○ | ○ | ○ | ○ |
| F10 Skills / plugins | ◐ | ● | ● | ● | ● | ● | ● | ● | ◐ | ◐ | ● | ◐ | ● | ◐ (App Intents) |
| F11 Browser use | ● | ● | ● | ● | ● (Chrome) | ● | ● | ● | ● | ○ | ◐ | ● | ● | ○ |
| F12 Code sandbox | ● | ● (Codex) | ● | ● | ● | ● | ● | ● | ● | ○ | ○ | ◐ | ● | ○ |
| F13 Sub-agents | ○ | ● | ○ | ◐ | ● | ◐ | ● | ● | ● | ○ | ◐ | ● | ● | ○ |
| F14 Credential vault | ● | ◐ | ◐ | ○ | ○ | ◐ | ○ | ○ | ◐ (your browser) | ○ | ◐ | ◐ | ● | ● |
| F15 Payments | ● (Stripe Link) | ◐ | ◐ (Shop Pay) | ○ | ○ | ◐ | ◐ (DIY) | ○ | ○ | ○ | ○ | ◐ | ● | ● (Apple Pay) |
| F16 Approval gate | ● (Sentinel) | ● | ● | ◐ | ◐ | ● | ◐ (DIY) | ◐ | ◐ | ◐ | ● | ● | ● | ● |
| F17 Audit / dashboard | ● | ◐ | ◐ | ◐ | ● | ◐ | ◐ | ◐ | ● | ○ | ● | ● | ● | ○ |
| F18 Living artifacts | ● (Spaces) | ◐ | ● | ● | ◐ | ● | ◐ | ◐ | ● | ○ | ○ | ◐ | ◐ | ○ |
| F19 Model choice / cost | ○ | ○ | ○ | ○ | ○ | ○ | ● | ● | ◐ | ○ | ◐ | ● | ○ | ○ |
| F20 Local / private | ○ | ○ | ○ | ● (local) | ● (local) | ○ | ● | ● | ○ | ○ | ○ | ◐ (Portable) | ○ | ● |

*(This matrix is synthesized from the profiles above. Cells marked ◐ often reflect thin evidence. Treat it as directional.)*

---

## 4. Fit for the user's three use cases

### 4.1 Side-project software engineering
| Strong | Why |
|---|---|
| **Claude Code** (+ cloud scheduled tasks, Telegram channels, Agent View) | Best-in-class coding plus GitHub-event triggers, so a personal dev agent works while the laptop is off |
| **Devin** | A fully autonomous cloud SWE at \$20/mo, ticket to PR |
| **OpenAI dots → Codex** | A dot can watch a repo or bug channel and delegate to Codex |
| **OpenClaw / Hermes** | Text your home box: "fix the failing test, open a PR"; sub-agents; any model |
| **Antigravity** | Already strong here: agentic IDE, sub-agents, browser testing, scheduled tasks |
| Manus | Good for greenfield web apps; weaker for an existing repo |

**Weak:** Muse (the model trails on code), Poke, Lindy, Martin, Siri, Perplexity.

### 4.2 Investment research
| Strong | Why |
|---|---|
| **Perplexity (Finance + Computer)** | Live data with citations to SEC filings and transcripts; parallel background research |
| **Claude (Duet / Cowork)** | Long-document reasoning on 10-Ks; financial models and market briefs; Duet **blocks trading**, which is a safe default |
| **OpenAI dots / ChatGPT** | Python data work, scheduled monitors, proactive alerts |
| **Gemini Spark** | Scheduled market briefs into Sheets and Docs; Deep Research |
| **OpenClaw / Hermes** | Custom watchers (news, Reddit, X, filings) feeding a Notion or markdown knowledge base; alert only on threshold. Hermes "watcher" jobs suit this especially well |
| Manus | Wide Research for one-off deep reports; credit-hungry |

**Gap across all of them:** no product combines (a) a persistent **thesis memory** per holding, (b) **event triggers** on filings, earnings and price, and (c) quantitative tooling. This is a clear opportunity for a custom agent. **Weak:** Poke, Lindy, Martin, Siri, Muse.

### 4.3 Life admin (email, reservations, taxes, bookkeeping)
| Sub-task | Best fits |
|---|---|
| **Email triage and drafting** | Poke, Lindy, Martin (messaging-native nudges); Claude Duet (morning brief, "gone quiet" alerts); Gemini Spark (Gmail-native, email-the-agent); dots |
| **Reservations and bookings** | **Meta Muse** (OpenTable action connector plus Stripe Link payments); OpenClaw Twilio voice plugin for phone-only venues; Gemini in Chrome and Comet for web booking with handoff |
| **Purchases and payments** | Muse (scoped single-use cards); Grok Bot (Vault); Siri (Apple Pay). Amazon blocks Muse |
| **Bookkeeping** | Claude Duet (QuickBooks, Stripe and Gusto connectors); dots (spreadsheet anomaly catching); custom local scripts for privacy |
| **Taxes** | Nobody files autonomously. Agents can gather documents (W-2s, 1099s, receipts → CSV) and prepare workpapers for a CPA or tax software (weareocta.com, reddit). For privacy, many users keep this local |

**Weak:** Devin, Claude Code (without extra wiring), Perplexity (execution).

**Key risks here:** privacy (Muse backlash), Gmail banning automated accounts, bot-blocking by merchants and banks.

---

## 5. Implications for a lightweight Antigravity-based build

The capabilities below are what the products in this report converge on. Antigravity coverage is per the search results and the antigravity.google docs.

| Frontier capability | Antigravity today | Gap to close (lightweight) |
|---|---|---|
| Scheduled jobs (F3) | ● Scheduled automations, `/schedule`, cron | Route output to a messaging channel |
| Always-on host (F1) | ◐ Remote Agents / cloud; desktop app | Run on a spare Mac or VPS, or rely on Remote Agents |
| Proactive heartbeat (F2) | ○ (cron only) | A cheap periodic "triage turn" with a HEARTBEAT checklist and a stay-silent default |
| Event triggers (F4) | ◐ hooks | A webhook or email-poll bridge (Gmail push, GitHub, price alerts) |
| Messaging interface (F5) | ○ | A Telegram bot is easiest; iMessage via a Mac bridge; WhatsApp via Baileys-type libraries (ToS risk) |
| Memory (F8) | ◐ rules and skills files | A MEMORY.md / USER.md plus daily logs pattern (OpenClaw and Hermes) |
| Self-improving skills (F9) | ◐ skills exist | A Hermes-style post-session reviewer that patches SKILL.md |
| Sub-agents (F13), browser (F11), code (F12), MCP (F10) | ● | none |
| Credential vault (F14) | ○ | macOS Keychain or 1Password CLI, injected by tools and never shown to the model |
| Payments (F15) | ○ | Defer; or Stripe Link / virtual cards with an approval gate |
| Approval gate (F16) | ◐ permissions | Out-of-band "approve?" replies in the messaging channel for sends, pays and deletes; hard-block trades and transfers (copy Duet's policy) |
| Audit (F17) | ◐ transcripts | A daily digest of actions taken and cost |

> [!WARNING]
> **Security lesson from OpenClaw and Muse:** skills are executable trust. Pin and review third-party skills, keep secrets out of the model context, run risky browsing in an isolated profile or VM, and put a human approval step on every outbound or irreversible action.

---

## 6. Source index (domains and canonical pages cited)
- **Meta Muse:** meta.com, about.fb.com, whatsapp.com, stripe.com, cnet.com, futurism.com, moneywise.com, apnews.com, geekwire.com, engadget.com, extremetech.com, mashable.com, forbesmiddleeast.com, datastudios.org, chatmaxima.com, margrop.net, techrepublic.com, almcorp.com, fintech.garden
- **OpenAI dots / Atlas / Pulse:** openai.com, chatgpt.com, zdnet.com, mindstudio.ai, every.to, cbsnews.com, benchlm.ai, securityonline.info, techstrong.ai, datacamp.com, prowlo.com, humansecurity.com, businessinsider.com
- **Anthropic:** claude.com, anthropic.com, engadget.com, cbsnews.com, claudecowork.im, claudefa.st, helpnetsecurity.com, enterprisedna.co
- **Google:** blog.google, gemini.google, antigravity.google, chrome.com, deepmind.google, 9to5google.com, pcmag.com, androidauthority.com, techspot.com, hbs.edu, scalevise.com, datafloq.com
- **OpenClaw:** openclaw.ai (docs), github.com/openclaw, clawdocs.org, steipete.me, forbes.com, time.com, thenewstack.io, snyk.io, paloaltonetworks.com (Unit 42), milvus.io, ibm.com, reddit.com/r/openclaw
- **Hermes:** nousresearch.com, github.com/NousResearch/hermes-agent, fp8.co, ishosting.com, startupfortune.com
- **Manus:** manus.im, apnews.com, venturebeat.com, nocode.mba, emergent.sh, zemith.com, trustpilot.com
- **Poke:** poke.com, observer.com, notis.ai, saner.ai, therundown.ai
- **Lindy:** lindy.ai, zapier.com, cloudtalk.io · **Martin:** trymartin.com, apps.apple.com
- **Perplexity:** perplexity.ai, engadget.com, geotoolbox.ai · **Grok Bot:** x.ai, withwillow.ai, ryzlabs.com
- **Microsoft:** microsoft.com, windowsforum.com · **Apple:** apple.com, wikipedia.org · **Rabbit:** rabbit.tech · **Devin:** devin.ai, cognition.com
- **Finance and tax:** moneyflock.com, wallstreetprep.com, weareocta.com, intuit.com, barebone.ai, pinggy.io
