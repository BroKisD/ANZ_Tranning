package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"strconv"

	"anz-training/crypto"
	"anz-training/database"
	"anz-training/handler"
	"anz-training/helper"
	"anz-training/repository"
	"anz-training/service"
)

func main() {
	if len(os.Args) < 2 {
		log.Fatalf("usage: %s {serve|migrate up|migrate status|migrate down-to <v>}", os.Args[0])
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
	var secrets handler.SecretStore
	if project := os.Getenv("SECRET_MANAGER_PROJECT"); project != "" {
		sm, err := service.NewSecretManager(context.Background(), project)
		if err != nil {
			log.Fatalf("secret manager: %v", err)
		}
		defer sm.Close()
		secrets = sm
	}
	ih := handler.NewIntegrationHandler(repository.NewStore(database.DB), secrets, masterKey, handler.HeaderUserID, nil)
	mux.HandleFunc("GET /api/integrations/github/callback", helper.APIKeyMiddleware(secretKey, http.HandlerFunc(ih.GitHubCallback)).ServeHTTP)
	mux.HandleFunc("POST /api/integrations", helper.APIKeyMiddleware(secretKey, http.HandlerFunc(ih.UpsertIntegration)).ServeHTTP)
	mux.HandleFunc("GET /api/integrations", helper.APIKeyMiddleware(secretKey, http.HandlerFunc(ih.ListIntegrations)).ServeHTTP)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("listening on %s", addr)
	log.Fatal(http.ListenAndServe(addr, mux))
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
