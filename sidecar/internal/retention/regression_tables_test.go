package retention

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/partition"
)

// G7-B12 / G4-B26: previously unbounded sage time-series are purged in
// bounded batches; recent rows survive.
func TestRun_PurgesPreviouslyUnboundedTables(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("g7b12")
	seeds := []struct {
		table, insert, count string
	}{
		{"notification_log",
			`INSERT INTO sage.notification_log (event, subject, sent_at)
			 VALUES ($1, 's', now() - $2::interval)`,
			`SELECT count(*) FROM sage.notification_log WHERE event=$1`},
		{"alert_log",
			`INSERT INTO sage.alert_log (severity, channel, dedup_key, sent_at)
			 VALUES ('info', 'slack', $1, now() - $2::interval)`,
			`SELECT count(*) FROM sage.alert_log WHERE dedup_key=$1`},
		{"decision",
			`INSERT INTO sage.decision (feature, intent, verdict, risk_tier, reason,
			 evidence_id, created_at) VALUES ('t', 'i', 'blocked', 'safe', 'r',
			 $1::text || md5(random()::text), now() - $2::interval)`,
			`SELECT count(*) FROM sage.decision WHERE evidence_id LIKE $1::text || '%'`},
		{"retention_run",
			`INSERT INTO sage.retention_run (schema_name, table_name, retention_column,
			 cutoff_at, disposition, created_at)
			 VALUES ('s', $1, 'c', now(), 'dry_run', now() - $2::interval)`,
			`SELECT count(*) FROM sage.retention_run WHERE table_name=$1`},
		{"health_history",
			`INSERT INTO sage.health_history (database_name, health_score, recorded_at)
			 VALUES ($1, 90, now() - $2::interval)`,
			`SELECT count(*) FROM sage.health_history WHERE database_name=$1`},
		{"size_history",
			`INSERT INTO sage.size_history (metric_type, object_name, size_bytes,
			 collected_at) VALUES ('table', $1, 1, now() - $2::interval)`,
			`SELECT count(*) FROM sage.size_history WHERE object_name=$1`},
		{"incidents",
			`INSERT INTO sage.incidents (severity, root_cause, source, detected_at,
			 last_detected_at, resolved_at) VALUES ('info', $1, 'deterministic',
			 now() - $2::interval, now() - $2::interval, now() - $2::interval)`,
			`SELECT count(*) FROM sage.incidents WHERE root_cause=$1`},
	}
	for _, s := range seeds {
		execRetry(t, ctx, s.insert, tag, "400 days")
		execRetry(t, ctx, s.insert, tag, "1 hour")
	}

	New(testPool, allDays(30), noopLog).Run(ctx)

	for _, s := range seeds {
		if n := countWhere(t, ctx, s.count, tag); n != 1 {
			t.Errorf("%s: %d rows remain, want 1 (recent kept, expired purged)", s.table, n)
		}
	}
}

// G7-B12 guard: every sage table with a time column must either have a
// purge rule or an explicit, justified exemption. New tables fail here
// until someone decides their retention. The agent_db_* tables are created
// on first use, so the guard creates them first; partitions of a
// partitioned table are covered by its rule.
func TestRetentionRules_CoverEveryTimeSeriesTable(t *testing.T) {
	ensureAgentSchema(t)
	_, ctx := requireDB(t)
	rows, err := testPool.Query(ctx, `SELECT DISTINCT c.table_name
		FROM information_schema.columns c
		JOIN information_schema.tables t
		  ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		JOIN pg_class pc ON pc.oid = to_regclass(quote_ident(c.table_schema) || '.' ||
		                                         quote_ident(c.table_name))
		WHERE c.table_schema = 'sage' AND t.table_type = 'BASE TABLE'
		  AND NOT pc.relispartition
		  AND c.data_type LIKE 'timestamp%'`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	covered := map[string]bool{}
	for _, r := range purgeRules(allDays(30)) {
		covered[r.table] = true
	}
	var missing []string
	agentTables := 0
	for rows.Next() {
		var table string
		if err := rows.Scan(&table); err != nil {
			t.Fatal(err)
		}
		if strings.HasPrefix(table, "agent_db_") {
			agentTables++
		}
		if !covered[table] && retentionExemptions[table] == "" {
			missing = append(missing, table)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("sage tables with no purge rule or exemption: %v", missing)
	}
	if agentTables < 18 {
		t.Fatalf("the guard saw %d agent_db_* tables, want all 18+", agentTables)
	}
}

// Every partitioned history table has a rule; its partitions are not
// listed separately.
func TestRetentionRules_CoverPartitionedTables(t *testing.T) {
	covered := map[string]bool{}
	for _, r := range purgeRules(allDays(30)) {
		covered[r.table] = true
	}
	for _, tbl := range partition.HistoryTables() {
		if !covered[tbl.Name] {
			t.Errorf("partitioned sage.%s has no retention rule", tbl.Name)
		}
	}
}

// RunEvery (the per-instance entry point for fleet wiring) purges
// immediately and returns when its context ends, even with a bad interval.
func TestRunEvery_RunsImmediatelyAndStops(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("run_every")
	execRetry(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - interval '400 days', $1, '{}'::jsonb)`, tag)
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		New(testPool, allDays(30), noopLog).RunEvery(runCtx, 0)
		close(done)
	}()
	deadline := time.Now().Add(10 * time.Second)
	for countWhere(t, ctx, `SELECT count(*) FROM sage.snapshots WHERE category=$1`, tag) != 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("RunEvery did not purge on its first pass")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("RunEvery did not return after cancellation")
	}
}
