# AI App Builder: Build Report

Status of the project built from `ai_app_builder_demo_build_guide.md` (Steps 2–6), on top of the existing Go / Cloud Run / MySQL boilerplate described in `REPORT.md`.

> **One-line summary:** a Go backend plus a small web UI where a user describes an app, Claude Code generates and self-tests it, a GitHub PR is opened for human approval, and on approval it is merged and deployed to the user's own Cloud Run through GitHub Actions. All the code and its tests are written and passing locally against fakes. **The real GitHub, Claude and GCP integrations have never been run end to end, and several security gaps (section 5, P0) must be closed before real users touch it.**

---

## 1. What has been built

| Guide step | Delivered | Where |
|---|---|---|
| **2. Database** | 3 Goose migrations: `users`, `user_integrations`, `projects` (UUID ids, FKs, status/step columns) | `database/migrations/` |
| **3. Project structure** | Layered packages next to the existing ones (flat layout kept instead of `internal/`) | see section 2.2 |
| **4.1 Onboarding and crypto** | AES-256-GCM `Encrypt`/`Decrypt`; GitHub installation callback; `POST /api/integrations` (GCP WIF config, optional BYOK Anthropic key to Secret Manager, only a reference stored); `GET /api/integrations` returns only `{provider, configured}` | `crypto/`, `handler/auth_integration.go`, `service/secretmanager.go` |
| **4.2 GitHub service** | App JWT (RS256) → 1-hour installation token; create private repo and commit the rendered deploy workflow; push `feature/ai-gen` and open PR; squash-merge PR; look up installation owner | `service/github.go` |
| **4.3 Claude self-correction loop** | Isolated per-project workspace; `claude -p … --dangerously-skip-permissions` with timeouts; up to 3 lint/test runs, feeding failures back; Dockerfile check; Go and Node stacks | `service/claude.go` |
| **4.4 Orchestrator** | Per-project goroutine state machine with a human-approval gate (HITL), approval timeout, bounded generation concurrency, compare-and-set start, graceful shutdown | `service/orchestrator.go` |
| **4.5 API and webhook** | `POST /api/projects` (202), `GET /api/projects/{id}`, `POST /api/projects/{id}/approve`, `POST /api/webhooks/github` | `handler/project.go`, `handler/webhook.go` |
| **5. CI/CD workflow** | Cloud Run deploy workflow template (WIF auth, build, push, deploy, webhook callbacks) | `templates/deploy.yml.tmpl` |
| **6. Web UI** | Dashboard: sign-in, connect GitHub, GCP form, create app, live pipeline tracker with approve button | `web/` |

Also kept from the boilerplate: user API (`/users`), API-key middleware, GORM/MySQL connection (Cloud SQL socket or TCP), Goose migrations embedded in the binary, Dockerfile, GitHub Actions CI/CD, Cloud Run `service.yml`.

**Size and tests:** about 2.8k lines of source, 66 Go test functions plus 2 Node test files. All pass under `go test ./... -race`, except 3 MySQL tests behind the `integration` build tag, which need Docker and were skipped in the build environment.

---

## 2. Architecture

### 2.1 System overview

```text
                         ┌───────────────────────────── Platform (Go, Cloud Run / local) ─────────────────────────────┐
 Browser                 │                                                                                           │
 (web/ dashboard) ──────►│  Handlers ──► Orchestrator (goroutine per project) ──► Claude runner (claude -p, go test)   │
  x-api-key, X-User-ID   │     │              │   │                                  workspace /tmp/workspaces/<id>   │
                         │     │              │   └────────► GitHub service ───────► GitHub REST API + git push        │
                         │     ▼              ▼                                       (installation token, in RAM)     │
                         │  Repository (GORM) ◄── AES-256-GCM ──► MySQL / Cloud SQL                                   │
                         │                         │                                                                  │
                         │                         └── BYOK key ref ──► GCP Secret Manager                            │
                         └───────────────▲───────────────────────────────────────────────────────────────────────────┘
                                         │ POST /api/webhooks/github  (per-project secret)
                                         │
 User's GitHub repo ── push to main ──► GitHub Actions ──(OIDC, no keys)──► User's GCP: Artifact Registry + Cloud Run
```

