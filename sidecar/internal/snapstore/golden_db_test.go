package snapstore_test

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/forecaster"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

// Golden comparison: one scenario is written in the legacy format (one full
// row per category and cycle) into its own database and through the delta
// writer into the package database. Every consumer must read the same
// results from both.

func openBootstrapped(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if _, err := pool.Exec(ctx, "TRUNCATE sage.snapshots"); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	return pool
}

// goldenStores returns (delta store, legacy store).
func goldenStores(t *testing.T) (*pgxpool.Pool, *pgxpool.Pool, context.Context) {
	t.Helper()
	ctx := context.Background()
	deltaPool := openBootstrapped(t, ctx, testdb.SkipUnlessLive(t))
	legacyPool := openBootstrapped(t, ctx, testdb.CreateDatabase(t, "snapstore_legacy"))
	return deltaPool, legacyPool, ctx
}

func toRows(c snapfixture.Cycle) []snapstore.Row {
	rows := make([]snapstore.Row, 0, len(c.Docs))
	for _, d := range c.Docs {
		rows = append(rows, snapstore.Row{Category: d.Category, Data: d.Data})
	}
	return rows
}

func writeBoth(t *testing.T, ctx context.Context, deltaPool, legacyPool *pgxpool.Pool,
	cycles []snapfixture.Cycle) {
	t.Helper()
	w := snapstore.NewWriter()
	for _, c := range cycles {
		if err := w.Persist(ctx, deltaPool, c.At, toRows(c)); err != nil {
			t.Fatalf("persist delta cycle %s: %v", c.At, err)
		}
		if err := snapfixture.InsertLegacy(ctx, legacyPool, c); err != nil {
			t.Fatalf("persist legacy cycle %s: %v", c.At, err)
		}
	}
}

// goldenScenario spans three days so the forecaster's per-day sampling has
// several days, with every catalog event the store must survive.
func goldenScenario(t *testing.T) []snapfixture.Cycle {
	t.Helper()
	sc := snapfixture.Scenario{
		Start:   time.Now().UTC().Add(-72 * time.Hour).Truncate(time.Minute),
		Step:    20 * time.Minute,
		Cycles:  210,
		Tables:  40,
		Indexes: 300,
		Seed:    20261002,
		Events: snapfixture.Events{CounterReset: 50, DropIndex: 80, Redefine: 100,
			Rename: 120, EmptyFrom: 140, EmptyTo: 144, Unavailable: 150, QueryChurn: 7},
	}
	cycles, err := sc.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return cycles
}

type readBack struct {
	at   time.Time
	cat  string
	data string
}

func readAll(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []readBack {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT collected_at, category, `+
		snapstore.DataSQL("")+`::text FROM sage.snapshots ORDER BY collected_at, category`)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	defer rows.Close()
	var out []readBack
	for rows.Next() {
		var r readBack
		var data *string
		if err := rows.Scan(&r.at, &r.cat, &data); err != nil {
			t.Fatalf("scan: %v", err)
		}
		if data == nil {
			t.Fatalf("%s row at %s reads back NULL", r.cat, r.at)
		}
		r.data = *data
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func countDeltas(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.snapshots
		WHERE base_id IS NOT NULL`).Scan(&n); err != nil {
		t.Fatalf("count deltas: %v", err)
	}
	return n
}

// Every stored document (history export, latest) reads back byte for byte
// as the legacy row, including element order, nulls, empty catalogs, the
// unavailable cycle, a counter reset, drop/recreate, redefinition, rename.
func TestGolden_EveryDocumentReadsBackIdentically(t *testing.T) {
	deltaPool, legacyPool, ctx := goldenStores(t)
	writeBoth(t, ctx, deltaPool, legacyPool, goldenScenario(t))
	got, want := readAll(t, ctx, deltaPool), readAll(t, ctx, legacyPool)
	if len(got) != len(want) || len(want) == 0 {
		t.Fatalf("%d delta-store rows, %d legacy rows", len(got), len(want))
	}
	for i := range want {
		if !got[i].at.Equal(want[i].at) || got[i].cat != want[i].cat ||
			got[i].data != want[i].data {
			t.Fatalf("row %d (%s %s) differs:\n delta  %.300s\n legacy %.300s",
				i, want[i].cat, want[i].at, got[i].data, want[i].data)
		}
	}
	if n := countDeltas(t, ctx, deltaPool); n < len(want)/2 {
		t.Fatalf("only %d of %d rows are deltas: dedupe barely engaged", n, len(want))
	}
}

// The forecaster's daily aggregates (system growth, query volume, sequence
// use) are identical and non-empty.
func TestGolden_ForecasterAggregatesIdentical(t *testing.T) {
	deltaPool, legacyPool, ctx := goldenStores(t)
	writeBoth(t, ctx, deltaPool, legacyPool, goldenScenario(t))
	sysD, err1 := forecaster.QueryDailySystemAggs(ctx, deltaPool, 7)
	sysL, err2 := forecaster.QueryDailySystemAggs(ctx, legacyPool, 7)
	qD, err3 := forecaster.QueryDailyQueryAggs(ctx, deltaPool, 7)
	qL, err4 := forecaster.QueryDailyQueryAggs(ctx, legacyPool, 7)
	sD, err5 := forecaster.QueryDailySeqAggs(ctx, deltaPool, 7)
	sL, err6 := forecaster.QueryDailySeqAggs(ctx, legacyPool, 7)
	for i, err := range []error{err1, err2, err3, err4, err5, err6} {
		if err != nil {
			t.Fatalf("aggregate %d: %v", i, err)
		}
	}
	if len(sysL) < 3 || len(qL) < 3 || len(sL) < 3 {
		t.Fatalf("legacy aggregates too small to compare: %d/%d/%d", len(sysL), len(qL),
			len(sL))
	}
	if !reflect.DeepEqual(sysD, sysL) {
		t.Errorf("system aggs differ:\n delta  %+v\n legacy %+v", sysD, sysL)
	}
	if !reflect.DeepEqual(qD, qL) {
		t.Errorf("query aggs differ:\n delta  %+v\n legacy %+v", qD, qL)
	}
	if !reflect.DeepEqual(sD, sL) {
		t.Errorf("sequence aggs differ:\n delta  %+v\n legacy %+v", sD, sL)
	}
}

