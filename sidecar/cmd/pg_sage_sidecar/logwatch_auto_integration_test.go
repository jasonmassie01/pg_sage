//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/logwatch"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rca-09b integration (PostgreSQL 14 and 17): with no logwatch key the
// real pg_settings detection of a stderr-only server leaves log RCA
// disabled with a reason naming log_destination and no WARN; after
// log_destination becomes csvlog (accepted on PostgreSQL 14, which has no
// jsonlog) the next retry re-reads the settings and moves on to the
// collector. The ephemeral servers keep their log files inside the
// container, so activation itself is covered by the unit tests.
func TestLogSourceAutoDetectsRealServerSettings(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	resetLogDestination(t, pool)
	t.Cleanup(func() { resetLogDestination(t, pool) })
	prepareMetaGlobals(t)
	cfg = config.DefaultConfig()
	logs := captureLogs(t)
	clock := newFakeAfter()
	set := newLogFanoutSet()
	set.after = clock.after
	runCtx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	set.forCluster(runCtx, "live:5432", pool)
	clock.nextWait(t)
	st := set.status("live:5432")
	if st.State != logwatch.SourceDisabled || !strings.HasPrefix(st.Reason, "auto: ") ||
		!strings.Contains(st.Reason, `log_destination is "stderr"`) {
		t.Fatalf("stderr server status = %+v, want disabled auto naming log_destination", st)
	}
	if _, err := pool.Exec(ctx, "ALTER SYSTEM SET log_destination = 'csvlog'"); err != nil {
		t.Fatalf("set log_destination: %v", err)
	}
	if _, err := pool.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	waitSetting(t, pool, "log_destination", "csvlog")
	clock.fire(t)
	clock.nextWait(t)
	st = set.status("live:5432")
	if st.State != logwatch.SourceDisabled || strings.Contains(st.Reason, "log_destination") ||
		!strings.Contains(st.Reason, "logging_collector") {
		t.Fatalf("csvlog server status = %+v, want the collector named next", st)
	}
	if w, i := countLogs(logs, "WARN", warnPhrase),
		countLogs(logs, "INFO", infoPhrase); w != 0 || i != 1 {
		t.Fatalf("logged %d WARN and %d INFO, want 0 and 1:\n%s", w, i, logs)
	}
}

func resetLogDestination(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, "ALTER SYSTEM RESET log_destination"); err != nil {
		t.Fatalf("reset log_destination: %v", err)
	}
	if _, err := pool.Exec(ctx, "SELECT pg_reload_conf()"); err != nil {
		t.Fatalf("reload: %v", err)
	}
	waitSetting(t, pool, "log_destination", "stderr")
}

// waitSetting waits for a reloaded setting to reach new sessions.
func waitSetting(t *testing.T, pool *pgxpool.Pool, name, want string) {
	t.Helper()
	for range 200 {
		var got string
		err := pool.QueryRow(context.Background(),
			"SELECT setting FROM pg_settings WHERE name = $1", name).Scan(&got)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if got == want {
			return
		}
		pool.Reset()
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never became %q", name, want)
}
