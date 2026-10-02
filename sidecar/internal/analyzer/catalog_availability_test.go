package analyzer

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/collector"
)

// Dogfood lifeos-1 finding 8: a catalog category the collector could not
// read is unavailable, not empty. Rules that would read absence as a
// fact (every foreign key "lacks" an index when the index list is
// missing; an index "supports no" foreign key when the key list is
// missing) do not run, so their open findings are neither raised nor
// resolved.

func ruleNeeds(t *testing.T, name string) func(cur, prev *collector.Snapshot) bool {
	t.Helper()
	for _, r := range AllRules {
		if r.Name == name {
			return r.Needs
		}
	}
	t.Fatalf("no rule %s", name)
	return nil
}

func availabilitySnapshot(unavailable ...string) *collector.Snapshot {
	s := &collector.Snapshot{
		Indexes:     []collector.IndexStats{{SchemaName: "public", IndexRelName: "i"}},
		ForeignKeys: []collector.ForeignKey{{TableName: "t", ReferencedTable: "r"}},
		Tables:      []collector.TableStats{{SchemaName: "public", RelName: "t"}},
	}
	for _, c := range unavailable {
		if s.Unavailable == nil {
			s.Unavailable = map[string]string{}
		}
		s.Unavailable[c] = "statement timeout"
	}
	return s
}

func TestRuleNeeds_UnavailableCategories(t *testing.T) {
	cases := []struct {
		rule        string
		unavailable []string
		want        bool
	}{
		{"missing_fk_indexes", nil, true},
		{"missing_fk_indexes", []string{"indexes"}, false},
		{"missing_fk_indexes", []string{"foreign_keys"}, false},
		{"unused_indexes", nil, true},
		{"unused_indexes", []string{"indexes"}, false},
		{"unused_indexes", []string{"foreign_keys"}, false},
		{"duplicate_indexes", []string{"indexes"}, false},
		{"duplicate_indexes", []string{"tables"}, true},
		{"invalid_indexes", []string{"foreign_keys"}, true},
		{"table_bloat", []string{"tables"}, false},
		{"table_bloat", []string{"indexes"}, true},
	}
	for _, c := range cases {
		got := ruleNeeds(t, c.rule)(availabilitySnapshot(c.unavailable...), nil)
		if got != c.want {
			t.Errorf("%s with %v unavailable: needs = %v, want %v", c.rule, c.unavailable,
				got, c.want)
		}
	}
}

// Nil/empty: a snapshot that never recorded availability (older code,
// tests) is fully available.
func TestSnapshotAvailable_NilMapMeansAvailable(t *testing.T) {
	var s collector.Snapshot
	for _, c := range []string{"indexes", "tables", "anything"} {
		if !s.Available(c) {
			t.Fatalf("%s unavailable on a zero snapshot", c)
		}
	}
}