Credential model: no long-lived raw tokens in the database. GitHub uses an App `installation_id` (1-hour tokens minted in memory per job). GCP uses Workload Identity Federation (no JSON keys). The optional Anthropic key lives in Secret Manager. Everything else in `user_integrations` is AES-256-GCM encrypted with `ENCRYPTION_MASTER_KEY`.

### 2.2 Code layout

```text
main.go                    wiring, routes, graceful shutdown
crypto/aes.go              AES-256-GCM, master-key loading (hex or base64)
model/                     entities (User, UserIntegration, Project), creds payloads, status/step enums, errors
repository/store.go        MySQL CRUD via GORM; compare-and-set update for projects
service/
  user.go                  user create / lookup
  github.go                GitHub App client, repo / workflow / PR / merge, workflow rendering
  claude.go                workspace, claude runner, test loop, sandbox env
  orchestrator.go          state machine + approval channels
  secretmanager.go         Secret Manager read / write
handler/
  user.go                  /users
  auth_integration.go      GitHub callback, /api/integrations
  project.go               /api/projects*
  webhook.go               /api/webhooks/github
  config.go                /api/config (public: GitHub install URL)
helper/                    API-key middleware, UUID, per-project webhook secret, validation
templates/deploy.yml.tmpl  workflow rendered into each generated repo (embedded)
web/                       index.html, app.js, logic.js, style.css (embedded, served with strict CSP)
database/                  connection + embedded Goose migrations
test/                      Go tests (+ test/web/*.js run by Node)
```

### 2.3 Project state machine

```text
 pending ──(CAS)──► generating ──────────────────────────────► waiting_for_approval ──approve──► deploying ──webhook──► success
                    initializing → repo_created → tests_passed        (PR open, gate)            merging → deploying    or failed
      any error / timeout / shutdown before the webhook ───────────────────────────────────────────────────────────► failed
```

- `pending → generating` is a compare-and-set on the DB row, so only one run can start per project.
- The approval channel is registered **before** the status flips to `waiting_for_approval`, so an approve call can never miss it.
- The webhook moves only a `deploying` project; repeats are idempotent, conflicts return 409.

### 2.4 End-to-end flow

1. The user signs in (API key + email), connects GitHub (installs the App) and saves GCP WIF settings. These are stored encrypted.
2. `POST /api/projects` saves a `pending` project and starts the orchestrator (202).
3. The orchestrator decrypts the integrations, reads the BYOK key if there is one, and creates a private repo with the deploy workflow on `main`.
4. Claude generates the app in an isolated workspace. `go vet`/`go test` (or `npm test`) must pass. Failures are fed back for up to 3 runs.
5. The code is committed to `feature/ai-gen` and a PR is opened. The status becomes `waiting_for_approval`.
6. The user reviews the PR and clicks **Approve & Deploy**. The PR is squash-merged.
7. The push to `main` triggers GitHub Actions: WIF login, `docker build/push`, `gcloud run deploy`, then a webhook call with the live URL (or a failure).
8. The webhook sets `success` + `live_url`, and the dashboard shows the link.

### 2.5 API surface

| Endpoint | Auth | Purpose |
|---|---|---|
| `POST /users`, `GET /users?email=` | API key | create / fetch user |
| `GET /api/config` | none | public config (GitHub install URL) |
| `GET /api/integrations/github/callback?installation_id=` | API key + `X-User-ID` | save GitHub installation |
| `POST /api/integrations`, `GET /api/integrations` | API key + `X-User-ID` | save GCP config (+BYOK) / list configured providers |
| `POST /api/projects` → 202 | API key + `X-User-ID` | create and start a build |
| `GET /api/projects/{id}` | API key + `X-User-ID` (owner) | status and URLs |
| `POST /api/projects/{id}/approve` | API key + `X-User-ID` (owner) | release the approval gate |
| `POST /api/webhooks/github` | per-project secret header | deploy result from Actions |
| `GET /` and static files | none | dashboard |

### 2.6 Configuration

| Variable | Needed for |
|---|---|
| `DB_*`, `INSTANCE_CONNECTION_NAME`, `API_KEY` | boilerplate (required) |
| `ENCRYPTION_MASTER_KEY` | **required**: 32 bytes, hex or base64 |
| `GITHUB_APP_ID`, `GITHUB_APP_PRIVATE_KEY`, `GITHUB_APP_SLUG` | enables projects, webhook and install button |
| `WEBHOOK_URL`, `WEBHOOK_SECRET` | required with the GitHub App; secret is a root from which per-project secrets are derived |
| `ANTHROPIC_API_KEY` | platform key for Claude (users may bring their own) |
| `SECRET_MANAGER_PROJECT` | enables BYOK |

