package optimizer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/sanitize"
)

// hypopgStatementTimeout bounds each statement of a what-if session. The
// catalog-read default (500 ms) is too short to plan in.
const hypopgStatementTimeout = "5s"

// maxHypoPGLockWait caps a what-if session's lock timeout below its
// statement timeout. The safety lock timeout (30 s by default) bounds DDL;
// left uncapped, the 5 s statement timeout would end every lock wait first
// (57014, counted as one unplannable statement), so each of up to 25
// statements would queue 5 s behind an ACCESS EXCLUSIVE lock in turn.
const maxHypoPGLockWait = time.Second

// sqlstateLockNotAvailable is lock_timeout's SQLSTATE.
const sqlstateLockNotAvailable = "55P03"

type hypopgSession struct {
	conn      *pgxpool.Conn
	tx        pgx.Tx
	namespace string
	version   int
}

// openHypoPGSession acquires one connection and opens the bounded
// transaction every what-if statement runs in.
func openHypoPGSession(ctx context.Context, pool *pgxpool.Pool, lockTimeout time.Duration,
) (*hypopgSession, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire HypoPG session: %w", err)
	}
	s := &hypopgSession{conn: conn}
	err = conn.QueryRow(ctx, `SELECT n.nspname, current_setting('server_version_num')::int
		FROM pg_catalog.pg_extension e
		JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='hypopg'`,
	).Scan(&s.namespace, &s.version)
	if err != nil {
		conn.Release()
		return nil, fmt.Errorf("discover HypoPG extension namespace: %w", err)
	}
	s.tx, err = conn.Begin(ctx)
	if err == nil {
		err = boundSession(ctx, s.tx, lockTimeout)
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("begin bounded HypoPG transaction: %w", err), s.close())
	}
	return s, nil
}

