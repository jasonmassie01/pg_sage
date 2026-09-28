package rca

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/config"
)

// Integration tests for the M0 lock-chain fast path against a real
// PostgreSQL blocking chain: an idle-in-transaction holder, a DDL queued
// behind it, and a reader queued behind the DDL (depth 2).

type blockingChain struct {
	holderPID   int
	holderStart time.Time
	release     func()
}

func dialTest(t *testing.T, ctx context.Context) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, os.Getenv("SAGE_TEST_DATABASE_URL"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	return conn
}

func startBlockingChain(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) *blockingChain {
	t.Helper()
	table := "sre_chain_" + strings.ToLower(
		strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	ident := pgx.Identifier{table}.Sanitize()
	if _, err := pool.Exec(ctx, "DROP TABLE IF EXISTS "+ident+
		"; CREATE TABLE "+ident+" (id int PRIMARY KEY)"); err != nil {
		t.Fatalf("create chain table: %v", err)
	}
	bc := &blockingChain{}
	holder := openHolder(t, ctx, pool, ident, bc)
	var wg sync.WaitGroup
	waiters := []string{"ALTER TABLE " + ident + " ADD COLUMN v int",
		"SELECT count(*) FROM " + ident}
	conns := []*pgx.Conn{holder}
	for _, sql := range waiters {
		conn := dialTest(t, ctx)
		conns = append(conns, conn)
		wg.Add(1)
		go func(c *pgx.Conn, sql string) {
			defer wg.Done()
			_, _ = c.Exec(context.Background(), sql)
		}(conn, sql)
		waitForWaiters(t, ctx, pool, len(conns)-1)
	}
	var once sync.Once
	bc.release = func() {
		once.Do(func() {
			_, _ = holder.Exec(context.Background(), "ROLLBACK")
			wg.Wait()
			for _, c := range conns {
				_ = c.Close(context.Background())
			}
			_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+ident)
		})
	}
	t.Cleanup(bc.release)
	return bc
}

