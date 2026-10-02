package probes

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// M6 reactive probes against real PostgreSQL (14 to 18): every probe
// runs on the server's own variant, returns its documented columns and
// reads live state, not a cached or invented value.

func run(ctx context.Context, pool *pgxpool.Pool, id ID) Result {
	return NewRunner(pool, Catalog(), NewLimiter(1)).Run(ctx, id, Args{})
}

func TestCatalog_CheckpointActivityCountsARequestedCheckpoint(t *testing.T) {
	pool, ctx := livePool(t)
	before, err := CheckpointStats(run(ctx, pool, CheckpointActivity))
	if err != nil {
		t.Fatalf("checkpoint_activity: %v", err)
	}
	if _, err := pool.Exec(ctx, "CHECKPOINT"); err != nil {
		t.Skipf("CHECKPOINT not permitted for the test role: %v", err)
	}
	var after CheckpointStat
	deadline := time.Now().Add(10 * time.Second)
	for {
		after, err = CheckpointStats(run(ctx, pool, CheckpointActivity))
		if err != nil {
			t.Fatalf("checkpoint_activity: %v", err)
		}
		if after.Requested > before.Requested || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if !(after.Requested > before.Requested) {
		t.Fatalf("requested checkpoints %v -> %v after CHECKPOINT", before.Requested,
			after.Requested)
	}
	var maxWAL, timeout int64
	_ = pool.QueryRow(ctx, `SELECT (SELECT setting::int8 FROM pg_settings
		WHERE name = 'max_wal_size'), (SELECT setting::int8 FROM pg_settings
		WHERE name = 'checkpoint_timeout')`).Scan(&maxWAL, &timeout)
	if after.MaxWALSize != float64(maxWAL<<20) || after.TimeoutS != float64(timeout) ||
		!Known(after.WALBytes) || !Known(after.WALFPI) || !Known(after.BackendFsyncs) ||
		after.CompletionTarget <= 0 || after.ServerStartedAt.IsZero() {
		t.Fatalf("checkpoint sample = %+v (max_wal_size %d MB, timeout %d s)", after,
			maxWAL, timeout)
	}
}

func TestCatalog_TempFileActivityReadsThisDatabase(t *testing.T) {
	pool, ctx := livePool(t)
	s, err := TempStats(run(ctx, pool, TempFileActivity))
	if err != nil {
		t.Fatalf("temp_file_activity: %v", err)
	}
	var files, bytes, workMem int64
	_ = pool.QueryRow(ctx, `SELECT temp_files, temp_bytes,
		pg_size_bytes(current_setting('work_mem')) FROM pg_stat_database
		WHERE datname = current_database()`).Scan(&files, &bytes, &workMem)
	if s.Files < float64(files) || s.Bytes < float64(bytes) ||
		s.WorkMemBytes != float64(workMem) || !Known(s.TempFileLimitKB) {
		t.Fatalf("temp sample = %+v, database files %d bytes %d work_mem %d", s, files,
			bytes, workMem)
	}
}

// A cursor over a spilled sort keeps its temp file open: the holder is
// that backend, attributed to this database.
func TestCatalog_TempFileHoldersSeesALiveSpill(t *testing.T) {
	pool, ctx := livePool(t)
	pid := spillingCursor(t, ctx, pool)
	res := run(ctx, pool, TempFileHolders)
	if res.Status == StatusNoPrivilege {
		t.Skip("pg_ls_tmpdir needs pg_monitor; the test role lacks it")
	}
	hs, err := TempHolders(res)
	if err != nil {
		t.Fatalf("temp_file_holders = %+v (%v)", res, err)
	}
	for _, h := range hs {
		if h.PID == pid {
			if !h.InCurrentDatabase || h.Files < 1 || !(h.Bytes > 0) ||
				h.TotalBytes < h.Bytes || h.BackendStart.IsZero() {
				t.Fatalf("holder = %+v", h)
			}
			return
		}
	}
	t.Fatalf("pid %d holds a spilled cursor but is not among %+v", pid, hs)
}

// spillingCursor leaves a session with an open cursor over a spilled
// sort; cleanup rolls it back and returns the connection.
func spillingCursor(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var pid int64
	_ = conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	_, err = conn.Conn().PgConn().Exec(ctx, `BEGIN; SET LOCAL work_mem = '64kB';
		DECLARE spill CURSOR FOR SELECT g FROM generate_series(1, 200000) g
		ORDER BY md5(g::text); FETCH 1 FROM spill;`).ReadAll()
	if err != nil {
		conn.Release()
		t.Fatalf("spilling cursor: %v", err)
	}
	t.Cleanup(func() {
		_, _ = conn.Exec(context.Background(), "ROLLBACK")
		conn.Release()
	})
	return pid
}

func TestCatalog_TempSpillStatementsFindsASpillingStatement(t *testing.T) {
	pool, ctx := livePool(t)
	if err := pgssReady(ctx, pool); err != nil {
		t.Skipf("pg_stat_statements unavailable: %v", err)
	}
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	_, err = conn.Conn().PgConn().Exec(ctx, `SET work_mem = '64kB';
		SELECT count(*) FROM (SELECT g FROM generate_series(1, 100000) g
		ORDER BY md5(g::text)) s;`).ReadAll()
	conn.Release()
	if err != nil {
		t.Fatalf("spilling statement: %v", err)
	}
	ss, err := SpillStatements(run(ctx, pool, TempSpillStatements))
	if err != nil || len(ss) == 0 {
		t.Fatalf("temp_spill_statements = %+v (%v)", ss, err)
	}
	if !(ss[0].TempBlksWritten > 0) || ss[0].Calls < 1 || ss[0].BlockSize < 1024 {
		t.Fatalf("top spilling statement = %+v", ss[0])
	}
}

// pgssReady creates pg_stat_statements when possible and checks it can
// be read (it must also be preloaded).
func pgssReady(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS pg_stat_statements"); err != nil {
		return err
	}
	_, err := pool.Exec(ctx, "SELECT count(*) FROM pg_stat_statements")
	return err
}

