package test

import (
	"anz-training/internal/testutil"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"anz-training/crypto"
	"anz-training/handler"
	"anz-training/model"
)

// fakeIntegrationStore is an in-memory IntegrationStore keyed by user+provider.
type fakeIntegrationStore struct {
	rows map[string]model.UserIntegration
}

func newFakeStore() *fakeIntegrationStore {
	return &fakeIntegrationStore{rows: map[string]model.UserIntegration{}}
}

func (f *fakeIntegrationStore) UpsertIntegration(_ context.Context, in *model.UserIntegration) error {
	f.rows[in.UserID+"|"+in.Provider] = *in
	return nil
}
func (f *fakeIntegrationStore) GetIntegration(_ context.Context, u, p string) (*model.UserIntegration, error) {
	if r, ok := f.rows[u+"|"+p]; ok {
		return &r, nil
	}
	return nil, model.ErrNotFound
}
func (f *fakeIntegrationStore) ListIntegrations(_ context.Context, u string) ([]model.UserIntegration, error) {
	var out []model.UserIntegration
	for _, r := range f.rows {
		if r.UserID == u {
			out = append(out, r)
		}
	}
	return out, nil
}

type fakeSecrets struct {
	stored map[string]string
	err    error
}

func (f *fakeSecrets) PutSecret(_ context.Context, id, v string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	f.stored[id] = v
	return "projects/p/secrets/" + id + "/versions/latest", nil
}

func newIntegrationServer(t *testing.T, st *fakeIntegrationStore, sec handler.SecretStore) *httptest.Server {
	t.Helper()
	h := handler.NewIntegrationHandler(st, sec, testutil.TestKey(9), handler.HeaderUserID, func(_ context.Context, token string) (string, error) {
		if token != "ghp_good" {
			return "", errors.New("bad credentials")
		}
		return "acme", nil
	})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/integrations/github", h.ConnectGitHub)
	mux.HandleFunc("POST /api/integrations", h.UpsertIntegration)
	mux.HandleFunc("GET /api/integrations", h.ListIntegrations)
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func do(t *testing.T, s *httptest.Server, method, path, user string, body any) (int, string) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, s.URL+path, rdr)
	if user != "" {
		req.Header.Set("X-User-ID", user)
	}
	resp, err := s.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func decryptRow(t *testing.T, st *fakeIntegrationStore, user, provider string, into any) string {
	t.Helper()
	row, ok := st.rows[user+"|"+provider]
	if !ok {
		t.Fatalf("no %s row for %s", provider, user)
	}
	plain, err := crypto.Decrypt(testutil.TestKey(9), row.EncryptedCredentials)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(plain, into); err != nil {
		t.Fatal(err)
	}
	return row.EncryptedCredentials
}

func gcpBody() map[string]string {
	return map[string]string{
		"project_id": "my-proj", "region": "asia-southeast1",
		"wif_provider":    "projects/1/locations/global/workloadIdentityPools/p/providers/gh",
		"service_account": "deployer@my-proj.iam.gserviceaccount.com",
	}
}

