# Deploying generated apps to Cloud Run

In **deploy mode** every project becomes a GitHub repo, a pull request and, after approval and merge, a real
Cloud Run service in this GCP project, built and deployed by GitHub Actions in the new repo.

The "generated" app is a **clone of this repo** (`DEMO_TEMPLATE_REPO`). Deployed without database settings
the app starts in *standalone mode* (a small status page on `/`, `/healthz`), so it passes Cloud Run's health
check. The clone comes from GitHub, so **push your changes to `main` of that repo first** and keep it public
(the clone does not authenticate).

## The easy way: run the whole demo from one test

```bash
LIVE_DEPLOY=1 GITHUB_TOKEN=ghp_... go test ./test -run TestLive_Deploy -v -timeout 20m
```

It prints every step: request saved, repo created, code cloned and mock-tested, PR opened, approval, merge,
each GitHub Actions step (mock build, docker build + push, mock tests, Cloud Run deploy), and finally
`GET <live url> -> 200`. Defaults: this repo as the code, project `project-9991dcfb-85ec-4182-b68`, registry
`anz-repo`, and the existing `github-provider` / `anz-trainning` identity. Override with `GCP_*`,
`DEMO_TEMPLATE_REPO`, `LIVE_OWNER`.

The GitHub token needs the **`repo` and `workflow`** scopes (`workflow` is needed to commit the deploy
workflow into the new repo).

## One-time: let the existing identity trust the new repos

The workflow logs in to Google with `GCP_WIF_PROVIDER` as `GCP_SERVICE_ACCOUNT`. Your provider and account
were set up for one repo; new repos are rejected at the "Google login" step until both trust your GitHub account.
Look at what is there first (the commands below replace settings, so keep what your CI already relies on):

```bash
P=project-9991dcfb-85ec-4182-b68
gcloud iam workload-identity-pools providers describe github-provider --project=$P --location=global \
  --workload-identity-pool=github-pool --format="yaml(attributeMapping,attributeCondition)"
gcloud iam service-accounts get-iam-policy anz-trainning@$P.iam.gserviceaccount.com
```

Then allow every repo of your account (this sets the mapping and condition; add any mapping the describe showed):

```bash
gcloud iam workload-identity-pools providers update-oidc github-provider --project=$P --location=global \
  --workload-identity-pool=github-pool \
  --attribute-mapping="google.subject=assertion.sub,attribute.repository=assertion.repository,attribute.repository_owner=assertion.repository_owner" \
  --attribute-condition="assertion.repository_owner=='BroKisD'"

gcloud iam service-accounts add-iam-policy-binding anz-trainning@$P.iam.gserviceaccount.com \
  --role=roles/iam.workloadIdentityUser \
  --member="principalSet://iam.googleapis.com/projects/386127327935/locations/global/workloadIdentityPools/github-pool/attribute.repository_owner/BroKisD"
```

The account `anz-trainning@...` already needs Artifact Registry write on `anz-repo` and Cloud Run deploy rights
(your `ci-cd.yml` uses it for exactly that). Apps run as the project's default compute account.

## The platform on Cloud Run

`service.yml` is the platform's own Cloud Run spec (the existing `ci-cd.yml` deploys it on push to `main`).
Create the secret it reads: `openssl rand -hex 32 | gcloud secrets create WEBHOOK_SECRET --data-file=-` and let the
platform's service account read it.

| Env var | Meaning |
|---|---|
| `DEMO_TEMPLATE_REPO` + `GCP_PROJECT_ID` | together they switch on **deploy mode** |
| `GCP_WIF_PROVIDER`, `GCP_SERVICE_ACCOUNT`, `GCP_ARTIFACT_REPO`, `GCP_REGION` | the fixed deploy identity ("static" mode; region defaults to `asia-southeast1`) |
| `WEBHOOK_URL` | `https://<service>-<project number>.<region>.run.app/api/webhooks/github` (the URL is deterministic) |
| `WEBHOOK_SECRET` | root secret; each repo gets a key derived from it, valid for that repo only |
| `DEPLOY_TIMEOUT` | default `15m`: a deploy silent for this long is marked failed |

Without `GCP_PROJECT_ID` the platform stays in demo mode (clone, mock tests, merge, no deploy); `REPO_ONLY=true`
stops after the repo is created.

**Why `service.yml` pins one instance with CPU always on:** each project runs in a background goroutine and the
approval wait lives in memory, so default Cloud Run settings (CPU only during requests, several instances)
would freeze runs and lose approvals. It costs one idle instance.

## Use it (API)

`POST /api/projects` -> `GET /api/projects/{id}` until `waiting_for_approval` -> `POST /api/projects/{id}/approve`.
After the merge the status stays `deploying` while `current_step` moves `merged -> building -> testing -> deploying`,
then `success` with `live_url`. `POST /api/projects/{id}/deploy` re-deploys a finished project.
`TestLive_Platform` drives a deployed platform this way (`PLATFORM_URL`, `API_KEY`, `GITHUB_TOKEN`).

## Alternative: let the platform create the GCP setup

Leave `GCP_WIF_PROVIDER` / `GCP_SERVICE_ACCOUNT` unset and set `GCP_ALLOWED_GITHUB_OWNERS` instead. The platform then
creates its own registry, deployer account, a role-less runtime account, a Workload Identity pool and one trust binding
per repo. It needs the roles from `scripts/grant-platform-roles.sh`; `TestLive_GCPProvision` runs just that part.

## Known limits

- **Isolation:** every app gets its own repo and its own Cloud Run service, but all share the deployer account. In
  static mode that is `anz-trainning@...`, which also deploys the platform: a workflow in any trusted repo can replace
  any service in the project, including the platform. Fine while the only trusted account is yours; for anything shared,
  deploy the apps into a separate GCP project. In static mode apps also run as the default compute account.
- Deleting a project does not delete its repo, service or IAM binding.
- Two deploys of one repo never run at once (`concurrency` cancels the older run).
- A deploy silent for `DEPLOY_TIMEOUT` is marked failed; if its run then really succeeds, the success still wins.
