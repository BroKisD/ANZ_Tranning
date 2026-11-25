package test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"anz-training/service"
)

// The tests drive the real orchestration code with a fake `claude` CLI (a shell
// script) and the real `go vet` / `go test` as the checker.

type claudeEnv struct {
	t      *testing.T
	root   string
	invDir string
	script string
	claude *service.Claude
}

func goEnvVar(t *testing.T, name string) string {
	t.Helper()
	out, err := exec.Command("go", "env", name).Output()
	if err != nil {
		t.Skipf("go toolchain unavailable: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// newClaudeEnv writes a fake claude whose body is `body` (sh). The body can use
// $n (invocation number, 1-based) and $2 (the prompt).
func newClaudeEnv(t *testing.T, body string, mods ...func(*service.ClaudeConfig)) *claudeEnv {
	t.Helper()
	e := &claudeEnv{t: t, root: t.TempDir(), invDir: t.TempDir()}
	e.script = filepath.Join(t.TempDir(), "claude")
	src := fmt.Sprintf(`#!/bin/sh
INV=%q
n=$(( $(ls "$INV" | grep -c prompt) + 1 ))
printf '%%s' "$2" > "$INV/$n.prompt"
printf '%%s\n' "$@" > "$INV/$n.args"
env > "$INV/$n.env"
pwd > "$INV/$n.cwd"
%s
`, e.invDir, body)
	if err := os.WriteFile(e.script, []byte(src), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := service.ClaudeConfig{
		Binary: e.script, WorkspaceRoot: e.root,
		GoCache: goEnvVar(t, "GOCACHE"), GoModCache: goEnvVar(t, "GOMODCACHE"),
		GenerateTimeout: 30 * time.Second, CheckTimeout: 2 * time.Minute,
	}
	for _, m := range mods {
		m(&cfg)
	}
	e.claude = service.NewClaude(cfg)
	return e
}

func (e *claudeEnv) invocations() int {
	entries, _ := os.ReadDir(e.invDir)
	n := 0
	for _, en := range entries {
		if strings.HasSuffix(en.Name(), ".prompt") {
			n++
		}
	}
	return n
}

func (e *claudeEnv) read(name string) string {
	b, err := os.ReadFile(filepath.Join(e.invDir, name))
	if err != nil {
		e.t.Fatal(err)
	}
	return string(b)
}

// Shell snippets the fake claude uses to "generate" a project. The heredoc
// delimiter is FILE_END so it cannot clash with anything.
const (
	shGoMod = "cat > go.mod <<'FILE_END'\nmodule generated\n\ngo 1.20\nFILE_END\n"

	shMain = "cat > main.go <<'FILE_END'\n" +
		"package main\n\n" +
		"import (\n\t\"net/http\"\n\t\"os\"\n)\n\n" +
		"func Add(a, b int) int { return a + b }\n\n" +
		"func main() {\n\tport := os.Getenv(\"PORT\")\n\tif port == \"\" {\n\t\tport = \"8080\"\n\t}\n" +
		"\thttp.ListenAndServe(\"0.0.0.0:\"+port, nil)\n}\n" +
		"FILE_END\n"

	shGoodTest = "cat > main_test.go <<'FILE_END'\n" +
		"package main\n\nimport \"testing\"\n\n" +
		"func TestAdd(t *testing.T) {\n\tif Add(1, 2) != 3 {\n\t\tt.Fatal(\"bad add\")\n\t}\n}\n" +
		"FILE_END\n"

	shBadTest = "cat > main_test.go <<'FILE_END'\n" +
		"package main\n\nimport \"testing\"\n\n" +
		"func TestAdd(t *testing.T) {\n\tif Add(1, 2) != 4 {\n\t\tt.Fatalf(\"Add(1,2) wrong: UNIQUE_FAILURE_MARKER\")\n\t}\n}\n" +
		"FILE_END\n"

	shDockerfile = "printf 'FROM golang:1.26\\nCMD [\"/app\"]\\n' > Dockerfile\n"
)

func req(id string) service.GenerateRequest {
	return service.GenerateRequest{ProjectID: id, Prompt: "build a todo list app"}
}

func TestClaude_SuccessFirstTry(t *testing.T) {
	e := newClaudeEnv(t, shGoMod+shMain+shGoodTest+shDockerfile)
	dir, err := e.claude.GenerateAndTest(context.Background(), req("proj-1"))
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Join(e.root, "proj-1") {
		t.Fatalf("dir = %q", dir)
	}
	if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err != nil {
		t.Fatal("workspace not kept on success")
	}
	if e.invocations() != 1 {
		t.Fatalf("claude invoked %d times, want 1", e.invocations())
	}
	args := e.read("1.args")
	if !strings.Contains(args, "-p\n") || !strings.Contains(args, "--dangerously-skip-permissions") {
		t.Fatalf("args = %q", args)
	}
	prompt := e.read("1.prompt")
	for _, want := range []string{"PORT", "Dockerfile", "build a todo list app", "<user_request>"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if cwd := strings.TrimSpace(e.read("1.cwd")); cwd != dir {
		t.Fatalf("claude ran in %q, want workspace %q", cwd, dir)
	}

	// caller cleans up after the PR is pushed
	if err := e.claude.Cleanup("proj-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("workspace not removed by Cleanup")
	}
}

func TestClaude_SelfCorrectionLoop(t *testing.T) {
	// 1st run produces a failing test; the fix run (prompt has <check_output>) repairs it
	body := shGoMod + shMain + shDockerfile +
		"case \"$2\" in\n  *\"<check_output>\"*)\n" + shGoodTest + "  ;;\n  *)\n" + shBadTest + "  ;;\nesac\n"
	e := newClaudeEnv(t, body)
	dir, err := e.claude.GenerateAndTest(context.Background(), req("proj-2"))
	if err != nil {
		t.Fatal(err)
	}
	if e.invocations() != 2 {
		t.Fatalf("claude invoked %d times, want 2 (generate + 1 fix)", e.invocations())
	}
	fix := e.read("2.prompt")
	if !strings.Contains(fix, "UNIQUE_FAILURE_MARKER") || !strings.Contains(fix, "build a todo list app") {
		t.Fatalf("fix prompt must carry the test output and the original request:\n%s", fix)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}

func TestClaude_GivesUpAfterMaxAttempts(t *testing.T) {
	e := newClaudeEnv(t, shGoMod+shMain+shDockerfile+shBadTest)
	dir, err := e.claude.GenerateAndTest(context.Background(), req("proj-3"))
	var ge *service.GenerationError
	if !errors.As(err, &ge) {
		t.Fatalf("want GenerationError, got %v", err)
	}
	if ge.Attempts != 3 || !strings.Contains(ge.Output, "UNIQUE_FAILURE_MARKER") {
		t.Fatalf("unexpected error: %+v", ge)
	}
	if e.invocations() != 3 { // 1 generate + 2 fixes; the 3rd test run has nothing left to try
		t.Fatalf("claude invoked %d times, want 3", e.invocations())
	}
	if dir != "" {
		t.Fatalf("dir should be empty on failure, got %q", dir)
	}
	if _, err := os.Stat(filepath.Join(e.root, "proj-3")); !os.IsNotExist(err) {
		t.Fatal("workspace not cleaned up after failure")
	}
}

func TestClaude_MissingDockerfileIsFedBack(t *testing.T) {
	body := shGoMod + shMain + shGoodTest +
		"case \"$2\" in\n  *\"<check_output>\"*)\n" + shDockerfile + "  ;;\nesac\n"
	e := newClaudeEnv(t, body)
	if _, err := e.claude.GenerateAndTest(context.Background(), req("proj-4")); err != nil {
		t.Fatal(err)
	}
	if e.invocations() != 2 || !strings.Contains(e.read("2.prompt"), "Dockerfile is missing") {
		t.Fatalf("missing Dockerfile not reported back (invocations=%d)", e.invocations())
	}
}

func TestClaude_UnknownStackIsFedBack(t *testing.T) {
	e := newClaudeEnv(t, shDockerfile+"echo hi > notes.txt\n")
	_, err := e.claude.GenerateAndTest(context.Background(), req("proj-5"))
	var ge *service.GenerationError
	if !errors.As(err, &ge) || !strings.Contains(ge.Output, "No go.mod or package.json") {
		t.Fatalf("got %v", err)
	}
}

func TestClaude_SandboxEnvAndBYOK(t *testing.T) {
	t.Setenv("ENCRYPTION_MASTER_KEY", "SERVER_MASTER_KEY_VALUE")
	t.Setenv("DB_PASSWORD", "SERVER_DB_PASSWORD")
	t.Setenv("GITHUB_TOKEN", "SERVER_GH_KEY")
	t.Setenv("ANTHROPIC_API_KEY", "ambient-key")

	// project whose test fails and prints the API key it sees
	leak := "cat > main_test.go <<'FILE_END'\n" +
		"package main\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\n" +
		"func TestAdd(t *testing.T) { t.Fatalf(\"key=%s\", os.Getenv(\"ANTHROPIC_API_KEY\")) }\n" +
		"FILE_END\n"
	e := newClaudeEnv(t, shGoMod+shMain+shDockerfile+leak, func(c *service.ClaudeConfig) { c.APIKey = "platform-key" })
	r := req("proj-6")
	r.AnthropicAPIKey = "sk-ant-BYOK-SECRET"
	_, err := e.claude.GenerateAndTest(context.Background(), r)
	var ge *service.GenerationError
	if !errors.As(err, &ge) {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(ge.Output, "sk-ant-BYOK-SECRET") || !strings.Contains(ge.Output, "key=***") {
		t.Fatalf("API key not redacted from check output: %s", ge.Output)
	}
	if strings.Contains(e.read("2.prompt"), "sk-ant-BYOK-SECRET") {
		t.Fatal("API key leaked into the fix prompt")
	}

	env := e.read("1.env")
	for _, secret := range []string{"SERVER_MASTER_KEY_VALUE", "SERVER_DB_PASSWORD", "SERVER_GH_KEY", "ambient-key", "platform-key"} {
		if strings.Contains(env, secret) {
			t.Errorf("server secret %q reached the claude process", secret)
		}
	}
	if !strings.Contains(env, "ANTHROPIC_API_KEY=sk-ant-BYOK-SECRET") {
		t.Error("BYOK key not provided to claude")
	}
	if !strings.Contains(env, "HOME="+filepath.Join(e.root, ".home-proj-6")) {
		t.Errorf("HOME is not the per-project dir:\n%s", env)
	}
	if _, err := os.Stat(filepath.Join(e.root, ".home-proj-6")); !os.IsNotExist(err) {
		t.Error("private HOME not cleaned up")
	}
}

func TestClaude_PlatformKeyUsedWithoutBYOK(t *testing.T) {
	e := newClaudeEnv(t, shGoMod+shMain+shGoodTest+shDockerfile, func(c *service.ClaudeConfig) { c.APIKey = "platform-key" })
	if _, err := e.claude.GenerateAndTest(context.Background(), req("proj-7")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(e.read("1.env"), "ANTHROPIC_API_KEY=platform-key") {
		t.Fatal("platform key not used")
	}
}

func TestClaude_TimeoutKillsProcessGroup(t *testing.T) {
	e := newClaudeEnv(t, "sleep 30\n", func(c *service.ClaudeConfig) { c.GenerateTimeout = 300 * time.Millisecond })
	start := time.Now()
	_, err := e.claude.GenerateAndTest(context.Background(), req("proj-8"))
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout error, got %v", err)
	}
	if time.Since(start) > 4*time.Second {
		t.Fatalf("timeout took %s: child process not killed", time.Since(start))
	}
	if _, err := os.Stat(filepath.Join(e.root, "proj-8")); !os.IsNotExist(err) {
		t.Fatal("workspace not cleaned up after timeout")
	}
}

func TestClaude_CLIFailureAndCancel(t *testing.T) {
	e := newClaudeEnv(t, "echo boom >&2\nexit 3\n")
	if _, err := e.claude.GenerateAndTest(context.Background(), req("proj-9")); err == nil {
		t.Fatal("expected error when claude exits non-zero")
	}
	if e.invocations() != 1 {
		t.Fatal("a failing claude run must not be retried")
	}

	e2 := newClaudeEnv(t, "sleep 30\n")
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(200 * time.Millisecond); cancel() }()
	start := time.Now()
	if _, err := e2.claude.GenerateAndTest(ctx, req("proj-10")); err == nil {
		t.Fatal("expected error on cancel")
	}
	if time.Since(start) > 4*time.Second {
		t.Fatal("cancel did not stop claude")
	}
}

func TestClaude_InputValidation(t *testing.T) {
	e := newClaudeEnv(t, "exit 0\n")
	for _, id := range []string{"", "../etc", "a/b", "-x", ".hidden", strings.Repeat("a", 65), "a b"} {
		_, err := e.claude.GenerateAndTest(context.Background(), service.GenerateRequest{ProjectID: id, Prompt: "x"})
		if !errors.Is(err, service.ErrInvalidProjectID) {
			t.Errorf("id %q: got %v", id, err)
		}
		if err := e.claude.Cleanup(id); !errors.Is(err, service.ErrInvalidProjectID) {
			t.Errorf("Cleanup(%q): got %v", id, err)
		}
	}
	for _, p := range []string{"", "   ", strings.Repeat("x", 10001)} {
		_, err := e.claude.GenerateAndTest(context.Background(), service.GenerateRequest{ProjectID: "ok", Prompt: p})
		if !errors.Is(err, service.ErrInvalidPrompt) {
			t.Errorf("prompt len %d: got %v", len(p), err)
		}
	}
	if e.invocations() != 0 {
		t.Fatal("claude must not run for invalid input")
	}
}

func TestClaude_RerunStartsFromCleanWorkspace(t *testing.T) {
	e := newClaudeEnv(t, shGoMod+shMain+shGoodTest+shDockerfile)
	stale := filepath.Join(e.root, "proj-11", "stale.txt")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := e.claude.GenerateAndTest(context.Background(), req("proj-11")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale files from a previous run survived")
	}
}
