//go:build cgo

package rca

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func vetOne(t *testing.T, sql string, logFn func(string, string, ...any)) Incident {
	t.Helper()
	inc := buildTier2Incident(tier2Response{RootCause: "x",
		RecommendedSQL: []string{sql}}, testSignals(1))
	vetModelSQL(context.Background(), emptyCatalog{}, nil, &inc, logFn)
	return inc
}

func TestTier2ProofReasons(t *testing.T) {
	cases := map[string]string{
		"DROP TABLE t": "only a SELECT",
		"WITH d AS (DELETE FROM t RETURNING 1) SELECT 1":      "data-modifying",
		"SELECT * FROM t FOR UPDATE":                          "row locking",
		"SELECT query_to_xml('DROP TABLE t', true, true, '')": "runs SQL",
	}
	for sql, reason := range cases {
		inc := vetOne(t, sql, noopLog)
		if inc.RecommendedSQL != "" || !strings.Contains(withheldNote(inc), reason) {
			t.Errorf("%s: SQL %q note %q, want withheld because %q", sql,
				inc.RecommendedSQL, withheldNote(inc), reason)
		}
	}
}

// A failed catalog read withholds the SQL, and the WARN names the read.
func TestTier2ProofCatalogErrorWithholds(t *testing.T) {
	var logs []string
	logFn := func(level, msg string, args ...any) {
		logs = append(logs, level+": "+fmt.Sprintf(msg, args...))
	}
	inc := vetOne(t, "SELECT pg_terminate_backend(123)", logFn)
	if inc.RecommendedSQL != "" {
		t.Fatalf("SQL kept after a catalog error: %q", inc.RecommendedSQL)
	}
	if !strings.Contains(withheldNote(inc), "could not be checked against the catalog") {
		t.Errorf("note = %q", withheldNote(inc))
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "warn: ") || !strings.Contains(joined, "resolve functions") ||
		!strings.Contains(joined, "pg_terminate_backend") {
		t.Errorf("logs do not name the failed catalog read:\n%s", joined)
	}
}