// boundSession sets the transaction's LOCAL bounds: the statement timeout
// and the lock timeout, so an EXPLAIN queued behind an ACCESS EXCLUSIVE
// lock fails with 55P03 instead of holding the connection. The lock
// timeout is the configured one, at most maxHypoPGLockWait; one below 1 ms
// (unset) takes the cap: a what-if never waits unbounded.
func boundSession(ctx context.Context, tx pgx.Tx, lockTimeout time.Duration) error {
	if lockTimeout < time.Millisecond || lockTimeout > maxHypoPGLockWait {
		lockTimeout = maxHypoPGLockWait
	}
	_, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true),
		set_config('lock_timeout', $2, true)`, hypopgStatementTimeout,
		fmt.Sprintf("%dms", lockTimeout.Milliseconds()))
	return err
}

func (s *hypopgSession) function(name string) string {
	return pgx.Identifier{s.namespace, name}.Sanitize()
}

func (s *hypopgSession) evaluate(ctx context.Context, ddl string, alongside []string,
	queries []QueryInfo,
) (WhatIfResult, error) {
	// In-flight indexes are part of the baseline: the candidate is judged
	// on what it adds to them.
	for _, d := range alongside {
		if _, err := s.createIndex(ctx, d); err != nil {
			return WhatIfResult{}, fmt.Errorf("in-flight index %q: %w", d, err)
		}
	}
	before, failed, err := s.measureCosts(ctx, queries)
	if err != nil || len(before) == 0 {
		return WhatIfResult{Failed: failed}, err
	}
	oid, err := s.createIndex(ctx, ddl)
	if err != nil {
		return WhatIfResult{}, err
	}
	after, _, err := s.measureCosts(ctx, queries)
	if err != nil {
		return WhatIfResult{}, err
	}
	res := WhatIfResult{Failed: failed}
	res.Improvement, res.Measured = weightedImprovement(queries, before, after)
	for id := range before {
		if _, ok := after[id]; !ok {
			res.Failed++ // planned without the index but not with it
		}
	}
	if res.Measured == 0 {
		return res, nil
	}
	res.SizeBytes, err = s.estimateSize(ctx, oid)
	if err != nil {
		return WhatIfResult{}, err
	}
	return res, nil
}

func (s *hypopgSession) createIndex(ctx context.Context, ddl string) (uint32, error) {
	var oid uint32
	err := s.tx.QueryRow(ctx, "SELECT indexrelid FROM "+
		s.function("hypopg_create_index")+"($1)", ddl).Scan(&oid)
	if err != nil {
		return 0, fmt.Errorf("create hypothetical index: %w", err)
	}
	return oid, nil
}

func (s *hypopgSession) estimateSize(ctx context.Context, oid uint32) (int64, error) {
	var size int64
	err := s.tx.QueryRow(ctx, "SELECT "+s.function("hypopg_relation_size")+"($1)", oid).Scan(&size)
	if err != nil {
		return 0, fmt.Errorf("measure hypothetical index size: %w", err)
	}
	return size, nil
}

// measureCosts plans every explainable workload query. Each EXPLAIN runs
// in its own savepoint, so a query that fails (a dropped column, a type
// it cannot plan) is counted and skipped instead of aborting the shared
// transaction. A cancelled context or a lock timeout (the table is locked:
// every other statement on it would wait as long) aborts the measurement.
func (s *hypopgSession) measureCosts(ctx context.Context,
	queries []QueryInfo,
) (map[int64]float64, int, error) {
	costs := make(map[int64]float64)
	failed := 0
	for _, query := range queries {
		if !isExplainable(query.Text) {
			continue
		}
		if err := sanitize.RejectMultiStatement(query.Text); err != nil {
			failed++
			continue
		}
		plan, err := s.explainIsolated(ctx, query.Text)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, failed, ctxErr
		}
		if err != nil {
			if errors.Is(err, errSessionBroken) {
				return nil, failed, err
			}
			if isLockTimeout(err) {
				return nil, failed, fmt.Errorf("lock timeout: queryid %d waited past "+
					"lock_timeout for a lock on its tables: %w", query.QueryID, err)
			}
			failed++
			continue
		}
		if cost := extractTotalCost(plan); cost > 0 {
			costs[query.QueryID] = cost
		}
	}
	return costs, failed, nil
}

// isLockTimeout reports a statement cancelled by lock_timeout.
func isLockTimeout(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == sqlstateLockNotAvailable
}

// errSessionBroken means a failed EXPLAIN could not be rolled back to its
// savepoint, so the session cannot measure anything else.
var errSessionBroken = errors.New("hypothetical session unusable")

func (s *hypopgSession) explainIsolated(ctx context.Context, query string) ([]byte, error) {
	if _, err := s.tx.Exec(ctx, "SAVEPOINT sage_hypo_query"); err != nil {
		return nil, fmt.Errorf("%w: savepoint: %w", errSessionBroken, err)
	}
	plan, err := s.explain(ctx, query)
	if err != nil {
		if _, rbErr := s.tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sage_hypo_query"); rbErr != nil {
			return nil, fmt.Errorf("%w: %w (rollback: %w)", errSessionBroken, err, rbErr)
		}
		return nil, fmt.Errorf("explain hypothetical workload query: %w", err)
	}
	if _, err := s.tx.Exec(ctx, "RELEASE SAVEPOINT sage_hypo_query"); err != nil {
		return nil, fmt.Errorf("%w: release savepoint: %w", errSessionBroken, err)
	}
	return plan, nil
}

func (s *hypopgSession) explain(ctx context.Context, query string) ([]byte, error) {
	if s.version < 160000 && strings.Contains(query, "$1") {
		return s.explainPrepared(ctx, query)
	}
	prefix := "EXPLAIN (FORMAT JSON) "
	if s.version >= 160000 {
		prefix = "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "
	}
	// Keep normalized $n parameters unbound and all HypoPG planner hooks on the owning session.
	return singleJSONPlan(s.tx.Conn().PgConn().Exec(ctx, prefix+query).ReadAll())
}

func singleJSONPlan(results []*pgconn.Result, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	if len(results) != 1 || len(results[0].Rows) != 1 || len(results[0].Rows[0]) != 1 {
		return nil, fmt.Errorf("hypothetical EXPLAIN returned no single JSON plan")
	}
	return results[0].Rows[0][0], nil
}

func (s *hypopgSession) close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var cleanupErr error
	if s.tx != nil {
		if err := s.tx.Rollback(ctx); err != nil && !errors.Is(err, pgx.ErrTxClosed) {
			cleanupErr = fmt.Errorf("rollback HypoPG transaction: %w", err)
		}
	}
	// HypoPG indexes are session state, so transaction rollback alone is insufficient.
	if _, err := s.conn.Exec(ctx, "SELECT "+s.function("hypopg_reset")+"()"); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("reset hypothetical indexes: %w", err))
	}
	if cleanupErr != nil {
		raw := s.conn.Hijack()
		return errors.Join(cleanupErr, raw.Close(ctx))
	}
	s.conn.Release()
	return nil
}
