//go:build cgo

package impact

import "testing"

// Dropping an index the catalog does not hold is unknown (the change
// fails), unless the script creates it first or says IF EXISTS.
func TestDropMissingIndexIsUnknown(t *testing.T) {
	e := newEnv(t)
	a := e.analyze("DROP INDEX rv.rv_nope_idx")
	if len(unknownWith(a, "rv.rv_nope_idx")) != 1 {
		t.Fatalf("want CR-17 for the missing index: %s", describe(a))
	}
	none(t, e.analyze("DROP INDEX IF EXISTS rv.rv_nope_idx"), RuleImpactUnknown)
	b := e.analyze("CREATE INDEX CONCURRENTLY rv_new_idx ON rv.rv_orders (status);\n" +
		"DROP INDEX CONCURRENTLY rv.rv_new_idx")
	if len(unknownWith(b, "rv.rv_new_idx")) != 0 {
		t.Fatalf("an index the script creates is not unknown: %s", describe(b))
	}
}
