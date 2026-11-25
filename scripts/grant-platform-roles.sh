#!/usr/bin/env bash
# One-time setup: lets the platform's Cloud Run service account create the Google Cloud
# resources that generated apps need (Artifact Registry repo, deployer service account,
# Workload Identity pool + per-repo trust, Cloud Run permissions). Run it once, as a
# project owner, then deploy the platform. Safe to run again.
#
#   ./scripts/grant-platform-roles.sh <gcp-project-id> [platform-service-account-email]
#
# The platform account defaults to the project's default compute service account, which
# is what service.yml runs as.
set -euo pipefail

PROJECT_ID="${1:-${PROJECT_ID:-}}"
if [ -z "$PROJECT_ID" ]; then
  echo "usage: $0 <gcp-project-id> [platform-service-account-email]" >&2
  exit 1
fi
NUMBER="$(gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)')"
PLATFORM_SA="${2:-${PLATFORM_SA:-${NUMBER}-compute@developer.gserviceaccount.com}}"

echo "Project:          $PROJECT_ID ($NUMBER)"
echo "Platform account: $PLATFORM_SA"

# the platform enables the other APIs itself, but needs these three to start
gcloud services enable serviceusage.googleapis.com cloudresourcemanager.googleapis.com \
  iam.googleapis.com --project "$PROJECT_ID"

ROLES=(
  roles/serviceusage.serviceUsageAdmin     # enable APIs
  roles/iam.workloadIdentityPoolAdmin      # pool + GitHub provider
  roles/iam.serviceAccountAdmin            # deployer account and its per-repo bindings
  roles/resourcemanager.projectIamAdmin    # deployer roles on the project
  roles/artifactregistry.admin             # image repository
  roles/run.admin                          # manual deploys / inspecting services
)
for role in "${ROLES[@]}"; do
  gcloud projects add-iam-policy-binding "$PROJECT_ID" \
    --member "serviceAccount:$PLATFORM_SA" --role "$role" --condition=None --quiet >/dev/null
  echo "granted $role"
done
echo "Done. Next: deploy the platform (see docs/deploy-setup.md)."
