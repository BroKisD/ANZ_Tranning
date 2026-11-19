package handler

import "net/http"

// GitHubWebhook handles POST /api/webhooks/github (X-Webhook-Secret, constant-time compare).
func GitHubWebhook(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "not implemented", http.StatusNotImplemented)
}
