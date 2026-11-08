package lakebaseobs

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	storeOperationTimeout = 5 * time.Second
	storeStatementTimeout = 2 * time.Second
	storeRollbackTimeout  = 3 * time.Second
	purgeBatchSize        = 500
)

// ErrRetentionPending means erasure needs another scheduled pass. Already
// committed batches remain erased; unread expired payloads remain inaccessible.
var ErrRetentionPending = errors.New("lakebase telemetry retention pending")

// beginStoreOperation bounds backend work separately from the caller's whole
// operation budget, which also covers acquiring a shared pool connection.
// Local settings never affect schema migrations, auth, or the next borrower.
func beginStoreOperation(ctx context.Context, pool *pgxpool.Pool, readOnly bool) (pgx.Tx, error) {
	opts := pgx.TxOptions{}
	if readOnly {
		opts.AccessMode = pgx.ReadOnly
	}
	tx, err := pool.BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(ctx, `SELECT
 pg_catalog.set_config('statement_timeout',$1,true),
 pg_catalog.set_config('lock_timeout',$1,true),
 pg_catalog.set_config('max_parallel_workers_per_gather','0',true),
 pg_catalog.set_config('jit','off',true)`, storeStatementTimeout.String())
	if err != nil {
		rollbackStoreOperation(tx)
		return nil, err
	}
	return tx, nil
}

func rollbackStoreOperation(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), storeRollbackTimeout)
	defer cancel()
	_ = tx.Rollback(ctx)
}
