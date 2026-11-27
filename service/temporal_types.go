package service

import "time"

// Names shared by the workflows, the worker, the runner and the webhook.
const (
	TaskQueueProjects = "anz-projects" // workflows and light activities
	TaskQueueGenerate = "anz-generate" // GenerateAndOpenPR only (concurrency capped)

	SignalApprove        = "approve"
	SignalDeployProgress = "deploy-progress"
	SignalDeployResult   = "deploy-result"
)

// WorkflowConfig is the non-secret run configuration passed to a workflow.
type WorkflowConfig struct {
	RepoOnly bool // stop once the repo exists
	Demo     bool // no GCP: success once the approved PR is merged
	Deploy   bool // deploy mode: Prepare + a real deployment
	Polling  bool // follow the deploy run through the GitHub API instead of the webhook

	ApprovalTimeout time.Duration // default 24h
	DeployTimeout   time.Duration // deploy inactivity timer; 0 = no timer
	StageTimeout    time.Duration // GitHub/MySQL activity StartToClose; default 5m
}

// ProjectInput starts a ProjectWorkflow or RedeployWorkflow. It carries no secrets.
type ProjectInput struct {
	ProjectID string
	Config    WorkflowConfig
}
