package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"anz-training/crypto"
	"anz-training/model"
)

var (
	ErrNotWaiting     = errors.New("project is not waiting for approval")
	ErrNoPendingRun   = errors.New("no pending run for this project")
	ErrAlreadyRunning = errors.New("project is already running")

	errApprovalTimeout = errors.New("approval timed out")
)

const (
	defaultApprovalTimeout = 24 * time.Hour
	defaultStageTimeout    = 5 * time.Minute
	defaultMaxGenerations  = 2
	maxErrorMessageLen     = 1000
)

// Collaborators of the orchestrator, as interfaces so the state machine can be
// tested without GitHub, Claude or MySQL.

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

type OrchestratorConfig struct {
	MasterKey                []byte        // decrypts user_integrations
	ApprovalTimeout          time.Duration // HITL gate timeout (default 24h)
	StageTimeout             time.Duration // per GitHub stage (default 5m)
	MaxConcurrentGenerations int           // parallel `claude` jobs (default 2)
}

// Orchestrator runs each project as a goroutine state machine and holds the
// HITL approval channels:
//
//	pending -> generating (initializing -> repo_created -> tests_passed)
//	        -> waiting_for_approval -> deploying (merging -> deploying)
//
// The deploy webhook (not this type) moves deploying to success/failed. Any
// error before that moves the project to failed with an error_message.
type Orchestrator struct {
	store        ProjectStore
	integrations IntegrationReader
	github       RepoHost
	gen          CodeGenerator
	secrets      SecretReader // may be nil: BYOK projects then fail
	cfg          OrchestratorConfig

	genSem chan struct{}

	base   context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu            sync.Mutex
	running       map[string]bool
	approvalChans map[string]chan struct{}
}

func NewOrchestrator(store ProjectStore, integrations IntegrationReader, gh RepoHost, gen CodeGenerator, secrets SecretReader, cfg OrchestratorConfig) *Orchestrator {
	if cfg.ApprovalTimeout <= 0 {
		cfg.ApprovalTimeout = defaultApprovalTimeout
	}
	if cfg.StageTimeout <= 0 {
		cfg.StageTimeout = defaultStageTimeout
	}
	if cfg.MaxConcurrentGenerations <= 0 {
		cfg.MaxConcurrentGenerations = defaultMaxGenerations
	}
	base, cancel := context.WithCancel(context.Background())
	return &Orchestrator{
		store: store, integrations: integrations, github: gh, gen: gen, secrets: secrets, cfg: cfg,
		genSem: make(chan struct{}, cfg.MaxConcurrentGenerations),
		base:   base, cancel: cancel,
		running: map[string]bool{}, approvalChans: map[string]chan struct{}{},
	}
}

// Start runs the project in a background goroutine (detached from any HTTP
// request). Use Shutdown to stop all runs.
func (o *Orchestrator) Start(projectID string) error {
	o.mu.Lock()
	if o.running[projectID] {
		o.mu.Unlock()
		return ErrAlreadyRunning
	}
	o.running[projectID] = true
	o.mu.Unlock()

	o.wg.Add(1)
	go func() {
		defer o.wg.Done()
		defer func() {
			o.mu.Lock()
			delete(o.running, projectID)
			o.mu.Unlock()
		}()
		o.Run(o.base, projectID)
	}()
	return nil
}

// Shutdown cancels every run and waits for the goroutines (or ctx) to finish.
func (o *Orchestrator) Shutdown(ctx context.Context) error {
	o.cancel()
	done := make(chan struct{})
	go func() { o.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Run drives one project to waiting_for_approval, blocks on the HITL gate, then
// merges. It blocks until the run ends; errors are recorded on the project.
func (o *Orchestrator) Run(ctx context.Context, projectID string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("orchestrator: project %s panicked: %v", projectID, r)
			o.fail(projectID, fmt.Errorf("internal error"))
		}
	}()
	if err := o.run(ctx, projectID); err != nil {
		log.Printf("orchestrator: project %s failed: %v", projectID, err)
		o.fail(projectID, err)
	}
}

