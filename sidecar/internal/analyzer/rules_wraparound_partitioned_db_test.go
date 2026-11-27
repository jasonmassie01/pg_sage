package analyzer

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/collector"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-01 integration (real PostgreSQL, own database): a partitioned
// parent with two leaves. The collector reads the parent's XID age as
// unknown (-1) instead of 2147483647, the wraparound rule (vacuum-08: on
// the shared horizon reader) flags only the leaves, and a leaf frozen
// with VACUUM (FREEZE) drops out on the next cycle while its sibling
// stays. No stored snapshot carries 2147483647. The database is on the
// exclusive server: the frozen leaf must read under 2,000 XIDs old a
// collector cycle later, but on the shared one other packages burn more
// than that meanwhile, and on PostgreSQL 14 each backend still logging in
// holds back the horizon the freeze advances to.
func TestWraparoundFreeze_PartitionedTreeOnRealPostgres(t *testing.T) {
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.Exclusive(t, "the XID age of a freshly frozen leaf"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	for _, sql := range []string{
		"CREATE TABLE public.wp (id int, k int) PARTITION BY RANGE (k)",
		`CREATE TABLE public.wp_1 PARTITION OF public.wp FOR VALUES FROM (0) TO (10)
		   WITH (autovacuum_enabled = off, autovacuum_freeze_max_age = 100000)`,
		`CREATE TABLE public.wp_2 PARTITION OF public.wp FOR VALUES FROM (10) TO (20)
		   WITH (autovacuum_enabled = off, autovacuum_freeze_max_age = 100000)`,
		"INSERT INTO public.wp SELECT g, g % 20 FROM generate_series(1, 400) g",
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	burnXIDs(t, ctx, pool, 3000)
	cfg := wraparoundDBConfig()
	first := collectOneSnapshot(t, pool, cfg)
	parent, ok := tableIn(first, "public", "wp")
	if !ok || parent.Relkind != "p" || parent.XIDAge != -1 {
		t.Fatalf("parent row = %+v (collected %v), want relkind p and xid_age -1",
			parent, ok)
	}
	got := wraparoundObjects(ruleWraparoundFreeze(first, nil, cfg,
		&RuleExtras{FreezeHorizons: readHorizons(t, ctx, pool)}))
	if _, flagged := got["public.wp"]; flagged || got["public.wp_1"] == "" ||
		got["public.wp_2"] == "" {
		t.Fatalf("first cycle findings = %v, want both leaves and never the parent", got)
	}
	if _, err := pool.Exec(ctx, "VACUUM (FREEZE) public.wp_1"); err != nil {
		t.Fatalf("freeze leaf: %v", err)
	}
	second := collectOneSnapshot(t, pool, cfg)
	got = wraparoundObjects(ruleWraparoundFreeze(second, nil, cfg,
		&RuleExtras{FreezeHorizons: readHorizons(t, ctx, pool)}))
	if _, stays := got["public.wp_1"]; stays || got["public.wp_2"] == "" {
		t.Fatalf("after freezing wp_1 findings = %v, want wp_2 only", got)
	}
	if _, flagged := got["public.wp"]; flagged {
		t.Fatalf("second cycle flagged the parent: %v", got)
	}
	assertNoSentinelSnapshots(t, ctx, pool)
}

// burnXIDs advances the XID counter by at least n in one statement: each
// exception block is a subtransaction, and its INSERT takes its own XID
// (txid_current() would only assign the top-level one).
func burnXIDs(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	if _, err := pool.Exec(ctx, "CREATE TABLE public.xid_burn (i int)"); err != nil {
		t.Fatalf("burn table: %v", err)
	}
	var before, after int64
	read := func(v *int64) {
		if err := pool.QueryRow(ctx, "SELECT txid_current()").Scan(v); err != nil {
			t.Fatalf("read xid: %v", err)
		}
	}
	read(&before)
	if _, err := pool.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN FOR i IN 1..%d LOOP
		BEGIN INSERT INTO public.xid_burn VALUES (i);
		EXCEPTION WHEN others THEN RAISE; END; END LOOP; END $$`, n)); err != nil {
		t.Fatalf("burn xids: %v", err)
	}
	read(&after)
	if after-before < int64(n) {
		t.Fatalf("burned %d xids, want at least %d", after-before, n)
	}
}

// wraparoundDBConfig flags a leaf past 2% of its 100,000 freeze maximum
// (2,000 XIDs): the fixture burns 3,000, and a freshly frozen leaf is far
// younger.
func wraparoundDBConfig() *config.Config {
	cfg := phase2Config()
	cfg.Collector.IntervalSeconds = 3600
	cfg.Safety.CPUCeilingPct = 100
	cfg.Analyzer.WraparoundFreezeShare = 0.02
	return cfg
}

// readHorizons reads the database's horizons as the freeze sampler does.
func readHorizons(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []probes.FreezeHorizon {
	t.Helper()
	horizons, err := probes.ReadFreezeHorizons(ctx, pool, 500, 0)
	if err != nil {
		t.Fatalf("read freeze horizons: %v", err)
	}
	return horizons
}

// collectOneSnapshot runs a fresh collector until its startup snapshot
// exists (and is persisted), then stops it.
func collectOneSnapshot(
	t *testing.T, pool *pgxpool.Pool, cfg *config.Config,
) *collector.Snapshot {
	t.Helper()
	var version int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatalf("version: %v", err)
	}
	stored := storedTableSnapshots(t, pool)
	coll := collector.New(pool, cfg, version, noopLog)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { coll.Run(ctx); close(done) }()
	// Run publishes the snapshot, then persists it: wait for both.
	deadline := time.Now().Add(30 * time.Second)
	for (coll.LatestSnapshot() == nil || storedTableSnapshots(t, pool) == stored) &&
		time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	cancel()
	<-done
	snap := coll.LatestSnapshot()
	if snap == nil {
		t.Fatal("collector produced no snapshot within 30s")
	}
	return snap
}

func storedTableSnapshots(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		"SELECT count(*) FROM sage.snapshots WHERE category = 'tables'").Scan(&n); err != nil {
		t.Fatalf("count stored snapshots: %v", err)
	}
	return n
}

func tableIn(snap *collector.Snapshot, schemaName, rel string) (collector.TableStats, bool) {
	for _, ts := range snap.Tables {
		if ts.SchemaName == schemaName && ts.RelName == rel {
			return ts, true
		}
	}
	return collector.TableStats{}, false
}

func wraparoundObjects(findings []Finding) map[string]string {
	out := map[string]string{}
	for _, f := range findings {
		if f.Category == "wraparound_freeze" {
			out[f.ObjectIdentifier] = f.Severity
		}
	}
	return out
}

// assertNoSentinelSnapshots is CHECK-02: no stored tables snapshot row
// carries the non-normal-XID age 2147483647.
func assertNoSentinelSnapshots(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	var rows, sentinel int
	if err := pool.QueryRow(ctx, `SELECT count(*),
		count(*) FILTER (WHERE data::text LIKE '%2147483647%')
		FROM sage.snapshots WHERE category = 'tables'`).Scan(&rows, &sentinel); err != nil {
		t.Fatalf("read stored snapshots: %v", err)
	}
	if rows == 0 || sentinel != 0 {
		t.Fatalf("stored tables snapshots = %d, with 2147483647: %d (want >0, 0)",
			rows, sentinel)
	}
}
