package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2/google"

	"anz-training/model"
)

const (
	gcpPoolID     = "ai-builder-pool"
	gcpProviderID = "github"
	gcpDeployerID = "ai-deployer"
	gcpRuntimeID  = "ai-app-runtime" // identity deployed apps run as: holds no roles
	githubIssuer  = "https://token.actions.githubusercontent.com"
	maxIAMRetries = 8
)

var (
	gcpAPIs = []string{
		"run.googleapis.com", "artifactregistry.googleapis.com", "iam.googleapis.com",
		"iamcredentials.googleapis.com", "sts.googleapis.com", "cloudresourcemanager.googleapis.com",
	}
	ghNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
)

// GCPError is a non-2xx answer from a Google API.
type GCPError struct {
	Status  int
	Message string
}

func (e *GCPError) Error() string { return fmt.Sprintf("google api %d: %s", e.Status, e.Message) }

// GCP prepares the platform's Google Cloud project for deployments: it creates
// what is missing (idempotently) and adds one trust binding per repo. It talks
// to the REST APIs with Application Default Credentials, so it needs no gcloud.
type GCP struct {
	cfg DeployConfig
	hc  *http.Client

	// PollEvery / PollMax bound the wait for long-running operations.
	PollEvery time.Duration
	PollMax   time.Duration
	// RetryEvery is the base wait between retries of IAM calls that fail only
	// because a just-created account has not propagated yet.
	RetryEvery time.Duration

	mu     sync.Mutex
	ready  bool
	number string // project number
}

// NewGCP builds the client; a nil hc uses Application Default Credentials.
func NewGCP(ctx context.Context, cfg DeployConfig, hc *http.Client) (*GCP, error) {
	if cfg.ProjectID == "" || cfg.Region == "" || cfg.ArtifactRepo == "" {
		return nil, errors.New("gcp: project, region and artifact repo are required")
	}
	for _, o := range cfg.AllowedOwners {
		if !ghNameRe.MatchString(o) {
			return nil, fmt.Errorf("gcp: invalid allowed owner %q", o)
		}
	}
	if len(cfg.AllowedOwners) == 0 {
		return nil, errors.New("gcp: at least one allowed GitHub owner is required")
	}
	if hc == nil {
		var err error
		if hc, err = google.DefaultClient(ctx, "https://www.googleapis.com/auth/cloud-platform"); err != nil {
			return nil, fmt.Errorf("gcp: application default credentials: %w", err)
		}
	}
	return &GCP{cfg: cfg, hc: hc, PollEvery: 2 * time.Second, PollMax: 3 * time.Minute, RetryEvery: 2 * time.Second}, nil
}

func (g *GCP) deployerEmail() string {
	return gcpDeployerID + "@" + g.cfg.ProjectID + ".iam.gserviceaccount.com"
}

func (g *GCP) runtimeEmail() string {
	return gcpRuntimeID + "@" + g.cfg.ProjectID + ".iam.gserviceaccount.com"
}

