# Step-by-Step Build Guide: AI App Builder (Demo)

Build an **AI Autonomous Software Factory (HITL)** using **Go (Goroutines + MySQL State Machine)**, **Claude Code**, **GitHub App / Actions**, **Secret Manager**, and **Google Cloud Run**.

---

## Step 1: Credential Architecture & User Onboarding

To avoid storing long-lived raw tokens in MySQL, combine **GitHub App (`installation_id`) + Secret Manager + GCP Workload Identity Federation (WIF)**:

1. **GitHub App (`provider = 'github'`)**:
   * **Onboarding:** User clicks *Connect GitHub* $\rightarrow$ installs your GitHub App on their account/org (`Administration`, `Contents`, `Pull requests`, `Secrets`, `Workflows` R/W).
   * **Runtime:** Store only `installation_id` and `owner` (not a long-lived PAT). Backend signs a JWT with the App Private Key to mint a 1-hour installation token in RAM per job.
2. **GCP Workload Identity Federation (`provider = 'gcp'`)**:
   * **Onboarding:** User binds GitHub OIDC (`https://token.actions.githubusercontent.com`) to a GCP Service Account (`roles/run.admin`, `roles/artifactregistry.writer`, `roles/iam.serviceAccountUser`) and enters 4 values: `project_id`, `region`, `wif_provider`, `service_account`.
   * **Runtime:** No static GCP JSON keys are stored; GitHub Actions exchanges OIDC tokens directly with GCP at deploy time.
3. **Secret Manager (Optional BYOK)**:
   * If users supply their own `ANTHROPIC_API_KEY`, store it in **GCP Secret Manager** and keep only the `secret_ref` path in `user_integrations`.

---

## Step 2: Database Setup (MySQL)

Run the DDL matching your schema:

```sql
CREATE DATABASE IF NOT EXISTS ai_factory CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci;
USE ai_factory;

CREATE TABLE users (
  id VARCHAR(36) PRIMARY KEY,
  name VARCHAR(255),
  email VARCHAR(255) UNIQUE NOT NULL,
  password_hash VARCHAR(255),
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE user_integrations (
  id VARCHAR(36) PRIMARY KEY,
  user_id VARCHAR(36) NOT NULL,
  provider VARCHAR(50) NOT NULL, -- 'github' | 'gcp'
  encrypted_credentials TEXT NOT NULL, -- AES-256-GCM encrypted JSON config/references
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  UNIQUE KEY uq_user_provider (user_id, provider),
  CONSTRAINT fk_integrations_user FOREIGN KEY (user_id) REFERENCES users(id) ON DELETE CASCADE
);

CREATE TABLE projects (
  id VARCHAR(36) PRIMARY KEY,
  owner_id VARCHAR(36) NOT NULL,
  name VARCHAR(255),
  prompt TEXT NOT NULL,
  status VARCHAR(50) NOT NULL DEFAULT 'pending', -- pending | generating | waiting_for_approval | deploying | success | failed
  current_step VARCHAR(100) NOT NULL DEFAULT 'initializing', -- initializing | repo_created | tests_passed | merging | deploying
  repo_url VARCHAR(500),
  branch_name VARCHAR(100) DEFAULT 'main',
  pr_url VARCHAR(500),
  live_url VARCHAR(500),
  error_message TEXT,
  created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
  updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
  INDEX idx_owner_status (owner_id, status),
  CONSTRAINT fk_projects_owner FOREIGN KEY (owner_id) REFERENCES users(id)
);
```

### `encrypted_credentials` Payloads (Encrypted with AES-256-GCM)
* **`github`**: `{"owner": "github-org-or-user", "installation_id": 12345678}` *(or short-lived demo PAT)*
* **`gcp`**: `{"project_id": "...", "region": "asia-southeast1", "artifact_repo": "ai-apps", "wif_provider": "projects/.../providers/...", "service_account": "deployer@....iam.gserviceaccount.com", "anthropic_secret_ref": "projects/.../secrets/.../versions/latest"}`

---

## Step 3: Project Structure (Go)