### 2.7 Security controls in place

- AES-256-GCM with a random nonce per encryption; wrong key or tampered data fails; GET never returns credentials.
- **Per-project webhook secret** (`HMAC(root, project_id)`): the secret visible in one repo's workflow cannot forge another project's result. Compared in constant time on hashes; authentication happens before any DB lookup (no existence oracle); `live_url` must be `https`.
- Workflow values are validated against a strict character set before rendering, so user input cannot break out of YAML or shell quoting.
- Repo operations are checked against the connected account's owner; the PR must belong to the repo.
- **Generated code cannot alter CI:** `.github/` is discarded and restored from `main` before the PR commit.
- Git token is passed via environment, never argv or `.git/config`, and is redacted from errors. API keys are redacted from check output.
- Claude and the test commands run with a scrubbed environment (no server secrets) in a per-project directory and `HOME`, in their own process group, with hard timeouts, an output cap and `GOTOOLCHAIN=local`.
- Mandatory human gate: nothing merges without an explicit approve by the project's owner.
- The UI renders API data only as text, links only for absolute `https` URLs, and is served under a CSP with no inline script or style.

### 2.8 Deliberate deviations from the guide

| Guide | Built | Reason |
|---|---|---|
| `internal/` + `cmd/server/` layout | flat packages, existing `main.go` | you asked to keep the current structure |
| One global webhook secret | per-project derived secret | the global one would be readable from every user's repo |
| `{{ }}` workflow template | `[[ ]]` delimiters | Go templates clash with GitHub Actions `${{ }}` |
| `PushAndOpenPR(workspace, creds, base)` | adds `repoURL` | the function needs to know which repo |
| `web/index.html` only | plus `app.js`, `logic.js`, `style.css` | CSP without `'unsafe-inline'`; testable logic |
| (not in guide) | `paths-ignore: .github/**`, pinned `--project`, webhook retries | avoid a false failed deploy from the first commit; robustness |

---

## 3. How it is tested

| Area | Coverage |
|---|---|
| Crypto | round trip, nonce uniqueness, wrong key, tampering, key parsing |
| Integrations handlers | validation, encryption at rest, upsert, BYOK reference only, no credential leaks |
| GitHub service | fake GitHub server; JWT verified against the public key; org / personal flow; workflow commit; PR open / reuse; merge guards; **real local git** for push, including CI-hijack protection |
| Claude runner | fake `claude` script + real `go vet/test`: success, fix loop, give-up, missing Dockerfile, env scrubbing, BYOK redaction, timeout kills process group, cancel |
| Orchestrator | state transitions, approval guards, every stage failure, timeout, cancel, BYOK, no double run, concurrency limit, panic safety (run with `-race`) |
| Project / webhook handlers | validation, ownership, status mapping, auth, idempotency, state guard, URL safety, full end-to-end through the real orchestrator |
| Workflow | rendered output parsed as real YAML and checked structurally |
| Web | served files and headers, CSP compatibility, element ids match the script; Node tests for stepper logic and the real `app.js` against a fake DOM (mutation-checked) |
| MySQL (`integration` tag) | upsert, FK, defaults, compare-and-set under concurrency (**not run in the build sandbox**) |

---

## 4. What has NOT been verified

- **No real end-to-end run.** GitHub App, repo creation, `git push`, PR merge, the Actions workflow, WIF login, Cloud Run deploy and the real `claude` CLI have only been exercised against fakes. Expect integration surprises (permissions, timing, rate limits).
- The three MySQL integration tests have not been run here (they need Docker).
- The UI was never opened in a real browser; it is tested with a fake DOM only.
- CI (`.github/workflows/ci-cd.yml`) and `service.yml` were not updated for the new env vars, Node tests, or Docker-dependent tests.

---

## 5. What needs to be fixed or improved

### P0: must fix before real users

1. **Identity and authorization are not real.**
   - Identity is an `X-User-ID` header plus one shared API key typed into the browser. Anyone holding the API key can act as any user and read or approve their projects.
   - `POST /users` hands out user ids.
   - **Fix:** real login (sessions or OIDC), short-lived tokens, user id derived server-side; per-user API access.
