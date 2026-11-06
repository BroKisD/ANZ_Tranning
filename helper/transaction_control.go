package helper

import (
	"context"

	"gorm.io/gorm"
)

type txKey struct{}

func InTx[T any](
	ctx context.Context,
	db *gorm.DB,
	fn func(ctx context.Context) (T, error),
) (T, error) {
	var zero T
	var result T

	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		txCtx := context.WithValue(ctx, txKey{}, tx)

		v, err := fn(txCtx)
		if err != nil {
			return err
		}
		result = v
		return nil
	})

	if err != nil {
		return zero, err
	}
	return result, nil
}
