---
name: gchat
description: >-
  Read, search, draft, and send Google Chat messages and manage Spaces/DMs using hq gchat.
  Use whenever the user mentions Google Chat, GChat, a Chat space or group (e.g. "Project Launch Space",
  "Engineering Core"), asks to send a message on Google Chat, check unread Chat messages, or list Chat spaces.
---

# Google Chat Integration (`hq gchat`)

Use `hq gchat` for all Google Chat operations across personal (`@gmail.com`) and Google Workspace accounts.

> **Never** use `chrome-devtools` MCP tools directly on `chat.google.com` (blocked by `PreToolUse`). Always use `hq gchat`, which manages an isolated, network-jailed `--headless=new` Chrome profile (`local/private/gchat/chrome-profile`) and enforces cryptographic 1-tap approval.

## 1. Sending a Google Chat Message (Inline 1-Step Workflow)

When the user asks to send a message to a Google Chat space or DM (even from `[HQ] Front Desk`), **do not spawn a subagent** and **do not grep code**. Execute this 2-turn flow directly:

1. **Stage Draft + Proposal in 1 Command (Supports Natural Space Names or IDs):**
   ```sh
   hq gchat propose --to="Project Launch Space" --body="Your message text here"
   ```
   - `--to` accepts:
     - A human-friendly Space/DM name (e.g., `"Project Launch Space"`, `"Engineering Core"`, `"Alice Chen"`),
     - Any **email address or username handle** (`"alice@gmail.com"`, `"alex_dev"`)—even if they have no existing DM in the top 20 sidebar spaces (`hq gchat propose` automatically runs Google Chat's `"New chat"` PeopleKit search in Go in ~2 seconds and caches the resulting DM `spaceId`), or
     - A raw Space ID (`AAAA_LAUNCH_01` or `spaces/AAAA_LAUNCH_01`).
   - **Never spawn a subagent** to manually search for or start a new DM in Chrome—`hq gchat propose --to="<name, email, or handle>" --body="..."` handles both existing spaces and brand-new DMs natively in 1 CLI call.
   - `hq gchat propose` automatically:
     1. Ensures the invisible `--headless=new` Chrome CDP session is running using saved cookies in `local/private/gchat/chrome-profile` (zero desktop popup windows),
     2. Fuzzy-resolves `--to` against cached/live spaces (`local/memory/gchat/spaces_cache.json`),
     3. Pre-warms the target space tab in headless Chrome so the composer is ready, and
     4. Creates the SHA-256 draft (`gchat-draft-*`) and stages a `gchat_send` proposal (`prop-*`).
2. **Present 1-Tap Approval via `ask_question` with In-Prompt Payload Preview:**
   - Always render the full message content as a clear markdown blockquote directly in the conversation turn BEFORE or WITH the approval prompt.
   - In `ask_question`, explicitly include the recipient and message preview in the question title or option text (e.g., `Approve sending Google Chat to <Recipient>: "..."?`) alongside the Proposal ID and `sha256` prefix (`PreToolUse` also injects the ground-truth recipient space and message body preview).
3. **Execute on Approval:**
   - Once the user selects Approve, run:
     ```sh
     hq approve <proposal-id> --sha256=<12-char-prefix>
     ```

## 2. Listing Spaces & Searching Messages (Safe Metadata — Non-Tainting)

These read-only metadata commands are safe to run inline (including at `[HQ] Front Desk` within the 3-lookup cap) and do **not** taint the conversation:

- **List Spaces & DMs (and refresh local name cache):**
  ```sh
  hq gchat spaces [--query="Launch"]
  ```
- **Search Message Snippets:**
  ```sh
  hq gchat search [--space="Project Launch Space"] [--query="is:unread"]
  ```

## 3. Reading Full Chat Transcripts (`read-quarantined` — Subagent Only)

- **CRITICAL QUARANTINE RULE:** Running `hq gchat read-quarantined --space="<Space Name or ID>"` permanently marks the calling conversation ID as a **Quarantined Reader** in `local/memory/tainted_sessions.json` (disabling `search_web` and restricting `run_command` to read/draft/propose subcommands).
- **Never** run `read-quarantined` directly in `[HQ] Front Desk` or a general coding conversation.
- Instead, dispatch a lightweight **`[quick] gchat: Quarantined Reader`** subagent (`invoke_subagent(Model="flash_lite")`) to run:
  ```sh
  hq gchat read-quarantined --space="Project Launch Space"
  ```
  and return the sanitized summary (or stage a reply via `hq gchat propose`).

## 4. First-Time, Expired, or Remote Sign-In (`hq gchat login`)

- In steady state, `hq gchat` reuses the isolated cookies in `local/private/gchat/chrome-profile` and launches Chrome invisibly with `--headless=new` (no window pops up).
- **Desktop Guided Login (or Testing Fresh Login):**
  ```sh
  hq gchat login               # Opens visible sign-in window if logged out
  hq gchat login --reset       # Backs up Cookies -> Cookies.bak and forces a fresh login test
  hq gchat restore-session     # Restores Cookies.bak in 1 second if you cancel a login test
  ```
  `hq gchat login` automatically fills your email address, waits for you to complete Google Sign-In/Passkey, caches your spaces list, closes the visible window, and restarts the browser in invisible `--headless=new` mode.
- **Remote Login from iPhone (`--remote`):**
  ```sh
  hq gchat login --remote [--reset]
  ```
  Runs in headless mode, auto-fills the Google email, auto-submits `local/private/gchat/.pass` (`0600`) if present, prints any **Google Prompt 2-digit number** (`📱 GOOGLE PROMPT ON PHONE: Tap "Yes" and select number [XX]`), and saves a live PNG screenshot to `local/memory/gchat/login-status.png` so the user can approve the prompt directly on their iPhone.