func (g *GCP) do(ctx context.Context, method, url string, in, out any) error {
	var rdr io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal(data, &e)
		msg := e.Error.Message
		if msg == "" {
			msg = http.StatusText(resp.StatusCode)
		}
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return &GCPError{Status: resp.StatusCode, Message: method + " " + shortURL(url) + ": " + msg}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

func shortURL(u string) string {
	if i := strings.Index(u, "?"); i >= 0 {
		u = u[:i]
	}
	return strings.TrimPrefix(u, "https://")
}

// exists reports whether a GET answers 200 (404 means missing).
func (g *GCP) exists(ctx context.Context, url string) (bool, error) {
	err := g.do(ctx, http.MethodGet, url, nil, nil)
	var ge *GCPError
	if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

// exists2 is exists that also decodes the resource.
func (g *GCP) exists2(ctx context.Context, url string, out any) (bool, error) {
	err := g.do(ctx, http.MethodGet, url, nil, out)
	var ge *GCPError
	if errors.As(err, &ge) && ge.Status == http.StatusNotFound {
		return false, nil
	}
	return err == nil, err
}

type operation struct {
	Name  string `json:"name"`
	Done  bool   `json:"done"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// create POSTs a create request and waits for its long-running operation.
func (g *GCP) create(ctx context.Context, host, url string, body any) error {
	err := g.mutate(ctx, http.MethodPost, host, url, body)
	var ge *GCPError
	if errors.As(err, &ge) && ge.Status == http.StatusConflict { // created meanwhile
		return nil
	}
	return err
}

// mutate sends a create/patch request and waits for its long-running operation.
func (g *GCP) mutate(ctx context.Context, method, host, url string, body any) error {
	var op operation
	if err := g.do(ctx, method, url, body, &op); err != nil {
		return err
	}
	deadline := time.Now().Add(g.PollMax)
	for !op.Done && op.Name != "" {
		if time.Now().After(deadline) {
			return fmt.Errorf("gcp: operation %s did not finish in %s", op.Name, g.PollMax)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.PollEvery):
		}
		if err := g.do(ctx, http.MethodGet, "https://"+host+"/v1/"+op.Name, nil, &op); err != nil {
			return err
		}
	}
	if op.Error != nil {
		return fmt.Errorf("gcp: operation %s failed: %s", op.Name, op.Error.Message)
	}
	return nil
}

// projectNumber returns (and caches) the numeric id of the project.
func (g *GCP) projectNumber(ctx context.Context) (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.number != "" {
		return g.number, nil
	}
	var p struct {
		ProjectNumber string `json:"projectNumber"`
	}
	if err := g.do(ctx, http.MethodGet, "https://cloudresourcemanager.googleapis.com/v1/projects/"+g.cfg.ProjectID, nil, &p); err != nil {
		return "", fmt.Errorf("project number: %w", err)
	}
	if p.ProjectNumber == "" {
		return "", errors.New("project number: empty")
	}
	g.number = p.ProjectNumber
	return g.number, nil
}

// retryableIAM reports whether an IAM call failed in a way that goes away by itself:
// 409 (the policy changed under us) or 400/404 (a just-created service account
// has not propagated yet).
func retryableIAM(err error) bool {
	var ge *GCPError
	if !errors.As(err, &ge) {
		return false
	}
	return ge.Status == http.StatusConflict || ge.Status == http.StatusBadRequest || ge.Status == http.StatusNotFound
}

// addBinding adds member to role in the IAM policy at policyURL (read-modify-write
// with the etag), retrying with a growing wait while the failure is transient.
func (g *GCP) addBinding(ctx context.Context, policyURL, role, member string) error {
	var last error
	for attempt := 0; attempt < maxIAMRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(g.RetryEvery * time.Duration(attempt)):
			}
		}
		done, err := g.tryAddBinding(ctx, policyURL, role, member)
		if err == nil && done {
			return nil
		}
		if err != nil && !retryableIAM(err) {
			return err
		}
		last = err
	}
	return fmt.Errorf("iam policy update did not succeed: %w", last)
}

// tryAddBinding makes one read-modify-write attempt.
func (g *GCP) tryAddBinding(ctx context.Context, policyURL, role, member string) (bool, error) {
	var pol map[string]any
	if err := g.do(ctx, http.MethodPost, policyURL+":getIamPolicy", map[string]any{}, &pol); err != nil {
		return false, err
	}
	bindings, _ := pol["bindings"].([]any)
	found := false
	for _, b := range bindings {
		bm, _ := b.(map[string]any)
		if bm["role"] != role || bm["condition"] != nil {
			continue
		}
		members, _ := bm["members"].([]any)
		for _, m := range members {
			if m == member {
				return true, nil // already bound
			}
		}
		bm["members"] = append(members, member)
		found = true
	}
	if !found {
		bindings = append(bindings, map[string]any{"role": role, "members": []any{member}})
	}
	pol["bindings"] = bindings
	if err := g.do(ctx, http.MethodPost, policyURL+":setIamPolicy", map[string]any{"policy": pol}, nil); err != nil {
		return false, err
	}
	return true, nil
}

// EnsureBase creates everything shared by all projects. It is idempotent and
// remembers a success for the life of the process.
func (g *GCP) EnsureBase(ctx context.Context) error {
	g.mu.Lock()
	ready := g.ready
	g.mu.Unlock()
	if ready {
		return nil
	}
	p, region := g.cfg.ProjectID, g.cfg.Region
	if _, err := g.projectNumber(ctx); err != nil {
		return err
	}

	// 1. APIs
	var disabled []string
	for _, api := range gcpAPIs {
		var s struct {
			State string `json:"state"`
		}
		if err := g.do(ctx, http.MethodGet, "https://serviceusage.googleapis.com/v1/projects/"+p+"/services/"+api, nil, &s); err != nil {
			return fmt.Errorf("check api %s: %w", api, err)
		}
		if s.State != "ENABLED" {
			disabled = append(disabled, api)
		}
	}
	if len(disabled) > 0 {
		if err := g.create(ctx, "serviceusage.googleapis.com", "https://serviceusage.googleapis.com/v1/projects/"+p+"/services:batchEnable",
			map[string]any{"serviceIds": disabled}); err != nil {
			return fmt.Errorf("enable apis: %w", err)
		}
	}

	// 2. Artifact Registry repo
	arBase := "https://artifactregistry.googleapis.com/v1/projects/" + p + "/locations/" + region + "/repositories"
	if ok, err := g.exists(ctx, arBase+"/"+g.cfg.ArtifactRepo); err != nil {
		return fmt.Errorf("check artifact repo: %w", err)
	} else if !ok {
		if err := g.create(ctx, "artifactregistry.googleapis.com", arBase+"?repositoryId="+g.cfg.ArtifactRepo,
			map[string]any{"format": "DOCKER", "description": "Images of apps built by AI App Builder"}); err != nil {
			return fmt.Errorf("create artifact repo: %w", err)
		}
	}

	// 3. deployer service account
	iamBase := "https://iam.googleapis.com/v1/projects/" + p
	email := g.deployerEmail()
	if ok, err := g.exists(ctx, iamBase+"/serviceAccounts/"+email); err != nil {
		return fmt.Errorf("check deployer account: %w", err)
	} else if !ok {
		if err := g.create(ctx, "iam.googleapis.com", iamBase+"/serviceAccounts", map[string]any{
			"accountId": gcpDeployerID, "serviceAccount": map[string]any{"displayName": "AI App Builder deployer"},
		}); err != nil {
			return fmt.Errorf("create deployer account: %w", err)
		}
	}

	// the account deployed apps run as: it holds no roles at all, so an app (or a
	// workflow in a bound repo) can never act as the platform or touch its secrets
	runtime := g.runtimeEmail()
	if ok, err := g.exists(ctx, iamBase+"/serviceAccounts/"+runtime); err != nil {
		return fmt.Errorf("check runtime account: %w", err)
	} else if !ok {
		if err := g.create(ctx, "iam.googleapis.com", iamBase+"/serviceAccounts", map[string]any{
			"accountId": gcpRuntimeID, "serviceAccount": map[string]any{"displayName": "AI App Builder app runtime (no roles)"},
		}); err != nil {
			return fmt.Errorf("create runtime account: %w", err)
		}
	}

	// 4. Workload Identity pool + GitHub provider
	poolURL := iamBase + "/locations/global/workloadIdentityPools"
	if ok, err := g.exists(ctx, poolURL+"/"+gcpPoolID); err != nil {
		return fmt.Errorf("check identity pool: %w", err)
	} else if !ok {
		if err := g.create(ctx, "iam.googleapis.com", poolURL+"?workloadIdentityPoolId="+gcpPoolID,
			map[string]any{"displayName": "AI App Builder"}); err != nil {
			return fmt.Errorf("create identity pool: %w", err)
		}
	}
	quoted := make([]string, len(g.cfg.AllowedOwners))
	for i, o := range g.cfg.AllowedOwners {
		quoted[i] = `"` + o + `"`
	}
	providerURL := poolURL + "/" + gcpPoolID + "/providers/" + gcpProviderID
	condition := "assertion.repository_owner in [" + strings.Join(quoted, ",") + "]"
	var prov struct {
		AttributeCondition string `json:"attributeCondition"`
	}
	if ok, err := g.exists2(ctx, providerURL, &prov); err != nil {
		return fmt.Errorf("check identity provider: %w", err)
	} else if !ok {
		if err := g.create(ctx, "iam.googleapis.com", poolURL+"/"+gcpPoolID+"/providers?workloadIdentityPoolProviderId="+gcpProviderID, map[string]any{
			"attributeMapping": map[string]string{
				"google.subject":             "assertion.sub",
				"attribute.repository":       "assertion.repository",
				"attribute.repository_owner": "assertion.repository_owner",
			},
			"attributeCondition": condition,
			"oidc":               map[string]any{"issuerUri": githubIssuer},
		}); err != nil {
			return fmt.Errorf("create identity provider: %w", err)
		}
	} else if prov.AttributeCondition != condition { // GCP_ALLOWED_GITHUB_OWNERS changed since the provider was created
		if err := g.mutate(ctx, http.MethodPatch, "iam.googleapis.com", providerURL+"?updateMask=attributeCondition",
			map[string]any{"attributeCondition": condition}); err != nil {
			return fmt.Errorf("update identity provider condition: %w", err)
		}
	}

	// 5. what the deployer may do
	member := "serviceAccount:" + email
	crm := "https://cloudresourcemanager.googleapis.com/v1/projects/" + p
	for _, role := range []string{"roles/run.admin", "roles/artifactregistry.writer"} {
		if err := g.addBinding(ctx, crm, role, member); err != nil {
			return fmt.Errorf("grant %s: %w", role, err)
		}
	}
	if err := g.addBinding(ctx, iamBase+"/serviceAccounts/"+runtime, "roles/iam.serviceAccountUser", member); err != nil {
		return fmt.Errorf("let deployer act as runtime account: %w", err)
	}

	g.mu.Lock()
	g.ready = true
	g.mu.Unlock()
	return nil
}

// BindRepo lets exactly one GitHub repo log in as the deployer and returns the
// values the deploy workflow needs.
func (g *GCP) BindRepo(ctx context.Context, owner, repo string) (model.GCPCreds, error) {
	if !ghNameRe.MatchString(owner) || !ghNameRe.MatchString(repo) {
		return model.GCPCreds{}, errors.New("gcp: invalid repository name")
	}
	allowed := false
	for _, o := range g.cfg.AllowedOwners {
		if o == owner { // exact: the GitHub token carries the canonical casing and the provider compares case-sensitively
			allowed = true
		}
	}
	if !allowed {
		return model.GCPCreds{}, fmt.Errorf("gcp: GitHub account %q is not in GCP_ALLOWED_GITHUB_OWNERS (compared case-sensitively)", owner)
	}
	number, err := g.projectNumber(ctx)
	if err != nil {
		return model.GCPCreds{}, err
	}
	pool := "projects/" + number + "/locations/global/workloadIdentityPools/" + gcpPoolID
	member := "principalSet://iam.googleapis.com/" + pool + "/attribute.repository/" + owner + "/" + repo
	policyURL := "https://iam.googleapis.com/v1/projects/" + g.cfg.ProjectID + "/serviceAccounts/" + g.deployerEmail()
	if err := g.addBinding(ctx, policyURL, "roles/iam.workloadIdentityUser", member); err != nil {
		return model.GCPCreds{}, fmt.Errorf("bind repo %s/%s: %w", owner, repo, err)
	}
	return model.GCPCreds{
		ProjectID: g.cfg.ProjectID, Region: g.cfg.Region, ArtifactRepo: g.cfg.ArtifactRepo,
		WIFProvider: pool + "/providers/" + gcpProviderID, ServiceAccount: g.deployerEmail(), RuntimeServiceAccount: g.runtimeEmail(),
	}, nil
}
