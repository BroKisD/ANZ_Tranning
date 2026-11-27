package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"anz-training/crypto"
	"anz-training/database"
	"anz-training/handler"
	"anz-training/helper"
	"anz-training/repository"
	"anz-training/service"

	"go.temporal.io/sdk/client"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: %s {serve|migrate up|migrate status|migrate down-to <v>}", os.Args[0])
	}

	if os.Args[1] == "serve" && noDatabaseConfigured() {
		runStandalone()
		return
	}

	db, err := database.ConnectDatabase()
	if err != nil {
		log.Fatalf("db connect failed: %v", err)
		panic(err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		log.Fatalf("get sql.DB: %v", err)
		panic(err)
	}
	defer sqlDB.Close()

	switch os.Args[1] {
	case "serve":
		serve()
	case "migrate":
		runMigrate(os.Args[2:])
	default:
		log.Fatalf("unknown command %q", os.Args[1])
	}
}

func serve() {
	if err := database.MigrateUp(); err != nil {
		log.Fatalf("migrate up failed: %v", err)
		panic(err)
	}

	svc := service.NewUserService(repository.NewStore(database.DB))
	h := handler.NewUserHandler(svc)

	secretKey := os.Getenv("API_KEY")
	if secretKey == "" {
		log.Fatal("API_KEY required")
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", helper.APIKeyMiddleware(secretKey, http.HandlerFunc(h.Create)).ServeHTTP)
	mux.HandleFunc("GET /users", helper.APIKeyMiddleware(secretKey, http.HandlerFunc(h.Get)).ServeHTTP)

	masterKey, err := crypto.LoadMasterKey(os.Getenv("ENCRYPTION_MASTER_KEY"))
	if err != nil {
		log.Fatal(err)
	}
	store := repository.NewStore(database.DB)

	var secrets handler.SecretStore       // BYOK writes (integration handler)
	var secretReader service.SecretReader // BYOK reads (orchestrator); stays a nil interface when disabled
	if project := os.Getenv("SECRET_MANAGER_PROJECT"); project != "" {
		sm, err := service.NewSecretManager(context.Background(), project)
		if err != nil {
			log.Fatalf("secret manager: %v", err)
		}
		defer sm.Close()
		secrets, secretReader = sm, sm
	}

	// Modes (first match wins):
	//   REPO_ONLY=true                         stop after the GitHub repo is created
	//   DEMO_TEMPLATE_REPO + GCP_PROJECT_ID    deploy: demo flow + real Cloud Run deployment
	//   DEMO_TEMPLATE_REPO                     demo: template clone, mock tests, PR, merge; no deploy
	//   (none)                                 Claude generation + the user's own GCP integration
	repoOnly := os.Getenv("REPO_ONLY") == "true"
	templateRepo := os.Getenv("DEMO_TEMPLATE_REPO")
	deploy := !repoOnly && templateRepo != "" && os.Getenv("GCP_PROJECT_ID") != ""
	demo := !repoOnly && templateRepo != "" && !deploy
	noUserGCP := repoOnly || demo || deploy
	switch {
	case repoOnly:
		log.Print("REPO_ONLY: projects stop after the GitHub repo is created")
	case deploy:
		log.Printf("DEPLOY: generated code is a clone of %s, tests are mocked, apps are deployed to Cloud Run", templateRepo)
	case demo:
		log.Printf("DEMO: generated code is a clone of %s, tests are mocked, nothing is deployed", templateRepo)
	}
	webhookOn := !repoOnly && !demo // deploy workflows report back through the webhook
	// Deploy mode with neither WEBHOOK_URL nor WEBHOOK_SECRET: no webhook; the platform
	// follows each deploy run through the GitHub API instead (it only needs to reach GitHub).
	pollDeploys := deploy && os.Getenv("WEBHOOK_URL") == "" && os.Getenv("WEBHOOK_SECRET") == ""
	if pollDeploys {
		webhookOn = false
		log.Print("DEPLOY: no webhook configured; deploy runs are followed through the GitHub API")
	}
	if webhookOn && (os.Getenv("WEBHOOK_SECRET") == "" || os.Getenv("WEBHOOK_URL") == "") {
		log.Fatal("WEBHOOK_URL and WEBHOOK_SECRET are required unless REPO_ONLY=true or only DEMO_TEMPLATE_REPO is set (deploy mode may leave both unset)")
	}
	// every GitHub call uses the user's own PAT (stored encrypted per user)
	gh := service.NewGitHub(service.GitHubConfig{
		WebhookURL:    os.Getenv("WEBHOOK_URL"),
		WebhookSecret: os.Getenv("WEBHOOK_SECRET"),
	})
	var deployer service.Deployer
	var deployTimeout time.Duration
	if deploy {
		if static, on, err := service.StaticCredsFromEnv(os.Getenv); err != nil {
			log.Fatalf("deploy mode: %v", err)
		} else if on {
			// fixed, already configured Workload Identity provider + service account
			sd, err := service.NewStaticDeployer(static, gh)
			if err != nil {
				log.Fatalf("deploy mode: %v", err)
			}
			if deployTimeout, err = service.DeployTimeoutFromEnv(os.Getenv); err != nil {
				log.Fatalf("deploy mode: %v", err)
			}
			deployer = sd
			log.Printf("DEPLOY: using the configured Workload Identity provider and service account %s", static.ServiceAccount)
		} else {
			// the platform creates the GCP setup itself, one trust binding per repo
			deployCfg, err := service.DeployConfigFromEnv(os.Getenv)
			if err != nil {
				log.Fatalf("deploy mode: %v", err)
			}
			g, err := service.NewGCP(context.Background(), deployCfg, nil)
			if err != nil {
				log.Fatalf("deploy mode: %v", err)
			}
			deployer, deployTimeout = service.NewCloudRunDeployer(g, gh), deployCfg.Timeout
		}
	}

	api := func(h http.HandlerFunc) http.HandlerFunc {
		return helper.APIKeyMiddleware(secretKey, h).ServeHTTP
	}
	ih := handler.NewIntegrationHandler(store, secrets, masterKey, handler.HeaderUserID, gh.TokenOwner)
	mux.HandleFunc("POST /api/integrations/github", api(ih.ConnectGitHub))
	mux.HandleFunc("POST /api/integrations", api(ih.UpsertIntegration))
	mux.HandleFunc("GET /api/integrations", api(ih.ListIntegrations))

	var gen service.CodeGenerator = service.NewClaude(service.ClaudeConfig{APIKey: os.Getenv("ANTHROPIC_API_KEY")})
	if demo || deploy {
		dg, err := service.NewDemoGenerator(service.DemoGeneratorConfig{TemplateURL: templateRepo})
		if err != nil {
			log.Fatalf("DEMO_TEMPLATE_REPO: %v", err)
		}
		gen = dg
	}
	// Temporal runs every project's workflow; this process hosts the worker.
	tc, err := client.Dial(client.Options{HostPort: temporalAddress()})
	if err != nil {
		log.Fatalf("temporal: %v", err)
	}
	defer tc.Close()
	acts := &service.Activities{
		Store: store, Integrations: store, GitHub: gh, Gen: gen,
		Secrets: secretReader, Deployer: deployer, MasterKey: masterKey,
	}
	if pollDeploys {
		acts.Monitor = service.NewDeployMonitor(store, gh, service.DeployMonitorConfig{Timeout: deployTimeout})
	}
	stopWorkers, err := service.StartWorkers(tc, acts, 0)
	if err != nil {
		log.Fatalf("temporal worker: %v", err)
	}
	runner := service.NewTemporalRunner(tc, store, service.WorkflowConfig{
		RepoOnly: repoOnly, Demo: demo, Deploy: deploy, Polling: pollDeploys, DeployTimeout: deployTimeout,
	}, deployer)
	ph := handler.NewProjectHandler(store, runner, handler.HeaderUserID)
	ph.NoGCP = noUserGCP
	mux.HandleFunc("POST /api/projects", api(ph.Create))
	mux.HandleFunc("GET /api/projects/{id}", api(ph.Get))
	mux.HandleFunc("POST /api/projects/{id}/approve", api(ph.Approve))
	mux.HandleFunc("POST /api/projects/{id}/deploy", api(ph.Deploy))

	if webhookOn {
		// called by GitHub Actions, so no x-api-key: it authenticates with its own per-project secret
		wh, err := handler.NewWebhookHandler(store, os.Getenv("WEBHOOK_SECRET"))
		if err != nil {
			log.Fatal(err)
		}
		wh.Signaler = runner
		mux.HandleFunc("POST /api/webhooks/github", wh.GitHubWebhook)
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	srv := &http.Server{Addr: ":" + port, Handler: mux, ReadHeaderTimeout: 10 * time.Second}

	stop, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	cleaned := make(chan struct{})
	go func() {
		defer close(cleaned)
		<-stop.Done()
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		_ = srv.Shutdown(ctx)
		stopWorkers() // drains in-flight activities; unfinished runs resume on the next start
	}()
	log.Printf("listening on %s", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	<-cleaned // ErrServerClosed means shutdown started; wait for orchestrator cleanup
}

func temporalAddress() string {
	if a := os.Getenv("TEMPORAL_ADDRESS"); a != "" {
		return a
	}
	return "localhost:7233"
}

func runMigrate(args []string) {
	if len(args) < 1 {
		log.Fatal("usage: migrate {up|status|down-to <v>}")
	}
	var err error
	switch args[0] {
	case "up":
		err = database.MigrateUp()
	case "status":
		err = database.MigrateStatus()
	case "down-to":
		if len(args) < 2 {
			log.Fatal("usage: migrate down-to <version>")
		}
		var v int64
		v, err = strconv.ParseInt(args[1], 10, 64)
		if err == nil {
			err = database.MigrateDownTo(v)
		}
	default:
		log.Fatalf("unknown migrate command %q", args[0])
	}
	if err != nil {
		log.Fatal(err)
	}
}
