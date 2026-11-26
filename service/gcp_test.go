package service_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"anz-training/service"
)

// fakeGoogle implements just the Google REST endpoints service/gcp.go uses.
type fakeGoogle struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	enabled   map[string]bool
	repos     map[string]bool
	sas       map[string]bool
	pools     map[string]bool
	providers map[string]map[string]any
	policies  map[string]*fakePolicy // "project" or "sa:<email>"
	mutations int                    // create / enable / setIamPolicy calls that were accepted
	failPool  int                    // if set, pool creation answers with this status
	conflict  bool                   // next setIamPolicy answers 409 once
	transient int                    // next N IAM policy calls answer 400 "does not exist" (eventual consistency)
}

type fakePolicy struct {
	etag     int
	bindings map[string][]string // role -> members
}

func newFakeGoogle(t *testing.T) *fakeGoogle {
	f := &fakeGoogle{
		t: t, enabled: map[string]bool{}, repos: map[string]bool{}, sas: map[string]bool{},
		pools: map[string]bool{}, providers: map[string]map[string]any{}, policies: map[string]*fakePolicy{},
	}
	f.srv = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

// client sends every request to the fake, whatever Google host it names.
func (f *fakeGoogle) client() *http.Client {
	u, _ := url.Parse(f.srv.URL)
	return &http.Client{Transport: rtFunc(func(r *http.Request) (*http.Response, error) {
		r2 := r.Clone(r.Context())
		r2.URL.Scheme, r2.URL.Host, r2.Host = u.Scheme, u.Host, u.Host
		return http.DefaultTransport.RoundTrip(r2)
	})}
}

type rtFunc func(*http.Request) (*http.Response, error)

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func (f *fakeGoogle) policy(key string) *fakePolicy {
	if f.policies[key] == nil {
		f.policies[key] = &fakePolicy{etag: 1, bindings: map[string][]string{}}
	}
	return f.policies[key]
}

func (f *fakeGoogle) members(key, role string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.policy(key).bindings[role]...)
}

