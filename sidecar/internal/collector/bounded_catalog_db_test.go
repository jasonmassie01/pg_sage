package collector

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// Dogfood lifeos-1 findings 4 and 8: with 160 leaked test schemas
// (35,439 indexes, 15,301 tables, 12,038 sequences) the collector's index
// query timed out and failed the whole snapshot, and every snapshot
// stored all 12,000 sequences (498 MB that the forecaster then expanded).
// Catalog categories now page by oid (each page bounded by the batch, not
// by the catalog), a category that fails is marked unavailable without
// failing the snapshot, and only the sequences that matter are kept.

const (
	manySchemas = 120
	tablesEach  = 10
)

// createManySchemas creates manySchemas schemas of tablesEach tables, each
// with a primary key and two secondary indexes.
func createManySchemas(t *testing.T, ctx context.Context, prefix string) {
	t.Helper()
	pool := testPool(t)
	t.Cleanup(func() {
		for s := 0; s < manySchemas; s++ {
			_, _ = pool.Exec(context.Background(),
				fmt.Sprintf("DROP SCHEMA IF EXISTS %s_%d CASCADE", prefix, s))
		}
	})
	for s := 0; s < manySchemas; s++ {
		_, err := pool.Exec(ctx, fmt.Sprintf(`DO $$ BEGIN
			EXECUTE 'CREATE SCHEMA %[1]s_%[2]d';
			FOR i IN 1..%[3]d LOOP
				EXECUTE format('CREATE TABLE %[1]s_%[2]d.t%%s (id int PRIMARY KEY, a int, b int)', i);
				EXECUTE format('CREATE INDEX ON %[1]s_%[2]d.t%%s (a)', i);
				EXECUTE format('CREATE INDEX ON %[1]s_%[2]d.t%%s (b)', i);
			END LOOP; END $$`, prefix, s, tablesEach))
		if err != nil {
			t.Fatalf("schema %d: %v", s, err)
		}
	}
}

func TestCollectCatalog_ManySchemasPagedByOID(t *testing.T) {
	pool := testPool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	createManySchemas(t, ctx, "bounded_many")
	cfg := testConfig()
	cfg.Collector.BatchSize = 500
	cfg.Safety.QueryTimeoutMs = 500
	c := New(pool, cfg, 160000, noopLog)
	start := time.Now()
	idx, err := c.collectIndexes(ctx)
	if err != nil {
		t.Fatalf("indexes: %v", err)
	}
	tables, err := c.collectTables(ctx)
	if err != nil {
		t.Fatalf("tables: %v", err)
	}
	if el := time.Since(start); el > 20*time.Second {
		t.Fatalf("catalog collection took %s", el)
	}
	seenIdx, seenTab := map[string]bool{}, map[string]bool{}
	for _, i := range idx {
		if strings.HasPrefix(i.SchemaName, "bounded_many_") {
			key := i.SchemaName + "." + i.IndexRelName
			if seenIdx[key] || i.IndexDef == "" || i.RelName == "" {
				t.Fatalf("index %s duplicated or incomplete: %+v", key, i)
			}
			seenIdx[key] = true
		}
	}
	for _, tb := range tables {
		if strings.HasPrefix(tb.SchemaName, "bounded_many_") {
			key := tb.SchemaName + "." + tb.RelName
			if seenTab[key] || tb.TotalBytes <= 0 {
				t.Fatalf("table %s duplicated or unsized: %+v", key, tb)
			}
			seenTab[key] = true
		}
	}
	if len(seenIdx) != manySchemas*tablesEach*3 || len(seenTab) != manySchemas*tablesEach {
		t.Fatalf("collected %d indexes and %d tables, want %d and %d", len(seenIdx),
			len(seenTab), manySchemas*tablesEach*3, manySchemas*tablesEach)
	}
	if !strings.Contains(indexStatsSQL, "s.indexrelid > $1") ||
		!strings.Contains(tableStatsSQL, "s.relid > $1") {
		t.Fatal("catalog pages are not keyed by oid")
	}
}

type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRecorder) log(level, msg string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, level+" "+fmt.Sprintf(msg, args...))
}

