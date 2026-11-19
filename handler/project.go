package handler

import "net/http"

// CreateProject handles POST /api/projects.
func CreateProject(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// GetProject handles GET /api/projects/{id}.
func GetProject(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// ApproveProject handles POST /api/projects/{id}/approve.
func ApproveProject(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
