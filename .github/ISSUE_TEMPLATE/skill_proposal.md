---
name: Progressive-Disclosure Skill Proposal
about: Propose a new .agents/plugins/agy-hq/skills/<name>/SKILL.md workflow or enhancement
title: "feat(skill): "
labels: ["enhancement", "skill"]
---

## Workflow Goal & Trigger Conditions
<!-- When should Antigravity activate this skill? What problem does it solve? -->

## Execution Mode & Model Tier
- **Execution Mode:** `Inline (<= 3 lookups)` / `Background Subagent (flash)` / `Dedicated Task Conversation (hq spawn)`
- **Default Tier:** `quick` / `standard` / `deep`

## Safety & Vault Boundaries
- **Vault Directories Read/Written (`local/...`):**
- **Untrusted Input Quarantine Required?** (`yes` / `no`)
- **Approval-Bound Outbound Actions (`./bin/hq propose`)?** (`yes` / `no`)

## Progressive-Disclosure Budget Check
- [ ] Proposed `SKILL.md` fits cleanly within the **150-line maximum** (`Gate H1`).
- [ ] Contains zero external Go module dependencies and zero personal PII (`Invariants P1–P6`).
