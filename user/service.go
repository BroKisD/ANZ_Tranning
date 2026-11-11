package user

import (
	"anz-training/helper"
	"context"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

type Service struct {
	db   *gorm.DB
	repo UserRepository
}

type UserRepository interface {
	FindByEmail(ctx context.Context, email string) (*User, error)
	Insert(ctx context.Context, u *User) error
}

func NewService(db *gorm.DB) *Service {
	return &Service{db: db, repo: NewRepo(db)}
}

func NewServiceWithRepo(repo UserRepository) *Service {
	return &Service{repo: repo}
}

func (s *Service) Create(ctx context.Context, email, name string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if email == "" || name == "" || !helper.IsValidEmail(email) {
		return nil, fmt.Errorf("%w: email and name are required, email must be valid", ErrValidation)
	}

	if _, err := s.repo.FindByEmail(ctx, email); err == nil {
		return nil, ErrEmailTaken
	} else if err != ErrNotFound {
		return nil, err
	}

	u := &User{Email: email, Name: name}
	if err := s.repo.Insert(ctx, u); err != nil {
		return nil, err
	}

	return u, nil
}

func (s *Service) GetByEmail(ctx context.Context, email string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	return s.repo.FindByEmail(ctx, email)
}
