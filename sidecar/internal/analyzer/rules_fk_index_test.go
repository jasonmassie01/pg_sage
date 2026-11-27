package analyzer

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/collector"
)

// collection-04 / rules-08: missing_fk_index reads each constraint's own
// schema and its columns in conkey order. Nothing is guessed: no "public"
// fallback and no skipping of table names shared by several schemas.
//
// No concurrency test: the rule is a pure function of one snapshot.

func tenantFK(schema string, cols ...string) collector.ForeignKey {
	return collector.ForeignKey{Schema: schema, TableName: "orders", RefSchema: schema,
		ReferencedTable: "customers", ConstraintName: "orders_customer_fk", Columns: cols}
}

func tenantIndex(schema, name, cols string, scans int64) collector.IndexStats {
	return collector.IndexStats{SchemaName: schema, RelName: "orders", IndexRelName: name,
		IsValid: true, IdxScan: scans,
		IndexDef: "CREATE INDEX " + name + " ON " + schema + ".orders USING btree (" +
			cols + ")"}
}

// twoTenants has tenant_a.orders and tenant_b.orders, each with its own
// orders_customer_fk; only tenant_a has an index on customer_id.
func twoTenants() *collector.Snapshot {
	return &collector.Snapshot{
		Tables: []collector.TableStats{
			{SchemaName: "tenant_a", RelName: "orders"},
			{SchemaName: "tenant_b", RelName: "orders"},
		},
		ForeignKeys: []collector.ForeignKey{
			tenantFK("tenant_a", "customer_id"), tenantFK("tenant_b", "customer_id"),
		},
		Indexes: []collector.IndexStats{
			tenantIndex("tenant_a", "orders_cust_a", "customer_id", 10),
		},
	}
}

func TestMissingFKIndex_SchemaAwareAcrossTenants(t *testing.T) {
	got := ruleMissingFKIndexes(twoTenants(), nil, nil, nil)
	if len(got) != 1 {
		t.Fatalf("findings = %d (%+v), want exactly one, for tenant_b", len(got), got)
	}
	f := got[0]
	if f.ObjectIdentifier != "tenant_b.orders(customer_id)" {
		t.Errorf("identifier = %q, want tenant_b.orders(customer_id)", f.ObjectIdentifier)
	}
	want := `CREATE INDEX CONCURRENTLY ON "tenant_b"."orders" ("customer_id");`
	if f.RecommendedSQL != want {
		t.Errorf("SQL = %q, want %q", f.RecommendedSQL, want)
	}
	if f.Detail["constraint"] != "orders_customer_fk" ||
		f.Detail["referenced_table"] != "customers" {
		t.Errorf("detail = %+v, want the constraint and referenced table", f.Detail)
	}
}

// Happy path: the recommended index lists a composite key's columns in
// declared (conkey) order; an index on the same columns in another order
// still supports the key.
func TestMissingFKIndex_CompositeKeepsConkeyOrder(t *testing.T) {
	snap := &collector.Snapshot{
		Tables:      []collector.TableStats{{SchemaName: "app", RelName: "orders"}},
		ForeignKeys: []collector.ForeignKey{tenantFK("app", "b", "a")},
	}
	got := ruleMissingFKIndexes(snap, nil, nil, nil)
	want := `CREATE INDEX CONCURRENTLY ON "app"."orders" ("b", "a");`
	if len(got) != 1 || got[0].RecommendedSQL != want {
		t.Fatalf("findings = %+v, want one with %s", got, want)
	}
	if cols := got[0].Detail["fk_columns"]; !reflect.DeepEqual(cols, []string{"b", "a"}) {
		t.Errorf("fk_columns = %v, want [b a]", cols)
	}
	snap.Indexes = []collector.IndexStats{tenantIndex("app", "orders_ab", "a, b", 1)}
	if got := ruleMissingFKIndexes(snap, nil, nil, nil); len(got) != 0 {
		t.Fatalf("index (a, b) does support FK (b, a); findings = %+v", got)
	}
}

// Nil/empty: an FK whose table is not in Tables, or whose schema is
// unknown (a row stored before collection-04), gives no finding, and no
// finding ever names a guessed "public".
func TestMissingFKIndex_UnknownTableOrSchemaGivesNoFinding(t *testing.T) {
	cases := map[string]*collector.Snapshot{
		"table missing": {ForeignKeys: []collector.ForeignKey{tenantFK("app", "c")}},
		"legacy schema": {
			Tables:      []collector.TableStats{{SchemaName: "public", RelName: "orders"}},
			ForeignKeys: []collector.ForeignKey{tenantFK("", "c")},
		},
		"other schema": {
			Tables:      []collector.TableStats{{SchemaName: "public", RelName: "orders"}},
			ForeignKeys: []collector.ForeignKey{tenantFK("app", "c")},
		},
		"system schema": {
			Tables:      []collector.TableStats{{SchemaName: "pg_catalog", RelName: "orders"}},
			ForeignKeys: []collector.ForeignKey{tenantFK("pg_catalog", "c")},
		},
		"no columns": {
			Tables:      []collector.TableStats{{SchemaName: "app", RelName: "orders"}},
			ForeignKeys: []collector.ForeignKey{tenantFK("app")},
		},
		"no keys": {Tables: []collector.TableStats{{SchemaName: "app", RelName: "orders"}}},
	}
	for name, snap := range cases {
		for _, f := range ruleMissingFKIndexes(snap, nil, nil, nil) {
			t.Errorf("%s: unexpected finding %s (%s)", name, f.ObjectIdentifier,
				f.RecommendedSQL)
		}
	}
}

func TestBuildFKRequirements_ExactSchemaAndLegacyFailsClosed(t *testing.T) {
	snap := twoTenants()
	snap.ForeignKeys = []collector.ForeignKey{tenantFK("tenant_b", "b", "a")}
	got := buildFKRequirements(snap)
	want := map[tableKey][][]string{{"tenant_b", "orders"}: {{"b", "a"}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("requirements = %v, want %v", got, want)
	}
	snap.ForeignKeys = []collector.ForeignKey{tenantFK("", "customer_id")}
	got = buildFKRequirements(snap)
	want = map[tableKey][][]string{
		{"tenant_a", "orders"}: {{"customer_id"}},
		{"tenant_b", "orders"}: {{"customer_id"}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("legacy requirements = %v, want every candidate schema %v", got, want)
	}
	if got := buildFKRequirements(&collector.Snapshot{}); len(got) != 0 {
		t.Fatalf("no keys: requirements = %v, want none", got)
	}
}

// State: the unused-index rule protects only the tenant whose FK an index
// backs. tenant_a's sole FK index stays; tenant_b's same-named unused
// index backs no key there and is a drop candidate.
func TestUnusedIndexes_FKProtectionIsPerSchema(t *testing.T) {
	snap := twoTenants()
	snap.ForeignKeys = []collector.ForeignKey{tenantFK("tenant_a", "customer_id")}
	snap.Indexes = []collector.IndexStats{
		tenantIndex("tenant_a", "orders_cust", "customer_id", 0),
		tenantIndex("tenant_b", "orders_cust", "customer_id", 0),
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	extras := &RuleExtras{FirstSeen: map[string]time.Time{
		"tenant_a.orders_cust": old, "tenant_b.orders_cust": old,
	}, RecentlyCreated: map[string]time.Time{}}
	got := ruleUnusedIndexes(snap, nil, unusedCfg(), extras)
	if len(got) != 1 || !strings.HasPrefix(got[0].ObjectIdentifier, "tenant_b.") {
		t.Fatalf("unused findings = %+v, want only tenant_b.orders_cust", got)
	}
}
