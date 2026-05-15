## Summary

<!-- One or two sentences on what this PR changes and why. -->

## Story / FR / NFR

<!-- e.g., Story 4.1, FR19-FR22. If this doesn't trace to the locked v1.0 spec, explain why. -->

## Test plan

- [ ] `go vet ./...` clean in both modules
- [ ] `go test -race ./...` passes in both modules
- [ ] Manual parity test run (only if `core/gateway`, `core/transport`, `core/template`, `core/config`, or `caddy/` changed — see `docs/manual-test.md`)
- [ ] Docs updated (`spec/` if behavior; `site/` if operator-facing)

## Spec impact

<!-- Did this require updating any of spec/01-project-brief.md, spec/02-prd.md, or spec/03-architecture.md? If yes, link the section(s). If no, explain why the change fits within the current spec. -->
