# Temporal Orchestration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the goroutine `Orchestrator` with Temporal workflows (whole run: generate flow, approval gate, merge, deploy phase, redeploy), running against a Temporal dev server in Docker.

**Architecture:** `ProjectWorkflow` / `RedeployWorkflow` orchestrate; every external call is an activity on a struct that wraps the existing interfaces (`RepoHost`, `CodeGenerator`, `Deployer`, ...). Activities load credentials from MySQL themselves, so no secret enters workflow history. A `TemporalRunner` keeps the `handler.ProjectRunner` API (`Start`/`Approve`/`Redeploy`); the webhook handler additionally signals the workflow. MySQL stays the read model. The worker runs inside the `serve` process.

**Tech Stack:** Go 1.26, `go.temporal.io/sdk`, `go.temporal.io/api` (serviceerror), Docker Compose (`temporalio/temporal`, `server start-dev`).

**Spec:** `docs/superpowers/specs/2026-11-27-temporal-orchestration-design.md` (read it first, including "Resolved details").

## Global Constraints

- Workflow ID = project ID, for both `ProjectWorkflow` and `RedeployWorkflow`.
- Task queues: `anz-projects` (workflows + light activities), `anz-generate` (`GenerateAndOpenPR`, concurrency cap = `MaxConcurrentGenerations`, default 2).
- Signals: `approve`, `deploy-progress`, `deploy-result` (no payload).
- Activity inputs/results never contain credentials; activities load and decrypt them from MySQL by project ID.
- Workflow code is deterministic: no `time.Now`, goroutines, map iteration, or I/O; use `workflow.*`.
- HTTP API, MySQL schema, webhook payload and status values are unchanged.
- **No `go.temporal.io/sdk/testsuite` tests.** Tests are ordinary unit tests in package `service_test` / `handler_test` beside the code (per-file layout), using fakes.
- Config: `TEMPORAL_ADDRESS` (default `localhost:7233`); Temporal image tag pinned in compose.
- Commit with the attribution line from the session system reminder. Stage files by explicit path (the working tree has unrelated changes in `.gitignore` and `scripts/test-server.sh`).

## Review Focus

Inputs/conditions the spec implies but the happy path never hits, most likely first:

1. `CreateRepo` retried after a partial success (repo exists, variables/files already set) must succeed, not fail on "already exists". Pinned in Task 2.
2. `Approve` called twice, or after the workflow ended (timeout/restart): twice is harmless; ended returns `ErrNoPendingRun`. Pinned in Task 6.
3. Webhook arrives for a project whose workflow is gone or finished: still 200, MySQL still updated, no error from the signal. Pinned in Task 7.
4. Duplicate webhook delivery: the second one must not break the run. Pinned in Task 7.
5. Generation fails or is cancelled midway: the workspace is still cleaned. Pinned in Task 4.
6. Worker restart mid-run resumes without losing the PAT or re-running finished steps. Pinned by the manual checklist in Task 10.

---

### Task 1: Extract shared ports from the orchestrator (no behaviour change)

**Files:**
- Create: `service/ports.go`
- Modify: `service/orchestrator.go` (remove what moves)
- Commit also: the two docs under `docs/superpowers/`

**Interfaces:**
- Produces (moved verbatim, still package `service`): interfaces `ProjectStore`, `IntegrationReader`, `RepoHost`, `CodeGenerator`, `SecretReader`; errors `ErrNotWaiting`, `ErrNoPendingRun`, `ErrAlreadyRunning`, `ErrNotRedeployable`; constants `defaultApprovalTimeout`, `defaultStageTimeout`, `defaultMaxGenerations`, `maxErrorMessageLen`, `runClockSkew`; `OrchestratorConfig` is **not** moved (it dies with the orchestrator).
- Produces new free functions (moved out of `*Orchestrator` methods, same bodies, taking explicit args): `loadIntegration(ctx, in IntegrationReader, masterKey []byte, ownerID, provider string, into any) error` and `resolveAPIKey(ctx, secrets SecretReader, gcp model.GCPCreds) (string, error)`. `Orchestrator` methods become thin callers.

- [ ] **Step 1:** `git switch -c feat/temporal-orchestration`.
- [ ] **Step 2:** Move the items above into `service/ports.go`; convert the two helpers to free functions and update their callers in `orchestrator.go`.
- [ ] **Step 3:** Run `go build ./... && go test ./service/... ./handler/...`. Expected: PASS, unchanged.
- [ ] **Step 4:** Commit: `git add service/ports.go service/orchestrator.go docs/superpowers && git commit -m "refactor: extract orchestrator ports; add Temporal design and plan"`.

