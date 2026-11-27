package service_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	"anz-training/internal/testutil"
	"anz-training/model"
	"anz-training/service"
)

// skew mirrors the service package's runClockSkew (10s).
const skew = 10 * time.Second

type actEnv struct {
	t     *testing.T
	a     *service.Activities
	store *testutil.MemStore
	ints  *testutil.MemIntegrations
	host  *fakeHost
	gen   *fakeGen
	key   []byte
}

func newActEnv(t *testing.T, secrets service.SecretReader) *actEnv {
	t.Helper()
	e := &actEnv{
		t: t, store: testutil.NewMemStore(), ints: testutil.NewMemIntegrations(),
		host: &fakeHost{}, gen: &fakeGen{dir: t.TempDir()}, key: testutil.TestKey(5),
	}
	e.ints.Put(t, e.key, "u1", model.ProviderGitHub, model.GitHubCreds{Owner: "acme", Token: "ghp_x"})
	e.ints.Put(t, e.key, "u1", model.ProviderGCP, testutil.GCPCreds)
	e.store.Add(model.Project{ID: "p1", OwnerID: "u1", Name: "My App", Prompt: "todo app", Status: model.StatusPending, CurrentStep: model.StepInitializing, BranchName: "main"})
	e.a = &service.Activities{Store: e.store, Integrations: e.ints, GitHub: e.host, Gen: e.gen, Secrets: secrets, MasterKey: e.key}
	return e
}

// withDeployer switches to deploy mode: the platform's GCP is used, not the user's.
func (e *actEnv) withDeployer(fd *fakeDeployer) {
	fd.rec = e.host.rec
	e.a.Deployer = fd
	delete(e.ints.Rows, "u1|gcp")
}

func nonRetryable(err error) bool {
	var ae *temporal.ApplicationError
	return errors.As(err, &ae) && ae.NonRetryable()
}

func TestActivities_ClaimProject_OnlyOnce(t *testing.T) {
	e := newActEnv(t, nil)
	ctx := context.Background()
	if ok, err := e.a.ClaimProject(ctx, "p1"); err != nil || !ok {
		t.Fatalf("first claim = %v, %v", ok, err)
	}
	if p := e.store.Get("p1"); p.Status != model.StatusGenerating || p.CurrentStep != model.StepInitializing {
		t.Fatalf("project: %+v", p)
	}
	if ok, err := e.a.ClaimProject(ctx, "p1"); err != nil || ok {
		t.Fatalf("second claim = %v, %v; want false", ok, err)
	}
}

