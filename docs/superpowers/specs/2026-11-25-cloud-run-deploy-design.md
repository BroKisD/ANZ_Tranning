# Real Cloud Run deployment of generated apps — design

Date: 2026-11-25. Status: approved in conversation, awaiting written-doc review.

## Intent and why

The platform turns a request into a GitHub repo, puts code in it (for the demo: a clone of a template
repo with mocked tests), opens a PR, and waits for human approval. This design adds the last stage:
**after the PR is merged, GitHub Actions in the new repo builds, tests (both partly mocked) and really
deploys the app to Google Cloud Run**, and the platform shows live progress and the final URL.

The point of the demo is to show that a request becomes a real, reachable URL on GCP, with the
infrastructure created automatically rather than by hand.

## Goals

- After merge, the Actions run in the new repo does: mock build, real `docker build` + push, mock tests,
  real `gcloud run services replace`. The result is a public https URL stored on the project.
- The platform shows progress while Actions runs: `GET /api/projects/{id}` moves through
  `building` -> `testing` -> `deploying` -> `success` (or `failed` with a message).
- GCP setup is automatic. The platform (running on Cloud Run) creates what is missing: APIs, Artifact
  Registry repo, deployer service account, Workload Identity pool/provider, and **one IAM trust binding per
  new repo**. The only manual step is a one-time role grant to the platform's own service account
  (`scripts/grant-platform-roles.sh`, written as part of this work).
- IaC is created with the repo: `service.yml` (Cloud Run spec) and `.github/workflows/deploy.yml` are
  committed when the repo is created.
- A `Deployer` interface covers different ways a deployment starts (auto after merge, manual re-deploy).
- Every part is testable from a script by the user (see Testing).

## Non-goals

- Deploying into each user's own GCP project (all demo apps go to the platform's one GCP project).
- Strong per-project isolation (see "Isolation level"). Terraform. Teardown/deletion of services or repos.
- Real code generation by Claude, real tests, polling the GitHub Actions API from the platform.
- Multi-region, custom domains, secrets management for the generated apps.

## Anti-goals

- No long-lived keys: no service-account JSON keys anywhere. Login from Actions is keyless (Workload Identity).
- The platform must not become a way for one repo to act as another: trust bindings are per repo.
- A webhook outage must never fail a deployment (notifications are best-effort).

## Constraints

