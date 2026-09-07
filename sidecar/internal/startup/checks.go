package startup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// CheckResult holds the results of prerequisite checks.
type CheckResult struct {
	PGVersionNum       int
	QueryTextVisible   bool
	HasWALColumns      bool
	HasPlanTimeColumns bool
}

// RunChecks validates that the target PostgreSQL instance meets all
// prerequisites for pg_sage operation. It returns a CheckResult
// describing the capabilities of the instance.
func RunChecks(ctx context.Context, pool *pgxpool.Pool) (*CheckResult, error) {
	result := &CheckResult{
		QueryTextVisible: true, // assume true, flip if disproved
	}

	if err := checkConnectivity(ctx, pool); err != nil {
		return nil, fmt.Errorf("connectivity check: %w", err)
	}

	versionNum, err := checkPGVersion(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("version check: %w", err)
	}
	result.PGVersionNum = versionNum

	if err := checkExtensionInstalled(ctx, pool); err != nil {
		return nil, fmt.Errorf("pg_stat_statements check: %w", err)
	}

	if err := checkExtensionReadable(ctx, pool); err != nil {
		return nil, fmt.Errorf("pg_stat_statements access: %w", err)
	}

	queryVisible, err := checkQueryTextVisible(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("query text visibility check: %w", err)
	}
	result.QueryTextVisible = queryVisible

	hasWAL, err := checkWALColumns(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("WAL columns check: %w", err)
	}
	result.HasWALColumns = hasWAL

	hasPlan, err := checkPlanTimeColumns(ctx, pool)
	if err != nil {
		return nil, fmt.Errorf("plan time columns check: %w", err)
	}
	result.HasPlanTimeColumns = hasPlan

	return result, nil
}

func queryCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}

func checkConnectivity(ctx context.Context, pool *pgxpool.Pool) error {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	var one int
	err := pool.QueryRow(qctx, "SELECT 1").Scan(&one)
	if err != nil {
		return fmt.Errorf("cannot connect to PostgreSQL: %w", err)
	}
	return nil
}

func checkPGVersion(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	var versionNum int
	err := pool.QueryRow(
		qctx,
		"SELECT current_setting('server_version_num')::int",
	).Scan(&versionNum)
	if err != nil {
		return 0, fmt.Errorf("cannot read server version: %w", err)
	}

	if versionNum < 140000 {
		return versionNum, errors.New(
			"PostgreSQL 14+ required",
		)
	}
	return versionNum, nil
}

func checkExtensionInstalled(ctx context.Context, pool *pgxpool.Pool) error {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	var one int
	err := pool.QueryRow(
		qctx,
		"SELECT 1 FROM pg_extension WHERE extname = 'pg_stat_statements'",
	).Scan(&one)
	if err != nil {
		return fmt.Errorf(
			"pg_stat_statements extension is not installed; "+
				"install it with: CREATE EXTENSION pg_stat_statements; "+
				"and add shared_preload_libraries = 'pg_stat_statements' "+
				"to postgresql.conf, then restart PostgreSQL: %w", err,
		)
	}
	return nil
}

func checkExtensionReadable(ctx context.Context, pool *pgxpool.Pool) error {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	// count(*) always returns exactly one row, so an *empty*
	// pg_stat_statements (post-reset, track=none, idle) no longer trips
	// ErrNoRows and gets misreported as a permission failure (audit L).
	// A genuine permission error still surfaces here.
	var n int
	err := pool.QueryRow(
		qctx,
		"/* pg_sage */ SELECT count(*) FROM pg_stat_statements",
	).Scan(&n)
	if err != nil {
		return fmt.Errorf(
			"cannot read pg_stat_statements — ensure the connected role "+
				"has the pg_read_all_stats role or is a superuser: %w", err,
		)
	}
	return nil
}

func checkQueryTextVisible(
	ctx context.Context, pool *pgxpool.Pool,
) (bool, error) {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	var query *string
	err := pool.QueryRow(
		qctx,
		"/* pg_sage */ SELECT query FROM pg_stat_statements WHERE query IS NOT NULL LIMIT 1",
	).Scan(&query)
	if err != nil {
		// No rows with non-null query text — likely all NULL.
		return false, nil
	}
	return true, nil
}

func checkPlanTimeColumns(
	ctx context.Context, pool *pgxpool.Pool,
) (bool, error) {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	// pg_stat_statements is a view, so information_schema.columns may not
	// list it under pg_catalog. Query the view columns directly via
	// pg_attribute on the relation OID, which works for both tables and views.
	var exists bool
	err := pool.QueryRow(
		qctx,
		`SELECT EXISTS(
			SELECT 1 FROM pg_attribute
			WHERE attrelid = 'pg_stat_statements'::regclass
			  AND attname = 'total_plan_time'
			  AND NOT attisdropped
		)`,
	).Scan(&exists)
	if err != nil {
		return false, nil
	}
	return exists, nil
}

func checkWALColumns(
	ctx context.Context, pool *pgxpool.Pool,
) (bool, error) {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	// Use pg_attribute instead of information_schema.columns since
	// pg_stat_statements is a view, not a base table.
	var exists bool
	err := pool.QueryRow(
		qctx,
		`SELECT EXISTS(
			SELECT 1 FROM pg_attribute
			WHERE attrelid = 'pg_stat_statements'::regclass
			  AND attname = 'wal_records'
			  AND NOT attisdropped
		)`,
	).Scan(&exists)
	if err != nil {
		return false, nil
	}
	return exists, nil
}
