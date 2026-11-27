package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"

	"anz-training/model"
)

// heartbeatEvery is how often a long activity reports that it is alive.
const heartbeatEvery = 10 * time.Second

// Activities holds the collaborators of every workflow activity. An activity
// loads and decrypts the credentials it needs itself from the project's owner,
// so no secret ever appears in an activity input, a result or workflow history.
type Activities struct {
	Store        ProjectStore
	Integrations IntegrationReader
	GitHub       RepoHost
	Gen          CodeGenerator
	Secrets      SecretReader // may be nil: BYOK projects then fail
	Deployer     Deployer     // nil unless deploy mode
	Monitor      *DeployMonitor
	MasterKey    []byte
	// Heartbeat reports liveness from long activities. Production sets it to
	// activity.RecordHeartbeat; nil (unit tests) is a no-op.
	Heartbeat func(ctx context.Context, details ...any)
}

// Inputs. They hold ids and flags only, never credentials.
type (
	CreateRepoInput struct {
		ProjectID string
		NoGCP     bool // repo-only, demo or deploy mode: only the GitHub integration is needed
		Deploy    bool // deploy mode: Prepare the platform's GCP first
	}
	GenerateInput struct {
		ProjectID string
		NoGCP     bool
	}
	UpdateInput struct {
		ProjectID string
		Fields    map[string]any
	}
	WatchInput struct {
		ProjectID string
		Since     time.Time
	}
	FailInput struct {
		ProjectID string
		Message   string
	}
)

// asActivityError makes a configuration problem non-retryable and leaves every
// other error retryable.
func asActivityError(err error) error {
	var ce configError
	if errors.As(err, &ce) {
		return temporal.NewNonRetryableApplicationError(err.Error(), "ConfigError", err)
	}
	return err
}

func (a *Activities) beat(ctx context.Context, details ...any) {
	if a.Heartbeat != nil {
		a.Heartbeat(ctx, details...)
	}
}

// keepAlive heartbeats until the returned stop function is called.
func (a *Activities) keepAlive(ctx context.Context) (stop func()) {
	done := make(chan struct{})
	go func() {
		t := time.NewTicker(heartbeatEvery)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-ctx.Done():
				return
			case <-t.C:
				a.beat(ctx)
			}
		}
	}()
	return func() { close(done) }
}

func (a *Activities) creds(ctx context.Context, ownerID string, noGCP bool) (model.GitHubCreds, model.GCPCreds, error) {
	var gh model.GitHubCreds
	var gcp model.GCPCreds
	if err := loadIntegration(ctx, a.Integrations, a.MasterKey, ownerID, model.ProviderGitHub, &gh); err != nil {
		return gh, gcp, err
	}
	if !noGCP {
		if err := loadIntegration(ctx, a.Integrations, a.MasterKey, ownerID, model.ProviderGCP, &gcp); err != nil {
			return gh, gcp, err
		}
	}
	return gh, gcp, nil
}

// ClaimProject moves pending to generating. false means another run owns it.
func (a *Activities) ClaimProject(ctx context.Context, id string) (bool, error) {
	return a.Store.UpdateProjectIf(ctx, id, model.StatusPending, map[string]any{
		"status": model.StatusGenerating, "current_step": model.StepInitializing,
	})
}

// PrepareAndCreateRepo prepares the deploy target (deploy mode), creates the
// repo and records it. Prepare runs here, not in its own activity, because its
// result (GCP settings) feeds CreateRepo and must not enter workflow history.
func (a *Activities) PrepareAndCreateRepo(ctx context.Context, in CreateRepoInput) (string, error) {
	p, err := a.Store.GetProject(ctx, in.ProjectID)
	if err != nil {
		return "", fmt.Errorf("load project: %w", err)
	}
	gh, gcp, err := a.creds(ctx, p.OwnerID, in.NoGCP)
	if err != nil {
		return "", asActivityError(err)
	}
	if in.Deploy {
		name := ServiceName(p.Name, p.ID)
		if gcp, err = a.Deployer.Prepare(ctx, DeployTarget{ProjectID: p.ID, ServiceName: name, RepoOwner: gh.Owner, RepoName: name}); err != nil {
			return "", fmt.Errorf("prepare deployment: %w", err)
		}
	}
	repoURL, err := a.GitHub.CreateRepo(ctx, gh, gcp, p.ID, p.Name)
	if err != nil {
		return "", fmt.Errorf("create repo: %w", err)
	}
	if err := a.Store.UpdateProject(ctx, p.ID, map[string]any{"repo_url": repoURL, "current_step": model.StepRepoCreated}); err != nil {
		return "", err
	}
	return repoURL, nil
}

// GenerateAndOpenPR generates and tests the code, pushes it and opens the PR,
// and always removes the workspace. It runs on the generate task queue, whose
// worker concurrency limit replaces the old in-process semaphore. Returns the PR URL.
func (a *Activities) GenerateAndOpenPR(ctx context.Context, in GenerateInput) (string, error) {
	p, err := a.Store.GetProject(ctx, in.ProjectID)
	if err != nil {
		return "", fmt.Errorf("load project: %w", err)
	}
	gh, gcp, err := a.creds(ctx, p.OwnerID, in.NoGCP)
	if err != nil {
		return "", asActivityError(err)
	}
	apiKey := ""
	if !in.NoGCP {
		if apiKey, err = resolveAPIKey(ctx, a.Secrets, gcp); err != nil {
			return "", asActivityError(err)
		}
	}

	defer func() { _ = a.Gen.Cleanup(p.ID) }() // success, push failure or half-written workspace
	defer a.keepAlive(ctx)()                   // heartbeat through generation and the push
	dir, err := a.Gen.GenerateAndTest(ctx, GenerateRequest{ProjectID: p.ID, Prompt: p.Prompt, AnthropicAPIKey: apiKey})
	if err != nil {
		return "", fmt.Errorf("generate: %w", err)
	}
	if err := a.Store.UpdateProject(ctx, p.ID, map[string]any{"current_step": model.StepTestsPassed}); err != nil {
		return "", err
	}

	base := p.BranchName
	if base == "" {
		base = "main"
	}
	prURL, err := a.GitHub.PushAndOpenPR(ctx, dir, gh, p.RepoURL, base)
	if err != nil {
		return "", fmt.Errorf("open pull request: %w", err)
	}
	return prURL, nil
}

