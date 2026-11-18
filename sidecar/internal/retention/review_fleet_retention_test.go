package retention

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// review-04b: finished fleet passes age out after 400 days on started_at,
// like the review runs; a running pass is never purged, and its member
// rows go with it (ON DELETE CASCADE). No concurrent access tests:
// purgeRules is a pure function of the config.
func TestPurgeRules_FinishedFleetPassesKept400Days(t *testing.T) {
	for _, days := range []int{0, 7, 30} {
		cfg := config.DefaultConfig()
		cfg.Retention.ActionsDays, cfg.Retention.FindingsDays = days, days
		var found []purgeRule
		for _, r := range purgeRules(cfg) {
			if r.table == "review_fleet_pass" || r.table == "review_fleet_member" {
				found = append(found, r)
			}
		}
		if len(found) != 1 || found[0].table != "review_fleet_pass" ||
			found[0].timeCol != "started_at" || found[0].days != 400 ||
			found[0].extra != "AND status <> 'running'" {
			t.Fatalf("actions/findings %d days: fleet rules %+v, want one on the pass's "+
				"started_at for 400 days of finished passes", days, found)
		}
	}
	why, ok := retentionExemptions["review_fleet_member"]
	if !ok || !strings.Contains(why, "CASCADE") {
		t.Fatalf("review_fleet_member exemption %q (%v), want the cascade named", why, ok)
	}
}
