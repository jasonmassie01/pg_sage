package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// TestWriteValueMetricsEmitsVerifiedToilAndIncidentSeries proves the
// value gauges follow the /api/v1/value honesty rules: verified
// successes are summed per database/feature, reverted actions
// contribute nothing, and incidents are counted by kind.
func TestWriteValueMetricsEmitsVerifiedToilAndIncidentSeries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	testPool, err := pgxpool.New(ctx, testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("connect test database: %v", err)
	}
	t.Cleanup(testPool.Close)
	if err := schema.Bootstrap(ctx, testPool); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}

	previousPool := pool
	pool = testPool
	t.Cleanup(func() { pool = previousPool })

	seedValueMetricsFixture(t, ctx, testPool)

	var b strings.Builder
	writeValueMetrics(&b, ctx)
	output := b.String()

	wantSeries := `pg_sage_toil_minutes_saved_total` +
		`{database="",feature="create_index_concurrently"} 45`
	if !strings.Contains(output, wantSeries) {
		t.Errorf("missing verified toil series %q in:\n%s",
			wantSeries, output)
	}
	if strings.Contains(output, "value_metrics_reverted") {
		t.Errorf("reverted action must not be credited:\n%s", output)
	}
	wantIncident := `pg_sage_incidents_avoided_total` +
		`{kind="xid_wraparound"} 1`
	if !strings.Contains(output, wantIncident) {
		t.Errorf("missing incident series %q in:\n%s",
			wantIncident, output)
	}
	for _, header := range []string{
		"# TYPE pg_sage_toil_minutes_saved_total counter",
		"# TYPE pg_sage_incidents_avoided_total counter",
	} {
		if !strings.Contains(output, header) {
			t.Errorf("missing header %q in:\n%s", header, output)
		}
	}
}

// seedValueMetricsFixture inserts one verified-credited action, one
// reverted (zero-credit) action, and one avoided incident with its
// required decision/verification chain.
func seedValueMetricsFixture(
	t *testing.T, ctx context.Context, testPool *pgxpool.Pool,
) {
	t.Helper()
	var creditedID, revertedID int64
	err := testPool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, toil_minutes_saved)
		VALUES ('create_index_concurrently', 'SELECT 1', 'success', 45)
		RETURNING id`).Scan(&creditedID)
	if err != nil {
		t.Fatalf("insert credited action: %v", err)
	}
	err = testPool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, toil_minutes_saved)
		VALUES ('value_metrics_reverted', 'SELECT 1', 'reverted', 0)
		RETURNING id`).Scan(&revertedID)
	if err != nil {
		t.Fatalf("insert reverted action: %v", err)
	}

	var decisionID, verificationID int64
	err = testPool.QueryRow(ctx, `INSERT INTO sage.decision
		(feature, intent, verdict, risk_tier, reason, evidence_id,
		 action_log_id)
		VALUES ('freeze', 'test incident credit', 'execute', 'safe',
		 'test', 'ev-value-metrics-test', $1) RETURNING id`,
		creditedID).Scan(&decisionID)
	if err != nil {
		t.Fatalf("insert decision: %v", err)
	}
	err = testPool.QueryRow(ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline,
		 minimum_samples, next_evaluation_at, hard_deadline_at,
		 verdict, completed_at)
		VALUES ($1, $2, '{}', '{}', 1, now(), now(), 'success', now())
		RETURNING id`, decisionID, creditedID).Scan(&verificationID)
	if err != nil {
		t.Fatalf("insert verification: %v", err)
	}
	_, err = testPool.Exec(ctx, `INSERT INTO sage.incident_avoided
		(kind, severity, credited_minutes, evidence_id, decision_id,
		 action_log_id, verification_id)
		VALUES ('xid_wraparound', 'near_miss', 120,
		 'ev-value-metrics-test', $1, $2, $3)`,
		decisionID, creditedID, verificationID)
	if err != nil {
		t.Fatalf("insert incident: %v", err)
	}

	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(
			context.Background(), 15*time.Second)
		defer cancel()
		_, _ = testPool.Exec(cleanupCtx,
			`DELETE FROM sage.incident_avoided
			 WHERE evidence_id='ev-value-metrics-test'`)
		_, _ = testPool.Exec(cleanupCtx,
			`DELETE FROM sage.verification WHERE decision_id=$1`,
			decisionID)
		_, _ = testPool.Exec(cleanupCtx,
			`DELETE FROM sage.decision WHERE id=$1`, decisionID)
		_, _ = testPool.Exec(cleanupCtx,
			`DELETE FROM sage.action_log WHERE id IN ($1, $2)`,
			creditedID, revertedID)
	})
}
