//go:build !cgo

package rca

import (
	"context"
	"strings"
	"testing"
)

// CHECK-06: without the parser no Tier 2 SQL can be proven, so all of it
// is withheld, even a plainly read-only statement.
func TestTier2WithholdsAllSQLWithoutCgo(t *testing.T) {
	inc := buildTier2Incident(tier2Response{RootCause: "x",
		RecommendedSQL: []string{"SELECT 1"}}, testSignals(1))
	vetModelSQL(context.Background(), emptyCatalog{}, nil, &inc, noopLog)
	if inc.RecommendedSQL != "" {
		t.Fatalf("RecommendedSQL = %q, want withheld without cgo", inc.RecommendedSQL)
	}
	if !strings.Contains(withheldNote(inc), "no cgo") {
		t.Fatalf("note = %q, want the missing parser named", withheldNote(inc))
	}
	eng := tier2SQLEngine(t, []string{"SELECT pid FROM pg_stat_activity"})
	got := eng.runTier2Correlation(testSignals(3), nil)
	if len(got) != 1 || got[0].RecommendedSQL != "" || withheldNote(got[0]) == "" {
		t.Fatalf("engine kept unproven SQL: %+v", got)
	}
}
