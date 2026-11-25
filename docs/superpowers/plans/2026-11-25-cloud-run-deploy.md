# Real Cloud Run Deployment Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** After a PR is merged, GitHub Actions in the generated repo mock-builds/tests, really builds, pushes and deploys to Cloud Run in the platform's own GCP project, and the platform shows live status; GCP trust is provisioned automatically per repo.

**Architecture:** A `Deployer` interface (first impl `CloudRunDeployer`) prepares GCP (REST APIs through Application Default Credentials) before the repo is created; `CreateRepo` commits `service.yml` + a workflow that posts stage events to the existing webhook; the webhook gains forward-only progress events; a watchdog fails silent deploys; a manual endpoint re-triggers the workflow.

**Tech Stack:** Go 1.26, `net/http`, `golang.org/x/oauth2/google` (ADC), GitHub REST (PAT), GitHub Actions, Cloud Run, Workload Identity Federation, GORM/MySQL, `httptest` fakes.

**Spec:** `docs/superpowers/specs/2026-11-25-cloud-run-deploy-design.md`

## Global Constraints

- No commits unless the user asks. Each task ends with a verification step, not a commit.
- Module name `anz-training`; tests live in package `test` (`/test`), run with `go test ./test`. Existing fakes in `test/` (`memStore`, `memIntegrations`, `fakeHost`, `fakeGen`, `newOrc`, `newFakeGitHub`, `newGH`, `gitCmd`, `write`, `seedRemote`, `ghCreds`) are reused, not duplicated.
- No service-account JSON keys anywhere; login is Workload Identity only.
- Webhook notifications from the workflow are best-effort (`|| true`); they never fail the deploy.
- Defaults: `GCP_REGION=asia-southeast1`, `GCP_ARTIFACT_REPO=ai-apps`, `DEPLOY_TIMEOUT=15m`; pool id `ai-builder-pool`, provider id `github`, deployer service account id `ai-deployer`.
- Mode precedence: repo-only > deploy (`DEMO_TEMPLATE_REPO` + `GCP_PROJECT_ID`) > demo (`DEMO_TEMPLATE_REPO`). The per-user GCP integration path stays untouched when neither demo nor deploy mode is on.
- Step order for progress events: `merging < building < testing < deploying`. Status stays `deploying` throughout.
- Generated repos commit infrastructure files with `[skip ci]` in the commit message.
- `go vet ./...` and `gofmt -l .` must be clean after every task.

## Review Focus

- A progress webhook with an unknown step, a project not in `deploying`, or a step that goes backwards: acknowledged, project unchanged (Task 5).
- `Prepare` fails (GCP error): project ends `failed`, no repo is created (Task 6).
- Calling `EnsureBase`/`BindRepo` twice, or a lost IAM etag race: no duplicate bindings, retried once (Task 2).
- A service name or URL with characters that could break out of the workflow YAML/shell: rejected before anything is committed (Task 4).
- Re-deploy of another user's project, of a project mid-deploy, or of one that never merged: 404 / 409 / 409 (Task 8).

---

### Task 1: Deploy types, step constants, config parsing

**Files:**
- Modify: `model/models.go` (constants)
- Create: `service/deploy.go`
- Test: `test/deploy_config_test.go`

**Interfaces:**
- Produces: `model.StepBuilding = "building"`, `model.StepTesting = "testing"`; in `service/deploy.go`:
  - `type DeployTarget struct{ ProjectID, ServiceName, RepoOwner, RepoName string }`
  - `type Deployer interface { Prepare(ctx context.Context, t DeployTarget) (model.GCPCreds, error); Trigger(ctx context.Context, gh model.GitHubCreds, repoURL string) error }`
  - `func StepRank(step string) int` (`merging`=1, `building`=2, `testing`=3, `deploying`=4, anything else 0)
  - `type DeployConfig struct{ ProjectID, Region, ArtifactRepo string; AllowedOwners []string; Timeout time.Duration }` and `func DeployConfigFromEnv(get func(string) string) (DeployConfig, error)` reading `GCP_PROJECT_ID`, `GCP_REGION`, `GCP_ARTIFACT_REPO`, `GCP_ALLOWED_GITHUB_OWNERS` (comma list, trimmed, required non-empty), `DEPLOY_TIMEOUT`.

