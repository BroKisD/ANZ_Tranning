# Running projects on Temporal

Every project run is a Temporal workflow. The app (`serve`) hosts the worker, and
Temporal itself runs as a container next to it.

## Start it

```bash
make up                      # db + temporal + app
# Web UI: http://localhost:8233   (namespace "default")
# gRPC:   localhost:7233          (app uses TEMPORAL_ADDRESS, default localhost:7233)
```

Running the app outside Docker: `docker compose up -d db temporal`, then run
`serve` with `TEMPORAL_ADDRESS=localhost:7233` (the default).

## What runs where

| Piece | Where | Does |
|---|---|---|
| `ProjectWorkflow` | queue `anz-projects` | claim, create repo, generate + PR, wait for approval, merge, wait for the deploy result |
| `RedeployWorkflow` | queue `anz-projects` | claim a redeploy, trigger it, wait for the deploy result |
| Activities (`service/activities.go`) | `anz-projects` | all GitHub / MySQL / deployer calls; credentials are read inside the activity, never passed through the workflow |
| `GenerateAndOpenPR` | queue `anz-generate` | generate, test, push, open the PR; worker concurrency = 2 parallel jobs |

Workflow ID = project ID. Signals: `approve` (the approve endpoint), `deploy-progress`
and `deploy-result` (the webhook, after it has written MySQL). MySQL stays the read model
for `GET /api/projects/{id}`.

Timers: approval waits 24h; the deploy wait is an inactivity timer (`DEPLOY_TIMEOUT`) that
every `deploy-progress` signal restarts. In polling mode (no webhook) one polling activity
follows the run through the GitHub API instead.

## Manual checklist

The workflow itself has no automated tests (ordering, timers and signals are checked by
running it). Run these against the compose stack and watch each run in the Web UI.
Tick an item only after you saw it.

- [ ] 1. **Happy path (demo mode).** The workflow runs all activities in order; the project ends `success`.
- [ ] 2. **Approval gate.** `POST /api/projects/{id}/approve` makes the waiting workflow continue; a second approve is harmless.
- [ ] 3. **Approval timeout.** Run with a very short approval timeout: the project ends `failed` with `approval timed out`, and approve then returns `no pending run for this project`.
- [ ] 4. **Deploy wait (deploy mode).** Send webhook `building`, then `success`: the workflow history shows the signals and completes; the project is `success`.
- [ ] 5. **Deploy inactivity.** Send no webhook: after `DEPLOY_TIMEOUT` the project is `failed` with a `no update from GitHub Actions` message; a late `success` webhook still recovers it.
- [ ] 6. **Restart mid-run.** Stop the app while a project waits for approval, restart it, approve: the run continues. In the Web UI, search the workflow history for the GitHub token: it must not appear anywhere.
- [ ] 7. **Redeploy.** `POST /api/projects/{id}/deploy` on a successful project starts `RedeployWorkflow` with the same workflow ID.
- [ ] 8. **Repeat safety.** Retry an activity that already created the repo/PR/merge (e.g. reset the worker mid-activity): no "already exists" failure.

## Known limits

- Projects that were mid-run before this was deployed have no workflow (the old goroutine
  run was lost on restart too): approve returns `no pending run`.
- The dev server stores history in a file on the `temporal_data` volume; it is for training,
  not production.
