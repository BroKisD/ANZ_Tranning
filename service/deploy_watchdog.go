package service

import (
	"context"
	"fmt"
	"log"
	"time"

	"anz-training/model"
)

// WatchdogStore is the persistence the deploy watchdog needs.
type WatchdogStore interface {
	// ListStaleDeploying returns deploying projects not updated since before.
	ListStaleDeploying(ctx context.Context, before time.Time) ([]model.Project, error)
	// UpdateProjectIf is a compare-and-set on status.
	UpdateProjectIf(ctx context.Context, id, fromStatus string, fields map[string]any) (bool, error)
}

// DeployWatchdog fails projects whose deploy workflow went silent (runner
// killed, job cancelled, webhook unreachable). Every progress event refreshes a
// project's updated_at, so only a truly quiet deploy times out.
type DeployWatchdog struct {
	store    WatchdogStore
	timeout  time.Duration
	interval time.Duration
}

func NewDeployWatchdog(s WatchdogStore, timeout, interval time.Duration) *DeployWatchdog {
	return &DeployWatchdog{store: s, timeout: timeout, interval: interval}
}

// Sweep fails every deploying project quiet for longer than the timeout and
// returns how many it changed. A project that finished in the meantime is left alone.
func (w *DeployWatchdog) Sweep(ctx context.Context, now time.Time) (int, error) {
	stale, err := w.store.ListStaleDeploying(ctx, now.Add(-w.timeout))
	if err != nil {
		return 0, fmt.Errorf("list stale deploys: %w", err)
	}
	failed := 0
	for _, p := range stale {
		ok, err := w.store.UpdateProjectIf(ctx, p.ID, model.StatusDeploying, map[string]any{
			"status":        model.StatusFailed,
			"error_message": fmt.Sprintf("%s for %s", WatchdogFailurePrefix, w.timeout),
		})
		if err != nil {
			return failed, fmt.Errorf("fail project %s: %w", p.ID, err)
		}
		if ok {
			failed++
			log.Printf("deploy watchdog: project %s failed (no update for %s)", p.ID, w.timeout)
		}
	}
	return failed, nil
}

// Run sweeps every interval until ctx is cancelled.
func (w *DeployWatchdog) Run(ctx context.Context) {
	t := time.NewTicker(w.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := w.Sweep(ctx, time.Now()); err != nil && ctx.Err() == nil {
				log.Printf("deploy watchdog: %v", err)
			}
		}
	}
}