---

### Task 2: Make `CreateRepo` and `MergePR` safe to repeat

**Files:**
- Modify: `service/github.go` (`CreateRepo`, `configureActions`, `putFile`, `MergePR`)
- Test: `service/github_test.go` (extend `fakeGitHub` to answer the new calls)

**Interfaces:** signatures unchanged. New behaviour only.

- [ ] **Step 1: Write failing tests** in `service/github_test.go`:
  - `TestGitHub_CreateRepo_RepeatAfterPartialSuccess`: fake returns 422 `name already exists` on `POST /user/repos`, 422 on `POST .../actions/variables` (variable exists), 422 on `PUT .../contents/...` (file exists). Assert `CreateRepo` returns the existing repo's `html_url` with nil error, variables were updated via `PATCH .../actions/variables/{name}`, and files were updated by re-PUT with the `sha` from `GET .../contents/{path}`.
  - `TestGitHub_CreateRepo_OtherCreateErrorStillFails`: 422 with a different message (or 403) still returns an error.
  - `TestGitHub_MergePR_AlreadyMergedIsSuccess`: `PUT /pulls/N/merge` answers 405; `GET /pulls/N` reports `merged: true`; `MergePR` returns nil. A 405 with `merged: false` still errors.
- [ ] **Step 2:** Run `go test ./service -run 'TestGitHub_(CreateRepo|MergePR)' -v`. Expected: the three new tests FAIL.
- [ ] **Step 3: Implement.** `CreateRepo`: on `APIError` 422 from the create call, `GET /repos/{owner}/{name}` and continue with that repo. `configureActions`: on 422/409 for a variable, `PATCH` it. `putFile`: on 422, `GET` the file for its `sha` and PUT again with it. `MergePR`: on 405/409 from the merge call, `GET` the PR and return nil iff `merged` is true.
- [ ] **Step 4:** Run `go test ./service -run TestGitHub -v`. Expected: all PASS (existing tests included).
- [ ] **Step 5:** Commit: `git add service/github.go service/github_test.go && git commit -m "feat(github): make CreateRepo and MergePR safe to repeat"`.

---

### Task 3: Temporal foundation (dependency, types, Docker service)

**Files:**
- Modify: `go.mod`, `go.sum`, `docker-compose.yml`
- Create: `service/temporal_types.go`

**Interfaces:**
- Produces in `service/temporal_types.go`:
  ```go
  const (
      TaskQueueProjects    = "anz-projects"
      TaskQueueGenerate    = "anz-generate"
      SignalApprove        = "approve"
      SignalDeployProgress = "deploy-progress"
      SignalDeployResult   = "deploy-result"
  )
  type WorkflowConfig struct {
      RepoOnly, Demo, Deploy, Polling bool
      ApprovalTimeout  time.Duration // default 24h
      DeployTimeout    time.Duration // inactivity timer; 0 = no timer
      StageTimeout     time.Duration // GitHub activity StartToClose, default 5m
  }
  type ProjectInput struct { ProjectID string; Config WorkflowConfig }
  ```

- [ ] **Step 1:** `go get go.temporal.io/sdk go.temporal.io/api && go mod tidy`.
- [ ] **Step 2:** Create `service/temporal_types.go` as above.
- [ ] **Step 3:** Add a `temporal` service to `docker-compose.yml`: image `temporalio/temporal` (pin a current tag), command `server start-dev --ip 0.0.0.0 --db-filename /data/temporal.db`, ports `7233:7233` and `8233:8233`, named volume `temporal_data:/data`, a healthcheck (e.g. `temporal operator cluster health`). Add to `app`: env `TEMPORAL_ADDRESS: temporal:7233`, and `depends_on: temporal: condition: service_healthy`.
- [ ] **Step 4:** Run `go build ./... && docker compose up -d temporal && docker compose ps`. Expected: build OK; `temporal` healthy; `curl -sf localhost:8233 >/dev/null` succeeds.
- [ ] **Step 5:** Commit: `git add go.mod go.sum docker-compose.yml service/temporal_types.go && git commit -m "feat: add Temporal dependency, shared types and dev server"`.

---

### Task 4: Activities

