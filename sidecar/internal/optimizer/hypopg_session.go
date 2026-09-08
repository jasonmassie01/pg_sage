package optimizer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/sanitize"
)

type hypopgSession struct {
	conn      *pgxpool.Conn
	tx        pgx.Tx
	namespace string
	version   int
}

func openHypoPGSession(ctx context.Context, pool *pgxpool.Pool) (*hypopgSession, error) {
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
		_, err = s.tx.Exec(ctx, "SET LOCAL statement_timeout = '5s'")
	}
	if err != nil {
		return nil, errors.Join(fmt.Errorf("begin bounded HypoPG transaction: %w", err), s.close())
	}
	return s, nil
}

func (s *hypopgSession) function(name string) string {
	return pgx.Identifier{s.namespace, name}.Sanitize()
}

func (s *hypopgSession) evaluate(ctx context.Context, ddl string,
	queries []QueryInfo,
) (float64, int64, error) {
	before, err := s.measureCosts(ctx, queries)
	if err != nil {
		return 0, 0, err
	}
	if len(before) == 0 {
		return 0, 0, nil
	}
	oid, err := s.createIndex(ctx, ddl)
	if err != nil {
		return 0, 0, err
	}
	after, err := s.measureCosts(ctx, queries)
	if err != nil {
		return 0, 0, err
	}
	improvement, measured := hypotheticalImprovement(before, after)
	if measured == 0 {
		return 0, 0, nil
	}
	size, err := s.estimateSize(ctx, oid)
	if err != nil {
		return 0, 0, err
	}
	return improvement, size, nil
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

func (s *hypopgSession) measureCosts(ctx context.Context,
	queries []QueryInfo,
) (map[int64]float64, error) {
	costs := make(map[int64]float64)
	for _, query := range queries {
		if !isExplainable(query.Text) {
			continue
		}
		if err := sanitize.RejectMultiStatement(query.Text); err != nil {
			return nil, fmt.Errorf("validate hypothetical workload query: %w", err)
		}
		plan, err := s.explain(ctx, query.Text)
		if err != nil {
			return nil, fmt.Errorf("explain hypothetical workload query: %w", err)
		}
		if cost := extractTotalCost(plan); cost > 0 {
			costs[query.QueryID] = cost
		}
	}
	return costs, nil
}

func (s *hypopgSession) explain(ctx context.Context, query string) ([]byte, error) {
	prefix := "EXPLAIN (FORMAT JSON) "
	if s.version >= 160000 {
		prefix = "EXPLAIN (GENERIC_PLAN, FORMAT JSON) "
	}
	// Keep normalized $n parameters unbound and all HypoPG planner hooks on the owning session.
	results, err := s.tx.Conn().PgConn().Exec(ctx, prefix+query).ReadAll()
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
