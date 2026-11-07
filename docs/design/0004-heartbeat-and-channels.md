# RFC-0004: Silent Gatekeeper, Quota Circuit Breaker & Channel Adapters

- **Status:** Approved
- **Related Docs:** [ADR-0004](../decisions/0004-remote-control-first-pluggable-channels.md), [04 — Antigravity Audit §1 & §3](../research/04_antigravity_audit.md)

---

## 1. Silent Gatekeeper & Quota Protection

Because background automations share your Antigravity plan quota (which refreshes every 5 hours with a weekly cap), `hq daemon` protects your quota and attention through four rules:

1. **Zero-Token Go Pre-Checks (30m Heartbeat):**
   - Every 30 minutes, `hq daemon` executes each pack's deterministic `precheck_cmd` (e.g., checking SEC EDGAR RSS feeds, GitHub CI status, or `local/inbox/` file drops) in pure Go.
   - If a pre-check returns no new actionable items, **zero LLM sessions are spawned (`HEARTBEAT_OK`)**.
2. **Flash-Only Background Spawns:**
   - Every scheduled automation and `hq daemon` call to `agentapi new-conversation` explicitly passes `--model=flash` (or `flash_lite`), reserving Pro / Opus / Argon quota for interactive sessions.
3. **Per-Pack & Global Circuit Breakers:**
   - Enforces per-pack `daily_spawn_budget` (from `pack.yaml`) and a global `max_daily_spawns: 15` limit in `local/config.yaml`.
   - If `agentapi` fails or hits `RESOURCE_EXHAUSTED`, `hq daemon` pauses non-urgent background spawns and surfaces a status alert.
4. **Dead-Man's Switch:**
   - If the 07:30 Morning Brief has not written an output artifact/card by 08:00, `hq daemon` logs a warning alert on the `HQ Console`.

---

## 2. Pluggable `ChannelAdapter` Interface (`internal/channels/`)

```go
type ChannelAdapter interface {
    Name() string
    SendBrief(ctx context.Context, card OutboxCard) error
    SendApprovalRequest(ctx context.Context, prop PendingProposal) error
    ListenEvents(ctx context.Context, handler func(InboundEvent)) error
}
```

- **Phase 1 Default Driver (`remote_control.go`):**
  - Piggybacks on Antigravity's built-in **Remote Control (`antigravity.google` Web/PWA)** and the local **HQ Console Auxiliary Side Pane (`sidecar://hq/dashboard`)**. Zero external bot credentials or tunnels required.
- **Phase 2 Drop-In Drivers (`gchat.go`, `telegram.go`):**
  - **Google Chat:** Uses Desktop OAuth user-level scopes (`chat.spaces`, `chat.messages`) on personal `@gmail.com` or Workspace accounts (or Cloud Pub/Sub on Workspace).
  - **Telegram:** Uses outbound HTTPS long-polling with inline approval keyboards.
  - **Exfiltration Guard:** Outbound notifications always target the owner's hardcoded space/chat ID from `local/config.yaml`; the agent cannot pass an arbitrary external recipient to `hq notify`.
