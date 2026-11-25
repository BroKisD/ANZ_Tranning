package test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"anz-training/handler"
	"anz-training/helper"
	"anz-training/model"
	"anz-training/repository"
	"anz-training/service"
)

var (
	_ handler.ProjectStore  = (*repository.Store)(nil)
	_ handler.WebhookStore  = (*repository.Store)(nil)
	_ handler.ProjectRunner = (*service.Orchestrator)(nil)
)

const rootSecret = "root-webhook-secret"

// projStore combines the in-memory project and integration stores.
type projStore struct {
	*memStore
	*memIntegrations
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
	ms := &memStore{projects: map[string]*model.Project{}}
	mi := &memIntegrations{rows: map[string]model.UserIntegration{}}
	if withIntegrations {
		mi.put(t, testKey(5), "u1", model.ProviderGitHub, model.GitHubCreds{Owner: "acme", Token: "ghp_x"})
		mi.put(t, testKey(5), "u1", model.ProviderGCP, gcpCreds)
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
	p := a.store.get(id)
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
	if len(a.store.projects) != 0 || len(a.runner.started) != 0 {
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
	for _, p := range a.store.projects {
		if p.Status != model.StatusFailed {
			t.Fatalf("project left in %s after a failed start", p.Status)
		}
	}
}

// ---- GET /api/projects/{id} ----

func TestProjects_Get(t *testing.T) {
	a := newProjAPI(t, true)
	a.store.add(model.Project{
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

// ---- full flow: handlers + real orchestrator + webhook ----

func TestProjects_EndToEnd(t *testing.T) {
	e := newOrc(t, nil)
	srv := newProjectServer(t, &projStore{e.store, e.ints}, e.o)

	code, body := do(t, srv, "POST", "/api/projects", "u1", map[string]string{"name": "Todo", "prompt": "todo app"})
	if code != 202 {
		t.Fatalf("create: %d %s", code, body)
	}
	id := decode(t, body)["id"].(string)

	status := func() string {
		_, b := do(t, srv, "GET", "/api/projects/"+id, "u1", nil)
		s, _ := decode(t, b)["status"].(string)
		return s
	}
	waitFor(t, "waiting_for_approval", func() bool { return status() == "waiting_for_approval" })
	_, b := do(t, srv, "GET", "/api/projects/"+id, "u1", nil)
	m := decode(t, b)
	if m["pr_url"] == nil || m["repo_url"] == nil || m["current_step"] != "tests_passed" {
		t.Fatalf("at gate: %v", m)
	}

	if code, _ := do(t, srv, "POST", "/api/projects/"+id+"/approve", "u2", nil); code != 404 {
		t.Fatalf("approve by other user: %d", code)
	}
	if e.host.count("merge") != 0 {
		t.Fatal("merged on a rejected approval")
	}
	if code, _ := do(t, srv, "POST", "/api/projects/"+id+"/approve", "u1", nil); code != 202 {
		t.Fatalf("approve: %d", code)
	}
	waitFor(t, "deploying", func() bool { return status() == "deploying" })

	// the generated repo's workflow reports back
	secret := helper.WebhookSecretFor(rootSecret, id)
	hook := map[string]string{"project_id": id, "status": "success", "live_url": "https://todo-abc.a.run.app"}
	if code, body := postHook(t, srv, secret, hook); code != 200 {
		t.Fatalf("webhook: %d %s", code, body)
	}
	_, b = do(t, srv, "GET", "/api/projects/"+id, "u1", nil)
	m = decode(t, b)
	if m["status"] != "success" || m["live_url"] != "https://todo-abc.a.run.app" {
		t.Fatalf("final: %v", m)
	}
	if code, body := postHook(t, srv, secret, hook); code != 200 || !strings.Contains(body, "duplicate") {
		t.Fatalf("duplicate delivery: %d %s", code, body)
	}
}

// ---- webhook ----

func postHook(t *testing.T, s *httptest.Server, secret string, payload any) (int, string) {
	t.Helper()
	var rdr *bytes.Reader
	if raw, ok := payload.([]byte); ok {
		rdr = bytes.NewReader(raw)
	} else {
		b, _ := json.Marshal(payload)
		rdr = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", s.URL+"/api/webhooks/github", rdr)
	if secret != "" {
		req.Header.Set("X-Webhook-Secret", secret)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func deployingProj(t *testing.T) (*projAPI, string) {
	t.Helper()
	a := newProjAPI(t, true)
	a.store.add(model.Project{ID: "p1", OwnerID: "u1", Name: "App", Prompt: "x", Status: model.StatusDeploying, CurrentStep: model.StepDeploying})
	return a, helper.WebhookSecretFor(rootSecret, "p1")
}

func TestWebhook_SuccessAndFailure(t *testing.T) {
	a, secret := deployingProj(t)
	if code, body := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.a.run.app"}); code != 200 {
		t.Fatalf("success: %d %s", code, body)
	}
	if p := a.store.get("p1"); p.Status != model.StatusSuccess || p.LiveURL != "https://x.a.run.app" {
		t.Fatalf("project: %+v", p)
	}

	a2, secret := deployingProj(t)
	if code, _ := postHook(t, a2.srv, secret, map[string]string{"project_id": "p1", "status": "failed"}); code != 200 {
		t.Fatalf("failed: %d", code)
	}
	if p := a2.store.get("p1"); p.Status != model.StatusFailed || p.ErrorMessage != "Cloud Run deployment failed" {
		t.Fatalf("default failure message missing: %+v", p)
	}

	a3, secret := deployingProj(t)
	postHook(t, a3.srv, secret, map[string]string{"project_id": "p1", "status": "failed", "error_message": strings.Repeat("e", 5000)})
	if n := len(a3.store.get("p1").ErrorMessage); n != 1000 {
		t.Fatalf("error message length = %d, want 1000", n)
	}
}

func TestWebhook_Authentication(t *testing.T) {
	a, secret := deployingProj(t)
	hook := map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.run.app"}

	other := helper.WebhookSecretFor(rootSecret, "someone-else")
	for name, s := range map[string]string{
		"missing":                      "",
		"wrong":                        "nope",
		"root secret itself":           rootSecret,
		"another project's secret":     other,
		"prefix of the right secret":   secret[:len(secret)-1],
		"right secret with extra char": secret + "x",
	} {
		if code, _ := postHook(t, a.srv, s, hook); code != http.StatusUnauthorized {
			t.Errorf("%s: got %d, want 401", name, code)
		}
	}
	if p := a.store.get("p1"); p.Status != model.StatusDeploying {
		t.Fatalf("unauthenticated call changed the project: %+v", p)
	}
	// a valid secret for an unknown project id reveals nothing before auth, then 404s
	ghost := helper.WebhookSecretFor(rootSecret, "ghost")
	if code, _ := postHook(t, a.srv, ghost, map[string]string{"project_id": "ghost", "status": "success", "live_url": "https://x.run.app"}); code != 404 {
		t.Errorf("unknown project: got %d, want 404", code)
	}
	if code, _ := postHook(t, a.srv, other, map[string]string{"project_id": "ghost", "status": "success", "live_url": "https://x.run.app"}); code != 401 {
		t.Errorf("unknown project with wrong secret must be 401 (no existence oracle), got %d", code)
	}
	if code, _ := postHook(t, a.srv, secret, map[string]string{"status": "success"}); code != 401 {
		t.Errorf("missing project_id: got %d, want 401", code)
	}
}

func TestWebhook_Validation(t *testing.T) {
	a, secret := deployingProj(t)
	bad := map[string]map[string]string{
		"unknown status": {"project_id": "p1", "status": "deploying"},
		"empty status":   {"project_id": "p1"},
		"success no url": {"project_id": "p1", "status": "success"},
		"javascript url": {"project_id": "p1", "status": "success", "live_url": "javascript:alert(1)"},
		"data url":       {"project_id": "p1", "status": "success", "live_url": "data:text/html,x"},
		"plain http":     {"project_id": "p1", "status": "success", "live_url": "http://x.run.app"},
		"userinfo url":   {"project_id": "p1", "status": "success", "live_url": "https://user:pw@x.run.app"},
		"relative url":   {"project_id": "p1", "status": "success", "live_url": "/x"},
		"too long url":   {"project_id": "p1", "status": "success", "live_url": "https://x.run.app/" + strings.Repeat("a", 600)},
	}
	for name, payload := range bad {
		if code, _ := postHook(t, a.srv, secret, payload); code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", name, code)
		}
	}
	if code, _ := postHook(t, a.srv, secret, []byte("{nope")); code != http.StatusBadRequest {
		t.Errorf("invalid json: got %d", code)
	}
	if code, _ := postHook(t, a.srv, secret, []byte(`{"project_id":"p1","error_message":"`+strings.Repeat("x", 200000)+`"}`)); code != http.StatusBadRequest {
		t.Errorf("oversized body: got %d", code)
	}
	if p := a.store.get("p1"); p.Status != model.StatusDeploying {
		t.Fatalf("invalid webhook changed the project: %+v", p)
	}
}

func TestWebhook_IdempotentAndStateGuard(t *testing.T) {
	a, secret := deployingProj(t)
	ok := map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.run.app"}

	if code, _ := postHook(t, a.srv, secret, ok); code != 200 {
		t.Fatal("first delivery")
	}
	if code, body := postHook(t, a.srv, secret, ok); code != 200 || !strings.Contains(body, "duplicate") {
		t.Fatalf("retry of the same result: %d %s", code, body)
	}
	// a later conflicting result must not overwrite a terminal state
	if code, _ := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "status": "failed"}); code != http.StatusConflict {
		t.Fatalf("conflicting result: got %d, want 409", code)
	}
	if p := a.store.get("p1"); p.Status != model.StatusSuccess || p.LiveURL != "https://x.run.app" {
		t.Fatalf("terminal state overwritten: %+v", p)
	}

	// a project that is not deploying yet cannot be finished by a webhook
	a.store.add(model.Project{ID: "p2", OwnerID: "u1", Status: model.StatusWaitingForApproval})
	if code, _ := postHook(t, a.srv, helper.WebhookSecretFor(rootSecret, "p2"), map[string]string{"project_id": "p2", "status": "success", "live_url": "https://x.run.app"}); code != http.StatusConflict {
		t.Fatalf("webhook before deploy: got %d, want 409", code)
	}
	if a.store.get("p2").Status != model.StatusWaitingForApproval {
		t.Fatal("project skipped the approval gate via webhook")
	}
}

func TestWebhook_Construction(t *testing.T) {
	if _, err := handler.NewWebhookHandler(&memStore{}, ""); err == nil {
		t.Fatal("empty webhook secret must be rejected (an empty secret would authenticate anyone)")
	}
}

func TestWebhookSecretFor(t *testing.T) {
	a, b := helper.WebhookSecretFor("root", "p1"), helper.WebhookSecretFor("root", "p2")
	if a == "" || a == b || a != helper.WebhookSecretFor("root", "p1") {
		t.Fatal("secret must be deterministic and per-project")
	}
	if strings.Contains(a, "root") || helper.WebhookSecretFor("other", "p1") == a {
		t.Fatal("secret must depend on the root secret and not reveal it")
	}
	if helper.WebhookSecretFor("", "p1") != "" {
		t.Fatal("empty root secret must give an empty (rejected) secret")
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
