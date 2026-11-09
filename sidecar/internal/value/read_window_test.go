package value

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The window is [Since, Until) in all three reads (earned-09): a one-day
// report counts the first and the last second of that day and nothing at
// the next midnight or before since.
func TestSnapshotWindowCountsTheWholeUntilDay(t *testing.T) {
	ctx := context.Background()
	src := newLedgerSource(t, "until_day")
	day := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	next := day.AddDate(0, 0, 1)
	last := next.Add(-time.Second)
	for at, minutes := range map[time.Time]float64{
		day: 10, last: 20, next: 40, day.Add(-time.Second): 80,
	} {
		insertWindowCredit(t, ctx, src.Pool, at, minutes)
		insertWindowPending(t, ctx, src.Pool, at)
	}
	graph := insertValueEvidenceGraph(t, ctx, src.Pool, 7, "until-day")
	for _, at := range []time.Time{last, next} {
		recordWindowIncident(t, ctx, src.Pool, graph, at)
	}

	got, err := readSnapshot(ctx, src.Pool, "until_day", Filter{Since: day, Until: next})
	if err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	if got.AllTimeMinutes != 30 || len(got.TrendMinutes) != 1 ||
		got.TrendMinutes[0].Day != "2026-08-01" || got.TrendMinutes[0].Minutes != 30 {
		t.Fatalf("realized = %v trend %+v, want 30 minutes on 2026-08-01",
			got.AllTimeMinutes, got.TrendMinutes)
	}
	if want := 2 * queuedBaseMinutes(t, ctx, src.Pool); got.PotentialMinutes != want {
		t.Fatalf("potential = %v, want %v (the two rows inside the day)",
			got.PotentialMinutes, want)
	}
	if got.IncidentMinutes != NearMissMinutes || len(got.Incidents) != 1 ||
		!got.Incidents[0].OccurredAt.Equal(last) {
		t.Fatalf("incidents = %v %+v, want the 23:59:59 one only",
			got.IncidentMinutes, got.Incidents)
	}
}

func insertWindowCredit(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, at time.Time, minutes float64,
) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_log (executed_at, action_type,
		sql_executed, outcome, toil_minutes_saved, toil_model_version)
		VALUES ($1, 'create_index_concurrently', 'SELECT 1', 'success', $2, 1)`,
		at, minutes); err != nil {
		t.Fatalf("insert credited action at %s: %v", at, err)
	}
}

func insertWindowPending(t *testing.T, ctx context.Context, pool *pgxpool.Pool, at time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO sage.action_queue
		(proposed_sql, action_risk, status, action_type, proposed_at)
		VALUES ('SELECT 1', 'safe', 'pending', 'create_index_concurrently', $1)`,
		at); err != nil {
		t.Fatalf("insert pending action at %s: %v", at, err)
	}
}

func recordWindowIncident(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	graph valueEvidenceGraph, at time.Time,
) {
	t.Helper()
	_, err := NewPostgresRepository(pool).RecordIncident(ctx, IncidentCredit{
		Kind: IncidentXIDWraparound, Severity: SeverityNearMiss,
		CreditedMinutes: NearMissMinutes, EvidenceID: graph.evidenceID + at.String(),
		ModelVersion: 1, DecisionID: graph.decisionID, ActionLogID: graph.actionID,
		VerificationID: graph.verificationID, OccurredAt: at,
	})
	if err != nil {
		t.Fatalf("record incident at %s: %v", at, err)
	}
}

func queuedBaseMinutes(t *testing.T, ctx context.Context, pool *pgxpool.Pool) float64 {
	t.Helper()
	var minutes float64
	if err := pool.QueryRow(ctx, `SELECT base_minutes::float8 FROM sage.toil_model
		WHERE action_type='create_index_concurrently' AND effective_to IS NULL`).
		Scan(&minutes); err != nil || minutes <= 0 {
		t.Fatalf("read the queued action's toil model: %v %v", minutes, err)
	}
	return minutes
}
