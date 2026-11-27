package selfmonitor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

type findingCase struct {
	name string
	in   FindingFields
	want bool
}

// footprint-06: which findings are about pg_sage itself. The self
// categories (pg_sage's own cost, budget and footprint) are kept whatever
// they name; the bare identifier sage is pg_sage's schema only when the
// object is a schema (a database or user table may be called sage); a
// schema-qualified sage. object is pg_sage's whatever its type.
// No concurrent access tests: IsFinding is a pure function.
var findingCases = []findingCase{
	{"self cost on a database named sage", FindingFields{Category: "sage_self_cost",
		ObjectType: "database", ObjectIdentifier: "sage",
		Title: "pg_sage monitoring used 4200 ms of database time on sage"}, false},
	{"self budget names pg_sage statements", FindingFields{Category: "sage_self_budget",
		ObjectType: "database", ObjectIdentifier: "sage",
		Title:  "pg_sage monitoring exceeded its own cpu budget on sage",
		Detail: map[string]any{"query": "/* pg_sage */ SELECT 1 FROM sage.findings"}}, false},
	{"footprint on a database named sage", FindingFields{Category: "sage_footprint",
		ObjectType: "database", ObjectIdentifier: "SAGE",
		Title: "pg_sage data uses 23.4% of database sage (limit 10%)"}, false},
	{"bare sage database, other category", FindingFields{Category: "db_size",
		ObjectType: "database", ObjectIdentifier: "sage", Title: "Database grows"}, false},
	{"user table named sage", FindingFields{Category: "table_bloat",
		ObjectType: "table", ObjectIdentifier: "sage", Title: "Table bloat"}, false},
	{"the sage schema", FindingFields{Category: "schema_lint:x",
		ObjectType: "schema", ObjectIdentifier: "sage", Title: "Schema check"}, true},
	{"the sage schema, type in capitals", FindingFields{Category: "schema_lint:x",
		ObjectType: " Schema ", ObjectIdentifier: " Sage ", Title: "Schema check"}, true},
	{"index in the sage schema", FindingFields{Category: "unused_index",
		ObjectType: "index", ObjectIdentifier: "sage.snapshots_pkey",
		Title: "Unused index"}, true},
	{"sage schema padded with a tab and newline", FindingFields{Category: "schema_lint:x",
		ObjectType: "schema\t", ObjectIdentifier: "\tsage\n", Title: "Schema check"}, true},
	{"sage index padded with a tab", FindingFields{Category: "unused_index",
		ObjectType: "index", ObjectIdentifier: "\tsage.findings_pkey", Title: "x"}, true},
	{"quoted sage table", FindingFields{Category: "table_bloat",
		ObjectType: "table", ObjectIdentifier: `"sage".findings`, Title: "Bloat"}, true},
	{"sage-prefixed index without a type", FindingFields{Category: "unused_index",
		ObjectIdentifier: "sage.findings_pkey", Title: "Unused index"}, true},
	{"user table pg_sage_events", FindingFields{Category: "table_bloat",
		ObjectType: "table", ObjectIdentifier: "public.pg_sage_events",
		Title: "Table bloat"}, false},
	{"user schema sagebrush", FindingFields{Category: "table_bloat",
		ObjectType: "table", ObjectIdentifier: "sagebrush.orders", Title: "Bloat"}, false},
	{"near-miss category is not exempt", FindingFields{Category: "sage_self_cost_v2",
		ObjectType: "index", ObjectIdentifier: "sage.findings_pkey", Title: "x"}, true},
	{"pg_sage title, other category", FindingFields{Category: "slow_query",
		ObjectType: "query", ObjectIdentifier: "queryid:1",
		Title: "Slow pg_sage query"}, true},
	{"tagged statement, other category", FindingFields{Category: "slow_query",
		ObjectType: "query", ObjectIdentifier: "queryid:2", Title: "Slow query",
		Detail: map[string]any{"query": "/* pg_sage */ SELECT 1"}}, true},
	{"monitor catalog read", FindingFields{Category: "slow_query",
		ObjectType: "query", ObjectIdentifier: "queryid:3", Title: "Slow query",
		Detail: map[string]any{"query": "SELECT * FROM pg_stat_statements"}}, false},
	{"zero value", FindingFields{}, false},
}

func TestIsFinding_SelfIdentity(t *testing.T) {
	for _, tc := range findingCases {
		if got := IsFinding(tc.in); got != tc.want {
			t.Errorf("%s: IsFinding = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestSelfFindingCategories(t *testing.T) {
	want := map[string]bool{"sage_self_cost": true, "sage_self_budget": true,
		"sage_footprint": true}
	if len(SelfFindingCategories) != len(want) {
		t.Fatalf("SelfFindingCategories = %v, want %v", SelfFindingCategories, want)
	}
	for _, c := range SelfFindingCategories {
		if !want[c] {
			t.Errorf("unexpected self category %q", c)
		}
	}
}

// The SQL twin hides exactly the rows IsFinding drops, on real
// PostgreSQL (regex and ILIKE semantics included).
func TestFindingsSQLExclusionClause_SelfParityOnRealPostgres(t *testing.T) {
	dsn := testdb.SkipUnlessLive(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	q := `SELECT count(*) FROM (SELECT $1::text AS category, $2::text AS object_type,
		$3::text AS object_identifier, $4::text AS title, $5::text AS recommended_sql,
		$6::text AS rollback_sql, $7::jsonb AS detail) f WHERE 1=1` +
		FindingsSQLExclusionClause()
	for _, tc := range findingCases {
		detail := []byte("{}")
		if tc.in.Detail != nil {
			if detail, err = json.Marshal(tc.in.Detail); err != nil {
				t.Fatal(err)
			}
		}
		var kept int
		err := pool.QueryRow(ctx, q, tc.in.Category, tc.in.ObjectType,
			tc.in.ObjectIdentifier, tc.in.Title, tc.in.RecommendedSQL,
			tc.in.RollbackSQL, string(detail)).Scan(&kept)
		if err != nil {
			t.Fatalf("%s: evaluate clause: %v", tc.name, err)
		}
		if hidden := kept == 0; hidden != tc.want {
			t.Errorf("%s: SQL hides = %v, Go IsFinding = %v", tc.name, hidden, tc.want)
		}
	}
}
