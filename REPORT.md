# ANZ Training Backend — Demonstration Report

## 1. Executive summary

This repository is a cloud-native Go backend that exposes a small User API. It is designed for deployment to Google Cloud Run, persists data in MySQL/Cloud SQL, and uses GitHub Actions for automated quality checks and deployment.

The application follows a layered design:

```text
Client
  -> Cloud Run / Go HTTP API
       -> API-key middleware -> Handler -> Service -> Repository (GORM)
       -> Cloud SQL Auth Proxy Unix socket -> Cloud SQL for MySQL
```

The main API capabilities are:

- `POST /users`: creates a user from `email` and `name`.
- `GET /users?email=<email>`: retrieves a user by email.

Email input is trimmed and normalized to lowercase. MySQL enforces uniqueness of `users.email`.

## 2. Architecture and configuration

### Application layers

- **HTTP handler**: parses JSON, returns consistent JSON errors, and maps business errors to HTTP status codes.
- **Service**: validates and normalizes input, then applies business rules.
- **Repository**: isolates GORM/MySQL queries.
- **Database**: creates a bounded `database/sql` connection pool and validates connectivity during startup.

The database layer uses GORM on top of `database/sql`, with a maximum of 25 open and 25 idle connections and a five-minute connection lifetime. MySQL time values are parsed in UTC. Multi-statement queries and clear-text password authentication are intentionally not enabled.

### Cloud SQL connection

In Cloud Run, the `run.googleapis.com/cloudsql-instances` annotation mounts the Cloud SQL Auth Proxy socket. When `INSTANCE_CONNECTION_NAME` is configured, the app connects through:

```text
${DB_SOCKET_DIR:-/cloudsql}/${INSTANCE_CONNECTION_NAME}
```

For local development, it falls back to TCP using `DB_HOST` and `DB_PORT`.

### Secrets and IAM

Cloud Run receives `DB_USER`, `DB_PASSWORD`, `DB_NAME`, `INSTANCE_CONNECTION_NAME`, `DB_SOCKET_DIR`, and `API_KEY` from Secret Manager references in `service.yml`. No API key is embedded in source code.

The runtime service account should have only these permissions:

- `roles/cloudsql.client` for the selected Cloud SQL instance.
- `roles/secretmanager.secretAccessor` on the six referenced secrets.

The GitHub Actions service account should separately have only the permissions required to push to Artifact Registry and update this Cloud Run service. Authentication uses GitHub OIDC Workload Identity Federation instead of a long-lived service-account key.

## 3. Security and reliability controls

- All business endpoints require an `x-api-key` header; missing or incorrect values return `401`.
- API-key comparison uses a constant-time comparison.
- The service refuses to start if `API_KEY` is absent.
- Request JSON is limited to 1 MiB and rejects unknown fields or multiple JSON values.
- The database's unique index is the source of truth for duplicate emails. MySQL duplicate-key error `1062` is converted to `409 Conflict`, preventing a race caused by a separate “check then insert” operation.
- The production image is a multi-stage build that runs a statically compiled binary as a non-root user in a distroless runtime image.
- Cloud Run is configured with 1 vCPU, 512 MiB memory, concurrency 80, timeout 300 seconds, and autoscaling between 0 and 5 instances.

## 4. Database migrations

Schema changes use versioned SQL migrations managed by Goose and embedded in the application binary. Supported commands are:

```bash
go run . migrate status
go run . migrate up
go run . migrate down-to <version>
```

`serve` currently runs pending migrations before it accepts requests. For a multi-instance production rollout, migrations should be run as a distinct, single-run release step before deploying a new Cloud Run revision; this avoids coupling schema changes to autoscaling startup.

## 5. Testing

- Unit tests cover handler status/error behavior and the duplicate-email race/error mapping.
- Integration tests start an isolated MySQL 8 container using Testcontainers and exercise create, retrieval, normalization, and duplicate-email behavior through HTTP.

Run locally:

```bash
go test ./...
go test -tags=integration ./...
```

The integration suite requires Docker. When Docker is unavailable, Testcontainers marks that suite as skipped.

## 6. CI/CD delivery flow

For every pull request, GitHub Actions performs:

1. Go module setup with cache.
2. `go vet ./...`.
3. Unit tests and Testcontainers integration tests.
4. Docker image build.

Only a push to `main` authenticates to Google Cloud, pushes the image to Artifact Registry, validates `service.yml`, and deploys the Cloud Run service. The deployed image is tagged with the immutable Git commit SHA rather than `latest`, which makes rollback and audit trails deterministic.

## 7. Local demonstration

Create a local `.env` file (do not commit it):

```dotenv
DB_NAME=anz_training
DB_USER=anz_user
DB_PASSWORD=replace-with-a-local-password
API_KEY=replace-with-a-long-random-local-key
```

Then start the stack and call the API:

```bash
docker compose up --build

curl -i -X POST http://localhost:9090/users \
  -H 'Content-Type: application/json' \
  -H 'x-api-key: replace-with-a-long-random-local-key' \
  --data '{"email":"ada@example.com","name":"Ada Lovelace"}'

curl -i 'http://localhost:9090/users?email=ada@example.com' \
  -H 'x-api-key: replace-with-a-long-random-local-key'

```

## 8. Remaining production recommendations

- Run migrations as a dedicated deployment job instead of on every application startup.
- Add Cloud Monitoring alerts, JSON logs with request IDs, metrics, and distributed tracing.
- Add rate limiting and consider replacing a shared API key with an identity-aware authentication mechanism such as IAP, JWT/OIDC, or an API gateway.
- Define a staging environment and require an approval gate before production deployment.
- Add an explicit Cloud Run startup/readiness probe configuration after validating it against the target Cloud Run API version.
