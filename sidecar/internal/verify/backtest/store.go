package backtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/verify"
)

// sage.verifier_backtest (ddlVerifierBacktest) holds one row per
// completed back-test in the database's own sage schema, beside
// sage.action_log and sage.action_outcome; database_id is the history
// store's meta-db record id in meta mode (NULL otherwise). A row is
// written only for a whole run: a failed or stopped run stores nothing.

// DB is a pool or connection the store runs on.
type DB interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

const insertSQL = `/* pg_sage */ INSERT INTO sage.verifier_backtest (database_id, ran_at,
	window_from, window_to, verifier, placebos, excluded, false_positives, fp_rate,
	fp_wilson_upper, inconclusive_share, injected, detected, seed, report)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15)
RETURNING id`

// Save stores r and returns its row id. databaseID is the history store's
// meta-db record id (0 in monitored mode, stored as NULL).
func Save(ctx context.Context, db DB, databaseID int, r Result) (int64, error) {
	report, err := json.Marshal(r)
	if err != nil {
		return 0, fmt.Errorf("encode the back-test report: %w", err)
	}
	var dbID *int
	if databaseID > 0 {
		dbID = &databaseID
	}
	c := r.V2
	var id int64
	err = db.QueryRow(ctx, insertSQL, dbID, r.RanAt, r.From, r.To, r.Verifier, c.Placebos,
		c.Excluded, c.FalsePositives, c.FPRate(), c.FPWilsonUpper(), c.InconclusiveShare(),
		c.Injected, c.Detected, r.Seed, report).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store the verifier back-test of %q: %w", r.Database, err)
	}
	return id, nil
}

// latestSQL is the newest row. The gate reads its columns; the report
// carries the rest (v1's counts, the database name).
const latestSQL = `/* pg_sage */ SELECT ran_at, verifier, placebos, excluded,
	false_positives, injected, detected, report
FROM sage.verifier_backtest ORDER BY ran_at DESC, id DESC LIMIT 1`

// Latest is the newest stored back-test; ok is false when none is.
func Latest(ctx context.Context, db DB) (Result, bool, error) {
	var r Result
	var report []byte
	var c Counts
	err := db.QueryRow(ctx, latestSQL).Scan(&r.RanAt, &r.Verifier, &c.Placebos,
		&c.Excluded, &c.FalsePositives, &c.Injected, &c.Detected, &report)
	if errors.Is(err, pgx.ErrNoRows) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, fmt.Errorf("read the latest verifier back-test: %w", err)
	}
	ranAt, verifier := r.RanAt, r.Verifier
	if err := json.Unmarshal(report, &r); err != nil {
		return Result{}, false, fmt.Errorf("decode the latest verifier back-test: %w", err)
	}
	c.Inconclusive = r.V2.Inconclusive
	r.RanAt, r.Verifier, r.V2 = ranAt.UTC(), verifier, c
	return r, true, nil
}

// Reader is lab-01c's verify.BacktestReader over one database's rows.
type Reader struct{ db DB }

// NewReader reads the back-tests stored in pool's database.
func NewReader(pool *pgxpool.Pool) *Reader { return &Reader{db: pool} }

// LatestBacktest is the newest row through Gate.
func (r *Reader) LatestBacktest(ctx context.Context, _ string) (verify.Backtest, bool,
	error) {
	if r == nil || r.db == nil {
		return verify.Backtest{}, false, errors.New("verifier back-test reader has no pool")
	}
	res, ok, err := Latest(ctx, r.db)
	if err != nil || !ok {
		return verify.Backtest{}, ok, err
	}
	return res.Backtest(), true, nil
}

// lockSQL takes the back-test's session advisory lock of one database: a
// CLI run and the weekly run never replay the same database at once.
const (
	lockKeys  = `(hashtext('pg_sage.verifier_backtest'), hashtext($1))`
	lockSQL   = `/* pg_sage */ SELECT pg_try_advisory_lock` + lockKeys
	unlockSQL = `/* pg_sage */ SELECT pg_advisory_unlock` + lockKeys
)

// Lock takes the back-test lock of database on a connection of pool; a
// held lock is ErrAlreadyRunning. unlock releases it and the connection.
func Lock(ctx context.Context, pool *pgxpool.Pool, database string) (func(), error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("take the back-test lock of %q: %w", database, err)
	}
	var got bool
	if err := conn.QueryRow(ctx, lockSQL, database).Scan(&got); err != nil {
		conn.Release()
		return nil, fmt.Errorf("take the back-test lock of %q: %w", database, err)
	}
	if !got {
		conn.Release()
		return nil, fmt.Errorf("%w on database %q", ErrAlreadyRunning, database)
	}
	return func() {
		var released bool
		if err := conn.QueryRow(context.WithoutCancel(ctx), unlockSQL, database).
			Scan(&released); err != nil || !released {
			// The lock dies with its session: close the connection.
			_ = conn.Conn().Close(context.WithoutCancel(ctx))
		}
		conn.Release()
	}, nil
}

// RunAndSave back-tests h under database's lock in pool and stores the
// result in pool's database. Nothing is stored when the run fails.
func RunAndSave(ctx context.Context, pool *pgxpool.Pool, h History, cfg Config) (Result,
	error) {
	unlock, err := Lock(ctx, pool, cfg.Database)
	if err != nil {
		return Result{}, err
	}
	defer unlock()
	r, err := Run(ctx, h, cfg)
	if err != nil {
		return Result{}, err
	}
	if _, err := Save(ctx, pool, histstore.Resolve(pool).DatabaseID(), r); err != nil {
		return Result{}, err
	}
	return r, nil
}
