# Using Temporal Cloud (for the Cloud Run deployment)

Locally, `docker compose` runs a Temporal dev server. Cloud Run has no Temporal server,
so the deployed service connects to **Temporal Cloud** instead. The app picks the server
from environment variables (`temporal_conn.go`); nothing else in the code changes.

| Variable | Local / compose | Cloud Run |
|---|---|---|
| `TEMPORAL_ADDRESS` | unset (`localhost:7233`) or `temporal:7233` | your Temporal Cloud regional endpoint, e.g. `<region>.<cloud>.api.temporal.io:7233` |
| `TEMPORAL_NAMESPACE` | unset (`default`) | `<namespace>.<account id>` |
| `TEMPORAL_API_KEY` | unset (plain gRPC) | a Temporal Cloud API key (turns on TLS) |

`TEMPORAL_API_KEY` without the other two is refused at startup, with a message naming what is missing.

## One-time setup

1. **Temporal Cloud** (cloud.temporal.io): create an account, a **namespace**, and an **API key**.
   Note the namespace (`<name>.<account id>`) and the regional endpoint shown for API-key access.
2. **Secret Manager**: store the API key, and let the Cloud Run service account read it.
   ```bash
   read -rs KEY            # paste the key; it is not echoed or kept in shell history
   printf '%s' "$KEY" | gcloud secrets create TEMPORAL_API_KEY --data-file=- --project=<gcp-project>
   gcloud secrets add-iam-policy-binding TEMPORAL_API_KEY --project=<gcp-project> \
     --member="serviceAccount:<the serviceAccountName in service.yml>" --role=roles/secretmanager.secretAccessor
   unset KEY
   ```
3. **GitHub Actions secrets** (repo settings, or `gh secret set NAME`): `TEMPORAL_ADDRESS` and `TEMPORAL_NAMESPACE`.
   They are not very sensitive, but they live next to the other deploy settings.

`scripts/render-service.py` adds all three to `service.yml` at deploy time and fails with
`missing GitHub Actions secret(s): ...` if step 3 is not done. It also keeps the service at one
always-on instance with CPU always allocated, because the Temporal worker runs inside the app and
must keep polling between requests.

## Try it locally against Temporal Cloud first

```bash
export TEMPORAL_ADDRESS=<endpoint>:7233 TEMPORAL_NAMESPACE=<ns>.<acct>
read -rs TEMPORAL_API_KEY && export TEMPORAL_API_KEY
make run
```
Workflows then show up in the Temporal Cloud web UI (your namespace), not on http://localhost:8233.
Run the checklist in `docs/temporal.md` against it before merging to `main`.

## Rollout order

`main` deploys automatically on every push. Do steps 1 to 3 first, try it locally, and only then merge.
If a setting is missing the deploy fails at the render step (clear message) or the revision does not
start; the previous Cloud Run revision keeps serving.

Not verified: this was written without access to a Temporal Cloud account or GCP. Check the endpoint
format and API-key steps against Temporal's current documentation.
