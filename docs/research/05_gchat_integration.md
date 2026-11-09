# 05 — Google Chat Integration Pathways & Account Architecture Audit

*Research Date: 2026-11-08 · Author: `[Task:gchat] gchat-integration-rfc` (`gemini-4-argon-high`)*  
*Related Docs: [RFC-0003](../design/0003-approval-bound-execution.md), [RFC-0004](../design/0004-heartbeat-and-channels.md), [RFC-0005](../design/0005-gmail-integration.md), [RFC-0008](../design/0008-gchat-integration.md)*

---

## 0. Executive Summary (Key Findings)

1. **Why Antigravity Has No Official `gchat` Plugin (Unlike Gmail, Calendar, Drive, Docs, Sheets, Slides):**
   - Local inspection of `~/.gemini/config/plugins/`, `https://www.gstatic.com/antigravity/plugins/`, and `https://storage.googleapis.com/antigravity-public/plugin/cli/` confirms that Antigravity ships first-party OAuth2 CLI plugins for `gmail`, `gcalendar`, `gdocs`, `gdrive`, `gsheets`, and `gslides`, but **returns `404` for `gchat` / `chat`**.
   - The root cause is an upstream Google platform boundary: while Gmail, Calendar, and Drive REST APIs treat consumer `@gmail.com` accounts and Google Workspace accounts identically under User OAuth 2.0, **the Google Chat REST API (`chat.googleapis.com/v1`), Space Incoming Webhooks, and GCP Chat App Configuration are organization-gated to Google Workspace accounts**.
2. **Correction to Earlier `RFC-0004` / `ADR-0004` Assumption:**
   - [RFC-0004](../design/0004-heartbeat-and-channels.md) and [ADR-0004](../decisions/0004-remote-control-first-pluggable-channels.md) previously assumed that Desktop OAuth user-level scopes (`chat.spaces`, `chat.messages`) work directly on personal `@gmail.com` accounts.
   - **Empirical Verification:** Every call to `chat.googleapis.com/v1/*` requires the OAuth client's GCP project to have a registered **Google Chat App** in the GCP Console `Configuration` tab (otherwise the API returns `HTTP 404: Google Chat app not found`). Attempting to access the `Configuration` tab or register a Chat App with a consumer `@gmail.com` account fails with:
     > *"The Google Chat API is only available to Google Workspace users."*
   - Furthermore, consumer `@gmail.com`-created spaces disable **Incoming Webhooks** (`Apps & integrations -> + Add webhooks` is unavailable).
3. **Two Distinct Roles for Google Chat in `agy-hq`:**
   - **Role A — User Workspace & Communication Tool (`./bin/hq gchat`):** Searching spaces, reading DMs/group threads in Quarantined Reader mode, drafting replies, and executing 1-tap mobile-approved sends (`gchat_send`)—analogous to `./bin/hq gmail`.
   - **Role B — Bidirectional Assistant Control Channel (`internal/channels/gchat.go` — `ChannelAdapter` M3.1):** Delivering Morning Briefs, interactive approval notifications, and receiving user commands in a dedicated self-chat space (`owner_space_id`).
4. **Recommended Multi-Transport Architecture:**
   - Rather than relying on a single fragile mechanism, `internal/gchat` should decouple **safe CRUD + quarantine + approval invariants** from a **Pluggable Transport Adapter** supporting:
     1. **Google Workspace REST API v1 (`oauth` / `gws` transport):** Native `chat.googleapis.com/v1` User OAuth 2.0 (plus optional Workspace Events API + Cloud Pub/Sub and Incoming Webhooks) for Google Workspace accounts (`@company.com` or a $7/mo personal-domain Workspace Business Starter account).
     2. **Workspace External Space Bridge (`external-space` hybrid):** A Workspace-owned `[HQ] Command Center` space that invites the user's personal `@gmail.com` account as an external guest—giving the user native mobile/web Google Chat access on `@gmail.com` while giving `agy-hq` 100% official REST API + Webhook access.
     3. **Local Chrome DevTools / CDP Transport (`cdp` transport):** Connects to an isolated, persistent local Chrome profile (`local/private/gchat/chrome-profile`) signed into `chat.google.com` for personal `@gmail.com` DMs and consumer spaces, with hard `PreToolUse` guardrails that forbid direct LLM browser clicking/typing and route all sends through `./bin/hq propose` + `./bin/hq approve`.
     4. **Gmail `chat-noreply@google.com` Fallback (`gmail-bridge` transport):** Zero-setup read-only ingestion of missed personal Google Chat DMs and `@mentions` forwarded to Gmail.

