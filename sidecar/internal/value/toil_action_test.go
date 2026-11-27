package value

import "testing"

// earned-04: one mapping from an action type (or a trust-ledger action
// class) to its sage.toil_model row, shared by the Value page credit and
// the shadow ledger's potential minutes.
// No concurrent access tests: both functions are pure lookups.

func TestToilActionTypeMapsLegacyNamesAndPassesTheRestThrough(t *testing.T) {
	cases := map[string]string{
		"create_index":              "create_index_concurrently",
		"drop_index":                "drop_unused_index",
		"reindex":                   "reindex_concurrently",
		"vacuum":                    "vacuum_table",
		"analyze":                   "analyze_table",
		"create_index_concurrently": "create_index_concurrently",
		"analyze_table":             "analyze_table",
		"freeze_table":              "freeze_table",
		"slot_drop":                 "slot_drop",
		"mystery":                   "mystery",
		"":                          "",
	}
	for in, want := range cases {
		if got := ToilActionType(in); got != want {
			t.Errorf("ToilActionType(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestToilActionTypeForClassMapsEveryModelledClass(t *testing.T) {
	cases := map[string]string{
		"index_create":      "create_index_concurrently",
		"index_drop":        "drop_unused_index",
		"reindex":           "reindex_concurrently",
		"vacuum":            "vacuum_table",
		"analyze":           "analyze_table",
		"freeze":            "freeze_table",
		"autovacuum_tuning": "set_table_autovacuum",
		"statistics":        "create_statistics",
		"query_hint":        "apply_query_hint",
		"config_guc":        "alter_system_guc",
	}
	for in, want := range cases {
		if got := ToilActionTypeForClass(in); got != want {
			t.Errorf("ToilActionTypeForClass(%q) = %q, want %q", in, got, want)
		}
	}
}

// A class without a toil model maps to "", never to a default model: the
// shadow page then says "not modelled" instead of inventing minutes.
func TestToilActionTypeForClassHasNoDefault(t *testing.T) {
	for _, in := range []string{"", "bogus", "wal_bound", "slot_drop", "unclassified",
		"INDEX_CREATE", " index_create", "create_index_concurrently", "*"} {
		if got := ToilActionTypeForClass(in); got != "" {
			t.Errorf("ToilActionTypeForClass(%q) = %q, want \"\"", in, got)
		}
	}
}
