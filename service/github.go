// Package service holds the GitHub, Claude Code and orchestration logic.
package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"anz-training/helper"
	"anz-training/model"
	"anz-training/templates"
)

const (
	defaultAPIURL   = "https://api.github.com"
	defaultGitURL   = "https://github.com"
	workflowPath    = ".github/workflows/deploy.yml"
	serviceSpecPath = "service.yml"
	featureBranch   = "feature/ai-gen"
	defaultBase     = "main"
	maxErrBodySize  = 512
)

// GitHubConfig configures the GitHub client. Every call is made with the
// user's own Personal Access Token (model.GitHubCreds.Token). APIURL/GitURL only
// need to be set to point at test doubles.
type GitHubConfig struct {
	WebhookURL    string // injected into the generated deploy workflow
	WebhookSecret string // root secret; each repo gets helper.WebhookSecretFor(root, projectID)
	APIURL        string
	GitURL        string
	HTTPClient    *http.Client
	RetryDelay    time.Duration // wait between contents-API retries (default 1s)
}

type GitHub struct {
	webhookURL    string
	webhookSecret string
	apiURL        string
	gitURL        string
	http          *http.Client
	retryDelay    time.Duration
}

func NewGitHub(cfg GitHubConfig) *GitHub {
	g := &GitHub{
		webhookURL: cfg.WebhookURL, webhookSecret: cfg.WebhookSecret,
		apiURL: strings.TrimRight(cfg.APIURL, "/"), gitURL: strings.TrimRight(cfg.GitURL, "/"),
		http: cfg.HTTPClient, retryDelay: time.Second,
	}
	if g.apiURL == "" {
		g.apiURL = defaultAPIURL
	}
	if g.gitURL == "" {
		g.gitURL = defaultGitURL
	}
	if cfg.RetryDelay > 0 {
		g.retryDelay = cfg.RetryDelay
	}
	if g.http == nil {
		g.http = &http.Client{Timeout: 30 * time.Second}
	}
	return g
}

// tokenFor returns the user's PAT. It is only ever held in memory for a job.
func tokenFor(gh model.GitHubCreds) (string, error) {
	if gh.Token == "" {
		return "", errors.New("github integration has no token")
	}
	return gh.Token, nil
}

// TokenOwner returns the login of the account a token belongs to. It validates
// the token and fills in `owner` when the user does not name an organization.
func (g *GitHub) TokenOwner(ctx context.Context, token string) (string, error) {
	var out struct {
		Login string `json:"login"`
	}
	if err := g.api(ctx, http.MethodGet, "/user", token, nil, &out); err != nil {
		return "", err
	}
	if out.Login == "" {
		return "", errors.New("token has no user")
	}
	return out.Login, nil
}

// ---------- HTTP helper ----------

// APIError is a non-2xx GitHub response (message only; never request headers).
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string { return fmt.Sprintf("github api %d: %s", e.Status, e.Message) }

func (g *GitHub) api(ctx context.Context, method, path, bearer string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.apiURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = http.StatusText(resp.StatusCode)
		}
		if len(e.Message) > maxErrBodySize {
			e.Message = e.Message[:maxErrBodySize]
		}
		return &APIError{Status: resp.StatusCode, Message: e.Message}
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ---------- naming + template ----------

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

// ServiceName derives a Cloud Run / repo safe name ([a-z][a-z0-9-]{0,62}) from the
// project name, suffixed with the start of the project id for uniqueness.
func ServiceName(projectName, projectID string) string {
	slug := strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(projectName), "-"), "-")
	if len(slug) > 40 {
		slug = strings.Trim(slug[:40], "-")
	}
	if slug == "" || slug[0] < 'a' || slug[0] > 'z' {
		slug = "app-" + slug
		slug = strings.TrimRight(slug, "-")
	}
	id := strings.ReplaceAll(strings.ToLower(projectID), "-", "")
	if len(id) > 8 {
		id = id[:8]
	}
	return slug + "-" + id
}

// DeployData is what templates/deploy.yml.tmpl is rendered with. ProjectID is
// this platform's project id (used in the webhook); GCPProjectID is the user's
// Google Cloud project.
type DeployData struct {
	BranchName     string
	WIFProvider    string
	ServiceAccount string
	Region         string
	GCPProjectID   string
	ArtifactRepo   string
	ServiceName    string
	WebhookURL     string
	WebhookSecret  string
	ProjectID      string
}

// safeValue limits what may be interpolated into the workflow YAML/shell so a
// user-supplied value can't break out of its quotes.
var safeValue = regexp.MustCompile(`^[A-Za-z0-9._@:/=?&%+~-]+$`)