---

## 1. Pathway 1: Google Workspace vs. Personal (`@gmail.com`) Account Limitations & Auth Models

### 1.1 Capability Matrix by Account Type

| Capability / Surface | Personal Consumer (`@gmail.com`) | Google Workspace Individual / Cloud Identity Free | Google Workspace (Business Starter / Standard / Enterprise) | Personal `@gmail.com` Invited to Workspace External Space |
| :--- | :--- | :--- | :--- | :--- |
| **`chat.google.com` Web & Mobile UI** | ✅ Full (DMs, Group Chats, Spaces) | ✅ Consumer UI only | ✅ Full Organizational UI | ✅ Full UI (marked with `External` badge) |
| **GCP Console: Enable `chat.googleapis.com`** | ⚠️ API toggle enables, but **Configuration tab blocked** | ❌ Blocked (`Workspace users only`) | ✅ Full access | ✅ Hosted in Workspace GCP project |
| **REST API v1 (`chat.googleapis.com/v1`) User OAuth** | ❌ Fails (`404 Chat app not found` / `403 Forbidden`) | ❌ Fails | ✅ Full (`chat.spaces`, `chat.messages.*`, `chat.users.readstate`) | ✅ Executed by Workspace host account in shared space |
| **REST API v1 Service Account (`chat.bot`)** | ❌ Cannot install custom bots in consumer spaces | ❌ Blocked | ✅ Full (`spaces.messages.create`, Card v2, slash commands) | ⚠️ Visible in space; external guest card clicks restricted |
| **Space Incoming Webhooks (`?key=...&token=...`)** | ❌ Disabled in UI (`+ Add webhooks` greyed out) | ❌ Disabled | ✅ Full (`POST` text or Card v2 JSON in `<100ms`) | ✅ Webhook created by Workspace owner; visible to `@gmail.com` guest |
| **Google Workspace Events API (Cloud Pub/Sub)** | ❌ Unavailable | ❌ Unavailable | ✅ Real-time `chat.message.v1.created` across `spaces/-` | ✅ Subscribed by Workspace host account for shared space |
| **Google Apps Script Advanced `Chat` Service** | ❌ Fails (wraps `chat.googleapis.com/v1`) | ❌ Fails | ✅ Full (`Chat.Spaces.Messages.*`) | ✅ Runs under Workspace host account |
| **AppSheet Chat Apps** | ❌ Explicitly blocked by Google docs | ❌ Blocked | ✅ Supported | ❌ External guests cannot invoke AppSheet bots |
| **Local Chrome CDP (`chat.google.com` session)** | ✅ Works for all personal DMs & Spaces | ✅ Works | ✅ Works (unless blocked by enterprise MDM policy) | ✅ Works |
| **Gmail `chat-noreply@google.com` Notifications** | ✅ Automatic email on unread DMs / `@mentions` | ✅ Automatic | ✅ Automatic | ✅ Automatic |

### 1.2 Why Consumer `@gmail.com` OAuth Fails Against `chat.googleapis.com/v1`
Unlike the Gmail API (`gmail.googleapis.com`), where any GCP project can create a Desktop OAuth 2.0 Client ID and immediately call `users.messages.list` for a `@gmail.com` test user, `chat.googleapis.com/v1` enforces a **two-stage organizational check**:
1. **Project-Level App Check:** Every REST call to `chat.googleapis.com/v1` looks up the Google Chat App configuration associated with the caller's GCP Project Number. Without a saved Chat App configuration, the API rejects all requests with:
   ```json
   {
     "error": {
       "code": 404,
       "message": "Google Chat app not found. To create a Chat app, you must turn on the Chat API and configure the app in the Google Cloud console.",
       "status": "NOT_FOUND"
     }
   }
   ```
2. **Workspace Principal Check:** Saving the Chat App configuration in GCP Console requires the GCP project to belong to a Google Workspace organization (or the administrator to belong to a Workspace domain with Google Chat enabled). Consumer `@gmail.com` principals are rejected at configuration time and when attempting user-scoped API calls outside a Workspace tenant.

---

## 2. Pathway 2: Google Chat REST API v1 (`chat.googleapis.com/v1`)

When operating with a Google Workspace account (either a work account or a personal custom domain on Workspace Business Starter), `chat.googleapis.com/v1` provides a clean, strongly typed REST interface.

### 2.1 Authentication Models Compared

