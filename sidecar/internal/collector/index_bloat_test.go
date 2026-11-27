package collector

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// rules-13 without a database: the index_bloat read is a bounded catalog
// statement, an extra category (unknown, not empty, when it fails) and a
// persisted snapshot row.
//
// No concurrent access tests: the steps run sequentially on the collector's
// loop goroutine.

// Bounded probe: the statement opens and stat()s no relation (no
// pgstattuple, no size functions), estimates btree leaf indexes only and
// is capped by its LIMIT parameter.
func TestIndexBloatSQL_IsABoundedCatalogRead(t *testing.T) {
	sql := strings.ToLower(indexBloatSQL)
	for _, banned := range []string{"pgstattuple", "pg_relation_size",
		"pg_total_relation_size", "pg_table_size", "pg_indexes_size", "pgstatindex"} {
		if strings.Contains(sql, banned) {
			t.Errorf("index bloat SQL calls %s", banned)
		}
	}
	for _, want := range []string{"limit $2", "relkind = 'i'", "indisvalid",
		"amname = 'btree'", "pg_stats", "'sage'", ">= $1"} {
		if !strings.Contains(sql, want) {
			t.Errorf("index bloat SQL lacks %q", want)
		}
	}
	if !strings.HasPrefix(indexBloatSQL, sageTag) {
		t.Error("index bloat SQL is not tagged as pg_sage's own statement")
	}
}

// State: index_bloat is an extra category on every server version, with a
// reset, so a failed read leaves it unknown rather than half read.
func TestExtraSteps_IncludeIndexBloat(t *testing.T) {
	for _, version := range []int{140000, 170000} {
		c := New(nil, testConfig(), version, noopLog)
		found := false
		for _, st := range c.extraSteps() {
			if st.name != "index_bloat" {
				continue
			}
			found = true
			snap := &Snapshot{IndexBloat: []IndexBloat{{IndexRelName: "i"}},
				IndexBloatTruncated: true}
			st.reset(snap)
			if snap.IndexBloat != nil || snap.IndexBloatTruncated {
				t.Errorf("reset kept %+v / truncated %v", snap.IndexBloat,
					snap.IndexBloatTruncated)
			}
		}
		if !found {
			t.Errorf("version %d: no index_bloat extra step", version)
		}
	}
}

// Error propagation: a failed index_bloat read marks the category
// unavailable and clears it; the cycle goes on.
func TestExtraSteps_FailedIndexBloatIsUnavailable(t *testing.T) {
	c := New(nil, testConfig(), 170000, noopLog)
	c.overrideStep("index_bloat", func(_ context.Context, s *Snapshot) error {
		s.IndexBloat = []IndexBloat{{IndexRelName: "partial"}}
		return errors.New("permission denied for table pg_statistic")
	})
	for _, st := range c.extraSteps() {
		if st.name != "index_bloat" {
			continue
		}
		snap := &Snapshot{}
		retries := 0
		if err := c.runStep(context.Background(), st, snap, &retries); err == nil {
			t.Fatal("override error not returned")
		} else {
			st.reset(snap)
			c.markUnavailable(snap, st.name, err)
		}
		if snap.Available("index_bloat") || snap.IndexBloat != nil {
			t.Fatalf("failed read: available %v, rows %v; want unknown",
				snap.Available("index_bloat"), snap.IndexBloat)
		}
		if !strings.Contains(snap.Unavailable["index_bloat"], "pg_statistic") {
			t.Fatalf("unavailable reason = %q", snap.Unavailable["index_bloat"])
		}
		return
	}
	t.Fatal("no index_bloat extra step")
}

// Nil/empty: an empty reading persists [], an unknown one no row, and a
// row keeps the estimate under its JSON names.
func TestSnapshotRows_IndexBloat(t *testing.T) {
	empty := rowCategories(t, &Snapshot{IndexBloat: []IndexBloat{}})
	if data, ok := empty["index_bloat"]; !ok || data != "[]" {
		t.Fatalf("empty reading: row %q present %v, want []", data, ok)
	}
	unknown := rowCategories(t, &Snapshot{IndexBloat: []IndexBloat{},
		Unavailable: map[string]string{"index_bloat": "statement timeout"}})
	if _, ok := unknown["index_bloat"]; ok {
		t.Fatal("unknown reading persisted a row")
	}
	one := rowCategories(t, &Snapshot{IndexBloat: []IndexBloat{{SchemaName: "app",
		RelName: "orders", IndexRelName: "orders_idx", BloatPct: 71.5, RelPages: 100}}})
	for _, want := range []string{`"schemaname":"app"`, `"indexrelname":"orders_idx"`,
		`"bloat_pct":71.5`, `"relpages":100`} {
		if !strings.Contains(one["index_bloat"], want) {
			t.Errorf("index_bloat row %s lacks %s", one["index_bloat"], want)
		}
	}
}
