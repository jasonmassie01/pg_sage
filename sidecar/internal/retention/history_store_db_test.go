package retention

import (
	"context"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/histstore"
	"github.com/pg-sage/sidecar/internal/testsupport/histfixture"
)

// history.store: meta. A per-database cleaner leaves the history tables
// alone (the store has one cleaner per process; rows left behind in the
// monitored database are removed by `history migrate --cleanup`). The
// store cleaner ages every database's history out by the process-wide
// windows and holds the store under the sum of the databases' caps.

func storeCfg() *config.Config {
	return &config.Config{Retention: config.RetentionConfig{SnapshotsDays: 7,
		QueryStoreDays: 14, FindingsDays: 30, SnapshotsMaxPct: 5}}
}

func countHist(t *testing.T, p *histfixture.Pair, meta bool, sql string, args ...any) int {
	t.Helper()
	pool := p.Monitored
	if meta {
		pool = p.Meta
	}
	var n int
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func insertAged(t *testing.T, p *histfixture.Pair, meta bool, db *int, age time.Duration) {
	t.Helper()
	pool := p.Monitored
	if meta {
		pool = p.Meta
	}
	ctx := context.Background()
	at := time.Now().Add(-age)
	if db == nil {
		_, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
			VALUES ($1, 'system', '{}');
			`, at)
		if err == nil {
			_, err = pool.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
				total_exec_time, mean_exec_time) VALUES ($1, 1, 1, 1, 1)`, at)
		}
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.snapshots (collected_at, category, data,
		database_id) VALUES ($1, 'system', '{}', $2)`, at, *db); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store (captured_at, queryid, calls,
		total_exec_time, mean_exec_time, database_id) VALUES ($1, 1, 1, 1, 1, $2)`,
		at, *db); err != nil {
		t.Fatal(err)
	}
}

func TestPerDatabaseCleanerLeavesHistoryToTheStore(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	own := histfixture.DatabaseID
	insertAged(t, p, false, nil, 40*24*time.Hour) // left behind before the migration
	insertAged(t, p, true, &own, 40*24*time.Hour)
	stats := New(p.Monitored, storeCfg(), noopLog).RunOnce(context.Background())
	for _, d := range stats.Dropped {
		t.Fatalf("a per-database cleaner in meta mode dropped history partition %s", d)
	}
	if stats.Deleted["snapshots"] != 0 || stats.Deleted["query_store"] != 0 {
		t.Fatalf("a per-database cleaner in meta mode deleted history rows: %v",
			stats.Deleted)
	}
	if n := countHist(t, p, false, "SELECT count(*) FROM sage.snapshots"); n != 1 {
		t.Fatalf("leftover monitored snapshots: %d, want 1 (cleanup's job)", n)
	}
	if n := countHist(t, p, true, "SELECT count(*) FROM sage.snapshots"); n != 1 {
		t.Fatalf("store snapshots: %d, want 1 (the store cleaner's job)", n)
	}
}

func TestStoreCleanerAgesOutEveryDatabasesHistory(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta)
	ctx := context.Background()
	for _, db := range []int{histfixture.DatabaseID, histfixture.OtherDatabaseID} {
		insertAged(t, p, true, &db, 40*24*time.Hour)
		insertAged(t, p, true, &db, time.Hour)
	}
	if _, err := p.Meta.Exec(ctx, `INSERT INTO sage.findings (category, severity, title,
		detail, status, resolved_at, last_seen, created_at) VALUES ('x', 'info', 't', '{}',
		'resolved', now() - interval '100 days', now() - interval '100 days',
		now() - interval '100 days')`); err != nil {
		t.Fatal(err)
	}
	c := New(p.Meta, storeCfg(), noopLog).ForHistoryStore()
	for i := 0; i < 3; i++ { // purges are paced: a few runs finish the backlog
		c.RunOnce(ctx)
	}
	for _, q := range []string{
		"SELECT count(*) FROM sage.snapshots WHERE collected_at < now() - interval '8 days'",
		"SELECT count(*) FROM sage.query_store WHERE captured_at < now() - interval '15 days'",
	} {
		if n := countHist(t, p, true, q); n != 0 {
			t.Fatalf("%d expired rows left: %s", n, q)
		}
	}
	for _, db := range []int{histfixture.DatabaseID, histfixture.OtherDatabaseID} {
		if n := countHist(t, p, true, `SELECT count(*) FROM sage.snapshots
			WHERE database_id = $1`, db); n != 1 {
			t.Fatalf("database %d keeps %d recent snapshots, want 1", db, n)
		}
	}
	if n := countHist(t, p, true, "SELECT count(*) FROM sage.findings"); n != 1 {
		t.Fatal("the store cleaner must touch only the history tables of the meta database")
	}
}

func TestStoreCapIsTheSumOfEachDatabasesCap(t *testing.T) {
	floor := minSnapshotCapBytes
	dbs := []histstore.StoreDatabase{{ID: 1, DBBytes: 100 << 30}, {ID: 2, DBBytes: 1 << 30},
		{ID: 3, DBBytes: 0}}
	want := (100<<30)*5/100 + floor + floor
	if got := storeCap(dbs, 5); got != want {
		t.Fatalf("store cap = %d, want %d (5%% of 100 GB + two floors)", got, want)
	}
	if got := storeCap(dbs, 0); got != 0 {
		t.Fatalf("pct 0 disables the cap, got %d", got)
	}
	if got := storeCap(nil, 5); got != 0 {
		t.Fatalf("no registered database: no cap, got %d", got)
	}
	if got := storeCap([]histstore.StoreDatabase{{ID: 1, DBBytes: -5}}, 5); got != floor {
		t.Fatalf("an unknown (negative) size counts the floor, got %d", got)
	}
}

func TestStoreCleanerCapReadsAndRefreshesTheRegistry(t *testing.T) {
	p := histfixture.NewPair(t)
	p.Switch(t, histstore.ModeMeta) // registers the monitored pool in the meta store
	ctx := context.Background()
	own := p.MetaStore(t)
	other, err := histstore.NewMeta(p.Meta, histfixture.OtherDatabaseID)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []histstore.Store{own, other} {
		if err := histstore.RegisterDatabase(ctx, s, "db"); err != nil {
			t.Fatal(err)
		}
	}
	if err := histstore.UpdateDatabaseSize(ctx, other, 20<<30); err != nil {
		t.Fatal(err)
	}
	c := New(p.Meta, storeCfg(), noopLog).ForHistoryStore()
	limit, on := c.capFor(ctx, 1<<40)
	if !on {
		t.Fatal("the cap must be on")
	}
	// Database 7's size is refreshed from its live monitored pool (a small
	// test database: the floor); database 8 keeps its last known 20 GB.
	if want := minSnapshotCapBytes + (20<<30)*5/100; limit != want {
		t.Fatalf("store cap = %d, want %d", limit, want)
	}
	dbs, err := histstore.StoreDatabases(ctx, p.Meta, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range dbs {
		if d.ID == histfixture.DatabaseID && (d.DBBytes <= 0 || d.DBBytes > 1<<30) {
			t.Fatalf("database 7's size was not refreshed from its pool: %d", d.DBBytes)
		}
	}
	if _, on := c.capFor(ctx, minSnapshotCapBytes/2); on {
		t.Fatal("a store under one floor is never capped")
	}
}
