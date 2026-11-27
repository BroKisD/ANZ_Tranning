package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"anz-training/crypto"
	"anz-training/model"
)

var (
	ErrNotWaiting     = errors.New("project is not waiting for approval")
	ErrNoPendingRun   = errors.New("no pending run for this project")
	ErrAlreadyRunning = errors.New("project is already running")
	// ErrNotRedeployable: the project is not in a state a manual deploy can start from.
	ErrNotRedeployable = errors.New("project cannot be re-deployed")

	errApprovalTimeout = errors.New("approval timed out")
)

const (
	defaultApprovalTimeout = 24 * time.Hour
	defaultStageTimeout    = 5 * time.Minute
	defaultMaxGenerations  = 2
	maxErrorMessageLen     = 1000
	// runClockSkew widens "runs created since" so a small difference between
	// this server's clock and GitHub's cannot hide the run that was just started.
	runClockSkew = 10 * time.Second
)

// Collaborators of the run, as interfaces so it can be tested without GitHub,
// Claude or MySQL.

type ProjectStore interface {
	GetProject(ctx context.Context, id string) (*model.Project, error)
	UpdateProject(ctx context.Context, id string, fields map[string]any) error
	// UpdateProjectIf is a compare-and-set on status.
	UpdateProjectIf(ctx context.Context, id, fromStatus string, fields map[string]any) (bool, error)
}

type IntegrationReader interface {
	GetIntegration(ctx context.Context, userID, provider string) (*model.UserIntegration, error)
}

type RepoHost interface {
	CreateRepo(ctx context.Context, gh model.GitHubCreds, gcp model.GCPCreds, projectID, projectName string) (string, error)
	PushAndOpenPR(ctx context.Context, workspaceDir string, gh model.GitHubCreds, repoURL, baseBranch string) (string, error)
	MergePR(ctx context.Context, gh model.GitHubCreds, repoURL, prURL string) error
}

type CodeGenerator interface {
	GenerateAndTest(ctx context.Context, req GenerateRequest) (string, error)
	Cleanup(projectID string) error
}

// SecretReader resolves a Secret Manager reference (BYOK Anthropic key).
type SecretReader interface {
	GetSecret(ctx context.Context, ref string) (string, error)
}

// loadIntegration decrypts the owner's integration for provider into `into`.
func loadIntegration(ctx context.Context, in IntegrationReader, masterKey []byte, ownerID, provider string, into any) error {
	rec, err := in.GetIntegration(ctx, ownerID, provider)
	if errors.Is(err, model.ErrNotFound) {
		return fmt.Errorf("%s integration is not configured", provider)
	}
	if err != nil {
		return fmt.Errorf("load %s integration: %w", provider, err)
	}
	plain, err := crypto.Decrypt(masterKey, rec.EncryptedCredentials)
	if err != nil {
		return fmt.Errorf("decrypt %s integration: %w", provider, err)
	}
	if err := json.Unmarshal(plain, into); err != nil {
		return fmt.Errorf("parse %s integration: %w", provider, err)
	}
	return nil
}

// resolveAPIKey returns the user's BYOK key, or "" to use the platform key. A
// project whose user configured BYOK never silently falls back to the platform key.
func resolveAPIKey(ctx context.Context, secrets SecretReader, gcp model.GCPCreds) (string, error) {
	if gcp.AnthropicSecretRef == "" {
		return "", nil
	}
	if secrets == nil {
		return "", errors.New("BYOK api key is configured but secret access is not enabled")
	}
	key, err := secrets.GetSecret(ctx, gcp.AnthropicSecretRef)
	if err != nil {
		return "", fmt.Errorf("read BYOK api key: %w", err)
	}
	return key, nil
}
