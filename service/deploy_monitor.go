package service

import (
	"context"
	"fmt"
	"log"
	"net/url"
	"time"

	"anz-training/model"
)

// DeployRun is what the platform learns about the deploy workflow's run by
// asking GitHub (instead of being called back by a webhook).
type DeployRun struct {
	Found     bool
	CreatedAt time.Time
	Done      bool
	// Conclusion is GitHub's verdict once Done: success, failure, cancelled, ...
	Conclusion string
	// Step is the furthest stage the run has started: building, testing or deploying ("" before that).
	Step string
	// FailedStage names the stage that failed: build, test or deploy ("" when none did).
	FailedStage string
	// LiveURL is the service URL the workflow published as an annotation.
	LiveURL string
}

// RunWatcher reads the newest deploy run created at or after since (implemented by *GitHub).
type RunWatcher interface {
	DeployRun(ctx context.Context, gh model.GitHubCreds, repoURL string, since time.Time) (DeployRun, error)
}

// MonitorStore is the persistence the monitor needs.
type MonitorStore interface {
	UpdateProjectIf(ctx context.Context, id, fromStatus string, fields map[string]any) (bool, error)
}

type DeployMonitorConfig struct {
	Poll         time.Duration // how often GitHub is asked (default 5s)
	StartTimeout time.Duration // how long to wait for the run to appear (default 3m)
	Timeout      time.Duration // the whole deploy (default 15m)
}

// DeployMonitor follows a deploy workflow run on GitHub and moves the project
// forward and to its final state. It replaces the webhook when the platform
// cannot be reached from GitHub.
type DeployMonitor struct {
	store   MonitorStore
	watcher RunWatcher
	cfg     DeployMonitorConfig
}

func NewDeployMonitor(s MonitorStore, w RunWatcher, cfg DeployMonitorConfig) *DeployMonitor {
	if cfg.Poll <= 0 {
		cfg.Poll = 5 * time.Second
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 3 * time.Minute
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Minute
	}
	return &DeployMonitor{store: s, watcher: w, cfg: cfg}
}

// Watch blocks until the project's deploy run ends, then records the result. It
// returns quietly when ctx is cancelled (the watchdog covers a restart).
func (m *DeployMonitor) Watch(ctx context.Context, projectID string, gh model.GitHubCreds, repoURL string, since time.Time) {
	deadline := time.Now().Add(m.cfg.Timeout)
	appearBy := time.Now().Add(m.cfg.StartTimeout)
	rank := StepRank(model.StepMerged)
	cancelled := false
	tick := time.NewTicker(m.cfg.Poll)
	defer tick.Stop()

	for {
		run, err := m.watcher.DeployRun(ctx, gh, repoURL, since)
		if ctx.Err() != nil {
			return
		}
		switch {
		case err != nil:
			log.Printf("deploy monitor %s: %v", projectID, err) // transient: keep polling until the deadline
		case !run.Found:
			if time.Now().After(appearBy) {
				msg := "the deploy workflow did not start"
				if cancelled {
					msg = "the deploy run was cancelled and no newer run started"
				}
				m.finish(ctx, projectID, map[string]any{"status": model.StatusFailed, "error_message": msg})
				return
			}
		default:
			if run.Step != "" && StepRank(run.Step) > rank {
				rank = StepRank(run.Step)
				_, _ = m.store.UpdateProjectIf(ctx, projectID, model.StatusDeploying, map[string]any{"current_step": run.Step})
			}
			if run.Done {
				if run.Conclusion == "cancelled" {
					// a newer run (re-deploy, or a push) replaces this one: follow it
					since, appearBy, cancelled = run.CreatedAt.Add(time.Second), time.Now().Add(m.cfg.StartTimeout), true
					break
				}
				m.finish(ctx, projectID, resultFields(run))
				return
			}
		}
		if time.Now().After(deadline) {
			m.finish(ctx, projectID, map[string]any{"status": model.StatusFailed,
				"error_message": fmt.Sprintf("%s for %s", WatchdogFailurePrefix, m.cfg.Timeout)})
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (m *DeployMonitor) finish(ctx context.Context, id string, fields map[string]any) {
	if _, err := m.store.UpdateProjectIf(ctx, id, model.StatusDeploying, fields); err != nil && ctx.Err() == nil {
		log.Printf("deploy monitor %s: record result: %v", id, err)
	}
}

func resultFields(run DeployRun) map[string]any {
	switch run.Conclusion {
	case "success":
		if !isHTTPSURL(run.LiveURL) {
			return map[string]any{"status": model.StatusFailed, "error_message": "deployed, but the workflow published no valid service URL"}
		}
		return map[string]any{"status": model.StatusSuccess, "live_url": run.LiveURL}
	default:
		stage := run.FailedStage
		if stage == "" {
			stage = "setup"
		}
		return map[string]any{"status": model.StatusFailed, "error_message": fmt.Sprintf("Deployment failed at the %s stage", stage)}
	}
}

// isHTTPSURL accepts only absolute https URLs: the value is shown as a link.
func isHTTPSURL(raw string) bool {
	if raw == "" || len(raw) > 500 {
		return false
	}
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil
}
