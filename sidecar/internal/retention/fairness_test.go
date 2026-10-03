package retention

import (
	"slices"
	"testing"
	"time"
)

// A rule that spends the run's budget every run (lifeos: sage.query_store's
// backlog, "29 rules deferred to the next run, starting with
// sage.query_store") must not starve the others: it goes to the back of the
// next run, so every rule runs within len(rules) runs.
func TestRunOnce_ABudgetHogDoesNotStarveTheOtherRules(t *testing.T) {
	pool, ctx := requireDB(t)
	hogTag, victimTag := uniqueTag("hog"), uniqueTag("victim")
	execRetry(t, ctx, `INSERT INTO sage.notification_log (event, subject, sent_at)
		SELECT $1, 's', now() - interval '400 days' FROM generate_series(1, 50000)`, hogTag)
	t.Cleanup(func() {
		execRetry(t, ctx, `DELETE FROM sage.notification_log WHERE event = $1`, hogTag)
	})
	execRetry(t, ctx, `INSERT INTO sage.alert_log (severity, channel, dedup_key, sent_at)
		VALUES ('warning', 'test', $1, now() - interval '400 days')`, victimTag)
	cfg := allDays(30)
	rules := purgeRules(cfg)
	hog := slices.IndexFunc(rules, func(r purgeRule) bool { return r.table == "notification_log" })
	victim := slices.IndexFunc(rules, func(r purgeRule) bool { return r.table == "alert_log" })
	if hog < 0 || victim < 0 || victim > hog {
		t.Fatalf("rules: hog %d, victim %d; the victim must come before the hog", hog, victim)
	}
	// A 1 ns budget: the hog deletes one full batch and is cut short every
	// time it runs; every other rule finishes its one statement.
	c := New(pool, cfg, noopLog).WithPacing(time.Millisecond, time.Nanosecond)
	c.next = hog
	hogRuns := 0
	for run := 1; run <= len(rules); run++ {
		stats := c.RunOnce(ctx)
		if stats.Batches["notification_log"] > 0 {
			hogRuns++
		}
		if countWhere(t, ctx, `SELECT count(*) FROM sage.alert_log WHERE dedup_key = $1`,
			victimTag) == 0 {
			if hogRuns == 0 {
				t.Fatal("the hog never ran")
			}
			return
		}
	}
	t.Fatalf("alert_log's expired row is still there after %d runs: starved by the hog",
		len(rules))
}
