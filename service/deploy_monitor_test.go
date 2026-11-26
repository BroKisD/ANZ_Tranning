package service_test

import (
	"anz-training/internal/testutil"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"anz-training/model"
	"anz-training/service"
)

// scriptedWatcher returns its results in order and then repeats the last one.
type scriptedWatcher struct {
	mu      sync.Mutex
	results []service.DeployRun
	errs    []error // parallel to results; nil entries mean no error
	calls   int
	sinces  []time.Time
}

func (w *scriptedWatcher) DeployRun(_ context.Context, _ model.GitHubCreds, _ string, since time.Time) (service.DeployRun, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	i := w.calls
	if i >= len(w.results) {
		i = len(w.results) - 1
	}
	w.calls++
	w.sinces = append(w.sinces, since)
	var err error
	if i < len(w.errs) {
		err = w.errs[i]
	}
	return w.results[i], err
}

func newMonitor(t *testing.T, w service.RunWatcher) (*service.DeployMonitor, *testutil.MemStore) {
	t.Helper()
	st := testutil.NewMemStore()
	st.Add(model.Project{ID: "p1", Status: model.StatusDeploying, CurrentStep: model.StepMerged})
	m := service.NewDeployMonitor(st, w, service.DeployMonitorConfig{
		Poll: time.Millisecond, StartTimeout: 80 * time.Millisecond, Timeout: 2 * time.Second,
	})
	return m, st
}

func watch(m *service.DeployMonitor) {
	m.Watch(context.Background(), "p1", model.GitHubCreds{Owner: "acme", Token: "t"}, "https://github.com/acme/x", time.Unix(1000, 0))
}

var t0 = time.Unix(2000, 0)

func TestDeployMonitor_ProgressThenSuccess(t *testing.T) {
	w := &scriptedWatcher{results: []service.DeployRun{
		{Found: true, CreatedAt: t0},
		{Found: true, CreatedAt: t0, Step: model.StepBuilding},
		{Found: true, CreatedAt: t0, Step: model.StepBuilding}, // repeated: no extra write
		{Found: true, CreatedAt: t0, Step: model.StepTesting},
		{Found: true, CreatedAt: t0, Step: model.StepDeploying},
		{Found: true, CreatedAt: t0, Step: model.StepDeploying, Done: true, Conclusion: "success", LiveURL: "https://x-1.a.run.app"},
	}}
	m, st := newMonitor(t, w)
	watch(m)

	p := st.Get("p1")
	if p.Status != model.StatusSuccess || p.LiveURL != "https://x-1.a.run.app" {
		t.Fatalf("project = %+v", p)
	}
	want := []string{"deploying|building", "deploying|testing", "deploying|deploying", "success|deploying"}
	if got := st.Hist(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("writes = %v, want %v", got, want)
	}
	if !w.sinces[0].Equal(time.Unix(1000, 0)) {
		t.Fatalf("since = %v", w.sinces[0])
	}
}

func TestDeployMonitor_FailureNamesTheStage(t *testing.T) {
	for stage, step := range map[string]string{"build": model.StepBuilding, "test": model.StepTesting, "deploy": model.StepDeploying, "": ""} {
		w := &scriptedWatcher{results: []service.DeployRun{
			{Found: true, CreatedAt: t0, Step: step, Done: true, Conclusion: "failure", FailedStage: stage},
		}}
		m, st := newMonitor(t, w)
		watch(m)
		p := st.Get("p1")
		name := stage
		if name == "" {
			name = "setup"
		}
		if p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "failed at the "+name+" stage") {
			t.Fatalf("stage %q: project = %+v", stage, p)
		}
	}
}

func TestDeployMonitor_NoRunStartedFails(t *testing.T) {
	m, st := newMonitor(t, &scriptedWatcher{results: []service.DeployRun{{}}})
	watch(m)
	if p := st.Get("p1"); p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "did not start") {
		t.Fatalf("project = %+v", p)
	}
}