// Without the extension the probe is unsupported with a stable reason,
// never "no statement spilled".
func TestCatalog_TempSpillStatementsUnsupportedWithoutExtension(t *testing.T) {
	_, ctx := livePool(t)
	fresh, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "sre_pgss_fresh"))
	if err != nil {
		t.Fatalf("connect fresh: %v", err)
	}
	t.Cleanup(fresh.Close)
	// template1 may carry the extension: a database without it is the case.
	if _, err := fresh.Exec(ctx, "DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatalf("drop extension: %v", err)
	}
	res := run(ctx, fresh, TempSpillStatements)
	if res.Status != StatusUnsupported || res.Reason != "extension_not_installed" ||
		len(res.Rows) != 0 {
		t.Fatalf("temp_spill_statements without the extension = %+v", res)
	}
}

// An extension in a schema outside search_path is still found.
func TestCatalog_TempSpillStatementsFindsTheExtensionInAnySchema(t *testing.T) {
	_, ctx := livePool(t)
	fresh, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "sre_pgss_schema"))
	if err != nil {
		t.Fatalf("connect fresh: %v", err)
	}
	t.Cleanup(fresh.Close)
	if _, err := fresh.Exec(ctx, `DROP EXTENSION IF EXISTS pg_stat_statements;
		CREATE SCHEMA "Odd Schema";
		CREATE EXTENSION pg_stat_statements SCHEMA "Odd Schema"`); err != nil {
		t.Skipf("pg_stat_statements not installable: %v", err)
	}
	res := run(ctx, fresh, TempSpillStatements)
	if res.Status == StatusUnsupported && res.Reason == "prerequisite_not_met" {
		t.Skip("pg_stat_statements is not preloaded on this server")
	}
	if !res.Status.Usable() {
		t.Fatalf("temp_spill_statements in a quoted schema = %+v", res)
	}
}

func TestCatalog_ReplicationLagV2ColumnsWithoutReplicas(t *testing.T) {
	pool, ctx := livePool(t)
	res := run(ctx, pool, ReplicationLag)
	if !res.Status.Usable() {
		t.Fatalf("replication_lag = %+v", res)
	}
	cols := strings.Join(res.Columns, ",")
	for _, c := range []string{"pid", "kind", "send_backlog_bytes", "flush_backlog_bytes",
		"replay_backlog_bytes", "replay_lag_bytes"} {
		if !strings.Contains(cols, c) {
			t.Errorf("replication_lag columns %s lack %s", cols, c)
		}
	}
	if _, err := ReplicationStages(res); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestCatalog_StandbyReplayStateOnAPrimary(t *testing.T) {
	pool, ctx := livePool(t)
	s, err := StandbyStates(run(ctx, pool, StandbyReplayState))
	if err != nil {
		t.Fatalf("standby_replay_state: %v", err)
	}
	var inRecovery bool
	_ = pool.QueryRow(ctx, "SELECT pg_is_in_recovery()").Scan(&inRecovery)
	if s.InRecovery != inRecovery || (!inRecovery && (s.ReplayPaused ||
		Known(s.ReceiveReplayBytes))) || !Known(s.Conflicts) || s.ServerStartedAt.IsZero() {
		t.Fatalf("standby state = %+v (in recovery %v)", s, inRecovery)
	}
}

// A sleeping session is sampled under its wait event; the probe's own
// backend never is.
func TestCatalog_LWLockWaitsSamplesActiveBackends(t *testing.T) {
	pool, ctx := livePool(t)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	var pid int64
	_ = conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid)
	go func() { _, _ = conn.Exec(context.Background(), "SELECT pg_sleep(3)") }()
	defer func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_cancel_backend($1)", pid)
		conn.Release()
	}()
	var gs []WaitGroup
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		gs, err = WaitGroups(run(ctx, pool, LWLockWaits))
		if err != nil {
			t.Fatalf("lwlock_waits: %v", err)
		}
		if sleeping(gs) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !sleeping(gs) {
		t.Fatalf("no PgSleep group in %+v", gs)
	}
	for _, g := range gs {
		if g.ActiveBackends < g.Backends || g.Backends < 1 {
			t.Fatalf("group %+v: counts are inconsistent", g)
		}
	}
}

func sleeping(gs []WaitGroup) bool {
	for _, g := range gs {
		if g.Type == "Timeout" && g.Event == "PgSleep" && g.InCurrentDatabase {
			return true
		}
	}
	return false
}

// The probes stay read-only: a write inside any of them would fail the
// read-only transaction the runner opens.
func TestCatalog_M6ProbesRunReadOnly(t *testing.T) {
	pool, ctx := livePool(t)
	for _, id := range m6ReactiveIDs() {
		res := run(ctx, pool, id)
		if res.Reason == "read_only_violation" {
			t.Errorf("%s attempted a write: %+v", id, res)
		}
		if res.Status == StatusError {
			t.Errorf("%s failed: %+v", id, res)
		}
	}
}
