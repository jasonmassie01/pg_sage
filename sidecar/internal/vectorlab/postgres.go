package vectorlab

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type postgresSource struct {
	tx              pgx.Tx
	manifest        Manifest
	extensionSchema string
	hnswIndexes     map[string]bool
}

// Run measures one workload in a single bounded read-only repeatable-read snapshot.
func Run(ctx context.Context, pool *pgxpool.Pool, m Manifest) (Report, error) {
	if err := m.Validate(); err != nil {
		return Report{}, err
	}
	if pool == nil {
		return Report{}, errors.New("vector lab requires a database pool")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(m.TotalTimeoutMS)*time.Millisecond)
	defer cancel()
	started := time.Now().UTC()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Report{}, safeError("begin read-only experiment", err)
	}
	defer rollback(tx)
	s := &postgresSource{tx: tx, manifest: m}
	metadata, err := s.prepare(ctx)
	if err != nil {
		return Report{}, err
	}
	report, err := runExperiment(ctx, s, m)
	if err != nil {
		return Report{}, err
	}
	report.StartedAt, report.PostgresVersion = started, metadata.PostgresVersion
	report.VectorVersion, report.Snapshot = metadata.VectorVersion, metadata.Snapshot
	return report, nil
}

func rollback(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Rollback failure makes pgx destroy the connection; no state returns to the pool.
	_ = tx.Rollback(ctx)
}

func (s *postgresSource) prepare(ctx context.Context) (Report, error) {
	timeout := strconv.Itoa(s.manifest.StatementTimeoutMS) + "ms"
	settings := [][2]string{
		{"statement_timeout", timeout}, {"lock_timeout", timeout},
		{"idle_in_transaction_session_timeout", timeout},
		{"plan_cache_mode", "force_custom_plan"}, {"search_path", "pg_catalog"},
		{"work_mem", "16MB"},
	}
	for _, setting := range settings {
		if err := s.setLocal(ctx, setting[0], setting[1]); err != nil {
			return Report{}, err
		}
	}
	var metadata Report
	err := s.tx.QueryRow(ctx, `SELECT e.extversion, n.nspname,
		current_setting('server_version'), pg_current_snapshot()::text
		FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace
		WHERE e.extname='vector'`).Scan(&metadata.VectorVersion, &s.extensionSchema,
		&metadata.PostgresVersion, &metadata.Snapshot)
	if errors.Is(err, pgx.ErrNoRows) {
		return Report{}, errors.New("pgvector extension is required")
	}
	if err != nil {
		return Report{}, safeError("inspect pgvector capabilities", err)
	}
	if !supportsIterative(metadata.VectorVersion) {
		return Report{}, errors.New("pgvector >=0.8.0 is required for explicit iterative-scan controls")
	}
	if err := s.validateTarget(ctx); err != nil {
		return Report{}, err
	}
	s.hnswIndexes, err = s.loadIndexes(ctx)
	return metadata, err
}

func (s *postgresSource) setLocal(ctx context.Context, name, value string) error {
	_, err := s.tx.Exec(ctx, "SELECT pg_catalog.set_config($1, $2, true)", name, value)
	if err != nil {
		return safeError("set bounded transaction controls", err)
	}
	return nil
}

func (s *postgresSource) observe(
	ctx context.Context, q Query, variant *Variant,
) (observation, error) {
	if err := s.configureScan(ctx, variant); err != nil {
		return observation{}, err
	}
	sql, args := buildSQL(s.manifest, s.extensionSchema, q, variant == nil)
	var indexes []string
	if variant != nil {
		var err error
		indexes, err = s.planIndexes(ctx, sql, args)
		if err != nil {
			return observation{}, err
		}
	}
	started := time.Now()
	rows, err := s.tx.Query(ctx, sql, args...)
	if err != nil {
		return observation{}, safeError("execute vector search", err)
	}
	defer rows.Close()
	result := observation{indexes: indexes}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.distance); err != nil {
			return observation{}, safeError("read vector result: ID must be non-null scalar", err)
		}
		result.rows = append(result.rows, r)
	}
	if err := rows.Err(); err != nil {
		return observation{}, safeError("read vector results", err)
	}
	result.elapsed = time.Since(started)
	return result, nil
}

func (s *postgresSource) configureScan(ctx context.Context, v *Variant) error {
	indexScan := "off"
	if v != nil {
		indexScan = "on"
	}
	settings := [][2]string{{"enable_indexscan", indexScan}, {"enable_indexonlyscan", indexScan}}
	if v != nil {
		settings = append(settings, [2]string{"hnsw.ef_search", strconv.Itoa(v.EFSearch)},
			[2]string{"hnsw.iterative_scan", v.IterativeScan},
			[2]string{"hnsw.max_scan_tuples", "20000"}, [2]string{"hnsw.scan_mem_multiplier", "1"})
	}
	for _, setting := range settings {
		if err := s.setLocal(ctx, setting[0], setting[1]); err != nil {
			return err
		}
	}
	return nil
}

func safeError(operation string, err error) error {
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%s: %w", operation, context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%s: %w", operation, context.DeadlineExceeded)
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fmt.Errorf("%s: PostgreSQL SQLSTATE %s", operation, pgErr.Code)
	}
	return fmt.Errorf("%s: database operation failed; check connectivity, privileges, and schema",
		operation)
}

func supportsIterative(version string) bool {
	var major, minor, patch int
	if _, err := fmt.Sscanf(version, "%d.%d.%d", &major, &minor, &patch); err != nil {
		return false
	}
	return major > 0 || (major == 0 && minor >= 8)
}
