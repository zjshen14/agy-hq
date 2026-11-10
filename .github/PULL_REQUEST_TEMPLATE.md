## Summary
<!-- What does this PR accomplish and why? Link any related RFC or issue. -->

## Verification Checklist
- [ ] Ran `go build -o ./bin/hq ./cmd/hq && ./bin/hq ci pre-push` locally with **0 failures**.
- [ ] Zero external Go dependencies added to `go.mod` (`Gate H4-A`).
- [ ] Zero personal paths, emails, opaque space IDs, or private Vault leaks (`Invariants P1–P6`).
- [ ] All modified `.agents/plugins/agy-hq/skills/*/SKILL.md` files remain `<= 150` lines (`Gate H1`).
- [ ] Any new `docs/{design,product,decisions,research}/*.md` files are indexed in `docs/README.md` (`Gate H4-B`).
- [ ] Added or updated unit / CUJ E2E tests covering the change.
