package migration

import (
	"reflect"
	"testing"
)

// No concurrent access tests: the classifier's rules are compiled once
// and Classify keeps no state between calls.

// versionCorpus touches every rule that used to branch on the server
// version (ADD COLUMN DEFAULT, ADD COLUMN NOT NULL, REINDEX).
var versionCorpus = []string{
	"ALTER TABLE t ADD COLUMN status text DEFAULT 'active'",
	"ALTER TABLE t ADD COLUMN status text NOT NULL",
	"ALTER TABLE t ADD COLUMN status text NOT NULL DEFAULT 'x'",
	"ALTER TABLE t ADD COLUMN id uuid DEFAULT gen_random_uuid()",
	"REINDEX TABLE orders",
	"REINDEX TABLE CONCURRENTLY orders",
	"VACUUM FULL orders",
	"CLUSTER orders USING orders_pkey",
	"CREATE INDEX i ON orders (a)",
	"",
}

// Regression (migration-12): the floor is PG 14, so the classifier's
// output must not depend on the version it is given: 0 (unknown), the
// floor and the newest supported major all classify alike.
func TestClassifyIsVersionIndependentOnSupportedServers(t *testing.T) {
	c := NewRegexClassifier()
	for _, sql := range versionCorpus {
		base := c.Classify(sql, 0)
		for _, v := range []int{140000, 180000, 14, 18} {
			if got := c.Classify(sql, v); !reflect.DeepEqual(got, base) {
				t.Errorf("Classify(%q, %d) = %+v, want %+v (version 0)", sql, v, got, base)
			}
		}
	}
}

// The PG < 11 ADD COLUMN NOT NULL rule is gone: on PG 11+ a new NOT NULL
// column without a DEFAULT is a catalog-only change on an empty table and
// fails fast on a populated one; it never rewrites.
func TestAddColumnNotNullRuleRemoved(t *testing.T) {
	for _, r := range ruleCatalog() {
		if r.ID == "ddl_add_column_not_null" {
			t.Fatal("ruleCatalog still defines ddl_add_column_not_null (PG < 11 only)")
		}
	}
	got := NewRegexClassifier().Classify("ALTER TABLE t ADD COLUMN s text NOT NULL", 0)
	for _, cl := range got {
		if cl.RequiresRewrite {
			t.Errorf("ADD COLUMN NOT NULL classified as a rewrite: %+v", cl)
		}
	}
}

// A constant DEFAULT never rewrites (PG 11+), and REINDEX without
// CONCURRENTLY is always flagged (PG 12+), whatever version is passed.
func TestSupportedFloorSemanticsHold(t *testing.T) {
	c := NewRegexClassifier()
	for _, v := range []int{0, 140000, 180000} {
		requireRuleAbsent(t, c.Classify(
			"ALTER TABLE t ADD COLUMN s text DEFAULT 'a'", v), "ddl_add_column_volatile_default")
		requireRulePresent(t, c.Classify("REINDEX TABLE orders", v),
			"ddl_reindex_not_concurrent")
	}
}

// MinPGVersion was copied onto every classification but never enforced.
func TestClassificationHasNoMinPGVersion(t *testing.T) {
	if _, ok := reflect.TypeOf(DDLClassification{}).FieldByName("MinPGVersion"); ok {
		t.Fatal("DDLClassification still carries the unenforced MinPGVersion")
	}
	if _, ok := reflect.TypeOf(ruleDefinition{}).FieldByName("MinPGVersion"); ok {
		t.Fatal("ruleDefinition still carries the unenforced MinPGVersion")
	}
}
