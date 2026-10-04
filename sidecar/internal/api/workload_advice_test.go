package api

import "testing"

// Dogfood round 2: hint cases are workload advice; a hint row whose query
// is diagnostic tooling (EXPLAIN, maintenance) is left out like pg_sage's
// own statements.
func TestFilterHintRowsDropsDiagnosticStatements(t *testing.T) {
	rows := []map[string]any{
		{"queryid": int64(1), "query_text": "EXPLAIN ANALYZE SELECT * FROM orders"},
		{"queryid": int64(2), "query_text": "VACUUM orders"},
		{"queryid": int64(3), "query_text": "/* pg_sage */ SELECT 1"},
		{"queryid": int64(4), "query_text": "SELECT * FROM orders WHERE id = $1"},
		{"queryid": int64(5)},
	}
	got := filterSelfMonitoringHintRows(rows)
	if len(got) != 2 || got[0]["queryid"] != int64(4) || got[1]["queryid"] != int64(5) {
		t.Fatalf("kept %v, want the application query 4 and the row without text", got)
	}
}