| Auth Model | OAuth Scopes | Capabilities | Security & Fit for `agy-hq` |
| :--- | :--- | :--- | :--- |
| **1. User OAuth 2.0 (Split Read/Write Tokens)** | **Read Token (Sidecar & Search):**<br>• `chat.spaces.readonly`<br>• `chat.memberships.readonly`<br>• `chat.messages.readonly`<br>• `chat.users.readstate.readonly`<br>**Write Token (Approve Executor):**<br>• `chat.messages.create` | • List all DMs and Spaces user belongs to<br>• Read message history & thread read states<br>• Search messages (`spaces/-/messages:search`)<br>• Post messages/replies as the user (`chat.messages.create` cannot edit or delete history) | **★★★★★ (Recommended for Workspace).** Enforces least privilege: `hq daemon` sidecar holds only the read-only token; `chat.messages.create` is only invoked by `./bin/hq approve` after SHA-256 verification. |
| **2. Service Account (`chat.bot` / `chat.app.*`)** | • `chat.bot`<br>• `chat.app.spaces`<br>• `chat.app.messages.readonly` | • Posts with `[App]` badge<br>• Only sees spaces where the bot was explicitly added<br>• Supports interactive Card v2 widgets (`buttonList`) | **★★★★☆ (Good for `ChannelAdapter` self-chat space).** Cannot read the user's regular 1:1 human DMs unless added to each conversation. |
| **3. Domain-Wide Delegation (DWD)** | Service account JWT with `sub: user@domain.com` + User scopes | • Silent impersonation of any domain user without interactive OAuth | **★☆☆☆☆ (Rejected).** Requires Super Admin tenant-wide grant; a compromised key exposes all domain users with zero interactive revocation boundary. |

### 2.2 Core REST API v1 Endpoints & Resource Semantics

1. **Listing Spaces & DMs (`GET /v1/spaces`):**
   - Returns `spaces[]` with `name` (`spaces/AAAA...`), `spaceType` (`SPACE`, `GROUP_CHAT`, `DIRECT_MESSAGE`), `singleUserBotDm`, `displayName`, and `lastActiveTime`.
2. **Checking Unread State in 0 Tokens (`GET /v1/users/me/spaces/{space}/spaceReadState`):**
   - Returns `lastReadTime` (RFC3339 timestamp). Comparing `space.lastActiveTime > spaceReadState.lastReadTime` enables `hq daemon` to detect unread activity across spaces in pure Go with **0 LLM tokens**.
3. **Listing & Searching Messages (`GET /v1/spaces/{space}/messages` & `GET /v1/spaces/-/messages:search`):**
   - Supports `pageSize`, `orderBy="createTime desc"`, and `filter='createTime > "2026-11-08T00:00:00Z"'`.
   - Each `Message` resource contains `name` (`spaces/{space}/messages/{msg}`), `sender.name` (`users/{id}`), `sender.displayName`, `sender.type` (`HUMAN` vs `BOT`), `createTime`, `text` (plain text), `formattedText`, `thread.name` (`spaces/{space}/threads/{thread}`), and `attachment[]`.
4. **Creating Messages & Thread Replies (`POST /v1/spaces/{space}/messages`):**
   - Query param: `messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD`.
   - Body:
     ```json
     {
       "text": "Plain-text message body",
       "thread": {
         "name": "spaces/AAAABBB/threads/12345678"
       }
     }
     ```
5. **Crucial Difference vs. Gmail REST API — No Server-Side Drafts Resource:**
   - Unlike Gmail (`users.drafts.create` / `users.drafts.send`), **Google Chat REST API v1 has no server-side draft resource** (Google Chat UI drafts are stored only in browser `IndexedDB`/`localStorage`).
   - **Architectural Implication for `agy-hq`:** `./bin/hq gchat draft` must persist immutable local draft manifests (`local/memory/gchat/drafts/<draftId>.json`) with a deterministic SHA-256 `contentHash` over `(account, spaceId, threadId, body)`.

### 2.3 Google Workspace CLI (`@googleworkspace/cli` / `gws chat`)
- Google's open-source Discovery-driven CLI (`gws`) supports `gws chat spaces list`, `gws chat spaces messages list`, and `gws chat spaces messages create`.
- Like `chat.googleapis.com/v1`, `gws chat` requires a Workspace-backed OAuth client for Chat endpoints.
- Notice [`internal/hooks/hooks.go`](../../internal/hooks/hooks.go) line 332 already blocks `gws (gmail|chat) (send|create|post)` in `outboundCommandPatterns`!

---

## 3. Pathway 3: Google Chat Apps, Chatbots, Apps Script, AppSheet & Cloud Pub/Sub