**Files:**
- Create: `service/activities.go`, `service/fakes_test.go`
- Test: `service/activities_test.go`

**Interfaces:**
- Consumes: Task 1 ports and helpers, Task 3 types, existing `Deployer`, `DeployMonitor`, `ServiceName`, `StepRank`, `model.Status*`/`Step*`.
- Produces `type Activities struct { Store ProjectStore; Integrations IntegrationReader; GitHub RepoHost; Gen CodeGenerator; Secrets SecretReader; Deployer Deployer; Monitor *DeployMonitor; MasterKey []byte; StageTimeout time.Duration }` with methods (all `(ctx context.Context, in X) (Y, error)`; inputs are small structs holding `ProjectID` plus flags, never credentials):
  - `ClaimProject(ctx, id string) (bool, error)`: `UpdateProjectIf` pending to generating/initializing.
  - `PrepareAndCreateRepo(ctx, CreateRepoInput{ProjectID string; Deploy, RepoOnly bool}) (string, error)`: loads project and creds, calls `Deployer.Prepare` first when `Deploy`, then `CreateRepo`, then writes `repo_url` and step `repo_created`.
  - `GenerateAndOpenPR(ctx, GenerateInput{ProjectID string}) (string, error)`: resolves BYOK key, generates (heartbeating), writes step `tests_passed`, `PushAndOpenPR`, returns the PR URL; `Cleanup` in a `defer`.
  - `UpdateProject(ctx, UpdateInput{ProjectID string; Fields map[string]any}) error`.
  - `MergePR(ctx, id string) (time.Time, error)`: merges using the stored `repo_url`/`pr_url`, sets step `merged` only if still `deploying/merging`, returns `now - runClockSkew`.
  - `WatchDeploy(ctx, WatchInput{ProjectID string; Since time.Time}) error`: loads GitHub creds, wraps `Monitor.Watch`, heartbeats.
  - `ClaimRedeploy(ctx, id string) (bool, error)`: compare-and-set from the project's current success/failed state to `deploying/merged` (error cleared), as `Redeploy` does today.
  - `TriggerDeploy(ctx, id string) (time.Time, error)`: `Deployer.Trigger`; returns `now - runClockSkew`.
  - `FailDeploy(ctx, FailInput{ProjectID, Message string}) error`: `UpdateProjectIf(deploying → failed)` only.
  - `MarkFailed(ctx, FailInput) error`: sets failed, truncating the message to `maxErrorMessageLen`.
  - Config problems (integration missing, decrypt/parse failure, BYOK unavailable, no secret reader) are returned as `temporal.NewNonRetryableApplicationError`.

- [ ] **Step 1:** Move `fakeHost`, `fakeGen`, `fakeSecretReader`, `fakeDeployer` and their helpers from `service/orchestrator_test.go` into `service/fakes_test.go` (package `service_test`); add a small `newActivities(t, ...)` helper that builds `Activities` over `testutil` memstore. Run `go test ./service` to confirm still PASS.
- [ ] **Step 2: Write failing tests** in `service/activities_test.go`, each invoking the method directly:
  - `TestActivities_ClaimProject_OnlyOnce`: first call true, second false.
  - `TestActivities_PrepareAndCreateRepo_PreparesBeforeRepoInDeployMode` (call order Prepare then CreateRepo), `..._PrepareFailure`, `..._RepoOnlyRecordsRepoURL`.
  - `TestActivities_GenerateAndOpenPR_HappyPath` (returns PR URL, step `tests_passed` written, `Cleanup` called once), `..._BYOKKeyPassedToGenerator`, `..._BYOKWithoutSecretReaderIsNonRetryable`, `..._PushFailureStillCleansWorkspace`, `..._GenerateFailureStillCleansWorkspace`.
  - `TestActivities_MergePR_SetsMergedStepOnlyWhenStillMerging` (a webhook-advanced step is not overwritten), `..._FailureIsReturned`.
  - `TestActivities_ClaimRedeploy_Refusals` (running, wrong owner state, no repo) and `..._FromSuccessAndFailedAfterMerge`.
  - `TestActivities_TriggerDeploy_UsesDeployer`.
  - `TestActivities_FailDeploy_DoesNotOverwriteTerminalStatus`.
  - `TestActivities_MarkFailed_TruncatesLongMessage`.
  - `TestActivities_MissingIntegrationIsNonRetryable` (assert `temporal.IsApplicationError` + `NonRetryable()`).
