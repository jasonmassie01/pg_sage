package retention

import "testing"

// Remembered what-if rejections age out on the findings window from their
// last measurement: a rejection older than the optimizer's max age is never
// read again, so purging it only means the idea may be measured afresh.
func TestPurgeRulesAgeOptimizerRejectionsFromLastMeasurement(t *testing.T) {
	for _, rule := range purgeRules(allDays(30)) {
		if rule.table != "optimizer_rejection" {
			continue
		}
		if rule.timeCol != "measured_at" || rule.days != 30 || rule.extra != "" ||
			rule.optional || rule.partitioned != nil {
			t.Fatalf("optimizer_rejection rule = %+v, want measured_at on the findings window",
				rule)
		}
		return
	}
	t.Fatal("sage.optimizer_rejection has no purge rule")
}

func TestRun_PurgesExpiredOptimizerRejections(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("rejpurge")
	insert := `INSERT INTO sage.optimizer_rejection (schema_name, table_name, shape_hash,
		method, key_cols, ddl, improvement_pct, min_improvement_pct, measured_at)
		VALUES ($1, 't', md5(random()::text) || md5(random()::text), 'btree', '{x}',
		'CREATE INDEX i ON t (x)', 0, 10, now() - $2::interval)`
	execRetry(t, ctx, insert, tag, "400 days")
	execRetry(t, ctx, insert, tag, "1 hour")
	New(testPool, allDays(30), noopLog).Run(ctx)
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.optimizer_rejection
		WHERE schema_name = $1`, tag); n != 1 {
		t.Fatalf("%d rows remain, want 1 (recent kept, expired purged)", n)
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.optimizer_rejection
		WHERE schema_name = $1 AND measured_at > now() - interval '1 day'`, tag); n != 1 {
		t.Fatal("the recent rejection was purged")
	}
}