// UpdateProject writes status fields (the workflow's own state changes).
func (a *Activities) UpdateProject(ctx context.Context, in UpdateInput) error {
	return a.Store.UpdateProject(ctx, in.ProjectID, in.Fields)
}

// MergePR merges the stored PR and records the merge. It returns the time to
// look for the deploy run from (the merge time minus a clock-skew margin).
func (a *Activities) MergePR(ctx context.Context, id string) (time.Time, error) {
	p, err := a.Store.GetProject(ctx, id)
	if err != nil {
		return time.Time{}, fmt.Errorf("load project: %w", err)
	}
	gh, _, err := a.creds(ctx, p.OwnerID, true)
	if err != nil {
		return time.Time{}, asActivityError(err)
	}
	mergeAt := time.Now().Add(-runClockSkew)
	if err := a.GitHub.MergePR(ctx, gh, p.RepoURL, p.PRURL); err != nil {
		return time.Time{}, fmt.Errorf("merge pull request: %w", err)
	}
	// Record that the merge happened: only a merged project can be re-deployed.
	// Skipped when a webhook event already moved the step on (never overwritten).
	if cur, err := a.Store.GetProject(ctx, id); err == nil && cur.Status == model.StatusDeploying && cur.CurrentStep == model.StepMerging {
		if _, err := a.Store.UpdateProjectIf(ctx, id, model.StatusDeploying, map[string]any{"current_step": model.StepMerged}); err != nil {
			return time.Time{}, err
		}
	}
	return mergeAt, nil
}

// WatchDeploy follows the deploy run on GitHub (polling mode) and records the
// result itself, exactly as DeployMonitor always did.
func (a *Activities) WatchDeploy(ctx context.Context, in WatchInput) error {
	if a.Monitor == nil {
		return temporal.NewNonRetryableApplicationError("deploy monitor is not configured", "ConfigError", nil)
	}
	p, err := a.Store.GetProject(ctx, in.ProjectID)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}
	gh, _, err := a.creds(ctx, p.OwnerID, true)
	if err != nil {
		return asActivityError(err)
	}
	stop := a.keepAlive(ctx)
	defer stop()
	a.Monitor.Watch(ctx, in.ProjectID, gh, p.RepoURL, in.Since)
	return ctx.Err() // cancelled or timed out by Temporal: let it retry or fail the activity
}

// ClaimRedeploy starts a manual redeploy: the project must be success, or
// failed after the merge. Compare-and-set, so two requests start only one.
func (a *Activities) ClaimRedeploy(ctx context.Context, id string) (bool, error) {
	if a.Deployer == nil {
		return false, nil
	}
	p, err := a.Store.GetProject(ctx, id)
	if err != nil {
		return false, fmt.Errorf("load project: %w", err)
	}
	okState := p.Status == model.StatusSuccess ||
		(p.Status == model.StatusFailed && StepRank(p.CurrentStep) >= StepRank(model.StepMerged))
	if !okState || p.RepoURL == "" {
		return false, nil
	}
	return a.Store.UpdateProjectIf(ctx, id, p.Status, map[string]any{
		"status": model.StatusDeploying, "current_step": model.StepMerged, "error_message": "",
	})
}

// TriggerDeploy dispatches the deploy workflow and returns the time to look for
// its run from.
func (a *Activities) TriggerDeploy(ctx context.Context, id string) (time.Time, error) {
	p, err := a.Store.GetProject(ctx, id)
	if err != nil {
		return time.Time{}, fmt.Errorf("load project: %w", err)
	}
	gh, _, err := a.creds(ctx, p.OwnerID, true)
	if err != nil {
		return time.Time{}, asActivityError(err)
	}
	at := time.Now().Add(-runClockSkew)
	if err := a.Deployer.Trigger(ctx, gh, p.RepoURL); err != nil {
		return time.Time{}, fmt.Errorf("trigger deployment: %w", err)
	}
	return at, nil
}

// FailDeploy fails a project that is still deploying (the deploy went silent).
// A project that already finished is left alone.
func (a *Activities) FailDeploy(ctx context.Context, in FailInput) error {
	_, err := a.Store.UpdateProjectIf(ctx, in.ProjectID, model.StatusDeploying, map[string]any{
		"status": model.StatusFailed, "error_message": truncateMessage(in.Message),
	})
	return err
}

// MarkFailed records a failed run.
func (a *Activities) MarkFailed(ctx context.Context, in FailInput) error {
	return a.Store.UpdateProject(ctx, in.ProjectID, map[string]any{
		"status": model.StatusFailed, "error_message": truncateMessage(in.Message),
	})
}

func truncateMessage(msg string) string {
	if len(msg) > maxErrorMessageLen {
		return msg[:maxErrorMessageLen] + "..."
	}
	return msg
}
