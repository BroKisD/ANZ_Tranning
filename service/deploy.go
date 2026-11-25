package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"anz-training/model"
)

// DeployTarget identifies the repo / Cloud Run service a deployment is for.
type DeployTarget struct {
	ProjectID   string // platform project id
	ServiceName string // Cloud Run service and repo name, from ServiceName(name, id)
	RepoOwner   string
	RepoName    string
}

// Deployer is how a project gets deployed. Auto deployment needs no call
// (merging the PR pushes to main, which starts the workflow); Trigger covers
// manual re-deploys.
type Deployer interface {
	// Prepare makes the deploy target ready for this repo (idempotent) and
	// returns the values the deploy workflow needs. Called before the repo exists.
	Prepare(ctx context.Context, t DeployTarget) (model.GCPCreds, error)
	// Trigger starts a deployment of the repo's default branch.
	Trigger(ctx context.Context, gh model.GitHubCreds, repoURL string) error
}

// WatchdogFailurePrefix starts the error message of a deploy the watchdog gave up on.
const WatchdogFailurePrefix = "no update from GitHub Actions"

// StepRank orders the steps of a deployment: merging < merged < building <
// testing < deploying. Any other step ranks 0.
func StepRank(step string) int {
	switch step {
	case model.StepMerging:
		return 1
	case model.StepMerged:
		return 2
	case model.StepBuilding:
		return 3
	case model.StepTesting:
		return 4
	case model.StepDeploying:
		return 5
	}
	return 0
}

// DeployConfig is the platform-level GCP configuration (deploy mode).
type DeployConfig struct {
	ProjectID     string
	Region        string
	ArtifactRepo  string
	AllowedOwners []string // GitHub accounts whose repos may authenticate to GCP
	Timeout       time.Duration
}

// DeployConfigFromEnv reads GCP_PROJECT_ID, GCP_REGION, GCP_ARTIFACT_REPO,
// GCP_ALLOWED_GITHUB_OWNERS and DEPLOY_TIMEOUT.
func DeployConfigFromEnv(get func(string) string) (DeployConfig, error) {
	cfg := DeployConfig{
		ProjectID:    strings.TrimSpace(get("GCP_PROJECT_ID")),
		Region:       strings.TrimSpace(get("GCP_REGION")),
		ArtifactRepo: strings.TrimSpace(get("GCP_ARTIFACT_REPO")),
		Timeout:      15 * time.Minute,
	}
	if cfg.ProjectID == "" {
		return cfg, errors.New("GCP_PROJECT_ID is required")
	}
	if cfg.Region == "" {
		cfg.Region = "asia-southeast1"
	}
	if cfg.ArtifactRepo == "" {
		cfg.ArtifactRepo = "ai-apps"
	}
	for _, o := range strings.Split(get("GCP_ALLOWED_GITHUB_OWNERS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.AllowedOwners = append(cfg.AllowedOwners, o)
		}
	}
	if len(cfg.AllowedOwners) == 0 {
		return cfg, errors.New("GCP_ALLOWED_GITHUB_OWNERS is required (comma-separated GitHub accounts)")
	}
	var err error
	if cfg.Timeout, err = DeployTimeoutFromEnv(get); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// DeployTimeoutFromEnv reads DEPLOY_TIMEOUT (default 15m).
func DeployTimeoutFromEnv(get func(string) string) (time.Duration, error) {
	v := strings.TrimSpace(get("DEPLOY_TIMEOUT"))
	if v == "" {
		return 15 * time.Minute, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("DEPLOY_TIMEOUT %q is not a positive duration", v)
	}
	return d, nil
}
