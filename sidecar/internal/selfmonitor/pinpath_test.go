package selfmonitor

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/apppath"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// security-01c-n: the pinpath build tag runs the suite with the
// search_path pin security-01c-r ships; without it sessions keep the
// role's path. The pin itself is tested here in both builds.

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

func pinnedSelfPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	ConfigurePool(cfg)
	pinSearchPath(cfg.ConnConfig)
	cfg.MaxConns = 4
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// Concurrent acquires each get a pinned, READ COMMITTED session.
func TestPinSearchPath_EverySessionIsPinned(t *testing.T) {
	pool := pinnedSelfPool(t)
	var wg sync.WaitGroup
	errs := make(chan string, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var path, iso string
			err := pool.QueryRow(context.Background(), `SELECT current_setting('search_path'),
				current_setting('default_transaction_isolation')`).Scan(&path, &iso)
			if err != nil || path != testdb.PinnedPath || iso != "read committed" {
				errs <- path + " / " + iso + " / " + errString(err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Errorf("session: %s", e)
	}
}

func errString(err error) string {
	if err == nil {
		return "<nil>"
	}
	return err.Error()
}

// An apppath.Pin transaction ends with the session back on the pin.
func TestPinSearchPath_ApplicationPathIsTransactionLocal(t *testing.T) {
	pool := pinnedSelfPool(t)
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
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
	_ = tx.Rollback(ctx)
	if err := conn.QueryRow(ctx, "SHOW search_path").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if inside == testdb.PinnedPath || after != testdb.PinnedPath {
		t.Fatalf("inside %q after %q, want the application's path, then the pin", inside, after)
	}
}

// ConfigurePool pins the path exactly when the pinpath tag is on.
func TestConfigurePool_SearchPathFollowsTheHarnessTag(t *testing.T) {
	cfg, err := pgxpool.ParseConfig(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatal(err)
	}
	ConfigurePool(cfg)
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var path string
	if err := pool.QueryRow(context.Background(), "SHOW search_path").Scan(&path); err != nil {
		t.Fatal(err)
	}
	if (path == testdb.PinnedPath) != pinPathHarness {
		t.Fatalf("search_path = %q with pinPathHarness = %v", path, pinPathHarness)
	}
}