- [ ] **Step 3:** Run `go test ./service -run TestActivities -v`. Expected: FAIL (undefined).
- [ ] **Step 4:** Implement `service/activities.go`. Reuse the bodies of `run()`, `Redeploy()`, `fail()` and `loadCreds`; do not duplicate logic that Task 1 already extracted.
- [ ] **Step 5:** Run `go test ./service -run TestActivities -v`. Expected: PASS.
- [ ] **Step 6:** Commit: `git add service/activities.go service/activities_test.go service/fakes_test.go service/orchestrator_test.go && git commit -m "feat: add Temporal activities"`.

---

### Task 5: Workflows

**Files:**
- Create: `service/project_workflow.go`

**Interfaces:**
- Consumes: Task 3 types, Task 4 `Activities` method names (referenced as method values, e.g. `(*Activities).ClaimProject`).
- Produces: `func ProjectWorkflow(ctx workflow.Context, in ProjectInput) error` and `func RedeployWorkflow(ctx workflow.Context, in ProjectInput) error`.

Tests: none by design (no Temporal test suite). Verified in Task 8 and Task 10; here only build and vet.

- [ ] **Step 1: Implement `ProjectWorkflow`** following spec steps 1 to 9: claim (false → return nil); `PrepareAndCreateRepo` (RepoOnly: `UpdateProject` status success, return); `GenerateAndOpenPR` run on `TaskQueueGenerate` via `ActivityOptions.TaskQueue`; `UpdateProject` pr_url + `waiting_for_approval`; wait for `SignalApprove` or `ApprovalTimeout` (`workflow.NewTimer` + selector); `UpdateProject` deploying/merging; `MergePR`; Demo without Deploy: status success, return; `Polling`: `WatchDeploy`; otherwise the deploy wait. Any returned error runs `MarkFailed` through `workflow.NewDisconnectedContext`, then returns the error. Approval timeout message: `approval timed out`.
- [ ] **Step 2: Deploy wait helper** `waitForDeploy(ctx, id string, timeout time.Duration) error`: loop with a selector over the `deploy-progress` channel, the `deploy-result` channel, and a fresh cancellable timer each iteration (skip the timer when `timeout == 0`). Progress: cancel the timer, loop. Result: return nil. Timer: run `FailDeploy` with message `<WatchdogFailurePrefix> for <timeout>`, return nil.
- [ ] **Step 3: Activity options** per spec: GitHub/MySQL activities `StartToClose = StageTimeout`, bounded retries with backoff; `GenerateAndOpenPR` long `StartToClose`, `HeartbeatTimeout`, `MaximumAttempts: 1`; `WatchDeploy` `StartToClose` slightly above `Monitor` timeouts with heartbeats. Pick the numbers and keep them as named constants.
- [ ] **Step 4: Implement `RedeployWorkflow`:** `ClaimRedeploy` (false → return nil), `TriggerDeploy`, then `Polling ? WatchDeploy : waitForDeploy`; errors run `MarkFailed` as above.
- [ ] **Step 5:** Run `go build ./... && go vet ./service/`. Expected: clean.
- [ ] **Step 6:** Commit: `git add service/project_workflow.go && git commit -m "feat: add ProjectWorkflow and RedeployWorkflow"`.

---

### Task 6: `TemporalRunner` (Start / Approve / Redeploy)

**Files:**
- Create: `service/temporal_runner.go`
- Test: `service/temporal_runner_test.go`

**Interfaces:**
- Consumes: Task 3/5 names; ports from Task 1.
- Produces:
  ```go
  type WorkflowClient interface {
      ExecuteWorkflow(ctx context.Context, opts client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
      SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
  }
  func NewTemporalRunner(c WorkflowClient, store ProjectStore, cfg WorkflowConfig, deployer Deployer) *TemporalRunner
  func (r *TemporalRunner) Start(projectID string) error
  func (r *TemporalRunner) Approve(ctx context.Context, projectID, ownerID string) error
  func (r *TemporalRunner) Redeploy(ctx context.Context, projectID, ownerID string) error
  func (r *TemporalRunner) SignalDeploy(ctx context.Context, projectID, signal string) error // ignores NotFound
  ```
  (`*client.Client` satisfies `WorkflowClient`; `client.Client` is from `go.temporal.io/sdk/client`.)

