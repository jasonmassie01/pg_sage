package api

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// collection-01: /snapshots/latest?metric=locks serves the bounded wait
// graph the collector stored, whose statement text never holds a literal.

// No concurrent access test: the handler reads one stored row; the
// collector's writes are covered in internal/collector.

const locksSecret = "secret-123"

func locksAPIPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	ctx := context.Background()
	dsn := testdb.CreateDatabase(t, "api_locks")
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, dsn
}

// holdSecretTx leaves a session idle in transaction after a statement
// whose literal is the secret, and returns its pid.
func holdSecretTx(t *testing.T, dsn string) int {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	var pid int
	if err := conn.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	for _, s := range []string{"CREATE TABLE IF NOT EXISTS public.acct (id text)",
		"BEGIN", "SELECT count(*) FROM public.acct WHERE id = '" + locksSecret + "'"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	return pid
}

// runOneCycle runs the collector until it has stored a locks row.
func runOneCycle(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		Collector: config.CollectorConfig{IntervalSeconds: 60, BatchSize: 100, MaxQueries: 50},
		Safety: config.SafetyConfig{CPUCeilingPct: 90, BackoffConsecutiveSkips: 5,
			DormantIntervalSeconds: 600},
	}
	c := collector.New(pool, cfg, version, func(string, string, ...any) {})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); c.Run(ctx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.snapshots
			WHERE category = 'locks'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the collector stored no locks row within 60 s")
}

func TestSnapshotLatestLocks_NeverServesTheLiteral(t *testing.T) {
	pool, dsn := locksAPIPool(t)
	holder := holdSecretTx(t, dsn)
	runOneCycle(t, pool)

	req := httptest.NewRequest("GET",
		"/api/v1/snapshots/latest?database=testdb&metric=locks", nil)
	w := httptest.NewRecorder()
	snapshotLatestHandler(phase2MgrWithPool(pool)).ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, locksSecret) {
		t.Fatalf("locks response holds the literal %q: %s", locksSecret, body)
	}
	var resp struct {
		Snapshot []collector.LockInfo `json:"snapshot"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var found *collector.LockInfo
	for i := range resp.Snapshot {
		if resp.Snapshot[i].PID == holder {
			found = &resp.Snapshot[i]
		}
	}
	if found == nil || found.Query == nil ||
		!strings.Contains(*found.Query, "WHERE id = '?'") || found.XactStart == nil {
		t.Fatalf("holder %d row = %+v, want it with its redacted statement", holder, found)
	}
}
