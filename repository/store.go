// Package repository is the MySQL CRUD layer for users, integrations, projects.
package repository

import (
	"context"
	"errors"
	"fmt"

	"anz-training/model"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

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

// UpsertIntegration inserts or, on (user_id, provider) conflict, replaces the
// encrypted credentials.
func (s *Store) UpsertIntegration(ctx context.Context, in *model.UserIntegration) error {
	err := s.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "user_id"}, {Name: "provider"}},
		DoUpdates: clause.AssignmentColumns([]string{"encrypted_credentials"}),
	}).Create(in).Error
	if err != nil {
		return fmt.Errorf("upsert integration: %w", err)
	}
	return nil
}

func (s *Store) GetIntegration(ctx context.Context, userID, provider string) (*model.UserIntegration, error) {
	var in model.UserIntegration
	err := s.db.WithContext(ctx).Where("user_id = ? AND provider = ?", userID, provider).First(&in).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get integration: %w", err)
	}
	return &in, nil
}

func (s *Store) ListIntegrations(ctx context.Context, userID string) ([]model.UserIntegration, error) {
	var out []model.UserIntegration
	if err := s.db.WithContext(ctx).Where("user_id = ?", userID).Find(&out).Error; err != nil {
		return nil, fmt.Errorf("list integrations: %w", err)
	}
	return out, nil
}

// Projects

// CreateProject inserts a project, applying the schema defaults when unset.
func (s *Store) CreateProject(ctx context.Context, p *model.Project) error {
	if p.Status == "" {
		p.Status = model.StatusPending
	}
	if p.CurrentStep == "" {
		p.CurrentStep = model.StepInitializing
	}
	if p.BranchName == "" {
		p.BranchName = "main"
	}
	if err := s.db.WithContext(ctx).Create(p).Error; err != nil {
		return fmt.Errorf("create project: %w", err)
	}
	return nil
}

func (s *Store) GetProject(ctx context.Context, id string) (*model.Project, error) {
	var p model.Project
	err := s.db.WithContext(ctx).Where("id = ?", id).First(&p).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, model.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	return &p, nil
}

// UpdateProject sets the given columns (keys are DB column names).
func (s *Store) UpdateProject(ctx context.Context, id string, fields map[string]any) error {
	res := s.db.WithContext(ctx).Model(&model.Project{}).Where("id = ?", id).Updates(fields)
	if res.Error != nil {
		return fmt.Errorf("update project: %w", res.Error)
	}
	return nil
}

// UpdateProjectIf applies fields only when the project is currently in
// fromStatus and reports whether it did (a compare-and-set). fields must change
// the status, because MySQL reports changed rows, not matched rows.
func (s *Store) UpdateProjectIf(ctx context.Context, id, fromStatus string, fields map[string]any) (bool, error) {
	res := s.db.WithContext(ctx).Model(&model.Project{}).
		Where("id = ? AND status = ?", id, fromStatus).Updates(fields)
	if res.Error != nil {
		return false, fmt.Errorf("update project: %w", res.Error)
	}
	return res.RowsAffected == 1, nil
}
