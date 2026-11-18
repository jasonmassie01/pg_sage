package retention

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// guard-01b: a registered backfill job ages out 90 days after it expired
// (on expires_at), so an active job is never purged and the weekly count
// of registered jobs has a quarter of history. No concurrent access
// tests: purgeRules is a pure function of the config.
func TestPurgeRules_PressureJobsKept90DaysPastExpiry(t *testing.T) {
	for _, days := range []int{0, 7, 30} {
		cfg := config.DefaultConfig()
		cfg.Retention.ActionsDays, cfg.Retention.FindingsDays = days, days
		var found []purgeRule
		for _, r := range purgeRules(cfg) {
			if r.table == "pressure_jobs" {
				found = append(found, r)
			}
		}
		if len(found) != 1 || found[0].timeCol != "expires_at" || found[0].days != 90 ||
			found[0].extra != "" {
			t.Fatalf("actions/findings %d days: pressure_jobs rules %+v, want one on "+
				"expires_at for 90 days", days, found)
		}
	}
}
