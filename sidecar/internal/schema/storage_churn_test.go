package schema

import (
	"strings"
	"testing"
)

// Dogfood round 2 (item 3): on lifeos the small sage tables updated in
// place sat at 15-100% dead tuples (trust_ledger_state 1 live / 3 dead,
// query_hints 1 / 8, change_lease 98 / 24, sre_budget_reservations 62 /
// 11): autovacuum's default trigger (50 rows + 20%) is never reached on a
// table this size, and pruning seldom runs on a page that never fills.
// pg_sage's own migration gives them the small-hot-table autovacuum
// settings its other small state tables already carry.
var smallStateTables = []string{"trust_ledger_state", "sre_budget_reservations",
	"change_lease", "action_queue", "verification", "query_hints", "sre_family_autonomy"}

func TestStorageMigration_SmallStateTablesVacuumAtAFewDeadRows(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	for _, table := range smallStateTables {
		var reloptions []string
		if err := pool.QueryRow(ctx, `SELECT COALESCE(reloptions, '{}') FROM pg_class
			WHERE oid = to_regclass($1)`, "sage."+table).Scan(&reloptions); err != nil {
			t.Fatalf("reloptions of %s: %v", table, err)
		}
		got := strings.Join(reloptions, ",")
		for _, o := range []string{"autovacuum_vacuum_threshold=10",
			"autovacuum_vacuum_scale_factor=0.05"} {
			if !strings.Contains(got, o) {
				t.Errorf("sage.%s reloptions = %q, want %s", table, got, o)
			}
		}
	}
}

// Setting a storage parameter rewrites the table's pg_class row; a second
// bootstrap must find them set and write nothing.
func TestStorageMigration_SecondBootstrapLeavesStorageParametersAlone(t *testing.T) {
	pool, ctx := requireDB(t)
	bootstrapWithRetry(t, ctx, pool)
	xmins := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT string_agg(relname || ':' || xmin::text, ','
			ORDER BY relname) FROM pg_class WHERE relnamespace = 'sage'::regnamespace
			AND relname = ANY($1)`, smallStateTables).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	before := xmins()
	bootstrapWithRetry(t, ctx, pool)
	if after := xmins(); after != before {
		t.Fatalf("second bootstrap rewrote storage parameters:\n%s\n->\n%s", before, after)
	}
}