- [ ] **Step 1: Write failing tests** in `test/deploy_config_test.go`: `TestDeployConfigFromEnv_Defaults` (only project + owners set → region `asia-southeast1`, repo `ai-apps`, timeout 15m, owners split `"a, b"` → `["a","b"]`); `TestDeployConfigFromEnv_Errors` (missing `GCP_PROJECT_ID`, missing owners, bad `DEPLOY_TIMEOUT=abc` each return an error); `TestStepRank_Order` (`StepRank("merging") < StepRank("building") < StepRank("testing") < StepRank("deploying")`, unknown = 0).
- [ ] **Step 2: Run** `go test ./test -run 'DeployConfig|StepRank'` → FAIL (undefined).
- [ ] **Step 3: Implement** the types/functions above in `service/deploy.go`; add the two constants to the `Project.CurrentStep` block in `model/models.go`.
- [ ] **Step 4: Run** the same command → PASS; `go vet ./...` clean.

### Task 2: GCP provisioner

**Files:**
- Create: `service/gcp.go`
- Test: `test/gcp_test.go`

**Interfaces:**
- Consumes: `DeployConfig` (Task 1).
- Produces:
  - `type GCP struct{…}`; `func NewGCP(ctx context.Context, cfg DeployConfig, hc *http.Client) (*GCP, error)` (nil `hc` → `google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform")`).
  - `func (g *GCP) EnsureBase(ctx context.Context) error` — idempotent, remembers success in memory.
  - `func (g *GCP) BindRepo(ctx context.Context, owner, repo string) (model.GCPCreds, error)` — returns `model.GCPCreds{ProjectID, Region, ArtifactRepo, WIFProvider: "projects/<number>/locations/global/workloadIdentityPools/ai-builder-pool/providers/github", ServiceAccount: "ai-deployer@<project>.iam.gserviceaccount.com"}`.

- [ ] **Step 1: Write failing tests** in `test/gcp_test.go` using an `httptest` server that implements the Google endpoints used and a `http.Client` whose transport rewrites every request to that server (keeping original host in a header). The fake keeps state (existing resources, IAM policies with etag, LRO that completes after one poll). Tests:
  - `TestGCP_EnsureBase_CreatesEverything`: on an empty fake, after `EnsureBase` the fake holds: the 6 APIs enabled, the docker Artifact Registry repo, service account `ai-deployer`, pool `ai-builder-pool`, provider `github` (issuer `https://token.actions.githubusercontent.com`, condition mentioning each allowed owner), and project roles `roles/run.admin` and `roles/artifactregistry.writer` for the deployer plus `roles/iam.serviceAccountUser` on the Cloud Run runtime service account (`<number>-compute@developer.gserviceaccount.com`).
  - `TestGCP_EnsureBase_Idempotent`: a second call (new `GCP` instance, same fake) makes no create/set calls (count them) and returns nil.
  - `TestGCP_BindRepo_AddsPerRepoMember`: after `BindRepo(ctx,"acme","my-app-1a2b")` the deployer SA policy has role `roles/iam.workloadIdentityUser` with member `principalSet://iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/ai-builder-pool/attribute.repository/acme/my-app-1a2b`; the returned creds equal the values above; calling it twice leaves exactly one such member.
  - `TestGCP_IAMEtagConflictRetried`: the fake rejects the first `setIamPolicy` with 409; the call still succeeds and the binding exists.
  - `TestGCP_ErrorSurfaced`: fake returns 403 for pool creation → `EnsureBase` error contains `403`.
