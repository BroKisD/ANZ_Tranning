package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"anz-training/crypto"
	"anz-training/model"
	"anz-training/repository"
	"anz-training/service"
)

// compile-time checks that the real implementations satisfy the interfaces
var (
	_ service.ProjectStore      = (*repository.Store)(nil)
	_ service.IntegrationReader = (*repository.Store)(nil)
	_ service.RepoHost          = (*service.GitHub)(nil)
	_ service.CodeGenerator     = (*service.Claude)(nil)
	_ service.SecretReader      = (*service.SecretManager)(nil)
)

// ---- fakes ----

type memStore struct {
	mu       sync.Mutex
	projects map[string]*model.Project
	history  []string // "status|step" after every write
}

func (m *memStore) add(p model.Project) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := p
	m.projects[p.ID] = &cp
}

func (m *memStore) apply(p *model.Project, f map[string]any) {
	for k, v := range f {
		s, _ := v.(string)
		switch k {
		case "status":
			p.Status = s
		case "current_step":
			p.CurrentStep = s
		case "repo_url":
			p.RepoURL = s
		case "pr_url":
			p.PRURL = s
		case "live_url":
			p.LiveURL = s
		case "error_message":
			p.ErrorMessage = s
		}
	}
	m.history = append(m.history, p.Status+"|"+p.CurrentStep)
}

func (m *memStore) GetProject(_ context.Context, id string) (*model.Project, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[id]
	if !ok {
		return nil, model.ErrNotFound
	}
	cp := *p
	return &cp, nil
}

func (m *memStore) UpdateProject(_ context.Context, id string, f map[string]any) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[id]
	if !ok {
		return model.ErrNotFound
	}
	m.apply(p, f)
	return nil
}

func (m *memStore) UpdateProjectIf(_ context.Context, id, from string, f map[string]any) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p, ok := m.projects[id]
	if !ok || p.Status != from {
		return false, nil
	}
	m.apply(p, f)
	return true, nil
}

func (m *memStore) CreateProject(_ context.Context, p *model.Project) error {
	m.add(*p)
	return nil
}

func (m *memStore) get(id string) model.Project {
	p, _ := m.GetProject(context.Background(), id)
	return *p
}

func (m *memStore) hist() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.history...)
}

type memIntegrations struct {
	mu   sync.Mutex
	rows map[string]model.UserIntegration
}

func (m *memIntegrations) GetIntegration(_ context.Context, u, p string) (*model.UserIntegration, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[u+"|"+p]
	if !ok {
		return nil, model.ErrNotFound
	}
	return &r, nil
}

func (m *memIntegrations) put(t *testing.T, key []byte, user, provider string, creds any) {
	t.Helper()
	plain, _ := json.Marshal(creds)
	enc, err := crypto.Encrypt(key, plain)
	if err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.rows[user+"|"+provider] = model.UserIntegration{UserID: user, Provider: provider, EncryptedCredentials: enc}
	m.mu.Unlock()
}

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

// ---- harness ----

type orcEnv struct {
	t     *testing.T
	store *memStore
	ints  *memIntegrations
	host  *fakeHost
	gen   *fakeGen
	o     *service.Orchestrator
	key   []byte
}

func newOrc(t *testing.T, secrets service.SecretReader, mods ...func(*service.OrchestratorConfig)) *orcEnv {
	t.Helper()
	e := &orcEnv{
		t:     t,
		store: &memStore{projects: map[string]*model.Project{}},
		ints:  &memIntegrations{rows: map[string]model.UserIntegration{}},
		host:  &fakeHost{},
		gen:   &fakeGen{dir: t.TempDir()},
		key:   testKey(5),
	}
	e.ints.put(t, e.key, "u1", model.ProviderGitHub, model.GitHubCreds{Owner: "acme", Token: "ghp_x"})
	e.ints.put(t, e.key, "u1", model.ProviderGCP, gcpCreds)
	e.store.add(model.Project{ID: "p1", OwnerID: "u1", Name: "My App", Prompt: "todo app", Status: model.StatusPending, CurrentStep: model.StepInitializing, BranchName: "main"})

	cfg := service.OrchestratorConfig{MasterKey: e.key, ApprovalTimeout: 5 * time.Second, StageTimeout: 5 * time.Second}
	for _, m := range mods {
		m(&cfg)
	}
	e.o = service.NewOrchestrator(e.store, e.ints, e.host, e.gen, secrets, cfg)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = e.o.Shutdown(ctx)
	})
	return e
}

