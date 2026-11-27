package service_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"anz-training/model"
	"anz-training/service"
)

// Test doubles shared by the activity and runner tests.

type fakeHost struct {
	mu                          sync.Mutex
	calls                       []string
	createErr, pushErr, mergeEr error
	pushDir                     string
	mergeArgs                   []string
	createGCP                   model.GCPCreds
}

func (h *fakeHost) rec(s string) { h.mu.Lock(); h.calls = append(h.calls, s); h.mu.Unlock() }
func (h *fakeHost) count(s string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, c := range h.calls {
		if c == s {
			n++
		}
	}
	return n
}

func (h *fakeHost) CreateRepo(_ context.Context, gh model.GitHubCreds, gcp model.GCPCreds, projectID, name string) (string, error) {
	h.rec("create")
	h.mu.Lock()
	h.createGCP = gcp
	h.mu.Unlock()
	if h.createErr != nil {
		return "", h.createErr
	}
	if gh.Owner != "acme" || (gcp.ProjectID != "my-proj" && gcp.ProjectID != "") {
		return "", errors.New("wrong creds decrypted")
	}
	return "https://github.com/acme/" + projectID, nil
}

func (h *fakeHost) PushAndOpenPR(_ context.Context, dir string, _ model.GitHubCreds, repoURL, base string) (string, error) {
	h.rec("push")
	h.mu.Lock()
	h.pushDir = dir
	h.mu.Unlock()
	if h.pushErr != nil {
		return "", h.pushErr
	}
	return repoURL + "/pull/1", nil
}

func (h *fakeHost) MergePR(_ context.Context, _ model.GitHubCreds, repoURL, prURL string) error {
	h.rec("merge")
	h.mu.Lock()
	h.mergeArgs = []string{repoURL, prURL}
	h.mu.Unlock()
	return h.mergeEr
}

type fakeGen struct {
	mu       sync.Mutex
	reqs     []service.GenerateRequest
	err      error
	panicV   any
	block    chan struct{} // if set, generation waits for it (or ctx)
	dir      string
	cleanups []string
	cur, max int32
	delay    time.Duration
}

func (g *fakeGen) GenerateAndTest(ctx context.Context, req service.GenerateRequest) (string, error) {
	g.mu.Lock()
	g.reqs = append(g.reqs, req)
	g.mu.Unlock()
	n := atomic.AddInt32(&g.cur, 1)
	defer atomic.AddInt32(&g.cur, -1)
	for {
		m := atomic.LoadInt32(&g.max)
		if n <= m || atomic.CompareAndSwapInt32(&g.max, m, n) {
			break
		}
	}
	if g.panicV != nil {
		panic(g.panicV)
	}
	if g.delay > 0 {
		time.Sleep(g.delay)
	}
	if g.block != nil {
		select {
		case <-g.block:
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	if g.err != nil {
		return "", g.err
	}
	return g.dir, nil
}

func (g *fakeGen) Cleanup(id string) error {
	g.mu.Lock()
	g.cleanups = append(g.cleanups, id)
	g.mu.Unlock()
	return nil
}

type fakeSecretReader struct {
	val string
	err error
	ref string
}

func (f *fakeSecretReader) GetSecret(_ context.Context, ref string) (string, error) {
	f.ref = ref
	return f.val, f.err
}

type fakeDeployer struct {
	mu      sync.Mutex
	rec     func(string)
	targets []service.DeployTarget
	creds   model.GCPCreds
	err     error

	triggers   []string // repo URLs passed to Trigger
	triggerErr error
}

func (d *fakeDeployer) Prepare(_ context.Context, t service.DeployTarget) (model.GCPCreds, error) {
	d.rec("prepare")
	d.mu.Lock()
	d.targets = append(d.targets, t)
	d.mu.Unlock()
	return d.creds, d.err
}

func (d *fakeDeployer) Trigger(_ context.Context, _ model.GitHubCreds, repoURL string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.triggers = append(d.triggers, repoURL)
	return d.triggerErr
}
