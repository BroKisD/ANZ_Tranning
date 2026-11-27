package handler_test

import (
	"anz-training/internal/testutil"
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
	"anz-training/service"
)

// ---- final results, authentication, idempotency ----

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
	a.store.Add(model.Project{ID: "p1", OwnerID: "u1", Name: "App", Prompt: "x", Status: model.StatusDeploying, CurrentStep: model.StepDeploying})
	return a, helper.WebhookSecretFor(rootSecret, "p1")
}

func TestWebhook_SuccessAndFailure(t *testing.T) {
	a, secret := deployingProj(t)
	if code, body := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.a.run.app"}); code != 200 {
		t.Fatalf("success: %d %s", code, body)
	}
	if p := a.store.Get("p1"); p.Status != model.StatusSuccess || p.LiveURL != "https://x.a.run.app" {
		t.Fatalf("project: %+v", p)
	}

	a2, secret := deployingProj(t)
	if code, _ := postHook(t, a2.srv, secret, map[string]string{"project_id": "p1", "status": "failed"}); code != 200 {
		t.Fatalf("failed: %d", code)
	}
	if p := a2.store.Get("p1"); p.Status != model.StatusFailed || p.ErrorMessage != "Cloud Run deployment failed" {
		t.Fatalf("default failure message missing: %+v", p)
	}

	a3, secret := deployingProj(t)
	postHook(t, a3.srv, secret, map[string]string{"project_id": "p1", "status": "failed", "error_message": strings.Repeat("e", 5000)})
	if n := len(a3.store.Get("p1").ErrorMessage); n != 1000 {
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
	if p := a.store.Get("p1"); p.Status != model.StatusDeploying {
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
	if p := a.store.Get("p1"); p.Status != model.StatusDeploying {
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
	if p := a.store.Get("p1"); p.Status != model.StatusSuccess || p.LiveURL != "https://x.run.app" {
		t.Fatalf("terminal state overwritten: %+v", p)
	}

	// a project that is not deploying yet cannot be finished by a webhook
	a.store.Add(model.Project{ID: "p2", OwnerID: "u1", Status: model.StatusWaitingForApproval})
	if code, _ := postHook(t, a.srv, helper.WebhookSecretFor(rootSecret, "p2"), map[string]string{"project_id": "p2", "status": "success", "live_url": "https://x.run.app"}); code != http.StatusConflict {
		t.Fatalf("webhook before deploy: got %d, want 409", code)
	}
	if a.store.Get("p2").Status != model.StatusWaitingForApproval {
		t.Fatal("project skipped the approval gate via webhook")
	}
}

func TestWebhook_Construction(t *testing.T) {
	if _, err := handler.NewWebhookHandler(testutil.NewMemStore(), ""); err == nil {
		t.Fatal("empty webhook secret must be rejected (an empty secret would authenticate anyone)")
	}
}

// ---- progress events and recovery ----

func mergedProj(t *testing.T, step string) (*projAPI, string) {
	t.Helper()
	a := newProjAPI(t, true)
	a.store.Add(model.Project{ID: "p1", OwnerID: "u1", Name: "App", Prompt: "x", Status: model.StatusDeploying, CurrentStep: step})
	return a, helper.WebhookSecretFor(rootSecret, "p1")
}

func TestWebhook_ProgressForward(t *testing.T) {
	a, secret := mergedProj(t, model.StepMerging)
	for _, step := range []string{"building", "testing", "deploying"} {
		if code, body := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "step": step}); code != http.StatusOK {
			t.Fatalf("step %s: %d %s", step, code, body)
		}
		if p := a.store.Get("p1"); p.CurrentStep != step || p.Status != model.StatusDeploying {
			t.Fatalf("after %s: status=%s step=%s", step, p.Status, p.CurrentStep)
		}
	}
}

