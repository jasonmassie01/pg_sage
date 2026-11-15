package schema

import (
	"strings"
	"testing"
)

// lab-02: sage.verifier_backtest is one T5 step, right after lab-01c's
// verifier columns, created only with IF NOT EXISTS under lock_timeout.
// Later T5 items append after it, so the pin is its place, not "last".
func TestVerifierBacktestMigration_FollowsTheVerifierColumns(t *testing.T) {
	steps := trancheFiveMigrations()
	idx := -1
	for i, step := range steps {
		if step[0] == "ddlVerifierBacktest" && step[1] == ddlVerifierBacktest {
			idx = i
		}
	}
	if idx < 1 || steps[idx-1][0] != "ddlActionOutcomeVerifier" {
		t.Fatalf("ddlVerifierBacktest at %d of tranche five, want after "+
			"ddlActionOutcomeVerifier", idx)
	}
	var naming []string
	for _, s := range migrationSteps() {
		if strings.Contains(s.sql, "sage.verifier_backtest") {
			naming = append(naming, s.name)
		}
	}
	if len(naming) != 1 || naming[0] != "ddlVerifierBacktest" {
		t.Fatalf("steps naming sage.verifier_backtest = %v", naming)
	}
	for _, want := range []string{"SET LOCAL lock_timeout",
		"CREATE TABLE IF NOT EXISTS sage.verifier_backtest", "CREATE INDEX IF NOT EXISTS",
		"fp_wilson_upper", "inconclusive_share", "report", "seed"} {
		if !strings.Contains(ddlVerifierBacktest, want) {
			t.Errorf("ddlVerifierBacktest lacks %q", want)
		}
	}
	for _, bad := range []string{"DROP ", "TRUNCATE", "DELETE "} {
		if strings.Contains(ddlVerifierBacktest, bad) {
			t.Errorf("ddlVerifierBacktest runs %q", bad)
		}
	}
}