func (d DeployData) validate() error {
	for name, v := range map[string]string{
		"BranchName": d.BranchName, "WIFProvider": d.WIFProvider, "ServiceAccount": d.ServiceAccount,
		"Region": d.Region, "GCPProjectID": d.GCPProjectID, "ArtifactRepo": d.ArtifactRepo,
		"ServiceName": d.ServiceName, "WebhookURL": d.WebhookURL, "WebhookSecret": d.WebhookSecret,
		"ProjectID": d.ProjectID,
	} {
		if !safeValue.MatchString(v) {
			return fmt.Errorf("deploy workflow: invalid %s", name)
		}
	}
	return nil
}

// RenderDeployWorkflow renders the Cloud Run deploy workflow. The template uses
// [[ ]] delimiters so GitHub Actions' own ${{ }} expressions pass through untouched.
func RenderDeployWorkflow(d DeployData) ([]byte, error) {
	if err := d.validate(); err != nil {
		return nil, err
	}
	t, err := template.New("deploy").Delims("[[", "]]").Option("missingkey=error").Parse(templates.DeployWorkflow)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ServiceData is what templates/service.yml.tmpl is rendered with.
type ServiceData struct {
	ServiceName string
	// RuntimeServiceAccount, when set, is the account the app runs as (instead of
	// the project's default compute account).
	RuntimeServiceAccount string
}

// RenderServiceYAML renders the Cloud Run service spec (the repo's IaC). The
// image stays as IMAGE_PLACEHOLDER; the deploy workflow substitutes it.
func RenderServiceYAML(d ServiceData) ([]byte, error) {
	if !safeValue.MatchString(d.ServiceName) {
		return nil, errors.New("service.yml: invalid ServiceName")
	}
	if d.RuntimeServiceAccount != "" && !safeValue.MatchString(d.RuntimeServiceAccount) {
		return nil, errors.New("service.yml: invalid RuntimeServiceAccount")
	}
	t, err := template.New("service").Delims("[[", "]]").Option("missingkey=error").Parse(templates.ServiceYAML)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ---------- CreateRepo ----------

// CreateRepo creates a private repo (auto-initialised on main), commits the
// rendered deploy workflow to main and returns the repo's web URL.
func (g *GitHub) CreateRepo(ctx context.Context, gh model.GitHubCreds, gcp model.GCPCreds, projectID, projectName string) (string, error) {
	name := ServiceName(projectName, projectID)
	// With no GCP project (repo-only demo mode) there is nothing to deploy to,
	// so the repo is created without a deploy workflow.
	var workflow, serviceYAML []byte
	if gcp.ProjectID != "" {
		var err error
		if serviceYAML, err = RenderServiceYAML(ServiceData{ServiceName: name, RuntimeServiceAccount: gcp.RuntimeServiceAccount}); err != nil {
			return "", err
		}
		workflow, err = RenderDeployWorkflow(DeployData{
			BranchName: defaultBase, WIFProvider: gcp.WIFProvider, ServiceAccount: gcp.ServiceAccount,
			Region: gcp.Region, GCPProjectID: gcp.ProjectID, ArtifactRepo: gcp.ArtifactRepo,
			ServiceName: name, WebhookURL: g.webhookURL, WebhookSecret: helper.WebhookSecretFor(g.webhookSecret, projectID), ProjectID: projectID,
		})
		if err != nil {
			return "", err
		}
	}
	token, err := tokenFor(gh)
	if err != nil {
		return "", err
	}

	// a PAT creates repos under the user (/user/repos) or an organization.
	var acct struct {
		Type string `json:"type"`
	}
	if err := g.api(ctx, http.MethodGet, "/users/"+url.PathEscape(gh.Owner), token, nil, &acct); err != nil {
		return "", fmt.Errorf("lookup owner: %w", err)
	}
	createPath := "/user/repos"
	if acct.Type == "Organization" {
		createPath = "/orgs/" + url.PathEscape(gh.Owner) + "/repos"
	}
	var repo struct {
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
	}
	if err := g.api(ctx, http.MethodPost, createPath, token, map[string]any{
		"name": name, "private": true, "auto_init": true,
		"description": "Generated by AI App Builder",
	}, &repo); err != nil {
		return "", fmt.Errorf("create repo: %w", err)
	}

	if workflow == nil {
		return repo.HTMLURL, nil
	}
	// [skip ci]: creating the repo must not start a deploy of an empty repo.
	if err := g.putFile(ctx, token, gh.Owner, name, workflowPath, "ci: add Cloud Run deploy workflow [skip ci]", workflow); err != nil {
		return "", fmt.Errorf("commit workflow: %w", err)
	}
	if err := g.putFile(ctx, token, gh.Owner, name, serviceSpecPath, "ci: add Cloud Run service spec [skip ci]", serviceYAML); err != nil {
		return "", fmt.Errorf("commit service.yml: %w", err)
	}
	return repo.HTMLURL, nil
}

// putFile commits one file to the default branch through the contents API.
// auto_init commits asynchronously, so a 404/409 is retried briefly.
func (g *GitHub) putFile(ctx context.Context, token, owner, repo, path, message string, content []byte) error {
	contentPath := fmt.Sprintf("/repos/%s/%s/contents/%s", url.PathEscape(owner), url.PathEscape(repo), path)
	put := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  defaultBase,
	}
	for attempt := 0; ; attempt++ {
		err := g.api(ctx, http.MethodPut, contentPath, token, put, nil)
		var ae *APIError
		if err == nil || attempt >= 4 || !errors.As(err, &ae) || (ae.Status != 404 && ae.Status != 409) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(g.retryDelay):
		}
	}
}

