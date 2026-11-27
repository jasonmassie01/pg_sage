package pgaudit

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/selfmonitor"
)

// Integration with real pgaudit (the pgaudit CI job): a CREATE INDEX
// CONCURRENTLY that waits 2 s for a lock under pg_sage's action tag is
// logged by pgaudit, ingested before its action row exists, and lands in
// its action's evidence pack. Needs pgaudit preloaded and the jsonlog
// collector, as TestRealPGAuditCorrelation does.
func TestRealPGAuditTaggedLongAction(t *testing.T) {
	pool, _ := freshDB(t, "pgaudit_real_tag")
	ctx := context.Background()
	if ok, _ := Installed(ctx, pool); !ok {
		t.Skip("pgaudit is not in shared_preload_libraries on this server")
	}
	var logfile *string
	if err := pool.QueryRow(ctx, "SELECT pg_current_logfile('jsonlog')").
		Scan(&logfile); err != nil || logfile == nil {
		t.Skip("the server does not write jsonlog through the logging collector")
	}
	table := "pgaudit_tag_" + itoa(time.Now().UnixNano())
	decision := seedDecision(t, pool)
	if _, err := pool.Exec(ctx, "CREATE TABLE "+table+" (a int)"); err != nil {
		t.Fatalf("create table: %v", err)
	}
	took := taggedLongIndexBuild(t, pool, table, decision)
	if took < 2*time.Second {
		t.Fatalf("the index build took %s, want at least 2 s", took)
	}
	entries := readLog(t, pool, *logfile, table+"_idx", 1)
	stats, err := NewCorrelator(pool, "orders", nil).Ingest(ctx, entries)
	if err != nil || stats.Stored != 1 {
		t.Fatalf("ingest of %d log lines = %+v, %v", len(entries), stats, err)
	}
	if count(t, pool, "correlated_by = 'action_tag' AND action_id IS NULL AND command = "+
		"'CREATE INDEX' AND statement LIKE '%"+table+"_idx%'") != 1 {
		t.Fatalf("the real tagged record was not stored by its tag")
	}
	action := seedAction(t, pool, decision, time.Now())
	if !strings.Contains(packFile(t, pool, action, "pgaudit.json"), table+"_idx") {
		t.Fatalf("action %d's pgaudit.json lacks the real tagged record", action)
	}
}

// taggedLongIndexBuild builds an index concurrently on a session named
// for decision, with pgaudit logging DDL, while another session holds a
// conflicting lock for 2 s. It returns how long the build took.
func taggedLongIndexBuild(t *testing.T, pool *pgxpool.Pool, table string,
	decision int64) time.Duration {
	t.Helper()
	ctx := context.Background()
	blocker, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin blocker: %v", err)
	}
	if _, err := blocker.Exec(ctx, "LOCK TABLE "+table+
		" IN SHARE UPDATE EXCLUSIVE MODE"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	// The lock is held 2 s from start, so the build, timed from start too,
	// waits at least that long (timed from before Acquire, it took 1.986 s).
	start := time.Now()
	go func() {
		time.Sleep(2 * time.Second)
		_ = blocker.Rollback(context.Background())
	}()
	for _, s := range []string{"SET pgaudit.log = 'ddl'",
		"SET application_name = '" + selfmonitor.ActionApplicationName(decision) + "'",
		"CREATE INDEX CONCURRENTLY " + table + "_idx ON " + table + " (a)",
		"RESET pgaudit.log", "RESET application_name"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return time.Since(start)
}
