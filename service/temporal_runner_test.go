package service_test

import (
	"context"
	"errors"
	"fmt"
	enumspb "go.temporal.io/api/enums/v1"
	"go.temporal.io/sdk/temporal"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"

	"go.temporal.io/api/serviceerror"
	"go.temporal.io/sdk/client"

	"anz-training/internal/testutil"
	"anz-training/model"
	"anz-training/service"
)

// fakeWorkflowClient records what the runner asks Temporal to do.
type fakeWorkflowClient struct {
	mu       sync.Mutex
	started  []client.StartWorkflowOptions
	workflow []string
	inputs   []any
	signals  []string // "workflowID:signal"
	startErr error
	sigErr   error
}

func funcName(f any) string { return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name() }

func (c *fakeWorkflowClient) ExecuteWorkflow(_ context.Context, o client.StartWorkflowOptions, wf interface{}, args ...interface{}) (client.WorkflowRun, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.startErr != nil {
		return nil, c.startErr
	}
	c.started = append(c.started, o)
	c.workflow = append(c.workflow, funcName(wf))
	c.inputs = append(c.inputs, args...)
	return nil, nil
}

func (c *fakeWorkflowClient) SignalWorkflow(_ context.Context, id, _ string, name string, _ interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.signals = append(c.signals, id+":"+name)
	return c.sigErr
}

func alreadyStarted() error {
	return serviceerror.NewWorkflowExecutionAlreadyStarted("running", "req", "run")
}

func newRunner(t *testing.T, withDeployer bool) (*service.TemporalRunner, *fakeWorkflowClient, *testutil.MemStore) {
	t.Helper()
	c, st := &fakeWorkflowClient{}, testutil.NewMemStore()
	var d service.Deployer
	if withDeployer {
		d = &fakeDeployer{}
	}
	return service.NewTemporalRunner(c, st, service.WorkflowConfig{Deploy: withDeployer}, d), c, st
}

func TestRunner_Start_StartsWorkflowWithProjectIDAsWorkflowID(t *testing.T) {
	r, c, _ := newRunner(t, true)
	if err := r.Start("p1"); err != nil {
		t.Fatal(err)
	}
	if len(c.started) != 1 || c.started[0].ID != "p1" || c.started[0].TaskQueue != service.TaskQueueProjects {
		t.Fatalf("started = %+v", c.started)
	}
	if !c.started[0].WorkflowExecutionErrorWhenAlreadyStarted {
		t.Fatal("without WorkflowExecutionErrorWhenAlreadyStarted the SDK hides an already-running workflow and ErrAlreadyRunning never fires")
	}
	if !strings.HasSuffix(c.workflow[0], "ProjectWorkflow") {
		t.Fatalf("workflow = %s", c.workflow[0])
	}
	if in := c.inputs[0].(service.ProjectInput); in.ProjectID != "p1" || !in.Config.Deploy {
		t.Fatalf("input = %+v", in)
	}
}

