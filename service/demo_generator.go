package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

var ErrInvalidTemplateURL = errors.New("template repo url must start with https:// or file://")

// DemoGeneratorConfig configures the stand-in for Claude Code.
type DemoGeneratorConfig struct {
	TemplateURL   string        // repo cloned as the "generated" app (https:// or file://)
	WorkspaceRoot string        // default /tmp/workspaces
	TestDelay     time.Duration // how long the mock test run takes (default 3s)
}

// DemoGenerator is a CodeGenerator for demos: instead of asking Claude to write
// the app it clones a fixed template repo, then pretends to run the tests.
type DemoGenerator struct{ cfg DemoGeneratorConfig }

func NewDemoGenerator(cfg DemoGeneratorConfig) (*DemoGenerator, error) {
	if !strings.HasPrefix(cfg.TemplateURL, "https://") && !strings.HasPrefix(cfg.TemplateURL, "file://") {
		return nil, ErrInvalidTemplateURL
	}
	if cfg.WorkspaceRoot == "" {
		cfg.WorkspaceRoot = defaultWorkspaceRoot
	}
	if cfg.TestDelay <= 0 {
		cfg.TestDelay = 3 * time.Second
	}
	return &DemoGenerator{cfg: cfg}, nil
}

func (d *DemoGenerator) workspaceDir(projectID string) string {
	return filepath.Join(d.cfg.WorkspaceRoot, projectID)
}

// Cleanup removes the project's workspace.
func (d *DemoGenerator) Cleanup(projectID string) error {
	if !projectIDRe.MatchString(projectID) {
		return ErrInvalidProjectID
	}
	return os.RemoveAll(d.workspaceDir(projectID))
}

// GenerateAndTest clones the template into a fresh workspace (without its git
// history, so PushAndOpenPR can re-init it on top of the new repo), then runs a
// mock test. On any failure the workspace is removed.
func (d *DemoGenerator) GenerateAndTest(ctx context.Context, req GenerateRequest) (dir string, err error) {
	if !projectIDRe.MatchString(req.ProjectID) {
		return "", ErrInvalidProjectID
	}
	if err := d.Cleanup(req.ProjectID); err != nil {
		return "", err
	}
	dir = d.workspaceDir(req.ProjectID)
	if err := os.MkdirAll(d.cfg.WorkspaceRoot, 0o700); err != nil {
		return "", err
	}
	defer func() {
		if err != nil {
			_ = d.Cleanup(req.ProjectID)
		}
	}()

	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--quiet", "--", d.cfg.TemplateURL, dir)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=https:file")
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("clone template: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if err := os.RemoveAll(filepath.Join(dir, ".git")); err != nil {
		return "", err
	}

	// mock test run
	log.Printf("demo generator: project %s: running tests (mock)", req.ProjectID)
	select {
	case <-time.After(d.cfg.TestDelay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	log.Printf("demo generator: project %s: tests passed (mock)", req.ProjectID)
	return dir, nil
}
