package analyzer

import "testing"

func TestIsSystemSchema(t *testing.T) {
	sys := []string{"pg_catalog", "pg_toast", "information_schema", "_timescaledb_catalog", "_timescaledb_internal", "google_ml", "PG_CATALOG"}
	for _, s := range sys {
		if !isSystemSchema(s) {
			t.Errorf("isSystemSchema(%q) = false, want true", s)
		}
	}
	user := []string{"public", "app", "lifeos", "sales", "timescaledb_information"}
	for _, s := range user {
		if isSystemSchema(s) {
			t.Errorf("isSystemSchema(%q) = true, want false", s)
		}
	}
}

func TestIsSubset_IncludeColumns(t *testing.T) {
	// a=(memo_id) INCLUDE (payload), b=(memo_id, claim_type): b does NOT
	// serve a's INCLUDE column -> a is NOT a droppable subset.
	a := ParsedIndex{Schema: "public", Table: "t", Columns: []string{"memo_id"}, IncludeCols: []string{"payload"}}
	b := ParsedIndex{Schema: "public", Table: "t", Columns: []string{"memo_id", "claim_type"}}
	if IsSubset(a, b) {
		t.Error("subset with uncovered INCLUDE column must not be a subset")
	}
	// b now includes payload -> a IS covered.
	b2 := ParsedIndex{Schema: "public", Table: "t", Columns: []string{"memo_id", "claim_type"}, IncludeCols: []string{"payload"}}
	if !IsSubset(a, b2) {
		t.Error("subset whose INCLUDE is covered should be a subset")
	}
	// plain leading-prefix subset (no INCLUDE) still works.
	a2 := ParsedIndex{Schema: "public", Table: "t", Columns: []string{"memo_id"}}
	if !IsSubset(a2, b) {
		t.Error("plain leading-prefix subset should be a subset")
	}
}