func (f *fakeGoogle) reply(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (f *fakeGoogle) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	lro := map[string]any{"name": "projects/p1/operations/op1", "done": false}
	notFound := map[string]any{"error": map[string]any{"code": 404, "message": "not found"}}

	switch {
	case strings.Contains(path, "/operations/"):
		f.reply(w, 200, map[string]any{"name": path, "done": true})

	case strings.HasSuffix(path, ":getIamPolicy"), strings.HasSuffix(path, ":setIamPolicy"):
		key := "project"
		if i := strings.Index(path, "/serviceAccounts/"); i >= 0 {
			key = "sa:" + strings.TrimSuffix(strings.TrimSuffix(path[i+len("/serviceAccounts/"):], ":getIamPolicy"), ":setIamPolicy")
		}
		p := f.policy(key)
		if f.transient > 0 {
			f.transient--
			f.reply(w, 400, map[string]any{"error": map[string]any{"code": 400, "message": "Service account does not exist"}})
			return
		}
		if strings.HasSuffix(path, ":setIamPolicy") {
			if f.conflict {
				f.conflict = false
				p.etag++ // someone else changed it first
				f.reply(w, 409, map[string]any{"error": map[string]any{"code": 409, "message": "etag mismatch"}})
				return
			}
			pol, _ := body["policy"].(map[string]any)
			if fmt.Sprint(pol["etag"]) != fmt.Sprint(p.etag) {
				f.reply(w, 409, map[string]any{"error": map[string]any{"code": 409, "message": "etag mismatch"}})
				return
			}
			p.bindings = map[string][]string{}
			bs, _ := pol["bindings"].([]any)
			for _, b := range bs {
				bm := b.(map[string]any)
				for _, m := range bm["members"].([]any) {
					p.bindings[bm["role"].(string)] = append(p.bindings[bm["role"].(string)], m.(string))
				}
			}
			p.etag++
			f.mutations++
		}
		var bs []map[string]any
		for role, members := range p.bindings {
			bs = append(bs, map[string]any{"role": role, "members": members})
		}
		f.reply(w, 200, map[string]any{"version": 1, "etag": fmt.Sprint(p.etag), "bindings": bs})

	case path == "/v1/projects/p1" && r.Method == "GET":
		f.reply(w, 200, map[string]any{"projectId": "p1", "projectNumber": "123456789"})

	case strings.Contains(path, "/services:batchEnable"):
		ids, _ := body["serviceIds"].([]any)
		for _, id := range ids {
			f.enabled[id.(string)] = true
		}
		f.mutations++
		f.reply(w, 200, map[string]any{"name": "operations/x", "done": true})
	case strings.Contains(path, "/services/") && r.Method == "GET":
		svc := path[strings.LastIndex(path, "/")+1:]
		state := "DISABLED"
		if f.enabled[svc] {
			state = "ENABLED"
		}
		f.reply(w, 200, map[string]any{"state": state})

	case strings.Contains(path, "/repositories"):
		id := r.URL.Query().Get("repositoryId")
		if r.Method == "POST" {
			f.repos[id] = true
			f.mutations++
			f.reply(w, 200, lro)
			return
		}
		if !f.repos[path[strings.LastIndex(path, "/")+1:]] {
			f.reply(w, 404, notFound)
			return
		}
		f.reply(w, 200, map[string]any{})

	case strings.HasSuffix(path, "/serviceAccounts") && r.Method == "POST":
		id, _ := body["accountId"].(string)
		f.sas[id+"@p1.iam.gserviceaccount.com"] = true
		f.mutations++
		f.reply(w, 200, map[string]any{"email": id + "@p1.iam.gserviceaccount.com"})
	case strings.Contains(path, "/serviceAccounts/") && r.Method == "GET":
		if !f.sas[path[strings.LastIndex(path, "/")+1:]] {
			f.reply(w, 404, notFound)
			return
		}
		f.reply(w, 200, map[string]any{})

	case strings.Contains(path, "/providers"):
		id := r.URL.Query().Get("workloadIdentityPoolProviderId")
		if r.Method == "POST" {
			f.providers[id] = body
			f.mutations++
			f.reply(w, 200, lro)
			return
		}
		prov := f.providers[path[strings.LastIndex(path, "/")+1:]]
		if prov == nil {
			f.reply(w, 404, notFound)
			return
		}
		if r.Method == "PATCH" {
			prov["attributeCondition"] = body["attributeCondition"]
			f.mutations++
			f.reply(w, 200, lro)
			return
		}
		f.reply(w, 200, map[string]any{"attributeCondition": prov["attributeCondition"]})
	case strings.Contains(path, "/workloadIdentityPools"):
		if r.Method == "POST" {
			if f.failPool != 0 {
				f.reply(w, f.failPool, map[string]any{"error": map[string]any{"code": f.failPool, "message": "permission denied"}})
				return
			}
			f.pools[r.URL.Query().Get("workloadIdentityPoolId")] = true
			f.mutations++
			f.reply(w, 200, lro)
			return
		}
		if !f.pools[path[strings.LastIndex(path, "/")+1:]] {
			f.reply(w, 404, notFound)
			return
		}
		f.reply(w, 200, map[string]any{})

	default:
		f.t.Errorf("fakeGoogle: unexpected %s %s", r.Method, path)
		f.reply(w, 500, map[string]any{"error": map[string]any{"message": "unexpected " + path}})
	}
}

func newTestGCP(t *testing.T, f *fakeGoogle) *service.GCP {
	t.Helper()
	g, err := service.NewGCP(context.Background(), service.DeployConfig{
		ProjectID: "p1", Region: "asia-southeast1", ArtifactRepo: "ai-apps", AllowedOwners: []string{"acme", "other"},
	}, f.client())
	if err != nil {
		t.Fatal(err)
	}
	g.PollEvery = time.Millisecond
	g.RetryEvery = time.Millisecond
	return g
}