- The platform runs on Cloud Run with a public URL (the user's choice), so GitHub Actions can call its webhook.
- The runtime image is currently `distroless/static`: it has no `git` and no `gcloud`. The platform shells
  out to `git` (template clone, push) so **the runtime image must be changed to include `git` and CA
  certificates** (e.g. `debian:12-slim`, still non-root). `gcloud` is not needed in the platform: GCP setup
  is done through REST APIs.
- The orchestrator runs each project in a background goroutine and holds the approval gate in memory.
  On Cloud Run that only works if the platform service is configured with CPU always allocated
  (`run.googleapis.com/cpu-throttling: "false"`), `minScale: 1` and `maxScale: 1` (the current `service.yml`
  has min 0 / max 5, which would freeze the goroutine after the 202 and lose approvals). This costs a
  little for an always-on instance. It is a documented limit of this demo, not fixed here.
- GCP org policies (e.g. domain-restricted sharing) may block public invocation; the generated
  `service.yml` uses the `run.googleapis.com/invoker-iam-disabled: "true"` annotation to make services public.
- The existing per-project webhook secret scheme (`helper.WebhookSecretFor`) stays.

## Approaches considered

For how the platform calls Google Cloud:
1. **REST APIs from Go via Application Default Credentials (chosen).** Small hand-written client,
   `golang.org/x/oauth2/google` (already in go.mod), idempotent create-if-missing calls, long-running
   operations polled. Easy to test with a fake HTTP server. No big dependencies.
2. Shell out to `gcloud`: quick to write but needs the Cloud SDK in the image (hundreds of MB), text parsing.
3. Official client libraries: typed, but several large dependencies for ~8 calls.

For how status reaches the platform (decided earlier with the user): the workflow **posts to the platform's
webhook** at each stage (platform is public). Polling the Actions API is deferred. A watchdog covers lost messages.

## Design

### Modes (env)

| Mode | Trigger | Behaviour |
|---|---|---|
| repo-only | `REPO_ONLY=true` | stops after the repo exists (existing) |
| demo | `DEMO_TEMPLATE_REPO` set | template clone + mock tests + PR + approve + merge, ends `success` (existing) |
| deploy | `DEMO_TEMPLATE_REPO` and `GCP_PROJECT_ID` set | demo + real Cloud Run deployment (new) |

Order of precedence: repo-only > deploy > demo. The old per-user GCP integration path is left untouched
and is used only when neither demo nor deploy mode is on.

New env (deploy mode): `GCP_PROJECT_ID`, `GCP_REGION` (default `asia-southeast1`), `GCP_ARTIFACT_REPO`
(default `ai-apps`), `GCP_ALLOWED_GITHUB_OWNERS` (comma list; required; used as the Workload Identity provider's
attribute condition as defence in depth), `WEBHOOK_URL` (the platform's public URL + `/api/webhooks/github`),
`WEBHOOK_SECRET`, `DEPLOY_TIMEOUT` (default 15m).

### `Deployer` interface (`service/deploy.go`)

```go
type DeployTarget struct {
    ProjectID   string // platform project id
    ServiceName string // Cloud Run service / repo name, from ServiceName(name, id)
    RepoOwner   string
    RepoName    string
}

type Deployer interface {
    // Prepare makes GCP ready for this repo (idempotent) and returns the values
    // the deploy workflow needs. It is called before the repo is created.
    Prepare(ctx context.Context, t DeployTarget) (model.GCPCreds, error)
    // Trigger starts a deployment of the repo's default branch (manual / re-deploy).
    // Auto deployment needs no call: merging the PR pushes to main, which starts the workflow.
    Trigger(ctx context.Context, gh model.GitHubCreds, repoURL string) error
}
```

`model.GCPCreds` already holds exactly the values the workflow template renders (`ProjectID, Region,
ArtifactRepo, WIFProvider, ServiceAccount`), so `CreateRepo` and `RenderDeployWorkflow` keep their shape.
First implementation: `CloudRunDeployer` (Prepare via the GCP provisioner; Trigger via GitHub
`workflow_dispatch`).

### GCP provisioner (`service/gcp.go`)

An authenticated `http.Client` (ADC, `cloud-platform` scope). All calls are create-if-missing; long-running
operations are polled with a timeout.

- `EnsureBase` (cached after the first success): enable APIs (`run`, `artifactregistry`, `iam`,
  `iamcredentials`, `sts`, `cloudresourcemanager`); Artifact Registry docker repo; deployer service
  account `ai-deployer` with roles `run.admin`, `artifactregistry.writer`, and `iam.serviceAccountUser` on the
  dedicated role-less runtime account `ai-app-runtime` (the app runs as it, set in `service.yml`; added after review so apps
  never run as the platform's account); Workload Identity pool `ai-builder-pool` and OIDC provider `github`
  (issuer `https://token.actions.githubusercontent.com`, mapping `google.subject=assertion.sub`,
  `attribute.repository=assertion.repository`, `attribute.repository_owner=assertion.repository_owner`,
  condition `assertion.repository_owner in [<allowed owners>]`). Project IAM policy changes are
  read-modify-write with the etag and retried on conflict.
- `BindRepo(owner, repo)`: add `roles/iam.workloadIdentityUser` on the deployer service account for member
  `principalSet://iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/ai-builder-pool/attribute.repository/<owner>/<repo>`.
  The project number comes from Resource Manager.

### Repo creation = IaC

`CreateRepo` commits, with `[skip ci]` in the commit message so creating the repo never starts a deploy of an
empty repo: `.github/workflows/deploy.yml` (rendered from `templates/deploy.yml.tmpl`) and `service.yml`
(rendered from a new `templates/service.yml.tmpl`: Knative Service named `<ServiceName>`, annotations
`invoker-iam-disabled: "true"`, `minScale 0`, `maxScale 2`, port 8080, 256Mi/1 cpu, image placeholder).
`PushAndOpenPR` already restores `.github` from the base branch so generated code cannot change CI; it must
**also restore `service.yml`** for the same reason.

### Workflow (`templates/deploy.yml.tmpl`)

Triggers: `push` to main (paths-ignore `.github/**`) and `workflow_dispatch` (manual). Steps, each wrapped so a
failure posts `failed` with the stage name:
1. checkout; GCP auth (Workload Identity); setup gcloud; configure docker.
2. notify `building`; mock build (echo + short sleep); **real** `docker build` and push to Artifact Registry.
3. notify `testing`; mock tests (echo + short sleep).
4. notify `deploying`; substitute the image into `service.yml`; `gcloud run services replace service.yml`;
   read the service URL.
5. post `success` with `live_url`; on any failure post `failed` with `error_message`.
`notify` is best-effort (`curl ... || true`): a webhook problem never fails the deploy.

### Webhook and status model

Payloads: progress `{project_id, step}` with step in `building|testing|deploying`; final
`{project_id, status:"success", live_url}` or `{project_id, status:"failed", error_message}`.
Same per-project secret authentication as today. Rules: a progress event only applies while the project is
`deploying`, and only moves `current_step` forward in the order `merging < merged < building < testing < deploying` (the orchestrator writes `merged` right after the PR merge; only
merged projects can be re-deployed)
(late or repeated events are acknowledged and ignored). Final events behave as today (idempotent, conflicting
result rejected). New constants `StepBuilding`, `StepTesting`.
After the PR merge the orchestrator **leaves `current_step` as `merging`** (it no longer writes `deploying`
itself, which would race with the first webhook events). In deploy mode it does not end the project;
the webhook does. In demo mode it still ends `success` after the merge.

### Watchdog (`service/deploy_watchdog.go`)

A goroutine started in `main` in deploy mode: every minute, projects with `status=deploying` whose
`updated_at` is older than `DEPLOY_TIMEOUT` are set to `failed` ("no update from GitHub Actions for 15m") with a
compare-and-set on status. Every progress event bumps `updated_at`.

### Manual deploy API

`POST /api/projects/{id}/deploy` (api-key + user id, owner only): allowed when the project is `success`, or
`failed` at/after the merge step; moves it to `deploying/merging` with a compare-and-set, calls
`Deployer.Trigger`; if the trigger fails the project is set `failed`. Returns 202. 404 for other users'
projects, 409 when the state does not allow it.

### Demo template

`demo-template/` in this repo: a standalone Go module (own `go.mod`) with a tiny HTTP server on `$PORT`
(default 8080) that prints a hello page, and a `Dockerfile`. The user pushes it as its own public repo and
uses it as `DEMO_TEMPLATE_REPO`. (`ANZ_Tranning` itself is unsuitable: it needs a database to start.)

### Platform deployment changes

- `Dockerfile`: runtime image with `git` and CA certificates (non-root).
- `service.yml` (platform): `minScale 1`, `maxScale 1`, `cpu-throttling: "false"`, new env vars (secrets
  for `WEBHOOK_SECRET`; plain values for the rest).
- `scripts/grant-platform-roles.sh`: grants the platform's runtime service account the roles needed to
  create the above (`serviceusage.serviceUsageAdmin`, `iam.workloadIdentityPoolAdmin`,
  `iam.serviceAccountAdmin`, `resourcemanager.projectIamAdmin`, `artifactregistry.admin`, `run.admin`).
- First deploy of the platform has no URL yet, so `WEBHOOK_URL` is set by a second `gcloud run services update`.

### Isolation level (explicit limitation)

Each project gets its own repo, its own Cloud Run service (`<name>-<id8>`), image tag and a per-repo trust
binding. The deployer service account, GCP project, registry repo and runtime service account are shared, so a
malicious repo could redeploy another project's service. Accepted for the demo. The `Deployer` interface
allows a later per-project-account implementation (note: 100 service accounts per project by default).

## Testing (the user wants to run these from scripts)

Unit/integration tests run with `go test ./test` and need no cloud:
- GCP provisioner against a fake Google HTTP server: idempotency (second call changes nothing), correct IAM
  member for a repo, etag retry, long-running operation polling, errors surfaced.
- Workflow and `service.yml` rendering: stages and notify calls present, `workflow_dispatch` present, unsafe
  values rejected, `[skip ci]` commit and both files committed by `CreateRepo`; `PushAndOpenPR` restores `service.yml`.
- Webhook: progress events (auth, forward-only, ignored when not deploying), final events unchanged.
- Orchestrator in deploy mode with a fake `Deployer` (Prepare before CreateRepo, ends waiting for the webhook,
  does not set success itself); demo mode unchanged. Watchdog marks stale projects failed.
- Manual deploy endpoint: ownership, state rules, trigger failure.

Opt-in live tests (skipped unless env is set), each printing the steps like `TestLive_DemoFlow` does:
- `TestLive_GCPProvision` (`LIVE_GCP=1`, needs `gcloud auth application-default login` or a key with the roles):
  runs `EnsureBase` and `BindRepo` for a fake repo and prints what was created.
- `TestLive_Deploy` (`LIVE_GITHUB=1 LIVE_GCP=1`): real repo + PR + merge + Actions run + Cloud Run. It cannot
  receive the webhook locally, so it observes progress through the GitHub Actions API, then reads the Cloud Run
  service URL and requests it expecting HTTP 200.
- `TestLive_Platform` (`PLATFORM_URL`, `API_KEY`, `GITHUB_TOKEN`): drives the deployed platform over HTTP
  (create user, connect GitHub, create project, poll to `waiting_for_approval`, approve, poll to `success`,
  GET the `live_url`). This is the actual demo end to end.

## Left to the builder

Exact Google REST request/response shapes and IAM policy merge details; polling intervals and timeouts;
the content of the mock steps and hello page; wording of messages; file/package layout beyond the names above.

## Addendum (2026-11-25, after the first implementation)

- **Static identity mode:** `GCP_WIF_PROVIDER` + `GCP_SERVICE_ACCOUNT` (+ `GCP_PROJECT_ID`, `GCP_ARTIFACT_REPO`) select
  `StaticDeployer`, a second `Deployer` that creates nothing in GCP and hands the fixed values to the workflow. The
  dynamic `CloudRunDeployer` stays available when they are unset. The owner of the GCP project must make the provider and
  account trust the new repos (see `docs/deploy-setup.md`).
- **Template = this repo:** `DEMO_TEMPLATE_REPO` points at the platform's own repo; `demo-template/` was removed. Without
  database settings `main` starts a standalone status page so Cloud Run accepts the revision.
- **Demo test:** `TestLive_Deploy` (`LIVE_DEPLOY=1`) is the single full-workflow demo script.