```text
ai-app-builder/
├── cmd/server/main.go               # HTTP router, DB & worker initialization
├── internal/
│   ├── crypto/aes.go                # AES-256-GCM encrypt/decrypt for user_integrations
│   ├── model/models.go              # User, UserIntegration, Project structs & enums
│   ├── repository/store.go          # MySQL CRUD for users, integrations, projects
│   ├── service/
│   │   ├── github.go                # Mint App installation token, create repo, commit workflow, PR & merge
│   │   ├── claude.go                # Run Claude Code CLI in isolated workspace + test/lint loop (max 3x)
│   │   └── orchestrator.go          # State machine goroutine + approval channel map
│   └── handler/
│       ├── auth_integration.go      # Auth + GitHub App callback + POST /api/integrations
│       ├── project.go               # POST /api/projects, GET /api/projects/:id, POST .../approve
│       └── webhook.go               # POST /api/webhooks/github
├── templates/deploy.yml.tmpl        # GitHub Actions Cloud Run deploy workflow
└── web/index.html                   # Demo Dashboard UI
```

---

## Step 4: Build Core Backend Modules

### 4.1. Onboarding & Crypto (`internal/crypto/aes.go` & `handler/auth_integration.go`)
* Implement `Encrypt` / `Decrypt` with `AES-256-GCM` using `ENCRYPTION_MASTER_KEY`.
* **GitHub Callback / Integration Endpoint**: Save `installation_id` & `owner` into `user_integrations` (`provider = 'github'`).
* **`POST /api/integrations`**: Encrypt and upsert GCP WIF config (`provider = 'gcp'`); if BYOK API key is provided, push to Secret Manager and store only `anthropic_secret_ref`. Never return decrypted credentials in `GET` responses.

### 4.2. GitHub Service (`internal/service/github.go`)
1. **`GetInstallationToken(installationID)`**: Sign JWT with GitHub App Private Key $\rightarrow$ exchange for 1-hour installation token in RAM.
2. **`CreateRepo(githubCreds, gcpCreds, projectID, projectName)`**:
   * Create private repo (`AutoInit: true` on `branch_name = 'main'`).
   * Render `templates/deploy.yml.tmpl` (injecting WIF provider, service account, webhook URL & secret) and commit `.github/workflows/deploy.yml` to `main`.
3. **`PushAndOpenPR(workspaceDir, githubCreds, baseBranch)`**: Push `feature/ai-gen` branch and open PR against `baseBranch` $\rightarrow$ return `pr_url`.
4. **`MergePR(githubCreds, repoURL, prURL)`**: Merge PR into `main` to trigger GitHub Actions.

### 4.3. Claude Code & Self-Correction Loop (`internal/service/claude.go`)
1. Create isolated workspace `/tmp/workspaces/<project_id>` (clean up after job).
2. Run `claude -p "<prompt + Cloud Run Dockerfile/PORT rules>" --dangerously-skip-permissions` with a hard `context.WithTimeout`.
3. Loop up to **3 times**:
   * Run stack linter & tests (`go vet ./... && go test ./...` or `npm test`).
   * If `exit_code == 0` $\rightarrow$ succeed; otherwise pass `stdout/stderr` back to `claude -p` to fix.

### 4.4. Orchestrator State Machine (`internal/service/orchestrator.go`)
Run each project in a goroutine with `approvalChans map[string]chan struct{}`:
1. **`status = 'generating'`, `current_step = 'initializing'`**: Decrypt `github` & `gcp` configs from `user_integrations` for `owner_id`.
2. **Create Repo**: `github.CreateRepo` $\rightarrow$ update `repo_url`, `current_step = 'repo_created'`.
3. **Generate & Test**: `claude.GenerateAndTest` $\rightarrow$ update `current_step = 'tests_passed'`.
4. **Open PR**: `github.PushAndOpenPR` $\rightarrow$ update `pr_url`, `status = 'waiting_for_approval'`.
5. **HITL Gate**: Block on `<-approvalChans[projectID]` (with timeout).
6. **Merge & Deploy**: On approve $\rightarrow$ `status = 'deploying'`, `current_step = 'merging'` $\rightarrow$ `github.MergePR` $\rightarrow$ `current_step = 'deploying'`.