### 3.1 Cloud Pub/Sub & Google Workspace Events API (`workspaceevents.googleapis.com`)
- **How it works:**
  - A Workspace user creates a subscription (`POST https://workspaceevents.googleapis.com/v1/subscriptions`) targeting `//chat.googleapis.com/spaces/-` (all user spaces) or `//chat.googleapis.com/spaces/{space}` for event type `google.workspace.chat.message.v1.created`, delivering events to `projects/<id>/topics/<topic>`.
  - `hq daemon` pulls from the Cloud Pub/Sub subscription over outbound HTTPS (`pubsub.googleapis.com`).
- **Pros:**
  - Zero inbound firewall ports, webhooks, or ngrok/Cloudflare tunnels required on the local host.
  - Near-instant push latency (`<1–2s`) vs. periodic REST polling.
- **Cons / Caveats:**
  - Requires a Google Workspace account + GCP project with Pub/Sub enabled.
  - User-authenticated Workspace Events subscriptions have a maximum TTL of **24 hours (`86400s`)** unless renewed automatically by `hq daemon` (`subscriptions.patch`).

### 3.2 Google Apps Script Bridge (`Chat` Advanced Service vs. `UrlFetchApp`)
- **Advanced Chat Service (`Chat.Spaces.Messages.*`):**
  - Directly wraps `chat.googleapis.com/v1`. Fails on personal `@gmail.com` accounts unless linked to a Workspace GCP project.
- **Apps Script as a Webhook Relay (`doPost` + `UrlFetchApp`):**
  - Inside a Workspace space, an Incoming Webhook URL (`https://chat.googleapis.com/v1/spaces/{space}/messages?key=...&token=...`) can be called directly from Go (`net/http`) without needing Apps Script in the middle.
  - On a personal `@gmail.com` account, Apps Script can poll `GmailApp.search('from:chat-noreply@google.com is:unread')`, but `agy-hq` already has native `./bin/hq gmail` sidecar polling in pure Go—making an Apps Script middleman redundant.

### 3.3 AppSheet Chat Apps
- Requires a Google Workspace account, forces interaction into AppSheet's rigid view/action DSL, and cannot act as a general-purpose CLI transport for `agy-hq`. **Verdict: Not recommended.**

---

## 4. Pathway 4: Chrome DevTools MCP & Local Browser Automation (`chat.google.com`)

For a personal `@gmail.com` account (`user@gmail.com`) where `chat.googleapis.com/v1` is blocked by Google's consumer policy, the official web client (`https://chat.google.com`) has full access to all personal 1:1 DMs, group chats, and spaces.

### 4.1 Option 4A: Managed Local Chrome CDP Transport (`chrome-devtools-mcp` / Headless CDP)
- **Host Environment Check:**
  - `/usr/bin/google-chrome` is installed locally.
  - `chrome-devtools-plugin` (`1.10.1`) is installed in `~/.gemini/config/plugins/chrome-devtools-plugin`.
- **How It Works Safely in `agy-hq`:**
  1. **Isolated Persistent Vault Profile (`local/private/gchat/chrome-profile`):**
     - Rather than attaching to the user's daily browsing tabs (which exposes banking/personal tabs to DOM snapshots), `./bin/hq gchat init --transport=cdp` launches a dedicated Chrome instance bound to `127.0.0.1:9222` with `--user-data-dir=local/private/gchat/chrome-profile` (`0700` permissions inside the git-ignored private Vault).
     - After a one-time Google sign-in to `https://chat.google.com`, session cookies persist across restarts in `local/private/gchat/chrome-profile`.
  2. **Deterministic Go CDP Execution (Zero Unsupervised LLM Browser Clicking):**
     - **Critical Threat:** If an LLM agent is allowed to use `chrome-devtools` MCP tools (`click`, `fill`, `press_key`, `evaluate_script`) directly on `chat.google.com`, a prompt injection could type a message and hit `Enter`, completely bypassing `./bin/hq propose`!
     - **Mechanical Defense:**
       - `PreToolUse` (`internal/hooks/hooks.go`) **hard-blocks** any `chrome-devtools` MCP tool call (`navigate_page`, `new_page`, `evaluate_script`, `click`, `fill`) when the URL or target page matches `chat.google.com` or `mail.google.com/chat`.
       - All browser interaction with `chat.google.com` is encapsulated inside `./bin/hq gchat <spaces|search|read-quarantined|send-draft>` in Go over the local Chrome DevTools Protocol (`ws://127.0.0.1:9222`).
       - `hq gchat draft` never touches the browser DOM—it writes strictly to `local/memory/gchat/drafts/<draftId>.json`.
       - Only `hq approve <id> --sha256=<prefix>` invokes `hq gchat send-draft`, which navigates to the exact target `spaceId`, verifies the space header, injects the verified plain-text draft, and submits it deterministically in Go (`0` LLM tokens).