// unusedEvidence is, per index identity, what an unused-index proof over
// the window needs: samples, the idx_scan range, counter decreases
// (resets) and how many definitions the name had (drop/recreate, DDL).
type unusedEvidence struct {
	ident                  string
	samples, minScans      int64
	maxScans, resets, defs int64
}

const unusedEvidenceSQL = `
WITH s AS (
    SELECT collected_at, %s AS data FROM sage.snapshots
     WHERE category = 'indexes'),
e AS (
    SELECT s.collected_at, (el->>'schemaname') || '.' || (el->>'indexrelname') AS ident,
           (el->>'idx_scan')::bigint AS scans, el->>'indexdef' AS def
      FROM s, jsonb_array_elements(CASE WHEN jsonb_typeof(s.data) = 'array'
                                        THEN s.data ELSE '[]' END) el),
l AS (
    SELECT ident, scans, def,
           lag(scans) OVER (PARTITION BY ident ORDER BY collected_at) AS prev
      FROM e)
SELECT ident, count(*), min(scans), max(scans),
       count(*) FILTER (WHERE scans < prev), count(DISTINCT def)
  FROM l GROUP BY ident ORDER BY ident`

func readEvidence(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []unusedEvidence {
	t.Helper()
	rows, err := pool.Query(ctx, fmt.Sprintf(unusedEvidenceSQL, snapstore.DataSQL("")))
	if err != nil {
		t.Fatalf("evidence: %v", err)
	}
	defer rows.Close()
	var out []unusedEvidence
	for rows.Next() {
		var e unusedEvidence
		if err := rows.Scan(&e.ident, &e.samples, &e.minScans, &e.maxScans, &e.resets,
			&e.defs); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// Unused-index detection over the window stays provable: idx_scan for
// every sample, the counter reset, the drop and recreate and the
// redefinition are all visible, identically to the legacy rows.
func TestGolden_UnusedIndexEvidenceIdentical(t *testing.T) {
	deltaPool, legacyPool, ctx := goldenStores(t)
	writeBoth(t, ctx, deltaPool, legacyPool, goldenScenario(t))
	got, want := readEvidence(t, ctx, deltaPool), readEvidence(t, ctx, legacyPool)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("evidence differs: %d vs %d identities", len(got), len(want))
	}
	byIdent := map[string]unusedEvidence{}
	resets, cold := 0, 0
	for _, e := range want {
		byIdent[e.ident] = e
		if e.resets > 0 {
			resets++
		}
		if e.maxScans == 0 && e.samples > 100 {
			cold++
		}
	}
	if resets == 0 || cold == 0 {
		t.Fatalf("scenario lacks evidence: %d identities with resets, %d cold", resets, cold)
	}
	if e := byIdent["app.ix_t000_c00000"]; e.defs != 2 {
		t.Fatalf("dropped and recreated index = %+v, want two definitions", e)
	}
	if _, ok := byIdent["app.ix_t003_c00003_renamed"]; !ok {
		t.Fatal("renamed index missing from the evidence")
	}
}

// A sample whose catalog is an empty array is an empty sample for the
// forecaster in both formats (legacy: tiny jsonb; delta: n = 0), so the
// day's last non-empty sample is the same.
func TestGolden_EmptyArraySampleIsEmptyForForecaster(t *testing.T) {
	deltaPool, legacyPool, ctx := goldenStores(t)
	day := time.Now().UTC().Truncate(24*time.Hour).AddDate(0, 0, -1)
	seqs := []byte(`[{"schemaname":"app","sequencename":"s1","pct_used":40,"max_value":100},` +
		`{"schemaname":"app","sequencename":"s2","pct_used":2,"max_value":100}]`)
	cycles := []snapfixture.Cycle{
		{At: day.Add(10 * time.Hour), Docs: []snapfixture.Doc{{Category: "sequences", Data: seqs}}},
		{At: day.Add(15 * time.Hour), Docs: []snapfixture.Doc{{Category: "sequences",
			Data: []byte(`[]`)}}},
	}
	writeBoth(t, ctx, deltaPool, legacyPool, cycles)
	if n := countDeltas(t, ctx, deltaPool); n != 1 {
		t.Fatalf("%d delta rows, want the empty sample stored as one delta", n)
	}
	got, err1 := forecaster.QueryDailySeqAggs(ctx, deltaPool, 3)
	want, err2 := forecaster.QueryDailySeqAggs(ctx, legacyPool, 3)
	if err1 != nil || err2 != nil {
		t.Fatalf("seq aggs: %v / %v", err1, err2)
	}
	if len(want) != 2 || !reflect.DeepEqual(got, want) {
		t.Fatalf("seq aggs:\n delta  %+v\n legacy %+v", got, want)
	}
}
