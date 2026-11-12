//go:build integration

package logwatch

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// rca-07 on real PostgreSQL: a deadlock between two sessions becomes an
// incident whose stored evidence names both PIDs and counts the event.

type deadlockResult struct {
	victim, other int
	err           *pgconn.PgError
}

func connectWithPID(ctx context.Context, t *testing.T, url string) (*pgx.Conn, int) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatalf("backend pid: %v", err)
	}
	if _, err := conn.Exec(ctx, "SET deadlock_timeout = '100ms'"); err != nil {
		t.Fatalf("set deadlock_timeout: %v", err)
	}
	return conn, pid
}

// provokeDeadlock makes two sessions wait on each other's row lock and
// returns the victim's error as PostgreSQL reported it.
func provokeDeadlock(ctx context.Context, t *testing.T, url, table string) deadlockResult {
	t.Helper()
	a, pidA := connectWithPID(ctx, t, url)
	b, pidB := connectWithPID(ctx, t, url)
	ident := pgx.Identifier{table}.Sanitize()
	setup := "CREATE TABLE IF NOT EXISTS " + ident + " (id int PRIMARY KEY, v int);" +
		"INSERT INTO " + ident + " VALUES (1, 0), (2, 0) ON CONFLICT DO NOTHING"
	if _, err := a.Exec(ctx, setup); err != nil {
		t.Fatalf("create %s: %v", table, err)
	}
	t.Cleanup(func() { _, _ = a.Exec(context.Background(), "DROP TABLE IF EXISTS "+ident) })
	update := "UPDATE " + ident + " SET v = v + 1 WHERE id = $1"
	for _, step := range []struct {
		c  *pgx.Conn
		id int
	}{{a, 1}, {b, 2}} {
		if _, err := step.c.Exec(ctx, "BEGIN"); err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := step.c.Exec(ctx, update, step.id); err != nil {
			t.Fatalf("first update: %v", err)
		}
	}
	errA := make(chan error, 1)
	go func() { _, err := a.Exec(ctx, update, 2); errA <- err }()
	time.Sleep(200 * time.Millisecond) // a now waits on b's row
	_, errB := b.Exec(ctx, update, 1)
	gotA := <-errA
	for _, c := range []*pgx.Conn{a, b} {
		_, _ = c.Exec(ctx, "ROLLBACK")
	}
	var pgErr *pgconn.PgError
	switch {
	case errors.As(errB, &pgErr):
		return deadlockResult{victim: pidB, other: pidA, err: pgErr}
	case errors.As(gotA, &pgErr):
		return deadlockResult{victim: pidA, other: pidB, err: pgErr}
	}
	t.Fatalf("no deadlock error: a=%v b=%v", gotA, errB)
	return deadlockResult{}
}

// writeCSVLogRecord writes the victim's error as a PG14+ csvlog record.
func writeCSVLogRecord(t *testing.T, dir, datname string, d deadlockResult) {
	t.Helper()
	rec := make([]string, csvExtColumns)
	rec[csvColTime] = time.Now().UTC().Format("2006-01-02 15:04:05.000") + " UTC"
	rec[csvColUser], rec[csvColDatabase] = "postgres", datname
	rec[csvColPID] = strconv.Itoa(d.victim)
	rec[csvColSeverity], rec[csvColSQLState] = "ERROR", d.err.Code
	rec[csvColMessage], rec[csvColDetail] = d.err.Message, d.err.Detail
	rec[csvColApplication], rec[csvColBackendType] = "rca07_it", "client backend"
	f, err := os.Create(filepath.Join(dir, "postgresql.csv"))
	if err != nil {
		t.Fatal(err)
	}
	w := csv.NewWriter(f)
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}
	w.Flush()
	if err := errors.Join(w.Error(), f.Close()); err != nil {
		t.Fatal(err)
	}
}

func bootstrapPool(ctx context.Context, t *testing.T, url string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

// CHECK-04: a real deadlock's csvlog record opens one incident whose stored
// evidence names both PIDs and whose log_occurrences count is at least 1.
func TestIntegration_RealDeadlockStoresParticipantsAndCount(t *testing.T) {
	url := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pool := bootstrapPool(ctx, t, url)
	var datname string
	if err := pool.QueryRow(ctx, "SELECT current_database()").Scan(&datname); err != nil {
		t.Fatal(err)
	}
	d := provokeDeadlock(ctx, t, url, "rca07_deadlock")
	if d.err.Code != "40P01" {
		t.Fatalf("victim error = %s %s, want 40P01", d.err.Code, d.err.Message)
	}
	dir := t.TempDir()
	writeCSVLogRecord(t, dir, datname, d)

	eng := newCrashEngine()
	name := fmt.Sprintf("rca07_it_%d", time.Now().UnixNano())
	eng.WithDatabaseName(name)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.incidents WHERE database_name = $1", name)
	})
	if err := eng.Hydrate(ctx, pool); err != nil {
		t.Fatalf("hydrate: %v", err)
	}
	fw := NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "csvlog",
		PollIntervalMs: 100, DedupWindowS: 60, MaxLinesPerCycle: 1000}, nopLog)
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("start watcher: %v", err)
	}
	defer fw.Stop()
	eng.SetLogSource(fw)
	cfg := &config.Config{RCA: config.RCAConfig{Enabled: true}}
	eng.AnalyzeContext(ctx, &collector.Snapshot{CollectedAt: time.Now()}, nil, cfg, nil)
	if err := eng.PersistIncidents(ctx, pool); err != nil {
		t.Fatalf("persist: %v", err)
	}
	assertStoredDeadlock(ctx, t, pool, name, d)
}

func assertStoredDeadlock(
	ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string, d deadlockResult,
) {
	t.Helper()
	var chain string
	var count *int
	err := pool.QueryRow(ctx, `SELECT causal_chain::text,
		(log_occurrences->>'count')::int FROM sage.incidents
		WHERE database_name = $1 AND 'log_deadlock_detected' = ANY(signal_ids)`,
		name).Scan(&chain, &count)
	if err != nil {
		t.Fatalf("stored deadlock incident: %v", err)
	}
	for _, pid := range []int{d.victim, d.other} {
		if !strings.Contains(chain, strconv.Itoa(pid)) {
			t.Errorf("evidence %s does not name pid %d", chain, pid)
		}
	}
	if count == nil || *count < 1 {
		t.Errorf("log_occurrences count = %v, want >= 1", count)
	}
}

// The same deadlock read from the server's own csvlog, when a csvlog
// server with a locally mounted log directory is available.
func TestIntegration_ServerCSVLogDeadlockNamesBothPIDs(t *testing.T) {
	url, _, dir := crashServerEnv(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	fw := NewFileWatcher(config.LogWatchConfig{LogDirectory: dir, Format: "csvlog",
		PollIntervalMs: 200, DedupWindowS: 1}, nopLog)
	if err := fw.Start(ctx); err != nil {
		t.Fatalf("start watcher on %s: %v", dir, err)
	}
	defer fw.Stop()
	discardBacklog(fw)
	d := provokeDeadlock(ctx, t, url, "rca07_deadlock_csvlog")
	for ctx.Err() == nil {
		for _, s := range fw.Drain() {
			pids, _ := s.Metrics["affected_pids"].([]int)
			if s.ID == "log_deadlock_detected" && slices.Contains(pids, d.victim) &&
				slices.Contains(pids, d.other) {
				return
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("no deadlock signal naming pids %d and %d", d.victim, d.other)
}