// openHolder starts the idle-in-transaction holder of ACCESS SHARE on
// ident and records its identity. The identity is read without touching
// catalogs inside the transaction, so the holder locks only the table.
func openHolder(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, ident string,
	bc *blockingChain,
) *pgx.Conn {
	t.Helper()
	holder := dialTest(t, ctx)
	for _, sql := range []string{"BEGIN", "SELECT count(*) FROM " + ident} {
		if _, err := holder.Exec(ctx, sql); err != nil {
			t.Fatalf("holder %s: %v", sql, err)
		}
	}
	if err := holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(
		&bc.holderPID); err != nil {
		t.Fatalf("holder pid: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT backend_start FROM pg_stat_activity
		WHERE pid = $1`, bc.holderPID).Scan(&bc.holderStart); err != nil {
		t.Fatalf("holder backend_start: %v", err)
	}
	return holder
}

func waitForWaiters(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, want int,
) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock'`,
		).Scan(&n); err != nil {
			t.Fatalf("count waiters: %v", err)
		}
		if n >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("blocking chain did not form %d waiters", want)
}

func chainProbeConfig() *config.Config {
	cfg := testConfig()
	cfg.Analyzer.LockChain = config.LockChainConfig{
		Enabled: true, MinBlockedThreshold: 1, CriticalBlockedThreshold: 10,
		IdleInTxTerminateMinutes: 5, ActiveQueryCancelMinutes: 15,
	}
	return cfg
}

func realProbe(pool *pgxpool.Pool, cfg *config.Config) LockChainProbe {
	return func(ctx context.Context) ([]analyzer.Finding, error) {
		return analyzer.ProbeLockChains(ctx, pool, cfg)
	}
}

type openRow struct {
	id       string
	chain    []ChainLink
	lastSeen time.Time
}

func openLockRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, db string,
) []openRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT id::text, causal_chain,
		last_detected_at FROM sage.incidents
		WHERE database_name = $1 AND resolved_at IS NULL
		  AND signal_ids = ARRAY['lock_contention']`, db)
	if err != nil {
		t.Fatalf("query incidents: %v", err)
	}
	defer rows.Close()
	var out []openRow
	for rows.Next() {
		var r openRow
		var raw []byte
		if err := rows.Scan(&r.id, &raw, &r.lastSeen); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if err := json.Unmarshal(raw, &r.chain); err != nil {
			t.Fatalf("causal_chain: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func findBlocker(chain []ChainLink, pid int) *BlockerIdentity {
	for _, l := range chain {
		if l.Blocker != nil && l.Blocker.PID == pid {
			return l.Blocker
		}
	}
	return nil
}

func TestFastPathDB_RealChainOpensIncidentAndNotifies(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	chain := startBlockingChain(t, ctx, pool)
	cfg := chainProbeConfig()
	lt := NewLockChainTicker(eng, pool, realProbe(pool, cfg), time.Minute,
		noopTestLog)

	if err := lt.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	rows := openLockRows(t, ctx, pool, db)
	assertHolderIdentity(t, rows, chain)
	det := rec.byType("incident_detected")
	if len(det) != 1 || det[0].Data["database"] != db ||
		det[0].Data["incident_id"] != rows[0].id {
		t.Fatalf("incident_detected events = %+v", det)
	}

	time.Sleep(10 * time.Millisecond)
	if err := lt.Tick(ctx); err != nil {
		t.Fatalf("second Tick: %v", err)
	}
	again := openLockRows(t, ctx, pool, db)
	if len(again) != 1 || again[0].id != rows[0].id {
		t.Fatalf("second tick changed incident identity: %+v", again)
	}
	if !again[0].lastSeen.After(rows[0].lastSeen) {
		t.Fatalf("last_detected_at not advanced by the second tick")
	}
	if n := len(rec.byType("incident_detected")); n != 1 {
		t.Fatalf("incident_detected sent %d times, want once", n)
	}

	chain.release()
	resolveAfterRelease(t, ctx, eng, pool, db, rec)
}

func assertHolderIdentity(t *testing.T, rows []openRow, chain *blockingChain) {
	t.Helper()
	if len(rows) != 1 {
		t.Fatalf("open lock incidents = %d, want 1", len(rows))
	}
	b := findBlocker(rows[0].chain, chain.holderPID)
	if b == nil {
		t.Fatalf("holder pid %d not in persisted chain %+v",
			chain.holderPID, rows[0].chain)
	}
	if !b.BackendStart.Equal(chain.holderStart) ||
		b.State != "idle in transaction" || b.TotalBlocked != 2 ||
		b.ChainDepth != 2 || b.QuerySHA == "" {
		t.Fatalf("persisted identity = %+v, want backend_start %s, idle in "+
			"transaction, 2 blocked, depth 2", b, chain.holderStart)
	}
}

// resolveAfterRelease runs analyzer cycles until the released chain's
// incident auto-resolves, and expects exactly one incident_resolved.
func resolveAfterRelease(
	t *testing.T, ctx context.Context, eng *Engine, pool *pgxpool.Pool,
	db string, rec *recordingDispatcher,
) {
	t.Helper()
	for i := 0; i < 6 && len(openLockRows(t, ctx, pool, db)) > 0; i++ {
		eng.AnalyzeContext(ctx, quietSnapshot(), nil, chainProbeConfig(), nil)
		if err := eng.PersistIncidents(ctx, pool); err != nil {
			t.Fatalf("persist: %v", err)
		}
	}
	if n := len(openLockRows(t, ctx, pool, db)); n != 0 {
		t.Fatalf("incident still open after the chain was released")
	}
	if n := len(rec.byType("incident_resolved")); n != 1 {
		t.Fatalf("incident_resolved sent %d times, want 1", n)
	}
}

func TestFastPathDB_NoChainWritesNothing(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	lt := NewLockChainTicker(eng, pool,
		realProbe(pool, chainProbeConfig()), time.Minute, noopTestLog)
	if err := lt.Tick(ctx); err != nil {
		t.Fatalf("Tick: %v", err)
	}
	if n := len(openLockRows(t, ctx, pool, db)); n != 0 {
		t.Fatalf("quiet database produced %d incidents", n)
	}
	if len(rec.events) != 0 {
		t.Fatalf("quiet database sent %d events", len(rec.events))
	}
}

func TestFastPathDB_ProbeErrorPropagates(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	boom := errors.New("lock chain query: permission denied for pg_locks")
	lt := NewLockChainTicker(eng, pool,
		func(context.Context) ([]analyzer.Finding, error) { return nil, boom },
		time.Minute, noopTestLog)
	err := lt.Tick(ctx)
	if !errors.Is(err, boom) || !strings.Contains(err.Error(), "probe") {
		t.Fatalf("Tick error = %v, want wrapped probe error", err)
	}
	if n := len(openLockRows(t, ctx, pool, db)); n != 0 {
		t.Fatalf("failed probe wrote %d incidents", n)
	}
}

func TestFastPathDB_ProbeDisabledByConfig(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	cfg := chainProbeConfig()
	cfg.Analyzer.LockChain.Enabled = false
	startBlockingChain(t, ctx, pool)
	got, err := analyzer.ProbeLockChains(ctx, pool, cfg)
	if err != nil || got != nil {
		t.Fatalf("disabled probe = %v, %v; want nil, nil", got, err)
	}
}

// Concurrent access: two fast ticks and the analyzer cycle race on the
// same incident against the real store; exactly one row and one
// incident_detected event result.
func TestFastPathDB_ConcurrentTicksAndAnalyzer(t *testing.T) {
	pool, ctx := lifecycleDB(t)
	eng, db := lifecycleEngine(t, pool)
	rec := &recordingDispatcher{}
	eng.WithDispatcher(rec)
	startBlockingChain(t, ctx, pool)
	cfg := chainProbeConfig()
	probe := realProbe(pool, cfg)
	lt := NewLockChainTicker(eng, pool, probe, time.Minute, noopTestLog)
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("hydrate: %v", err)
	}

	for _, err := range raceTicksAndAnalyzer(ctx, eng, lt, pool, cfg, probe) {
		t.Error(err)
	}
	if n := len(openLockRows(t, ctx, pool, db)); n != 1 {
		t.Fatalf("open lock incidents = %d, want 1", n)
	}
	if n := len(rec.byType("incident_detected")); n != 1 {
		t.Fatalf("incident_detected sent %d times, want 1", n)
	}
}

// raceTicksAndAnalyzer runs two fast-path tickers and one analyzer loop
// concurrently (5 iterations each) and returns every error.
func raceTicksAndAnalyzer(
	ctx context.Context, eng *Engine, lt *LockChainTicker,
	pool *pgxpool.Pool, cfg *config.Config, probe LockChainProbe,
) []error {
	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				if err := lt.Tick(ctx); err != nil {
					errs <- fmt.Errorf("tick: %w", err)
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 5; i++ {
			f, err := probe(ctx)
			if err != nil {
				errs <- err
				continue
			}
			eng.AnalyzeContext(ctx, quietSnapshot(), nil, cfg, f)
			if err := eng.PersistIncidents(ctx, pool); err != nil {
				errs <- fmt.Errorf("persist: %w", err)
			}
		}
	}()
	wg.Wait()
	close(errs)
	var out []error
	for err := range errs {
		out = append(out, err)
	}
	return out
}
