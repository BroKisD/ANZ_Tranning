// Package model holds the persistent entities (users, user_integrations,
// projects) and their enums. Field layout mirrors database/migrations.
package model

import "time"

type User struct {
	ID           string `gorm:"primaryKey;size:36"`
	Name         string
	Email        string
	PasswordHash string `json:"-"`
	CreatedAt    time.Time
}

const (
	ProviderGitHub = "github"
	ProviderGCP    = "gcp"
)

// UserIntegration stores AES-256-GCM encrypted JSON (see GitHubCreds / GCPCreds).
type UserIntegration struct {
	ID                   string `gorm:"primaryKey;size:36"`
	UserID               string
	Provider             string
	EncryptedCredentials string `json:"-"`
	CreatedAt            time.Time
}

// GitHubCreds is the decrypted payload of provider = 'github'.
type GitHubCreds struct {
	Owner string `json:"owner"`
	Token string `json:"token"` // Personal Access Token; only stored AES-GCM encrypted
}

// GCPCreds is the decrypted payload of provider = 'gcp'.
type GCPCreds struct {
	ProjectID          string `json:"project_id"`
	Region             string `json:"region"`
	ArtifactRepo       string `json:"artifact_repo"`
	WIFProvider        string `json:"wif_provider"`
	ServiceAccount     string `json:"service_account"`
	AnthropicSecretRef string `json:"anthropic_secret_ref,omitempty"`
	// RuntimeServiceAccount is the role-less account deployed apps run as (platform deploy mode).
	RuntimeServiceAccount string `json:"runtime_service_account,omitempty"`
}

type Project struct {
	ID           string `gorm:"primaryKey;size:36"`
	OwnerID      string
	Name         string
	Prompt       string
	Status       string
	CurrentStep  string
	RepoURL      string
	BranchName   string
	PRURL        string `gorm:"column:pr_url"`
	LiveURL      string
	ErrorMessage string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Project.Status values.
const (
	StatusPending            = "pending"
	StatusGenerating         = "generating"
	StatusWaitingForApproval = "waiting_for_approval"
	StatusDeploying          = "deploying"
	StatusSuccess            = "success"
	StatusFailed             = "failed"
)

// Project.CurrentStep values.
const (
	StepInitializing = "initializing"
	StepRepoCreated  = "repo_created"
	StepTestsPassed  = "tests_passed"
	StepMerging      = "merging"
	StepMerged       = "merged" // the PR is merged to main; the deploy workflow takes over
	StepBuilding     = "building"
	StepTesting      = "testing"
	StepDeploying    = "deploying"
)
