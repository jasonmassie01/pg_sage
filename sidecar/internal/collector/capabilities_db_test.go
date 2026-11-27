package collector

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testsupport/pgssepoch"
)

// collection-02 on a live server: the collector re-derives what the
// monitored database can report each cycle, so pg_stat_statements created
// after startup, dropped and re-created, or a major upgrade behind the same
// endpoint is picked up without a restart.

// capsDatabase creates a database from template0 (no pg_stat_statements
// extension) with track_io_timing on, and returns a one-connection pool on
// it: the temp-table workload and the collector share that session.
func capsDatabase(t *testing.T) *pgxpool.Pool {
	t.Helper()
	admin := testPool(t)
	ctx := context.Background()
	name := fmt.Sprintf("sage_caps_%d_%d", os.Getpid(), fixtureSeq.Add(1))
	ident := pgx.Identifier{name}.Sanitize()
	mustExec(t, admin, "CREATE DATABASE "+ident+" TEMPLATE template0")
	mustExec(t, admin, "ALTER DATABASE "+ident+" SET track_io_timing = on")
	pcfg := admin.Config().Copy()
	pcfg.ConnConfig.Database = name
	pcfg.MaxConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatalf("pool on %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+ident+
			" WITH (FORCE)"); err != nil {
			t.Logf("drop %s: %v", name, err)
		}
	})
	return pool
}

// capsWorkload reads a temp table bigger than temp_buffers (local block
// reads, timed with track_io_timing) and writes a regular table (WAL). It
// returns the markers of the read and the write statements.
func capsWorkload(t *testing.T, pool *pgxpool.Pool) (read, write string) {
	t.Helper()
	mustExec(t, pool, `SET temp_buffers = '800kB'`)
	n := fixtureSeq.Add(1)
	tmp := fmt.Sprintf("caps_tmp_%d", n)
	mustExec(t, pool, `CREATE TEMP TABLE `+tmp+` AS
		SELECT g AS id, repeat('x', 200) AS pad FROM generate_series(1, 60000) g`)
	for i := 0; i < 3; i++ {
		mustExec(t, pool, `SELECT count(*) FROM `+tmp+` WHERE pad <> ''`)
	}
	wal := fmt.Sprintf("caps_wal_%d", n)
	mustExec(t, pool, `CREATE TABLE `+wal+` (id int, pad text)`)
	mustExec(t, pool, `INSERT INTO `+wal+` SELECT g, repeat('y', 100)
		FROM generate_series(1, 2000) g`)
	return "FROM " + tmp + " WHERE pad", "INSERT INTO " + wal
}

// queryCountersMarker is in the queries step's counters read only (its text
// read is statementTextsTag).
const queryCountersMarker = "sum(calls)::bigint AS calls"

// statsSQLRecorder keeps the pg_stat_statements counters read of each cycle.
type statsSQLRecorder struct {
	mu   sync.Mutex
	sqls []string
}

func (r *statsSQLRecorder) hook(_ context.Context, _ pgx.Tx, sql string, _ []any) {
	if !strings.Contains(sql, queryCountersMarker) {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sqls = append(r.sqls, sql)
}

func (r *statsSQLRecorder) take() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.sqls
	r.sqls = nil
	return out
}

// fullColumnProblems checks one cycle collected every column the installed
// pg_stat_statements exposes: WAL and plan time selected, the write's WAL
// bytes and the read's block read time non-zero.
func fullColumnProblems(snap *Snapshot, sqls []string, read, write string,
	version int) []string {
	if !snap.Available("queries") {
		return []string{"queries unavailable: " + snap.Unavailable["queries"]}
	}
	var out []string
	if len(sqls) != 1 {
		return []string{fmt.Sprintf("%d pg_stat_statements reads this cycle, want 1",
			len(sqls))}
	}
	for _, col := range []string{"wal_bytes", "total_plan_time"} {
		if !strings.Contains(sqls[0], col) {
			out = append(out, "statements read does not select "+col)
		}
	}
	var sawRead, sawWrite bool
	for _, q := range snap.Queries {
		switch {
		case strings.Contains(q.Query, read):
			sawRead = true
			// Measured: PG16 does not time local-buffer reads.
			if q.BlkReadTime <= 0 && (version < 160000 || version >= 170000) {
				out = append(out, fmt.Sprintf("read BlkReadTime = %v, want > 0",
					q.BlkReadTime))
			}
		case strings.Contains(q.Query, write):
			sawWrite = true
			if q.WALBytes <= 0 {
				out = append(out, fmt.Sprintf("write WALBytes = %d, want > 0", q.WALBytes))
			}
		}
	}
	if !sawRead || !sawWrite {
		out = append(out, fmt.Sprintf("workload not collected (read %v, write %v)",
			sawRead, sawWrite))
	}
	return out
}

