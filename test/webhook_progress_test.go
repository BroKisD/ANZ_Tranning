package test

import (
	"net/http"
	"strings"
	"testing"

	"anz-training/helper"
	"anz-training/model"
	"anz-training/service"
)

func mergedProj(t *testing.T, step string) (*projAPI, string) {
	t.Helper()
	a := newProjAPI(t, true)
	a.store.add(model.Project{ID: "p1", OwnerID: "u1", Name: "App", Prompt: "x", Status: model.StatusDeploying, CurrentStep: step})
	return a, helper.WebhookSecretFor(rootSecret, "p1")
}

func TestWebhook_ProgressForward(t *testing.T) {
	a, secret := mergedProj(t, model.StepMerging)
	for _, step := range []string{"building", "testing", "deploying"} {
		if code, body := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "step": step}); code != http.StatusOK {
			t.Fatalf("step %s: %d %s", step, code, body)
		}
		if p := a.store.get("p1"); p.CurrentStep != step || p.Status != model.StatusDeploying {
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
	if p := a.store.get("p1"); p.CurrentStep != model.StepTesting {
		t.Fatalf("step moved backwards to %s", p.CurrentStep)
	}
}

func TestWebhook_ProgressIgnoredWhenNotDeploying(t *testing.T) {
	a := newProjAPI(t, true)
	a.store.add(model.Project{ID: "p2", OwnerID: "u1", Status: model.StatusWaitingForApproval, CurrentStep: model.StepTestsPassed})
	code, _ := postHook(t, a.srv, helper.WebhookSecretFor(rootSecret, "p2"), map[string]string{"project_id": "p2", "step": "building"})
	if code != http.StatusConflict {
		t.Fatalf("progress before deploy: got %d, want 409", code)
	}
	if p := a.store.get("p2"); p.Status != model.StatusWaitingForApproval || p.CurrentStep != model.StepTestsPassed {
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
	if p := a.store.get("p1"); p.CurrentStep != model.StepMerging {
		t.Fatalf("rejected events changed the project: %+v", p)
	}
}

func failedProj(t *testing.T, msg string) (*projAPI, string) {
	t.Helper()
	a := newProjAPI(t, true)
	a.store.add(model.Project{ID: "p1", OwnerID: "u1", Name: "App", Prompt: "x", Status: model.StatusFailed, CurrentStep: model.StepBuilding, ErrorMessage: msg})
	return a, helper.WebhookSecretFor(rootSecret, "p1")
}

// The watchdog gives up on a quiet deploy; if the run then really finishes, the truth wins.
func TestWebhook_SuccessRecoversWatchdogFailure(t *testing.T) {
	a, secret := failedProj(t, service.WatchdogFailurePrefix+" for 15m0s")
	code, body := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.run.app"})
	if code != http.StatusOK {
		t.Fatalf("late success: %d %s", code, body)
	}
	if p := a.store.get("p1"); p.Status != model.StatusSuccess || p.LiveURL != "https://x.run.app" || p.ErrorMessage != "" {
		t.Fatalf("project: %+v", p)
	}
}

func TestWebhook_SuccessDoesNotOverrideRealFailure(t *testing.T) {
	a, secret := failedProj(t, "Deployment failed at the build stage")
	if code, _ := postHook(t, a.srv, secret, map[string]string{"project_id": "p1", "status": "success", "live_url": "https://x.run.app"}); code != http.StatusConflict {
		t.Fatalf("success after a real failure: got %d, want 409", code)
	}
	if p := a.store.get("p1"); p.Status != model.StatusFailed {
		t.Fatalf("a real failure was overwritten: %+v", p)
	}
}