// runAsync starts Run in a goroutine and returns a channel closed when it ends.
func (e *orcEnv) runAsync(ctx context.Context, id string) <-chan struct{} {
	done := make(chan struct{})
	go func() { e.o.Run(ctx, id); close(done) }()
	return done
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (e *orcEnv) waitStatus(id, status string) {
	e.t.Helper()
	waitFor(e.t, "status "+status, func() bool { return e.store.get(id).Status == status })
}

func finish(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return")
	}
}

// ---- tests ----

func TestOrchestrator_HappyPath(t *testing.T) {
	e := newOrc(t, nil)
	done := e.runAsync(context.Background(), "p1")

	e.waitStatus("p1", model.StatusWaitingForApproval)
	p := e.store.get("p1")
	if p.RepoURL != "https://github.com/acme/p1" || p.PRURL != "https://github.com/acme/p1/pull/1" || p.CurrentStep != model.StepTestsPassed {
		t.Fatalf("project at gate: %+v", p)
	}
	if e.host.count("merge") != 0 {
		t.Fatal("merged before approval")
	}
	if e.host.pushDir != e.gen.dir {
		t.Fatalf("PR pushed from %q, want generated workspace %q", e.host.pushDir, e.gen.dir)
	}
	e.gen.mu.Lock()
	cleanups := len(e.gen.cleanups)
	e.gen.mu.Unlock()
	if cleanups == 0 {
		t.Fatal("workspace not cleaned before the long approval wait")
	}

	if err := e.o.Approve(context.Background(), "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	finish(t, done)

	p = e.store.get("p1")
	if p.Status != model.StatusDeploying || p.CurrentStep != model.StepMerged || p.ErrorMessage != "" {
		t.Fatalf("final project: %+v", p)
	}
	if got := e.host.mergeArgs; len(got) != 2 || got[0] != p.RepoURL || got[1] != p.PRURL {
		t.Fatalf("merge args = %v", got)
	}
	want := []string{
		"generating|initializing",
		"generating|repo_created",
		"generating|tests_passed",
		"waiting_for_approval|tests_passed",
		"deploying|merging",
		"deploying|merged",
	}
	if got := e.store.hist(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("state transitions:\n got  %v\n want %v", got, want)
	}
	if r := e.gen.reqs[0]; r.ProjectID != "p1" || r.Prompt != "todo app" || r.AnthropicAPIKey != "" {
		t.Fatalf("generate request = %+v", r)
	}
}

func TestOrchestrator_ApproveGuards(t *testing.T) {
	block := make(chan struct{})
	e := newOrc(t, nil)
	e.gen.block = block
	ctx := context.Background()

	// pending: not approvable
	if err := e.o.Approve(ctx, "p1", "u1"); !errors.Is(err, service.ErrNotWaiting) {
		t.Fatalf("pending: %v", err)
	}
	done := e.runAsync(ctx, "p1")
	waitFor(t, "generation start", func() bool { e.gen.mu.Lock(); defer e.gen.mu.Unlock(); return len(e.gen.reqs) == 1 })
	if err := e.o.Approve(ctx, "p1", "u1"); !errors.Is(err, service.ErrNotWaiting) {
		t.Fatalf("generating: %v", err)
	}
	close(block)
	e.waitStatus("p1", model.StatusWaitingForApproval)

	if err := e.o.Approve(ctx, "p1", "intruder"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("other owner must see not-found, got %v", err)
	}
	if err := e.o.Approve(ctx, "nope", "u1"); !errors.Is(err, model.ErrNotFound) {
		t.Fatalf("unknown project: %v", err)
	}
	if e.host.count("merge") != 0 {
		t.Fatal("rejected approvals must not merge")
	}

	// double approve is harmless and merges exactly once
	if err := e.o.Approve(ctx, "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	_ = e.o.Approve(ctx, "p1", "u1")
	finish(t, done)
	if e.host.count("merge") != 1 {
		t.Fatalf("merge called %d times", e.host.count("merge"))
	}
	if err := e.o.Approve(ctx, "p1", "u1"); !errors.Is(err, service.ErrNotWaiting) {
		t.Fatalf("after deploy: %v", err)
	}
}

func TestOrchestrator_StageFailures(t *testing.T) {
	cases := []struct {
		name      string
		setup     func(e *orcEnv)
		wantMsg   string
		wantCalls map[string]int // host calls expected
	}{
		{"github not connected", func(e *orcEnv) { delete(e.ints.rows, "u1|github") }, "github integration is not configured", map[string]int{"create": 0}},
		{"gcp not configured", func(e *orcEnv) { delete(e.ints.rows, "u1|gcp") }, "gcp integration is not configured", map[string]int{"create": 0}},
		{"undecryptable creds", func(e *orcEnv) { e.ints.put(e.t, testKey(1), "u1", "github", gcpCreds) }, "decrypt github integration", map[string]int{"create": 0}},
		{"create repo", func(e *orcEnv) { e.host.createErr = errors.New("quota exceeded") }, "create repo: quota exceeded", map[string]int{"create": 1, "push": 0}},
		{"generation", func(e *orcEnv) { e.gen.err = errors.New("tests still failing") }, "generate: tests still failing", map[string]int{"create": 1, "push": 0}},
		{"open pr", func(e *orcEnv) { e.host.pushErr = errors.New("push rejected") }, "open pull request: push rejected", map[string]int{"push": 1, "merge": 0}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newOrc(t, nil)
			tc.setup(e)
			finish(t, e.runAsync(context.Background(), "p1"))
			p := e.store.get("p1")
			if p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, tc.wantMsg) {
				t.Fatalf("status=%s msg=%q, want failed containing %q", p.Status, p.ErrorMessage, tc.wantMsg)
			}
			for call, n := range tc.wantCalls {
				if got := e.host.count(call); got != n {
					t.Errorf("%s called %d times, want %d", call, got, n)
				}
			}
		})
	}
}

