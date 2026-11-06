package database

import (
	"database/sql"
	"embed"
	"fmt"

	"github.com/pressly/goose/v3"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func sqlDB() (*sql.DB, error) {
	return DB.DB()
}

func MigrateUp() error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}

	db, err := sqlDB()
	if err != nil {
		return fmt.Errorf("get sql.DB for migration: %w", err)
	}
	return goose.Up(db, "migrations")
}

func MigrateStatus() error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}

	db, err := sqlDB()
	if err != nil {
		return fmt.Errorf("get sql.DB for migration: %w", err)
	}
	return goose.Status(db, "migrations")
}

func MigrateDownTo(v int64) error {
	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect("mysql"); err != nil {
		return err
	}

	db, err := sqlDB()
	if err != nil {
		return fmt.Errorf("get sql.DB for migration: %w", err)
	}
	return goose.DownTo(db, "migrations", v)
}
