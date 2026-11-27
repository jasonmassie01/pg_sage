package clusterlock_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/clusterlock"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// LockConfig (role budgets, 49752e33) is Lock for a pool's connection
// configuration: the same cluster-wide lock, taken by a session on the
// maintenance database, and the caller's configuration is left as it was.
// No concurrent access tests beyond the exclusivity check below:
// LockConfig shares the advisory key and release path that
// TestTryLockConcurrentCallersGetOneHolder already races.

func TestLockConfigTakesTheSameClusterLock(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	database, app := cfg.Database, cfg.RuntimeParams["application_name"]
	name := uniqueName(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	release, err := clusterlock.LockConfig(ctx, cfg, name)
	if err != nil || release == nil {
		t.Fatalf("LockConfig(%q) = release %v, err %v", name, release != nil, err)
	}
	if cfg.Database != database || cfg.RuntimeParams["application_name"] != app {
		t.Fatalf("LockConfig changed the caller's configuration: database %q, params %v",
			cfg.Database, cfg.RuntimeParams)
	}
	assertHolderSession(t, ctx, dsn)
	if again, ok := tryLock(t, dsn, name); ok {
		again()
		t.Fatal("TryLock was granted a lock LockConfig holds")
	}
	release()
	release() // releasing twice is harmless
	after, ok := tryLock(t, dsn, name)
	if !ok {
		t.Fatal("the lock stayed held after LockConfig's release")
	}
	after()
}

// assertHolderSession checks an advisory lock is held by a session named
// "pg_sage cluster lock" on the maintenance database.
func assertHolderSession(t *testing.T, ctx context.Context, dsn string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background())
	var n int
	err = conn.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_locks l
		JOIN pg_catalog.pg_stat_activity a ON a.pid = l.pid
		WHERE l.locktype = 'advisory' AND l.granted
		  AND a.application_name = 'pg_sage cluster lock' AND a.datname = $1`,
		clusterlock.MaintenanceDatabase).Scan(&n)
	if err != nil || n < 1 {
		t.Fatalf("holder sessions on %s = %d, err %v; want the lock's session",
			clusterlock.MaintenanceDatabase, n, err)
	}
}

func TestLockConfigInvalidInput(t *testing.T) {
	ctx := context.Background()
	cfg, err := pgx.ParseConfig("postgres://u:p@127.0.0.1:1/db?connect_timeout=2")
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cases := []struct {
		name string
		cfg  *pgx.ConnConfig
		lock string
		want string
	}{
		{"empty name", cfg, "  ", "empty lock name"},
		{"nil configuration", nil, "x", "no connection configuration"},
		{"unreachable server", cfg, "lock_x", `cluster lock "lock_x": connect maintenance ` +
			"database " + clusterlock.MaintenanceDatabase + " on 127.0.0.1:1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			release, err := clusterlock.LockConfig(ctx, tc.cfg, tc.lock)
			if release != nil || err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LockConfig = release %v, err %v; want an error naming %q",
					release != nil, err, tc.want)
			}
		})
	}
}
