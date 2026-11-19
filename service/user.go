package service

import (
	"context"
	"fmt"
	"strings"

	"anz-training/helper"
	"anz-training/model"
)

type UserRepository interface {
	FindUserByEmail(ctx context.Context, email string) (*model.User, error)
	InsertUser(ctx context.Context, u *model.User) error
}

type UserService struct{ repo UserRepository }

func NewUserService(repo UserRepository) *UserService { return &UserService{repo: repo} }

func (s *UserService) Create(ctx context.Context, email, name string) (*model.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	name = strings.TrimSpace(name)
	if email == "" || name == "" || !helper.IsValidEmail(email) {
		return nil, fmt.Errorf("%w: email and name are required, email must be valid", model.ErrValidation)
	}

	if _, err := s.repo.FindUserByEmail(ctx, email); err == nil {
		return nil, model.ErrEmailTaken
	} else if err != model.ErrNotFound {
		return nil, err
	}

	u := &model.User{ID: helper.NewID(), Email: email, Name: name}
	if err := s.repo.InsertUser(ctx, u); err != nil {
		return nil, err
	}
	return u, nil
}

func (s *UserService) GetByEmail(ctx context.Context, email string) (*model.User, error) {
	return s.repo.FindUserByEmail(ctx, strings.ToLower(strings.TrimSpace(email)))
}
