package retention

import (
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// market-02: sage.baseline_samples keeps baseline.weeks + 1 weeks of
// hour-of-week buckets, so scoring never depends on the 14-day
// query_store retention. No concurrency tests: the rule is a value in a list.

func baselineRule(t *testing.T, cfg *config.Config) purgeRule {
	t.Helper()
	for _, r := range purgeRules(cfg) {
		if r.table == "baseline_samples" {
			return r
		}
	}
	t.Fatal("no purge rule for sage.baseline_samples")
	return purgeRule{}
}

func TestPurgeRules_BaselineSamplesKeepWeeksPlusOne(t *testing.T) {
	cfg := config.DefaultConfig()
	r := baselineRule(t, cfg)
	if r.timeCol != "week_start" || r.days != 35 || r.extra != "" {
		t.Fatalf("rule = %+v, want week_start, 35 days (4 + 1 weeks), no keep predicate", r)
	}
	cfg.Baseline.Weeks = 8
	if r := baselineRule(t, cfg); r.days != 63 {
		t.Fatalf("days = %d, want 63 (8 + 1 weeks)", r.days)
	}
	// Zero weeks (a literal config) keeps the default window, never 7 days.
	cfg.Baseline.Weeks = 0
	if r := baselineRule(t, cfg); r.days != 35 {
		t.Fatalf("zero weeks = %d days, want the default 35", r.days)
	}
	q := purgeSQL(baselineRule(t, config.DefaultConfig()), "sage.baseline_samples", batchSize)
	if !strings.Contains(q, "LIMIT 1000") || !strings.Contains(q, "week_start < now()") {
		t.Fatalf("purge SQL is not a bounded purge on week_start:\n%s", q)
	}
}
