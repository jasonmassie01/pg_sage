package selfcost

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/catalogread"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// pgssDatabase is an extra database whose pg_stat_statements is the given
// version, with pg_sage's schema bootstrapped.
func pgssDatabase(t *testing.T, version string) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.CreateDatabase(t, "selfcost_pgss")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	var available bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_available_extension_versions
		WHERE name = 'pg_stat_statements' AND version = $1)`, version).Scan(&available); err != nil {
		t.Fatalf("available versions: %v", err)
	}
	if !available {
		t.Skipf("pg_stat_statements %s is not shipped with this server", version)
	}
	for _, s := range []string{"DROP EXTENSION IF EXISTS pg_stat_statements",
		"CREATE EXTENSION pg_stat_statements VERSION '" + version + "'"} {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool, ctx
}

func boundedReader(pool *pgxpool.Pool, lock time.Duration) catalogread.Reader {
	return catalogread.New(pool, catalogread.Timeouts{Statement: 20 * time.Second, Lock: lock})
}

// pg_stat_statements 1.8 (an upgraded cluster that never ran ALTER
// EXTENSION ... UPDATE) has no toplevel column: the reading has the schema
// counters and the statements, and no error.
func TestRead_PGSS18KeepsSchemaAndStatements(t *testing.T) {
	pool, ctx := pgssDatabase(t, "1.8")
	r, err := Read(ctx, boundedReader(pool, time.Second))
	if err != nil {
		t.Fatalf("Read on pg_stat_statements 1.8: %v", err)
	}
	if r.SchemaBytes <= 0 || r.Database == "" || r.StatementsErr != nil || !r.StatementsKnown {
		t.Fatalf("1.8 reading = %+v, want schema counters and statements", r)
	}
	for k := range r.Statements {
		if k.TopLevel {
			t.Fatalf("1.8 entry with toplevel true: %+v", k)
		}
	}
}

// pg_stat_statements 1.7 has no total_exec_time: statements are unknown,
// the schema counters stay, there is no error and the update is advised.
func TestRead_PGSS17StatementsUnknownSchemaKept(t *testing.T) {
	pool, ctx := pgssDatabase(t, "1.7")
	logs := &noticeLog{}
	s := NewSampler(logs.log)
	q := boundedReader(pool, time.Second)
	sc, err := ReadSchema(ctx, q)
	if err != nil {
		t.Fatalf("ReadSchema: %v", err)
	}
	r := s.Sample(ctx, q, sc)
	if r.StatementsKnown || r.StatementsErr != nil || r.SchemaBytes <= 0 || r.RowsWritten < 0 {
		t.Fatalf("1.7 reading = %+v, want schema counters, statements unknown", r)
	}
	s.Sample(ctx, q, sc)
	if n := logs.matching("ALTER EXTENSION pg_stat_statements UPDATE"); n != 1 {
		t.Fatalf("update notice logged %d times, want once: %v", n, logs.lines)
	}
}

// Error propagation: a sage table held under AccessExclusiveLock (a
// migration, a retention job) cuts the read off at lock_timeout instead of
// stalling the cycle.
func TestRead_LockedSageTableTimesOut(t *testing.T) {
	pool, ctx := livePool(t)
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(ctx,
		"LOCK TABLE sage.health_history IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	lock := 500 * time.Millisecond
	start := time.Now()
	_, err = Read(ctx, boundedReader(pool, lock))
	elapsed := time.Since(start)
	var pgErr *pgconn.PgError
	if err == nil || !strings.Contains(err.Error(), "read sage schema") ||
		!errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("Read under a held lock: err %v, want a 55P03 lock timeout", err)
	}
	if elapsed > lock+time.Second {
		t.Fatalf("Read took %v, want under lock_timeout %v + 1 s", elapsed, lock)
	}
}

// textCounter is a bounded reader that counts the statements that load
// pg_stat_statements' query texts.
func textCounter(pool *pgxpool.Pool) (catalogread.Reader, *atomic.Int32) {
	n := &atomic.Int32{}
	r := boundedReader(pool, 5*time.Second)
	r.After = func(_ context.Context, _ pgx.Tx, sql string, _ []any) {
		if strings.Contains(sql, "pg_stat_statements(true)") {
			n.Add(1)
		}
	}
	return r, n
}

// settle samples until a sample reads no texts: pg_sage's own reads are
// new pg_stat_statements entries the first time they run.
func settle(t *testing.T, ctx context.Context, s *Sampler, q catalogread.Reader,
	n *atomic.Int32) {
	t.Helper()
	for i := 0; i < 8; i++ {
		before := n.Load()
		if _, _, err := s.Statements(ctx, q); err != nil {
			t.Fatalf("Statements: %v", err)
		}
		if n.Load() == before {
			return
		}
	}
	t.Fatal("the key cache never settled: every sample read the query texts")
}

// Performance: with no new keys a sample does not load the query texts;
// a new tagged statement makes exactly one sample load them, once.
func TestSampler_LiveNoTextReadWithoutNewKeys(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewSampler(nil)
	q, n := textCounter(pool)
	settle(t, ctx, s, q, n)
	before := n.Load()
	if _, _, err := s.Statements(ctx, q); err != nil || n.Load() != before {
		t.Fatalf("settled sample read texts (%d -> %d), err %v", before, n.Load(), err)
	}
	runAsSage(t, ctx, "SELECT 41 AS selfcost_new_key, 'probe'::text AS new_key_shape")
	got, known, err := s.Statements(ctx, q)
	if err != nil || !known || n.Load() != before+1 {
		t.Fatalf("new tagged key: %d text reads (want 1), known=%v err=%v",
			n.Load()-before, known, err)
	}
	if !hasText(got, "selfcost_new_key") {
		t.Fatalf("the new tagged statement is not in the sample: %d entries", len(got))
	}
}

// State transition: after pg_stat_statements_reset() the cache is read
// again and the window counts the re-created entry from zero.
func TestSampler_LiveResetRevalidates(t *testing.T) {
	// The cache revalidates on the stats_reset epoch, which only a reset of
	// every database moves: that reset runs on the exclusive server.
	dsn := testdb.Exclusive(t, "the pg_stat_statements stats_reset epoch")
	pool, ctx := livePoolOn(t, dsn)
	s := NewSampler(nil)
	q, n := textCounter(pool)
	probe := "SELECT 43 AS selfcost_reset_probe, 'probe'::text AS reset_shape"
	runAsSageOn(t, ctx, dsn, probe, probe, probe)
	settle(t, ctx, s, q, n)
	sc, err := ReadSchema(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	prev := s.Sample(ctx, q, sc)
	if _, err := pool.Exec(ctx, "SELECT pg_stat_statements_reset()"); err != nil {
		t.Fatal(err)
	}
	runAsSageOn(t, ctx, dsn, probe)
	before := n.Load()
	cur := s.Sample(ctx, q, sc)
	if !cur.StatementsKnown || n.Load() <= before {
		t.Fatalf("after reset: known=%v, %d text reads, want the cache read again",
			cur.StatementsKnown, n.Load()-before)
	}
	top := TopStatements(prev.Statements, cur.Statements, 50)
	for _, st := range top {
		if strings.Contains(st.Text, "selfcost_reset_probe") {
			if st.Calls != 1 {
				t.Fatalf("probe after reset counted %d calls, want 1 (from zero)", st.Calls)
			}
			return
		}
	}
	t.Fatalf("the probe run after the reset is not in the window: %+v", top)
}

// Nil/empty: a database whose sage schema has no tables measures 0 bytes
// and a reading that is still usable.
func TestReadSchema_LiveEmptySchema(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "selfcost_empty")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "CREATE SCHEMA sage"); err != nil {
		t.Fatal(err)
	}
	q := boundedReader(pool, time.Second)
	first, err := Read(ctx, q)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if first.SchemaBytes != 0 || first.Database == "" || first.RowsRead != 0 {
		t.Fatalf("empty sage schema reading = %+v", first)
	}
	time.Sleep(10 * time.Millisecond)
	second, err := Read(ctx, q)
	if err != nil {
		t.Fatal(err)
	}
	if c := Between(first, second, time.Minute); !c.Known || c.SchemaBytes != 0 {
		t.Fatalf("cost between empty readings = %+v, want known with 0 bytes", c)
	}
}

func hasText(m map[StatementKey]StatementCounters, marker string) bool {
	for _, c := range m {
		if strings.Contains(c.Text, marker) {
			return true
		}
	}
	return false
}

// Nil: pg_stat_statements not installed in the database leaves the
// statements unknown with no error and the schema counters measured.
func TestRead_LiveWithoutExtension(t *testing.T) {
	dsn := testdb.CreateDatabase(t, "selfcost_nopgss")
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if _, err := pool.Exec(ctx, "DROP EXTENSION IF EXISTS pg_stat_statements"); err != nil {
		t.Fatal(err)
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	r, err := Read(ctx, boundedReader(pool, time.Second))
	if err != nil || r.StatementsKnown || r.StatementsErr != nil || r.SchemaBytes <= 0 {
		t.Fatalf("without the extension: reading %+v, err %v", r, err)
	}
}
