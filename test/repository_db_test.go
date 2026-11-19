//go:build integration
// +build integration

package test

import (
	"context"
	"path/filepath"
	"runtime"
	"testing"

	"anz-training/model"
	"anz-training/repository"

	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
)

// TestStore_Integrations_WithMySQL checks the (user_id, provider) upsert and
// the cascade-on-delete against a real MySQL instance.
func TestStore_Integrations_WithMySQL(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()
	_, thisFile, _, _ := runtime.Caller(0)

	c, err := mysql.Run(ctx, "mysql:8.0",
		mysql.WithDatabase("testdb"), mysql.WithUsername("test"), mysql.WithPassword("test"),
		mysql.WithScripts(filepath.Join(filepath.Dir(thisFile), "testdata", "schema.sql")),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Terminate(ctx) })

	dsn, _ := c.ConnectionString(ctx, "parseTime=true")
	db, err := gorm.Open(gormmysql.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	store := repository.NewStore(db)

	if err := store.InsertUser(ctx, &model.User{ID: "u1", Email: "a@b.com", Name: "A"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetIntegration(ctx, "u1", "gcp"); err != model.ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}

	for i, enc := range []string{"first", "second"} {
		if err := store.UpsertIntegration(ctx, &model.UserIntegration{
			ID: "id-" + string(rune('a'+i)), UserID: "u1", Provider: "gcp", EncryptedCredentials: enc,
		}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := store.GetIntegration(ctx, "u1", "gcp")
	if err != nil || got.EncryptedCredentials != "second" {
		t.Fatalf("upsert did not replace: %+v %v", got, err)
	}
	list, _ := store.ListIntegrations(ctx, "u1")
	if len(list) != 1 {
		t.Fatalf("want 1 row, got %d", len(list))
	}

	// FK: integration for an unknown user is rejected
	if err := store.UpsertIntegration(ctx, &model.UserIntegration{ID: "x", UserID: "ghost", Provider: "gcp", EncryptedCredentials: "e"}); err == nil {
		t.Fatal("expected FK violation")
	}
}