const (
	deployerSA = "ai-deployer@p1.iam.gserviceaccount.com"
	runtimeSA  = "ai-app-runtime@p1.iam.gserviceaccount.com"
)

func TestGCP_EnsureBase_CreatesEverything(t *testing.T) {
	f := newFakeGoogle(t)
	if err := newTestGCP(t, f).EnsureBase(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, api := range []string{"run.googleapis.com", "artifactregistry.googleapis.com", "iam.googleapis.com",
		"iamcredentials.googleapis.com", "sts.googleapis.com", "cloudresourcemanager.googleapis.com"} {
		if !f.enabled[api] {
			t.Errorf("api %s not enabled", api)
		}
	}
	if !f.repos["ai-apps"] || !f.sas[deployerSA] || !f.sas[runtimeSA] || !f.pools["ai-builder-pool"] {
		t.Fatalf("missing resources: repos=%v sas=%v pools=%v", f.repos, f.sas, f.pools)
	}
	prov := f.providers["github"]
	oidc, _ := prov["oidc"].(map[string]any)
	cond, _ := prov["attributeCondition"].(string)
	if oidc["issuerUri"] != "https://token.actions.githubusercontent.com" || !strings.Contains(cond, "acme") || !strings.Contains(cond, "other") {
		t.Fatalf("provider = %v", prov)
	}
	member := "serviceAccount:" + deployerSA
	for _, role := range []string{"roles/run.admin", "roles/artifactregistry.writer"} {
		if got := f.members("project", role); len(got) != 1 || got[0] != member {
			t.Errorf("%s members = %v", role, got)
		}
	}
	// apps run as a dedicated account that holds no roles: the deployer may act as it ...
	if got := f.members("sa:"+runtimeSA, "roles/iam.serviceAccountUser"); len(got) != 1 || got[0] != member {
		t.Errorf("serviceAccountUser on runtime SA = %v", got)
	}
	// ... and never as the platform's own (default compute) account
	if got := f.members("sa:123456789-compute@developer.gserviceaccount.com", "roles/iam.serviceAccountUser"); len(got) != 0 {
		t.Errorf("deployer must not act as the platform account: %v", got)
	}
	for role, members := range f.policy("project").bindings {
		for _, m := range members {
			if m == "serviceAccount:"+runtimeSA {
				t.Errorf("runtime account must hold no project roles, has %s", role)
			}
		}
	}
}

func TestGCP_EnsureBase_Idempotent(t *testing.T) {
	f := newFakeGoogle(t)
	if err := newTestGCP(t, f).EnsureBase(context.Background()); err != nil {
		t.Fatal(err)
	}
	before := f.mutations
	if err := newTestGCP(t, f).EnsureBase(context.Background()); err != nil { // new instance: no in-memory cache
		t.Fatal(err)
	}
	if f.mutations != before {
		t.Fatalf("second EnsureBase made %d changes", f.mutations-before)
	}
}

func TestGCP_BindRepo_AddsPerRepoMember(t *testing.T) {
	f := newFakeGoogle(t)
	g := newTestGCP(t, f)
	if err := g.EnsureBase(context.Background()); err != nil {
		t.Fatal(err)
	}
	creds, err := g.BindRepo(context.Background(), "acme", "my-app-1a2b")
	if err != nil {
		t.Fatal(err)
	}
	want := "principalSet://iam.googleapis.com/projects/123456789/locations/global/workloadIdentityPools/ai-builder-pool/attribute.repository/acme/my-app-1a2b"
	if got := f.members("sa:"+deployerSA, "roles/iam.workloadIdentityUser"); len(got) != 1 || got[0] != want {
		t.Fatalf("members = %v, want [%s]", got, want)
	}
	if creds.ProjectID != "p1" || creds.Region != "asia-southeast1" || creds.ArtifactRepo != "ai-apps" || creds.ServiceAccount != deployerSA ||
		creds.RuntimeServiceAccount != runtimeSA ||
		creds.WIFProvider != "projects/123456789/locations/global/workloadIdentityPools/ai-builder-pool/providers/github" {
		t.Fatalf("creds = %+v", creds)
	}
	// binding the same repo again, and another repo, keeps exactly one member each
	_, _ = g.BindRepo(context.Background(), "acme", "my-app-1a2b")
	_, _ = g.BindRepo(context.Background(), "acme", "second-app")
	got := f.members("sa:"+deployerSA, "roles/iam.workloadIdentityUser")
	sort.Strings(got)
	if len(got) != 2 || !strings.HasSuffix(got[0], "/acme/my-app-1a2b") || !strings.HasSuffix(got[1], "/acme/second-app") {
		t.Fatalf("members after rebind = %v", got)
	}
}

func TestGCP_IAMEtagConflictRetried(t *testing.T) {
	f := newFakeGoogle(t)
	g := newTestGCP(t, f)
	if err := g.EnsureBase(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.conflict = true
	f.mu.Unlock()
	if _, err := g.BindRepo(context.Background(), "acme", "retry-app"); err != nil {
		t.Fatalf("conflict must be retried: %v", err)
	}
	if got := f.members("sa:"+deployerSA, "roles/iam.workloadIdentityUser"); len(got) != 1 {
		t.Fatalf("members = %v", got)
	}
}

func TestGCP_ErrorSurfaced(t *testing.T) {
	f := newFakeGoogle(t)
	f.failPool = 403
	err := newTestGCP(t, f).EnsureBase(context.Background())
	if err == nil || !strings.Contains(err.Error(), "403") {
		t.Fatalf("want a 403 error, got %v", err)
	}
}

func TestGCP_EnsureBase_UpdatesProviderCondition(t *testing.T) {
	f := newFakeGoogle(t)
	if err := newTestGCP(t, f).EnsureBase(context.Background()); err != nil {
		t.Fatal(err)
	}
	g2, err := service.NewGCP(context.Background(), service.DeployConfig{
		ProjectID: "p1", Region: "asia-southeast1", ArtifactRepo: "ai-apps", AllowedOwners: []string{"acme", "other", "third"},
	}, f.client())
	if err != nil {
		t.Fatal(err)
	}
	g2.PollEvery, g2.RetryEvery = time.Millisecond, time.Millisecond
	if err := g2.EnsureBase(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cond, _ := f.providers["github"]["attributeCondition"].(string); !strings.Contains(cond, "third") {
		t.Fatalf("a newly allowed owner must reach the identity provider, condition = %q", cond)
	}
}

func TestGCP_BindRepo_OwnerMustMatchExactly(t *testing.T) {
	f := newFakeGoogle(t)
	g := newTestGCP(t, f)
	// GitHub's token carries the canonical casing and CEL compares case-sensitively,
	// so a differently-cased owner would pass here and then fail at login: refuse it now
	for _, owner := range []string{"ACME", "Acme", "stranger"} {
		if _, err := g.BindRepo(context.Background(), owner, "app"); err == nil || !strings.Contains(err.Error(), "GCP_ALLOWED_GITHUB_OWNERS") {
			t.Errorf("owner %q: got %v, want a GCP_ALLOWED_GITHUB_OWNERS error", owner, err)
		}
	}
	if len(f.members("sa:"+deployerSA, "roles/iam.workloadIdentityUser")) != 0 {
		t.Fatal("a refused owner must not get a binding")
	}
}

func TestGCP_IAMEventualConsistencyRetried(t *testing.T) {
	f := newFakeGoogle(t)
	g := newTestGCP(t, f)
	if err := g.EnsureBase(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	f.transient = 3 // "service account does not exist" right after creating it
	f.mu.Unlock()
	if _, err := g.BindRepo(context.Background(), "acme", "fresh-app"); err != nil {
		t.Fatalf("transient 400s must be retried: %v", err)
	}
	if got := f.members("sa:"+deployerSA, "roles/iam.workloadIdentityUser"); len(got) != 1 {
		t.Fatalf("members = %v", got)
	}
}
