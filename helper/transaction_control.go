package helper

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type txKey struct{}

func InTx[T any](
	ctx context.Context,
	db *sql.DB,
	fn func(ctx context.Context) (T, error),
) (T, error) {
	var zero T

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return zero, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	ctx = context.WithValue(ctx, txKey{}, tx)

	v, err := fn(ctx)
	if err != nil {
		if rbErr := tx.Rollback(); rbErr != nil && !errors.Is(rbErr, sql.ErrTxDone) {
			return zero, errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return zero, err
	}

	if err := tx.Commit(); err != nil {
		return zero, fmt.Errorf("commit: %w", err)
	}
	return v, nil
}