func TestActivities_PrepareAndCreateRepo_PreparesBeforeRepoInDeployMode(t *testing.T) {
	e := newActEnv(t, nil)
	fd := &fakeDeployer{creds: testutil.GCPCreds}
	e.withDeployer(fd)

	url, err := e.a.PrepareAndCreateRepo(context.Background(), service.CreateRepoInput{ProjectID: "p1", NoGCP: true, Deploy: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(e.host.calls) < 2 || e.host.calls[0] != "prepare" || e.host.calls[1] != "create" {
		t.Fatalf("call order = %v, want prepare then create", e.host.calls)
	}
	if e.host.createGCP != testutil.GCPCreds {
		t.Fatalf("CreateRepo got %+v, want the deployer's creds", e.host.createGCP)
	}
	name := service.ServiceName("My App", "p1")
	if got := fd.targets[0]; got != (service.DeployTarget{ProjectID: "p1", ServiceName: name, RepoOwner: "acme", RepoName: name}) {
		t.Fatalf("target = %+v", got)
	}
	if p := e.store.Get("p1"); p.RepoURL != url || p.CurrentStep != model.StepRepoCreated {
		t.Fatalf("project: %+v", p)
	}
}

func TestActivities_PrepareAndCreateRepo_PrepareFailure(t *testing.T) {
	e := newActEnv(t, nil)
	e.withDeployer(&fakeDeployer{err: errors.New("iam denied")})
	_, err := e.a.PrepareAndCreateRepo(context.Background(), service.CreateRepoInput{ProjectID: "p1", NoGCP: true, Deploy: true})
	if err == nil || !strings.Contains(err.Error(), "prepare") || !strings.Contains(err.Error(), "iam denied") {
		t.Fatalf("err = %v", err)
	}
	if e.host.count("create") != 0 {
		t.Fatal("repo must not be created when preparing the deployment failed")
	}
}

func TestActivities_PrepareAndCreateRepo_UsersOwnGCPOutsideDeployMode(t *testing.T) {
	e := newActEnv(t, nil)
	if _, err := e.a.PrepareAndCreateRepo(context.Background(), service.CreateRepoInput{ProjectID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if e.host.createGCP != testutil.GCPCreds {
		t.Fatalf("CreateRepo got %+v, want the user's GCP integration", e.host.createGCP)
	}
}

func TestActivities_MissingIntegrationIsNonRetryable(t *testing.T) {
	e := newActEnv(t, nil)
	delete(e.ints.Rows, "u1|github")
	_, err := e.a.PrepareAndCreateRepo(context.Background(), service.CreateRepoInput{ProjectID: "p1", NoGCP: true})
	if err == nil || !nonRetryable(err) || !strings.Contains(err.Error(), "github integration is not configured") {
		t.Fatalf("err = %v (non-retryable=%v)", err, nonRetryable(err))
	}
}

func prepared(e *actEnv) {
	e.store.Add(model.Project{ID: "p1", OwnerID: "u1", Name: "My App", Prompt: "todo app", Status: model.StatusGenerating,
		CurrentStep: model.StepRepoCreated, BranchName: "main", RepoURL: "https://github.com/acme/p1"})
}

func TestActivities_GenerateAndOpenPR_HappyPath(t *testing.T) {
	e := newActEnv(t, nil)
	prepared(e)
	pr, err := e.a.GenerateAndOpenPR(context.Background(), service.GenerateInput{ProjectID: "p1"})
	if err != nil {
		t.Fatal(err)
	}
	if pr != "https://github.com/acme/p1/pull/1" {
		t.Fatalf("pr = %q", pr)
	}
	if e.host.pushDir != e.gen.dir {
		t.Fatalf("PR pushed from %q, want generated workspace %q", e.host.pushDir, e.gen.dir)
	}
	if p := e.store.Get("p1"); p.CurrentStep != model.StepTestsPassed {
		t.Fatalf("step = %q", p.CurrentStep)
	}
	if len(e.gen.cleanups) != 1 {
		t.Fatalf("cleanups = %v, want exactly one", e.gen.cleanups)
	}
}

func TestActivities_GenerateAndOpenPR_BYOKKeyPassedToGenerator(t *testing.T) {
	sr := &fakeSecretReader{val: "sk-user-key"}
	e := newActEnv(t, sr)
	byok := testutil.GCPCreds
	byok.AnthropicSecretRef = "projects/x/secrets/k/versions/1"
	e.ints.Put(t, e.key, "u1", model.ProviderGCP, byok)
	prepared(e)
	if _, err := e.a.GenerateAndOpenPR(context.Background(), service.GenerateInput{ProjectID: "p1"}); err != nil {
		t.Fatal(err)
	}
	if sr.ref != byok.AnthropicSecretRef || e.gen.reqs[0].AnthropicAPIKey != "sk-user-key" {
		t.Fatalf("secret ref %q, generator key %q", sr.ref, e.gen.reqs[0].AnthropicAPIKey)
	}
}

func TestActivities_GenerateAndOpenPR_BYOKWithoutSecretReaderIsNonRetryable(t *testing.T) {
	e := newActEnv(t, nil)
	byok := testutil.GCPCreds
	byok.AnthropicSecretRef = "projects/x/secrets/k/versions/1"
	e.ints.Put(t, e.key, "u1", model.ProviderGCP, byok)
	prepared(e)
	_, err := e.a.GenerateAndOpenPR(context.Background(), service.GenerateInput{ProjectID: "p1"})
	if err == nil || !nonRetryable(err) {
		t.Fatalf("err = %v (non-retryable=%v)", err, nonRetryable(err))
	}
	if len(e.gen.reqs) != 0 {
		t.Fatal("generation must not start without the BYOK key")
	}
}

func TestActivities_GenerateAndOpenPR_PushFailureStillCleansWorkspace(t *testing.T) {
	e := newActEnv(t, nil)
	e.host.pushErr = errors.New("push rejected")
	prepared(e)
	_, err := e.a.GenerateAndOpenPR(context.Background(), service.GenerateInput{ProjectID: "p1"})
	if err == nil || !strings.Contains(err.Error(), "push rejected") {
		t.Fatalf("err = %v", err)
	}
	if len(e.gen.cleanups) != 1 {
		t.Fatalf("cleanups = %v", e.gen.cleanups)
	}
}

func TestActivities_GenerateAndOpenPR_GenerateFailureStillCleansWorkspace(t *testing.T) {
	e := newActEnv(t, nil)
	e.gen.err = errors.New("tests failed")
	prepared(e)
	_, err := e.a.GenerateAndOpenPR(context.Background(), service.GenerateInput{ProjectID: "p1"})
	if err == nil || !strings.Contains(err.Error(), "tests failed") {
		t.Fatalf("err = %v", err)
	}
	if len(e.gen.cleanups) != 1 {
		t.Fatalf("cleanups = %v (a half-written workspace must be cleaned)", e.gen.cleanups)
	}
	if e.host.count("push") != 0 {
		t.Fatal("pushed after a failed generation")
	}
}

func readyToMerge(e *actEnv, step string) {
	e.store.Add(model.Project{ID: "p1", OwnerID: "u1", Name: "x", Prompt: "x", Status: model.StatusDeploying,
		CurrentStep: step, RepoURL: "https://github.com/acme/p1", PRURL: "https://github.com/acme/p1/pull/1"})
}

func TestActivities_MergePR_SetsMergedStepOnlyWhenStillMerging(t *testing.T) {
	for _, tc := range []struct{ from, want string }{
		{model.StepMerging, model.StepMerged},
		{model.StepBuilding, model.StepBuilding}, // a webhook already moved on: never overwritten
	} {
		e := newActEnv(t, nil)
		readyToMerge(e, tc.from)
		at, err := e.a.MergePR(context.Background(), "p1")
		if err != nil {
			t.Fatal(err)
		}
		if e.host.mergeArgs[0] != "https://github.com/acme/p1" || e.host.mergeArgs[1] != "https://github.com/acme/p1/pull/1" {
			t.Fatalf("merge args = %v", e.host.mergeArgs)
		}
		if p := e.store.Get("p1"); p.CurrentStep != tc.want {
			t.Fatalf("from %s: step = %q, want %q", tc.from, p.CurrentStep, tc.want)
		}
		if d := time.Since(at); d < skew-time.Second || d > skew+5*time.Second {
			t.Fatalf("mergeAt is %v ago, want about the clock skew", d)
		}
	}
}

func TestActivities_MergePR_FailureIsReturned(t *testing.T) {
	e := newActEnv(t, nil)
	e.host.mergeEr = errors.New("conflict")
	readyToMerge(e, model.StepMerging)
	if _, err := e.a.MergePR(context.Background(), "p1"); err == nil || !strings.Contains(err.Error(), "merge pull request") {
		t.Fatalf("err = %v", err)
	}
}

func deployed(e *actEnv, status, step string) {
	e.store.Add(model.Project{ID: "p1", OwnerID: "u1", Name: "x", Prompt: "x", Status: status, CurrentStep: step,
		RepoURL: "https://github.com/acme/x", ErrorMessage: "old error"})
}

func TestActivities_ClaimRedeploy_FromSuccessAndFailedAfterMerge(t *testing.T) {
	for _, tc := range []struct{ status, step string }{
		{model.StatusSuccess, model.StepDeploying},
		{model.StatusFailed, model.StepTesting},
		{model.StatusFailed, model.StepMerged},
	} {
		e := newActEnv(t, nil)
		e.withDeployer(&fakeDeployer{})
		deployed(e, tc.status, tc.step)
		ok, err := e.a.ClaimRedeploy(context.Background(), "p1")
		if err != nil || !ok {
			t.Fatalf("%s/%s: claim = %v, %v", tc.status, tc.step, ok, err)
		}
		if p := e.store.Get("p1"); p.Status != model.StatusDeploying || p.CurrentStep != model.StepMerged || p.ErrorMessage != "" {
			t.Fatalf("%s/%s: project %+v", tc.status, tc.step, p)
		}
	}
}

func TestActivities_ClaimRedeploy_Refusals(t *testing.T) {
	for name, tc := range map[string]struct{ status, step string }{
		"still deploying":         {model.StatusDeploying, model.StepBuilding},
		"failed before the merge": {model.StatusFailed, model.StepRepoCreated},
		"waiting for approval":    {model.StatusWaitingForApproval, model.StepTestsPassed},
	} {
		e := newActEnv(t, nil)
		e.withDeployer(&fakeDeployer{})
		deployed(e, tc.status, tc.step)
		if ok, err := e.a.ClaimRedeploy(context.Background(), "p1"); err != nil || ok {
			t.Fatalf("%s: claim = %v, %v; want false", name, ok, err)
		}
		if p := e.store.Get("p1"); p.Status != tc.status {
			t.Fatalf("%s: project changed: %+v", name, p)
		}
	}
}

func TestActivities_TriggerDeploy_UsesDeployer(t *testing.T) {
	e := newActEnv(t, nil)
	fd := &fakeDeployer{}
	e.withDeployer(fd)
	deployed(e, model.StatusDeploying, model.StepMerged)
	at, err := e.a.TriggerDeploy(context.Background(), "p1")
	if err != nil {
		t.Fatal(err)
	}
	if len(fd.triggers) != 1 || fd.triggers[0] != "https://github.com/acme/x" {
		t.Fatalf("triggers = %v", fd.triggers)
	}
	if time.Since(at) < skew-time.Second {
		t.Fatalf("triggeredAt = %v is not skew-adjusted", at)
	}

	fd.triggerErr = errors.New("dispatch failed")
	if _, err := e.a.TriggerDeploy(context.Background(), "p1"); err == nil || !strings.Contains(err.Error(), "trigger deployment") {
		t.Fatalf("err = %v", err)
	}
}

func TestActivities_FailDeploy_DoesNotOverwriteTerminalStatus(t *testing.T) {
	e := newActEnv(t, nil)
	deployed(e, model.StatusDeploying, model.StepBuilding)
	if err := e.a.FailDeploy(context.Background(), service.FailInput{ProjectID: "p1", Message: "quiet for 10m"}); err != nil {
		t.Fatal(err)
	}
	if p := e.store.Get("p1"); p.Status != model.StatusFailed || p.ErrorMessage != "quiet for 10m" {
		t.Fatalf("project: %+v", p)
	}

	e2 := newActEnv(t, nil)
	deployed(e2, model.StatusSuccess, model.StepDeploying)
	if err := e2.a.FailDeploy(context.Background(), service.FailInput{ProjectID: "p1", Message: "late"}); err != nil {
		t.Fatal(err)
	}
	if p := e2.store.Get("p1"); p.Status != model.StatusSuccess {
		t.Fatalf("a finished project was overwritten: %+v", p)
	}
}

func TestActivities_MarkFailed_TruncatesLongMessage(t *testing.T) {
	e := newActEnv(t, nil)
	if err := e.a.MarkFailed(context.Background(), service.FailInput{ProjectID: "p1", Message: strings.Repeat("x", 5000)}); err != nil {
		t.Fatal(err)
	}
	p := e.store.Get("p1")
	if p.Status != model.StatusFailed || len(p.ErrorMessage) > 1100 || !strings.HasSuffix(p.ErrorMessage, "...") {
		t.Fatalf("status %q, message length %d", p.Status, len(p.ErrorMessage))
	}
}

func TestActivities_WatchDeploy_RecordsTheResult(t *testing.T) {
	e := newActEnv(t, nil)
	deployed(e, model.StatusDeploying, model.StepMerged)
	w := &scriptedWatcher{results: []service.DeployRun{
		{Found: true, CreatedAt: t0, Done: true, Conclusion: "success", LiveURL: "https://app.run.app"},
	}}
	e.a.Monitor = service.NewDeployMonitor(e.store, w, service.DeployMonitorConfig{Poll: time.Millisecond, StartTimeout: time.Second, Timeout: 2 * time.Second})
	if err := e.a.WatchDeploy(context.Background(), service.WatchInput{ProjectID: "p1", Since: time.Unix(1000, 0)}); err != nil {
		t.Fatal(err)
	}
	if p := e.store.Get("p1"); p.Status != model.StatusSuccess || p.LiveURL != "https://app.run.app" {
		t.Fatalf("project: %+v", p)
	}
}

func TestActivities_UpdateProject_WritesFields(t *testing.T) {
	e := newActEnv(t, nil)
	if err := e.a.UpdateProject(context.Background(), service.UpdateInput{ProjectID: "p1", Fields: map[string]any{"status": model.StatusSuccess}}); err != nil {
		t.Fatal(err)
	}
	if e.store.Get("p1").Status != model.StatusSuccess {
		t.Fatal("status not written")
	}
}
