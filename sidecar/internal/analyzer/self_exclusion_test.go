package analyzer

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// sagePool opens a pool configured like pg_sage's own (application_name
// pg_sage, tagged statements).
func sagePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Skipf("DSN unavailable: %v", err)
	}
	selfmonitor.ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pg_sage pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// appConn opens a non-pg_sage session named app.
func appConn(t *testing.T, app string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(os.Getenv("SAGE_DATABASE_URL"))
	if err != nil {
		t.Skipf("DSN unavailable: %v", err)
	}
	cfg.RuntimeParams["application_name"] = app
	conn, err := pgx.ConnectConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("app connection: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	return conn
}

// pg_sage's own sessions (its collector locks every sequence it reads,
// clone schemas' included) must not make a clone family look in use.
func TestCloneSessionsIgnorePgSageBackends(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	for _, s := range []string{"DROP SCHEMA IF EXISTS tenant_990002 CASCADE",
		"CREATE SCHEMA tenant_990002", "CREATE TABLE tenant_990002.orders (id int)"} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS tenant_990002 CASCADE")
	})
	tx, err := sagePool(t).Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "LOCK TABLE tenant_990002.orders IN ACCESS SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	a := New(pool, phase2Config(), nil, nil, nil, nil, nil, noopLog)
	schemas, queries, err := a.loadCloneSessions(ctx)
	if err != nil {
		t.Fatalf("load sessions: %v", err)
	}
	if schemas["tenant_990002"] {
		t.Fatal("pg_sage's own lock made the clone schema look in use")
	}
	for _, q := range queries {
		if strings.Contains(q, "tenant_990002") {
			t.Fatalf("pg_sage's own statement counted as session activity: %q", q)
		}
	}

	// The same lock held by an application session still counts.
	app := appConn(t, "clone_probe_app")
	appTx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = appTx.Rollback(ctx) }()
	if _, err := appTx.Exec(ctx, "LOCK TABLE tenant_990002.orders IN ACCESS SHARE MODE"); err != nil {
		t.Fatal(err)
	}
	schemas, _, err = a.loadCloneSessions(ctx)
	if err != nil {
		t.Fatalf("load sessions: %v", err)
	}
	if !schemas["tenant_990002"] {
		t.Fatalf("application lock not seen: %v", schemas)
	}
}

// A pg_sage session idle in a transaction is not a connection leak finding
// (pg_sage's own cost is reported by the self-cost meter instead); an
// application's is.
func TestConnectionLeaksIgnorePgSageBackends(t *testing.T) {
	pool := phase2Pool(t)
	ctx := context.Background()
	sageConn, err := sagePool(t).Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer sageConn.Release()
	sageTx, err := sageConn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sageTx.Rollback(ctx) }()
	var sagePID, appPID int
	if err := sageTx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&sagePID); err != nil {
		t.Fatal(err)
	}
	app := appConn(t, "leaky_app")
	appTx, err := app.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = appTx.Rollback(ctx) }()
	if err := appTx.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&appPID); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond) // both sessions now idle in transaction

	cfg := phase2Config()
	cfg.Analyzer.IdleInTxTimeoutMinutes = 0
	a := New(pool, cfg, nil, nil, nil, nil, nil, noopLog)
	findings := a.checkConnectionLeaks(ctx)
	seen := map[any]bool{}
	for _, f := range findings {
		seen[f.Detail["pid"]] = true
	}
	if seen[sagePID] {
		t.Fatalf("pg_sage backend %d reported as a connection leak", sagePID)
	}
	if !seen[appPID] {
		t.Fatalf("application backend %d not reported; findings = %+v", appPID, findings)
	}
}
