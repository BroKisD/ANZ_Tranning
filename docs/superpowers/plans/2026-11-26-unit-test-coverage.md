# Per-file Unit Tests with Coverage Output Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every source file has a sibling `_test.go`, and `go test ./...` / `make test` print coverage for every package.

**Architecture:** Move the 20 files in the central `test/` package into the packages they exercise, as external `package x_test` files (they only use exported APIs). Fakes that several packages need go into one non-test helper package `internal/testutil`. New simple tests fill the files that have none.

**Tech Stack:** Go 1.26, standard `testing`, existing deps only (testcontainers for the `integration` tag).

**Spec:** `docs/superpowers/specs/2026-11-26-unit-test-coverage-design.md`

## Global Constraints

- No new dependencies; no behaviour changes to product code.
- Integration tests keep the `//go:build integration` tag and are not run by `make test`.
- Tests stay simple: happy path plus the obvious error; table-driven where there are several inputs.
- `make test` = `go test -cover -coverprofile=coverage.out ./...` then `go tool cover -func=coverage.out`. `coverage.out` is gitignored. CI step gets `-cover`. No `cover-html` target and no coverage threshold.
- **Deviation from the spec (my call, flagged for review):** the spec said "no shared package". Handler tests (`project_handler_test`, `webhook_test`, `integration_handler_test`) use `memStore`, `memIntegrations`, `newOrc`, `waitFor` and `testKey`, which live in service/crypto tests. Test files cannot be imported across packages, so these go in `internal/testutil` rather than being copied.

## Review Focus

- Plain `go test ./...` (no flags) must pass from the repo root and from inside each package directory (relative paths like `..` and `testdata` break when files move).
- `go vet -tags=integration ./...` must still compile every integration-tagged file after the move.
- `go test -race ./...` still passes (`make test-race`).
- Tests that need `git` on PATH or `LIVE_*` env vars still skip or run exactly as before, with no new network use.
- `coverage.out` is not tracked by git.

---

### Task 1: Coverage tooling (Makefile, CI, .gitignore)

**Files:**
- Modify: `Makefile` (`test` target, `help` text), `.gitignore`, `.github/workflows/ci-cd.yml:54`

**Interfaces:** Produces: `make test` prints a per-function coverage table and `total:` line.

- [ ] **Step 1:** Change the `test` target to `$(GO) test -v -cover -coverprofile=coverage.out ./...` followed by `$(GO) tool cover -func=coverage.out`. Update the `help` line to "Run unit tests with coverage".
- [ ] **Step 2:** Add `coverage.out` to `.gitignore`. Change the CI step at line 54 to `go test -v -cover ./...`.
- [ ] **Step 3:** Run `make test`. Expected: it may still print `no such tool "covdata"` for packages without tests, which is fixed by Tasks 3 to 8. Run `go test -cover ./test/` and confirm it still passes.
- [ ] **Step 4:** Commit: `git add Makefile .gitignore .github/workflows/ci-cd.yml && git commit -m "test: print coverage from make test and CI"`

### Task 2: Extract shared fakes into `internal/testutil`

**Files:**
- Create: `internal/testutil/crypto.go`, `internal/testutil/store.go`, `internal/testutil/orchestrator.go`
- Modify: `test/*_test.go` (point at the new package)

**Interfaces:** Produces (exported, package `testutil`):
- `func TestKey(b byte) []byte` (from `testKey`, `test/crypto_test.go`)
- `type MemStore`, `func NewMemStore() *MemStore` (from `memStore`)
- `type MemIntegrations`, `func NewMemIntegrations() *MemIntegrations`, method `Put(t *testing.T, key []byte, userID, provider string, creds any)` (from `memIntegrations`, `mi.put`)
- `type OrcEnv`, `func NewOrc(t *testing.T, opts ...) *OrcEnv`, `func WaitFor(t *testing.T, what string, cond func() bool)` (from `orcEnv`, `newOrc`, `waitFor`), plus the fakes they need (`fakeHost`, `fakeGen`, `fakeSecretReader`, exported as needed)

Only move what a *second* package needs; fakes used by one package stay in that package's test files.