func TestOrchestrator_PushFailureStillCleansWorkspace(t *testing.T) {
	e := newOrc(t, nil)
	e.host.pushErr = errors.New("push rejected")
	finish(t, e.runAsync(context.Background(), "p1"))
	e.gen.mu.Lock()
	defer e.gen.mu.Unlock()
	if len(e.gen.cleanups) == 0 {
		t.Fatal("workspace leaked after PR failure")
	}
}

func TestOrchestrator_MergeFailure(t *testing.T) {
	e := newOrc(t, nil)
	e.host.mergeEr = errors.New("not mergeable")
	done := e.runAsync(context.Background(), "p1")
	e.waitStatus("p1", model.StatusWaitingForApproval)
	if err := e.o.Approve(context.Background(), "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	finish(t, done)
	p := e.store.get("p1")
	if p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "merge pull request: not mergeable") {
		t.Fatalf("project: %+v", p)
	}
}

func TestOrchestrator_ApprovalTimeout(t *testing.T) {
	e := newOrc(t, nil, func(c *service.OrchestratorConfig) { c.ApprovalTimeout = 150 * time.Millisecond })
	finish(t, e.runAsync(context.Background(), "p1"))
	p := e.store.get("p1")
	if p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "approval timed out") {
		t.Fatalf("project: %+v", p)
	}
	if e.host.count("merge") != 0 {
		t.Fatal("merged after timeout")
	}
	if err := e.o.Approve(context.Background(), "p1", "u1"); !errors.Is(err, service.ErrNotWaiting) {
		t.Fatalf("late approve: %v", err)
	}
}

func TestOrchestrator_CancelWhileWaiting(t *testing.T) {
	e := newOrc(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := e.runAsync(ctx, "p1")
	e.waitStatus("p1", model.StatusWaitingForApproval)
	cancel()
	finish(t, done)
	p := e.store.get("p1")
	if p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "cancelled") {
		t.Fatalf("project: %+v", p)
	}
}

