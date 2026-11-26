package database_test

import (
	"testing"

	"anz-training/database"
)

func TestConnectDatabase_Unreachable(t *testing.T) {
	t.Setenv("INSTANCE_CONNECTION_NAME", "")
	t.Setenv("DB_HOST", "127.0.0.1")
	t.Setenv("DB_PORT", "1") // nothing listens here
	t.Setenv("DB_USER", "u")
	t.Setenv("DB_NAME", "d")

	db, err := database.ConnectDatabase()
	if err == nil || db != nil {
		t.Fatalf("expected a connection error, got db=%v err=%v", db, err)
	}
	if database.DB != nil {
		t.Fatal("global DB must stay unset after a failed connect")
	}
}
