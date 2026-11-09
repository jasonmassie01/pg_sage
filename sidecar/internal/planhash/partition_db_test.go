package planhash

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

func livePool(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, ctx
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// dailyTable creates schema.phx_events partitioned by day with one
// partition per day and a partitioned index on ts.
func dailyTable(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema string,
	days ...string) {
	t.Helper()
	mustExec(t, ctx, pool, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	mustExec(t, ctx, pool, "CREATE SCHEMA "+schema)
	mustExec(t, ctx, pool, "CREATE TABLE "+schema+
		".phx_events (id int, ts date NOT NULL) PARTITION BY RANGE (ts)")
	mustExec(t, ctx, pool, "CREATE INDEX ON "+schema+".phx_events (ts)")
	for _, d := range days {
		addDay(t, ctx, pool, schema, d)
	}
}

func addDay(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, day string) {
	t.Helper()
	next, err := time.Parse("2006-01-02", day)
	if err != nil {
		t.Fatal(err)
	}
	mustExec(t, ctx, pool, fmt.Sprintf(`CREATE TABLE %s.phx_events_p%s
		PARTITION OF %s.phx_events FOR VALUES FROM ('%s') TO ('%s')`, schema,
		strings.ReplaceAll(day, "-", ""), schema, day,
		next.AddDate(0, 0, 1).Format("2006-01-02")))
}

func explainJSON(t *testing.T, ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, sql string) []byte {
	t.Helper()
	var plan []byte
	if err := q.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql).Scan(&plan); err != nil {
		t.Fatalf("explain %s: %v", sql, err)
	}
	return plan
}

// resolvedHash resolves one capture, as the collectors do.
func resolvedHash(t *testing.T, ctx context.Context, db Beginner, plan []byte) string {
	t.Helper()
	roots, err := Resolve(ctx, db, plan)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	return mustWith(t, string(plan), roots)
}

func TestResolve_DailyCapturesShareOneHash(t *testing.T) {
	pool, ctx := livePool(t)
	dailyTable(t, ctx, pool, "phx_daily", "2026-11-05", "2026-11-06", "2026-11-07")
	for _, verbose := range []bool{false, true} {
		q := func(day string) []byte {
			sql := "SELECT id FROM phx_daily.phx_events WHERE ts = DATE '" + day + "'"
			if verbose {
				return verbosePlan(t, ctx, pool, sql)
			}
			return explainJSON(t, ctx, pool, sql)
		}
		day1, day2 := q("2026-11-05"), q("2026-11-06")
		if !strings.Contains(string(day1), "phx_events_p20261105") ||
			!strings.Contains(string(day2), "phx_events_p20261106") {
			t.Fatalf("plans do not scan the daily partitions:\n%s\n%s", day1, day2)
		}
		if mustCompute(t, string(day1)) == mustCompute(t, string(day2)) {
			t.Fatal("unresolved hashes should differ; the fixture does not show the bug")
		}
		h1, h2 := resolvedHash(t, ctx, pool, day1), resolvedHash(t, ctx, pool, day2)
		if h1 != h2 || !resolvedPattern.MatchString(h1) {
			t.Fatalf("verbose=%v: daily captures hashed %s and %s, want one v2 hash",
				verbose, h1, h2)
		}
	}
}

func verbosePlan(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string) []byte {
	t.Helper()
	var plan []byte
	if err := pool.QueryRow(ctx, "EXPLAIN (VERBOSE, FORMAT JSON) "+sql).Scan(&plan); err != nil {
		t.Fatalf("explain verbose %s: %v", sql, err)
	}
	return plan
}

// Scanning every partition is bucket "all" however many leaves exist, and
// pruning to some of them is a different plan.
func TestResolve_AllPartitionsBucketFollowsLeafCount(t *testing.T) {
	pool, ctx := livePool(t)
	dailyTable(t, ctx, pool, "phx_all", "2026-11-05", "2026-11-06", "2026-11-07")
	full := "SELECT count(*) FROM phx_all.phx_events"
	three := resolvedHash(t, ctx, pool, explainJSON(t, ctx, pool, full))
	two := resolvedHash(t, ctx, pool, explainJSON(t, ctx, pool, full+
		" WHERE ts IN (DATE '2026-11-05', DATE '2026-11-06')"))
	addDay(t, ctx, pool, "phx_all", "2026-11-08")
	four := resolvedHash(t, ctx, pool, explainJSON(t, ctx, pool, full))
	if three != four {
		t.Errorf("full scans of 3 and 4 partitions hashed %s and %s, want one (all)",
			three, four)
	}
	if two == three {
		t.Error("a scan of 2 of 3 partitions must differ from a scan of all of them")
	}
}

// A generic plan prunes at executor start: EXPLAIN shows one child and
// "Subplans Removed", which the hash records.
func TestResolve_RuntimePruningSetsPrunedFlag(t *testing.T) {
	pool, ctx := livePool(t)
	dailyTable(t, ctx, pool, "phx_prune", "2026-11-05", "2026-11-06", "2026-11-07")
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	for _, s := range []string{"SET plan_cache_mode = force_generic_plan",
		"PREPARE phx_q(date) AS SELECT id FROM phx_prune.phx_events WHERE ts = $1"} {
		if _, err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	defer func() { _, _ = conn.Exec(context.Background(), "DEALLOCATE ALL; RESET ALL") }()
	day1 := explainJSON(t, ctx, conn, "EXECUTE phx_q('2026-11-05')")
	day2 := explainJSON(t, ctx, conn, "EXECUTE phx_q('2026-11-06')")
	if !strings.Contains(string(day1), `"Subplans Removed": 2`) {
		t.Fatalf("generic plan was not pruned at executor start:\n%s", day1)
	}
	h1, h2 := resolvedHash(t, ctx, pool, day1), resolvedHash(t, ctx, pool, day2)
	if h1 != h2 {
		t.Fatalf("runtime-pruned captures of two days hashed %s and %s", h1, h2)
	}
	stripped := stripKey(t, day1, "Subplans Removed")
	if resolvedHash(t, ctx, pool, stripped) == h1 {
		t.Fatal("the pruned flag is not part of the hash")
	}
}

func stripKey(t *testing.T, plan []byte, key string) []byte {
	t.Helper()
	var v any
	if err := json.Unmarshal(plan, &v); err != nil {
		t.Fatal(err)
	}
	var walk func(any)
	walk = func(n any) {
		switch x := n.(type) {
		case map[string]any:
			delete(x, key)
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(v)
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Without VERBOSE a relation name carries no schema; a name that exists in
// two schemas cannot be resolved and the hash says so.
func TestResolve_AmbiguousNameStaysUnresolved(t *testing.T) {
	pool, ctx := livePool(t)
	dailyTable(t, ctx, pool, "phx_amb", "2026-11-05")
	mustExec(t, ctx, pool, "DROP SCHEMA IF EXISTS phx_amb_twin CASCADE")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS phx_amb_twin CASCADE")
	})
	mustExec(t, ctx, pool, "CREATE SCHEMA phx_amb_twin")
	mustExec(t, ctx, pool, "CREATE TABLE phx_amb_twin.phx_events_p20261105 (id int)")
	sql := "SELECT id FROM phx_amb.phx_events WHERE ts = DATE '2026-11-05'"
	h := resolvedHash(t, ctx, pool, explainJSON(t, ctx, pool, sql))
	if !unresolvedPattern.MatchString(h) {
		t.Errorf("ambiguous non-VERBOSE name resolved: %s", h)
	}
	h = resolvedHash(t, ctx, pool, verbosePlan(t, ctx, pool, sql))
	if !resolvedPattern.MatchString(h) {
		t.Errorf("schema-qualified VERBOSE name must resolve: %s", h)
	}
}

func TestResolve_UnknownRelationAndCanceledReadAreUnresolved(t *testing.T) {
	pool, ctx := livePool(t)
	ghost := []byte(`{"Node Type": "Seq Scan", "Relation Name": "phx_no_such_table"}`)
	if h := resolvedHash(t, ctx, pool, ghost); !unresolvedPattern.MatchString(h) {
		t.Errorf("a relation missing from the catalog resolved: %s", h)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	roots, err := Resolve(canceled, pool, []byte(basePlan))
	if err == nil || !strings.Contains(err.Error(), "partition roots") {
		t.Fatalf("canceled read: err = %v, want a partition-roots read error", err)
	}
	if h := mustWith(t, basePlan, roots); !unresolvedPattern.MatchString(h) {
		t.Fatalf("a failed read must leave the hash unresolved, got %s", h)
	}
}

// boundsProbe records the limits the resolver's transaction runs under.
type boundsProbe struct {
	pool     *pgxpool.Pool
	mu       sync.Mutex
	settings []string
}

type probedTx struct {
	pgx.Tx
	probe *boundsProbe
}

func (b *boundsProbe) BeginTx(ctx context.Context, o pgx.TxOptions) (pgx.Tx, error) {
	tx, err := b.pool.BeginTx(ctx, o)
	return probedTx{Tx: tx, probe: b}, err
}

func (p probedTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	var st, lt, ro string
	err := p.Tx.QueryRow(ctx, `SELECT current_setting('statement_timeout'),
		current_setting('lock_timeout'), current_setting('transaction_read_only')`).
		Scan(&st, &lt, &ro)
	if err != nil {
		return nil, err
	}
	p.probe.mu.Lock()
	p.probe.settings = append(p.probe.settings, st, lt, ro)
	p.probe.mu.Unlock()
	return p.Tx.Query(ctx, sql, args...)
}

func TestResolve_ReadIsReadOnlyAndBounded(t *testing.T) {
	pool, ctx := livePool(t)
	probe := &boundsProbe{pool: pool}
	if _, err := Resolve(ctx, probe, []byte(basePlan)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := strings.Join(probe.settings, ","); got != "2s,100ms,on" {
		t.Fatalf("resolver read ran with statement_timeout,lock_timeout,read_only = %s", got)
	}
}

func TestResolve_ConcurrentCapturesAgree(t *testing.T) {
	pool, ctx := livePool(t)
	dailyTable(t, ctx, pool, "phx_conc", "2026-11-05", "2026-11-06")
	const q = "SELECT id FROM phx_conc.phx_events WHERE ts = DATE "
	plans := [][]byte{
		explainJSON(t, ctx, pool, q+"'2026-11-05'"),
		explainJSON(t, ctx, pool, q+"'2026-11-06'"),
	}
	hashes := make([]string, 16)
	errs := make([]error, 16)
	var wg sync.WaitGroup
	for i := range hashes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			roots, err := Resolve(ctx, pool, plans[i%2])
			if err == nil {
				hashes[i], err = ComputeWith(plans[i%2], Options{Roots: roots})
			}
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, h := range hashes {
		if errs[i] != nil || h != hashes[0] || !resolvedPattern.MatchString(h) {
			t.Fatalf("capture %d: hash %q err %v, want %q", i, h, errs[i], hashes[0])
		}
	}
}
