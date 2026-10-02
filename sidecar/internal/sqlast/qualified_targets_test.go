//go:build cgo

package sqlast

import (
	"strings"
	"testing"
)

// Phase 0 #4: an unqualified destructive target resolves through the
// session search_path at execution time ("$user" can be sage, pg_catalog
// is always searched first), so the protected-schema check cannot see it.
// Destructive statements must name their schema.
func TestCheckRejectsUnqualifiedDestructiveTargets(t *testing.T) {
	for sql, want := range map[string]string{
		"DROP INDEX CONCURRENTLY idx_orders_old":                      "schema-qualified",
		"DROP INDEX CONCURRENTLY IF EXISTS idx_orders_old":            "schema-qualified",
		"DROP INDEX pg_class_oid_index":                               "schema-qualified",
		`DROP INDEX CONCURRENTLY "findings_pkey"`:                     "schema-qualified",
		"ALTER TABLE orders SET (autovacuum_enabled = false)":         "schema-qualified",
		"ALTER TABLE IF EXISTS action_log RESET (fillfactor)":         "schema-qualified",
		"ALTER TABLE ONLY findings DROP CONSTRAINT findings_pkey":     "schema-qualified",
		"ALTER TABLE orders VALIDATE CONSTRAINT orders_x_nn":          "schema-qualified",
		"DROP INDEX CONCURRENTLY postgres.sage.idx_findings_category": "protected schema",
		"DROP INDEX CONCURRENTLY app.pg_catalog.pg_class_oid_index":   "protected schema",
		`DROP INDEX CONCURRENTLY U&"\0073age".idx`:                    "protected schema",
		`DROP INDEX CONCURRENTLY "SAGE".idx`:                          "protected schema",
		"DROP INDEX CONCURRENTLY PG_CATALOG.pg_class_oid_index":       "protected schema",
		"ALTER TABLE sage.findings SET (fillfactor = 50)":             "protected schema",
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

func TestCheckAcceptsQualifiedDestructiveTargets(t *testing.T) {
	for _, sql := range []string{
		"DROP INDEX CONCURRENTLY public.idx_orders_old",
		`DROP INDEX CONCURRENTLY IF EXISTS "App"."Idx Old"`,
		"DROP INDEX CONCURRENTLY appdb.public.idx_orders_old",
		"ALTER TABLE public.orders SET (autovacuum_vacuum_scale_factor = 0.05)",
		`ALTER TABLE ONLY "public"."orders" DROP CONSTRAINT orders_x_nn`,
	} {
		if err := Check(sql, testRules); err != nil {
			t.Errorf("Check(%q) = %v, want accepted", sql, err)
		}
	}
}

// Non-destructive maintenance keeps accepting unqualified names: ANALYZE
// and VACUUM of any table cannot remove data or definitions.
func TestCheckKeepsUnqualifiedMaintenance(t *testing.T) {
	for _, sql := range []string{"ANALYZE orders", "VACUUM orders"} {
		if err := Check(sql, testRules); err != nil {
			t.Errorf("Check(%q) = %v, want accepted", sql, err)
		}
	}
}

// A nil ProtectedSchema still enforces qualification: the requirement does
// not depend on an allowlist being configured.
func TestCheckRequiresQualificationWithoutProtectedSchemaRule(t *testing.T) {
	err := Check("DROP INDEX CONCURRENTLY idx_orders_old", Rules{})
	if err == nil || !strings.Contains(err.Error(), "schema-qualified") {
		t.Fatalf("Check with empty rules = %v, want schema-qualified refusal", err)
	}
}
