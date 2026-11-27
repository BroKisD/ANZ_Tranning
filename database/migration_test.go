package database

import (
	"io/fs"
	"testing"
)

func TestMigrations_AreEmbedded(t *testing.T) {
	files, err := fs.Glob(migrationsFS, "migrations/*.sql")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) < 3 {
		t.Fatalf("expected at least 3 embedded migrations, got %v", files)
	}
}