func TestOrchestrator_BYOK(t *testing.T) {
	// key is fetched via the stored ref and handed to the generator
	sec := &fakeSecretReader{val: "sk-ant-USER-KEY"}
	e := newOrc(t, sec)
	gcp := gcpCreds
	gcp.AnthropicSecretRef = "projects/p/secrets/anthropic-key-u1/versions/latest"
	e.ints.put(t, e.key, "u1", model.ProviderGCP, gcp)
	done := e.runAsync(context.Background(), "p1")
	e.waitStatus("p1", model.StatusWaitingForApproval)
	if sec.ref != gcp.AnthropicSecretRef || e.gen.reqs[0].AnthropicAPIKey != "sk-ant-USER-KEY" {
		t.Fatalf("ref=%q req=%+v", sec.ref, e.gen.reqs[0])
	}
	if err := e.o.Approve(context.Background(), "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	finish(t, done)
	if strings.Contains(fmt.Sprint(e.store.hist(), e.store.get("p1")), "sk-ant-USER-KEY") {
		t.Fatal("API key leaked into project state")
	}

	// BYOK configured but secrets unavailable: fail, never fall back to the platform key
	e2 := newOrc(t, nil)
	e2.ints.put(t, e2.key, "u1", model.ProviderGCP, gcp)
	finish(t, e2.runAsync(context.Background(), "p1"))
	if p := e2.store.get("p1"); p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "BYOK") || len(e2.gen.reqs) != 0 {
		t.Fatalf("project: %+v reqs=%d", p, len(e2.gen.reqs))
	}

	// secret read failure
	e3 := newOrc(t, &fakeSecretReader{err: errors.New("permission denied")})
	e3.ints.put(t, e3.key, "u1", model.ProviderGCP, gcp)
	finish(t, e3.runAsync(context.Background(), "p1"))
	if p := e3.store.get("p1"); p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "read BYOK api key") {
		t.Fatalf("project: %+v", p)
	}
}

func TestOrchestrator_NoDoubleRun(t *testing.T) {
	e := newOrc(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); e.o.Run(context.Background(), "p1") }()
	}
	e.waitStatus("p1", model.StatusWaitingForApproval)
	if err := e.o.Approve(context.Background(), "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	if n := e.host.count("create"); n != 1 {
		t.Fatalf("CreateRepo ran %d times for one project", n)
	}

	// Start refuses a project that is already running
	e2 := newOrc(t, nil)
	if err := e2.o.Start("p1"); err != nil {
		t.Fatal(err)
	}
	if err := e2.o.Start("p1"); !errors.Is(err, service.ErrAlreadyRunning) {
		t.Fatalf("second Start: %v", err)
	}
	e2.waitStatus("p1", model.StatusWaitingForApproval)
}

func TestOrchestrator_ShutdownCancelsRuns(t *testing.T) {
	e := newOrc(t, nil)
	if err := e.o.Start("p1"); err != nil {
		t.Fatal(err)
	}
	e.waitStatus("p1", model.StatusWaitingForApproval)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := e.o.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	if p := e.store.get("p1"); p.Status != model.StatusFailed {
		t.Fatalf("status after shutdown = %s", p.Status)
	}
}

func TestOrchestrator_GenerationConcurrencyLimit(t *testing.T) {
	e := newOrc(t, nil, func(c *service.OrchestratorConfig) { c.MaxConcurrentGenerations = 1 })
	e.gen.delay = 80 * time.Millisecond
	e.store.add(model.Project{ID: "p2", OwnerID: "u1", Name: "Two", Prompt: "x", Status: model.StatusPending})
	e.store.add(model.Project{ID: "p3", OwnerID: "u1", Name: "Three", Prompt: "x", Status: model.StatusPending})
	for _, id := range []string{"p1", "p2", "p3"} {
		e.runAsync(context.Background(), id)
	}
	for _, id := range []string{"p1", "p2", "p3"} {
		e.waitStatus(id, model.StatusWaitingForApproval)
	}
	if m := atomic.LoadInt32(&e.gen.max); m != 1 {
		t.Fatalf("max concurrent generations = %d, want 1", m)
	}
}

