## Summary
<!-- What does this PR accomplish and why? Link any related RFC or issue. -->

## Verification Checklist
- [ ] Ran `go build -o ./bin/hq ./cmd/hq && ./bin/hq ci pre-push` locally with **0 failures**.
- [ ] Zero external Go dependencies added to `go.mod` (`Gate H4-A`).
- [ ] Zero personal paths, emails, opaque space IDs, or private Vault leaks (`Invariants P1–P6`).
- [ ] All modified `SKILL.md` files (`.agents/plugins/agy-hq/skills/` Core OS & `templates/skills/` starters) remain `<= 150` lines with `0` tracked `rsi:rule` blocks (`Gate H1`).
- [ ] Any new `docs/{design,product,decisions,research}/*.md` files are indexed in `docs/README.md` (`Gate H4-B`).
- [ ] Added or updated unit / CUJ E2E tests covering the change (and documented any new `TestCUJ*` in `docs/product/0003-critical-user-journeys.md`, `Gate H5-E`).
