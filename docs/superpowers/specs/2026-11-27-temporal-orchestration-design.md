# Temporal orchestration: design

Date: 2026-11-27. Status: draft, awaiting approval.

## Intent and why

The platform runs each project through a goroutine state machine
(`service/orchestrator.go`): create repo, generate code, open PR, wait for human
approval, merge, deploy. This project is a **training exercise to learn
Temporal**. The goal is to rebuild that run on Temporal using its real building
blocks (workflow, activities, retries, signals, timers, test environment), with
Temporal running in a Docker container next to the app.

A side effect is that runs survive an app restart. That is a bonus, not the
reason. The migration should be faithful enough that each Temporal concept is
used where it naturally fits, not bolted on.

## Goals

- The whole run (generate flow, approval gate, merge, deploy phase, redeploy) is
  driven by Temporal. The goroutine orchestrator, in-memory approval channels and
  the `DeployWatchdog` are removed once the new path passes the ported tests.
- Temporal runs as a service in `docker-compose.yml` (dev server, Web UI on 8233).
- The HTTP API and the MySQL schema stay as they are. MySQL stays the read model
  that `GET /api/projects/{id}` reads.
- Each external call is an activity with an explicit timeout and retry policy.
- Tests stay simple: ordinary unit tests next to the code (per-file layout),
  no Temporal test environment; the workflow is checked manually in the Web UI.

## Non-goals

- A production Temporal cluster, Temporal Cloud, multiple workers or namespaces.
- Moving project state out of MySQL.
- Versioning or migrating in-flight workflows across code changes (noted as a
  later topic; the worker and workflow code ship together here).
- Changing the HTTP API shape, the webhook payload, or the deploy templates.

## Anti-goals (a failure even if it technically works)

- Decrypted credentials (GitHub PAT, BYOK key) appearing in workflow history or
  activity inputs/results. History is visible in the Web UI.
- Non-deterministic workflow code (`time.Now`, random, goroutines, map iteration,
  direct I/O inside the workflow function).
- Two orchestration styles left in the code at the end.

## Constraints

- Go module `anz-training`, Go 1.26; add `go.temporal.io/sdk`.
- Existing mode flags (`REPO_ONLY`, demo, deploy, webhook vs polling) must keep
  their behaviour.
- Existing interface seams (`RepoHost`, `CodeGenerator`, `ProjectStore`,
  `Deployer`, `RunWatcher`, `SecretReader`) are reused as the activity
  dependencies.
- Create-repo, push/open-PR and merge must be safe to repeat before they become
  retryable activities (see Idempotency).
- Webhook handler stays authenticated by its per-project secret.

## Approaches considered

- **A. Front half only.** Workflow covers create repo to merge; the deploy phase
  keeps `DeployMonitor` and `DeployWatchdog`. Smaller, but two orchestration
  styles remain and deploy stays non-durable.
- **B. Whole run (chosen).** Workflow covers everything including the deploy
  result arriving as a signal and the watchdog becoming a timer. Biggest change,
  but covers the concepts that differ most from the current code.
- **C. Both behind a `USE_TEMPORAL` flag.** Safest and easy to compare, but two
  orchestrators and double the tests.

B was chosen because the goal is learning, and signals-from-a-webhook plus
timers are where Temporal differs most from today's code. The old orchestrator
may stay in the tree only while the ported tests are being brought over.

## Design

### Infrastructure

- New `temporal` service in `docker-compose.yml`: the `temporalio/temporal`
  image running `server start-dev --ip 0.0.0.0` with a file-backed DB on a named
  volume. Ports 7233 (gRPC) and 8233 (Web UI). The `app` service gets
  `TEMPORAL_ADDRESS=temporal:7233` and waits for it to be healthy. Pin the image
  tag (builder picks a current one).
- The worker runs **inside the existing app process** (`serve`), started in
  `main.go` beside the HTTP server and stopped on the same signal.
- Namespace `default`. Task queues: `anz-projects` for workflows and light
  activities, `anz-generate` for the Claude/generation activity.

