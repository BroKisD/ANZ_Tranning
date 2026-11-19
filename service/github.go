// Package service holds the GitHub, Claude Code and orchestration logic.
package service

import (
	"context"
	"errors"

	"anz-training/model"
)

var ErrNotImplemented = errors.New("not implemented")

type GitHub struct{}

// GetInstallationToken signs an App JWT and mints a 1-hour installation token (RAM only).
func (g *GitHub) GetInstallationToken(ctx context.Context, installationID int64) (string, error) {
	return "", ErrNotImplemented
}

// CreateRepo creates a private repo and commits the rendered deploy workflow to main.
func (g *GitHub) CreateRepo(ctx context.Context, gh model.GitHubCreds, gcp model.GCPCreds, projectID, projectName string) (repoURL string, err error) {
	return "", ErrNotImplemented
}

// PushAndOpenPR pushes feature/ai-gen and opens a PR against baseBranch.
func (g *GitHub) PushAndOpenPR(ctx context.Context, workspaceDir string, gh model.GitHubCreds, baseBranch string) (prURL string, err error) {
	return "", ErrNotImplemented
}

// MergePR merges the PR into main, triggering GitHub Actions.
func (g *GitHub) MergePR(ctx context.Context, gh model.GitHubCreds, repoURL, prURL string) error {
	return ErrNotImplemented
}
