package selfmonitor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-r: every pool built through ConfigurePool pins its sessions
// to search_path = pg_catalog, pg_temp, after the READ COMMITTED pin.

func TestPinSearchPath_EarlierHookErrorSkipsThePin(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("isolation pin failed")
	calls := 0
	cfg.AfterConnect = func(context.Context, *pgconn.PgConn) error {
		calls++
		return wantErr
	}
	pinSearchPath(cfg)
	// A nil connection: the pin's Exec would panic if it were attempted.
	if err := cfg.AfterConnect(context.Background(), nil); !errors.Is(err, wantErr) ||
		calls != 1 {
		t.Fatalf("err = %v calls = %d, want %v from the earlier hook once", err, calls, wantErr)
	}
}

func TestPinSearchPath_NilHookStillPins(t *testing.T) {
	cfg, err := pgx.ParseConfig("postgres://u@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	cfg.AfterConnect = nil
	pinSearchPath(cfg)
	if cfg.AfterConnect == nil {
		t.Fatal("AfterConnect not set: the session would keep the role's search_path")
	}
	if !strings.Contains(PinSearchPathSQL, "search_path = pg_catalog, pg_temp") {
		t.Fatalf("PinSearchPathSQL = %q", PinSearchPathSQL)
	}
}

// configuredPool is a pool built exactly as pg_sage builds its own: through
// ConfigurePool and nothing else.
func configuredPool(t *testing.T, dsn string, maxConns int32) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	ConfigurePool(cfg)
	cfg.MaxConns = maxConns
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Happy path: a pooled session shows the pin, and the database's own
// setting (reset_val, which apppath reads) is untouched: the pin is a SET,
// not a startup parameter.
func TestConfigurePool_PinsSearchPath(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "pin_path")
	admin := configuredPool(t, dsn, 1)
	db := pgx.Identifier{databaseName(t, admin)}.Sanitize()
	if _, err := admin.Exec(context.Background(),
		"ALTER DATABASE "+db+" SET search_path = app, public"); err != nil {
		t.Fatal(err)
	}
	pool := configuredPool(t, dsn, 2)
	var path, reset string
	if err := pool.QueryRow(context.Background(), `SELECT current_setting('search_path'),
		(SELECT reset_val FROM pg_settings WHERE name = 'search_path')`).
		Scan(&path, &reset); err != nil {
		t.Fatal(err)
	}
	if path != testdb.PinnedPath {
		t.Fatalf("search_path = %q, want %q", path, testdb.PinnedPath)
	}
	if reset != "app, public" {
		t.Fatalf("reset_val = %q, want the database's own app, public", reset)
	}
}

func databaseName(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var name string
	if err := pool.QueryRow(context.Background(), "SELECT current_database()").
		Scan(&name); err != nil {
		t.Fatal(err)
	}
	return name
}

// Concurrency: 20 sessions held at once (so 20 distinct server sessions)
// each see the pin and READ COMMITTED.
func TestConfigurePool_TwentyConcurrentSessionsArePinned(t *testing.T) {
	const n = 20
	pool := configuredPool(t, testdb.SkipUnlessLive(t), n)
	ctx := context.Background()
	type result struct {
		conn      *pgxpool.Conn
		pid       int
		path, iso string
		err       error
	}
	results := make(chan result, n)
	for i := 0; i < n; i++ {
		go func() {
			var r result
			if r.conn, r.err = pool.Acquire(ctx); r.err == nil {
				r.err = r.conn.QueryRow(ctx, `SELECT pg_backend_pid(),
					current_setting('search_path'),
					current_setting('default_transaction_isolation')`).
					Scan(&r.pid, &r.path, &r.iso)
			}
			results <- r
		}()
	}
	pids := map[int]bool{}
	for i := 0; i < n; i++ {
		r := <-results
		if r.conn != nil {
			defer r.conn.Release()
		}
		if r.err != nil || r.path != testdb.PinnedPath || r.iso != "read committed" {
			t.Errorf("session %d: path %q iso %q err %v", r.pid, r.path, r.iso, r.err)
		}
		pids[r.pid] = true
	}
	if len(pids) != n {
		t.Fatalf("%d distinct sessions, want %d held at once", len(pids), n)
	}
}

// State transition: an apppath.Pin transaction plans on the application's
// path and the session returns to the pool on the pin.
func TestConfigurePool_ApplicationPathIsTransactionLocal(t *testing.T) {
	pool := configuredPool(t, testdb.SkipUnlessLive(t), 1)
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := apppath.Pin(ctx, tx); err != nil {
		t.Fatalf("apppath.Pin: %v", err)
	}
	var inside, after string
	if err := tx.QueryRow(ctx, "SHOW search_path").Scan(&inside); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	// MaxConns 1: this is the same session the transaction ran on.
	if err := pool.QueryRow(ctx, "SHOW search_path").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if inside == testdb.PinnedPath || after != testdb.PinnedPath {
		t.Fatalf("inside %q after %q, want the application's path, then the pin", inside, after)
	}
}
