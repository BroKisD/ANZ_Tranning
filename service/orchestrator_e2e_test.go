package service_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"anz-training/handler"
	"anz-training/helper"
	"anz-training/internal/testutil"
)

const e2eRootSecret = "root-webhook-secret"

// e2eStore combines the in-memory project and integration stores.
type e2eStore struct {
	*testutil.MemStore
	*testutil.MemIntegrations
}

// call sends one JSON request as the given user (X-User-ID) and returns status and body.
func call(t *testing.T, s *httptest.Server, method, path, user, secret string, body any) (int, string) {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest(method, s.URL+path, bytes.NewReader(b))
	if user != "" {
		req.Header.Set("X-User-ID", user)
	}
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

func jsonMap(t *testing.T, body string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("not json: %q (%v)", body, err)
	}
	return m
}

// Handlers + real orchestrator + deploy webhook, end to end.
func TestOrchestrator_EndToEndThroughHandlers(t *testing.T) {
	e := newOrc(t, nil)
	store := &e2eStore{e.store, e.ints}
	ph := handler.NewProjectHandler(store, e.o, handler.HeaderUserID)
	wh, err := handler.NewWebhookHandler(store, e2eRootSecret)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/projects", ph.Create)
	mux.HandleFunc("GET /api/projects/{id}", ph.Get)
	mux.HandleFunc("POST /api/projects/{id}/approve", ph.Approve)
	mux.HandleFunc("POST /api/webhooks/github", wh.GitHubWebhook)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	code, body := call(t, srv, "POST", "/api/projects", "u1", "", map[string]string{"name": "Todo", "prompt": "todo app"})
	if code != 202 {
		t.Fatalf("create: %d %s", code, body)
	}
	id := jsonMap(t, body)["id"].(string)

	get := func() map[string]any {
		_, b := call(t, srv, "GET", "/api/projects/"+id, "u1", "", nil)
		return jsonMap(t, b)
	}
	waitFor(t, "waiting_for_approval", func() bool { return get()["status"] == "waiting_for_approval" })
	if m := get(); m["pr_url"] == nil || m["repo_url"] == nil || m["current_step"] != "tests_passed" {
		t.Fatalf("at gate: %v", m)
	}

	if code, _ := call(t, srv, "POST", "/api/projects/"+id+"/approve", "u2", "", nil); code != 404 {
		t.Fatalf("approve by other user: %d", code)
	}
	if e.host.count("merge") != 0 {
		t.Fatal("merged on a rejected approval")
	}
	if code, _ := call(t, srv, "POST", "/api/projects/"+id+"/approve", "u1", "", nil); code != 202 {
		t.Fatalf("approve: %d", code)
	}
	waitFor(t, "deploying", func() bool { return get()["status"] == "deploying" })

	// the generated repo's workflow reports back
	secret := helper.WebhookSecretFor(e2eRootSecret, id)
	hook := map[string]string{"project_id": id, "status": "success", "live_url": "https://todo-abc.a.run.app"}
	if code, body := call(t, srv, "POST", "/api/webhooks/github", "", secret, hook); code != 200 {
		t.Fatalf("webhook: %d %s", code, body)
	}
	if m := get(); m["status"] != "success" || m["live_url"] != "https://todo-abc.a.run.app" {
		t.Fatalf("final: %v", m)
	}
	if code, body := call(t, srv, "POST", "/api/webhooks/github", "", secret, hook); code != 200 || !strings.Contains(body, "duplicate") {
		t.Fatalf("duplicate delivery: %d %s", code, body)
	}
}
