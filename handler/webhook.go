package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"

	"anz-training/helper"
	"anz-training/model"
	"anz-training/service"
)

const (
	maxWebhookBody       = 64 << 10
	maxWebhookURL        = 500
	defaultDeployFailure = "Cloud Run deployment failed"
)

// WebhookStore is the persistence the deploy webhook needs.
type WebhookStore interface {
	GetProject(ctx context.Context, id string) (*model.Project, error)
	UpdateProjectIf(ctx context.Context, id, fromStatus string, fields map[string]any) (bool, error)
}

// WebhookHandler receives the deploy result posted by the generated repo's
// GitHub Actions workflow.
type WebhookHandler struct {
	store      WebhookStore
	rootSecret string
}

func NewWebhookHandler(store WebhookStore, rootSecret string) (*WebhookHandler, error) {
	if rootSecret == "" {
		return nil, errors.New("webhook secret must not be empty")
	}
	return &WebhookHandler{store: store, rootSecret: rootSecret}, nil
}

type webhookPayload struct {
	ProjectID    string `json:"project_id"`
	Step         string `json:"step"` // progress event: building | testing | deploying
	Status       string `json:"status"`
	LiveURL      string `json:"live_url"`
	ErrorMessage string `json:"error_message"`
}

// secretMatches compares in constant time. Both sides are hashed first so the
// comparison also takes the same time whatever the supplied length is.
func secretMatches(got, want string) bool {
	a, b := sha256.Sum256([]byte(got)), sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// GitHubWebhook handles POST /api/webhooks/github.
//
// The X-Webhook-Secret header must equal the secret derived for the payload's
// project, so the secret visible in one repo's workflow cannot forge another
// project's result. Updates are idempotent: only a `deploying` project moves,
// a repeated delivery of the same result is acknowledged, and a conflicting one
// is rejected.
func (h *WebhookHandler) GitHubWebhook(w http.ResponseWriter, r *http.Request) {
	var p webhookPayload
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxWebhookBody)).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	// authenticate before touching the database or revealing whether a project exists
	want := helper.WebhookSecretFor(h.rootSecret, p.ProjectID)
	if p.ProjectID == "" || len(p.ProjectID) > 64 || want == "" ||
		!secretMatches(r.Header.Get("X-Webhook-Secret"), want) {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	if p.Status == "" && p.Step != "" {
		h.progress(w, r, p)
		return
	}

	fields := map[string]any{}
	switch p.Status {
	case model.StatusSuccess:
		if !validLiveURL(p.LiveURL) {
			writeErr(w, http.StatusBadRequest, "live_url must be a valid https URL")
			return
		}
		fields["status"], fields["live_url"] = model.StatusSuccess, p.LiveURL
	case model.StatusFailed:
		msg := strings.TrimSpace(p.ErrorMessage)
		if msg == "" {
			msg = defaultDeployFailure
		}
		if len(msg) > maxErrorMessage {
			msg = msg[:maxErrorMessage]
		}
		fields["status"], fields["error_message"] = model.StatusFailed, msg
	default:
		writeErr(w, http.StatusBadRequest, `status must be "success" or "failed"`)
		return
	}

	applied, err := h.store.UpdateProjectIf(r.Context(), p.ProjectID, model.StatusDeploying, fields)
	if err != nil {
		log.Printf("webhook update %s: %v", p.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if applied {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}

	cur, err := h.store.GetProject(r.Context(), p.ProjectID)
	switch {
	case errors.Is(err, model.ErrNotFound):
		writeErr(w, http.StatusNotFound, "project not found")
	case err != nil:
		log.Printf("webhook load %s: %v", p.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	case cur.Status == p.Status: // duplicate delivery
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "duplicate": true})
	case p.Status == model.StatusSuccess && cur.Status == model.StatusFailed && strings.HasPrefix(cur.ErrorMessage, service.WatchdogFailurePrefix):
		// the watchdog gave up on a slow deploy that then really finished: the truth wins
		fields["error_message"] = ""
		ok, err := h.store.UpdateProjectIf(r.Context(), p.ProjectID, model.StatusFailed, fields)
		if err != nil {
			log.Printf("webhook recover %s: %v", p.ProjectID, err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
		if !ok {
			writeErr(w, http.StatusConflict, "project is not awaiting a deploy result")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "recovered": true})
	default:
		writeErr(w, http.StatusConflict, "project is not awaiting a deploy result")
	}
}

// progressSteps are the stages a deploy workflow reports while it runs.
var progressSteps = map[string]bool{model.StepBuilding: true, model.StepTesting: true, model.StepDeploying: true}

// progress records a stage of a running deploy. Events only move the project
// forward (late or repeated ones are acknowledged and ignored) and only while it
// is deploying.
func (h *WebhookHandler) progress(w http.ResponseWriter, r *http.Request, p webhookPayload) {
	if !progressSteps[p.Step] {
		writeErr(w, http.StatusBadRequest, "step must be building, testing or deploying")
		return
	}
	cur, err := h.store.GetProject(r.Context(), p.ProjectID)
	switch {
	case errors.Is(err, model.ErrNotFound):
		writeErr(w, http.StatusNotFound, "project not found")
		return
	case err != nil:
		log.Printf("webhook load %s: %v", p.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	case cur.Status != model.StatusDeploying:
		writeErr(w, http.StatusConflict, "project is not deploying")
		return
	case service.StepRank(p.Step) <= service.StepRank(cur.CurrentStep):
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "ignored": true})
		return
	}
	if _, err := h.store.UpdateProjectIf(r.Context(), p.ProjectID, model.StatusDeploying, map[string]any{"current_step": p.Step}); err != nil {
		log.Printf("webhook progress %s: %v", p.ProjectID, err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

const maxErrorMessage = 1000

// validLiveURL accepts only absolute https URLs: the value is rendered as a
// clickable link, so javascript:/data: URLs must never get through.
func validLiveURL(raw string) bool {
	if raw == "" || len(raw) > maxWebhookURL {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}
