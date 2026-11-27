# Per-file unit tests with coverage output

## Intent
The repo has no test convention: 20 test files live in one central `test/` package, so `go test ./...` reports `coverage: [no statements]`. The user wants a unit test for each source file and a coverage figure whenever `go test` runs.

## Why (user's words)
The training focuses on engineering, not business ideas or complex logic. So the deliverable is a clean, conventional test structure and visible coverage tooling. A high coverage number is not the point. Tests should be simple and readable, and show the convention (table-driven tests, same-directory `_test.go`, mocks at interfaces).

## Goals
- Every non-trivial `.go` file has a sibling `<file>_test.go` in the same directory.
- `go test ./...` prints a coverage percentage for every package.
- `make test` also writes `coverage.out` and prints per-function and total coverage.

## Non-goals / anti-goals
- No new dependencies and no behaviour changes to product code (testability refactors only if unavoidable, and flagged).
- Integration tests (`-tags=integration`, Docker) are not changed or moved to a different mechanism.
- No coverage percentage target is enforced in CI (the user did not ask for one).

## Constraints
- Go 1.26 toolchain. `go test -cover` currently fails with `no such tool "covdata"` for packages that have no tests. It should go away once every package has a test file. If it doesn't, fix the toolchain.
- Existing tests are in `package test` and use only exported APIs. Moving them into the real packages is done as external `package x_test` files, so they keep working unchanged.

## Approaches
1. **Per-file tests next to the code (chosen by the user).** Standard Go, gives per-package coverage with plain `go test`.
2. Keep `test/` and add `-coverpkg=./...`. Quick, but not a test file per source file. Rejected.

## Design
- Move each `test/*_test.go` into the package it exercises (for example `crypto_test.go` to `crypto/aes_test.go`, `github_test.go` to `service/github_test.go`). Split large mixed files (`unit_test.go`, `orchestrator_test.go`) by source file. Test helpers shared across packages go in a small internal test-helper file per package, not a shared package.
- Add new tests for files with no coverage: `helper/*`, `model/*`, `templates`, `database/migration.go`, `service/user.go`, `handler/*`, `repository/store.go`, `main.go` and `standalone.go` where practical.
- Delete `test/` once empty, keeping `test/testdata/schema.sql` if integration tests still need it (it moves next to them).
- `Makefile`: `make test` runs `go test -cover -coverprofile=coverage.out ./...` then `go tool cover -func=coverage.out`. Add `coverage.out` to `.gitignore`. CI step gets `-cover`.
- Order of work: one package at a time, running `go test ./<pkg> -cover` after each, then the full suite.

## Left to the builder
Exact test cases, how much to mock, and whether the entry-point files (`main.go`, `standalone.go`) are worth testing beyond what is easy. Keep to simple cases (happy path plus the obvious error) rather than exhaustive branches.