func TestRunner_Start_AlreadyStartedMapsToErrAlreadyRunning(t *testing.T) {
	r, c, _ := newRunner(t, false)
	c.startErr = alreadyStarted()
	if err := r.Start("p1"); !errors.Is(err, service.ErrAlreadyRunning) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunner_Approve_Guards(t *testing.T) {
	r, c, st := newRunner(t, false)
	ctx := context.Background()
	st.Add(model.Project{ID: "p1", OwnerID: "u1", Status: model.StatusPending})
	if err := r.Approve(ctx, "p1", "u1"); !errors.Is(err, service.ErrNotWaiting) {
		t.Fatalf("pending: %v", err)
	}
	st.Add(model.Project{ID: "p1", OwnerID: "u1", Status: model.StatusWaitingForApproval})
	if err := r.Approve(ctx, "p1", "intruder"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("other owner must see not-found, got %v", err)
	}
	if err := r.Approve(ctx, "nope", "u1"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if len(c.signals) != 0 {
		t.Fatalf("rejected approvals must not signal: %v", c.signals)
	}
	if err := r.Approve(ctx, "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	if len(c.signals) != 1 || c.signals[0] != "p1:"+service.SignalApprove {
		t.Fatalf("signals = %v", c.signals)
	}

	c.sigErr = serviceerror.NewNotFound("workflow not found") // the run ended or the server lost it
	if err := r.Approve(ctx, "p1", "u1"); !errors.Is(err, service.ErrNoPendingRun) {
		t.Fatalf("no workflow: %v", err)
	}
}

func TestRunner_Approve_Twice_IsHarmless(t *testing.T) {
	r, c, st := newRunner(t, false)
	st.Add(model.Project{ID: "p1", OwnerID: "u1", Status: model.StatusWaitingForApproval})
	for i := 0; i < 2; i++ {
		if err := r.Approve(context.Background(), "p1", "u1"); err != nil {
			t.Fatalf("approve #%d: %v", i+1, err)
		}
	}
	if len(c.signals) != 2 {
		t.Fatalf("signals = %v", c.signals)
	}
}

func TestRunner_Redeploy_Refusals(t *testing.T) {
	cases := []struct {
		name, owner, status, step string
		want                      error
	}{
		{"failed before the merge", "u1", model.StatusFailed, model.StepRepoCreated, service.ErrNotRedeployable},
		{"merge itself failed", "u1", model.StatusFailed, model.StepMerging, service.ErrNotRedeployable},
		{"already deploying", "u1", model.StatusDeploying, model.StepBuilding, service.ErrNotRedeployable},
		{"waiting for approval", "u1", model.StatusWaitingForApproval, model.StepTestsPassed, service.ErrNotRedeployable},
		{"someone else's project", "u2", model.StatusSuccess, model.StepDeploying, model.ErrNotFound},
	}
	for _, tc := range cases {
		r, c, st := newRunner(t, true)
		st.Add(model.Project{ID: "r1", OwnerID: "u1", Status: tc.status, CurrentStep: tc.step, RepoURL: "https://github.com/acme/x"})
		if err := r.Redeploy(context.Background(), "r1", tc.owner); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if len(c.started) != 0 {
			t.Errorf("%s: a workflow was started", tc.name)
		}
	}
	// without a Deployer there is nothing to trigger
	r, _, st := newRunner(t, false)
	st.Add(model.Project{ID: "r1", OwnerID: "u1", Status: model.StatusSuccess, RepoURL: "https://github.com/acme/x"})
	if err := r.Redeploy(context.Background(), "r1", "u1"); !errors.Is(err, service.ErrNotRedeployable) {
		t.Errorf("no deployer: %v", err)
	}
}

func TestRunner_Redeploy_StartsRedeployWorkflow_AlreadyStartedIsErrAlreadyRunning(t *testing.T) {
	r, c, st := newRunner(t, true)
	st.Add(model.Project{ID: "r1", OwnerID: "u1", Status: model.StatusSuccess, CurrentStep: model.StepDeploying, RepoURL: "https://github.com/acme/x"})
	if err := r.Redeploy(context.Background(), "r1", "u1"); err != nil {
		t.Fatal(err)
	}
	if c.started[0].ID != "r1" || !strings.HasSuffix(c.workflow[0], "RedeployWorkflow") {
		t.Fatalf("started %+v %v", c.started, c.workflow)
	}
	if p := st.Get("r1"); p.Status != model.StatusSuccess {
		t.Fatalf("the runner must not change state; the workflow's first activity does: %+v", p)
	}

	c.startErr = alreadyStarted()
	if err := r.Redeploy(context.Background(), "r1", "u1"); !errors.Is(err, service.ErrAlreadyRunning) {
		t.Fatalf("err = %v", err)
	}
}

func TestRunner_SignalDeploy_MissingWorkflowIsIgnored(t *testing.T) {
	r, c, _ := newRunner(t, true)
	if err := r.SignalDeploy(context.Background(), "p1", service.SignalDeployResult); err != nil {
		t.Fatal(err)
	}
	if c.signals[0] != "p1:"+service.SignalDeployResult {
		t.Fatalf("signals = %v", c.signals)
	}
	c.sigErr = serviceerror.NewNotFound("workflow not found")
	if err := r.SignalDeploy(context.Background(), "p1", service.SignalDeployResult); err != nil {
		t.Fatalf("a finished workflow must be ignored, got %v", err)
	}
	c.sigErr = errors.New("connection refused")
	if err := r.SignalDeploy(context.Background(), "p1", service.SignalDeployResult); err == nil {
		t.Fatal("other signal errors must be returned")
	}
}

func TestFailureMessage_ReadableForTimeoutsAndApplicationErrors(t *testing.T) {
	hb := fmt.Errorf("activity error: %w", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_HEARTBEAT, nil))
	if got := service.FailureMessage(hb); !strings.Contains(got, "stopped responding") || strings.Contains(got, "activity error") {
		t.Fatalf("heartbeat timeout message = %q", got)
	}
	st := fmt.Errorf("activity error: %w", temporal.NewTimeoutError(enumspb.TIMEOUT_TYPE_START_TO_CLOSE, nil))
	if got := service.FailureMessage(st); !strings.Contains(got, "took too long") {
		t.Fatalf("start-to-close timeout message = %q", got)
	}
	app := fmt.Errorf("activity error: %w", temporal.NewApplicationError("create repo: boom", "T"))
	if got := service.FailureMessage(app); got != "create repo: boom" {
		t.Fatalf("application error message = %q", got)
	}
}
