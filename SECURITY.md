# Security Policy

Because `agy-hq` is a **Personal Agent OS** that connects autonomous LLM workflows to private vaults, emails, calendars, and git repositories, mechanical security boundaries are our highest engineering priority.

## Supported Versions

| Version / Branch | Supported |
| :--- | :--- |
| `main` (latest rolling release) | ✅ Supported |
| Older commits / unmerged branches | ❌ Update to latest `main` via `./bin/hq upgrade` |

## Reporting a Vulnerability

**Please do NOT report security vulnerabilities through public GitHub issues or pull requests.**

Instead, please report security issues privately through **[GitHub Private Vulnerability Reporting](https://github.com/zjshen14/agy-hq/security/advisories/new)** on this repository.

When reporting a vulnerability, please include:
1. Which mechanical security boundary is affected (see Scope below).
2. Minimal, deterministic reproduction steps (using synthetic test fixtures—never include personal credentials or real PII).
3. Impact assessment (e.g., outbound action execution without SHA-256 verification, tainted session egress bypass, or Two-Layer Vault leak).

### Response Targets
- **Initial acknowledgment:** Within **48 hours**.
- **Triage & reproduction assessment:** Within **5 business days**.
- **Coordinated fix & advisory publication:** Within **14–30 days** depending on severity.

---

## Mechanical Security Scope (In-Scope Boundaries)

Vulnerabilities in any of the following deterministic Go microkernel boundaries are **high-priority in-scope security issues**:

1. **Two-Layer Vault Isolation (`internal/vault/`, `internal/ci/pii.go`):**
   - Bypass of `.gitignore` or `hq ci check-pii` Invariants `P1`–`P6` allowing files from `local/` or `vault/` (or credentials/PII) to be committed to the Layer 1 root repository.
2. **Quarantined Reader & Universal Credential Vault Isolation (`internal/hooks/`, `internal/taint/`, `internal/authz/`, `internal/watcher/`, `internal/gmail/`, `internal/gchat/`):**
   - Bypass of the Universal Credential Vault Guard allowing *any* session (tainted or untainted) to read, write, or shell-inspect `local/private/{gmail,gchat,credentials,pii-denylist.txt}` (including via symlinked paths).
   - Bypass of `local/private/taint/` fail-closed `O_EXCL` taint lockdown (or legacy `tainted_sessions.json` 1-release fallback) after `hq gmail read`, `hq gchat read-quarantined`, or `hq watch read` (e.g., enabling `search_web`, `write_file`, `edit_file`, `invoke_subagent`, `schedule`, or `call_mcp_tool`, reading symlink-resolved `local/private/**`, or injecting newline-chained shell commands from a tainted session).
3. **SHA-256 Approval-Bound Outbound Execution, TTL & Contextual-PII Guards (`internal/approvals/`, `PreToolUse`):**
   - Bypass of `PreToolUse` hard blocks on direct external mutations (`hq gmail send`, `hq gchat send-draft`, `gh pr merge`, `npm publish`, `/api/approve`), bait-and-switch hash verification (`VerifyAndSendDraft` / mandatory `>= 8`-char `hq approve --sha256=<prefix>`), `24h` proposal expiration (`ExpiresAt`), outbound contextual-PII warning injection (`RiskFlags`), or the anti-spoofing `ask_question` canonical draft overwrite.
4. **Ring 0 Immutable Trusted Computing Base (`internal/rsi/tcb.go`):**
   - Bypass of `IsRing0Path` or `rsi.Apply` re-verification allowing recursive self-improvement (`hq rsi`) to mutate `internal/approvals/`, `internal/ci/`, `internal/events/`, `internal/plugin/`, `internal/rsi/{tcb,verifier,proposer}.go`, `internal/sandbox/`, `internal/timers/`, `internal/version/`, `internal/webui/`, `internal/vault/vault.go`, `.agents/plugins/agy-hq/{plugin.json,hooks.json,rules/,sidecars/}`, `.gemini/`, `.github/`, `.gitignore`, `.git/hooks/`, `LICENSE`, `SECURITY.md`, `local/.gitignore`, `scripts/bootstrap.sh`, or `templates/{embed.go,gitignore.tmpl,sidecars/}`.
5. **Loopback Sidecar & Headless Browser Network Jail (`internal/webui/`, `internal/gchat/cdp_ws.go`):**
   - Cross-origin request forgery (CSRF / missing `X-HQ-CSRF-Token`), DNS rebinding on `127.0.0.1:4242`, sidecar proxy token bypass, or escape from `HostResolverJailRule` / `Network.setBlockedURLs` in isolated CDP Chrome profiles.

### Out of Scope
- Prompt behavior variance inside non-quarantined user-initiated sessions that does not cross a mechanical `PreToolUse`, taint, Vault, or SHA-256 approval boundary.
- Misconfigurations where the user explicitly disables Antigravity's OS sandbox (`TURBO` mode—flagged with `WARN` by `./bin/hq doctor`).

See [Architecture Overview](docs/architecture.md) and [RFC-0005](docs/design/0005-gmail-integration.md) / [RFC-0008](docs/design/0008-gchat-integration.md) / [RFC-0011](docs/design/0011-ci-pipeline-and-regression-harness.md) / [RFC-0014](docs/design/0014-proactive-engine-and-living-context.md) / [RFC-0017b](docs/design/0017b-provenance-taint-and-offer-gateway.md) / [RFC-0026](docs/design/0026-declarative-scoped-delegates.md) for full threat models.
