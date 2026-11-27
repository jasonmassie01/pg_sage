package startup

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/rolegrants"
)

// ErrStatementsUnavailable marks a missing or unreadable pg_stat_statements:
// the one prerequisite pg_sage can run without. It degrades (catalog-only
// first look and rules, no query statistics) instead of refusing to start.
var ErrStatementsUnavailable = errors.New("pg_stat_statements is unavailable")

// querier runs one-row checks: a pool or a connection.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// RunChecks validates that the target PostgreSQL instance meets all
// prerequisites for pg_sage operation. It returns a CheckResult
// describing the capabilities of the instance.
func RunChecks(ctx context.Context, pool *pgxpool.Pool) (*CheckResult, error) {
	if pool == nil {
		return nil, errors.New("startup checks require a PostgreSQL connection pool")
	}
	endpoint := pool.Config().ConnConfig
	if err := ValidateSessionEndpoint(endpoint.Host, endpoint.Port); err != nil {
		return nil, err
	}
	return runChecks(ctx, pool)
}

func runChecks(ctx context.Context, q querier) (*CheckResult, error) {
	if err := checkConnectivity(ctx, q); err != nil {
		return nil, fmt.Errorf("connectivity check: %w", err)
	}
	result := &CheckResult{}
	versionNum, err := checkPGVersion(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("version check: %w", err)
	}
	result.PGVersionNum = versionNum

	schema, err := checkExtensionInstalled(ctx, q)
	if err != nil {
		return nil, statementsError(ctx, "pg_stat_statements check", err)
	}
	result.StatementsSchema = schema
	rel := statementsRelation(schema)
	if err := checkExtensionReadable(ctx, q, rel); err != nil {
		return nil, statementsError(ctx, "pg_stat_statements access", err)
	}

	queryVisible, err := checkQueryTextVisible(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("query text visibility check: %w", err)
	}
	result.QueryTextVisible = queryVisible
	if err := checkCapabilityColumns(ctx, q, rel, result); err != nil {
		return nil, err
	}
	return result, nil
}

func queryCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, 5*time.Second)
}

func checkConnectivity(ctx context.Context, q querier) error {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	var one int
	err := q.QueryRow(qctx, "SELECT 1").Scan(&one)
	if err != nil {
		return fmt.Errorf("cannot connect to PostgreSQL: %w", err)
	}
	return nil
}

func checkPGVersion(ctx context.Context, q querier) (int, error) {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	var versionNum int
	err := q.QueryRow(
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

// statementsSchemaSQL finds the schema pg_stat_statements was created in.
const statementsSchemaSQL = `/* pg_sage */ SELECT n.nspname::text
  FROM pg_catalog.pg_extension e
  JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
 WHERE e.extname = 'pg_stat_statements'`

// checkExtensionInstalled returns the schema of pg_stat_statements.
func checkExtensionInstalled(ctx context.Context, q querier) (string, error) {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	var schema string
	if err := q.QueryRow(qctx, statementsSchemaSQL).Scan(&schema); err != nil {
		return "", fmt.Errorf(
			"pg_stat_statements extension is not installed; "+
				"install it with: CREATE EXTENSION pg_stat_statements; "+
				"and add shared_preload_libraries = 'pg_stat_statements' "+
				"to postgresql.conf, then restart PostgreSQL (the dashboard's "+
				"first look lists the exact steps for this provider): %w", err,
		)
	}
	return schema, nil
}

// checkExtensionReadable reads rel, the qualified pg_stat_statements.
func checkExtensionReadable(ctx context.Context, q querier, rel string) error {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	// count(*) always returns exactly one row, so an *empty*
	// pg_stat_statements (post-reset, track=none, idle) no longer trips
	// ErrNoRows and gets misreported as a permission failure (audit L).
	// A genuine permission error still surfaces here.
	var n int
	err := q.QueryRow(qctx, "/* pg_sage */ SELECT count(*) FROM "+rel).Scan(&n)
	if err != nil {
		return fmt.Errorf(
			"cannot read %s — ensure the connected role "+
				"has the pg_read_all_stats role or is a superuser: %w", rel, err,
		)
	}
	return nil
}

// checkQueryTextVisible reports pg_read_all_stats membership. A failed
// query is an error (unknown), never "hidden".
func checkQueryTextVisible(ctx context.Context, q querier) (bool, error) {
	qctx, cancel := queryCtx(ctx)
	defer cancel()

	var visible bool
	err := q.QueryRow(qctx, "/* pg_sage */ SELECT "+rolegrants.QueryTextVisibleSQL).
		Scan(&visible)
	if err != nil {
		return false, fmt.Errorf("check pg_read_all_stats membership: %w", err)
	}
	return visible, nil
}

// statementsError marks a pg_stat_statements failure degradable, unless the
// context ended: that is not a missing extension.
func statementsError(ctx context.Context, what string, err error) error {
	if ctx.Err() != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	return fmt.Errorf("%s: %w: %w", what, ErrStatementsUnavailable, err)
}
