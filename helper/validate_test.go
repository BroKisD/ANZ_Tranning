package helper_test

import (
	"strings"
	"testing"

	"anz-training/helper"
)

func TestIsValidEmail(t *testing.T) {
	tests := []struct {
		name  string
		email string
		want  bool
	}{
		{"valid", "a@b.co", true},
		{"minimum length", "a@b", true},
		{"empty", "", false},
		{"too short", "a@", false},
		{"no at sign", "abcdef", false},
		{"too long", strings.Repeat("a", 250) + "@b.co", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := helper.IsValidEmail(tt.email); got != tt.want {
				t.Fatalf("IsValidEmail(%q) = %v, want %v", tt.email, got, tt.want)
			}
		})
	}
}
