package test

import (
	"anz-training/internal/testutil"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"anz-training/model"
	"anz-training/service"
)

// Live tests for the Cloud Run deployment. Each is skipped unless its env flag is
// set, creates real resources (never deleted), and logs numbered STEP lines.
// See docs/deploy-setup.md for the exact commands.

func liveDeployConfig(t *testing.T) service.DeployConfig {
	t.Helper()
	cfg, err := service.DeployConfigFromEnv(os.Getenv)
	if err != nil {
		t.Fatalf("GCP config: %v", err)
	}
	return cfg
}

// TestLive_GCPProvision runs the GCP setup alone: shared resources, then a trust
// binding for a made-up repo. Needs `gcloud auth application-default login`
// (or a key) for an account holding the roles in scripts/grant-platform-roles.sh.
func TestLive_GCPProvision(t *testing.T) {
	if os.Getenv("LIVE_GCP") != "1" {
		t.Skip("set LIVE_GCP=1 (and GCP_PROJECT_ID, GCP_ALLOWED_GITHUB_OWNERS) to provision GCP for real")
	}
	cfg := liveDeployConfig(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	g, err := service.NewGCP(ctx, cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("STEP 1  ensuring APIs, registry %q, deployer account, identity pool + provider in %s ...", cfg.ArtifactRepo, cfg.ProjectID)
	if err := g.EnsureBase(ctx); err != nil {
		t.Fatalf("EnsureBase: %v", err)
	}
	t.Logf("STEP 2  shared setup is ready; binding a made-up repo ...")
	repo := "live-bind-" + time.Now().Format("150405")
	creds, err := g.BindRepo(ctx, cfg.AllowedOwners[0], repo)
	if err != nil {
		t.Fatalf("BindRepo: %v", err)
	}
	t.Logf("STEP 3  %s/%s can now log in as %s", cfg.AllowedOwners[0], repo, creds.ServiceAccount)
	t.Logf("        workload identity provider: %s", creds.WIFProvider)
}

type ghRun struct {
	ID         int64  `json:"id"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
}

func ghGet(ctx context.Context, token, path string, out any) error {
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://api.github.com"+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("github %d: %.200s", resp.StatusCode, b)
	}
	return json.Unmarshal(b, out)
}

// liveStaticCreds are the GCP values of the existing CI/CD setup (resource names,
// not secrets); any of them can be overridden with the GCP_* environment variables.
func liveStaticCreds() model.GCPCreds {
	pick := func(env, def string) string {
		if v := os.Getenv(env); v != "" {
			return v
		}
		return def
	}
	return model.GCPCreds{
		ProjectID:    pick("GCP_PROJECT_ID", "project-9991dcfb-85ec-4182-b68"),
		Region:       pick("GCP_REGION", "asia-southeast1"),
		ArtifactRepo: pick("GCP_ARTIFACT_REPO", "anz-repo"),
		WIFProvider: pick("GCP_WIF_PROVIDER",
			"projects/386127327935/locations/global/workloadIdentityPools/github-pool/providers/github-provider"),
		ServiceAccount: pick("GCP_SERVICE_ACCOUNT", "anz-trainning@project-9991dcfb-85ec-4182-b68.iam.gserviceaccount.com"),
	}
}

// TestLive_Deploy is THE DEMO: the whole product flow against real GitHub and
// real Google Cloud, from one command and with no server, database or Postman
// (an in-memory store stands in for MySQL):
//
//	request -> GitHub repo -> clone of this repo + mock tests -> pull request
//	-> approval -> merge -> GitHub Actions (mock build, real docker build + push,
//	mock tests, REAL Cloud Run deploy) -> live URL answering 200
//
//	LIVE_DEPLOY=1 GITHUB_TOKEN=ghp_... go test ./test -run TestLive_Deploy -v -timeout 20m
//
// Needs: a GitHub token with the `repo` and `workflow` scopes, and the GCP provider and
// service account (defaults above) must trust the repos that get created (see
// docs/deploy-setup.md). It creates a real repo and Cloud Run service and deletes neither.
func TestLive_Deploy(t *testing.T) {
	if os.Getenv("LIVE_DEPLOY") != "1" {
		t.Skip("set LIVE_DEPLOY=1 and GITHUB_TOKEN to run the full demo against real GitHub and Cloud Run")
	}
	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		t.Fatal("GITHUB_TOKEN is required")
	}
	template := os.Getenv("DEMO_TEMPLATE_REPO")
	if template == "" {
		template = "https://github.com/BroKisD/ANZ_Tranning" // this very repo is the "generated" app
	}
	creds := liveStaticCreds()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	hookURL := os.Getenv("WEBHOOK_URL")
	if hookURL == "" {
		hookURL = "https://example.invalid/api/webhooks/github" // this process is not reachable from GitHub; progress is read from the Actions API
	}
	gh := service.NewGitHub(service.GitHubConfig{WebhookURL: hookURL, WebhookSecret: "live-demo-secret"})
	owner, err := gh.TokenOwner(ctx, token)
	if err != nil {
		t.Fatalf("GitHub rejected the token: %v", err)
	}
	if v := os.Getenv("LIVE_OWNER"); v != "" {
		owner = v
	}
	t.Logf("STEP 0  setup")
	t.Logf("        GitHub account : %s", owner)
	t.Logf("        code to deploy : %s", template)
	t.Logf("        GCP project    : %s (%s)", creds.ProjectID, creds.Region)
	t.Logf("        deploys as     : %s", creds.ServiceAccount)

	deployer, err := service.NewStaticDeployer(creds, gh)
	if err != nil {
		t.Fatal(err)
	}
	gen, err := service.NewDemoGenerator(service.DemoGeneratorConfig{TemplateURL: template, WorkspaceRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	key := testutil.TestKey(5)
	store := testutil.NewMemStore()
	ints := testutil.NewMemIntegrations()
	ints.Put(t, key, "u1", model.ProviderGitHub, model.GitHubCreds{Owner: owner, Token: token})
	orch := service.NewOrchestrator(store, ints, gh, gen, nil, service.OrchestratorConfig{
		MasterKey: key, Deployer: deployer, StageTimeout: 5 * time.Minute, ApprovalTimeout: 10 * time.Minute,
	})
	defer func() {
		c, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = orch.Shutdown(c)
	}()

	// ---- the user's request ----
	const id = "deploy-live-1"
	name := "Live Demo " + time.Now().Format("150405")
	store.Add(model.Project{ID: id, OwnerID: "u1", Name: name, Prompt: "deploy a copy of this app", Status: model.StatusPending, CurrentStep: model.StepInitializing, BranchName: "main"})
	t.Logf("STEP 1  request saved: project %q (status=pending)", name)
	if err := orch.Start(id); err != nil {
		t.Fatal(err)
	}

	say := map[string]string{
		model.StepRepoCreated: "STEP 2  GitHub repo created with deploy.yml + service.yml",
		model.StepTestsPassed: "STEP 3  code cloned into the repo workspace, mock tests passed",
		model.StepMerged:      "STEP 5  pull request merged -> GitHub Actions starts",
	}
	last := ""
	waitFor := func(want string) model.Project {
		for ctx.Err() == nil {
			p := store.Get(id)
			if cur := p.Status + "|" + p.CurrentStep; cur != last {
				last = cur
				msg := say[p.CurrentStep]
				if p.Status == model.StatusWaitingForApproval {
					msg = "STEP 4  pull request opened, waiting for approval: " + p.PRURL
				}
				t.Logf("        status=%-20s step=%-14s", p.Status, p.CurrentStep)
				if msg != "" {
					t.Logf("%s", msg)
				}
			}
			if p.Status == model.StatusFailed {
				t.Fatalf("project failed: %s", p.ErrorMessage)
			}
			if p.Status == want {
				return p
			}
			time.Sleep(300 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s", want)
		return model.Project{}
	}
	p := waitFor(model.StatusWaitingForApproval)
	t.Logf("        repo: %s", p.RepoURL)
	t.Logf("        (the human reviews the PR here; the demo approves it now)")
	if err := orch.Approve(ctx, id, "u1"); err != nil {
		t.Fatal(err)
	}
	p = waitFor(model.StatusDeploying)
	for p.CurrentStep != model.StepMerged && ctx.Err() == nil {
		time.Sleep(200 * time.Millisecond)
		p = store.Get(id)
	}
	t.Logf("%s", say[model.StepMerged])

	// ---- GitHub Actions: observe the run through the API ----
	repoPath := strings.TrimPrefix(p.RepoURL, "https://github.com/")
	var run ghRun
	logged := map[string]string{}
	for ctx.Err() == nil {
		var runs struct {
			Runs []ghRun `json:"workflow_runs"`
		}
		if err := ghGet(ctx, token, "/repos/"+repoPath+"/actions/runs?event=push&branch=main", &runs); err != nil {
			t.Fatal(err)
		}
		if len(runs.Runs) > 0 {
			run = runs.Runs[0]
			if logged["run"] == "" {
				t.Logf("        workflow run: %s", run.HTMLURL)
			}
			var jobs struct {
				Jobs []struct {
					Steps []struct {
						Name       string `json:"name"`
						Status     string `json:"status"`
						Conclusion string `json:"conclusion"`
					} `json:"steps"`
				} `json:"jobs"`
			}
			if err := ghGet(ctx, token, fmt.Sprintf("/repos/%s/actions/runs/%d/jobs", repoPath, run.ID), &jobs); err == nil && len(jobs.Jobs) > 0 {
				for _, s := range jobs.Jobs[0].Steps {
					state := s.Status
					if s.Status == "completed" {
						state = s.Conclusion
					}
					if logged[s.Name] != state && (s.Status != "queued" && s.Status != "pending") {
						logged[s.Name] = state
						t.Logf("        actions: %-34s %s", s.Name, state)
					}
				}
			}
			logged["run"] = run.Status
			if run.Status == "completed" {
				break
			}
		}
		time.Sleep(8 * time.Second)
	}
	if run.Conclusion != "success" {
		t.Fatalf("workflow run ended %q: %s\n(if the failing step is the Google login, the Workload Identity provider or service account does not trust this repo yet: see docs/deploy-setup.md)", run.Conclusion, run.HTMLURL)
	}
	t.Logf("STEP 6  workflow succeeded: mock build, docker build + push, mock tests, Cloud Run deploy")

	// ---- the live app ----
	number := strings.Split(strings.TrimPrefix(creds.WIFProvider, "projects/"), "/")[0]
	url := fmt.Sprintf("https://%s-%s.%s.run.app", service.ServiceName(name, id), number, creds.Region)
	var code int
	var body string
	for i := 0; i < 40 && ctx.Err() == nil; i++ {
		if r, err := http.Get(url); err == nil {
			b, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
			r.Body.Close()
			code, body = r.StatusCode, string(b)
			if code == 200 {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	if code != 200 {
		t.Fatalf("GET %s -> %d, want 200", url, code)
	}
	t.Logf("STEP 7  LIVE  GET %s -> 200", url)
	t.Logf("        %.120s", strings.Join(strings.Fields(body), " "))
	t.Logf("DONE    repo %s | run %s | app %s", p.RepoURL, run.HTMLURL, url)
	t.Logf("        (nothing is deleted: remove the repo and the Cloud Run service %s when you are done)", service.ServiceName(name, id))
}

// TestLive_Platform drives a deployed platform over HTTP: the real demo.
func TestLive_Platform(t *testing.T) {
	base, apiKey, token := strings.TrimRight(os.Getenv("PLATFORM_URL"), "/"), os.Getenv("API_KEY"), os.Getenv("GITHUB_TOKEN")
	if base == "" {
		t.Skip("set PLATFORM_URL (the deployed platform), API_KEY and GITHUB_TOKEN to run the real demo")
	}
	if apiKey == "" || token == "" {
		t.Fatal("API_KEY and GITHUB_TOKEN are required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	call := func(method, path, user string, body any) (int, map[string]any) {
		t.Helper()
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequestWithContext(ctx, method, base+path, rdr)
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("Content-Type", "application/json")
		if user != "" {
			req.Header.Set("X-User-ID", user)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return resp.StatusCode, m
	}

	email := fmt.Sprintf("live-%d@example.com", time.Now().Unix())
	code, u := call("POST", "/users", "", map[string]string{"email": email, "name": "Live Demo"})
	uid, _ := u["ID"].(string)
	if uid == "" {
		uid, _ = u["id"].(string)
	}
	if code != 201 || uid == "" {
		t.Fatalf("create user: %d %v", code, u)
	}
	t.Logf("STEP 1  user %s created", uid)
	if code, m := call("POST", "/api/integrations/github", uid, map[string]string{"token": token}); code != 200 {
		t.Fatalf("connect github: %d %v", code, m)
	}
	t.Logf("STEP 2  GitHub connected")
	code, m := call("POST", "/api/projects", uid, map[string]string{"name": "Live Platform " + time.Now().Format("150405"), "prompt": "hello"})
	pid, _ := m["id"].(string)
	if code != 202 || pid == "" {
		t.Fatalf("create project: %d %v", code, m)
	}
	t.Logf("STEP 3  project %s created", pid)

	last := ""
	wait := func(want string) map[string]any {
		for ctx.Err() == nil {
			_, p := call("GET", "/api/projects/"+pid, uid, nil)
			st, step := fmt.Sprint(p["status"]), fmt.Sprint(p["current_step"])
			if cur := st + "|" + step; cur != last {
				last = cur
				t.Logf("        status=%-20s step=%-14s", st, step)
			}
			if st == model.StatusFailed {
				t.Fatalf("project failed: %v", p["error_message"])
			}
			if st == want {
				return p
			}
			time.Sleep(3 * time.Second)
		}
		t.Fatalf("timed out waiting for %s", want)
		return nil
	}
	p := wait(model.StatusWaitingForApproval)
	t.Logf("STEP 4  PR open: %v", p["pr_url"])
	if code, m := call("POST", "/api/projects/"+pid+"/approve", uid, nil); code != 202 {
		t.Fatalf("approve: %d %v", code, m)
	}
	t.Logf("STEP 5  approved; Actions is building, testing and deploying ...")
	p = wait(model.StatusSuccess)
	url := fmt.Sprint(p["live_url"])
	t.Logf("STEP 6  deployed: %s", url)
	var status int
	for i := 0; i < 20 && ctx.Err() == nil; i++ {
		if r, err := http.Get(url); err == nil {
			status = r.StatusCode
			r.Body.Close()
			if status == 200 {
				break
			}
		}
		time.Sleep(3 * time.Second)
	}
	if status != 200 {
		t.Fatalf("GET %s -> %d, want 200", url, status)
	}
	t.Logf("STEP 7  LIVE: GET %s -> 200", url)
}
