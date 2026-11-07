# ADR-0001: Adopt "Option 1.5" (Antigravity App + Thin Local Sidecar/Hooks)

- **Status:** Accepted
- **Date:** 2026-11-07
- **References:** [03 — Technical Build Options](../research/03_technical.md), [04 — Antigravity Audit](../research/04_antigravity_audit.md)

---

## Context

We need an architecture for a 24/7 proactive personal assistant supporting software engineering, investment research, and life admin, while minimizing custom framework maintenance and preserving strong security guarantees.

## Options Evaluated

| Option | Effort | Power | Security | Est. Cost | Assessment |
|---|---|---|---|---|---|
| **1. Pure Native Antigravity** | 1–2 days | Medium | High | Existing sub | Lacks silent heartbeat, event triggers (`fsnotify`), and automatic cross-session memory. |
| **1.5. Antigravity App + Thin Local Sidecar & Hooks (`agy-hq`)** | **3–4 days** | **High** | **Very High** | **Existing sub** | **Selected.** Runs 24/7 on an always-on Mac desktop; adds memory hooks, 0-token gatekeeper, and approval queue. |
| **2. Antigravity Python SDK Daemon** | 5–7 days | Very High | Very High | \$10–60/mo API + VM | Best future path if migrating to a headless Linux VM; shares 100% of `local/` Vault and `SKILL.md` files. |
| **3. Fork OpenClaw (394k★) or Nous Hermes (261k★)** | Weeks to harden | Very High | Low–Fair | \$50–250+/mo API | High attack surface (OpenClaw `CVE-2026-24763`, `ClawJacked`, ClawHub malware) and ToS incompatibility with consumer logins. |
| **4. Micro-Claw (`NanoClaw` / `nanobot`)** | 2–4 days | Med–High | Very High | API + host | Useful reference for container isolation, but lacks Antigravity's native IDE/browser/Remote Control integration. |
| **5. Cloud Durable Stack (Cloudflare / ADK / Temporal)** | 1–3 weeks | Highest | High | \$5–40/mo + API | Unnecessary overhead when an always-on Mac desktop is available. |

## Decision

Adopt **Option 1.5 (`agy-hq`)**: run the Antigravity Desktop App on an always-on Mac desktop (with *"Keep computer awake"* and *"Run in background"* enabled) and augment it with a lightweight local microkernel (`bin/hq`) via Antigravity's `hooks.json` and `sidecar.json` interfaces.

## Consequences

- **Positive:** Zero cloud hosting bills; reuses Antigravity's built-in Seatbelt sandbox, Chrome DevTools browser automation, subagents, and Remote Control mobile PWA; 100% of `local/` Vault state and `SKILL.md` files remain portable to the Antigravity Python SDK if ever needed.
- **Negative / Mitigated:** Background runs share plan quota with interactive sessions (mitigated by pinning background runs to `--model=flash` and gating them behind 0-token Go pre-checks in [RFC-0004](../design/0004-heartbeat-and-channels.md)).
