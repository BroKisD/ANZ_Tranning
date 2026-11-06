package database

import (
	"context"
	"database/sql"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
	_ "github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
)

var DB *sql.DB

func ConnectDatabase() error {

	err := godotenv.Load()
	if err != nil {
		return err
	}

	cfg := mysql.NewConfig()
	cfg.User = os.Getenv("DB_USER")
	cfg.Passwd = os.Getenv("DB_PASSWORD")
	cfg.Net = "tcp"
	cfg.Addr = os.Getenv("DB_HOST") + ":" + os.Getenv("DB_PORT")
	cfg.DBName = os.Getenv("DB_NAME")
	cfg.ParseTime = true
	cfg.MultiStatements = true
	cfg.Loc, _ = time.LoadLocation("UTC")

	DB, err = sql.Open("mysql", cfg.FormatDSN())
	if err != nil {
		return err
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := DB.PingContext(ctx); err != nil {
		DB.Close()
		return err
	}

	return nil
}
