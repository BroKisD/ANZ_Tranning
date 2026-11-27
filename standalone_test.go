package main

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The platform repo is also the demo template: deployed on its own (no database
// configured) it must still start and answer, or Cloud Run rejects the revision.
func TestMain_StandaloneWithoutDatabase(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not installed")
	}
	root, _ := filepath.Abs(".")
	bin := filepath.Join(t.TempDir(), "app")
	build := exec.Command("go", "build", "-o", bin, ".")
	build.Dir = root
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	cmd := exec.Command(bin, "serve")
	cmd.Dir = t.TempDir() // no .env here
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), fmt.Sprintf("PORT=%d", port)}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	get := func(path string) (int, string) {
		var lastErr error
		for i := 0; i < 50; i++ {
			resp, err := http.Get(fmt.Sprintf("http://127.0.0.1:%d%s", port, path))
			if err == nil {
				defer resp.Body.Close()
				b, _ := io.ReadAll(resp.Body)
				return resp.StatusCode, string(b)
			}
			lastErr = err
			time.Sleep(100 * time.Millisecond)
		}
		t.Fatalf("server did not start without a database: %v", lastErr)
		return 0, ""
	}
	if code, body := get("/"); code != 200 || !strings.Contains(body, "Hello") {
		t.Fatalf("GET /: %d %q", code, body)
	}
	if code, _ := get("/healthz"); code != 200 {
		t.Fatalf("GET /healthz: %d", code)
	}
}

func TestNoDatabaseConfigured(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		want bool
	}{
		{"nothing set", nil, true},
		{"user set", map[string]string{"DB_USER": "u"}, false},
		{"host set", map[string]string{"DB_HOST": "h"}, false},
		{"cloud sql set", map[string]string{"INSTANCE_CONNECTION_NAME": "p:r:i"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(t.TempDir()) // so a developer's .env is not loaded
			for _, k := range []string{"DB_USER", "DB_HOST", "INSTANCE_CONNECTION_NAME"} {
				t.Setenv(k, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			if got := noDatabaseConfigured(); got != tt.want {
				t.Fatalf("noDatabaseConfigured() = %v, want %v", got, tt.want)
			}
		})
	}
}