- [ ] **Step 1:** Create the three files by moving the definitions listed above out of `test/orchestrator_test.go` and `test/crypto_test.go`, exporting the names. Fields that tests touch directly become exported fields or methods.
- [ ] **Step 2:** Update every `test/*_test.go` reference to use `testutil.*`.
- [ ] **Step 3:** Run `go vet ./... && go test ./test/`. Expected: PASS, same test count as before (`go test -v ./test/ | grep -c '^--- PASS'`; record the number before starting).
- [ ] **Step 4:** Commit: `git commit -am "test: extract shared fakes into internal/testutil"`

### Task 3: `crypto` and `helper` packages

**Files:**
- Move: `test/crypto_test.go` to `crypto/aes_test.go` (`package crypto_test`)
- Move: `TestWebhookSecretFor` (from `test/webhook_test.go`) to `helper/webhook_test.go`
- Create: `helper/id_test.go`, `helper/validate_test.go`, `helper/middlerware_test.go`

- [ ] **Step 1:** Do the moves; use `testutil.TestKey` in `crypto/aes_test.go`.
- [ ] **Step 2:** Write `TestNewID_Format` (matches `^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`, two calls differ), `TestIsValidEmail` (table: `a@b`, `""`, `"ab"`, no `@`, 255 chars), `TestAPIKeyMiddleware` (table via `httptest`: missing key, wrong key give 401 and next not called; right key gives 200).
- [ ] **Step 3:** Run `go test -cover ./crypto ./helper`. Expected: PASS and a `coverage:` percentage for each.
- [ ] **Step 4:** Commit: `test: per-file tests for crypto and helper`

### Task 4: `model`, `templates`, `database`

**Files:**
- Create: `model/errors_test.go`, `model/models_test.go`, `templates/templates_test.go`, `database/db_connection_test.go`, `database/migration_test.go`

- [ ] **Step 1:** `TestErrors_AreDistinct` (`errors.Is` true for itself, false across `ErrNotFound`/`ErrEmailTaken`/`ErrValidation`; wrapped with `%w` still matches). `models_test.go`: cover whatever small methods/constants `model/models.go` has (read it first, test each exported function or method once; if it holds only struct declarations, assert the JSON field names of `User` and `Project` round-trip).
- [ ] **Step 2:** `TestTemplates_NotEmpty`: `templates.DeployWorkflow` and `templates.ServiceYAML` are non-empty.
- [ ] **Step 3:** `TestConnectDatabase_Unreachable`: `t.Setenv("DB_HOST","127.0.0.1")`, `t.Setenv("DB_PORT","1")`, `t.Setenv("INSTANCE_CONNECTION_NAME","")`; expect a non-nil error and `database.DB` unchanged (nil). Needs no Docker. `migration_test.go`: with `database.DB == nil` the migrate functions must not be called (they would panic); test only what is reachable without a DB. If nothing is, keep one test that the embedded migrations are present (`fs.Glob` count is at least 3, via a test in `package database`). Real migration runs stay in the integration tests (Task 8).
- [ ] **Step 4:** Run `go test -cover ./model ./templates ./database`. Expected: PASS, percentages shown.
- [ ] **Step 5:** Commit: `test: per-file tests for model, templates, database`

### Task 5: `service` package, move existing tests

**Files:** Move into `service/` as `package service_test`:

| from `test/` | to `service/` |
|---|---|
| `claude_test.go` | `claude_test.go` |
| `cloudrun_deployer_test.go` | `cloudrun_deployer_test.go` |
| `demo_generator_test.go` | `demo_generator_test.go` |
| `deploy_config_test.go` (`TestDeployConfigFromEnv*`, `TestStepRank`) | `deploy_test.go` |
| `deploy_live_test.go` | `deploy_live_test.go` |
| `deploy_monitor_test.go` | `deploy_monitor_test.go` |
| `watchdog_test.go` | `deploy_watchdog_test.go` |
| `gcp_test.go` | `gcp_test.go` |
| `github_test.go` + `iac_test.go` | `github_test.go` (iac tests exercise `RenderDeployWorkflow`/`RenderServiceYAML`, which live in `github.go`) |
| `orchestrator_test.go` | `orchestrator_test.go` |
| `static_deployer_test.go` | `static_deployer_test.go` |

**Interfaces:** Consumes: `testutil.TestKey`, `testutil.NewMemStore`, `testutil.NewOrc`, etc. from Task 2.

