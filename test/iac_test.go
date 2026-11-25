package test

import (
	"context"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"

	"anz-training/service"
)

func deployData() service.DeployData {
	return service.DeployData{
		BranchName: "main", WIFProvider: gcpCreds.WIFProvider, ServiceAccount: gcpCreds.ServiceAccount,
		Region: "asia-southeast1", GCPProjectID: "my-proj", ArtifactRepo: "ai-apps", ServiceName: "svc-1",
		WebhookURL: "https://b.example.com/hook", WebhookSecret: "sec", ProjectID: "proj-uuid",
	}
}

func TestRenderDeployWorkflow_Stages(t *testing.T) {
	out, err := service.RenderDeployWorkflow(deployData())
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("rendered workflow is not valid YAML: %v", err)
	}
	on, _ := doc["on"].(map[string]any)
	if _, ok := on["workflow_dispatch"]; !ok {
		t.Error("workflow_dispatch trigger missing (manual re-deploy)")
	}
	if _, ok := on["push"]; !ok {
		t.Error("push trigger missing (auto deploy after merge)")
	}

	// a re-deploy (or a push during a deploy) cancels the older run, which then reports nothing
	if conc, _ := doc["concurrency"].(map[string]any); conc == nil || conc["cancel-in-progress"] != true || conc["group"] == nil {
		t.Errorf("concurrency group with cancel-in-progress missing: %v", doc["concurrency"])
	}

	s := string(out)
	// stages run and are reported in this order
	order := []string{
		`"step":"building"`, "docker build", "docker push",
		`"step":"testing"`,
		`"step":"deploying"`, "gcloud run services replace service.yml",
		`"status":"success"`, `"status":"failed"`,
	}
	last := -1
	for _, want := range order {
		i := strings.Index(s, want)
		if i < 0 {
			t.Fatalf("workflow missing %q", want)
		}
		if i < last {
			t.Fatalf("%q appears before an earlier stage", want)
		}
		last = i
	}
	if !strings.Contains(s, "IMAGE_PLACEHOLDER") {
		t.Error("workflow must substitute the image into service.yml")
	}
	// a webhook problem must never fail the deploy
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, "curl ") && (!strings.Contains(line, "||") || !strings.Contains(line, "--fail-with-body")) {
			t.Errorf("notify curl must report HTTP errors and still never fail the job: %s", line)
		}
	}
	// an empty service URL must fail the deploy step, not be posted as a "success"
	for _, want := range []string{`URL="$(gcloud run services describe`, `[ -n "$URL" ]`} {
		if !strings.Contains(s, want) {
			t.Errorf("workflow missing %q", want)
		}
	}
}

func TestRenderServiceYAML(t *testing.T) {
	out, err := service.RenderServiceYAML(service.ServiceData{ServiceName: "my-app-1a2b3c4d"})
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Metadata struct {
			Name        string            `yaml:"name"`
			Annotations map[string]string `yaml:"annotations"`
		} `yaml:"metadata"`
		Spec struct {
			Template struct {
				Spec struct {
					ServiceAccountName string `yaml:"serviceAccountName"`
					Containers         []struct {
						Image string `yaml:"image"`
						Ports []struct {
							ContainerPort int `yaml:"containerPort"`
						} `yaml:"ports"`
					} `yaml:"containers"`
				} `yaml:"spec"`
			} `yaml:"template"`
		} `yaml:"spec"`
	}
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}
	c := doc.Spec.Template.Spec.Containers
	if doc.Metadata.Name != "my-app-1a2b3c4d" || doc.Metadata.Annotations["run.googleapis.com/invoker-iam-disabled"] != "true" ||
		len(c) != 1 || c[0].Image != "IMAGE_PLACEHOLDER" || len(c[0].Ports) != 1 || c[0].Ports[0].ContainerPort != 8080 {
		t.Fatalf("unexpected service.yml:\n%s", out)
	}
	if got := doc.Spec.Template.Spec.ServiceAccountName; got != "" {
		t.Errorf("no runtime account given, but serviceAccountName = %q", got)
	}
	// with a runtime account the app runs as it instead of the default compute account
	out2, err := service.RenderServiceYAML(service.ServiceData{ServiceName: "my-app", RuntimeServiceAccount: runtimeSA})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out2), "serviceAccountName: "+runtimeSA) {
		t.Errorf("serviceAccountName missing:\n%s", out2)
	}
	if _, err := service.RenderServiceYAML(service.ServiceData{ServiceName: "my-app", RuntimeServiceAccount: "x y"}); err == nil {
		t.Error("unsafe runtime account must be rejected")
	}
	for _, bad := range []string{`x"y`, "a b", "x\ny", "$(id)", ""} {
		if _, err := service.RenderServiceYAML(service.ServiceData{ServiceName: bad}); err == nil {
			t.Errorf("service name %q should be rejected", bad)
		}
	}
}

func TestGitHub_CreateRepo_CommitsIaC(t *testing.T) {
	f := newFakeGitHub(t)
	g := newGH(t, f, "")
	creds := gcpCreds
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
	root := t.TempDir()
	bare := filepath.Join(root, "acme", "repo.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, bare, "init", "-q", "--bare", "--initial-branch=main")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-q", "--initial-branch=main")
	write(t, filepath.Join(seed, "README.md"), "# repo\n")
	write(t, filepath.Join(seed, "service.yml"), "TRUSTED\n")
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	gitCmd(t, seed, "push", "-q", bare, "main")

	ws := t.TempDir()
	write(t, filepath.Join(ws, "main.go"), "package main\n")
	write(t, filepath.Join(ws, "service.yml"), "EVIL: expose everything\n") // generated code must not change IaC
	g := newGH(t, newFakeGitHub(t), "file://"+root)
	if _, err := g.PushAndOpenPR(context.Background(), ws, ghCreds, "https://github.com/acme/repo", "main"); err != nil {
		t.Fatal(err)
	}
	if got := gitCmd(t, bare, "show", "feature/ai-gen:service.yml"); strings.TrimSpace(got) != "TRUSTED" {
		t.Fatalf("service.yml on the PR branch = %q", got)
	}
}
