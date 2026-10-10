package perfgate

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Settle checkpoints the server after the fixture is built: its load and
// VACUUM dirtied gigabytes, and a checkpoint flushing them during the
// measured phases would charge pg_sage's reads for the fixture's writes.
// A long-running deployment's database is settled; the gate's should be.
func Settle(ctx context.Context, pool *pgxpool.Pool) error {
	if pool == nil {
		return errors.New("perfgate: settle: no connection pool")
	}
	if _, err := pool.Exec(ctx, "CHECKPOINT"); err != nil {
		return fmt.Errorf("perfgate: settle: checkpoint: %w", err)
	}
	return nil
}
