package service

import (
	"errors"
	"fmt"
	"time"

	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"anz-training/model"
)

// Activity timing. Workflow code must stay deterministic: no time.Now, no I/O,
// no goroutines; every side effect is an activity.
const (
	generateTimeout  = 60 * time.Minute // GenerateAndOpenPR StartToClose; Claude generation can run ~45 min
	heartbeatTimeout = time.Minute
	watchMargin      = 5 * time.Minute // WatchDeploy runs this long past the monitor's own timeout
	defaultWatch     = 20 * time.Minute
)

// act is only used to name activities (method values on a nil pointer).
var act *Activities

func (c WorkflowConfig) withDefaults() WorkflowConfig {
	if c.ApprovalTimeout <= 0 {
		c.ApprovalTimeout = defaultApprovalTimeout
	}
	if c.StageTimeout <= 0 {
		c.StageTimeout = defaultStageTimeout
	}
	return c
}

// lightCtx is for GitHub and MySQL activities: bounded retries with backoff.
func lightCtx(ctx workflow.Context, cfg WorkflowConfig) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: cfg.StageTimeout,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval: 2 * time.Second, BackoffCoefficient: 2, MaximumInterval: 30 * time.Second, MaximumAttempts: 3,
		},
	})
}

// claimCtx is for the compare-and-set claims. A retry after a lost completion
// would see the status already moved and wrongly report "owned by another run",
// leaving the project stuck, so a claim is attempted once and a failure is recorded.
func claimCtx(ctx workflow.Context, cfg WorkflowConfig) workflow.Context {
	return workflow.WithActivityOptions(lightCtx(ctx, cfg), workflow.ActivityOptions{
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
	})
}

// generateCtx runs on the capped generate queue; generation is slow and costly, so it is not retried.
func generateCtx(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		TaskQueue:           TaskQueueGenerate,
		StartToCloseTimeout: generateTimeout,
		HeartbeatTimeout:    heartbeatTimeout,
		// one retry covers a worker restart mid-run: generation starts from a clean
		// workspace, the push is forced and the PR is reused
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 2},
	})
}

// watchCtx is for the polling-mode DeployMonitor wrapper.
func watchCtx(ctx workflow.Context, cfg WorkflowConfig) workflow.Context {
	d := defaultWatch
	if cfg.DeployTimeout > 0 {
		d = cfg.DeployTimeout + watchMargin
	}
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout: d,
		HeartbeatTimeout:    heartbeatTimeout,
		// safe to repeat: its writes are compare-and-set from deploying, so a restart resumes the watch
		RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 3},
	})
}

// failureMessage is the text stored in error_message: the activity's own
// message, without Temporal's wrapper.
func failureMessage(err error) string {
	if temporal.IsCanceledError(err) {
		return "cancelled"
	}
	var te *temporal.TimeoutError
	if errors.As(err, &te) {
		if te.TimeoutType() == enumspb.TIMEOUT_TYPE_HEARTBEAT {
			return "the step stopped responding (the worker restarted or crashed)"
		}
		return "the step took too long (timed out)"
	}
	var ae *temporal.ApplicationError
	if errors.As(err, &ae) {
		return ae.Message()
	}
	return err.Error()
}

// markFailed records a failed run. It uses a disconnected context so a
// cancelled workflow still gets to write the failure.
func markFailed(ctx workflow.Context, cfg WorkflowConfig, id string, cause error) {
	dctx, _ := workflow.NewDisconnectedContext(ctx)
	dctx = lightCtx(dctx, cfg)
	if err := workflow.ExecuteActivity(dctx, act.MarkFailed, FailInput{ProjectID: id, Message: failureMessage(cause)}).Get(dctx, nil); err != nil {
		workflow.GetLogger(ctx).Error("record failure", "project", id, "error", err)
	}
}

