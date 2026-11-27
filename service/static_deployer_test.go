package service_test

import (
	"context"
	"strings"
	"testing"

	"anz-training/model"
	"anz-training/service"
)

var staticCreds = model.GCPCreds{
	ProjectID: "project-9991dcfb-85ec-4182-b68", Region: "asia-southeast1", ArtifactRepo: "anz-repo",
	WIFProvider:    "projects/386127327935/locations/global/workloadIdentityPools/github-pool/providers/github-provider",
	ServiceAccount: "anz-trainning@project-9991dcfb-85ec-4182-b68.iam.gserviceaccount.com",
}

func TestStaticDeployer_PrepareReturnsFixedCredsWithoutTouchingGCP(t *testing.T) {
	d, err := service.NewStaticDeployer(staticCreds, &fakeDispatcher{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := d.Prepare(context.Background(), service.DeployTarget{ProjectID: "p1", ServiceName: "my-app-1a2b", RepoOwner: "acme", RepoName: "my-app-1a2b"})
	if err != nil || got != staticCreds {
		t.Fatalf("Prepare = %+v, %v", got, err)
	}
	if got.RuntimeServiceAccount != "" {
		t.Fatal("static mode has no dedicated runtime account")
	}
}

func TestStaticDeployer_Trigger(t *testing.T) {
	disp := &fakeDispatcher{}
	d, _ := service.NewStaticDeployer(staticCreds, disp)
	if err := d.Trigger(context.Background(), ghCreds, "https://github.com/acme/repo"); err != nil {
		t.Fatal(err)
	}
	if disp.file != "deploy.yml" || disp.ref != "main" || disp.repoURL != "https://github.com/acme/repo" {
		t.Fatalf("dispatch = %+v", disp)
	}
}

func TestStaticDeployer_RejectsIncompleteCreds(t *testing.T) {
	for _, mut := range []func(*model.GCPCreds){
		func(c *model.GCPCreds) { c.ProjectID = "" }, func(c *model.GCPCreds) { c.Region = "" },
		func(c *model.GCPCreds) { c.ArtifactRepo = "" }, func(c *model.GCPCreds) { c.WIFProvider = "" },
		func(c *model.GCPCreds) { c.ServiceAccount = "" },
	} {
		c := staticCreds
		mut(&c)
		if _, err := service.NewStaticDeployer(c, &fakeDispatcher{}); err == nil {
			t.Errorf("%+v must be rejected", c)
		}
	}
}

func TestStaticCredsFromEnv(t *testing.T) {
	env := map[string]string{
		"GCP_PROJECT_ID": staticCreds.ProjectID, "GCP_ARTIFACT_REPO": "anz-repo",
		"GCP_WIF_PROVIDER": staticCreds.WIFProvider, "GCP_SERVICE_ACCOUNT": staticCreds.ServiceAccount,
	}
	got, on, err := service.StaticCredsFromEnv(envOf(env))
	if err != nil || !on || got != staticCreds { // region defaults to asia-southeast1
		t.Fatalf("got %+v on=%v err=%v", got, on, err)
	}
	// not configured: static mode is off, no error
	if _, on, err := service.StaticCredsFromEnv(envOf(map[string]string{"GCP_PROJECT_ID": "p"})); on || err != nil {
		t.Fatalf("without WIF settings: on=%v err=%v", on, err)
	}
	// half configured is a mistake worth failing on
	for _, drop := range []string{"GCP_PROJECT_ID", "GCP_ARTIFACT_REPO", "GCP_SERVICE_ACCOUNT"} {
		e := map[string]string{}
		for k, v := range env {
			if k != drop {
				e[k] = v
			}
		}
		if _, on, err := service.StaticCredsFromEnv(envOf(e)); !on || err == nil || !strings.Contains(err.Error(), drop) {
			t.Errorf("missing %s: on=%v err=%v", drop, on, err)
		}
	}
}
