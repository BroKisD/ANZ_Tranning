package service

import (
	"context"
	"sync"

	"anz-training/repository"
)

// Orchestrator runs each project as a goroutine state machine and holds the
// HITL approval channels.
type Orchestrator struct {
	store  *repository.Store
	github *GitHub
	claude *Claude

	mu            sync.Mutex
	approvalChans map[string]chan struct{}
}

func NewOrchestrator(store *repository.Store, gh *GitHub, cl *Claude) *Orchestrator {
	return &Orchestrator{store: store, github: gh, claude: cl, approvalChans: map[string]chan struct{}{}}
}

// Run: generating -> repo_created -> tests_passed -> PR -> waiting_for_approval
// -> (approve) merging -> deploying. Webhook finishes with success/failed.
func (o *Orchestrator) Run(ctx context.Context, projectID string) {}

// Approve signals the HITL gate for projectID.
func (o *Orchestrator) Approve(projectID string) error { return ErrNotImplemented }
