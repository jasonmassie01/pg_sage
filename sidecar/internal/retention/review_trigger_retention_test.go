package retention

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// review-04a: done review triggers age out after 400 days on fired_at,
// like their runs; a fired or claimed trigger is never purged. No
// concurrent access tests: purgeRules is a pure function of the config.
func TestPurgeRules_DoneReviewTriggersKept400Days(t *testing.T) {
	for _, days := range []int{0, 7, 30} {
		cfg := config.DefaultConfig()
		cfg.Retention.ActionsDays, cfg.Retention.FindingsDays = days, days
		var found []purgeRule
		for _, r := range purgeRules(cfg) {
			if r.table == "review_trigger" {
				found = append(found, r)
			}
		}
		if len(found) != 1 || found[0].timeCol != "fired_at" || found[0].days != 400 ||
			found[0].extra != "AND state IN ('consumed', 'coalesced')" {
			t.Fatalf("actions/findings %d days: review_trigger rules %+v, want one on "+
				"fired_at for 400 days of done triggers only", days, found)
		}
	}
}
