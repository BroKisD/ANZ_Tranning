package helper_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"anz-training/helper"
)

func TestAPIKeyMiddleware(t *testing.T) {
	tests := []struct {
		name       string
		key        string
		wantStatus int
		wantCalled bool
	}{
		{"missing key", "", http.StatusUnauthorized, false},
		{"wrong key", "nope", http.StatusUnauthorized, false},
		{"right key", "secret", http.StatusOK, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			h := helper.APIKeyMiddleware("secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				called = true
			}))
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tt.key != "" {
				req.Header.Set("x-api-key", tt.key)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tt.wantStatus || called != tt.wantCalled {
				t.Fatalf("status=%d called=%v, want %d/%v", rec.Code, called, tt.wantStatus, tt.wantCalled)
			}
		})
	}
}