// ---------- repo / PR URL parsing ----------

func parseRepoURL(raw string) (owner, name string, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid repo url")
	}
	return parts[0], strings.TrimSuffix(parts[1], ".git"), nil
}

func parsePRURL(raw string) (owner, name string, number int, err error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", "", 0, err
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return "", "", 0, errors.New("invalid pull request url")
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return "", "", 0, errors.New("invalid pull request number")
	}
	return parts[0], parts[1], n, nil
}

// repoFor validates that repoURL belongs to the connected account, so the
// token is never used against someone else's repository.
func repoFor(gh model.GitHubCreds, repoURL string) (owner, name string, err error) {
	owner, name, err = parseRepoURL(repoURL)
	if err != nil {
		return "", "", err
	}
	if !strings.EqualFold(owner, gh.Owner) {
		return "", "", errors.New("repository does not belong to the connected account")
	}
	return owner, name, nil
}

// ---------- PushAndOpenPR ----------

type gitRunner struct {
	dir    string
	env    []string
	secret string
}

func (r gitRunner) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = r.dir
	cmd.Env = append(os.Environ(), r.env...)
	out, err := cmd.CombinedOutput()
	s := string(out)
	if r.secret != "" {
		s = strings.ReplaceAll(s, r.secret, "***")
	}
	if err != nil {
		return s, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(s))
	}
	return s, nil
}

// PushAndOpenPR commits the generated workspace on feature/ai-gen on top of
// baseBranch, pushes it and opens a PR (reusing an open one if it exists).
// The token is passed through the environment, never argv or .git/config, and
// the deploy workflow on baseBranch is restored so generated code cannot alter CI.
func (g *GitHub) PushAndOpenPR(ctx context.Context, workspaceDir string, gh model.GitHubCreds, repoURL, baseBranch string) (string, error) {
	if baseBranch == "" {
		baseBranch = defaultBase
	}
	owner, name, err := repoFor(gh, repoURL)
	if err != nil {
		return "", err
	}
	token, err := tokenFor(gh)
	if err != nil {
		return "", err
	}
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + token))
	git := gitRunner{
		dir: workspaceDir, secret: basic,
		env: []string{
			"GIT_TERMINAL_PROMPT=0",
			"GIT_CONFIG_COUNT=1",
			"GIT_CONFIG_KEY_0=http.extraHeader",
			"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
		},
	}
	remote := fmt.Sprintf("%s/%s/%s.git", g.gitURL, owner, name)
	base := "origin/" + baseBranch

	steps := [][]string{
		{"init", "-q"},
		{"remote", "remove", "origin"}, // ignore failure: may not exist
		{"remote", "add", "origin", remote},
		{"fetch", "-q", "origin", baseBranch},
		{"symbolic-ref", "HEAD", "refs/heads/" + featureBranch},
		{"reset", "-q", base}, // history of base, keep working tree
	}
	for _, s := range steps {
		if _, err := git.run(ctx, s...); err != nil && s[0] != "remote" { // "remote remove" fails on a fresh repo
			return "", err
		}
	}
	// files deleted relative to base come back; generated code may not drop them
	if out, err := git.run(ctx, "ls-files", "--deleted", "-z"); err == nil && strings.Trim(out, "\x00") != "" {
		args := append([]string{"checkout", "--"}, strings.Split(strings.Trim(out, "\x00"), "\x00")...)
		if _, err := git.run(ctx, args...); err != nil {
			return "", err
		}
	}
	_ = os.RemoveAll(workspaceDir + "/.github") // never trust generated CI config
	if out, _ := git.run(ctx, "ls-tree", "--name-only", base, ".github"); strings.TrimSpace(out) != "" {
		if _, err := git.run(ctx, "checkout", base, "--", ".github"); err != nil {
			return "", err
		}
	}
	// the IaC is as trusted as the workflow: generated code may not change it
	if out, _ := git.run(ctx, "ls-tree", "--name-only", base, serviceSpecPath); strings.TrimSpace(out) != "" {
		if _, err := git.run(ctx, "checkout", base, "--", serviceSpecPath); err != nil {
			return "", err
		}
	}
	if _, err := git.run(ctx, "add", "-A"); err != nil {
		return "", err
	}
	if _, err := git.run(ctx, "diff", "--cached", "--quiet"); err == nil {
		return "", errors.New("nothing to commit: generated workspace has no changes")
	}
	if _, err := git.run(ctx,
		"-c", "user.name=AI App Builder", "-c", "user.email=ai-app-builder@users.noreply.github.com",
		"commit", "-q", "-m", "feat: AI generated application"); err != nil {
		return "", err
	}
	if _, err := git.run(ctx, "push", "-q", "--force", "origin", "HEAD:refs/heads/"+featureBranch); err != nil {
		return "", err
	}

	return g.openPR(ctx, token, owner, name, baseBranch)
}

