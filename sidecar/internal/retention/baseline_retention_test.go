package retention

import (
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/baseline"
	"github.com/pg-sage/sidecar/internal/config"
)

// market-02: sage.baseline_samples keeps baseline.weeks + 1 weeks of
// hour-of-week buckets, so scoring never depends on the 14-day
// query_store retention. Two more days cover a week_start (a local
// Monday date) that lags UTC by up to 12 hours in a western zone, read as
// midnight of a session TimeZone up to 14 hours ahead of UTC.
// No concurrency tests: the rule is a value in a list.

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
	if r.timeCol != "week_start" || r.days != 37 || r.extra != "" {
		t.Fatalf("rule = %+v, want week_start, 37 days (4 + 1 weeks and 2 days), "+
			"no keep predicate", r)
	}
	cfg.Baseline.Weeks = 8
	if r := baselineRule(t, cfg); r.days != 65 {
		t.Fatalf("days = %d, want 65 (8 + 1 weeks and 2 days)", r.days)
	}
	// Zero weeks (a literal config) keeps the default window, never 7 days.
	cfg.Baseline.Weeks = 0
	if r := baselineRule(t, cfg); r.days != 37 {
		t.Fatalf("zero weeks = %d days, want the default 37", r.days)
	}
	q := purgeSQL(baselineRule(t, config.DefaultConfig()), "sage.baseline_samples", batchSize)
	if !strings.Contains(q, "LIMIT 1000") || !strings.Contains(q, "week_start < now()") {
		t.Fatalf("purge SQL is not a bounded purge on week_start:\n%s", q)
	}
}

// The oldest week the scoring reads (baseline.Store.History) survives the
// purge at every hour of the week, in the zones furthest from UTC and with
// the date read at midnight of a session zone 14 hours ahead of UTC.
func TestPurgeRules_BaselineWindowSurvivesThePurge(t *testing.T) {
	cfg := config.DefaultConfig()
	keep := time.Duration(baselineRule(t, cfg).days) * 24 * time.Hour
	for _, zone := range []string{"UTC", "America/Chicago", "Etc/GMT+12", "Etc/GMT-14"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatalf("load %s: %v", zone, err)
		}
		w := baseline.Window{Loc: loc, Weeks: cfg.Baseline.Weeks}
		start := time.Date(2026, 11, 2, 0, 0, 0, 0, time.UTC)
		for h := 0; h < 2*baseline.BucketsPerWeek; h++ {
			now := start.Add(time.Duration(h) * time.Hour)
			oldest := w.WeekStart(now).AddDate(0, 0, -7*w.Weeks).Add(-14 * time.Hour)
			if oldest.Before(now.Add(-keep)) {
				t.Fatalf("%s at %s: the oldest scored week %s is purged (kept since %s)",
					zone, now, w.WeekStart(now).AddDate(0, 0, -7*w.Weeks), now.Add(-keep))
			}
		}
	}
}
