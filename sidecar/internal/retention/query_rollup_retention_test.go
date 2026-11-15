package retention

import (
	"testing"

	"github.com/pg-sage/sidecar/internal/config"
)

// evid-01a: the hourly per-query rollup is built from the snapshots and
// ages out with retention.snapshots_days. No concurrency tests: the rules
// are values in a list, and the purge itself is covered by the generic
// purge tests.

func TestPurgeRules_QueryRollupFollowsTheSnapshots(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Retention.SnapshotsDays = 45
	cfg.Retention.QueryStoreDays = 14
	found := map[string]purgeRule{}
	for _, r := range purgeRules(cfg) {
		if r.table == "query_rollup_hourly" || r.table == "query_rollup_coverage" {
			if _, dup := found[r.table]; dup {
				t.Fatalf("two purge rules for sage.%s", r.table)
			}
			found[r.table] = r
		}
	}
	for _, table := range []string{"query_rollup_hourly", "query_rollup_coverage"} {
		r, ok := found[table]
		if !ok {
			t.Fatalf("no purge rule for sage.%s", table)
		}
		if r.timeCol != "hour" || r.days != 45 || r.extra != "" || r.partitioned != nil {
			t.Fatalf("%s rule = %+v, want hour, snapshots_days (45), unpartitioned", table, r)
		}
		if _, exempt := retentionExemptions[table]; exempt {
			t.Fatalf("sage.%s is both purged and exempt", table)
		}
	}
	cfg.Retention.SnapshotsDays = 0
	for _, r := range purgeRules(cfg) {
		if r.table == "query_rollup_hourly" && r.days != 0 {
			t.Fatalf("snapshots_days 0 (off) must leave the rollup unpurged, got %d", r.days)
		}
	}
}

func TestRun_PurgesExpiredQueryRollupRowsOnly(t *testing.T) {
	_, ctx := requireDB(t)
	execRetry(t, ctx, `DELETE FROM sage.query_rollup_hourly WHERE queryid = 424242`)
	insert := `INSERT INTO sage.query_rollup_hourly (queryid, hour, calls, exec_ms, rows,
		shared_blks_hit, shared_blks_read, temp_blks_read, temp_blks_written, epoch_break,
		pairs) VALUES (424242, date_trunc('hour', now() - $1::interval), 1, 1, 1, 1, 1, 0, 0,
		false, 1)`
	for _, age := range []string{"40 days", "31 days", "29 days", "2 hours"} {
		execRetry(t, ctx, insert, age)
	}
	New(testPool, allDays(30), noopLog).Run(ctx)
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.query_rollup_hourly
		WHERE queryid = $1`, 424242); n != 2 {
		t.Fatalf("%d rollup rows remain, want 2 (29 days and 2 hours)", n)
	}
}
