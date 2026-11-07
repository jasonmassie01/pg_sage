package selfmonitor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// No concurrent access tests: the pin is a per-connection hook with no
// shared state; the auditchain and executor tests drive it concurrently.

func TestConfigureConn_InstallsIsolationPin(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	ConfigureConn(cfg)
	if cfg.AfterConnect == nil {
		t.Fatal("AfterConnect not set: sessions keep a non-read-committed role default")
	}
}

// An earlier AfterConnect hook runs first, and its error fails the
// connection before the pin touches it.
func TestConfigureConn_EarlierAfterConnectErrorStopsThePin(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("session setup failed")
	calls := 0
	cfg.AfterConnect = func(context.Context, *pgconn.PgConn) error {
		calls++
		return wantErr
	}
	ConfigureConn(cfg)
	// A nil connection proves the pin never ran: it would dereference it.
	err = cfg.AfterConnect(context.Background(), nil)
	if !errors.Is(err, wantErr) || calls != 1 {
		t.Fatalf("err = %v calls = %d, want %v from the earlier hook once", err, calls, wantErr)
	}
}

// isolationDSN is a fresh database whose role default is level.
func isolationDSN(t *testing.T, label, level string) string {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	testdb.SetIsolationDefault(t, dsn, level)
	return dsn
}

func openPool(t *testing.T, dsn string, configure func(*pgxpool.Config)) *pgxpool.Pool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	if configure != nil {
		configure(cfg)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// txIsolation is the isolation level a plain BEGIN gets on pool.
func txIsolation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var level string
	if err := tx.QueryRow(ctx, "SHOW transaction_isolation").Scan(&level); err != nil {
		t.Fatalf("show transaction_isolation: %v", err)
	}
	return level
}

// Every pg_sage session runs READ COMMITTED whatever the role default is,
// and the default itself is left alone (a SET, not a startup parameter).
func TestConfigurePool_PinsReadCommittedOverRoleDefault(t *testing.T) {
	for _, level := range []string{"repeatable read", "serializable", "read committed"} {
		t.Run(level, func(t *testing.T) {
			dsn := isolationDSN(t, "iso_pin", level)
			if got := txIsolation(t, openPool(t, dsn, nil)); got != level {
				t.Fatalf("unpinned session isolation = %q, want the role default %q",
					got, level)
			}
			pinned := openPool(t, dsn, ConfigurePool)
			if got := txIsolation(t, pinned); got != "read committed" {
				t.Fatalf("pinned session isolation = %q, want read committed", got)
			}
			var reset string
			if err := pinned.QueryRow(context.Background(), `SELECT reset_val
				FROM pg_settings WHERE name = 'default_transaction_isolation'`).
				Scan(&reset); err != nil {
				t.Fatalf("read reset_val: %v", err)
			}
			if reset != level {
				t.Fatalf("reset_val = %q, want the untouched role default %q", reset, level)
			}
		})
	}
}

// An earlier AfterConnect hook still runs, and the pin runs after it, so
// a hook that sets another level cannot undo it.
func TestConfigurePool_EarlierAfterConnectRunsThenPinWins(t *testing.T) {
	dsn := isolationDSN(t, "iso_chain", "repeatable read")
	var calls int
	pool := openPool(t, dsn, func(cfg *pgxpool.Config) {
		cfg.ConnConfig.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
			calls++
			return pc.Exec(ctx, "SET SESSION default_transaction_isolation = 'serializable'").
				Close()
		}
		ConfigurePool(cfg)
	})
	if got := txIsolation(t, pool); got != "read committed" {
		t.Fatalf("isolation = %q, want read committed after the earlier hook", got)
	}
	if calls < 1 {
		t.Fatal("the earlier AfterConnect hook never ran")
	}
}

// A pin that cannot be applied fails the connection with an error that
// names the setting: pg_sage never runs a session it could not pin.
func TestConfigureConn_PinFailureFailsTheConnection(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "iso_fail")
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = func(ctx context.Context, pc *pgconn.PgConn) error {
		return pc.Close(ctx) // the pin then writes to a closed connection
	}
	ConfigureConn(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err == nil {
		_ = conn.Close(ctx)
		t.Fatal("connect succeeded although the isolation pin could not be applied")
	}
	if !strings.Contains(err.Error(), "default_transaction_isolation") {
		t.Fatalf("err = %v, want it to name default_transaction_isolation", err)
	}
}