- [ ] **Step 1:** `git mv` each file, change `package test` to `package service_test`, drop the now-redundant `service.` import only if the file switches to in-package; otherwise keep it. Helpers shared between these files (`fakeGitHub`, `fakeDispatcher`, `envOf`, `gitCmd`, `step`, and so on) stay together because all files are in the same `service_test` package.
- [ ] **Step 2:** Run `go vet ./service && go test -cover ./service`. Expected: PASS; compile errors mean a helper was left behind in `test/`, so move it next to its first user.
- [ ] **Step 3:** Commit: `test: move service tests next to the code`

### Task 6: `service` package, fill the gaps

**Files:** Create `service/user_test.go`, `service/secretmanager_test.go`

- [ ] **Step 1:** `user_test.go` with a fake `UserRepository`: `TestUserService_Create` table (empty email, empty name, invalid email gives `model.ErrValidation`; existing email gives `model.ErrEmailTaken`; repo find error is returned as is; success lowercases and trims the email and sets a non-empty ID), `TestUserService_GetByEmail_Normalizes` (repo receives lowercased, trimmed email).
- [ ] **Step 2:** `secretmanager_test.go`: only logic reachable without GCP credentials. Read `service/secretmanager.go`; if `PutSecret`/`GetSecret` do pure parsing or building of names, table-test that. If everything calls the GCP client, write no test file for it and note in the commit message that it is only exercised through `fakeSecretReader` in the orchestrator tests.
- [ ] **Step 3:** Run `go test -cover ./service`. Expected: PASS and every non-test file in the package has a test file or a documented reason.
- [ ] **Step 4:** Commit: `test: add user service tests`

### Task 7: `handler` package

**Files:** Move into `handler/` as `package handler_test`:

| from `test/` | to `handler/` |
|---|---|
| `unit_test.go` | `user_test.go` |
| `integration_handler_test.go` | `auth_integration_test.go` |
| `project_handler_test.go` | `project_test.go` |
| `webhook_test.go` (minus `TestWebhookSecretFor`, moved in Task 3) | `webhook_test.go` |
| `db_test.go` (integration tag) | `user_integration_test.go` |

- [ ] **Step 1:** `git mv` and rename the package. Replace local copies of `memStore`/`newOrc`/`testKey`/`waitFor` with `testutil.*`.
- [ ] **Step 2:** Run `go vet -tags=integration ./handler && go test -cover ./handler`. Expected: PASS (integration file compiles but is excluded without the tag).
- [ ] **Step 3:** Commit: `test: move handler tests next to the code`

### Task 8: `repository` package and schema fixture

**Files:**
- Move: `test/repository_db_test.go` to `repository/store_integration_test.go` (keep the `integration` build tag)
- Move: `test/testdata/schema.sql` to `internal/testutil/testdata/schema.sql`; add `func SchemaPath() string` to `internal/testutil` (uses `runtime.Caller` as the old tests did) and use it from both integration tests.
- Create: `repository/store_test.go` (no tag): sqlite is not available, so test only what needs no DB. If nothing is, skip this file and rely on the integration test (record that in the commit message).

- [ ] **Step 1:** Do the moves and replace the two `filepath.Join(..., "testdata", "schema.sql")` calls with `testutil.SchemaPath()`.
- [ ] **Step 2:** Run `go vet -tags=integration ./...`. Expected: no errors. If Docker is available also run `go test -tags=integration ./repository ./handler`; otherwise say it was not run.
- [ ] **Step 3:** Commit: `test: move repository integration tests and schema fixture`

### Task 9: `main` package and cleanup

**Files:**
- Move: `test/standalone_test.go` to `standalone_test.go` (package `main`, repo root). Change `filepath.Abs("..")` to `filepath.Abs(".")`.
- Add `TestNoDatabaseConfigured` table to the same file: with `DB_USER`, `DB_HOST`, `INSTANCE_CONNECTION_NAME` all empty gives true, any one set gives false. Use `t.Setenv`; run from a temp dir so a developer `.env` is not picked up (`t.Chdir(t.TempDir())`).
- Delete: `test/` directory (should now be empty).

- [ ] **Step 1:** Do the move, add the test, `git rm -r test`.
- [ ] **Step 2:** Run `make test`. Expected: every package shows `coverage: N% of statements`, no `covdata` error, `total:` line printed. Paste the output in the commit or PR description.
- [ ] **Step 3:** Run `make test-race`, `make vet`, `make fmt-check`. Expected: all pass.
- [ ] **Step 4:** Commit: `test: finish per-file layout, remove central test package`
