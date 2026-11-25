package test

import (
	"reflect"
	"testing"
	"time"

	"anz-training/model"
	"anz-training/service"
)

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func TestDeployConfigFromEnv_Defaults(t *testing.T) {
	cfg, err := service.DeployConfigFromEnv(envOf(map[string]string{
		"GCP_PROJECT_ID": "p1", "GCP_ALLOWED_GITHUB_OWNERS": "a, b",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProjectID != "p1" || cfg.Region != "asia-southeast1" || cfg.ArtifactRepo != "ai-apps" || cfg.Timeout != 15*time.Minute {
		t.Fatalf("defaults wrong: %+v", cfg)
	}
	if !reflect.DeepEqual(cfg.AllowedOwners, []string{"a", "b"}) {
		t.Fatalf("owners = %v", cfg.AllowedOwners)
	}
}

func TestDeployConfigFromEnv_Errors(t *testing.T) {
	cases := map[string]map[string]string{
		"missing project": {"GCP_ALLOWED_GITHUB_OWNERS": "a"},
		"missing owners":  {"GCP_PROJECT_ID": "p1"},
		"bad timeout":     {"GCP_PROJECT_ID": "p1", "GCP_ALLOWED_GITHUB_OWNERS": "a", "DEPLOY_TIMEOUT": "abc"},
	}
	for name, env := range cases {
		if _, err := service.DeployConfigFromEnv(envOf(env)); err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestStepRank_Order(t *testing.T) {
	order := []string{model.StepMerging, model.StepMerged, model.StepBuilding, model.StepTesting, model.StepDeploying}
	for i := 1; i < len(order); i++ {
		if service.StepRank(order[i-1]) >= service.StepRank(order[i]) {
			t.Fatalf("%s must rank below %s", order[i-1], order[i])
		}
	}
	if service.StepRank("nope") != 0 || service.StepRank(model.StepRepoCreated) != 0 {
		t.Fatal("unknown steps must rank 0")
	}
}