func TestDeployMonitor_CancelledRunFollowsTheNewerOne(t *testing.T) {
	t1 := t0.Add(time.Minute)
	w := &scriptedWatcher{results: []service.DeployRun{
		{Found: true, CreatedAt: t0, Step: model.StepBuilding, Done: true, Conclusion: "cancelled"}, // superseded by a re-deploy
		{Found: true, CreatedAt: t1, Step: model.StepDeploying, Done: true, Conclusion: "success", LiveURL: "https://x-1.a.run.app"},
	}}
	m, st := newMonitor(t, w)
	watch(m)
	if p := st.Get("p1"); p.Status != model.StatusSuccess {
		t.Fatalf("project = %+v", p)
	}
	if !w.sinces[1].After(t0) {
		t.Fatalf("after a cancelled run the watcher must look for runs newer than it, since = %v", w.sinces[1])
	}
}

func TestDeployMonitor_CancelledWithNoReplacementFails(t *testing.T) {
	w := &scriptedWatcher{results: []service.DeployRun{
		{Found: true, CreatedAt: t0, Done: true, Conclusion: "cancelled"},
		{}, // nothing newer
	}}
	m, st := newMonitor(t, w)
	watch(m)
	if p := st.Get("p1"); p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "cancelled") {
		t.Fatalf("project = %+v", p)
	}
}

func TestDeployMonitor_SuccessWithoutValidURLFails(t *testing.T) {
	for _, u := range []string{"", "http://x.example.com", "javascript:alert(1)"} {
		w := &scriptedWatcher{results: []service.DeployRun{
			{Found: true, CreatedAt: t0, Done: true, Conclusion: "success", LiveURL: u},
		}}
		m, st := newMonitor(t, w)
		watch(m)
		if p := st.Get("p1"); p.Status != model.StatusFailed || p.LiveURL != "" {
			t.Fatalf("url %q: project = %+v", u, p)
		}
	}
}

func TestDeployMonitor_ToleratesTransientErrors(t *testing.T) {
	w := &scriptedWatcher{
		results: []service.DeployRun{{}, {}, {Found: true, CreatedAt: t0, Done: true, Conclusion: "success", LiveURL: "https://x-1.a.run.app"}},
		errs:    []error{errors.New("github 502"), errors.New("github 502"), nil},
	}
	m, st := newMonitor(t, w)
	watch(m)
	if p := st.Get("p1"); p.Status != model.StatusSuccess {
		t.Fatalf("project = %+v", p)
	}
}

func TestDeployMonitor_TimeoutFails(t *testing.T) {
	st := testutil.NewMemStore()
	st.Add(model.Project{ID: "p1", Status: model.StatusDeploying, CurrentStep: model.StepMerged})
	m := service.NewDeployMonitor(st, &scriptedWatcher{results: []service.DeployRun{{Found: true, CreatedAt: t0, Step: model.StepBuilding}}},
		service.DeployMonitorConfig{Poll: time.Millisecond, StartTimeout: time.Second, Timeout: 50 * time.Millisecond})
	watch(m)
	if p := st.Get("p1"); p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, service.WatchdogFailurePrefix) {
		t.Fatalf("project = %+v", p)
	}
}

func TestDeployMonitor_ContextCancelLeavesProjectAlone(t *testing.T) {
	m, st := newMonitor(t, &scriptedWatcher{results: []service.DeployRun{{Found: true, CreatedAt: t0, Step: model.StepBuilding}}})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.Watch(ctx, "p1", model.GitHubCreds{}, "https://github.com/acme/x", time.Now()); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Watch did not stop on cancel")
	}
	if p := st.Get("p1"); p.Status != model.StatusDeploying {
		t.Fatalf("a shutdown must not fail the project (the watchdog handles it): %+v", p)
	}
}

func TestDeployMonitor_DoesNotOverwriteAFinishedProject(t *testing.T) {
	w := &scriptedWatcher{results: []service.DeployRun{{Found: true, CreatedAt: t0, Done: true, Conclusion: "failure", FailedStage: "build"}}}
	m, st := newMonitor(t, w)
	st.Projects["p1"].Status = model.StatusSuccess
	watch(m)
	if p := st.Get("p1"); p.Status != model.StatusSuccess || p.ErrorMessage != "" {
		t.Fatalf("project = %+v", p)
	}
}
