package model_test

import (
	"encoding/json"
	"strings"
	"testing"

	"anz-training/model"
)

func TestModels_SecretsAreNeverSerialized(t *testing.T) {
	u, _ := json.Marshal(model.User{Email: "a@b.co", PasswordHash: "hash-secret"})
	if strings.Contains(string(u), "hash-secret") {
		t.Fatalf("user JSON leaks the password hash: %s", u)
	}
	i, _ := json.Marshal(model.UserIntegration{Provider: "gcp", EncryptedCredentials: "cipher-secret"})
	if strings.Contains(string(i), "cipher-secret") {
		t.Fatalf("integration JSON leaks encrypted credentials: %s", i)
	}
}

func TestGCPCreds_JSONRoundTrip(t *testing.T) {
	in := model.GCPCreds{ProjectID: "p", Region: "r", ArtifactRepo: "a", WIFProvider: "w", ServiceAccount: "s"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"project_id":"p"`) || strings.Contains(string(b), "anthropic_secret_ref") {
		t.Fatalf("unexpected JSON: %s", b)
	}
	var out model.GCPCreds
	if err := json.Unmarshal(b, &out); err != nil || out != in {
		t.Fatalf("round trip = %+v, %v", out, err)
	}
}

func TestProjectStatuses_AreDistinct(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range []string{
		model.StatusPending, model.StatusGenerating, model.StatusWaitingForApproval,
		model.StatusDeploying, model.StatusSuccess, model.StatusFailed,
	} {
		if s == "" || seen[s] {
			t.Fatalf("status %q is empty or duplicated", s)
		}
		seen[s] = true
	}
}
