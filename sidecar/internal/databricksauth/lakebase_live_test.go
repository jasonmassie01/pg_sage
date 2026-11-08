package databricksauth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLakebaseLiveOAuthReconnect exercises the actual production pool hook,
// TLS, unified authentication and a new physical connection after pool reset.
// It reads only the connection's own catalog state and creates no objects.
func TestLakebaseLiveOAuthReconnect(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE=1, SAGE_LAKEBASE_LIVE_DATABASE_URL and SAGE_LAKEBASE_ENDPOINT")
	}
	dsn := os.Getenv("SAGE_LAKEBASE_LIVE_DATABASE_URL")
	if dsn == "" || os.Getenv("SAGE_LAKEBASE_ENDPOINT") == "" {
		t.Fatal("live Lakebase testing requires a passwordless DSN and endpoint resource path")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal("invalid live Lakebase DSN")
	}
	cfg.MinConns, cfg.MaxConns = 0, 1
	if err := ConfigurePool(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	probe := func() int {
		t.Helper()
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		var pid int
		var tls bool
		var version, readonly, username string
		if err := tx.QueryRow(ctx, `SELECT pg_backend_pid(), ssl,
			current_setting('server_version'), current_setting('transaction_read_only'), current_user
			FROM pg_stat_ssl WHERE pid = pg_backend_pid()`).Scan(&pid, &tls, &version, &readonly, &username); err != nil {
			t.Fatal(err)
		}
		if !tls || readonly != "on" || username != cfg.ConnConfig.User {
			t.Fatalf("TLS/read-only/identity mismatch: tls=%v, read_only=%s", tls, readonly)
		}
		t.Logf("PostgreSQL %s: TLS + read-only identity verified", version)
		return pid
	}
	first := probe()
	pool.Reset()
	if second := probe(); second == first {
		t.Fatal("pool reset did not establish a new physical backend")
	}
}