- [ ] **Step 2: Run** `go test ./test -run TestGCP_` → FAIL.
- [ ] **Step 3: Implement** `service/gcp.go`. Endpoints: Service Usage `services:batchEnable`; Artifact Registry `repositories?repositoryId=`; IAM `serviceAccounts`, `workloadIdentityPools?workloadIdentityPoolId=`, `.../providers?workloadIdentityPoolProviderId=`; Resource Manager `projects/<id>` (project number) and `projects/<id>:getIamPolicy|setIamPolicy`; SA `:getIamPolicy|setIamPolicy`. Treat HTTP 409 on create as "already exists". Poll long-running operations (`done` flag) every 2s up to 2 minutes (configurable field for tests). IAM changes: get policy, add binding if absent, set with etag, on 409 re-read and retry once.
- [ ] **Step 4: Run** `go test ./test -run TestGCP_ -race` → PASS.

### Task 3: CloudRunDeployer and workflow dispatch

**Files:**
- Create: `service/cloudrun_deployer.go`
- Modify: `service/github.go` (add `DispatchWorkflow`)
- Test: `test/cloudrun_deployer_test.go`, extend `test/github_test.go` fake GitHub with `POST /repos/{o}/{r}/actions/workflows/deploy.yml/dispatches` → 204

**Interfaces:**
- Consumes: `GCP.EnsureBase`, `GCP.BindRepo`; `Deployer` (Task 1); `tokenFor` in `service/github.go`.
- Produces:
  - `func (g *GitHub) DispatchWorkflow(ctx context.Context, gh model.GitHubCreds, repoURL, workflowFile, ref string) error` (uses `repoFor`, body `{"ref": ref}`).
  - `type WorkflowDispatcher interface{ DispatchWorkflow(ctx context.Context, gh model.GitHubCreds, repoURL, workflowFile, ref string) error }`
  - `type GCPBinder interface{ EnsureBase(ctx context.Context) error; BindRepo(ctx context.Context, owner, repo string) (model.GCPCreds, error) }`
  - `func NewCloudRunDeployer(g GCPBinder, d WorkflowDispatcher) *CloudRunDeployer` implementing `Deployer`: `Prepare` = `EnsureBase` then `BindRepo(t.RepoOwner, t.RepoName)`; `Trigger` = `DispatchWorkflow(..., "deploy.yml", "main")`.

