package user

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
)

type Handler struct {
	svc UserService
}
type UserService interface {
	Create(ctx context.Context, email, name string) (*User, error)
	GetByEmail(ctx context.Context, email string) (*User, error)
}

func NewHandler(svc UserService) *Handler {
	return &Handler{svc: svc}
}

type createReq struct {
	Email string `json:"email"`
	Name  string `json:"name"`
}

func (h *Handler) Create(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid json")
		return
	}

	u, err := h.svc.Create(r.Context(), req.Email, req.Name)

	if err != nil {
		switch {
		case errors.Is(err, ErrEmailTaken):
			writeErr(w, http.StatusConflict, "email already in use")
		case errors.Is(err, ErrValidation):
			writeErr(w, http.StatusUnprocessableEntity, err.Error())
		default:
			log.Printf("create user: %v", err)
			writeErr(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusCreated, u)
}

func (h *Handler) Get(w http.ResponseWriter, r *http.Request) {
	email := r.URL.Query().Get("email")
	if email == "" {
		writeErr(w, http.StatusBadRequest, "email query param required")
		return
	}

	u, err := h.svc.GetByEmail(r.Context(), email)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeErr(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("get user: %v", err)
		writeErr(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, u)
}

// ---- tiny response helpers ----

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
