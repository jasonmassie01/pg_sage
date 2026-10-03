// Package catalogread runs pg_sage's read-only analysis statements on a
// monitored database with server-side bounds: each statement gets its own
// READ ONLY transaction with LOCAL statement and lock timeouts (from
// safety.query_timeout_ms / lock_timeout_ms), no parallel workers and no
// JIT. A pool-wide statement_timeout is deliberately not used: the same
// pool runs schema bootstrap, snapshot writes and executor DDL, which
// need longer and set their own limits.
package catalogread

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/config"
)

// Timeouts bound one statement. Zero means no limit.
type Timeouts struct {
	Statement time.Duration
	Lock      time.Duration
}

// Default is the shipped safety configuration's bounds.
func Default() Timeouts {
	return FromSafety(config.SafetyConfig{QueryTimeoutMs: config.DefaultQueryTimeoutMs})
}

// FromSafety maps the safety configuration to statement bounds.
func FromSafety(s config.SafetyConfig) Timeouts {
	return Timeouts{
		Statement: time.Duration(s.QueryTimeoutMs) * time.Millisecond,
		Lock:      time.Duration(s.LockTimeout()) * time.Millisecond,
	}
}

// Beginner is a pool or a single connection.
type Beginner interface {
	BeginTx(context.Context, pgx.TxOptions) (pgx.Tx, error)
}

// Querier is what a read-only analysis path needs; a Reader and a
// *pgxpool.Pool both satisfy it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Reader runs each statement in its own bounded read-only transaction.
type Reader struct {
	DB       Beginner
	Timeouts Timeouts
	// After, when set, runs inside the transaction after the statement and
	// before the rollback (tests: lock counts, settings).
	After func(ctx context.Context, tx pgx.Tx, sql string, args []any)
}

// New returns a Reader over db.
func New(db Beginner, t Timeouts) Reader { return Reader{DB: db, Timeouts: t} }

var errNoDatabase = errors.New("catalogread: no database connection")

// Begin opens one bounded read-only transaction on db.
func Begin(ctx context.Context, db Beginner, t Timeouts) (pgx.Tx, error) {
	if db == nil {
		return nil, errNoDatabase
	}
	tx, err := db.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin bounded read: %w", err)
	}
	_, err = tx.Exec(ctx, `SELECT
		set_config('statement_timeout', $1, true),
		set_config('lock_timeout', $2, true),
		set_config('max_parallel_workers_per_gather', '0', true),
		set_config('jit', 'off', true)`,
		fmt.Sprintf("%dms", t.Statement.Milliseconds()),
		fmt.Sprintf("%dms", t.Lock.Milliseconds()))
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, fmt.Errorf("set bounded read limits: %w", err)
	}
	return tx, nil
}

// Query runs sql; closing the rows ends the transaction.
func (r Reader) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	tx, err := r.begin(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		_ = tx.Rollback(context.Background())
		return nil, err
	}
	return &txRows{Rows: rows, tx: tx, after: r.bind(ctx, sql, args)}, nil
}

// QueryRow runs sql; Scan ends the transaction.
func (r Reader) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	tx, err := r.begin(ctx)
	if err != nil {
		return txRow{err: err}
	}
	return txRow{row: tx.QueryRow(ctx, sql, args...), tx: tx, after: r.bind(ctx, sql, args)}
}

func (r Reader) begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := Begin(ctx, r.DB, r.Timeouts)
	if err != nil {
		return nil, err
	}
	if before := beforeStatement(ctx); before != nil {
		if err := before(ctx, tx); err != nil {
			_ = tx.Rollback(context.Background())
			return nil, err
		}
	}
	return tx, nil
}

func (r Reader) bind(ctx context.Context, sql string, args []any) func(pgx.Tx) {
	if r.After == nil {
		return nil
	}
	return func(tx pgx.Tx) { r.After(ctx, tx, sql, args) }
}

type txRows struct {
	pgx.Rows
	tx     pgx.Tx
	closed bool
	after  func(pgx.Tx)
}

func (r *txRows) Next() bool {
	if r.Rows.Next() {
		return true
	}
	r.Close()
	return false
}

func (r *txRows) Close() {
	if r.closed {
		return
	}
	r.closed = true
	r.Rows.Close()
	if r.after != nil {
		r.after(r.tx)
	}
	_ = r.tx.Rollback(context.Background())
}

type txRow struct {
	row   pgx.Row
	tx    pgx.Tx
	err   error
	after func(pgx.Tx)
}

func (r txRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer func() { _ = r.tx.Rollback(context.Background()) }()
	err := r.row.Scan(dest...)
	if r.after != nil {
		r.after(r.tx)
	}
	return err
}

type beforeKey struct{}

// WithBeforeStatement returns a context whose bounded reads first run fn
// inside their transaction, under its limits (fault injection in tests:
// a slow read). Production code never sets it.
func WithBeforeStatement(
	ctx context.Context, fn func(context.Context, pgx.Tx) error,
) context.Context {
	return context.WithValue(ctx, beforeKey{}, fn)
}

func beforeStatement(ctx context.Context) func(context.Context, pgx.Tx) error {
	fn, _ := ctx.Value(beforeKey{}).(func(context.Context, pgx.Tx) error)
	return fn
}
