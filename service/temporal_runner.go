package service

import (
	"context"
	"errors"
	"time"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"anz-training/model"
)

const runnerCallTimeout = 10 * time.Second

// WorkflowClient is the part of the Temporal client the runner uses
// (client.Client satisfies it).
type WorkflowClient interface {
	ExecuteWorkflow(ctx context.Context, options client.StartWorkflowOptions, workflow interface{}, args ...interface{}) (client.WorkflowRun, error)
	SignalWorkflow(ctx context.Context, workflowID, runID, signalName string, arg interface{}) error
}

// TemporalRunner is what the HTTP handlers call (handler.ProjectRunner): it starts
// and signals workflows. The workflow ID is always the project ID.
type TemporalRunner struct {
	client   WorkflowClient
	store    ProjectStore
	cfg      WorkflowConfig
	deployer Deployer // nil unless deploy mode
}

func NewTemporalRunner(c WorkflowClient, store ProjectStore, cfg WorkflowConfig, deployer Deployer) *TemporalRunner {
	return &TemporalRunner{client: c, store: store, cfg: cfg, deployer: deployer}
}

func (r *TemporalRunner) execute(ctx context.Context, projectID string, workflow any) error {
	_, err := r.client.ExecuteWorkflow(ctx, client.StartWorkflowOptions{
		ID: projectID, TaskQueue: TaskQueueProjects,
		// without this the SDK hides an already-running workflow behind a nil error
		WorkflowExecutionErrorWhenAlreadyStarted: true,
	}, workflow, ProjectInput{ProjectID: projectID, Config: r.cfg})
	var started *serviceerror.WorkflowExecutionAlreadyStarted
	if errors.As(err, &started) {
		return ErrAlreadyRunning
	}
	return err
}

// Start begins the project's run in the background. A run that is already open
// for this project is ErrAlreadyRunning.
func (r *TemporalRunner) Start(projectID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), runnerCallTimeout)
	defer cancel()
	return r.execute(ctx, projectID, ProjectWorkflow)
}

// Approve releases the approval gate of a project waiting for approval. It is
// idempotent, and reports not-found for projects the caller does not own.
func (r *TemporalRunner) Approve(ctx context.Context, projectID, ownerID string) error {
	p, err := r.store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if p.OwnerID != ownerID {
		return model.ErrNotFound
	}
	if p.Status != model.StatusWaitingForApproval {
		return ErrNotWaiting
	}
	err = r.client.SignalWorkflow(ctx, projectID, "", SignalApprove, nil)
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return ErrNoPendingRun // e.g. the run already ended (approval timed out)
	}
	return err
}

// Redeploy starts a manual deployment of a project that already has merged code:
// it must be success, or failed after the PR was merged. The compare-and-set to
// deploying happens in the workflow's first activity, so a rejected start leaves
// the project untouched. It reports not-found for projects the caller does not own.
func (r *TemporalRunner) Redeploy(ctx context.Context, projectID, ownerID string) error {
	if r.deployer == nil {
		return ErrNotRedeployable
	}
	p, err := r.store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if p.OwnerID != ownerID {
		return model.ErrNotFound
	}
	okState := p.Status == model.StatusSuccess ||
		(p.Status == model.StatusFailed && StepRank(p.CurrentStep) >= StepRank(model.StepMerged))
	if !okState || p.RepoURL == "" {
		return ErrNotRedeployable
	}
	return r.execute(ctx, projectID, RedeployWorkflow)
}

// SignalDeploy tells the project's workflow about a deploy event (the webhook
// calls it). A workflow that already finished is not an error.
func (r *TemporalRunner) SignalDeploy(ctx context.Context, projectID, signal string) error {
	err := r.client.SignalWorkflow(ctx, projectID, "", signal, nil)
	var missing *serviceerror.NotFound
	if errors.As(err, &missing) {
		return nil
	}
	return err
}
