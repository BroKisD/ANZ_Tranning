#!/usr/bin/env python3
"""Adds the platform's deploy-mode settings to service.yml before CI deploys it.

The values come from GitHub Actions secrets (passed in as environment variables), so
nothing is committed and nothing has to be set by hand on the Cloud Run service:

  required: GCP_PROJECT_ID GCP_REGION GCP_ARTIFACT_REPO GCP_WIF_PROVIDER GCP_SERVICE_ACCOUNT DEMO_TEMPLATE_REPO
            TEMPORAL_ADDRESS TEMPORAL_NAMESPACE   (Temporal Cloud endpoint and namespace)

ENCRYPTION_MASTER_KEY and TEMPORAL_API_KEY are read from the Secret Manager secrets of the same name
(like the other secrets in service.yml); they are referenced, never copied.

It also pins one always-on instance with CPU always allocated: the Temporal worker runs in this
process and must keep polling for work between requests.
Usage: render-service.py service.yml   (edits the file in place)
"""
import os
import sys

import yaml

REQUIRED = ["GCP_PROJECT_ID", "GCP_REGION", "GCP_ARTIFACT_REPO", "GCP_WIF_PROVIDER", "GCP_SERVICE_ACCOUNT", "DEMO_TEMPLATE_REPO",
            "TEMPORAL_ADDRESS", "TEMPORAL_NAMESPACE"]
SECRET_REFS = ["ENCRYPTION_MASTER_KEY", "TEMPORAL_API_KEY"]  # Secret Manager secrets, latest version


def main(path):
    missing = [k for k in REQUIRED if not os.environ.get(k, "").strip()]
    if missing:
        sys.exit("missing GitHub Actions secret(s): " + ", ".join(missing))

    with open(path) as f:
        doc = yaml.safe_load(f)
    tmpl = doc["spec"]["template"]
    container = tmpl["spec"]["containers"][0]

    names = REQUIRED + SECRET_REFS
    env = [e for e in container.get("env", []) if e.get("name") not in names]
    env += [{"name": k, "value": os.environ[k].strip()} for k in REQUIRED]
    env += [{"name": k, "valueFrom": {"secretKeyRef": {"name": k, "key": "latest"}}} for k in SECRET_REFS]
    container["env"] = env

    ann = tmpl.setdefault("metadata", {}).setdefault("annotations", {})
    ann["autoscaling.knative.dev/minScale"] = "1"
    ann["autoscaling.knative.dev/maxScale"] = "1"
    ann["run.googleapis.com/cpu-throttling"] = "false"

    with open(path, "w") as f:
        yaml.safe_dump(doc, f, sort_keys=False)
    print("service.yml: added %s" % ", ".join(names))  # names only, never values


if __name__ == "__main__":
    if len(sys.argv) != 2:
        sys.exit(__doc__)
    main(sys.argv[1])
