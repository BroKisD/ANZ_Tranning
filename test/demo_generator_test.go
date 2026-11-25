package test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
