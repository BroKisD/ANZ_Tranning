// Package handler contains the HTTP handlers for the AI app builder API.
package handler

import "net/http"

// GitHubCallback saves installation_id & owner (provider = 'github').
func GitHubCallback(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}

// UpsertIntegration handles POST /api/integrations (GCP WIF config, optional BYOK key).
func UpsertIntegration(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
