// Package repository is the MySQL CRUD layer for users, integrations, projects.
package repository

import (
	"context"
	"errors"
	"fmt"

	"anz-training/model"

	"gorm.io/gorm"
)

var ErrNotImplemented = errors.New("not implemented")

type Store struct{ db *gorm.DB }

func NewStore(db *gorm.DB) *Store { return &Store{db: db} }

// Users
func (s *Store) FindUserByEmail(ctx context.Context, email string) (*model.User, error) {
	var u model.User
	err := s.db.WithContext(ctx).Where("email = ?", email).First(&u).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user by email: %w", err)
	}
	return &u, nil
}

func (s *Store) InsertUser(ctx context.Context, u *model.User) error {
	if err := s.db.WithContext(ctx).Create(u).Error; err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	return nil
}

// Integrations
func (s *Store) UpsertIntegration(ctx context.Context, in *model.UserIntegration) error {
	return ErrNotImplemented
}
func (s *Store) GetIntegration(ctx context.Context, userID, provider string) (*model.UserIntegration, error) {
	return nil, ErrNotImplemented
}

// Projects
func (s *Store) CreateProject(ctx context.Context, p *model.Project) error { return ErrNotImplemented }
func (s *Store) GetProject(ctx context.Context, id string) (*model.Project, error) {
	return nil, ErrNotImplemented
}
func (s *Store) UpdateProject(ctx context.Context, id string, fields map[string]any) error {
	return ErrNotImplemented
}