func (g *GitHub) openPR(ctx context.Context, token, owner, name, base string) (string, error) {
	var pr struct {
		HTMLURL string `json:"html_url"`
	}
	base2 := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(name))
	err := g.api(ctx, http.MethodPost, base2, token, map[string]any{
		"title": "AI generated application",
		"head":  featureBranch,
		"base":  base,
		"body":  "Generated by AI App Builder. Review, then approve to merge and deploy.",
	}, &pr)
	if err == nil {
		return pr.HTMLURL, nil
	}
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != http.StatusUnprocessableEntity {
		return "", fmt.Errorf("open pr: %w", err)
	}
	// likely "a pull request already exists" (retry of a failed job): reuse it
	var existing []struct {
		HTMLURL string `json:"html_url"`
	}
	q := fmt.Sprintf("%s?state=open&head=%s:%s&base=%s", base2, url.QueryEscape(owner), url.QueryEscape(featureBranch), url.QueryEscape(base))
	if err2 := g.api(ctx, http.MethodGet, q, token, nil, &existing); err2 != nil || len(existing) == 0 {
		return "", fmt.Errorf("open pr: %w", err)
	}
	return existing[0].HTMLURL, nil
}

// ---------- MergePR ----------

// MergePR squash-merges the PR into its base branch, which triggers the deploy
// workflow. The PR must belong to repoURL and to the connected account.
func (g *GitHub) MergePR(ctx context.Context, gh model.GitHubCreds, repoURL, prURL string) error {
	owner, name, err := repoFor(gh, repoURL)
	if err != nil {
		return err
	}
	pOwner, pName, number, err := parsePRURL(prURL)
	if err != nil {
		return err
	}
	if !strings.EqualFold(pOwner, owner) || !strings.EqualFold(pName, name) {
		return errors.New("pull request does not belong to the repository")
	}
	token, err := tokenFor(gh)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/pulls/%d/merge", url.PathEscape(owner), url.PathEscape(name), number)
	if err := g.api(ctx, http.MethodPut, path, token, map[string]any{"merge_method": "squash"}, nil); err != nil {
		return fmt.Errorf("merge pr: %w", err)
	}
	return nil
}

// ---------- DispatchWorkflow ----------

// DispatchWorkflow starts a workflow that has a workflow_dispatch trigger.
func (g *GitHub) DispatchWorkflow(ctx context.Context, gh model.GitHubCreds, repoURL, workflowFile, ref string) error {
	owner, name, err := repoFor(gh, repoURL)
	if err != nil {
		return err
	}
	token, err := tokenFor(gh)
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/repos/%s/%s/actions/workflows/%s/dispatches", url.PathEscape(owner), url.PathEscape(name), url.PathEscape(workflowFile))
	if err := g.api(ctx, http.MethodPost, path, token, map[string]any{"ref": ref}, nil); err != nil {
		return fmt.Errorf("dispatch workflow: %w", err)
	}
	return nil
}
