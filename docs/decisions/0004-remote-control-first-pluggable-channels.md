# ADR-0004: Piggyback on Antigravity Remote Control First Behind a Pluggable `ChannelAdapter`

- **Status:** Accepted
- **Date:** 2026-11-07
- **References:** [RFC-0004](../design/0004-heartbeat-and-channels.md), [PRD-0001](../product/0001-vision-and-ux.md)

---

## Context

A proactive personal assistant needs a mobile/remote touchpoint for reading Morning Briefs, delegating tasks on the go, and approving staged actions. Building external chat tunnels (Google Chat OAuth/PubSub or Telegram bots) adds setup friction and credential management if required on Day 1.

Meanwhile, Antigravity already includes **Remote Control** (`antigravity.google` Web & Mobile PWA), which mirrors local sessions on the always-on Mac desktop, delivers native PWA push notifications, and renders native approval prompts.

## Decision

1. Define an abstract `ChannelAdapter` interface (`internal/channels/adapter.go`) in `bin/hq` from Day 1.
2. Use **Antigravity Remote Control** (plus the local `HQ Console` Auxiliary Side Pane) as the **Phase 1 default channel driver**, requiring zero external chat bot setup.
3. Add **Google Chat** (supporting both personal `@gmail.com` via Desktop OAuth `chat.spaces`/`chat.messages` and Workspace Cloud Pub/Sub) and **Telegram** as opt-in **Phase 2 adapters** implementing `ChannelAdapter`.

## Consequences

- **Positive:** Zero external tunnel or bot setup needed to use `agy-hq` from a phone on Day 1; full support for `hq` hooks, slash commands, and artifacts over Remote Control; clean extension point for Google Chat and Telegram later.
- **Negative:** Remote Control requires opening the PWA/web view rather than living inside an existing messaging app until a Phase 2 chat adapter is enabled.
