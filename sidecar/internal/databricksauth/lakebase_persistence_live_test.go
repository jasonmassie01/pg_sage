package databricksauth

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLakebaseLivePersistenceReadback reads rows left by the isolated native
// fixture. Observing endpoint suspension is a separate external prerequisite;
// this test alone establishes only fresh-session authentication and persistence.
func TestLakebaseLivePersistenceReadback(t *testing.T) {
	if os.Getenv("PG_SAGE_LIVE_LAKEBASE_READBACK") != "1" {
		t.Skip("set PG_SAGE_LIVE_LAKEBASE_READBACK=1 for isolated fixture readback")
	}
	if os.Getenv("SAGE_LAKEBASE_ENDPOINT") != "projects/search/branches/pg-sage-test/endpoints/primary" {
		t.Fatal("persistence readback requires the explicitly isolated pg-sage-test endpoint")
	}
	cfg, err := pgxpool.ParseConfig(os.Getenv("SAGE_LAKEBASE_LIVE_DATABASE_URL"))
	if err != nil || cfg == nil || cfg.ConnConfig.Database != "pg_sage_test" || cfg.ConnConfig.Password != "" {
		t.Fatal("persistence readback requires a passwordless pg_sage_test DSN")
	}
	if cfg.ConnConfig.TLSConfig == nil || cfg.ConnConfig.TLSConfig.InsecureSkipVerify {
		t.Fatal("persistence readback requires verified TLS")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cfg.MinConns, cfg.MaxConns = 0, 1
	if err := ConfigurePool(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var marker, username, readonly string
	var snapshots, findings int64
	var tls bool
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT value FROM sage.config WHERE key='lakebase_live_marker'),
		(SELECT count(*) FROM sage.snapshots),
		(SELECT count(*) FROM sage.findings),
		current_user, current_setting('transaction_read_only'),
		(SELECT ssl FROM pg_stat_ssl WHERE pid=pg_backend_pid())`).Scan(
		&marker, &snapshots, &findings, &username, &readonly, &tls); err != nil {
		t.Fatal(err)
	}
	if marker != "persistent" || snapshots < 1 || findings < 1 {
		t.Fatalf("fixture persistence mismatch: marker=%q snapshots=%d findings=%d", marker, snapshots, findings)
	}
	if username != cfg.ConnConfig.User || readonly != "on" || !tls {
		t.Fatal("fresh-session identity, read-only transaction or TLS mismatch")
	}
	t.Logf("production OAuth fresh-session readback: marker retained; snapshots=%d findings=%d; TLS and read-only identity verified", snapshots, findings)
}