### 4.5. API & Webhook Handlers (`internal/handler/`)
* **`POST /api/projects`**: Insert into `projects` (`status = 'pending'`, `current_step = 'initializing'`) $\rightarrow$ `go orchestrator.Run(project.ID)` $\rightarrow$ `202 Accepted`.
* **`GET /api/projects/{id}`**: Return `status`, `current_step`, `repo_url`, `pr_url`, `live_url`, `error_message`.
* **`POST /api/projects/{id}/approve`**: Check `status == 'waiting_for_approval'` $\rightarrow$ signal `approvalChans[id]`.
* **`POST /api/webhooks/github`**: Verify `X-Webhook-Secret` via `subtle.ConstantTimeCompare` $\rightarrow$ idempotently update `projects` (`status = 'success'`, `live_url` or `status = 'failed'`, `error_message`).

---

## Step 5: CI/CD Workflow Template (`templates/deploy.yml.tmpl`)

```yaml
name: Deploy to Cloud Run
on:
  push:
    branches: [ "{{ .BranchName }}" ]
permissions:
  contents: read
  id-token: write

jobs:
  deploy:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: google-github-actions/auth@v2
        with:
          workload_identity_provider: '{{ .WIFProvider }}'
          service_account: '{{ .ServiceAccount }}'
      - uses: google-github-actions/setup-gcloud@v2
      - run: gcloud auth configure-docker {{ .Region }}-docker.pkg.dev --quiet
      - id: deploy
        run: |
          IMAGE="{{ .Region }}-docker.pkg.dev/{{ .ProjectID }}/{{ .ArtifactRepo }}/{{ .ServiceName }}:${{ github.sha }}"
          docker build -t "$IMAGE" . && docker push "$IMAGE"
          gcloud run deploy {{ .ServiceName }} --image "$IMAGE" --region {{ .Region }} --platform managed --allow-unauthenticated --port 8080
          echo "url=$(gcloud run services describe {{ .ServiceName }} --region {{ .Region }} --format='value(status.url)')" >> $GITHUB_OUTPUT
      - if: success()
        run: |
          curl -s -X POST "{{ .WebhookURL }}" -H "Content-Type: application/json" -H "X-Webhook-Secret: {{ .WebhookSecret }}" \
            -d '{"project_id":"{{ .ProjectID }}","status":"success","live_url":"${{ steps.deploy.outputs.url }}"}'
      - if: failure()
        run: |
          curl -s -X POST "{{ .WebhookURL }}" -H "Content-Type: application/json" -H "X-Webhook-Secret: {{ .WebhookSecret }}" \
            -d '{"project_id":"{{ .ProjectID }}","status":"failed","error_message":"Cloud Run deployment failed"}'
```

---

## Step 6: Web UI (`web/index.html`)

1. **Onboarding / Settings**: *Connect GitHub App* button + form for the 4 GCP WIF fields (`POST /api/integrations`).
2. **Create App Form**: Inputs for `name` and `prompt` (`POST /api/projects`).
3. **Live Pipeline Tracker** (polls `GET /api/projects/{id}` every 2s):
   * Progress stepper: `initializing` $\rightarrow$ `repo_created` $\rightarrow$ `tests_passed` $\rightarrow$ `waiting_for_approval` $\rightarrow$ `merging` $\rightarrow$ `deploying` $\rightarrow$ `success`.
   * Show `repo_url`, show `pr_url` + **\[Approve & Deploy\]** at `waiting_for_approval`, and show clickable **`live_url`** at `success`.

---

## Step 7: Security Best Practices Checklist

* **Zero Long-Lived Raw Tokens in DB:** Use GitHub App `installation_id` (1-hr runtime tokens), Secret Manager references for API keys, and GCP WIF (no JSON keys).
* **Encryption & Redaction:** Encrypt `user_integrations.encrypted_credentials` with AES-256-GCM; redact secrets from all logs and API responses.
* **Constant-Time Webhook Auth:** Validate `X-Webhook-Secret` using `subtle.ConstantTimeCompare` and enforce idempotent status updates.
* **Sandbox Isolation:** Run Claude Code and test/lint commands in isolated per-project containers/directories with CPU/memory limits, `context.WithTimeout`, blocked cloud metadata IP (`169.254.169.254`), and `maxRetries = 3`.
* **Mandatory HITL Gate:** Never auto-merge to `main` without explicit user approval on the PR.