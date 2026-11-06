package user

import (
	"anz-training/helper"
	"context"
	"database/sql"
	"fmt"
	"strings"
)

type Service struct {
	db   *sql.DB
	repo Repo
}

func NewService(db *sql.DB) *Service {
	return &Service{db: db, repo: Repo{}}
}

func (s *Service) Create(ctx context.Context, email, name string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if email == "" || name == "" || !helper.IsValidEmail(email) {
		return nil, fmt.Errorf("%w: email and name are required, email must be valid", ErrValidation)
	}

	if _, err := s.repo.FindByEmail(ctx, s.db, email); err == nil {
		return nil, ErrEmailTaken
	} else if err != ErrNotFound {
		return nil, err
	}

	u := &User{Email: email, Name: name}
	if err := s.repo.Insert(ctx, s.db, u); err != nil {
		return nil, err
	}

	return u, nil
}

func (s *Service) GetByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	return s.repo.FindByEmail(ctx, s.db, email)
}
