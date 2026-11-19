package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"anz-training/model"
)

type UserService interface {
	Create(ctx context.Context, email, name string) (*model.User, error)
	GetByEmail(ctx context.Context, email string) (*model.User, error)
}

type UserHandler struct{ svc UserService }

func NewUserHandler(svc UserService) *UserHandler { return &UserHandler{svc: svc} }

type createUserReq struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (h *UserHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req createUserReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}

	u, err := h.svc.Create(r.Context(), req.Email, req.Name)
	if err != nil {
		switch {
		case errors.Is(err, model.ErrEmailTaken):
			writeErr(w, http.StatusConflict, "email already in use")
		case errors.Is(err, model.ErrValidation):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		default:
			log.Printf("create user: %v", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (h *UserHandler) Get(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	if email == "" {
		writeErr(w, http.StatusBadRequest, "email query param required")
		return
	}

	u, err := h.svc.GetByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("get user: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