func TestOrchestrator_PanicAndLongErrors(t *testing.T) {
	e := newOrc(t, nil)
	e.gen.panicV = "boom"
	finish(t, e.runAsync(context.Background(), "p1")) // must not crash the test binary
	if p := e.store.get("p1"); p.Status != model.StatusFailed || p.ErrorMessage != "internal error" {
		t.Fatalf("project: %+v", p)
	}

	e2 := newOrc(t, nil)
	e2.gen.err = errors.New(strings.Repeat("x", 5000))
	finish(t, e2.runAsync(context.Background(), "p1"))
	if msg := e2.store.get("p1").ErrorMessage; len(msg) > 1100 {
		t.Fatalf("error message not truncated: %d bytes", len(msg))
	}
}

func TestOrchestrator_AlreadyStartedProjectIsLeftAlone(t *testing.T) {
	e := newOrc(t, nil)
	e.store.add(model.Project{ID: "p9", OwnerID: "u1", Name: "x", Prompt: "x", Status: model.StatusSuccess})
	finish(t, e.runAsync(context.Background(), "p9"))
	if e.host.count("create") != 0 || e.store.get("p9").Status != model.StatusSuccess {
		t.Fatal("Run touched a project that is not pending")
	}
}

// RepoOnly (step 1 demo): only GitHub is needed, the run stops after the repo exists.
func TestOrchestrator_RepoOnly(t *testing.T) {
	e := newOrc(t, nil, func(c *service.OrchestratorConfig) { c.RepoOnly = true })
	delete(e.ints.rows, "u1|gcp") // no GCP integration at all
	e.o.Run(context.Background(), "p1")

	p := e.store.get("p1")
	if p.Status != model.StatusSuccess || p.CurrentStep != model.StepRepoCreated || p.RepoURL == "" {
		t.Fatalf("got status=%s step=%s repo=%q", p.Status, p.CurrentStep, p.RepoURL)
	}
	if len(e.gen.reqs) != 0 {
		t.Fatal("code generation must not run in repo-only mode")
	}
}