- [ ] **Step 1: Write failing tests**: `TestCloudRunDeployer_PrepareOrder` (fake binder records `ensure` before `bind`, args owner/repo passed, creds returned unchanged; an `EnsureBase` error stops before `BindRepo`); `TestCloudRunDeployer_Trigger` (fake dispatcher receives creds, repoURL, `"deploy.yml"`, `"main"`); `TestGitHub_DispatchWorkflow` (real `GitHub` against the fake server: path and body `{"ref":"main"}`, 404 → error, foreign owner refused before any call).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test ./test -run 'CloudRunDeployer|DispatchWorkflow'` → PASS.

### Task 4: Workflow + service.yml templates and CreateRepo

**Files:**
- Modify: `templates/deploy.yml.tmpl`, `templates/` embed file (add `ServiceYAML`), `service/github.go` (`CreateRepo`, `PushAndOpenPR`, `DeployData` unchanged)
- Create: `templates/service.yml.tmpl`
- Test: extend `test/github_test.go`

**Interfaces:**
- Produces: `type ServiceData struct{ ServiceName string }`, `func RenderServiceYAML(d ServiceData) ([]byte, error)` (validates `ServiceName` with `safeValue`); the rendered file contains the literal placeholder `IMAGE_PLACEHOLDER` for the image. `CreateRepo` signature unchanged; when `gcp.ProjectID != ""` it commits `.github/workflows/deploy.yml` then `service.yml`, both with commit messages ending in `[skip ci]`.

- [ ] **Step 1: Write failing tests**: `TestRenderDeployWorkflow_Stages` (rendered YAML parses, has `workflow_dispatch`, `push` with `paths-ignore: .github/**`, and contains in order the notify steps `building`, `testing`, `deploying`, a `success` post with `live_url`, a failure post, a real `docker build`, `gcloud run services replace service.yml`, and every notify uses `|| true`); `TestRenderServiceYAML` (parses as YAML; `metadata.name` equals the service name; annotation `run.googleapis.com/invoker-iam-disabled: "true"`; port 8080; contains `IMAGE_PLACEHOLDER`; a name with `"` or a space is rejected); `TestGitHub_CreateRepo_CommitsIaC` (fake records two contents PUTs, paths `.github/workflows/deploy.yml` and `service.yml`, messages contain `[skip ci]`; with empty `GCPCreds` neither is committed); `TestGitHub_PushAndOpenPR_RestoresServiceYML` (base has `service.yml` = `TRUSTED`; workspace overwrites it → pushed branch still has `TRUSTED`; extend `seedRemote` or add a variant).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** Workflow stage wrapper: one `notify` shell function (`curl -s --max-time 30 --retry 3 ... || true`) defined at the top of the deploy step; a failing step posts `failed` with `error_message` naming the stage. Image tag uses `${{ github.sha }}`; `sed` replaces `IMAGE_PLACEHOLDER` in `service.yml`. In `PushAndOpenPR`, restore `service.yml` from base when it exists there, same pattern as `.github`.
- [ ] **Step 4: Run** `go test ./test -run 'Workflow|ServiceYAML|CreateRepo|PushAndOpenPR'` → PASS.

### Task 5: Webhook progress events

**Files:**
- Modify: `handler/webhook.go`
- Test: `test/project_handler_test.go` (webhook section) or new `test/webhook_progress_test.go`

**Interfaces:**
- Consumes: `service.StepRank` (Task 1), `model.StepBuilding/StepTesting`.
- Produces: webhook accepts `{project_id, step}` with `step ∈ {building,testing,deploying}` and no `status`; success/failed payloads unchanged.

- [ ] **Step 1: Write failing tests** (reuse the webhook test server helper): `TestWebhook_ProgressForward` (project `deploying/merging`; post `building` then `testing` → 200 each, `current_step` follows, status still `deploying`); `TestWebhook_ProgressNeverBackwards` (at `testing`, posting `building` → 200, step stays `testing`); `TestWebhook_ProgressIgnoredWhenNotDeploying` (project `waiting_for_approval` → 409 or 200-ignored — pick 409 consistent with final events; project unchanged); `TestWebhook_ProgressBadStepAndAuth` (unknown step → 400; wrong secret → 401; step plus empty status with project of another id's secret → 401).
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement**: authenticate as today; if `Status == ""` and `Step != ""` → validate step, `GetProject`, if `status != deploying` → 409, if `StepRank(step) <= StepRank(current)` → 200 `{"status":"ok","ignored":true}`, else `UpdateProjectIf(..., deploying, {"current_step": step})`. **Step 4: Run** → PASS.

### Task 6: Orchestrator deploy mode

**Files:**
- Modify: `service/orchestrator.go`, `model` unchanged
- Test: `test/orchestrator_test.go`

**Interfaces:**
- Consumes: `Deployer`, `DeployTarget`, `ServiceName(name, id)` from `service/github.go`.
- Produces: `OrchestratorConfig.Deployer Deployer` (nil = not deploy mode). With a `Deployer` set the orchestrator behaves like `Demo` (GitHub creds only, no BYOK), plus: before `CreateRepo` it calls `Deployer.Prepare(ctx, DeployTarget{ProjectID: id, ServiceName: ServiceName(p.Name,id), RepoOwner: gh.Owner, RepoName: ServiceName(p.Name,id)})` and passes the returned `model.GCPCreds` to `CreateRepo`; after the merge it does **not** write `current_step` and does **not** set `success` (the webhook finishes the project).

- [ ] **Step 1: Write failing tests** with a `fakeDeployer` (records `Prepare` targets, returns `gcpCreds`, optional error): `TestOrchestrator_Deploy_PreparesBeforeRepo` (call order `prepare`, `create`; `CreateRepo` received creds equal to `gcpCreds`; target repo name equals `service.ServiceName("My App","p1")`); `TestOrchestrator_Deploy_EndsWaitingForWebhook` (after approve: status `deploying`, `current_step` `merging`, `success` never set by the orchestrator); `TestOrchestrator_Deploy_PrepareFailure` (Prepare error → project `failed`, `error_message` contains `prepare`, `host.count("create") == 0`). Update the existing `TestOrchestrator_HappyPath` expectation: final step is `merging` and the history ends `deploying|merging` (no `deploying|deploying`).
- [ ] **Step 2: Run** `go test ./test -run Orchestrator` → FAIL. **Step 3: Implement** in `run`: treat `Deployer != nil` like `Demo` for credentials; call Prepare in a `stage(...)` before create; remove the post-merge `current_step=deploying` write in the non-Demo path. **Step 4: Run** → all Orchestrator tests PASS.

### Task 7: Watchdog

**Files:**
- Create: `service/deploy_watchdog.go`
- Modify: `repository/store.go` (add `ListStaleDeploying`)
- Test: `test/watchdog_test.go`; DB-backed test added to `test/repository_db_test.go` style if testcontainers is available there

**Interfaces:**
- Produces: `type WatchdogStore interface{ ListStaleDeploying(ctx context.Context, before time.Time) ([]model.Project, error); UpdateProjectIf(ctx context.Context, id, fromStatus string, fields map[string]any) (bool, error) }`; `func NewDeployWatchdog(s WatchdogStore, timeout, interval time.Duration) *DeployWatchdog`; `func (w *DeployWatchdog) Sweep(ctx context.Context, now time.Time) (int, error)` (returns number failed); `func (w *DeployWatchdog) Run(ctx context.Context)` (loop until ctx done). `(*repository.Store).ListStaleDeploying` returns projects with `status='deploying' AND updated_at < before`.

- [ ] **Step 1: Write failing tests** with an in-memory store: `TestWatchdog_FailsStaleDeploying` (one deploying project updated 20m ago, one 1m ago, one `success` 20m ago; timeout 15m → exactly the first becomes `failed` with message containing `no update from GitHub Actions`, `Sweep` returns 1); `TestWatchdog_DoesNotOverrideFinishedProject` (store CAS reports not applied → count 0, no error); `TestWatchdog_RunStopsOnCancel`.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** `Sweep` (list, then `UpdateProjectIf(deploying → failed)` per project) and the repository query. **Step 4: Run** → PASS; if `repository_db_test.go` runs a MySQL container locally, add a case for `ListStaleDeploying` there too.

### Task 8: Manual deploy endpoint

**Files:**
- Modify: `service/orchestrator.go` (add `Redeploy`, `ErrNotRedeployable`), `handler/project.go` (add `Deploy`, extend `ProjectRunner`), `main.go` route (wired in Task 9)
- Test: `test/project_handler_test.go`, `test/orchestrator_test.go`

**Interfaces:**
- Consumes: `Deployer.Trigger`, `OrchestratorConfig.Deployer`.
- Produces: `var ErrNotRedeployable = errors.New("project cannot be re-deployed")`; `func (o *Orchestrator) Redeploy(ctx context.Context, projectID, ownerID string) error` (not-found for other owners as `model.ErrNotFound`; allowed when status `success`, or `failed` with `StepRank(current_step) >= StepRank("merging")`; CAS to `deploying` + `current_step=merging` and clear `error_message`; calls `Deployer.Trigger` with the owner's GitHub creds and `RepoURL`; on trigger error set `failed` and return it; if no Deployer configured return `ErrNotRedeployable`). `ProjectRunner` gains `Redeploy(ctx context.Context, projectID, ownerID string) error`. `func (h *ProjectHandler) Deploy(w, r)` → 202 `{"status":"deploying"}`, 404, 409, 500.

- [ ] **Step 1: Write failing tests**: orchestrator `TestOrchestrator_Redeploy_FromSuccess` (project `success`, trigger recorded once, status `deploying`); `..._FailedBeforeMergeRefused` (`failed` at `repo_created` → `ErrNotRedeployable`, no trigger); `..._MidDeployRefused` (`deploying` → `ErrNotRedeployable`); `..._OtherUserNotFound`; `..._TriggerFailureMarksFailed`. Handler `TestProjectHandler_Deploy` status mapping 202/404/409/401. Update the fake `ProjectRunner` used in handler tests, if any, with the new method.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement.** **Step 4: Run** `go test ./test -run 'Redeploy|ProjectHandler'` → PASS.

### Task 9: main.go wiring

**Files:**
- Modify: `main.go`
- Test: `test/deploy_config_test.go` (env errors already covered); manual start check

**Interfaces:**
- Consumes: Tasks 1–8.
- Produces: deploy mode when `DEMO_TEMPLATE_REPO != ""` and `GCP_PROJECT_ID != ""` and not `REPO_ONLY`; startup `log.Fatal` if deploy mode and `WEBHOOK_URL` or `WEBHOOK_SECRET` missing, or `DeployConfigFromEnv` fails; builds `NewGCP`, `NewCloudRunDeployer(gcp, gh)`, passes `Deployer` into `OrchestratorConfig`, registers `POST /api/projects/{id}/deploy`, registers the webhook (as in non-demo), starts `NewDeployWatchdog(store, cfg.Timeout, time.Minute).Run(ctx)` tied to the shutdown context.

- [ ] **Step 1:** implement the wiring with the precedence rules from Global Constraints; the webhook route is registered whenever not repo-only/demo-only.
- [ ] **Step 2: Verify** `go build ./... && go vet ./... && go test ./...` pass; run `REPO_ONLY=false DEMO_TEMPLATE_REPO=https://x GCP_PROJECT_ID=p API_KEY=k ENCRYPTION_MASTER_KEY=… go run . serve` without `WEBHOOK_URL` and confirm the process exits with the missing-variable message (DB must be reachable; otherwise note it as unverified).

### Task 10: Demo template app

**Files:**
- Create: `demo-template/main.go`, `demo-template/go.mod` (module `demo-template`), `demo-template/Dockerfile`, `demo-template/README.md`
- Test: `test/demo_template_test.go`

- [ ] **Step 1: Write failing test** `TestDemoTemplate_BuildsAndServes` (skips without `go`): `go build` in `demo-template`, start the binary with `PORT` set to a free port, `GET /` returns 200 and a body containing `Hello`; `GET /healthz` returns 200.
- [ ] **Step 2: Run** → FAIL. **Step 3: Implement** a stdlib HTTP server reading `PORT` (default `8080`), `/` hello page, `/healthz`; multi-stage `Dockerfile` (golang build, `distroless/static` runtime, `EXPOSE 8080`); README says to push this folder as its own public repo and use it as `DEMO_TEMPLATE_REPO`. **Step 4: Run** → PASS; `docker build demo-template` if docker is available (else note unverified).

### Task 11: Platform deployment files

**Files:**
- Modify: `Dockerfile`, `service.yml`, `docker-compose.yml`
- Create: `scripts/grant-platform-roles.sh`, `docs/deploy-setup.md`

- [ ] **Step 1: Dockerfile** — runtime stage becomes `debian:12-slim` + `git` + `ca-certificates`, non-root user, same `ENTRYPOINT ["/app"]`/`CMD ["serve"]`. Verify `docker build .` succeeds if docker is available.
- [ ] **Step 2: service.yml** — `autoscaling.knative.dev/minScale: "1"`, `maxScale: "1"`, annotation `run.googleapis.com/cpu-throttling: "false"`; add env `GCP_PROJECT_ID`, `GCP_REGION`, `GCP_ARTIFACT_REPO`, `GCP_ALLOWED_GITHUB_OWNERS`, `DEMO_TEMPLATE_REPO`, `WEBHOOK_URL` (plain values with clear placeholders) and `WEBHOOK_SECRET` (secretKeyRef). Validate with `python -c yaml.safe_load` (or `yq`).
- [ ] **Step 3: scripts/grant-platform-roles.sh** — `set -euo pipefail`; takes `PROJECT_ID` and `PLATFORM_SA` (defaults documented); `gcloud projects add-iam-policy-binding` for `roles/serviceusage.serviceUsageAdmin`, `roles/iam.workloadIdentityPoolAdmin`, `roles/iam.serviceAccountAdmin`, `roles/resourcemanager.projectIamAdmin`, `roles/artifactregistry.admin`, `roles/run.admin`; prints what it did. Check with `bash -n` and `shellcheck` if present.
- [ ] **Step 4: docs/deploy-setup.md** — the one-time grant, deploy platform, set `WEBHOOK_URL` after first deploy (`gcloud run services update … --update-env-vars`), env var table, cost note for `minScale 1`, how to run each live test. Also add the new env vars to `docker-compose.yml`.

### Task 12: Live test scripts

**Files:**
- Modify: `test/github_live_test.go` (or create `test/deploy_live_test.go`)

**Interfaces:** Consumes Tasks 2, 3, 4, 6. All tests skip unless their env flag is `1`, print numbered `STEP` log lines like `TestLive_DemoFlow`, never delete resources, never print tokens.

- [ ] **Step 1: `TestLive_GCPProvision`** (`LIVE_GCP=1`, needs ADC with the platform roles, `GCP_PROJECT_ID`, `GCP_ALLOWED_GITHUB_OWNERS`): `NewGCP(ctx, cfg, nil)`, `EnsureBase`, `BindRepo(ctx, owner, "live-bind-<time>")`, log the returned `WIFProvider` and `ServiceAccount`.
- [ ] **Step 2: `TestLive_Deploy`** (`LIVE_GITHUB=1 LIVE_GCP=1`, `GITHUB_TOKEN`, `DEMO_TEMPLATE_REPO`, `WEBHOOK_URL` may be any URL): run the real orchestrator with `Deployer: NewCloudRunDeployer(gcp, gh)` and the demo generator as in `TestLive_DemoFlow`; after approve and merge, poll the GitHub Actions API (`GET /repos/{o}/{r}/actions/runs`) logging each run/job status until conclusion; fail if not `success`; then `GET https://run.googleapis.com/v2/projects/{p}/locations/{r}/services/{name}` (ADC) for the URL, request it, require HTTP 200, log the URL. Overall timeout 10 minutes.
- [ ] **Step 3: `TestLive_Platform`** (`PLATFORM_URL`, `API_KEY`, `GITHUB_TOKEN`): over HTTP create a user (`POST /users`, unique email), `POST /api/integrations/github`, `POST /api/projects`, poll `GET /api/projects/{id}` logging each `status/current_step` change until `waiting_for_approval`, `POST …/approve`, poll through `building`, `testing`, `deploying` to `success`, then `GET live_url` expecting 200. Timeout 12 minutes.
- [ ] **Step 4: Verify** `go vet ./test` passes and `go test ./test -run TestLive -v` shows all three SKIP without env. State plainly in the final report that the live tests were not run against real GCP/GitHub.

---

## Self-Review

- **Spec coverage:** modes/env → Tasks 1, 9; Deployer interface → 1, 3; provisioner → 2; repo IaC + `[skip ci]` + `service.yml` restore → 4; workflow stages and best-effort notify → 4; webhook progress → 5; orchestrator changes (no step write after merge, Prepare first) → 6; watchdog → 7; manual endpoint → 8; demo template → 10; image/service.yml/grant script/docs → 11; live tests → 12; isolation limitation documented in `docs/deploy-setup.md` (Task 11).
- **Type consistency:** `DeployTarget`, `Deployer`, `GCPBinder`, `WorkflowDispatcher`, `StepRank`, `ErrNotRedeployable`, `ServiceData`, `RenderServiceYAML` are defined once and used with the same names later; `model.GCPCreds` is the shared carrier between Prepare, CreateRepo and the workflow.
- **Proportion:** each task names files, signatures, test names with assertions and values; bodies are left to the implementer.