// collectFullColumns runs the workload and one cycle, repeating when another
// package reset pg_stat_statements in between.
func collectFullColumns(t *testing.T, c *Collector, pool *pgxpool.Pool,
	rec *statsSQLRecorder) {
	t.Helper()
	ctx := context.Background()
	version := serverVersion(t, pool)
	pgssepoch.Attempt(t, ctx, pool, 3, func() []string {
		read, write := capsWorkload(t, pool)
		rec.take()
		snap, err := c.collect(ctx)
		if err != nil {
			return []string{"collect: " + err.Error()}
		}
		return fullColumnProblems(snap, rec.take(), read, write, version)
	})
}

// CHECK-21: degraded at startup (no extension), CREATE EXTENSION, and the
// next cycle selects WAL and plan time with non-zero block read time, with
// no restart; one INFO line says what is now collected.
func TestCapabilities_ExtensionCreatedWithoutRestart(t *testing.T) {
	pool := capsDatabase(t)
	ctx := context.Background()
	cfg := testConfig()
	cfg.HasWALColumns, cfg.HasPlanTimeColumns = false, false
	logs := &logRecorder{}
	c := New(pool, cfg, serverVersion(t, pool), logs.log)
	rec := &statsSQLRecorder{}
	c.onCatalogQuery = rec.hook
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect without the extension: %v", err)
	}
	if snap.Available("queries") {
		t.Fatal("queries available without pg_stat_statements, want unavailable")
	}
	if !strings.Contains(snap.Unavailable["queries"], "pg_stat_statements") {
		t.Fatalf("Unavailable[queries] = %q, want the missing view", snap.Unavailable["queries"])
	}
	mustExec(t, pool, "CREATE EXTENSION pg_stat_statements")
	collectFullColumns(t, c, pool, rec)
	if !logs.has("INFO", "now collected") {
		t.Errorf("no INFO naming the new columns; logs: %v", logs.lines)
	}
}

// DROP EXTENSION mid-run makes queries unavailable with a plain read (a
// failed statement never reaches the catalog hook, so the template is
// read from the capabilities);
// re-creating it restores the full columns without a restart.
func TestCapabilities_DropAndRecreateExtension(t *testing.T) {
	pool := capsDatabase(t)
	ctx := context.Background()
	mustExec(t, pool, "CREATE EXTENSION pg_stat_statements")
	cfg := testConfig()
	c := New(pool, cfg, serverVersion(t, pool), noopLog)
	rec := &statsSQLRecorder{}
	c.onCatalogQuery = rec.hook
	collectFullColumns(t, c, pool, rec)

	mustExec(t, pool, "DROP EXTENSION pg_stat_statements")
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect after DROP EXTENSION: %v", err)
	}
	if snap.Available("queries") {
		t.Fatal("queries available after DROP EXTENSION, want unavailable")
	}
	if !strings.Contains(snap.Unavailable["queries"], "pg_stat_statements") {
		t.Fatalf("Unavailable[queries] = %q, want the missing view", snap.Unavailable["queries"])
	}
	if caps := c.Capabilities(); caps.WALColumns || caps.PlanTimeColumns {
		t.Fatalf("capabilities after DROP EXTENSION = %+v, want the plain read", caps)
	}

	mustExec(t, pool, "CREATE EXTENSION pg_stat_statements")
	collectFullColumns(t, c, pool, rec)
}

// probeCounter counts the capability probe statements.
type probeCounter struct {
	mu          sync.Mutex
	key, column int
}

func (p *probeCounter) hook(_ context.Context, _ pgx.Tx, sql string, _ []any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if strings.Contains(sql, "capabilities: key") {
		p.key++
	}
	if strings.Contains(sql, "capabilities: columns") {
		p.column++
	}
}

