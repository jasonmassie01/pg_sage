package snapstore

import (
	"strings"
	"testing"
)

// Change-only catalog storage (roadmap phase 3): every category the
// collector writes is either stored as changes against a base (keyed
// lists and object documents) or stored in full for a stated reason. A
// new category must make that choice explicitly: Coverage reports an
// unknown category as neither.

func TestCoverage_DeltaCategories(t *testing.T) {
	for _, cat := range []string{"tables", "indexes", "sequences", "foreign_keys",
		"partitions", "queries", "io", "config_data"} {
		delta, reason := Coverage(cat)
		if !delta || reason != "" {
			t.Errorf("%s: delta=%v reason=%q, want change-only storage", cat, delta, reason)
		}
	}
}

func TestCoverage_FullCategoriesSayWhy(t *testing.T) {
	for _, cat := range []string{"system", "locks", "replication"} {
		delta, reason := Coverage(cat)
		if delta || len(reason) < 20 {
			t.Errorf("%s: delta=%v reason=%q, want full storage with a reason", cat, delta,
				reason)
		}
	}
	_, why := Coverage("system")
	if !strings.Contains(why, "forecaster") {
		t.Errorf("system reason %q should name the raw readers it protects", why)
	}
}

func TestCoverage_UnknownCategoryIsUndecided(t *testing.T) {
	for _, cat := range []string{"", "prepared_xacts", "Tables"} {
		if delta, reason := Coverage(cat); delta || reason != "" {
			t.Errorf("%q: delta=%v reason=%q, want undecided", cat, delta, reason)
		}
	}
}

// Coverage agrees with what the writer does: a full category never gets
// a delta, a delta category is parsed for one.
func TestCoverage_AgreesWithTheEncoder(t *testing.T) {
	docs := map[string]string{
		"tables":      `[{"schemaname":"a","relname":"t"}]`,
		"config_data": `{"pg_settings":[]}`,
		"system":      `{"db_size_bytes":1}`,
		"locks":       `[]`,
		"replication": `{"replicas":[],"slots":[]}`,
	}
	for cat, doc := range docs {
		_, encodable, err := parseDocument(cat, []byte(doc))
		if err != nil {
			t.Fatalf("%s: %v", cat, err)
		}
		if delta, _ := Coverage(cat); delta != encodable {
			t.Errorf("%s: Coverage says delta=%v, encoder says %v", cat, delta, encodable)
		}
	}
}
