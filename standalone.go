package main

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/joho/godotenv"
)

// noDatabaseConfigured is true when no database settings exist at all. This is
// the case when the platform repo is itself used as the demo template and gets
// deployed to Cloud Run without the platform's secrets.
func noDatabaseConfigured() bool {
	_ = godotenv.Load() // same .env the normal path reads; never overrides real env vars
	return os.Getenv("DB_USER") == "" && os.Getenv("DB_HOST") == "" && os.Getenv("INSTANCE_CONNECTION_NAME") == ""
}

// runStandalone serves a small status page so the container starts and passes
// Cloud Run's health check without a database.
func runStandalone() {
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "ok") })
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><title>Hello from AI App Builder</title>
<body style="font-family:sans-serif;max-width:40rem;margin:4rem auto">
<h1>Hello from AI App Builder</h1>
<p>This code was cloned into a new GitHub repo, built by GitHub Actions and deployed to Cloud Run.</p>
<p><small>Running in standalone mode: no database is configured.</small></p>
</body>`)
	})
	log.Printf("no database configured: standalone mode, listening on :%s", port)
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.ListenAndServe())
}