// Demo mode: full flow without GCP, ends as success once the approved PR is merged.
func TestOrchestrator_Demo(t *testing.T) {
	e := newOrc(t, nil, func(c *service.OrchestratorConfig) { c.Demo = true })
	delete(e.ints.rows, "u1|gcp")
	done := e.runAsync(context.Background(), "p1")

	e.waitStatus("p1", model.StatusWaitingForApproval)
	if p := e.store.get("p1"); p.PRURL == "" || p.RepoURL == "" || p.CurrentStep != model.StepTestsPassed {
		t.Fatalf("project at gate: %+v", p)
	}
	if err := e.o.Approve(context.Background(), "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	finish(t, done)
	if p := e.store.get("p1"); p.Status != model.StatusSuccess || p.ErrorMessage != "" {
		t.Fatalf("final project: %+v", p)
	}
	if e.host.count("merge") != 1 {
		t.Fatal("PR was not merged")
	}
}

// ---- deploy mode: a Deployer prepares the target, the webhook finishes the project ----

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

func newDeployOrc(t *testing.T, fd *fakeDeployer) *orcEnv {
	t.Helper()
	e := newOrc(t, nil, func(c *service.OrchestratorConfig) { c.Deployer = fd })
	fd.rec = e.host.rec
	delete(e.ints.rows, "u1|gcp") // the platform's GCP is used, not the user's
	return e
}

func TestOrchestrator_Deploy_PreparesBeforeRepo(t *testing.T) {
	fd := &fakeDeployer{creds: gcpCreds}
	e := newDeployOrc(t, fd)
	e.runAsync(context.Background(), "p1")
	e.waitStatus("p1", model.StatusWaitingForApproval)

	if len(e.host.calls) < 2 || e.host.calls[0] != "prepare" || e.host.calls[1] != "create" {
		t.Fatalf("call order = %v, want prepare then create", e.host.calls)
	}
	if e.host.createGCP != gcpCreds {
		t.Fatalf("CreateRepo got %+v, want the deployer's creds", e.host.createGCP)
	}
	name := service.ServiceName("My App", "p1")
	if got := fd.targets[0]; got != (service.DeployTarget{ProjectID: "p1", ServiceName: name, RepoOwner: "acme", RepoName: name}) {
		t.Fatalf("target = %+v", got)
	}
	if len(e.gen.reqs) == 0 || e.gen.reqs[0].AnthropicAPIKey != "" {
		t.Fatalf("generate request = %+v", e.gen.reqs)
	}
}

func TestOrchestrator_Deploy_EndsWaitingForWebhook(t *testing.T) {
	e := newDeployOrc(t, &fakeDeployer{creds: gcpCreds})
	done := e.runAsync(context.Background(), "p1")
	e.waitStatus("p1", model.StatusWaitingForApproval)
	if err := e.o.Approve(context.Background(), "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	finish(t, done)
	p := e.store.get("p1")
	if p.Status != model.StatusDeploying || p.CurrentStep != model.StepMerged || p.LiveURL != "" || p.ErrorMessage != "" {
		t.Fatalf("after merge: %+v (the webhook, not the orchestrator, must finish a deploy)", p)
	}
	if e.host.count("merge") != 1 {
		t.Fatal("PR was not merged")
	}
}

func TestOrchestrator_Deploy_PrepareFailure(t *testing.T) {
	e := newDeployOrc(t, &fakeDeployer{err: errors.New("iam denied")})
	e.o.Run(context.Background(), "p1")
	p := e.store.get("p1")
	if p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "prepare") || !strings.Contains(p.ErrorMessage, "iam denied") {
		t.Fatalf("project: %+v", p)
	}
	if e.host.count("create") != 0 {
		t.Fatal("repo must not be created when preparing the deployment failed")
	}
}

// ---- manual re-deploy ----

func addDeployed(e *orcEnv, id, owner, status, step string) {
	e.store.add(model.Project{ID: id, OwnerID: owner, Name: "x", Prompt: "x", Status: status, CurrentStep: step,
		RepoURL: "https://github.com/acme/x", ErrorMessage: "old error"})
}

func TestOrchestrator_Redeploy_FromSuccessAndFailedAfterMerge(t *testing.T) {
	for _, tc := range []struct{ status, step string }{
		{model.StatusSuccess, model.StepDeploying},
		{model.StatusFailed, model.StepTesting},
		{model.StatusFailed, model.StepMerged}, // merged, but the workflow never reported
	} {
		fd := &fakeDeployer{}
		e := newDeployOrc(t, fd)
		addDeployed(e, "r1", "u1", tc.status, tc.step)
		if err := e.o.Redeploy(context.Background(), "r1", "u1"); err != nil {
			t.Fatalf("%s/%s: %v", tc.status, tc.step, err)
		}
		p := e.store.get("r1")
		if p.Status != model.StatusDeploying || p.CurrentStep != model.StepMerged || p.ErrorMessage != "" {
			t.Fatalf("%s/%s: project %+v", tc.status, tc.step, p)
		}
		if len(fd.triggers) != 1 || fd.triggers[0] != "https://github.com/acme/x" {
			t.Fatalf("triggers = %v", fd.triggers)
		}
	}
}

func TestOrchestrator_Redeploy_Refusals(t *testing.T) {
	cases := []struct {
		name, owner, status, step string
		want                      error
	}{
		{"failed before the merge", "u1", model.StatusFailed, model.StepRepoCreated, service.ErrNotRedeployable},
		{"merge itself failed", "u1", model.StatusFailed, model.StepMerging, service.ErrNotRedeployable},
		{"already deploying", "u1", model.StatusDeploying, model.StepBuilding, service.ErrNotRedeployable},
		{"waiting for approval", "u1", model.StatusWaitingForApproval, model.StepTestsPassed, service.ErrNotRedeployable},
		{"someone else's project", "u2", model.StatusSuccess, model.StepDeploying, model.ErrNotFound},
	}
	for _, tc := range cases {
		fd := &fakeDeployer{}
		e := newDeployOrc(t, fd)
		addDeployed(e, "r1", "u1", tc.status, tc.step)
		before := e.store.get("r1")
		if err := e.o.Redeploy(context.Background(), "r1", tc.owner); !errors.Is(err, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, err, tc.want)
		}
		if len(fd.triggers) != 0 || e.store.get("r1") != before {
			t.Errorf("%s: project changed or trigger called", tc.name)
		}
	}
	// without a Deployer there is nothing to trigger
	e := newOrc(t, nil)
	addDeployed(e, "r1", "u1", model.StatusSuccess, model.StepDeploying)
	if err := e.o.Redeploy(context.Background(), "r1", "u1"); !errors.Is(err, service.ErrNotRedeployable) {
		t.Errorf("no deployer: %v", err)
	}
}

