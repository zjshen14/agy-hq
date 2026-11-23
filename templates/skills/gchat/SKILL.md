---
name: gchat
description: >-
  Read, search, draft, and send Google Chat messages and manage Spaces/DMs using hq gchat.
  Use whenever the user mentions Google Chat, GChat, a Chat space or group (e.g. "Project Launch Space",
  "Engineering Core"), asks to send a message on Google Chat, check unread Chat messages, or list Chat spaces.
---

# Google Chat Integration (`hq gchat`)

Use `hq gchat` for all Google Chat operations across personal (`@gmail.com`) and Google Workspace accounts.

> **Never** use `chrome-devtools` MCP tools directly on `chat.google.com` (blocked by `PreToolUse`). Always use `hq gchat`, which manages an isolated `--headless=new` Chrome profile (`local/private/gchat/chrome-profile`) and enforces cryptographic 1-tap approval.

## 0. Extension Dispatch & Overlay Check

Run `hq dispatch resolve --skill=gchat --json`; if `overridden == true`, activate the resolved companion skill (`skill_path`) and stop following this file; if `has_overlay == true` with non-empty `overlay_rules`, apply those active personal rules for this run.

## 1. Sending a Google Chat Message (Inline 1-Command Staging + Approval Flow)

When asked to send a Google Chat message (even from `[HQ] Front Desk`), **do not spawn a subagent** and **do not grep code**. Execute this 1-command staging + approval flow directly:

1. **Stage Draft + Proposal in 1 Command:**
   ```sh
   # Simple single-line message without '$'
   hq gchat propose --to="Project Launch Space" --body="Your message text here"

   # Multi-line or messages containing '$' (e.g. $382.34) — ALWAYS use --body-stdin with <<'EOF' or --body-file=<path>
   cat <<'EOF' | hq gchat propose --to="Project Launch Space" --body-stdin
   Total reimbursement is $382.34.
   Please confirm when received.
   EOF
   ```
   - **Quote-Safety Rule (Enforced by `PreToolUse`):** **Never** use double-quoted `--body="..."` when the message contains `$` (`$382.34` silently expands `$3` in bash to `82.34`). Use `--body-stdin` with `<<'EOF'` or `--body-file=<path>`, and verify the echoed `Body verified: ... '$' chars` line.
   - `--to` accepts Space/DM names (`"Project Launch Space"`, `"Engineering Core"`, `"Alice Chen"`), **emails or handles** (`"alice@gmail.com"`, `"alex_dev"` — resolved automatically via Google Chat `"New chat"` PeopleKit search and cached in `local/memory/gchat/spaces_cache.json`), or raw Space IDs (`AAAA_LAUNCH_01`, `spaces/AAAA_LAUNCH_01`).
   - `hq gchat propose` automatically starts headless Chrome CDP, fuzzy-resolves `--to`, pre-warms the space tab, creates the SHA-256 draft (`gchat-draft-*`), and stages `gchat_send` (`prop-*`).
2. **Present 1-Tap Approval via `ask_question`:**
   - Render the full message body as a markdown blockquote before or with `ask_question`, citing the recipient, Proposal ID, and `sha256` prefix.
3. **Execute on Approval:**
   ```sh
   hq approve <proposal-id> --sha256=<12-char-prefix>
   ```

## 2. Listing Spaces & Searching Messages (Safe Metadata — Non-Tainting)

Safe to run inline (including at `[HQ] Front Desk` within the 3-lookup cap); does **not** taint the conversation:
- `hq gchat spaces [--query="Launch"]` — list Spaces/DMs and refresh `spaces_cache.json`.
- `hq gchat search [--space="Project Launch Space"] [--query="is:unread"]` — search message snippets.

## 3. Reading Full Chat Transcripts (`read-quarantined` — Subagent Only)

- **CRITICAL QUARANTINE RULE:** Running `hq gchat read-quarantined --space="<Space Name or ID>"` permanently marks the calling conversation ID as a **Quarantined Reader** in `local/private/taint/` (disabling `search_web` and restricting `run_command` to quarantine-safe verbs such as `hq gchat read-quarantined`, `hq gchat triage-result`, `hq offer stage`, and `hq outbox add` — `hq gchat draft` and `hq gchat propose` are blocked in tainted sessions).
- **Never** run `read-quarantined` directly in `[HQ] Front Desk` or a general coding conversation. Dispatch a lightweight **`[quick] gchat: Quarantined Reader`** subagent (`invoke_subagent(Model="flash_lite")`) to run:
  ```sh
  hq gchat read-quarantined --space="Project Launch Space"
  ```
  and return a sanitized summary (or stage a reply via `hq offer stage`), letting an untainted caller run `hq gchat propose`.

## 4. First-Time, Expired, or Remote Sign-In (`hq gchat login`)

- In steady state, `hq gchat` reuses `local/private/gchat/chrome-profile` invisibly via `--headless=new`.
- **Desktop Guided Login:** `hq gchat login` (opens visible window if logged out; auto-fills email, caches spaces, returns to headless), `hq gchat login --reset` (backs up `Cookies` -> `Cookies.bak` to test fresh login), and `hq gchat restore-session` (restores `Cookies.bak`).
- **Remote Login from iPhone:** `hq gchat login --remote [--reset]` — runs headless, auto-fills email and `local/private/gchat/.pass` (`0600`) if present, prints any **Google Prompt 2-digit number**, and saves `local/memory/gchat/login-status.png`.
