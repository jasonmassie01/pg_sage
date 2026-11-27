package catalogread

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrNoDB is a snapshot asked of no pool or connection.
var ErrNoDB = errors.New("catalogread: no database connection")

// BeginSnapshot opens one READ ONLY REPEATABLE READ transaction, so a run
// of several checks sees one catalog snapshot, with JIT off and
// statement_timeout set LOCAL to timeout (it must be positive). tag
// prefixes the setup statement, so pg_sage's own reads are recognised.
// The caller rolls the transaction back.
func BeginSnapshot(ctx context.Context, db Beginner, tag string,
	timeout time.Duration) (pgx.Tx, error) {
	if db == nil {
		return nil, ErrNoDB
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("catalogread: snapshot statement timeout %s must be positive",
			timeout)
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin read-only transaction: %w", err)
	}
	if _, err := tx.Exec(ctx, tag+`SELECT pg_catalog.set_config('jit', 'off', true),
		pg_catalog.set_config('statement_timeout', $1, true)`,
		strconv.FormatInt(timeout.Milliseconds(), 10)); err != nil {
		_ = tx.Rollback(context.Background()) // the setup error is the one to report
		return nil, fmt.Errorf("set JIT and statement timeout: %w", err)
	}
	return tx, nil
}

// InSavepoint runs fn under a savepoint of tx, so a failing statement
// aborts only fn's work and the transaction stays usable for the next
// check. fn's error is returned as it is; savepoint errors are wrapped.
func InSavepoint(ctx context.Context, tx pgx.Tx, fn func(pgx.Tx) error) error {
	sp, err := tx.Begin(ctx)
	if err != nil {
		return fmt.Errorf("savepoint: %w", err)
	}
	if err := fn(sp); err != nil {
		_ = sp.Rollback(ctx) // fn's error is the one to report
		return err
	}
	if err := sp.Commit(ctx); err != nil {
		return fmt.Errorf("release savepoint: %w", err)
	}
	return nil
}
