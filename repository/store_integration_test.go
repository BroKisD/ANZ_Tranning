//go:build integration
// +build integration

package repository_test

import (
	"anz-training/internal/testutil"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

	c, err := mysql.Run(ctx, "mysql:8.0",
		mysql.WithDatabase("testdb"), mysql.WithUsername("test"), mysql.WithPassword("test"),
		mysql.WithScripts(testutil.SchemaPath()),
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

// TestStore_Projects_WithMySQL checks defaults, NULL-able columns and the
// compare-and-set used to start a run exactly once.
func TestStore_Projects_WithMySQL(t *testing.T) {
	testcontainers.SkipIfProviderIsNotHealthy(t)
	ctx := context.Background()

	c, err := mysql.Run(ctx, "mysql:8.0",
		mysql.WithDatabase("testdb"), mysql.WithUsername("test"), mysql.WithPassword("test"),
		mysql.WithScripts(testutil.SchemaPath()),
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

	if _, err := store.GetProject(ctx, "nope"); err != model.ErrNotFound {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
	p := &model.Project{ID: "p1", OwnerID: "u1", Name: "App", Prompt: "todo"}
	if err := store.CreateProject(ctx, p); err != nil {
		t.Fatal(err)
	}
	got, err := store.GetProject(ctx, "p1")
	if err != nil || got.Status != model.StatusPending || got.CurrentStep != model.StepInitializing || got.BranchName != "main" {
		t.Fatalf("defaults not applied: %+v %v", got, err)
	}

	// exactly one of several concurrent starters wins the pending -> generating CAS
	var wins int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := store.UpdateProjectIf(ctx, "p1", model.StatusPending, map[string]any{"status": model.StatusGenerating})
			if err != nil {
				t.Error(err)
			}
			if ok {
				atomic.AddInt32(&wins, 1)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d goroutines won the CAS, want 1", wins)
	}

	if err := store.UpdateProject(ctx, "p1", map[string]any{"repo_url": "https://github.com/a/b", "pr_url": "https://github.com/a/b/pull/1", "current_step": model.StepRepoCreated}); err != nil {
		t.Fatal(err)
	}
	got, _ = store.GetProject(ctx, "p1")
	if got.RepoURL != "https://github.com/a/b" || got.PRURL != "https://github.com/a/b/pull/1" || got.CurrentStep != model.StepRepoCreated || got.Status != model.StatusGenerating {
		t.Fatalf("update did not persist: %+v", got)
	}

	// deploy tracking: a progress event (current_step only) applies while deploying and
	// refreshes updated_at; the watchdog query finds only quiet deploying projects
	if err := store.UpdateProject(ctx, "p1", map[string]any{"status": model.StatusDeploying, "current_step": model.StepMerging}); err != nil {
		t.Fatal(err)
	}
	if ok, err := store.UpdateProjectIf(ctx, "p1", model.StatusDeploying, map[string]any{"current_step": model.StepBuilding}); err != nil || !ok {
		t.Fatalf("progress update: ok=%v err=%v", ok, err)
	}
	if got, _ := store.ListStaleDeploying(ctx, time.Now().Add(-time.Hour)); len(got) != 0 {
		t.Fatalf("fresh deploy listed as stale: %+v", got)
	}
	if got, _ := store.ListStaleDeploying(ctx, time.Now().Add(time.Hour)); len(got) != 1 || got[0].ID != "p1" {
		t.Fatalf("quiet deploy not listed: %+v", got)
	}
}
