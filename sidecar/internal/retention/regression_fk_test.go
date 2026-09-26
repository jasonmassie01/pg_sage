package retention

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
)

func allDays(n int) *config.Config {
	return &config.Config{Retention: config.RetentionConfig{
		SnapshotsDays: n, FindingsDays: n, ActionsDays: n, ExplainsDays: n,
	}}
}

func insertID(t *testing.T, ctx context.Context, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := testPool.QueryRow(ctx, sql, args...).Scan(&id); err != nil {
		t.Fatalf("insert: %v\nsql: %s", err, sql)
	}
	return id
}

func countWhere(t *testing.T, ctx context.Context, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := testPool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count: %v\nsql: %s", err, sql)
	}
	return n
}

func uniqueTag(prefix string) string {
	return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
}

func insertDecision(t *testing.T, ctx context.Context, evidence string, age string) int64 {
	t.Helper()
	return insertID(t, ctx, `INSERT INTO sage.decision
		(feature, intent, verdict, risk_tier, reason, evidence_id, created_at)
		VALUES ('retention_test', 'test', 'observe_only', 'safe', 'test', $1,
		        now() - $2::interval) RETURNING id`, evidence, age)
}

// G7-B11 / G1-B12: an alert_log row referencing a resolved finding must not
// block the finding purge; the alert keeps its audit row with a NULL link.
func TestRun_PurgesFindingReferencedByAlertLog(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("b12_alert")
	findingID := insertID(t, ctx, `INSERT INTO sage.findings
		(category, severity, title, detail, status, last_seen)
		VALUES ('retention_test', 'warning', $1, '{}'::jsonb, 'resolved',
		        now() - interval '400 days') RETURNING id`, tag)
	alertID := insertID(t, ctx, `INSERT INTO sage.alert_log
		(finding_id, severity, channel, dedup_key)
		VALUES ($1, 'warning', 'slack', $2) RETURNING id`, findingID, tag)

	New(testPool, allDays(30), noopLog).Run(ctx)

	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.findings WHERE id=$1`,
		findingID); n != 0 {
		t.Fatalf("resolved finding referenced by alert_log was not purged")
	}
	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.alert_log
		WHERE id=$1 AND finding_id IS NULL`, alertID); n != 1 {
		t.Fatalf("recent alert_log row lost or still pointing at purged finding")
	}
}

// G1-B12: action_log purges must not fail on the non-cascading references
// from findings, action_queue, decision, verification and schema_baseline.
func TestRun_PurgesActionLogWithDependents(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("b12_action")
	actionID := insertID(t, ctx, `INSERT INTO sage.action_log
		(executed_at, action_type, sql_executed, outcome)
		VALUES (now() - interval '400 days', $1, 'SELECT 1', 'success')
		RETURNING id`, tag)
	findingID := insertID(t, ctx, `INSERT INTO sage.findings
		(category, severity, title, detail, action_log_id)
		VALUES ('retention_test', 'info', $1, '{}'::jsonb, $2) RETURNING id`,
		tag, actionID)
	queueID := insertID(t, ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, action_log_id) VALUES ('SELECT 1', 'safe', $1)
		RETURNING id`, actionID)
	decisionID := insertDecision(t, ctx, tag, "1 day")
	execRetry(t, ctx, `UPDATE sage.decision SET action_log_id=$1 WHERE id=$2`,
		actionID, decisionID)

	New(testPool, allDays(30), noopLog).Run(ctx)

	if n := countWhere(t, ctx, `SELECT count(*) FROM sage.action_log WHERE id=$1`,
		actionID); n != 0 {
		t.Fatal("expired action_log row with dependents was not purged")
	}
	checks := map[string]string{
		"findings":     `SELECT count(*) FROM sage.findings WHERE id=$1 AND action_log_id IS NULL`,
		"action_queue": `SELECT count(*) FROM sage.action_queue WHERE id=$1 AND action_log_id IS NULL`,
		"decision":     `SELECT count(*) FROM sage.decision WHERE id=$1 AND action_log_id IS NULL`,
	}
	ids := map[string]int64{"findings": findingID, "action_queue": queueID, "decision": decisionID}
	for table, sql := range checks {
		if countWhere(t, ctx, sql, ids[table]) != 1 {
			t.Errorf("%s row lost or still references the purged action", table)
		}
	}
}

// G1-B12: value evidence (incident_avoided has NOT NULL references) keeps
// its action/decision/verification rows; the rest of the purge proceeds.
func TestRun_KeepsActionReferencedByValueLedger(t *testing.T) {
	_, ctx := requireDB(t)
	tag := uniqueTag("b12_value")
	actionID := insertID(t, ctx, `INSERT INTO sage.action_log
		(executed_at, action_type, sql_executed, outcome)
		VALUES (now() - interval '400 days', $1, 'SELECT 1', 'success')
		RETURNING id`, tag)
	decisionID := insertDecision(t, ctx, tag, "400 days")
	verificationID := insertID(t, ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict, created_at, completed_at)
		VALUES ($1, $2, '{}'::jsonb, '{}'::jsonb, 1, now(), now(), 'success',
		        now() - interval '400 days', now() - interval '400 days')
		RETURNING id`, decisionID, actionID)
	execRetry(t, ctx, `INSERT INTO sage.incident_avoided
		(kind, severity, credited_minutes, evidence_id, decision_id,
		 action_log_id, verification_id, occurred_at)
		VALUES ('lock_storm', 'prevented', 5, $1, $2, $3, $4,
		        now() - interval '400 days')`, tag, decisionID, actionID, verificationID)
	snapTag := uniqueTag("b12_value_snap")
	execRetry(t, ctx, `INSERT INTO sage.snapshots (collected_at, category, data)
		VALUES (now() - interval '400 days', $1, '{}'::jsonb)`, snapTag)

	New(testPool, allDays(30), noopLog).Run(ctx)

	for table, id := range map[string]int64{
		"action_log": actionID, "decision": decisionID, "verification": verificationID,
	} {
		if countWhere(t, ctx, `SELECT count(*) FROM sage.`+table+` WHERE id=$1`, id) != 1 {
			t.Errorf("%s row referenced by incident_avoided was deleted", table)
		}
	}
	if countWhere(t, ctx, `SELECT count(*) FROM sage.snapshots WHERE category=$1`,
		snapTag) != 0 {
		t.Error("an unrelated purge was aborted")
	}
}

// G1-B32: purge failures are logged at ERROR, not at INFO under a
// component name passed as the level.
func TestPurgeTable_FailureLoggedAtErrorLevel(t *testing.T) {
	_, ctx := requireDB(t)
	var mu sync.Mutex
	var levels []string
	logFn := func(level, msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		levels = append(levels, level+" "+fmt.Sprintf(msg, args...))
	}
	c := New(testPool, allDays(30), logFn)
	c.purgeTable(ctx, "b32_does_not_exist", "created_at", 30, "")
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(levels, "|")
	if !strings.Contains(joined, "ERROR ") || !strings.Contains(joined, "b32_does_not_exist") {
		t.Fatalf("purge failure logs = %q; want an ERROR-level entry", levels)
	}
}
