package analyzer

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/snapstore"
	"github.com/pg-sage/sidecar/internal/testdb"
	"github.com/pg-sage/sidecar/internal/testsupport/snapfixture"
)

// Snapshot dedupe golden comparison for the query_regression history: the
// per-query historical means over the lookback (downsampled to 100 samples)
// are identical whether the queries history was stored as legacy full rows
// or as deltas, and the downsampling really applies.
func TestHistoricalAverages_DedupeGolden(t *testing.T) {
	deltaPool := phase2Pool(t)
	ctx := context.Background()
	const clean = `DELETE FROM sage.snapshots WHERE category = 'queries'`
	if _, err := deltaPool.Exec(ctx, clean); err != nil {
		t.Fatalf("clean: %v", err)
	}
	t.Cleanup(func() {
		_, _ = deltaPool.Exec(ctx, `DELETE FROM sage.snapshots WHERE category = 'queries'`)
	})
	legacyPool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "analyzer_snap_legacy"))
	if err != nil {
		t.Fatalf("connect legacy: %v", err)
	}
	t.Cleanup(legacyPool.Close)
	if err := schema.Bootstrap(ctx, legacyPool); err != nil {
		t.Fatalf("bootstrap legacy: %v", err)
	}
	sc := snapfixture.Scenario{
		Start:  time.Now().UTC().Add(-50 * time.Hour).Truncate(time.Minute),
		Step:   15 * time.Minute,
		Cycles: 190, // more than the 100 samples the average keeps
		Tables: 10, Indexes: 10, Seed: 99,
		Events: snapfixture.Events{QueryChurn: 4},
	}
	cycles, err := sc.Generate()
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	w := snapstore.NewWriter()
	for _, c := range cycles {
		var rows []snapstore.Row
		for _, d := range c.Docs {
			if d.Category == "queries" {
				rows = append(rows, snapstore.Row{Category: d.Category, Data: d.Data})
			}
		}
		if err := w.Persist(ctx, deltaPool, c.At, rows); err != nil {
			t.Fatalf("persist: %v", err)
		}
		legacyCycle := snapfixture.Cycle{At: c.At}
		for _, d := range c.Docs {
			if d.Category == "queries" {
				legacyCycle.Docs = append(legacyCycle.Docs, d)
			}
		}
		if err := snapfixture.InsertLegacy(ctx, legacyPool, legacyCycle); err != nil {
			t.Fatalf("legacy: %v", err)
		}
	}
	got := New(deltaPool, phase2Config(), nil, nil, nil, nil, nil, noopLog).
		buildHistoricalAverages(ctx)
	want := New(legacyPool, phase2Config(), nil, nil, nil, nil, nil, noopLog).
		buildHistoricalAverages(ctx)
	if len(want) < 60 || !reflect.DeepEqual(got, want) {
		t.Fatalf("historical averages differ: %d delta vs %d legacy queries", len(got),
			len(want))
	}
}
