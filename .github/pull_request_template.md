# Description

<!-- What does this PR do? What problem does it solve? -->

## Why

<!-- Why this change? Link issue / discussion. For non-trivial changes, explain the design. -->

## Changes

<!-- Bullet-point list of notable changes. -->

-
-

## Test plan

<!-- How did you verify this? Reproduction steps if fixing a bug. -->

-
-

# Checklist (see docs/production-refactoring.md §2.3)

- [ ] `make lint` passes locally (or CI green)
- [ ] `make test-race` passes locally (or CI green)
- [ ] New public symbols have a doc comment starting with the symbol name
- [ ] Errors use `errors.Is` / `%w` wrap — no `err.Error() == "..."` comparisons
- [ ] Any new goroutine has a clear owner and exit path
- [ ] Any new I/O path takes `context.Context` as its first parameter
- [ ] New public API has metric and/or trace hooks (unless justified below)
- [ ] No `panic()` in request paths
- [ ] Breaking changes documented in CHANGELOG
- [ ] `//nolint` directives include `// reason: ...` justification

# Risk / rollback

<!-- What could go wrong? How do we roll back? -->

# References

<!-- Linked issues, design docs, RFCs. -->
