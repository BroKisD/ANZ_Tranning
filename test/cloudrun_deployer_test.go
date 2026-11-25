package test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"anz-training/model"
	"anz-training/service"
)

type fakeBinder struct {
	mu        sync.Mutex
	calls     []string
	ensureErr error
	creds     model.GCPCreds
}

func (b *fakeBinder) EnsureBase(context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "ensure")
	return b.ensureErr
}

func (b *fakeBinder) BindRepo(_ context.Context, owner, repo string) (model.GCPCreds, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, "bind:"+owner+"/"+repo)
	return b.creds, nil
}

type fakeDispatcher struct {
	gh                 model.GitHubCreds
	repoURL, file, ref string
	calls              int
	err                error
}

func (d *fakeDispatcher) DispatchWorkflow(_ context.Context, gh model.GitHubCreds, repoURL, file, ref string) error {
	d.gh, d.repoURL, d.file, d.ref = gh, repoURL, file, ref
	d.calls++
	return d.err
}

func TestCloudRunDeployer_PrepareOrder(t *testing.T) {
	b := &fakeBinder{creds: gcpCreds}
	d := service.NewCloudRunDeployer(b, &fakeDispatcher{})
	got, err := d.Prepare(context.Background(), service.DeployTarget{ProjectID: "p1", ServiceName: "my-app-1a2b", RepoOwner: "acme", RepoName: "my-app-1a2b"})
	if err != nil || got != gcpCreds {
		t.Fatalf("creds = %+v, %v", got, err)
	}
	if strings.Join(b.calls, ",") != "ensure,bind:acme/my-app-1a2b" {
		t.Fatalf("calls = %v", b.calls)
	}

	b2 := &fakeBinder{ensureErr: errors.New("boom")}
	if _, err := service.NewCloudRunDeployer(b2, &fakeDispatcher{}).Prepare(context.Background(), service.DeployTarget{RepoOwner: "acme", RepoName: "x"}); err == nil {
		t.Fatal("EnsureBase error must fail Prepare")
	}
	if len(b2.calls) != 1 {
		t.Fatalf("BindRepo must not run after a failed EnsureBase: %v", b2.calls)
	}
}

func TestCloudRunDeployer_Trigger(t *testing.T) {
	disp := &fakeDispatcher{}
	d := service.NewCloudRunDeployer(&fakeBinder{}, disp)
	if err := d.Trigger(context.Background(), ghCreds, "https://github.com/acme/repo"); err != nil {
		t.Fatal(err)
	}
	if disp.gh != ghCreds || disp.repoURL != "https://github.com/acme/repo" || disp.file != "deploy.yml" || disp.ref != "main" {
		t.Fatalf("dispatch = %+v", disp)
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
