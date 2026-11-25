package service

import (
	"context"

	"anz-training/model"
)

const (
	deployWorkflowFile = "deploy.yml"
	deployRef          = defaultBase
)

// WorkflowDispatcher starts a GitHub Actions workflow by hand.
type WorkflowDispatcher interface {
	DispatchWorkflow(ctx context.Context, gh model.GitHubCreds, repoURL, workflowFile, ref string) error
}

// GCPBinder prepares Google Cloud for a repo (implemented by *GCP).
type GCPBinder interface {
	EnsureBase(ctx context.Context) error
	BindRepo(ctx context.Context, owner, repo string) (model.GCPCreds, error)
}

// CloudRunDeployer deploys generated apps to the platform's own Cloud Run
// project through the repo's GitHub Actions workflow.
type CloudRunDeployer struct {
	gcp  GCPBinder
	disp WorkflowDispatcher
}

func NewCloudRunDeployer(g GCPBinder, d WorkflowDispatcher) *CloudRunDeployer {
	return &CloudRunDeployer{gcp: g, disp: d}
}

// Prepare ensures the shared GCP setup exists, then trusts this one repo.
func (d *CloudRunDeployer) Prepare(ctx context.Context, t DeployTarget) (model.GCPCreds, error) {
	if err := d.gcp.EnsureBase(ctx); err != nil {
		return model.GCPCreds{}, err
	}
	return d.gcp.BindRepo(ctx, t.RepoOwner, t.RepoName)
}

// Trigger starts the deploy workflow on main (manual re-deploy).
func (d *CloudRunDeployer) Trigger(ctx context.Context, gh model.GitHubCreds, repoURL string) error {
	return d.disp.DispatchWorkflow(ctx, gh, repoURL, deployWorkflowFile, deployRef)
}
