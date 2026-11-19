package model

import "errors"

var (
	ErrNotFound   = errors.New("not found")
	ErrEmailTaken = errors.New("email already in use")
	ErrValidation = errors.New("validation error")
)
