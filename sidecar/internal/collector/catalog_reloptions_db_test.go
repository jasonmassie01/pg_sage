package collector

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// rules-02: each table's reloptions ride on the paged tables query, so
// the deterministic autovacuum rule sees a DBA's per-table tuning even
// when the advisor's configuration snapshot is off (no LLM).

const reloptionsFixtureSQL = `
CREATE SCHEMA golden_relopt;
CREATE TABLE golden_relopt.ff (id int) WITH (fillfactor = 90);
CREATE TABLE golden_relopt.tuned (id int)
  WITH (fillfactor = 90, autovacuum_vacuum_scale_factor = 0.01);
CREATE TABLE golden_relopt.off (id int) WITH (autovacuum_enabled = false);
CREATE TABLE golden_relopt.plain (id int);`

func TestCollectTables_GoldenReloptions(t *testing.T) {
	c := defaultsCollector(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = c.pool.Exec(context.Background(),
			"DROP SCHEMA IF EXISTS golden_relopt CASCADE")
	})
	if _, err := c.pool.Exec(ctx, reloptionsFixtureSQL); err != nil {
		t.Fatalf("reloptions fixture: %v", err)
	}
	c.WithoutConfigSnapshots()
	var relQueries int
	c.onCatalogQuery = func(_ context.Context, _ pgx.Tx, sql string, _ []any) {
		// The index_bloat read takes index fillfactors from the index
		// reloptions (rules-13); the table reloptions are counted here.
		if strings.Contains(sql, "reloptions") && sql != indexBloatSQL {
			relQueries++
		}
	}
	snap, err := c.collect(ctx)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if snap.ConfigData != nil {
		t.Fatal("precondition: ConfigData collected after WithoutConfigSnapshots")
	}
	if relQueries < 1 || relQueries > c.tablePages(snap) {
		t.Errorf("%d catalog queries read reloptions, want one per tables page (%d)",
			relQueries, c.tablePages(snap))
	}
	want := map[string][]string{
		"ff":    {"fillfactor=90"},
		"tuned": {"fillfactor=90", "autovacuum_vacuum_scale_factor=0.01"},
		"off":   {"autovacuum_enabled=false"},
		"plain": nil,
	}
	got := map[string][]string{}
	for _, ts := range snap.Tables {
		if ts.SchemaName == "golden_relopt" {
			got[ts.RelName] = ts.Reloptions
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reloptions\n got  %q\n want %q", got, want)
	}
	assertTablesCategoryCarriesReloptions(t, snap)
}

// tablePages is how many pages collectTables read for snap.
func (c *Collector) tablePages(snap *Snapshot) int {
	return len(snap.Tables)/collectorBatchSize(c.cfg) + 1
}

// assertTablesCategoryCarriesReloptions checks the persisted tables
// category, not only the in-memory snapshot.
func assertTablesCategoryCarriesReloptions(t *testing.T, snap *Snapshot) {
	t.Helper()
	rows, err := snapshotRows(snap)
	if err != nil {
		t.Fatalf("snapshotRows: %v", err)
	}
	for _, r := range rows {
		if r.Category != "tables" {
			continue
		}
		var tables []TableStats
		if err := json.Unmarshal(r.Data, &tables); err != nil {
			t.Fatalf("tables category: %v", err)
		}
		for _, ts := range tables {
			if ts.SchemaName == "golden_relopt" && ts.RelName == "ff" {
				if !reflect.DeepEqual(ts.Reloptions, []string{"fillfactor=90"}) {
					t.Fatalf("persisted ff reloptions = %q", ts.Reloptions)
				}
				if !strings.Contains(string(r.Data), `"reloptions":["fillfactor=90"]`) {
					t.Fatal("tables category JSON has no reloptions member")
				}
				return
			}
		}
		t.Fatal("golden_relopt.ff missing from the tables category")
	}
	t.Fatal("snapshot has no tables category")
}