### Workflow

One workflow type, `ProjectWorkflow`, with **workflow ID = project ID**. This
replaces the `running` map: starting a second run for the same project returns
"already started", which maps to `ErrAlreadyRunning`.

Input: project ID plus non-secret mode config (`RepoOnly`, `Demo`, deploy-mode
flag, approval timeout, deploy timeout, whether deploys are polled). No secrets.

Flow, mirroring `run()`:

1. Activity: claim project (`pending` to `generating`, compare-and-set). If not
   claimed, the workflow ends quietly (same as today's no-op).
2. Deploy mode: activity `PrepareDeployment`.
3. Activity `CreateRepo`; record `repo_url`, step `repo_created`. `RepoOnly` ends
   here with status `success`.
4. Activity `GenerateAndTest` on the `anz-generate` queue; step `tests_passed`.
5. Activity `PushAndOpenPR`, then `Cleanup` workspace.
6. Set `waiting_for_approval` and `pr_url`, then wait for the **`approve` signal**
   or the approval timer (default 24h, `workflow.AwaitWithTimeout`). Timeout
   fails the project with the existing approval-timeout message.
7. Set `deploying/merging`; activity `MergePR`; step `merged`. Demo mode ends
   here with `success`.
8. Deploy phase (deploy mode): wait for the **`deploy-result` signal** or the
   deploy timer. The timer replaces `DeployWatchdog`; on expiry the project fails
   with the existing `WatchdogFailurePrefix` message so the webhook's late-success
   recovery keeps working. In polling mode (no webhook) a heartbeating polling
   activity wraps the existing `DeployMonitor.Watch` logic and its result is used
   instead of the signal.
9. Any workflow error runs a final `MarkFailed` activity (in a disconnected
   context so cancellation cannot skip it) that writes `failed` plus a truncated
   `error_message`, exactly like `fail()` today.

`RedeployWorkflow` (same workflow ID, started after the previous run finished,
so ID reuse is allowed): trigger activity, then the same deploy-phase wait as
step 8. `Redeploy()` keeps its compare-and-set precondition checks before
starting it.

### Activities and where the seams are

Activities are methods on a struct holding the existing dependencies. Each one
loads what it needs itself: **credentials are read and decrypted inside the
activity from the project's owner ID**, never passed through the workflow.
Every status write that `run()` does today becomes a small activity
(`UpdateProject` / `UpdateProjectIf`). `time.Now()` uses (merge timestamp for
run matching, clock skew) happen inside the activity and are returned as values.

Retry policy is set per activity: GitHub calls retry with backoff and a bounded
attempt count; `GenerateAndTest` gets few or no automatic retries (it is slow and
costly) but heartbeats while running; configuration errors (integration missing,
decrypt/parse failure, BYOK key unavailable) are returned as non-retryable
application errors. The generate concurrency cap (`MaxConcurrentGenerations`,
default 2) becomes the worker option `MaxConcurrentActivityExecutionSize` on the
`anz-generate` queue, replacing the `genSem` semaphore.

### API surface

`handler.ProjectHandler` keeps calling `Start`, `Approve`, `Redeploy` with the
same signatures and errors. A new type in `service` (Temporal-backed) implements
them:

- `Start`: start `ProjectWorkflow`; already-started becomes `ErrAlreadyRunning`.
- `Approve`: keep the ownership and `waiting_for_approval` checks against MySQL,
  then signal `approve`. A missing workflow becomes `ErrNoPendingRun`.
  Signalling is naturally idempotent for the workflow.
- `Redeploy`: as above.

`WebhookHandler` keeps updating MySQL exactly as today and additionally sends the
`deploy-result` signal for terminal results. A signal to a workflow that has
already finished (watchdog gave up, recovery case) is ignored, not an error.

### Idempotency (must land before retries are enabled)

- `CreateRepo`: if the repo already exists for this project, return it instead of
  failing.
- `PushAndOpenPR`: if a PR for the head branch is already open, return its URL.
- `MergePR`: an already-merged PR counts as success.
- Status updates already use forward-only or compare-and-set semantics.

### Testing

Kept simple on purpose: **no Temporal test-suite (`testsuite`) workflow tests.**

- Activity logic is plain Go, so it gets ordinary unit tests next to the code,
  reusing the existing fakes (`internal/testutil`, store fakes): idempotent
  create-repo/PR/merge, config errors being non-retryable, `MarkFailed` message
  truncation.
- The Temporal-backed `Start`/`Approve`/`Redeploy` and the webhook's signalling
  are tested through small interfaces over the Temporal client, with fakes
  (no Temporal server needed).
- The workflow itself (ordering, timers, signals) is verified by running it
  against the compose Temporal server and watching it in the Web UI, using a
  short manual checklist in the docs (happy path, approval timeout, deploy
  timeout, restart mid-run).
- Existing orchestrator tests are deleted together with the orchestrator; the
  behaviours worth keeping are covered by the activity tests above.

## Resolved details (design calls made after the reviewer pass)

These are my calls, not things you decided in conversation. Correct any you
disagree with.

1. **Deploy timer is inactivity-based, like today's watchdog.** The webhook's
   progress events (`building`/`testing`/`deploying`) send a `deploy-progress`
   signal; the workflow re-arms its timer on each one. Only silence for
   `deployTimeout` fails the project. This replaces step 8's single fixed timer.
2. **The webhook stays the only writer of a terminal deploy status** in webhook
   mode. The `deploy-result` signal is a wake-up with no payload; on receiving it
   the workflow just ends. The webhook sends it only after its MySQL write was
   applied or was a duplicate, never on a conflict. If the signal is lost, the
   timer fires and its failure write is a compare-and-set from `deploying`, so it
   cannot overwrite a result the webhook already stored.
3. **Deploy-mode `CreateRepo` calls `Deployer.Prepare` itself.** `Prepare` and
   `CreateRepo` are one activity in deploy mode so GCP creds never enter history.
   `Prepare` must be safe to repeat; verify and fix that while building.
4. **Generate, push/open PR and workspace cleanup are one activity**
   (`GenerateAndOpenPR`) on the `anz-generate` queue, with cleanup in a `defer`.
   This removes the local-disk handoff between activities. It writes the
   `tests_passed` step between generation and push.
5. **Polling mode:** the polling activity wraps `DeployMonitor.Watch` unchanged,
   so it writes MySQL itself and keeps its own start/overall timeouts as the
   authority (the activity's timeout is set a little above them, with
   heartbeats). A "run never started" failure stays distinct from the
   inactivity-timeout failure.
6. **In-flight projects at cutover are out of scope.** They have no workflow;
   `Approve` returns `ErrNoPendingRun` for them and webhook signals to a missing
   workflow are ignored, as a restart strands them today. No backfill.
7. **Redeploy:** `Redeploy` checks preconditions read-only, then starts
   `RedeployWorkflow`; if a run with that ID is still open it returns
   `ErrAlreadyRunning`. The compare-and-set to `deploying` moves into the
   workflow's first activity (like the claim step in `ProjectWorkflow`), so a
   rejected start leaves project state untouched. Both workflow types accept the
   same `deploy-progress`/`deploy-result` signals.
8. **Behaviours dropped on purpose:** the "cancelled: server shutting down"
   message (a worker stopping no longer fails runs; they resume). Panics inside
   activities are recovered by the SDK and surface as activity failures, which
   end in `MarkFailed` ("internal error"). A panic in workflow code is a bug and
   is left to fail the workflow task so it is visible in the Web UI.

## Left to the builder

- Exact file and package layout (keep tests beside code).
- Activity timeout and retry numbers; Temporal image tag; health check command.
- Whether claim/status writes are separate activities or grouped, as long as no
  secrets cross the workflow boundary.
- Worker shutdown/drain behaviour on SIGTERM.
- README/docs updates for running Temporal locally.

## Open questions

None blocking.
