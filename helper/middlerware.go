package helper

import (
	"net/http"
)

func APIKeyMiddleware(expectedKey string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientKey := r.Header.Get("x-api-key")

		if clientKey == "" || clientKey != expectedKey {
			http.Error(w, "Unauthorized: Invalid or missing API Key", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
