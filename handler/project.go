package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"anz-training/helper"
	"anz-training/model"
	"anz-training/service"
)

const (
	maxProjectBody   = 64 << 10
	maxProjectName   = 100
	maxProjectPrompt = 10000
)

// ProjectStore is the persistence the project handlers need.
type ProjectStore interface {
	CreateProject(ctx context.Context, p *model.Project) error
	GetProject(ctx context.Context, id string) (*model.Project, error)
	UpdateProject(ctx context.Context, id string, fields map[string]any) error
	GetIntegration(ctx context.Context, userID, provider string) (*model.UserIntegration, error)
}

// ProjectRunner is the orchestrator.
type ProjectRunner interface {
	Start(projectID string) error
	Approve(ctx context.Context, projectID, ownerID string) error
	Redeploy(ctx context.Context, projectID, ownerID string) error
}

type ProjectHandler struct {
	store  ProjectStore
	runner ProjectRunner
	userID UserIDFunc
	// NoGCP skips the GCP integration check (demo modes).
	NoGCP bool
}

func NewProjectHandler(store ProjectStore, runner ProjectRunner, userID UserIDFunc) *ProjectHandler {
	return &ProjectHandler{store: store, runner: runner, userID: userID}
}

type createProjectReq struct {
	Name   string `json:"name"`
	Prompt string `json:"prompt"`
}

// Create handles POST /api/projects: it stores the project, starts the
// orchestrator in the background and answers 202 Accepted.
func (h *ProjectHandler) Create(w http.ResponseWriter, r *http.Request) {
	userID, err := h.userID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req createProjectReq
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxProjectBody)).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}
	req.Name, req.Prompt = strings.TrimSpace(req.Name), strings.TrimSpace(req.Prompt)
	if req.Name == "" || utf8.RuneCountInString(req.Name) > maxProjectName {
		writeErr(w, http.StatusUnprocessableEntity, "name is required (max 100 characters)")
		return
	}
	if req.Prompt == "" || utf8.RuneCountInString(req.Prompt) > maxProjectPrompt {
		writeErr(w, http.StatusUnprocessableEntity, "prompt is required (max 10000 characters)")
		return
	}

	// fail fast instead of creating a project that is certain to fail
	providers := []string{model.ProviderGitHub, model.ProviderGCP}
	if h.NoGCP {
		providers = providers[:1]
	}
	for _, provider := range providers {
		if _, err := h.store.GetIntegration(r.Context(), userID, provider); err != nil {
			if errors.Is(err, model.ErrNotFound) {
				writeErr(w, http.StatusUnprocessableEntity, provider+" integration is not configured")
				return
			}
			log.Printf("check %s integration: %v", provider, err)
			writeErr(w, http.StatusInternalServerError, "internal error")
			return
		}
	}

	p := &model.Project{
		ID: helper.NewID(), OwnerID: userID, Name: req.Name, Prompt: req.Prompt,
		Status: model.StatusPending, CurrentStep: model.StepInitializing, BranchName: "main",
	}
	if err := h.store.CreateProject(r.Context(), p); err != nil {
		log.Printf("create project: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := h.runner.Start(p.ID); err != nil {
		log.Printf("start project %s: %v", p.ID, err)
		_ = h.store.UpdateProject(context.WithoutCancel(r.Context()), p.ID, map[string]any{
			"status": model.StatusFailed, "error_message": "could not start the build",
		})
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Location", "/api/projects/"+p.ID)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"id": p.ID, "status": p.Status, "current_step": p.CurrentStep,
	})
}

type projectView struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Status       string    `json:"status"`
	CurrentStep  string    `json:"current_step"`
	RepoURL      string    `json:"repo_url,omitempty"`
	PRURL        string    `json:"pr_url,omitempty"`
	LiveURL      string    `json:"live_url,omitempty"`
	ErrorMessage string    `json:"error_message,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// Get handles GET /api/projects/{id}. Projects of other users look not-found.
func (h *ProjectHandler) Get(w http.ResponseWriter, r *http.Request) {
	userID, err := h.userID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	p, err := h.store.GetProject(r.Context(), r.PathValue("id"))
	if errors.Is(err, model.ErrNotFound) || (err == nil && p.OwnerID != userID) {
		writeErr(w, http.StatusNotFound, "project not found")
		return
	}
	if err != nil {
		log.Printf("get project: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, projectView{
		ID: p.ID, Name: p.Name, Status: p.Status, CurrentStep: p.CurrentStep,
		RepoURL: p.RepoURL, PRURL: p.PRURL, LiveURL: p.LiveURL, ErrorMessage: p.ErrorMessage,
		CreatedAt: p.CreatedAt, UpdatedAt: p.UpdatedAt,
	})
}

// Approve handles POST /api/projects/{id}/approve: it releases the HITL gate.
func (h *ProjectHandler) Approve(w http.ResponseWriter, r *http.Request) {
	userID, err := h.userID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	err = h.runner.Approve(r.Context(), r.PathValue("id"), userID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "approved"})
	case errors.Is(err, model.ErrNotFound):
		writeErr(w, http.StatusNotFound, "project not found")
	case errors.Is(err, service.ErrNotWaiting):
		writeErr(w, http.StatusConflict, "project is not waiting for approval")
	case errors.Is(err, service.ErrNoPendingRun):
		writeErr(w, http.StatusConflict, "this run is no longer active; create the project again")
	default:
		log.Printf("approve project: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}

// Deploy handles POST /api/projects/{id}/deploy: a manual (re-)deployment of a
// project whose code is already in its repo.
func (h *ProjectHandler) Deploy(w http.ResponseWriter, r *http.Request) {
	userID, err := h.userID(r)
	if err != nil {
		writeErr(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	err = h.runner.Redeploy(r.Context(), r.PathValue("id"), userID)
	switch {
	case err == nil:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "deploying"})
	case errors.Is(err, model.ErrNotFound):
		writeErr(w, http.StatusNotFound, "project not found")
	case errors.Is(err, service.ErrNotRedeployable):
		writeErr(w, http.StatusConflict, "project cannot be deployed in its current state")
	default:
		log.Printf("redeploy project: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
	}
}
