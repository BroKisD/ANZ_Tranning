package test

import (
	"context"
	"os"
	"testing"
	"time"

	"anz-training/model"
	"anz-training/service"
)

// TestLive_CreateRepo really creates a private repo on GitHub with a PAT. It is
// skipped unless LIVE_GITHUB=1, and the repo is NOT deleted afterwards.
//
//	LIVE_GITHUB=1 GITHUB_TOKEN=ghp_xxx go test ./test -run TestLive_CreateRepo -v
//
// Optional: LIVE_OWNER=<org> to create the repo in an organization instead of
// under the token's own user.
func TestLive_CreateRepo(t *testing.T) {
	if os.Getenv("LIVE_GITHUB") != "1" {
		t.Skip("set LIVE_GITHUB=1 to create a real repo on GitHub")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Fatal("GITHUB_TOKEN required")
	}
	gh := service.NewGitHub(service.GitHubConfig{})
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	owner := os.Getenv("LIVE_OWNER")
	login, err := gh.TokenOwner(ctx, token)
	if err != nil {
		t.Fatalf("token rejected by GitHub: %v", err)
	}
	t.Logf("token belongs to %q", login)
	if owner == "" {
		owner = login
	}
	// empty GCPCreds = repo-only mode (no deploy workflow)
	url, err := gh.CreateRepo(ctx, model.GitHubCreds{Owner: owner, Token: token}, model.GCPCreds{},
		"live"+time.Now().Format("150405"), "Live Test")
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	t.Logf("CREATED: %s  (delete it manually when done)", url)
}

// TestLive_DemoFlow runs the whole demo pipeline against real GitHub, with no
// server, DB or Postman (an in-memory store stands in for MySQL):
//
//	create project -> create repo -> clone template + mock tests -> open PR
//	-> wait for approval -> approve -> merge -> success
//
// It creates a real private repo and does NOT delete it.
//
//	LIVE_GITHUB=1 GITHUB_TOKEN=ghp_xxx DEMO_TEMPLATE_REPO=https://github.com/<user>/<public-repo> \
//	  go test ./test -run TestLive_DemoFlow -v
//
// Optional: LIVE_OWNER=<org> to create the repo in an organization.
func TestLive_DemoFlow(t *testing.T) {
	if os.Getenv("LIVE_GITHUB") != "1" {
		t.Skip("set LIVE_GITHUB=1 to run against real GitHub")
	}
	token, template := os.Getenv("GITHUB_TOKEN"), os.Getenv("DEMO_TEMPLATE_REPO")
	if token == "" || template == "" {
		t.Fatal("GITHUB_TOKEN and DEMO_TEMPLATE_REPO are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	gh := service.NewGitHub(service.GitHubConfig{})
	owner := os.Getenv("LIVE_OWNER")
	login, err := gh.TokenOwner(ctx, token)
	if err != nil {
		t.Fatalf("token rejected by GitHub: %v", err)
	}
	if owner == "" {
		owner = login
	}
	t.Logf("STEP 0  token ok, repos will be created under %q", owner)

	gen, err := service.NewDemoGenerator(service.DemoGeneratorConfig{TemplateURL: template, WorkspaceRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	key := testKey(5)
	store := &memStore{projects: map[string]*model.Project{}}
	ints := &memIntegrations{rows: map[string]model.UserIntegration{}}
	ints.put(t, key, "u1", model.ProviderGitHub, model.GitHubCreds{Owner: owner, Token: token})
	orch := service.NewOrchestrator(store, ints, gh, gen, nil, service.OrchestratorConfig{
		MasterKey: key, Demo: true, StageTimeout: time.Minute, ApprovalTimeout: 2 * time.Minute,
	})
	defer func() {
		c, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = orch.Shutdown(c)
	}()

	// 1. the user's request, saved
	const id = "demo-live-1"
	store.add(model.Project{
		ID: id, OwnerID: "u1", Name: "Live Demo " + time.Now().Format("150405"), Prompt: "a todo app",
		Status: model.StatusPending, CurrentStep: model.StepInitializing, BranchName: "main",
	})
	t.Logf("STEP 1  request saved: project %s (status=pending)", id)
	if err := orch.Start(id); err != nil {
		t.Fatal(err)
	}

	// watch the project move through the pipeline
	last := ""
	waitFor := func(want string) model.Project {
		t.Helper()
		for ctx.Err() == nil {
			p := store.get(id)
			if cur := p.Status + "|" + p.CurrentStep; cur != last {
				last = cur
				t.Logf("        status=%-20s step=%-14s repo=%s pr=%s", p.Status, p.CurrentStep, p.RepoURL, p.PRURL)
			}
			if p.Status == model.StatusFailed {
				t.Fatalf("project failed: %s", p.ErrorMessage)
			}
			if p.Status == want {
				return p
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s (last %s)", want, last)
		return model.Project{}
	}

	p := waitFor(model.StatusWaitingForApproval)
	t.Logf("STEP 2  repo created:           %s", p.RepoURL)
	t.Logf("STEP 3  template cloned, mock tests passed (step=%s)", p.CurrentStep)
	t.Logf("STEP 4  pull request opened:    %s", p.PRURL)

	// human approval gate
	t.Logf("STEP 5  approving ...")
	if err := orch.Approve(ctx, id, "u1"); err != nil {
		t.Fatal(err)
	}
	p = waitFor(model.StatusSuccess)
	t.Logf("STEP 6  PR merged, project %s. Check %s (delete the repo when done)", p.Status, p.RepoURL)
}