- [ ] **Step 1: Write failing tests** with a `fakeWorkflowClient` that records calls and can return `*serviceerror.WorkflowExecutionAlreadyStarted` / `*serviceerror.NotFound`:
  - `TestRunner_Start_StartsWorkflowWithProjectIDAsWorkflowID` (workflow ID, task queue `anz-projects`).
  - `TestRunner_Start_AlreadyStartedMapsToErrAlreadyRunning`.
  - `TestRunner_Approve_Guards` (ported from `TestOrchestrator_ApproveGuards`: wrong owner is `model.ErrNotFound`, not waiting is `ErrNotWaiting`, workflow NotFound is `ErrNoPendingRun`).
  - `TestRunner_Approve_SignalsWorkflow_Twice_IsHarmless` (Review Focus 2).
  - `TestRunner_Redeploy_Refusals` (ported from `TestOrchestrator_Redeploy_Refusals`: no deployer, wrong owner, wrong state, no repo URL, all `ErrNotRedeployable`/`ErrNotFound`).
  - `TestRunner_Redeploy_StartsRedeployWorkflow_AlreadyStartedIsErrAlreadyRunning`.
  - `TestRunner_SignalDeploy_MissingWorkflowIsIgnored`.
- [ ] **Step 2:** Run `go test ./service -run TestRunner -v`. Expected: FAIL (undefined).
- [ ] **Step 3:** Implement. `Start` uses a 10s `context.WithTimeout` (the interface has no ctx) and `StartWorkflowOptions{ID: projectID, TaskQueue: TaskQueueProjects}`. `Approve` keeps today's ownership and `waiting_for_approval` checks before signalling. `Redeploy` keeps today's read-only precondition checks, then starts `RedeployWorkflow`.
- [ ] **Step 4:** Run `go test ./service -run TestRunner -v`. Expected: PASS.
- [ ] **Step 5:** Commit: `git add service/temporal_runner.go service/temporal_runner_test.go && git commit -m "feat: add Temporal-backed project runner"`.

---

### Task 7: Webhook sends signals

**Files:**
- Modify: `handler/webhook.go`
- Test: `handler/webhook_test.go`

**Interfaces:**
- Produces: `type DeploySignaler interface { SignalDeploy(ctx context.Context, projectID, signal string) error }` and an exported field `Signaler DeploySignaler` on `WebhookHandler` (nil = no signalling; `NewWebhookHandler` signature unchanged). Handler uses `service.SignalDeployProgress` / `service.SignalDeployResult`.

- [ ] **Step 1: Write failing tests** with a `fakeSignaler`:
  - `TestWebhook_ResultApplied_SignalsDeployResult`.
  - `TestWebhook_Progress_SignalsDeployProgress` (only when the step actually advanced).
  - `TestWebhook_DuplicateDelivery_StillOKAndSignalIsHarmless` (Review Focus 4: 200, signal allowed).
  - `TestWebhook_Conflict_DoesNotSignal`.
  - `TestWebhook_SignalError_DoesNotFailTheRequest` (Review Focus 3: signaler returns an error, response still 200, MySQL still updated).
  - `TestWebhook_NilSignaler_Works` (existing tests keep passing).
- [ ] **Step 2:** Run `go test ./handler -run TestWebhook -v`. Expected: new tests FAIL.
- [ ] **Step 3:** Implement: signal after a successful write (applied, duplicate, recovered, or advanced progress); log, never return, signal errors.
- [ ] **Step 4:** Run `go test ./handler -v`. Expected: PASS.
- [ ] **Step 5:** Commit: `git add handler/webhook.go handler/webhook_test.go && git commit -m "feat(webhook): signal the workflow on deploy events"`.

---

### Task 8: Worker and `main.go` wiring

**Files:**
- Create: `service/temporal_worker.go`
- Modify: `main.go`

**Interfaces:**
- Consumes: Tasks 4 to 7.
- Produces: `func StartWorkers(c client.Client, acts *Activities, maxGenerate int) (stop func(), err error)`. It starts one worker on `anz-projects` (registers both workflows and all activities) and one on `anz-generate` (`MaxConcurrentActivityExecutionSize: maxGenerate`, registers only `GenerateAndOpenPR`).

