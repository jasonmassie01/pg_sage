package schema

import (
	"strings"
	"testing"
)

// lab-02: sage.verifier_backtest is one T5 step, the last, created only
// with IF NOT EXISTS under lock_timeout.
func TestVerifierBacktestMigration_IsTheLastT5Step(t *testing.T) {
	steps := trancheFiveMigrations()
	if last := steps[len(steps)-1]; last[0] != "ddlVerifierBacktest" ||
		last[1] != ddlVerifierBacktest {
		t.Fatalf("last T5 step = %s, want ddlVerifierBacktest", last[0])
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
