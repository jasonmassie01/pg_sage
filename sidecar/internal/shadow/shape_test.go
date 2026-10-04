package shadow

import (
	"strings"
	"testing"
)

// Roadmap 1.4: a shadow decision is matched to what later happened (an
// operator approval, the same change applied through pg_sage or by a
// migration) by the normalized shape of its SQL, so cosmetic differences
// (index name, CONCURRENTLY, IF [NOT] EXISTS, case, quoting, spacing,
// comments, the default access method and schema) never hide a match and
// real differences (columns, their order, uniqueness, predicate
// literals) never make one.

func TestShapeIgnoresCosmeticDifferences(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"index name, concurrently, schema, method, quoting",
			`CREATE INDEX CONCURRENTLY idx_a ON public.orders (customer_id)`,
			`create index if not exists other_name on orders using btree ("customer_id");`},
		{"spacing and comments",
			"CREATE INDEX  CONCURRENTLY i ON public.orders\n\t( a ,  b ) /* x */",
			`CREATE INDEX j ON public.orders(a,b) -- trailing`},
		{"drop forms",
			`DROP INDEX CONCURRENTLY IF EXISTS public.idx_old`,
			`drop index idx_old;`},
		{"vacuum spacing", `VACUUM  public.orders`, `vacuum public.orders`},
		{"guc spacing", `ALTER SYSTEM SET work_mem = '64MB'`, `alter system set work_mem='64MB'`},
		{"partial index", `CREATE INDEX i ON public.o (a) WHERE status = 'open'`,
			`CREATE INDEX CONCURRENTLY k ON public.o USING btree (a) WHERE status='open'`},
	} {
		if Shape(tc.a) != Shape(tc.b) {
			t.Errorf("%s: %q != %q", tc.name, Shape(tc.a), Shape(tc.b))
		}
	}
}

func TestShapeKeepsRealDifferences(t *testing.T) {
	for _, tc := range []struct{ name, a, b string }{
		{"columns", `CREATE INDEX i ON public.o (a)`, `CREATE INDEX i ON public.o (b)`},
		{"column order", `CREATE INDEX i ON public.o (a, b)`, `CREATE INDEX i ON public.o (b, a)`},
		{"uniqueness", `CREATE INDEX i ON public.o (a)`, `CREATE UNIQUE INDEX i ON public.o (a)`},
		{"table", `CREATE INDEX i ON public.o (a)`, `CREATE INDEX i ON public.p (a)`},
		{"schema", `CREATE INDEX i ON app.o (a)`, `CREATE INDEX i ON public.o (a)`},
		{"method", `CREATE INDEX i ON public.o (a)`, `CREATE INDEX i ON public.o USING hash (a)`},
		{"predicate literal case", `CREATE INDEX i ON public.o (a) WHERE s = 'Open'`,
			`CREATE INDEX i ON public.o (a) WHERE s = 'open'`},
		{"quoted mixed-case identifier", `CREATE INDEX i ON public."Orders" (a)`,
			`CREATE INDEX i ON public.orders (a)`},
		{"dropped index", `DROP INDEX public.idx_a`, `DROP INDEX public.idx_b`},
		{"guc value", `ALTER SYSTEM SET work_mem = '64MB'`, `ALTER SYSTEM SET work_mem = '32MB'`},
		{"statement kind", `VACUUM public.orders`, `ANALYZE public.orders`},
	} {
		if Shape(tc.a) == Shape(tc.b) {
			t.Errorf("%s: both shape to %q", tc.name, Shape(tc.a))
		}
	}
}

func TestShapeOfAnIndexNamesNoIndexName(t *testing.T) {
	got := Shape(`CREATE INDEX CONCURRENTLY idx_secret_name ON public.orders (a)`)
	if strings.Contains(got, "idx_secret_name") || !strings.Contains(got, "public.orders") {
		t.Fatalf("shape %q must name the table, not the index", got)
	}
	if !strings.Contains(got, "btree") {
		t.Fatalf("shape %q must make the default access method explicit", got)
	}
}

func TestShapeEmptyAndGarbage(t *testing.T) {
	if Shape("") != "" || Shape("   \n\t ") != "" || Shape("/* only a comment */") != "" {
		t.Fatal("blank input must shape to the empty string")
	}
	// Unparseable index statements still shape deterministically (as text).
	a, b := Shape("CREATE INDEX ON"), Shape("create   index on")
	if a == "" || a != b {
		t.Fatalf("truncated statement: %q vs %q", a, b)
	}
	// An unterminated literal does not panic and is kept verbatim.
	if got := Shape("ALTER SYSTEM SET work_mem = '64MB"); !strings.Contains(got, "'64MB") {
		t.Fatalf("unterminated literal: %q", got)
	}
}

func TestFingerprintIsStableAndDistinguishing(t *testing.T) {
	shape := Shape(`CREATE INDEX i ON public.o (a)`)
	a := Fingerprint("index_create", "public.o", shape)
	if len(a) != 64 || strings.Trim(a, "0123456789abcdef") != "" {
		t.Fatalf("fingerprint %q is not 64 lowercase hex characters", a)
	}
	if a != Fingerprint("index_create", "public.o", shape) {
		t.Fatal("fingerprint is not deterministic")
	}
	for _, other := range []string{
		Fingerprint("index_drop", "public.o", shape),
		Fingerprint("index_create", "public.p", shape),
		Fingerprint("index_create", "public.o", Shape(`CREATE INDEX i ON public.o (b)`)),
		// Field boundaries are not ambiguous.
		Fingerprint("index_createpublic.o", "", shape),
	} {
		if other == a {
			t.Fatal("different decisions share a fingerprint")
		}
	}
}

func TestClassOfMapsActionTypesToTheLedgerPair(t *testing.T) {
	for _, tc := range []struct{ actionType, sql, family, class string }{
		{"create_index_concurrently", `CREATE INDEX CONCURRENTLY i ON public.o (a)`,
			"tuning", "index_create"},
		{"drop_unused_index", `DROP INDEX CONCURRENTLY public.i`, "hygiene", "index_drop"},
		{"vacuum_table", `VACUUM public.o`, "hygiene", "vacuum"},
		{"analyze_table", `ANALYZE public.o`, "hygiene", "analyze"},
		{"alter_system_guc", `ALTER SYSTEM SET work_mem = '64MB'`, "tuning", "config_guc"},
		{"set_table_autovacuum",
			`ALTER TABLE public.o SET (autovacuum_vacuum_scale_factor = 0.05)`,
			"tuning", "autovacuum_tuning"},
		// Not a self-initiated class: no ledger pair, never shadowed.
		{"cancel_backend", `SELECT pg_cancel_backend(42)`, "", "backend_cancel"},
		{"", `SELECT 1`, "", "unclassified"},
	} {
		family, class := ClassOf(tc.actionType, tc.sql)
		if family != tc.family || class != tc.class {
			t.Errorf("%s: %s/%s, want %s/%s", tc.actionType, family, class, tc.family, tc.class)
		}
	}
}
