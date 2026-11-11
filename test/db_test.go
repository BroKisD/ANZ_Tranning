//go:build integration
// +build integration

package test

import (
	"anz-training/user"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestUsersAPI_WithMySQL exercises the HTTP handlers against a real, isolated
// MySQL instance. Docker must be available to run this integration test.
func TestUsersAPI_WithMySQL(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)

	ctx := context.Background()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("find test fixture path")
	}

	mysqlC, err := mysql.Run(ctx,
		"mysql:8.0",
		mysql.WithDatabase("testdb"),
		mysql.WithUsername("test"),
		mysql.WithPassword("test"),
		mysql.WithScripts(filepath.Join(filepath.Dir(thisFile), "testdata", "schema.sql")),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mysqlC.Terminate(ctx) })

	dsn, err := mysqlC.ConnectionString(ctx, "parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	h := user.NewHandler(user.NewService(db))
	mux := http.NewServeMux()
	mux.HandleFunc("POST /users", h.Create)
	mux.HandleFunc("GET /users", h.Get)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	created := requestJSON(t, server, http.MethodPost, "/users", map[string]string{
		"email": "  ADA@EXAMPLE.COM ",
		"name":  " Ada Lovelace ",
	}, http.StatusCreated)
	if created.Email != "ada@example.com" || created.Name != "Ada Lovelace" || created.ID == 0 {
		t.Fatalf("unexpected created user: %#v", created)
	}

	fetched := requestJSON(t, server, http.MethodGet, "/users?email=ada@example.com", nil, http.StatusOK)
	if fetched.ID != created.ID || fetched.Email != created.Email || fetched.Name != created.Name {
		t.Fatalf("GET /users returned %#v; want %#v", fetched, created)
	}

	requestJSON(t, server, http.MethodPost, "/users", map[string]string{
		"email": "ada@example.com",
		"name":  "Another Ada",
	}, http.StatusConflict)
}

func requestJSON(t *testing.T, server *httptest.Server, method, path string, body any, wantStatus int) user.User {
	t.Helper()

	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(data)
	}

	req, err := http.NewRequest(method, server.URL+path, payload)
	if err != nil {
		t.Fatal(err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s: got status %d, want %d", method, path, resp.StatusCode, wantStatus)
	}

	var result user.User
	if wantStatus < http.StatusBadRequest {
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatal(err)
		}
	}
	return result
}
