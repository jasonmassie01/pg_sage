package analyzer

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// market-07 on a live server: the collector's archiver row feeds the
// archiver rules. The test servers run with archive_mode = off, where
// nothing may fire (AP-11 owns that case); on a server started with
// archive_mode = on, archive_command = '/bin/false' and a pg_switch_wal()
// make wal_archive_failing fire.

func archiverServer(t *testing.T) (*pgxpool.Pool, int, string) {
	t.Helper()
	pool := phase2Pool(t)
	var version int
	var mode string
	if err := pool.QueryRow(context.Background(), `SELECT
		current_setting('server_version_num')::int, current_setting('archive_mode')`).
		Scan(&version, &mode); err != nil {
		t.Fatalf("read server version and archive_mode: %v", err)
	}
	return pool, version, mode
}

// lockWAL serializes the cluster-wide archiver changes with every other
// suite that switches WAL or changes archiving on this server.
func lockWAL(t *testing.T) {
	t.Helper()
	unlock, err := testdb.LockCluster(context.Background(), os.Getenv("SAGE_DATABASE_URL"),
		"wal")
	if err != nil {
		t.Fatalf("lock the cluster's WAL: %v", err)
	}
	t.Cleanup(unlock)
}

// archiverSnapshot runs one collector cycle (firstSnapshot requires
// replication data, which a server without slots has none of).
func archiverSnapshot(t *testing.T, pool *pgxpool.Pool, version int) *collector.Snapshot {
	t.Helper()
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 3600
	cfg.Safety.CPUCeilingPct = 100
	coll := collector.New(pool, cfg, version, noopLog)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coll.Run(ctx); close(done) }()
	deadline := time.Now().Add(60 * time.Second)
	for coll.LatestSnapshot() == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	snap := coll.LatestSnapshot()
	if snap == nil {
		t.Fatal("the collector produced no snapshot in 60 s")
	}
	return snap
}

func switchWAL(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `CREATE TEMP TABLE IF NOT EXISTS market07_wal(x int);
		INSERT INTO market07_wal SELECT generate_series(1, 1000)`); err != nil {
		t.Fatalf("write WAL: %v", err)
	}
	if _, err := pool.Exec(ctx, "SELECT pg_switch_wal()"); err != nil {
		t.Fatalf("pg_switch_wal: %v", err)
	}
}

func TestArchiverRules_ArchiveModeOffOnRealServer(t *testing.T) {
	pool, version, mode := archiverServer(t)
	if mode != "off" {
		t.Skipf("archive_mode=%s: this test covers a server that does not archive", mode)
	}
	lockWAL(t)
	first := archiverSnapshot(t, pool, version)
	switchWAL(t, pool)
	second := archiverSnapshot(t, pool, version)
	for i, s := range []*collector.Snapshot{first, second} {
		if !s.Available("archiver") || s.Archiver == nil {
			t.Fatalf("snapshot %d: archiver unread (%v)", i+1, s.Unavailable["archiver"])
		}
		if s.Archiver.ArchiveMode != "off" || s.Archiver.SegmentBytes <= 0 {
			t.Fatalf("snapshot %d: archiver = %+v, want mode off and a segment size", i+1,
				s.Archiver)
		}
	}
	if !hasArchiverInterval(second, first) {
		t.Fatal("two reads of one stats epoch are not an interval")
	}
	cfg := backupCfg()
	if got := ruleWALArchiveFailing(second, first, cfg, &RuleExtras{}); len(got) != 0 {
		t.Fatalf("archive_mode off: failing = %+v", got)
	}
	if got := ruleWALArchiveStale(second, first, cfg, &RuleExtras{}); len(got) != 0 {
		t.Fatalf("archive_mode off: stale = %+v", got)
	}
}

func TestArchiverRules_FailingArchiveCommandFires(t *testing.T) {
	pool, version, mode := archiverServer(t)
	if mode != "on" && mode != "always" {
		t.Skipf("archive_mode=%s: archive_mode needs a server restart, so this test needs a "+
			"server started with archive_mode=on (it sets archive_command itself)", mode)
	}
	lockWAL(t)
	ctx := context.Background()
	failArchiveCommand(t, pool, version)
	first := archiverSnapshot(t, pool, version)
	switchWAL(t, pool)
	waitForArchiveFailure(t, pool, first.Archiver.FailedCount)
	second := archiverSnapshot(t, pool, version)
	got := ruleWALArchiveFailing(second, first, backupCfg(), &RuleExtras{})
	f := findingFor(got, "archiver")
	if f == nil || f.Category != "wal_archive_failing" || f.Severity == "" {
		var row string
		_ = pool.QueryRow(ctx, "SELECT row_to_json(a)::text FROM pg_stat_archiver a").
			Scan(&row)
		t.Fatalf("findings %+v, want wal_archive_failing (pg_stat_archiver %s)", got, row)
	}
	if f.Detail["failed_count"].(int64) <= first.Archiver.FailedCount {
		t.Fatalf("detail %+v: failed_count did not rise past %d", f.Detail,
			first.Archiver.FailedCount)
	}
}

// failArchiveCommand points archive_command at /bin/false (a reload
// setting) and restores the server's own command afterwards.
func failArchiveCommand(t *testing.T, pool *pgxpool.Pool, version int) {
	t.Helper()
	ctx := context.Background()
	stmts := []string{"ALTER SYSTEM SET archive_command = '/bin/false'"}
	if version >= 150000 {
		stmts = append(stmts, "ALTER SYSTEM SET archive_library = ''")
	}
	for _, s := range append(stmts, "SELECT pg_reload_conf()") {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, "ALTER SYSTEM RESET archive_command")
		_, _ = pool.Exec(ctx, "ALTER SYSTEM RESET archive_library")
		_, _ = pool.Exec(ctx, "SELECT pg_reload_conf()")
	})
	time.Sleep(500 * time.Millisecond) // the archiver re-reads the file on SIGHUP
}

func waitForArchiveFailure(t *testing.T, pool *pgxpool.Pool, before int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var failed int64
		if err := pool.QueryRow(context.Background(),
			"SELECT failed_count FROM pg_stat_archiver").Scan(&failed); err != nil {
			t.Fatalf("read failed_count: %v", err)
		}
		if failed > before {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("failed_count stayed at %d for 30 s after pg_switch_wal()", before)
}