func TestOrchestrator_Redeploy_TriggerFailureMarksFailed(t *testing.T) {
	fd := &fakeDeployer{triggerErr: errors.New("dispatch refused")}
	e := newDeployOrc(t, fd)
	addDeployed(e, "r1", "u1", model.StatusSuccess, model.StepDeploying)
	if err := e.o.Redeploy(context.Background(), "r1", "u1"); err == nil {
		t.Fatal("trigger error must be returned")
	}
	if p := e.store.get("r1"); p.Status != model.StatusFailed || !strings.Contains(p.ErrorMessage, "dispatch refused") {
		t.Fatalf("project: %+v", p)
	}
}

// A merge that fails leaves nothing deployable: a re-deploy must be refused, not run an empty main.
func TestOrchestrator_Deploy_MergeFailureIsNotRedeployable(t *testing.T) {
	fd := &fakeDeployer{creds: gcpCreds}
	e := newDeployOrc(t, fd)
	e.host.mergeEr = errors.New("conflict")
	done := e.runAsync(context.Background(), "p1")
	e.waitStatus("p1", model.StatusWaitingForApproval)
	if err := e.o.Approve(context.Background(), "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	finish(t, done)
	p := e.store.get("p1")
	if p.Status != model.StatusFailed || p.CurrentStep != model.StepMerging {
		t.Fatalf("after a failed merge: %+v", p)
	}
	if err := e.o.Redeploy(context.Background(), "p1", "u1"); !errors.Is(err, service.ErrNotRedeployable) {
		t.Fatalf("Redeploy after a failed merge = %v, want ErrNotRedeployable", err)
	}
	if len(fd.triggers) != 0 {
		t.Fatal("workflow was triggered for a project that never merged")
	}
}

// ---- deploy mode without a webhook: the monitor follows the run on GitHub ----

func newPollingOrc(t *testing.T, fd *fakeDeployer, w service.RunWatcher) *orcEnv {
	t.Helper()
	e := newOrc(t, nil, func(c *service.OrchestratorConfig) {
		c.Deployer = fd
		c.RunWatcher = w
		c.Monitor = service.DeployMonitorConfig{Poll: time.Millisecond, StartTimeout: time.Second, Timeout: 5 * time.Second}
	})
	fd.rec = e.host.rec
	delete(e.ints.rows, "u1|gcp")
	return e
}

var okRun = service.DeployRun{Found: true, CreatedAt: time.Now(), Done: true, Conclusion: "success", Step: model.StepDeploying, LiveURL: "https://x-1.a.run.app"}

func TestOrchestrator_Polling_FinishesProjectAfterMerge(t *testing.T) {
	e := newPollingOrc(t, &fakeDeployer{creds: gcpCreds}, &scriptedWatcher{results: []service.DeployRun{{}, okRun}})
	done := e.runAsync(context.Background(), "p1")
	e.waitStatus("p1", model.StatusWaitingForApproval)
	if err := e.o.Approve(context.Background(), "p1", "u1"); err != nil {
		t.Fatal(err)
	}
	finish(t, done)
	if p := e.store.get("p1"); p.Status != model.StatusSuccess || p.LiveURL != "https://x-1.a.run.app" {
		t.Fatalf("project = %+v", p)
	}
}

func TestOrchestrator_Polling_RedeployFollowsTheNewRun(t *testing.T) {
	e := newPollingOrc(t, &fakeDeployer{}, &scriptedWatcher{results: []service.DeployRun{okRun}})
	addDeployed(e, "r1", "u1", model.StatusFailed, model.StepTesting)
	if err := e.o.Redeploy(context.Background(), "r1", "u1"); err != nil {
		t.Fatal(err)
	}
	e.waitStatus("r1", model.StatusSuccess)
	if p := e.store.get("r1"); p.LiveURL != "https://x-1.a.run.app" || p.ErrorMessage != "" {
		t.Fatalf("project = %+v", p)
	}
}
