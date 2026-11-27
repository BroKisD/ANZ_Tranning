package handler_test

import (
	"anz-training/internal/testutil"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"anz-training/handler"
	"anz-training/model"
	"anz-training/repository"
	"anz-training/service"
)

var (
	_ handler.ProjectStore  = (*repository.Store)(nil)
	_ handler.WebhookStore  = (*repository.Store)(nil)
	_ handler.ProjectRunner = (*service.TemporalRunner)(nil)
)

const rootSecret = "root-webhook-secret"

// projStore combines the in-memory project and integration stores.
type projStore struct {
	*testutil.MemStore
	*testutil.MemIntegrations
}

type fakeRunner struct {
	mu          sync.Mutex
	started     []string
	startErr    error
	approveErr  error
	approvals   [][2]string
	redeployed  [][2]string
	redeployErr error
}

func (f *fakeRunner) Start(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, id)
	return f.startErr
}

func (f *fakeRunner) Approve(_ context.Context, id, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.approvals = append(f.approvals, [2]string{id, owner})
	return f.approveErr
}

func (f *fakeRunner) Redeploy(_ context.Context, id, owner string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.redeployed = append(f.redeployed, [2]string{id, owner})
	return f.redeployErr
}

type projAPI struct {
	store  *projStore
	runner *fakeRunner
	srv    *httptest.Server
}

func newProjAPI(t *testing.T, withIntegrations bool) *projAPI {
	t.Helper()
	ms := testutil.NewMemStore()
	mi := testutil.NewMemIntegrations()
	if withIntegrations {
		mi.Put(t, testutil.TestKey(5), "u1", model.ProviderGitHub, model.GitHubCreds{Owner: "acme", Token: "ghp_x"})
		mi.Put(t, testutil.TestKey(5), "u1", model.ProviderGCP, testutil.GCPCreds)
	}
	a := &projAPI{store: &projStore{ms, mi}, runner: &fakeRunner{}}
	a.srv = newProjectServer(t, a.store, a.runner)
	return a
}

func newProjectServer(t *testing.T, store *projStore, runner handler.ProjectRunner) *httptest.Server {
	t.Helper()
	ph := handler.NewProjectHandler(store, runner, handler.HeaderUserID)
	wh, err := handler.NewWebhookHandler(store, rootSecret)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/projects", ph.Create)
	mux.HandleFunc("GET /api/projects/{id}", ph.Get)
	mux.HandleFunc("POST /api/projects/{id}/approve", ph.Approve)
	mux.HandleFunc("POST /api/projects/{id}/deploy", ph.Deploy)
	mux.HandleFunc("POST /api/webhooks/github", wh.GitHubWebhook)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func decode(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("not json: %q (%v)", body, err)
	}
	return m
}

// ---- POST /api/projects ----

func TestProjects_Create(t *testing.T) {
	a := newProjAPI(t, true)
	code, body := do(t, a.srv, "POST", "/api/projects", "u1", map[string]string{"name": "  My App ", "prompt": " todo list "})
	if code != http.StatusAccepted {
		t.Fatalf("got %d %s", code, body)
	}
	resp := decode(t, body)
	id, _ := resp["id"].(string)
	if id == "" || resp["status"] != "pending" || resp["current_step"] != "initializing" {
		t.Fatalf("response = %v", resp)
	}
	p := a.store.Get(id)
	if p.OwnerID != "u1" || p.Name != "My App" || p.Prompt != "todo list" || p.Status != model.StatusPending || p.BranchName != "main" {
		t.Fatalf("stored project = %+v", p)
	}
	if len(a.runner.started) != 1 || a.runner.started[0] != id {
		t.Fatalf("runner.Start calls = %v", a.runner.started)
	}
}

func TestProjects_CreateValidation(t *testing.T) {
	a := newProjAPI(t, true)
	cases := map[string]struct {
		user string
		body any
		want int
	}{
		"no user":        {"", map[string]string{"name": "a", "prompt": "b"}, 401},
		"missing name":   {"u1", map[string]string{"prompt": "b"}, 422},
		"blank prompt":   {"u1", map[string]string{"name": "a", "prompt": "   "}, 422},
		"long name":      {"u1", map[string]string{"name": strings.Repeat("n", 101), "prompt": "b"}, 422},
		"long prompt":    {"u1", map[string]string{"name": "a", "prompt": strings.Repeat("p", 10001)}, 422},
		"no integration": {"u2", map[string]string{"name": "a", "prompt": "b"}, 422},
	}
	for name, c := range cases {
		if code, body := do(t, a.srv, "POST", "/api/projects", c.user, c.body); code != c.want {
			t.Errorf("%s: got %d (%s), want %d", name, code, body, c.want)
		}
	}
	req, _ := http.NewRequest("POST", a.srv.URL+"/api/projects", strings.NewReader("{not json"))
	req.Header.Set("X-User-ID", "u1")
	resp, err := a.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("bad json: got %d", resp.StatusCode)
	}
	if len(a.store.Projects) != 0 || len(a.runner.started) != 0 {
		t.Fatal("invalid requests must not create or start anything")
	}
}

