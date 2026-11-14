package helper

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Settle is called from a shared-server test: it is reported.
func Settle(pool *pgxpool.Pool) {
	_, _ = pool.Exec(context.Background(), "CHECKPOINT") // want
}

// Checkpoint is called only from a test that holds the exclusive server.
func Checkpoint(pool *pgxpool.Pool) {
	_, _ = pool.Exec(context.Background(), "CHECKPOINT")
}
