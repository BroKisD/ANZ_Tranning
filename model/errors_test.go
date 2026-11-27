package model_test

import (
	"errors"
	"fmt"
	"testing"

	"anz-training/model"
)

func TestErrors_AreDistinct(t *testing.T) {
	all := []error{model.ErrNotFound, model.ErrEmailTaken, model.ErrValidation}
	for i, a := range all {
		for j, b := range all {
			if got := errors.Is(a, b); got != (i == j) {
				t.Errorf("errors.Is(%v, %v) = %v", a, b, got)
			}
		}
	}
}

func TestErrors_MatchWhenWrapped(t *testing.T) {
	err := fmt.Errorf("lookup user: %w", model.ErrNotFound)
	if !errors.Is(err, model.ErrNotFound) {
		t.Fatal("wrapped ErrNotFound should still match")
	}
}
