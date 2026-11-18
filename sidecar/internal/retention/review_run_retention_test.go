package retention

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// review-01a: grade runs age out after 400 days on started_at (their
// dimension rows go with them by the foreign key's ON DELETE CASCADE); a
// running row is never purged. No concurrent access tests: purgeRules is
// a pure function of the config.
func TestPurgeRules_ReviewRunsKept400Days(t *testing.T) {
	for _, days := range []int{0, 7, 30} {
		cfg := config.DefaultConfig()
		cfg.Retention.ActionsDays, cfg.Retention.FindingsDays = days, days
		var found []purgeRule
		for _, r := range purgeRules(cfg) {
			if r.table == "review_run" {
				found = append(found, r)
			}
		}
		if len(found) != 1 || found[0].timeCol != "started_at" || found[0].days != 400 ||
			found[0].extra != "AND status <> 'running'" {
			t.Fatalf("actions/findings %d days: review_run rules %+v, want one on "+
				"started_at for 400 days that keeps running rows", days, found)
		}
	}
}
