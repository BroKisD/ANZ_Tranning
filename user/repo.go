package user

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Querier is satisfied by both *sql.DB and *sql.Tx, so repos work
// inside or outside a transaction with no changes.
type Querier interface {
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
}

var (
	_ Querier = (*sql.DB)(nil)
	_ Querier = (*sql.Tx)(nil)
)

type Repo struct{}

func (Repo) FindByEmail(ctx context.Context, q Querier, email string) (*User, error) {
	const query = `SELECT id, email, name, created_at FROM users WHERE email = ?`
	var u User
	err := q.QueryRowContext(ctx, query, email).
		Scan(&u.ID, &u.Email, &u.Name, &u.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("find user by email: %w", err)
	}
	return &u, nil
}

func (Repo) Insert(ctx context.Context, q Querier, u *User) error {
	const query = `INSERT INTO users (email, name) VALUES (?, ?)`
	res, err := q.ExecContext(ctx, query, u.Email, u.Name)
	if err != nil {
		return fmt.Errorf("insert user: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("last insert id: %w", err)
	}
	u.ID = id
	return nil
}
