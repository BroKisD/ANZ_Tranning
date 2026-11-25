package test

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"anz-training/model"
	"anz-training/service"
)

// wdStore is an in-memory service.WatchdogStore.
type wdStore struct {
	mu        sync.Mutex
	projects  map[string]*model.Project
	refuseCAS bool // pretend the project finished meanwhile
}

func newWDStore(ps ...model.Project) *wdStore {
	s := &wdStore{projects: map[string]*model.Project{}}
	for i := range ps {
		p := ps[i]
		s.projects[p.ID] = &p
	}
	return s
}

func (s *wdStore) ListStaleDeploying(_ context.Context, before time.Time) ([]model.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []model.Project
	for _, p := range s.projects {
		if p.Status == model.StatusDeploying && p.UpdatedAt.Before(before) {
			out = append(out, *p)
		}
	}
	return out, nil
}

func (s *wdStore) UpdateProjectIf(_ context.Context, id, from string, fields map[string]any) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.projects[id]
	if s.refuseCAS || p == nil || p.Status != from {
		return false, nil
	}
	if v, ok := fields["status"]; ok {
		p.Status = v.(string)
	}
	if v, ok := fields["error_message"]; ok {
		p.ErrorMessage = v.(string)
	}
	return true, nil
}

func (s *wdStore) get(id string) model.Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	return *s.projects[id]
}

func TestWatchdog_FailsStaleDeploying(t *testing.T) {
	now := time.Now()
	st := newWDStore(
		model.Project{ID: "stale", Status: model.StatusDeploying, UpdatedAt: now.Add(-20 * time.Minute)},
		model.Project{ID: "fresh", Status: model.StatusDeploying, UpdatedAt: now.Add(-1 * time.Minute)},
		model.Project{ID: "done", Status: model.StatusSuccess, UpdatedAt: now.Add(-20 * time.Minute)},
	)
	w := service.NewDeployWatchdog(st, 15*time.Minute, time.Minute)
	n, err := w.Sweep(context.Background(), now)
	if err != nil || n != 1 {
		t.Fatalf("Sweep = %d, %v; want 1", n, err)
	}
	if p := st.get("stale"); p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "no update from GitHub Actions") {
		t.Fatalf("stale project: %+v", p)
	}
	if st.get("fresh").Status != model.StatusDeploying || st.get("done").Status != model.StatusSuccess {
		t.Fatal("only the stale deploying project may change")
	}
}

func TestWatchdog_DoesNotOverrideFinishedProject(t *testing.T) {
	now := time.Now()
	st := newWDStore(model.Project{ID: "p", Status: model.StatusDeploying, UpdatedAt: now.Add(-time.Hour)})
	st.refuseCAS = true // the webhook finished it between list and update
	n, err := service.NewDeployWatchdog(st, time.Minute, time.Minute).Sweep(context.Background(), now)
	if err != nil || n != 0 {
		t.Fatalf("Sweep = %d, %v; want 0, nil", n, err)
	}
}

func TestWatchdog_RunStopsOnCancel(t *testing.T) {
	st := newWDStore(model.Project{ID: "p", Status: model.StatusDeploying, UpdatedAt: time.Now().Add(-time.Hour)})
	w := service.NewDeployWatchdog(st, time.Minute, 5*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { w.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for st.get("p").Status != model.StatusFailed && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if st.get("p").Status != model.StatusFailed {
		t.Fatal("Run never swept")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after cancel")
	}
}