func TestProjects_StartFailureMarksFailed(t *testing.T) {
	a := newProjAPI(t, true)
	a.runner.startErr = errors.New("boom")
	code, _ := do(t, a.srv, "POST", "/api/projects", "u1", map[string]string{"name": "a", "prompt": "b"})
	if code != http.StatusInternalServerError {
		t.Fatalf("got %d", code)
	}
	for _, p := range a.store.Projects {
		if p.Status != model.StatusFailed {
			t.Fatalf("project left in %s after a failed start", p.Status)
		}
	}
}

// ---- GET /api/projects/{id} ----

func TestProjects_Get(t *testing.T) {
	a := newProjAPI(t, true)
	a.store.Add(model.Project{
		ID: "p1", OwnerID: "u1", Name: "App", Prompt: "SECRET PROMPT", Status: model.StatusSuccess,
		CurrentStep: model.StepDeploying, RepoURL: "https://github.com/acme/r", PRURL: "https://github.com/acme/r/pull/1",
		LiveURL: "https://app.run.app", ErrorMessage: "",
	})
	code, body := do(t, a.srv, "GET", "/api/projects/p1", "u1", nil)
	if code != 200 {
		t.Fatalf("got %d %s", code, body)
	}
	m := decode(t, body)
	for k, want := range map[string]string{
		"id": "p1", "status": "success", "current_step": "deploying", "repo_url": "https://github.com/acme/r",
		"pr_url": "https://github.com/acme/r/pull/1", "live_url": "https://app.run.app",
	} {
		if m[k] != want {
			t.Errorf("%s = %v, want %s", k, m[k], want)
		}
	}
	if strings.Contains(body, "SECRET PROMPT") || strings.Contains(body, "owner") {
		t.Fatalf("response leaks internal fields: %s", body)
	}

	if code, _ := do(t, a.srv, "GET", "/api/projects/p1", "u2", nil); code != 404 {
		t.Errorf("other user: got %d, want 404", code)
	}
	if code, _ := do(t, a.srv, "GET", "/api/projects/nope", "u1", nil); code != 404 {
		t.Errorf("unknown: got %d, want 404", code)
	}
	if code, _ := do(t, a.srv, "GET", "/api/projects/p1", "", nil); code != 401 {
		t.Errorf("no user: got %d, want 401", code)
	}
}

// ---- POST /api/projects/{id}/approve ----

func TestProjects_Approve(t *testing.T) {
	a := newProjAPI(t, true)
	for errIn, want := range map[error]int{
		nil:                       http.StatusAccepted,
		model.ErrNotFound:         http.StatusNotFound,
		service.ErrNotWaiting:     http.StatusConflict,
		service.ErrNoPendingRun:   http.StatusConflict,
		errors.New("db exploded"): http.StatusInternalServerError,
	} {
		a.runner.approveErr = errIn
		code, body := do(t, a.srv, "POST", "/api/projects/p1/approve", "u1", nil)
		if code != want {
			t.Errorf("approve err=%v: got %d (%s), want %d", errIn, code, body, want)
		}
		if strings.Contains(body, "db exploded") {
			t.Error("internal error text leaked to the client")
		}
	}
	last := a.runner.approvals[len(a.runner.approvals)-1]
	if last != [2]string{"p1", "u1"} {
		t.Fatalf("runner got %v", last)
	}
	if code, _ := do(t, a.srv, "POST", "/api/projects/p1/approve", "", nil); code != 401 {
		t.Errorf("no user: got %d", code)
	}
}

// ---- POST /api/projects/{id}/deploy ----

func TestProjects_Deploy(t *testing.T) {
	a := newProjAPI(t, true)
	if code, _ := do(t, a.srv, "POST", "/api/projects/p1/deploy", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no user: %d", code)
	}
	code, body := do(t, a.srv, "POST", "/api/projects/p1/deploy", "u1", nil)
	if code != http.StatusAccepted || decode(t, body)["status"] != "deploying" {
		t.Fatalf("ok: %d %s", code, body)
	}
	if len(a.runner.redeployed) != 1 || a.runner.redeployed[0] != [2]string{"p1", "u1"} {
		t.Fatalf("redeployed = %v", a.runner.redeployed)
	}
	for err, want := range map[error]int{
		model.ErrNotFound:          http.StatusNotFound,
		service.ErrNotRedeployable: http.StatusConflict,
		errors.New("boom"):         http.StatusInternalServerError,
	} {
		a.runner.redeployErr = err
		if code, _ := do(t, a.srv, "POST", "/api/projects/p1/deploy", "u1", nil); code != want {
			t.Errorf("%v: got %d, want %d", err, code, want)
		}
	}
}
