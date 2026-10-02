package runbook

import (
	"reflect"
	"testing"

	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// A decision may only name a column a catalog probe returns. The column
// set is read from the probe's fixed SQL (the main SELECT list), so a
// probe added to the catalog later is covered without another list to
// maintain; columns_db_test.go checks it against real result columns.

func TestColumnsOf_ReadsTheMainSelectList(t *testing.T) {
	cases := map[string]struct {
		sql  string
		want []string
	}{
		"aliases and qualified columns": {
			sql: `/* pg_sage sre:x v1 */ SELECT a.pid, a.state AS st,
				EXTRACT(EPOCH FROM now() - a.xact_start)::float8 AS age_s,
				count(*)::int8 AS n, b.total
				FROM pg_stat_activity a, t b LIMIT $1`,
			want: []string{"pid", "st", "age_s", "n", "total"}},
		"CTE bodies are skipped": {
			sql: `WITH RECURSIVE c AS (SELECT x AS inner_col FROM y),
				d AS (SELECT 1 AS other FROM z)
				SELECT c.inner_col AS outer_col, d.other FROM c, d`,
			want: []string{"outer_col", "other"}},
		"distinct, casts and function names": {
			sql: `SELECT DISTINCT p.owner::text, pg_catalog.md5(p.gid),
				CASE WHEN x THEN 'a' ELSE 'b' END AS kind FROM p`,
			want: []string{"owner", "md5", "kind"}},
		"union takes the first branch's names": {
			sql:  `SELECT 1 AS a, 2 AS b UNION ALL SELECT 3 AS c, 4 AS d`,
			want: []string{"a", "b"}},
		"line comments": {
			sql:  "SELECT x AS kept -- , y AS dropped\n, z FROM t",
			want: []string{"kept", "z"}},
		"no select":  {sql: `VALUES (1)`, want: nil},
		"empty text": {sql: ``, want: nil},
	}
	for name, c := range cases {
		if got := ColumnsOf(c.sql); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: ColumnsOf = %v, want %v", name, got, c.want)
		}
	}
}

func TestHasColumn_KnowsTheCatalog(t *testing.T) {
	known := map[probes.ID][]string{
		probes.LockGraph:            {"blocker_pid", "blocker_xact_age_s", "requested_mode"},
		probes.LongTransactions:     {"pid", "xact_age_s", "state", "backend_xmin_age"},
		probes.ConnectionSaturation: {"backends", "max_connections", "application_name"},
		probes.ReplicationSlots:     {"retained_bytes", "active", "wal_status"},
		probes.PreparedXacts:        {"prepared_age_s", "xid_age"},
	}
	for id, cols := range known {
		for _, c := range cols {
			if !HasColumn(id, c) {
				t.Errorf("HasColumn(%s, %s) = false", id, c)
			}
		}
	}
	for _, bad := range []struct {
		id  probes.ID
		col string
	}{{probes.LockGraph, "query"}, {probes.LockGraph, ""}, {"nope", "pid"},
		{probes.LongTransactions, "a.pid"}, {probes.LongTransactions, "PID"}} {
		if HasColumn(bad.id, bad.col) {
			t.Errorf("HasColumn(%s, %q) = true", bad.id, bad.col)
		}
	}
}

func TestOutputColumns_EveryCatalogProbeHasColumns(t *testing.T) {
	for _, id := range probes.Catalog().IDs() {
		spec, _ := probes.Catalog().Spec(id)
		if cols := OutputColumns(spec); len(cols) == 0 {
			t.Errorf("probe %s: no output columns read from its SQL", id)
		}
	}
	if cols := OutputColumns(probes.Spec{}); cols != nil {
		t.Errorf("spec without SQL: columns = %v", cols)
	}
}