func (l *logRecorder) has(level, sub string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.HasPrefix(s, level) && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// One slow category marks itself unavailable; the snapshot, the other
// categories and their persistence go on, and nothing is stored for the
// unavailable one (an empty row would read as "no indexes").
func TestCollect_OneFailedCategoryDoesNotFailTheSnapshot(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	rec := &logRecorder{}
	c := New(pool, testConfig(), serverVersion(t, pool), rec.log)
	timeout := errors.New("canceling statement due to statement timeout")
	c.overrideStep("indexes", func(context.Context, *Snapshot) error { return timeout })
	snap, err := c.collect(ctx)
	if err != nil || snap == nil {
		t.Fatalf("collect = %v (%v), want a snapshot", snap, err)
	}
	if snap.Available("indexes") || !strings.Contains(snap.Unavailable["indexes"],
		"statement timeout") || snap.Indexes != nil {
		t.Fatalf("indexes = %v / %q, want unavailable with the reason", snap.Indexes,
			snap.Unavailable["indexes"])
	}
	if !snap.Available("tables") || len(snap.Tables) == 0 || !snap.Available("system") {
		t.Fatalf("other categories missing: tables %d, unavailable %v", len(snap.Tables),
			snap.Unavailable)
	}
	if !rec.has("WARN", "indexes") {
		t.Fatalf("logs = %v, want a warning naming the indexes category", rec.lines)
	}
	if _, err := pool.Exec(ctx, `CREATE SCHEMA IF NOT EXISTS sage;
		CREATE TABLE IF NOT EXISTS sage.snapshots (id bigserial PRIMARY KEY,
		collected_at timestamptz NOT NULL, category text NOT NULL, data jsonb NOT NULL)`); err != nil {
		t.Fatalf("snapshots table: %v", err)
	}
	if err := c.persist(ctx, snap); err != nil {
		t.Fatalf("persist: %v", err)
	}
	var idxRows, tableRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE category = 'indexes'),
		count(*) FILTER (WHERE category = 'tables') FROM sage.snapshots
		WHERE collected_at = $1`, snap.CollectedAt).Scan(&idxRows, &tableRows); err != nil {
		t.Fatalf("count: %v", err)
	}
	if idxRows != 0 || tableRows != 1 {
		t.Fatalf("persisted %d index rows and %d table rows, want 0 and 1", idxRows,
			tableRows)
	}
}

// System stats are the snapshot's spine: without them there is no
// snapshot (error propagation stays distinguishable).
func TestCollect_SystemFailureStillFailsTheSnapshot(t *testing.T) {
	c := New(testPool(t), testConfig(), 170000, noopLog)
	c.overrideStep("system", func(context.Context, *Snapshot) error {
		return errors.New("connection refused")
	})
	if snap, err := c.collect(context.Background()); err == nil || snap != nil ||
		!strings.Contains(err.Error(), "system") {
		t.Fatalf("collect = %v (%v), want a failure naming system", snap, err)
	}
}

// Only sequences that matter are kept: every one at or above the floor
// and the top N by use; never-used sequences never.
func TestCollectSequences_KeepsOnlyThoseThatMatter(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS seq_keep CASCADE")
	})
	if _, err := pool.Exec(ctx, `CREATE SCHEMA seq_keep;
		DO $$ BEGIN
			FOR i IN 1..400 LOOP
				EXECUTE format('CREATE SEQUENCE seq_keep.low_%s AS integer', i);
				EXECUTE format('SELECT setval(%L, %s)', 'seq_keep.low_' || i, i);
			END LOOP;
			FOR i IN 1..50 LOOP
				EXECUTE format('CREATE SEQUENCE seq_keep.unused_%s', i);
			END LOOP;
			FOR i IN 1..3 LOOP
				EXECUTE format('CREATE SEQUENCE seq_keep.hot_%s AS integer', i);
				EXECUTE format('SELECT setval(%L, %s)', 'seq_keep.hot_' || i, i * 500000000);
			END LOOP; END $$`); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	got, err := New(pool, testConfig(), 160000, noopLog).collectSequences(ctx)
	if err != nil {
		t.Fatalf("sequences: %v", err)
	}
	if len(got) > SequenceTopN+3 || len(got) < 3 {
		t.Fatalf("kept %d sequences, want at most %d", len(got), SequenceTopN+3)
	}
	hot := 0
	for i, s := range got {
		if strings.HasPrefix(s.SequenceName, "unused_") {
			t.Fatalf("never-used sequence kept: %+v", s)
		}
		if strings.HasPrefix(s.SequenceName, "hot_") {
			hot++
		}
		if i > 0 && s.PctUsed > got[i-1].PctUsed {
			t.Fatalf("not ordered by use at %d", i)
		}
		if i >= SequenceTopN && s.PctUsed < SequenceFloorPct {
			t.Fatalf("row %d (%s, %.2f%%) is past the top %d and under the floor", i,
				s.SequenceName, s.PctUsed, SequenceTopN)
		}
	}
	if hot != 3 {
		t.Fatalf("kept %d of the 3 sequences above the floor", hot)
	}
}

// serverVersion is the test server's server_version_num: collectSystem
// picks its SQL by version.
func serverVersion(t *testing.T, pool interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}) int {
	t.Helper()
	var v int
	if err := pool.QueryRow(context.Background(),
		"SELECT current_setting('server_version_num')::int").Scan(&v); err != nil {
		t.Fatalf("server version: %v", err)
	}
	return v
}
