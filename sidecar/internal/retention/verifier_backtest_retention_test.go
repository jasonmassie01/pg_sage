package retention

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// lab-02: verifier back-test rows age out after 365 days on ran_at,
// whatever the other retention settings say.
func TestPurgeRules_VerifierBacktestKeptAYear(t *testing.T) {
	for _, days := range []int{0, 7, 400} {
		cfg := config.DefaultConfig()
		cfg.Retention.ActionsDays, cfg.Retention.FindingsDays = days, days
		var found []purgeRule
		for _, r := range purgeRules(cfg) {
			if r.table == "verifier_backtest" {
				found = append(found, r)
			}
		}
		if len(found) != 1 || found[0].timeCol != "ran_at" || found[0].days != 365 ||
			found[0].extra != "" {
			t.Fatalf("actions/findings %d days: verifier_backtest rules %+v, want one on "+
				"ran_at for 365 days", days, found)
		}
	}
}