func TestWebhook_ProgressNeverBackwards(t *testing.T) {
	a, secret := mergedProj(t, model.StepTesting)
	code, body := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "step": "building"})
	if code != http.StatusOK || !strings.Contains(body, "ignored") {
		t.Fatalf("late event: %d %s", code, body)
	}
	if code, _ := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "step": "testing"}); code != http.StatusOK { // repeat
		t.Fatalf("repeated event: %d", code)
	}
	if p := a.store.Get("p1"); p.CurrentStep != model.StepTesting {
		t.Fatalf("step moved backwards to %s", p.CurrentStep)
	}
}

func TestWebhook_ProgressIgnoredWhenNotDeploying(t *testing.T) {
	a := newProjAPI(t, true)
	a.store.Add(model.Project{ID: "p2", OwnerID: "u1", Status: model.StatusWaitingForApproval, CurrentStep: model.StepTestsPassed})
	code, _ := postHook(t, a.srv, helper.WebhookSecretFor(rootSecret, "p2"), map[string]string{"project_id": "p2", "step": "building"})
	if code != http.StatusConflict {
		t.Fatalf("progress before deploy: got %d, want 409", code)
	}
	if p := a.store.Get("p2"); p.Status != model.StatusWaitingForApproval || p.CurrentStep != model.StepTestsPassed {
		t.Fatalf("project changed: %+v", p)
	}
	if code, _ := postHook(t, a.srv, helper.WebhookSecretFor(rootSecret, "nope"), map[string]string{"project_id": "nope", "step": "building"}); code != http.StatusNotFound {
		t.Fatalf("unknown project: got %d, want 404", code)
	}
}

func TestWebhook_ProgressBadStepAndAuth(t *testing.T) {
	a, secret := mergedProj(t, model.StepMerging)
	for _, step := range []string{"hack", "merging", "repo_created", "BUILDING"} {
		if code, _ := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "step": step}); code != http.StatusBadRequest {
			t.Errorf("step %q: got %d, want 400", step, code)
		}
	}
	if code, _ := postHook(t, a.srv, "wrong", map[string]string{"project_id": "p1", "step": "building"}); code != http.StatusUnauthorized {
		t.Errorf("wrong secret: got %d", code)
	}
	if code, _ := postHook(t, a.srv, helper.WebhookSecretFor(rootSecret, "p9"), map[string]string{"project_id": "p1", "step": "building"}); code != http.StatusUnauthorized {
		t.Errorf("another project's secret: got %d", code)
	}
	if p := a.store.Get("p1"); p.CurrentStep != model.StepMerging {
		t.Fatalf("rejected events changed the project: %+v", p)
	}
}

func failedProj(t *testing.T, msg string) (*projAPI, string) {
	t.Helper()
	a := newProjAPI(t, true)
	a.store.Add(model.Project{ID: "p1", OwnerID: "u1", Name: "App", Prompt: "x", Status: model.StatusFailed, CurrentStep: model.StepBuilding, ErrorMessage: msg})
	return a, helper.WebhookSecretFor(rootSecret, "p1")
}

// The watchdog gives up on a quiet deploy; if the run then really finishes, the truth wins.
func TestWebhook_SuccessRecoversWatchdogFailure(t *testing.T) {
	a, secret := failedProj(t, service.WatchdogFailurePrefix+" for 15m0s")
	code, body := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.run.app"})
	if code != http.StatusOK {
		t.Fatalf("late success: %d %s", code, body)
	}
	if p := a.store.Get("p1"); p.Status != model.StatusSuccess || p.LiveURL != "https://x.run.app" || p.ErrorMessage != "" {
		t.Fatalf("project: %+v", p)
	}
}

func TestWebhook_SuccessDoesNotOverrideRealFailure(t *testing.T) {
	a, secret := failedProj(t, "Deployment failed at the build stage")
	if code, _ := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.run.app"}); code != http.StatusConflict {
		t.Fatalf("success after a real failure: got %d, want 409", code)
	}
	if p := a.store.Get("p1"); p.Status != model.StatusFailed {
		t.Fatalf("a real failure was overwritten: %+v", p)
	}
}

// ---- signalling the workflow ----

