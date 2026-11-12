package database

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/joho/godotenv"
	gormmysql "gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

func ConnectDatabase() (*gorm.DB, error) {
	_ = godotenv.Load()

	cfg := mysql.NewConfig()
	cfg.User = os.Getenv("DB_USER")
	cfg.Passwd = os.Getenv("DB_PASSWORD")
	cfg.DBName = os.Getenv("DB_NAME")
	cfg.ParseTime = true
	cfg.MultiStatements = true
	cfg.Loc, _ = time.LoadLocation("UTC")
	cfg.AllowCleartextPasswords = true

	instanceConnName := os.Getenv("INSTANCE_CONNECTION_NAME")
	if instanceConnName != "" {
		cfg.Net = "unix"
		cfg.Addr = fmt.Sprintf("/cloudsql/%s", instanceConnName)
	} else {
		cfg.Net = "tcp"
		host := os.Getenv("DB_HOST")
		port := os.Getenv("DB_PORT")
		if host == "" {
			host = "127.0.0.1"
		}
		if port == "" {
			port = "3306"
		}
		cfg.Addr = fmt.Sprintf("%s:%s", host, port)
	}

	db, err := gorm.Open(gormmysql.Open(cfg.FormatDSN()), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, err
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, err
	}

	sqlDB.SetMaxOpenConns(25)
	sqlDB.SetMaxIdleConns(25)
	sqlDB.SetConnMaxLifetime(5 * time.Minute)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, err
	}

	DB = db
	return db, nil
}