func (p *probeCounter) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.key, p.column
}

// CHECK-22: with an unchanged key a steady-state cycle issues exactly one
// probe statement; the column probe runs once, on the first cycle.
func TestCapabilities_SteadyStateOneProbeStatement(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	c := New(pool, testConfig(), serverVersion(t, pool), noopLog)
	counter := &probeCounter{}
	c.onCatalogQuery = counter.hook
	for i := 1; i <= 3; i++ {
		if _, err := c.collect(ctx); err != nil {
			t.Fatalf("cycle %d: %v", i, err)
		}
		key, column := counter.counts()
		if key != i || column != 1 {
			t.Fatalf("after cycle %d: %d key and %d column probes, want %d and 1",
				i, key, column, i)
		}
	}
}

// checkpointed issues a CHECKPOINT so the checkpoint counters are non-zero.
func checkpointed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, pool, "CHECKPOINT")
}

// CHECK-23 (PG16 to PG17): a collector built for PG16 against a PG17
// server still produces system stats, both through a cycle and through the
// system read alone (the 42703 fallback).
func TestCapabilities_StalePG16VersionOnPG17(t *testing.T) {
	pool := exclusivePool(t, "checkpoint counters after a forced CHECKPOINT")
	if v := serverVersion(t, pool); v < 170000 {
		t.Skipf("needs a PG17+ server, have %d", v)
	}
	checkpointed(t, pool)
	direct := New(pool, testConfig(), 160000, noopLog)
	s, err := direct.collectSystem(context.Background())
	if err != nil {
		t.Fatalf("system read with a PG16 collector on PG17: %v", err)
	}
	if s.TotalCheckpoints <= 0 || s.MaxConnections <= 0 {
		t.Fatalf("system stats = %+v, want checkpoints and max_connections", s)
	}
	snap, err := New(pool, testConfig(), 160000, noopLog).collect(context.Background())
	if err != nil {
		t.Fatalf("cycle with a PG16 collector on PG17: %v", err)
	}
	if snap.System.TotalCheckpoints <= 0 {
		t.Fatalf("total_checkpoints = %d, want > 0", snap.System.TotalCheckpoints)
	}
}

// The pg_stat_io gate follows the live version: a collector seeded with a
// pre-16 version reads pg_stat_io on a PG16+ server.
func TestCapabilities_IOGateFollowsLiveVersion(t *testing.T) {
	pool := testPool(t)
	if v := serverVersion(t, pool); v < 160000 {
		t.Skipf("needs a PG16+ server (pg_stat_io), have %d", v)
	}
	snap, err := New(pool, testConfig(), 150000, noopLog).collect(context.Background())
	if err != nil {
		t.Fatalf("cycle with a collector seeded with 150000: %v", err)
	}
	if len(snap.IO) == 0 || !snap.Available("io") {
		t.Fatalf("pg_stat_io rows = %d (unavailable %q), want them read",
			len(snap.IO), snap.Unavailable["io"])
	}
}

// CHECK-23 (PG17 to PG14): a collector built for PG17 against an older
// server falls back to the pre-17 system SQL.
func TestCapabilities_StalePG17VersionOnPG14(t *testing.T) {
	pool := exclusivePool(t, "checkpoint counters after a forced CHECKPOINT")
	if v := serverVersion(t, pool); v >= 170000 {
		t.Skipf("needs a pre-17 server, have %d", v)
	}
	checkpointed(t, pool)
	s, err := New(pool, testConfig(), 170000, noopLog).collectSystem(context.Background())
	if err != nil {
		t.Fatalf("system read with a PG17 collector on a pre-17 server: %v", err)
	}
	if s.TotalCheckpoints <= 0 {
		t.Fatalf("total_checkpoints = %d, want > 0", s.TotalCheckpoints)
	}
	snap, err := New(pool, testConfig(), 170000, noopLog).collect(context.Background())
	if err != nil {
		t.Fatalf("cycle with a PG17 collector on a pre-17 server: %v", err)
	}
	if snap.System.TotalCheckpoints <= 0 {
		t.Fatalf("total_checkpoints = %d, want > 0", snap.System.TotalCheckpoints)
	}
}
