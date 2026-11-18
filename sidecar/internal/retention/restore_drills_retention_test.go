package retention

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// drill-01a: restore drill rows age out after 400 days on finished_at,
// whatever the other retention settings say. No concurrent access tests:
// purgeRules is a pure function of the config.
func TestPurgeRules_RestoreDrillsKept400Days(t *testing.T) {
	for _, days := range []int{0, 7, 30} {
		cfg := config.DefaultConfig()
		cfg.Retention.ActionsDays, cfg.Retention.FindingsDays = days, days
		var found []purgeRule
		for _, r := range purgeRules(cfg) {
			if r.table == "restore_drills" {
				found = append(found, r)
			}
		}
		if len(found) != 1 || found[0].timeCol != "finished_at" || found[0].days != 400 ||
			found[0].extra != "" {
			t.Fatalf("actions/findings %d days: restore_drills rules %+v, want one on "+
				"finished_at for 400 days", days, found)
		}
	}
}
