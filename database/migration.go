package database

import (
	"embed"

	_ "github.com/go-sql-driver/mysql"
	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func MigrateUp() error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}
	return goose.Up(DB, "migrations")
}

func MigrateStatus() error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}
	return goose.Status(DB, "migrations")
}

func MigrateDownTo(v int64) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}
	return goose.DownTo(DB, "migrations", v)
}
