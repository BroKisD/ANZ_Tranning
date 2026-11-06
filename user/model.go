package user

import (
	"errors"
	"time"
)

type User struct {
	ID        int64
	Email     string
	Name      string
	CreatedAt time.Time
}

var (
	ErrNotFound   = errors.New("user not found")
	ErrEmailTaken = errors.New("email already in use")
	ErrValidation = errors.New("validation error")
)
