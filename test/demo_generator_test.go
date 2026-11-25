package test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"anz-training/service"
)

func TestDemoGenerator(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	// local template repo
	tpl := t.TempDir()
	gitCmd(t, tpl, "init", "-q", "--initial-branch=main")
	write(t, filepath.Join(tpl, "main.go"), "package main\n")
	write(t, filepath.Join(tpl, "Dockerfile"), "FROM scratch\n")
	gitCmd(t, tpl, "add", "-A")
	gitCmd(t, tpl, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")

	root := t.TempDir()
	g, err := service.NewDemoGenerator(service.DemoGeneratorConfig{
		TemplateURL: "file://" + tpl, WorkspaceRoot: root, TestDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	dir, err := g.GenerateAndTest(context.Background(), service.GenerateRequest{ProjectID: "proj-1", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("template not cloned: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		t.Fatal("template git history must be removed")
	}
	if err := g.Cleanup("proj-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Fatal("workspace not removed")
	}

	// a bad template fails and leaves no workspace behind
	bad, _ := service.NewDemoGenerator(service.DemoGeneratorConfig{TemplateURL: "file://" + tpl + "-missing", WorkspaceRoot: root})
	if _, err := bad.GenerateAndTest(context.Background(), service.GenerateRequest{ProjectID: "proj-2"}); err == nil {
		t.Fatal("missing template must fail")
	}
	if _, err := os.Stat(filepath.Join(root, "proj-2")); err == nil {
		t.Fatal("failed run left a workspace")
	}

	// guards
	if _, err := service.NewDemoGenerator(service.DemoGeneratorConfig{TemplateURL: "ext::sh -c evil"}); !errors.Is(err, service.ErrInvalidTemplateURL) {
		t.Fatalf("unsafe url accepted: %v", err)
	}
	if _, err := g.GenerateAndTest(context.Background(), service.GenerateRequest{ProjectID: "../x"}); !errors.Is(err, service.ErrInvalidProjectID) {
		t.Fatalf("bad project id: %v", err)
	}
}

// A repo created without a deploy workflow (no GCP) has no .github on main.
func TestGitHub_PushAndOpenPR_NoWorkflowOnBase(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	f := newFakeGitHub(t)
	root := t.TempDir()
	bare := filepath.Join(root, "acme", "repo.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	gitCmd(t, bare, "init", "-q", "--bare", "--initial-branch=main")
	seed := t.TempDir()
	gitCmd(t, seed, "init", "-q", "--initial-branch=main")
	write(t, filepath.Join(seed, "README.md"), "# repo\n")
	gitCmd(t, seed, "add", "-A")
	gitCmd(t, seed, "-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "-m", "init")
	gitCmd(t, seed, "push", "-q", bare, "main")

	ws := t.TempDir()
	write(t, filepath.Join(ws, "main.go"), "package main\n")
	write(t, filepath.Join(ws, ".github/workflows/evil.yml"), "EVIL\n") // generated CI is never trusted
	g := newGH(t, f, "file://"+root)
	if _, err := g.PushAndOpenPR(context.Background(), ws, ghCreds, "https://github.com/acme/repo", "main"); err != nil {
		t.Fatal(err)
	}
	if out := gitCmd(t, bare, "ls-tree", "-r", "--name-only", "feature/ai-gen"); !strings.Contains(out, "main.go") || strings.Contains(out, "evil.yml") {
		t.Fatalf("pushed tree:\n%s", out)
	}
}