func (o *Orchestrator) run(ctx context.Context, id string) error {
	// pending -> generating; the compare-and-set makes a second Run a no-op
	started, err := o.store.UpdateProjectIf(ctx, id, model.StatusPending, map[string]any{
		"status": model.StatusGenerating, "current_step": model.StepInitializing,
	})
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if !started {
		return nil
	}
	p, err := o.store.GetProject(ctx, id)
	if err != nil {
		return fmt.Errorf("load project: %w", err)
	}

	gh, gcp, err := o.loadCreds(ctx, p.OwnerID)
	if err != nil {
		return err
	}
	apiKey, err := o.resolveAPIKey(ctx, gcp)
	if err != nil {
		return err
	}

	// 2. create repo
	repoURL, err := stage(ctx, o.cfg.StageTimeout, func(c context.Context) (string, error) {
		return o.github.CreateRepo(c, gh, gcp, id, p.Name)
	})
	if err != nil {
		return fmt.Errorf("create repo: %w", err)
	}
	if err := o.store.UpdateProject(ctx, id, map[string]any{
		"repo_url": repoURL, "current_step": model.StepRepoCreated,
	}); err != nil {
		return err
	}

	// 3. generate + test (bounded parallelism: each job is a heavy process tree)
	dir, err := o.generate(ctx, GenerateRequest{ProjectID: id, Prompt: p.Prompt, AnthropicAPIKey: apiKey})
	if err != nil {
		return fmt.Errorf("generate: %w", err)
	}
	defer func() { _ = o.gen.Cleanup(id) }() // success path: cleanup after PR push or any later failure
	if err := o.store.UpdateProject(ctx, id, map[string]any{"current_step": model.StepTestsPassed}); err != nil {
		return err
	}

	// 4. push + open PR
	base := p.BranchName
	if base == "" {
		base = "main"
	}
	prURL, err := stage(ctx, o.cfg.StageTimeout, func(c context.Context) (string, error) {
		return o.github.PushAndOpenPR(c, dir, gh, repoURL, base)
	})
	if err != nil {
		return fmt.Errorf("open pull request: %w", err)
	}
	_ = o.gen.Cleanup(id) // workspace is no longer needed; don't hold disk during the wait

	// 5. HITL gate. The channel is registered BEFORE the status flips so an
	// Approve that sees waiting_for_approval always finds it.
	approved := make(chan struct{}, 1)
	o.mu.Lock()
	o.approvalChans[id] = approved
	o.mu.Unlock()
	defer func() {
		o.mu.Lock()
		delete(o.approvalChans, id)
		o.mu.Unlock()
	}()
	if err := o.store.UpdateProject(ctx, id, map[string]any{
		"pr_url": prURL, "status": model.StatusWaitingForApproval,
	}); err != nil {
		return err
	}

	timer := time.NewTimer(o.cfg.ApprovalTimeout)
	defer timer.Stop()
	select {
	case <-approved:
	case <-timer.C:
		return errApprovalTimeout
	case <-ctx.Done():
		return ctx.Err()
	}

	// 6. merge & deploy
	if err := o.store.UpdateProject(ctx, id, map[string]any{
		"status": model.StatusDeploying, "current_step": model.StepMerging,
	}); err != nil {
		return err
	}
	if _, err := stage(ctx, o.cfg.StageTimeout, func(c context.Context) (struct{}, error) {
		return struct{}{}, o.github.MergePR(c, gh, repoURL, prURL)
	}); err != nil {
		return fmt.Errorf("merge pull request: %w", err)
	}
	return o.store.UpdateProject(ctx, id, map[string]any{"current_step": model.StepDeploying})
}

func stage[T any](ctx context.Context, timeout time.Duration, fn func(context.Context) (T, error)) (T, error) {
	c, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return fn(c)
}

func (o *Orchestrator) generate(ctx context.Context, req GenerateRequest) (string, error) {
	select {
	case o.genSem <- struct{}{}:
		defer func() { <-o.genSem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return o.gen.GenerateAndTest(ctx, req)
}

// loadCreds decrypts the owner's GitHub and GCP integrations.
func (o *Orchestrator) loadCreds(ctx context.Context, ownerID string) (model.GitHubCreds, model.GCPCreds, error) {
	var gh model.GitHubCreds
	var gcp model.GCPCreds
	if err := o.loadIntegration(ctx, ownerID, model.ProviderGitHub, &gh); err != nil {
		return gh, gcp, err
	}
	if err := o.loadIntegration(ctx, ownerID, model.ProviderGCP, &gcp); err != nil {
		return gh, gcp, err
	}
	return gh, gcp, nil
}

func (o *Orchestrator) loadIntegration(ctx context.Context, ownerID, provider string, into any) error {
	in, err := o.integrations.GetIntegration(ctx, ownerID, provider)
	if errors.Is(err, model.ErrNotFound) {
		return fmt.Errorf("%s integration is not configured", provider)
	}
	if err != nil {
		return fmt.Errorf("load %s integration: %w", provider, err)
	}
	plain, err := crypto.Decrypt(o.cfg.MasterKey, in.EncryptedCredentials)
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
func (o *Orchestrator) resolveAPIKey(ctx context.Context, gcp model.GCPCreds) (string, error) {
	if gcp.AnthropicSecretRef == "" {
		return "", nil
	}
	if o.secrets == nil {
		return "", errors.New("BYOK api key is configured but secret access is not enabled")
	}
	key, err := o.secrets.GetSecret(ctx, gcp.AnthropicSecretRef)
	if err != nil {
		return "", fmt.Errorf("read BYOK api key: %w", err)
	}
	return key, nil
}

// Approve releases the HITL gate for a project waiting for approval. It is
// idempotent, and reports not-found for projects the caller does not own.
func (o *Orchestrator) Approve(ctx context.Context, projectID, ownerID string) error {
	p, err := o.store.GetProject(ctx, projectID)
	if err != nil {
		return err
	}
	if p.OwnerID != ownerID {
		return model.ErrNotFound
	}
	if p.Status != model.StatusWaitingForApproval {
		return ErrNotWaiting
	}
	o.mu.Lock()
	ch, ok := o.approvalChans[projectID]
	o.mu.Unlock()
	if !ok {
		return ErrNoPendingRun // e.g. the server restarted since the PR was opened
	}
	select {
	case ch <- struct{}{}:
	default: // already approved
	}
	return nil
}

// fail records the failure with a fresh context (the run's may be cancelled).
func (o *Orchestrator) fail(projectID string, err error) {
	msg := err.Error()
	if errors.Is(err, context.Canceled) {
		msg = "cancelled: server shutting down"
	}
	if len(msg) > maxErrorMessageLen {
		msg = msg[:maxErrorMessageLen] + "..."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if uerr := o.store.UpdateProject(ctx, projectID, map[string]any{
		"status": model.StatusFailed, "error_message": msg,
	}); uerr != nil {
		log.Printf("orchestrator: record failure for %s: %v", projectID, uerr)
	}
}