### 4.2 Option 4B: Reverse-Engineered `DynamiteWebUi` / `WebChannel` Cookie Bridge (`mautrix-googlechat`)
- **How It Works:**
  - Extracts `COMPASS`, `SSID`, `SID`, `HSID`, `OSID`, and `SAPISID` cookies from Chrome and speaks Google's internal protobuf/pblite `batchexecute` and `/webchannel/register` endpoints directly.
- **Why We Reject Option 4B:**
  1. **Session Collision & Frequent Breakage:** Google's backend invalidates `COMPASS`/WebChannel tokens when the user opens `chat.google.com` or Gmail in a regular browser tab.
  2. **ToS & Account Lockout Risk:** Reverse-engineered non-browser TLS/HTTP fingerprints calling internal `DynamiteWebUi` RPCs risk automated abuse detection on the user's primary Google account.
  3. **Maintenance Burden:** Internal protobuf field numbers change without notice.

---

## 5. Pathway 5: Webhook Proxies & Bridging Mechanisms

### 5.1 Bridge 5A: The "Workspace External Space Bridge" (Best of Both Worlds for `ChannelAdapter`)
If the user has (or provisions) any Google Workspace account (e.g., a work account or a $7/mo personal-domain Google Workspace Business Starter account):
1. The Workspace account creates a dedicated Google Chat Space (`[HQ] Front Desk`) with **"Allow people outside your organization to join"** enabled.
2. The Workspace account invites the user's personal `@gmail.com` account (`user@gmail.com`) into `[HQ] Front Desk`.
3. The Workspace account creates an **Incoming Webhook URL** (`https://chat.googleapis.com/v1/spaces/<SPACE_ID>/messages?key=...&token=...`) and/or authorizes a **Workspace User OAuth Token** locked to `<SPACE_ID>`.
4. **Result:**
   - The user interacts with `[HQ] Front Desk` inside their everyday **personal `@gmail.com` Google Chat mobile & web app** (receiving instant mobile push notifications!).
   - `agy-hq` uses 100% official, zero-browser **Incoming Webhooks + Google Chat REST API v1** to post Morning Briefs, stage 1-tap approval requests, and read replies in `[HQ] Front Desk`!

### 5.2 Bridge 5B: Gmail `chat-noreply@google.com` Read-Only Notification Bridge
- Google Chat automatically sends email notifications from `chat-noreply@google.com` to the user's Gmail inbox whenever a personal 1:1 DM or `@mention` goes unread while away/offline.
- Because `./bin/hq gmail` (`internal/gmail`) is already live and authenticated for `user@gmail.com`, `internal/gchat` can include a **zero-setup `gmail-bridge` fallback** that queries `from:chat-noreply@google.com is:unread` to surface missed Google Chat messages in `/morning-brief` and `/inbox-triage` even when Chrome CDP is not running!
- *Limitation:* Read-only and delayed by Google Chat's offline notification timer (~2–5 minutes); replies still require CDP or REST transport.

---

## 6. Synthesis & Recommended Architecture for `agy-hq`

| Use Case | Account Topology | Recommended Primary Transport | Fallback Transport |
| :--- | :--- | :--- | :--- |
| **1. Personal `@gmail.com` DMs & Consumer Spaces (`./bin/hq gchat`)** | `@gmail.com` (Consumer) | **Managed Chrome CDP (`cdp`)** on isolated `local/private/gchat/chrome-profile` (`127.0.0.1:9222`) | **Gmail Notification Bridge (`gmail-bridge`)** (`from:chat-noreply@google.com`) for read-only triage |
| **2. Workspace DMs & Organizational Spaces (`./bin/hq gchat`)** | Google Workspace (`@company.com` / `@custom-domain`) | **Google Chat REST API v1 (`oauth`)** with split read-only sidecar token + `chat.messages.create` send token | `gws chat` CLI wrapper |
| **3. Proactive `[HQ]` Self-Chat Notification & Control Channel (`ChannelAdapter`)** | Personal `@gmail.com` + Workspace Host Space (or pure Workspace) | **Workspace External Space Bridge (`webhook` + `oauth`)** locked to `owner_space_id` | **Antigravity Remote Control (`antigravity.google` PWA)** (already active in Phase 1) |