func TestIntegration_ConnectGitHub(t *testing.T) {
	st := newFakeStore()
	s := newIntegrationServer(t, st, nil)
	good := map[string]string{"token": "ghp_good"}

	if code, _ := do(t, s, "POST", "/api/integrations/github", "", good); code != http.StatusUnauthorized {
		t.Fatalf("no user: got %d", code)
	}
	if code, _ := do(t, s, "POST", "/api/integrations/github", "u1", map[string]string{"token": ""}); code != http.StatusUnprocessableEntity {
		t.Fatalf("empty token: got %d", code)
	}
	if code, _ := do(t, s, "POST", "/api/integrations/github", "u1", map[string]string{"token": "ghp_bad"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("token rejected by github: got %d", code)
	}
	if _, ok := st.rows["u1|github"]; ok {
		t.Fatal("a rejected token must not be stored")
	}
	code, body := do(t, s, "POST", "/api/integrations/github", "u1", good)
	if code != http.StatusOK || strings.Contains(body, "ghp_good") {
		t.Fatalf("connect: got %d %s (response must not echo the token)", code, body)
	}
	var c model.GitHubCreds
	enc := decryptRow(t, st, "u1", model.ProviderGitHub, &c)
	if c.Owner != "acme" || c.Token != "ghp_good" {
		t.Fatalf("unexpected creds %#v", c)
	}
	if strings.Contains(enc, "ghp_good") {
		t.Fatal("token stored in plaintext")
	}

	// an explicit owner (an organization) overrides the token's own login
	do(t, s, "POST", "/api/integrations/github", "u2", map[string]string{"token": "ghp_good", "owner": "my-org"})
	decryptRow(t, st, "u2", model.ProviderGitHub, &c)
	if c.Owner != "my-org" {
		t.Fatalf("owner = %q", c.Owner)
	}
}

func TestIntegration_UpsertGCP_ValidationAndEncryption(t *testing.T) {
	st := newFakeStore()
	s := newIntegrationServer(t, st, nil)

	if code, _ := do(t, s, "POST", "/api/integrations", "u1", map[string]string{"project_id": "x"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("missing fields: got %d", code)
	}
	if code, _ := do(t, s, "POST", "/api/integrations", "u1", gcpBody()); code != http.StatusOK {
		t.Fatalf("upsert: got %d", code)
	}
	var c model.GCPCreds
	enc := decryptRow(t, st, "u1", model.ProviderGCP, &c)
	if c.ProjectID != "my-proj" || c.ArtifactRepo != "ai-apps" || c.AnthropicSecretRef != "" {
		t.Fatalf("unexpected creds %#v", c)
	}
	if strings.Contains(enc, "my-proj") {
		t.Fatal("credentials stored in plaintext")
	}

	// upsert again replaces (one row per user+provider)
	b := gcpBody()
	b["region"] = "us-central1"
	do(t, s, "POST", "/api/integrations", "u1", b)
	decryptRow(t, st, "u1", model.ProviderGCP, &c)
	if c.Region != "us-central1" || len(st.rows) != 1 {
		t.Fatalf("upsert did not replace: %#v rows=%d", c, len(st.rows))
	}
}

func TestIntegration_BYOK_StoresOnlyRef(t *testing.T) {
	st := newFakeStore()
	sec := &fakeSecrets{stored: map[string]string{}}
	s := newIntegrationServer(t, st, sec)

	b := gcpBody()
	b["anthropic_api_key"] = "sk-ant-SUPERSECRET"
	code, resp := do(t, s, "POST", "/api/integrations", "u1", b)
	if code != http.StatusOK || strings.Contains(resp, "SUPERSECRET") {
		t.Fatalf("got %d %s", code, resp)
	}
	if sec.stored["anthropic-key-u1"] != "sk-ant-SUPERSECRET" {
		t.Fatal("key not sent to secret store")
	}
	var c model.GCPCreds
	decryptRow(t, st, "u1", model.ProviderGCP, &c)
	if c.AnthropicSecretRef != "projects/p/secrets/anthropic-key-u1/versions/latest" {
		t.Fatalf("secret ref = %q", c.AnthropicSecretRef)
	}
	if strings.Contains(st.rows["u1|gcp"].EncryptedCredentials, "SUPERSECRET") {
		t.Fatal("raw key leaked into DB row")
	}

	// re-saving without a key keeps the existing ref
	do(t, s, "POST", "/api/integrations", "u1", gcpBody())
	decryptRow(t, st, "u1", model.ProviderGCP, &c)
	if c.AnthropicSecretRef == "" {
		t.Fatal("existing secret ref was dropped")
	}
}

func TestIntegration_BYOK_Failures(t *testing.T) {
	b := gcpBody()
	b["anthropic_api_key"] = "sk-ant-x"

	// BYOK disabled
	st := newFakeStore()
	s := newIntegrationServer(t, st, nil)
	if code, _ := do(t, s, "POST", "/api/integrations", "u1", b); code != http.StatusUnprocessableEntity {
		t.Fatalf("BYOK disabled: got %d", code)
	}

	// secret store failure must not save anything or echo the key
	st = newFakeStore()
	s = newIntegrationServer(t, st, &fakeSecrets{err: errors.New("boom sk-ant-x")})
	code, resp := do(t, s, "POST", "/api/integrations", "u1", b)
	if code != http.StatusBadGateway || strings.Contains(resp, "sk-ant-x") || len(st.rows) != 0 {
		t.Fatalf("got %d %s rows=%d", code, resp, len(st.rows))
	}
}

func TestIntegration_ListNeverReturnsCredentials(t *testing.T) {
	st := newFakeStore()
	s := newIntegrationServer(t, st, nil)
	do(t, s, "POST", "/api/integrations", "u1", gcpBody())
	do(t, s, "POST", "/api/integrations/github", "u1", map[string]string{"token": "ghp_good"})
	do(t, s, "POST", "/api/integrations", "u2", gcpBody())

	code, body := do(t, s, "GET", "/api/integrations", "u1", nil)
	if code != http.StatusOK {
		t.Fatalf("got %d", code)
	}
	for _, leak := range []string{"my-proj", "acme", "42", "wif", "service_account", "encrypted", "EncryptedCredentials"} {
		if strings.Contains(body, leak) {
			t.Fatalf("response leaks %q: %s", leak, body)
		}
	}
	var items []struct {
		Provider   string `json:"provider"`
		Configured bool   `json:"configured"`
	}
	if err := json.Unmarshal([]byte(body), &items); err != nil || len(items) != 2 {
		t.Fatalf("want 2 providers for u1, got %s (%v)", body, err)
	}
	if code, _ := do(t, s, "GET", "/api/integrations", "", nil); code != http.StatusUnauthorized {
		t.Fatalf("no user: got %d", code)
	}
}
