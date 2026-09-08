//go:build providerlive

// Package providerlive exercises only explicitly supplied disposable provider projects.
package providerlive

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/startup"
)

type target struct{ name, prefix string }

// No t.Parallel: startup owns fixed sage metadata for one disposable project at a time.
func TestProviderLive(t *testing.T) {
	for _, provider := range []target{{"neon", "SAGE_NEON"}, {"supabase", "SAGE_SUPABASE"}} {
		t.Run(provider.name, func(t *testing.T) {
			dsn := os.Getenv(provider.prefix + "_DATABASE_URL")
			if dsn == "" {
				t.Skip(provider.prefix + "_DATABASE_URL absent; live provider NOT verified")
			}
			pool := connect(t, dsn)
			t.Run("connection_and_cancellation", func(t *testing.T) { checkConnection(t, pool) })
			t.Run("startup_capabilities", func(t *testing.T) { checkStartup(t, pool) })
			t.Run("synthetic_features", func(t *testing.T) { checkFeatures(t, pool, provider) })
		})
	}
}

func connect(t *testing.T, dsn string) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	checkError(t, "parse provider DSN", err)
	cfg.MaxConns = 3
	cfg.ConnConfig.ConnectTimeout = 15 * time.Second
	pool, err := pgxpool.NewWithConfig(t.Context(), cfg)
	checkError(t, "create provider pool", err)
	t.Cleanup(pool.Close)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	checkError(t, "connect provider", pool.Ping(ctx))
	return pool
}

func checkConnection(t *testing.T, pool *pgxpool.Pool) {
	var one int
	checkError(t, "read constant", pool.QueryRow(t.Context(), "SELECT 1").Scan(&one))
	if one != 1 {
		t.Fatalf("constant query returned %d", one)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := pool.Ping(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled request: expected context.Canceled, got %T", err)
	}
	checkError(t, "connection recovers after cancellation", pool.Ping(t.Context()))
}

func checkStartup(t *testing.T, pool *pgxpool.Pool) {
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	result, err := startup.RunChecks(ctx, pool)
	checkError(t, "startup prerequisites", err)
	if result == nil || result.PGVersionNum < 140000 || !result.QueryTextVisible {
		t.Fatalf("missing startup capability: %+v", result)
	}
	t.Logf("PostgreSQL=%d visible_queries=%t WAL_columns=%t plan_columns=%t",
		result.PGVersionNum, result.QueryTextVisible, result.HasWALColumns,
		result.HasPlanTimeColumns)
	rows, err := pool.Query(ctx, `SELECT e.extname, e.extversion, n.nspname
		FROM pg_catalog.pg_extension e JOIN pg_catalog.pg_namespace n ON n.oid=e.extnamespace
		WHERE e.extname IN ('pg_stat_statements','vector','hypopg','pg_hint_plan') ORDER BY 1`)
	checkError(t, "discover extension namespaces", err)
	defer rows.Close()
	for rows.Next() {
		var name, version, namespace string
		checkError(t, "read extension capability", rows.Scan(&name, &version, &namespace))
		t.Logf("extension=%s version=%s namespace=%s", name, version, namespace)
	}
	checkError(t, "read extension capabilities", rows.Err())
}

// DSNs and provider error text can include credentials; only emit stable classifications.
func checkError(t *testing.T, operation string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		t.Fatalf("%s failed: SQLSTATE %s", operation, pgErr.Code)
	}
	t.Fatalf("%s failed: %T (details suppressed to protect connection secrets)", operation, err)
}

func requireMutationTarget(t *testing.T, pool *pgxpool.Pool, provider target) {
	t.Helper()
	if os.Getenv("SAGE_PROVIDER_LIVE_ALLOW_MUTATIONS") != "true" {
		t.Skip("synthetic writes disabled; SAGE_PROVIDER_LIVE_ALLOW_MUTATIONS=true required")
	}
	expected := strings.TrimSpace(os.Getenv(provider.prefix + "_EXPECTED_DATABASE"))
	if expected == "" {
		t.Fatal(provider.prefix + "_EXPECTED_DATABASE required for disposable project guard")
	}
	var actual string
	checkError(t, "identify disposable database",
		pool.QueryRow(t.Context(), "SELECT current_database()").Scan(&actual))
	if actual != expected {
		t.Fatal("database identity differs from explicitly authorized disposable target")
	}
}