// ProjectWorkflow runs one project: pending -> generating -> waiting_for_approval
// -> deploying -> the deploy result. Workflow ID = project ID.
func ProjectWorkflow(ctx workflow.Context, in ProjectInput) (err error) {
	cfg, id := in.Config.withDefaults(), in.ProjectID
	defer func() {
		if err != nil {
			markFailed(ctx, cfg, id, err)
		}
	}()
	light := lightCtx(ctx, cfg)

	var claimed bool
	if err = workflow.ExecuteActivity(claimCtx(ctx, cfg), act.ClaimProject, id).Get(ctx, &claimed); err != nil {
		return err
	}
	if !claimed {
		return nil // another run owns it
	}

	noGCP := cfg.RepoOnly || cfg.Demo || cfg.Deploy
	deploy := cfg.Deploy && !cfg.RepoOnly
	if err = workflow.ExecuteActivity(light, act.PrepareAndCreateRepo, CreateRepoInput{ProjectID: id, NoGCP: noGCP, Deploy: deploy}).Get(ctx, nil); err != nil {
		return err
	}
	if cfg.RepoOnly {
		return workflow.ExecuteActivity(light, act.UpdateProject, UpdateInput{ProjectID: id, Fields: map[string]any{"status": model.StatusSuccess}}).Get(ctx, nil)
	}

	var prURL string
	if err = workflow.ExecuteActivity(generateCtx(ctx), "GenerateAndOpenPR", GenerateInput{ProjectID: id, NoGCP: noGCP}).Get(ctx, &prURL); err != nil {
		return err
	}
	if err = workflow.ExecuteActivity(light, act.UpdateProject, UpdateInput{ProjectID: id, Fields: map[string]any{
		"pr_url": prURL, "status": model.StatusWaitingForApproval,
	}}).Get(ctx, nil); err != nil {
		return err
	}

	// human-in-the-loop gate: the approve signal or the approval timer
	if err = awaitApproval(ctx, cfg.ApprovalTimeout); err != nil {
		return err
	}

	if err = workflow.ExecuteActivity(light, act.UpdateProject, UpdateInput{ProjectID: id, Fields: map[string]any{
		"status": model.StatusDeploying, "current_step": model.StepMerging,
	}}).Get(ctx, nil); err != nil {
		return err
	}
	var mergeAt time.Time
	if err = workflow.ExecuteActivity(light, act.MergePR, id).Get(ctx, &mergeAt); err != nil {
		return err
	}
	switch {
	case cfg.Demo && !cfg.Deploy: // nothing to deploy: merged is the end
		return workflow.ExecuteActivity(light, act.UpdateProject, UpdateInput{ProjectID: id, Fields: map[string]any{"status": model.StatusSuccess}}).Get(ctx, nil)
	case cfg.Polling:
		return workflow.ExecuteActivity(watchCtx(ctx, cfg), act.WatchDeploy, WatchInput{ProjectID: id, Since: mergeAt}).Get(ctx, nil)
	default:
		return waitForDeploy(ctx, cfg, id)
	}
}

// RedeployWorkflow triggers a manual deployment of a project that already has
// merged code, then waits for the result like ProjectWorkflow does. It reuses
// the project's workflow ID (the earlier run has finished).
func RedeployWorkflow(ctx workflow.Context, in ProjectInput) (err error) {
	cfg, id := in.Config.withDefaults(), in.ProjectID
	defer func() {
		if err != nil {
			markFailed(ctx, cfg, id, err)
		}
	}()
	light := lightCtx(ctx, cfg)

	var claimed bool
	if err = workflow.ExecuteActivity(claimCtx(ctx, cfg), act.ClaimRedeploy, id).Get(ctx, &claimed); err != nil {
		return err
	}
	if !claimed {
		return nil
	}
	var triggeredAt time.Time
	if err = workflow.ExecuteActivity(light, act.TriggerDeploy, id).Get(ctx, &triggeredAt); err != nil {
		return err
	}
	if cfg.Polling {
		return workflow.ExecuteActivity(watchCtx(ctx, cfg), act.WatchDeploy, WatchInput{ProjectID: id, Since: triggeredAt}).Get(ctx, nil)
	}
	return waitForDeploy(ctx, cfg, id)
}

// awaitApproval blocks until the approve signal arrives, or fails after timeout.
func awaitApproval(ctx workflow.Context, timeout time.Duration) error {
	approve := workflow.GetSignalChannel(ctx, SignalApprove)
	timerCtx, cancelTimer := workflow.WithCancel(ctx)
	defer cancelTimer()
	approved := false
	sel := workflow.NewSelector(ctx)
	sel.AddReceive(approve, func(c workflow.ReceiveChannel, _ bool) {
		c.Receive(ctx, nil)
		approved = true
	})
	sel.AddFuture(workflow.NewTimer(timerCtx, timeout), func(workflow.Future) {})
	sel.Select(ctx)
	switch {
	case approved:
		return nil
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		return errApprovalTimeout
	}
}

// waitForDeploy waits for the webhook's deploy-result signal. Every
// deploy-progress signal restarts the inactivity timer, so only a deploy that
// goes quiet for cfg.DeployTimeout fails (the old DeployWatchdog's job). The
// webhook is the only writer of the final status, so the result signal just ends the wait.
func waitForDeploy(ctx workflow.Context, cfg WorkflowConfig, id string) error {
	progress := workflow.GetSignalChannel(ctx, SignalDeployProgress)
	result := workflow.GetSignalChannel(ctx, SignalDeployResult)
	for {
		timerCtx, cancelTimer := workflow.WithCancel(ctx)
		done, quiet := false, false
		sel := workflow.NewSelector(ctx)
		sel.AddReceive(progress, func(c workflow.ReceiveChannel, _ bool) { c.Receive(ctx, nil) })
		sel.AddReceive(result, func(c workflow.ReceiveChannel, _ bool) {
			c.Receive(ctx, nil)
			done = true
		})
		if cfg.DeployTimeout > 0 {
			sel.AddFuture(workflow.NewTimer(timerCtx, cfg.DeployTimeout), func(f workflow.Future) {
				quiet = f.Get(timerCtx, nil) == nil
			})
		}
		sel.Select(ctx)
		cancelTimer()
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case done:
			return nil
		case quiet:
			// compare-and-set from deploying: a result the webhook already stored is never overwritten
			light := lightCtx(ctx, cfg)
			return workflow.ExecuteActivity(light, act.FailDeploy, FailInput{
				ProjectID: id, Message: fmt.Sprintf("%s for %s", WatchdogFailurePrefix, cfg.DeployTimeout),
			}).Get(ctx, nil)
		}
		// a progress signal: loop with a fresh timer
	}
}
