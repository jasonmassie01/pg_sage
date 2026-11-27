package retention

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// evid-02a: stored workload evidence packs age out on built_at after
// retention.workload_packs_days (180); 0 keeps them. A pack is never
// updated, only deleted. plan-01a adds the keep predicate for packs a plan
// cites (plan_retention_db_test.go tests it on a server). No DB or
// concurrency test here: the rule is data and the purge machinery is
// covered by the generic purge tests.

func TestPurgeRules_WorkloadPacks(t *testing.T) {
	for days, want := range map[int]int{180: 180, 0: 0, 7: 7} {
		cfg := config.DefaultConfig()
		cfg.Retention.WorkloadPacksDays = days
		var found []purgeRule
		for _, r := range purgeRules(cfg) {
			if r.table == "workload_packs" {
				found = append(found, r)
			}
		}
		if len(found) != 1 || found[0].timeCol != "built_at" || found[0].days != want ||
			found[0].extra != keepCitedPacks(cfg.Retention.ActionsDays) {
			t.Fatalf("days %d: workload_packs rules %+v, want one on built_at for %d days",
				days, found, want)
		}
	}
}
