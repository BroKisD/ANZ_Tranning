package test

import (
	"strings"
	"testing"

	"anz-training/service"

	"go.yaml.in/yaml/v3"
)

func deployData() service.DeployData {
	return service.DeployData{BranchName: "main", ServiceName: "svc-1", ProjectID: "proj-uuid"}
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

func TestRenderDeployWorkflow_PollingSupport(t *testing.T) {
	out, err := service.RenderDeployWorkflow(deployData())
	if err != nil {
		t.Fatal(err)
	}
	s := string(out)
	// the platform reads the live URL from the run's annotations
	if !strings.Contains(s, "::notice title=live_url::${{ steps.deploy.outputs.url }}") {
		t.Error("workflow must publish the service URL as a live_url notice")
	}
	// without a webhook configured the notifier does nothing
	if !strings.Contains(s, `[ -n "$WEBHOOK_URL" ] || return 0`) {
		t.Error("notify must be a no-op when WEBHOOK_URL is empty")
	}
	// the polled stage names are the step names GitHub reports
	for _, name := range []string{"name: Build", "name: Test", "name: Deploy"} {
		if !strings.Contains(s, name) {
			t.Errorf("workflow missing step %q", name)
		}
	}
}
