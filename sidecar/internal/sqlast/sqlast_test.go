//go:build cgo

package sqlast

import (
	"strings"
	"testing"
)

var testRules = Rules{
	SystemParam:   func(name string) bool { return name == "work_mem" },
	DatabaseParam: func(name string) bool { return name == "work_mem" },
	ProtectedSchema: func(schema string) bool {
		switch schema {
		case "sage", "pg_catalog", "information_schema", "pg_toast":
			return true
		}
		return false
	},
}

// The cgo build is what CI, Docker images and releases ship; the AST layer
// must be active there (the no-cgo fallback only serves local builds).
func TestAvailableWithCgo(t *testing.T) {
	if !Available() {
		t.Fatal("AST validation unavailable in a cgo build")
	}
}

func TestCheckAcceptsExecutorStatements(t *testing.T) {
	for _, sql := range []string{
		"CREATE INDEX CONCURRENTLY idx_orders_status ON public.orders (status)",
		`CREATE UNIQUE INDEX CONCURRENTLY "Idx" ON "App"."Orders" (id) WHERE deleted_at IS NULL`,
		"CREATE INDEX idx_small ON public.lookup (code)",
		"DROP INDEX CONCURRENTLY public.idx_orders_old",
		"DROP INDEX CONCURRENTLY IF EXISTS public.idx_orders_old;",
		"REINDEX INDEX CONCURRENTLY public.idx_orders_status",
		"REINDEX TABLE CONCURRENTLY public.orders",
		"VACUUM public.orders",
		"VACUUM (ANALYZE, VERBOSE) public.orders",
		"VACUUM (FREEZE) public.orders",
		"ANALYZE public.orders",
		"ANALYZE",
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.05)",
		"ALTER TABLE public.orders RESET (autovacuum_vacuum_scale_factor)",
		"ALTER TABLE public.orders ADD CONSTRAINT orders_x_nn CHECK (x IS NOT NULL) NOT VALID",
		"ALTER TABLE public.orders VALIDATE CONSTRAINT orders_x_nn",
		"ALTER TABLE public.orders ALTER COLUMN x SET NOT NULL",
		"ALTER TABLE public.orders DROP CONSTRAINT orders_x_nn",
		"ALTER TABLE public.orders ADD CONSTRAINT orders_code_key UNIQUE USING INDEX idx_code",
		"ALTER SYSTEM SET work_mem = '64MB'",
		"ALTER SYSTEM RESET work_mem",
		"ALTER DATABASE app SET work_mem = '32MB'",
		"SELECT pg_cancel_backend(4711)",
		"SELECT pg_terminate_backend(4711);",
		"INSERT INTO hint_plan.hints (query_id, application_name, hints) VALUES (1, '', 'SeqScan(t)')",
		"DELETE FROM hint_plan.hints WHERE query_id = 1",
	} {
		if err := Check(sql, testRules); err != nil {
			t.Errorf("Check(%q) = %v, want accepted", sql, err)
		}
	}
}

func TestCheckRejectsByStructure(t *testing.T) {
	for sql, want := range map[string]string{
		"":                                   "no statement",
		"ANALYZE public.a; ANALYZE public.b": "exactly one statement",
		"DROP TABLE public.orders":           "only indexes",
		"DROP INDEX public.a, public.b":      "one index",
		"DROP INDEX CONCURRENTLY public.idx CASCADE":                  "CASCADE",
		"TRUNCATE public.orders":                                      "not allowed",
		"CREATE TABLE public.x (id int)":                              "not allowed",
		"VACUUM FULL public.orders":                                   "FULL",
		"VACUUM (FULL) public.orders":                                 "FULL",
		"ALTER TABLE public.orders ALTER COLUMN amount TYPE numeric":  "subcommand",
		"ALTER TABLE public.orders SET (fillfactor = 70), SET LOGGED": "exactly one subcommand",
		"ALTER TABLE public.orders OWNER TO attacker":                 "subcommand",
		"ALTER TABLE public.orders ADD CONSTRAINT c CHECK (x > 0)":    "constraint",
		"ALTER SYSTEM SET shared_preload_libraries = 'evil'":          "parameter",
		"ALTER DATABASE app OWNER TO attacker":                        "not allowed",
		"ALTER DATABASE app SET search_path = 'evil'":                 "parameter",
		"SELECT pg_cancel_backend(pid) FROM pg_stat_activity":         "one literal",
		"SELECT pg_terminate_backend(1), pg_terminate_backend(2)":     "one literal",
		"SELECT pg_read_file('/etc/passwd')":                          "function",
		"SELECT 1":                                                    "function",
		"INSERT INTO public.orders VALUES (1)":                        "hint_plan.hints",
		"DELETE FROM hint_plan.hints":                                 "WHERE",
		"UPDATE hint_plan.hints SET hints = ''":                       "not allowed",
		"CREATE INDEX CONCURRENTLY i ON sage.findings (id)":           "protected schema",
		"ANALYZE pg_catalog.pg_class":                                 "protected schema",
		"VACUUM sage.action_log":                                      "protected schema",
		"DROP INDEX CONCURRENTLY information_schema.x":                "protected schema",
		"REINDEX SYSTEM CONCURRENTLY app":                             "REINDEX",
		"COPY public.orders TO PROGRAM 'id'":                          "not allowed",
		"DO $$ BEGIN PERFORM pg_sleep(1); END $$":                     "not allowed",
		"SET statement_timeout = 0":                                   "not allowed",
		"not sql at all":                                              "parse",
	} {
		err := Check(sql, testRules)
		if err == nil {
			t.Errorf("Check(%q) accepted, want refusal mentioning %q", sql, want)
			continue
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("Check(%q) = %v, want it to mention %q", sql, err, want)
		}
	}
}

// Obfuscations that fool text matching are plain structure to the parser.
func TestCheckSeesThroughTextTricks(t *testing.T) {
	for _, sql := range []string{
		`ANALYZE "sage".findings`,
		`ANALYZE U&"\0073age".findings`,
		`VACUUM /* comment */ sage.findings`,
		"ANALYZE public.orders; -- trailing\nDROP TABLE public.orders",
		"SELECT pg_cancel_backend(4711) UNION SELECT pg_terminate_backend(1)",
		"SELECT pg_catalog.pg_read_file('x')",
	} {
		if err := Check(sql, testRules); err == nil {
			t.Errorf("Check(%q) accepted a disguised statement", sql)
		}
	}
}
