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
2. **Quarantined Reader & Session Taint Isolation (`internal/hooks/`, `internal/gmail/`, `internal/gchat/`):**
   - Bypass of `tainted_sessions.json` lockdown after `hq gmail read` or `hq gchat read-quarantined` (e.g., enabling `search_web`, reading `local/private/**`, or staging cross-thread/cross-space outbound drafts from a tainted session).
3. **SHA-256 Approval-Bound Outbound Execution (`internal/approvals/`, `PreToolUse`):**
   - Bypass of `PreToolUse` hard blocks on direct external mutations (`hq gmail send`, `hq gchat send-draft`, `gh pr merge`, `npm publish`), bait-and-switch hash verification (`VerifyAndSendDraft` / `hq approve --sha256`), or the anti-spoofing `ask_question` metadata overwrite.
4. **Ring 0 Immutable Trusted Computing Base (`internal/rsi/tcb.go`):**
   - Bypass of `IsRing0Path` allowing recursive self-improvement (`hq rsi`) to mutate `internal/approvals/`, `internal/ci/`, `LICENSE`, `SECURITY.md`, `.github/`, or `.git/hooks/`.
5. **Loopback Sidecar & Headless Browser Network Jail (`internal/webui/`, `internal/gchat/cdp_ws.go`):**
   - Cross-origin request forgery (CSRF), DNS rebinding on `127.0.0.1:4242`, or escape from `HostResolverJailRule` / `Network.setBlockedURLs` in isolated CDP Chrome profiles.

### Out of Scope
- Prompt behavior variance inside non-quarantined user-initiated sessions that does not cross a mechanical `PreToolUse`, taint, Vault, or SHA-256 approval boundary.
- Misconfigurations where the user explicitly disables Antigravity's OS sandbox (`TURBO` mode—flagged with `WARN` by `./bin/hq doctor`).

See [Architecture Overview](docs/architecture.md) and [RFC-0005](docs/design/0005-gmail-integration.md) / [RFC-0008](docs/design/0008-gchat-integration.md) / [RFC-0011](docs/design/0011-ci-pipeline-and-regression-harness.md) for full threat models.