2. **GitHub installation hijack.**
   - `GET …/github/callback?installation_id=N` trusts the id from the query string. User A can submit the installation id of organization B and then create repos and PRs in B's org through the platform's App.
   - **Fix:** enable "Request user authorization (OAuth) during installation", exchange the `code`, and verify the user can access that installation (`/user/installations`); add a `state` parameter against CSRF.
3. **The sandbox is not a real sandbox.**
   - Claude (with `--dangerously-skip-permissions`) and generated tests run as the **same OS user and in the same container** as the server. A scrubbed environment does not stop them reading `/proc/<server-pid>/environ`, the server's files, the DB socket, the metadata server (`169.254.169.254`), or making arbitrary network calls. This can expose `ENCRYPTION_MASTER_KEY`, DB credentials and the GitHub App key.
   - There are no CPU or memory limits.
   - **Fix:** run each job in a separate, locked-down container or microVM (for example a Cloud Run Job or gVisor) with no server secrets, a separate user, a read-only root, resource limits and egress restrictions.
4. **Migration history was rewritten.** The two original migrations were deleted and `users` changed from `BIGINT` ids to `VARCHAR(36)`. Any database already migrated (including the Cloud SQL one from the boilerplate) has the old goose versions recorded and an incompatible `users` table, so `serve` will fail or mis-migrate. **Fix:** write a conversion migration, or reset non-production databases explicitly.

### P1: needed for a reliable service

5. **State lives in memory.** Running jobs and approval channels are lost on restart, so projects stay stuck in `generating` or `waiting_for_approval`, and an approve then returns "run no longer active". With more than one Cloud Run instance, the approve request may reach an instance that does not own the run. **Fix:** persist job state and make runs resumable (worker plus queue, e.g. Cloud Tasks or a DB lease), and resume the approval wait from the DB.
6. **Cloud Run CPU throttling.** Background goroutines get almost no CPU between requests. Needs `--no-cpu-throttling` and a pinned instance count, or move builds to a worker or Job (also fixes point 5).
7. **No deploy timeout.** A `deploying` project whose workflow never calls back stays there forever. Add a watchdog that polls GitHub Actions or fails after N minutes.
8. **No quotas or rate limits.** Each project runs Claude (cost) and creates a repo. Add per-user concurrency and daily limits, request rate limiting, and a spend cap.
9. **Runtime image cannot run builds.** The distroless image has no `claude`, `go`, `git` or `node`. Build a worker image that includes them.
10. **Personal GitHub accounts do not work** (installation tokens cannot create repos for a user account). Document an org requirement or add a user-token flow.
11. **No cleanup or lifecycle.** Failed or abandoned projects leave GitHub repos, open PRs, Cloud Run services and Secret Manager secrets behind. There is no delete or disconnect for integrations or projects, and no key rotation (the master key has no versioning).
12. **Real end-to-end validation** against a test GitHub org and GCP project; add a CI job for the Docker-based MySQL tests; update `ci-cd.yml`, `service.yml` and `docker-compose.yml` with the new env vars and secrets.

### P2: quality and product

13. Features: no project list or history, no live generation logs, no retry or cancel, no way to ask for changes after the first generation, and `password_hash` is unused.
14. Webhook secret is stored in plaintext in each repo's workflow file (derived per project, so limited blast radius). A GitHub repo secret would be cleaner.
15. Observability: unstructured logs, no request ids or metrics, no audit trail of who approved what.
16. GitHub client: installation tokens are not cached, no rate-limit or retry handling, force-push on `feature/ai-gen`, repo names derived from the project name need a collision strategy.
17. Migrations run on every instance at startup. Move to a one-off release step (already noted in `REPORT.md`).
18. Housekeeping: `REPORT.md` only describes the original user API; `helper/transaction_control.go` is unused; no top-level README or `.env.example` for the new variables; an `anz-training` binary sits in the repo directory (ignored by git).

---

## 6. Suggested next steps (in order)

1. Replace the identity model (item 1) and fix the installation hijack (item 2).
2. Move generation and tests into an isolated worker (items 3, 5, 6, 9) with a persisted job state.
3. Decide how to handle the migration history (item 4).
4. Run one full real end-to-end test against a sandbox GitHub org and GCP project, and fix what it shows (item 12).
5. Add quotas, a deploy watchdog and cleanup (items 7, 8, 11).
6. Then the product work (item 13) and the polish items.