type fakeSignaler struct {
	mu      sync.Mutex
	signals []string // "projectID:signal"
	err     error
}

func (f *fakeSignaler) SignalDeploy(_ context.Context, projectID, signal string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, projectID+":"+signal)
	return f.err
}

// signalAPI serves only the webhook, with a signaler attached.
func signalAPI(t *testing.T, status, step string) (*httptest.Server, *testutil.MemStore, *fakeSignaler, string) {
	t.Helper()
	ms := testutil.NewMemStore()
	ms.Add(model.Project{ID: "p1", OwnerID: "u1", Name: "App", Prompt: "x", Status: status, CurrentStep: step})
	wh, err := handler.NewWebhookHandler(ms, rootSecret)
	if err != nil {
		t.Fatal(err)
	}
	sig := &fakeSignaler{}
	wh.Signaler = sig
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/webhooks/github", wh.GitHubWebhook)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s, ms, sig, helper.WebhookSecretFor(rootSecret, "p1")
}

func TestWebhook_ResultApplied_SignalsDeployResult(t *testing.T) {
	s, ms, sig, secret := signalAPI(t, model.StatusDeploying, model.StepDeploying)
	if code, body := postHook(t, s, secret, map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.a.run.app"}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if ms.Get("p1").Status != model.StatusSuccess {
		t.Fatal("MySQL must be written by the webhook")
	}
	if len(sig.signals) != 1 || sig.signals[0] != "p1:"+service.SignalDeployResult {
		t.Fatalf("signals = %v", sig.signals)
	}
}

func TestWebhook_Progress_SignalsDeployProgress_OnlyWhenTheStepAdvanced(t *testing.T) {
	s, _, sig, secret := signalAPI(t, model.StatusDeploying, model.StepMerged)
	post := func(step string) {
		t.Helper()
		if code, body := postHook(t, s, secret, map[string]string{"project_id": "p1", "step": step}); code != 200 {
			t.Fatalf("%s: %d %s", step, code, body)
		}
	}
	post(model.StepTesting)
	post(model.StepBuilding) // late: testing already happened, ignored
	post(model.StepTesting)  // repeated: ignored
	if len(sig.signals) != 1 || sig.signals[0] != "p1:"+service.SignalDeployProgress {
		t.Fatalf("signals = %v, want exactly one progress signal", sig.signals)
	}
}

func TestWebhook_DuplicateDelivery_StillOKAndSignalIsHarmless(t *testing.T) {
	s, _, sig, secret := signalAPI(t, model.StatusDeploying, model.StepDeploying)
	ok := map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.a.run.app"}
	if code, _ := postHook(t, s, secret, ok); code != 200 {
		t.Fatal("first delivery failed")
	}
	code, body := postHook(t, s, secret, ok)
	if code != 200 || !strings.Contains(body, "duplicate") {
		t.Fatalf("duplicate: %d %s", code, body)
	}
	for _, sg := range sig.signals {
		if sg != "p1:"+service.SignalDeployResult {
			t.Fatalf("unexpected signal %q", sg)
		}
	}
}

func TestWebhook_Conflict_DoesNotSignal(t *testing.T) {
	s, _, sig, secret := signalAPI(t, model.StatusSuccess, model.StepDeploying)
	if code, _ := postHook(t, s, secret, map[string]string{"project_id": "p1", "status": "failed"}); code != 409 {
		t.Fatalf("code = %d, want 409", code)
	}
	if len(sig.signals) != 0 {
		t.Fatalf("a rejected result must not signal: %v", sig.signals)
	}
}

func TestWebhook_SignalError_DoesNotFailTheRequest(t *testing.T) {
	s, ms, sig, secret := signalAPI(t, model.StatusDeploying, model.StepDeploying)
	sig.err = errors.New("temporal unreachable")
	if code, body := postHook(t, s, secret, map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.a.run.app"}); code != 200 {
		t.Fatalf("%d %s", code, body)
	}
	if ms.Get("p1").Status != model.StatusSuccess {
		t.Fatal("MySQL must still be updated when the signal fails")
	}
}
