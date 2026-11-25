package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"anz-training/model"
)

// StaticDeployer deploys with a fixed, already configured Workload Identity
// provider and service account (for example the ones an existing CI/CD setup
// uses). It creates nothing in GCP: Prepare just hands the values to the repo's
// workflow. Whoever owns the GCP project must make sure that provider and
// account trust the repos the platform creates.
type StaticDeployer struct {
	creds model.GCPCreds
	disp  WorkflowDispatcher
}

func NewStaticDeployer(creds model.GCPCreds, d WorkflowDispatcher) (*StaticDeployer, error) {
	for name, v := range map[string]string{
		"project": creds.ProjectID, "region": creds.Region, "artifact repo": creds.ArtifactRepo,
		"workload identity provider": creds.WIFProvider, "service account": creds.ServiceAccount,
	} {
		if strings.TrimSpace(v) == "" {
			return nil, fmt.Errorf("static deployer: %s is required", name)
		}
	}
	return &StaticDeployer{creds: creds, disp: d}, nil
}

func (d *StaticDeployer) Prepare(context.Context, DeployTarget) (model.GCPCreds, error) {
	return d.creds, nil
}

func (d *StaticDeployer) Trigger(ctx context.Context, gh model.GitHubCreds, repoURL string) error {
	return d.disp.DispatchWorkflow(ctx, gh, repoURL, deployWorkflowFile, deployRef)
}

// StaticCredsFromEnv reads GCP_PROJECT_ID, GCP_REGION (default asia-southeast1),
// GCP_ARTIFACT_REPO, GCP_WIF_PROVIDER and GCP_SERVICE_ACCOUNT. The bool reports
// whether static mode is switched on (GCP_WIF_PROVIDER or GCP_SERVICE_ACCOUNT
// is set); a half-filled configuration is an error naming the missing variable.
func StaticCredsFromEnv(get func(string) string) (model.GCPCreds, bool, error) {
	c := model.GCPCreds{
		ProjectID: strings.TrimSpace(get("GCP_PROJECT_ID")), Region: strings.TrimSpace(get("GCP_REGION")),
		ArtifactRepo: strings.TrimSpace(get("GCP_ARTIFACT_REPO")),
		WIFProvider:  strings.TrimSpace(get("GCP_WIF_PROVIDER")), ServiceAccount: strings.TrimSpace(get("GCP_SERVICE_ACCOUNT")),
	}
	if c.WIFProvider == "" && c.ServiceAccount == "" {
		return model.GCPCreds{}, false, nil
	}
	if c.Region == "" {
		c.Region = "asia-southeast1"
	}
	for _, r := range []struct{ env, val string }{
		{"GCP_PROJECT_ID", c.ProjectID}, {"GCP_ARTIFACT_REPO", c.ArtifactRepo},
		{"GCP_WIF_PROVIDER", c.WIFProvider}, {"GCP_SERVICE_ACCOUNT", c.ServiceAccount},
	} {
		if r.val == "" {
			return model.GCPCreds{}, true, errors.New(r.env + " is required when GCP_WIF_PROVIDER / GCP_SERVICE_ACCOUNT are set")
		}
	}
	return c, true, nil
}