- [ ] **Step 1:** Implement `StartWorkers`. Register activities with explicit names so the two queues don't both pick up `GenerateAndOpenPR`.
- [ ] **Step 2:** In `main.go`: `client.Dial` with `TEMPORAL_ADDRESS` (default `localhost:7233`, `log.Fatal` on error); build `Activities` from the existing `store`, `gh`, `gen`, `secretReader`, `deployer`, `masterKey`, plus a `DeployMonitor` when `pollDeploys`; build `WorkflowConfig` from the mode flags (`Deploy: deploy`, `Polling: pollDeploys`, `DeployTimeout: deployTimeout`); `runner := service.NewTemporalRunner(...)`; pass `runner` to `NewProjectHandler`; set `wh.Signaler = runner`; call `StartWorkers`; on shutdown call `stop()` then `c.Close()`.
- [ ] **Step 3:** Leave the old orchestrator and watchdog constructors out of `main.go` (their code is deleted in Task 9).
- [ ] **Step 4:** Run `go build ./... && go vet ./...`. Expected: clean.
- [ ] **Step 5: Smoke test** in demo mode: `docker compose up -d temporal db`, start the app locally with `DEMO_TEMPLATE_REPO` set and a real GitHub PAT integration (see `scripts/test-server.sh`), create a project, then check `http://localhost:8233` shows workflow `<project-id>` Running, waiting on the approval signal. Expected: status `waiting_for_approval` in `GET /api/projects/{id}`.
- [ ] **Step 6:** Commit: `git add service/temporal_worker.go main.go && git commit -m "feat: run Temporal worker in serve and wire the runner"`.

---

### Task 9: Remove the old orchestrator

**Files:**
- Delete: `service/orchestrator.go`, `service/orchestrator_test.go`, `service/orchestrator_e2e_test.go`, `service/deploy_watchdog.go`, `service/deploy_watchdog_test.go`
- Modify: anything still referencing `OrchestratorConfig`, `NewOrchestrator`, `DeployWatchdog` (compile errors will list them); `docs/` mentions

- [ ] **Step 1:** Delete the files above. Keep `DeployMonitor`, `WatchdogFailurePrefix` and everything in `ports.go`.
- [ ] **Step 2:** Run `go build ./... && go vet ./... && go test ./...`. Expected: PASS. Fix leftover references (the old `fakes` already live in `fakes_test.go`).
- [ ] **Step 3:** Run `grep -rn "Orchestrator\|DeployWatchdog\|approvalChans" --include=*.go .`. Expected: no hits (comments mentioning history are fine).
- [ ] **Step 4:** Commit: `git add -A service main.go && git commit -m "refactor: remove goroutine orchestrator and watchdog (replaced by Temporal)"` (verify with `git status` that `.gitignore` and `scripts/test-server.sh` are not staged).

---

### Task 10: Docs and manual workflow checklist

**Files:**
- Create: `docs/temporal.md`

- [ ] **Step 1:** Write `docs/temporal.md`: how to start (`make up`), the Web UI URL, what each workflow/activity/signal does, env var `TEMPORAL_ADDRESS`, and the checklist below.
- [ ] **Step 2: Run the checklist** against the compose stack and tick each item in the doc with what you saw:
  1. Happy path (demo mode): workflow runs through all activities in the UI; project ends `success`.
  2. Approval gate: `POST /approve` makes the waiting workflow continue; second approve is harmless.
  3. Approval timeout: run with a very short `ApprovalTimeout`; project ends `failed` with `approval timed out`.
  4. Deploy wait (deploy mode): webhook `building` then `success` events; the workflow shows signals received and completes; project `success`.
  5. Deploy inactivity: send no webhook; after `DEPLOY_TIMEOUT` the project is `failed` with the `no update from GitHub Actions` message; a late `success` webhook still recovers it.
  6. Restart mid-run (Review Focus 6): stop the app while waiting for approval, restart it, approve; the run continues. In the UI history search for the PAT value: it must not appear anywhere.
  7. Redeploy: `POST /deploy` on a successful project starts `RedeployWorkflow` with the same ID.
- [ ] **Step 3:** Commit: `git add docs/temporal.md && git commit -m "docs: Temporal run guide and manual workflow checklist"`.

---

## Self-review notes

- Spec coverage: infrastructure (T3, T8), workflow + timers + signals (T5), activities, credentials-in-activities, generate+PR merged activity, polling wrapper (T4, T5), API surface (T6), webhook signalling (T7), idempotency (T2), cutover/removal (T9), testing approach and checklist (T10). "Resolved details" 1 to 8 each map to T5/T6/T7/T4.
- Riskiest task: T2 (GitHub behaviours for repeat-safety are verified only against the fake); Task 10 item 6 is the real check.
