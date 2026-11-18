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

	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/planhash"
	"github.com/pg-sage/sidecar/internal/sanitize"
	"github.com/pg-sage/sidecar/internal/workload"
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
	// canHide: the extension has hypopg_hide_index and
	// hypopg_unhide_all_indexes (HypoPG 1.4+), found in pg_proc because
	// providers ship different HypoPG versions.
	canHide bool
	// discard: the backend's hidden-index state is in doubt, so close
	// closes the connection instead of pooling it.
	discard bool
}

// hypopgSessionSQL finds the extension's schema, the server version and
// whether the extension can hide existing indexes.
const hypopgSessionSQL = `SELECT n.nspname, current_setting('server_version_num')::int,
	(SELECT count(DISTINCT p.proname) = 2 FROM pg_catalog.pg_proc p
	  WHERE p.pronamespace = e.extnamespace
	    AND p.proname IN ('hypopg_hide_index', 'hypopg_unhide_all_indexes'))
	FROM pg_catalog.pg_extension e
	JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace WHERE e.extname='hypopg'`

// openHypoPGSession acquires one connection and opens the bounded
// transaction every what-if statement runs in.
func openHypoPGSession(ctx context.Context, pool *pgxpool.Pool, lockTimeout time.Duration,
) (*hypopgSession, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire HypoPG session: %w", err)
	}
	s := &hypopgSession{conn: conn}
	err = conn.QueryRow(ctx, hypopgSessionSQL).Scan(&s.namespace, &s.version, &s.canHide)
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
// (unset) takes the cap: a what-if never waits unbounded. The workload
// and the candidate DDL resolve on the application's search_path
// (apppath.Pin), never on pg_sage's session path.
func boundSession(ctx context.Context, tx pgx.Tx, lockTimeout time.Duration) error {
	if lockTimeout < time.Millisecond || lockTimeout > maxHypoPGLockWait {
		lockTimeout = maxHypoPGLockWait
	}
	_, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true),
		set_config('lock_timeout', $2, true)`, hypopgStatementTimeout,
		fmt.Sprintf("%dms", lockTimeout.Milliseconds()))
	if err != nil {
		return err
	}
	return apppath.Pin(ctx, tx)
}

func (s *hypopgSession) function(name string) string {
	return pgx.Identifier{s.namespace, name}.Sanitize()
}

// evaluate plans the workload without and with rec's index. In-flight
// indexes are part of the baseline: the candidate is judged on what it adds
// to them. A replacement's old index is hidden once the candidate exists,
// so the improvement is judged on the swapped plan and each statement that
// used the old index is guarded against a regression; without hide support
// the old index stays present and the result says so.
func (s *hypopgSession) evaluate(ctx context.Context, rec Recommendation,
	queries []QueryInfo,
) (WhatIfResult, error) {
	for _, d := range rec.Alongside {
		if _, err := s.createIndex(ctx, d); err != nil {
			return WhatIfResult{}, fmt.Errorf("in-flight index %q: %w", d, err)
		}
	}
	old, err := s.indexToHide(ctx, rec.Replaces)
	if err != nil {
		return WhatIfResult{}, err
	}
	before, err := s.measureCosts(ctx, queries, old.name)
	if err != nil || len(before.costs) == 0 {
		return WhatIfResult{Failed: len(before.unplanned),
			UnplannedQueryIDs: before.unplanned}, err
	}
	after, oid, err := s.measureWithCandidate(ctx, rec.DDL, old, queries)
	if err != nil {
		return WhatIfResult{}, err
	}
	res := comparedCosts(queries, before.costs, after.costs, before.unplanned)
	res.OldIndexPresent = rec.Replaces != nil && !s.canHide
	if old.name != "" {
		res.HiddenIndex = old.name
		res.Guarded, res.Regressions = guardedRegressions(queries, before, after.costs)
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

// measureWithCandidate creates the candidate, hides old (if any) and plans
// the workload again. It returns the candidate's OID for its size.
func (s *hypopgSession) measureWithCandidate(ctx context.Context, ddl string,
	old hiddenIndex, queries []QueryInfo,
) (measurement, uint32, error) {
	oid, err := s.createIndex(ctx, ddl)
	if err != nil {
		return measurement{}, 0, err
	}
	if old.name != "" {
		if err := s.hide(ctx, old.oid); err != nil {
			return measurement{}, 0, err
		}
	}
	after, err := s.measureCosts(ctx, queries, "")
	return after, oid, err
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

// comparedCosts is the evaluation of the costs planned without (before)
// and with (after) the index: the improvement over the statements planned
// both times, and as unplanned those that failed before (unplanned) and
// those planned only without the index. Any unplanned statement keeps the
// verdict unverified (whatIfVerdict).
func comparedCosts(queries []QueryInfo, before, after map[int64]float64,
	unplanned []int64,
) WhatIfResult {
	res := WhatIfResult{UnplannedQueryIDs: append(unplanned,
		lostPlans(queries, before, after)...)}
	res.Failed = len(res.UnplannedQueryIDs)
	res.Improvement, res.Measured = weightedImprovement(queries, before, after)
	return res
}

// lostPlans are the queryids planned without the index but not with it,
// in workload order.
func lostPlans(queries []QueryInfo, before, after map[int64]float64) []int64 {
	var lost []int64
	seen := make(map[int64]bool, len(queries))
	for _, q := range queries {
		_, planned := before[q.QueryID]
		_, replanned := after[q.QueryID]
		if planned && !replanned && !seen[q.QueryID] {
			lost = append(lost, q.QueryID)
		}
		seen[q.QueryID] = true
	}
	return lost
}

// measureCosts plans every explainable workload query and, when guard
// names an index, records the statements whose plan uses it. Each EXPLAIN runs
// in its own savepoint, so a query that fails (a dropped column, a type
// it cannot plan) is named in the unplanned queryids and skipped instead
// of aborting the shared transaction. A cancelled context or a lock
// timeout (the table is locked: every other statement on it would wait
// as long) aborts the measurement.
func (s *hypopgSession) measureCosts(ctx context.Context, queries []QueryInfo,
	guard string,
) (measurement, error) {
	m := measurement{costs: make(map[int64]float64), uses: make(map[int64]bool)}
	for _, query := range queries {
		if ok, readOnly := workload.Plannable(query.Text); !ok || !readOnly {
			continue
		}
		if err := sanitize.RejectMultiStatement(query.Text); err != nil {
			m.unplanned = append(m.unplanned, query.QueryID)
			continue
		}
		plan, err := s.explainIsolated(ctx, query.Text)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return measurement{unplanned: m.unplanned}, ctxErr
		}
		if err != nil {
			if errors.Is(err, errSessionBroken) {
				return measurement{unplanned: m.unplanned}, err
			}
			if isLockTimeout(err) {
				return measurement{unplanned: m.unplanned}, fmt.Errorf("lock timeout: "+
					"queryid %d waited past lock_timeout for a lock on its tables: %w",
					query.QueryID, err)
			}
			m.unplanned = append(m.unplanned, query.QueryID)
			continue
		}
		if cost := extractTotalCost(plan); cost > 0 {
			m.costs[query.QueryID] = cost
			m.uses[query.QueryID] = planhash.UsesIndex(plan, guard)
		}
	}
	return m, nil
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
	// Hidden and hypothetical indexes are backend state, not transactional:
	// the rollback undoes neither, and hypopg_reset does not unhide.
	if s.canHide {
		_, err := s.conn.Exec(ctx, "SELECT "+s.function("hypopg_unhide_all_indexes")+"()")
		if err != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("unhide indexes: %w", err))
		}
	}
	if _, err := s.conn.Exec(ctx, "SELECT "+s.function("hypopg_reset")+"()"); err != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("reset hypothetical indexes: %w", err))
	}
	if cleanupErr != nil || s.discard {
		raw := s.conn.Hijack()
		return errors.Join(cleanupErr, raw.Close(ctx))
	}
	s.conn.Release()
	return nil
}
