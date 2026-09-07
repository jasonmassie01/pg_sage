package value

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

func TestPostgresRepositoryReadSnapshotFiltersHonestValue(t *testing.T) {
	pool, ctx := requireValuePostgres(t)
	repo := NewPostgresRepository(pool)
	databaseID, databaseName := insertValueDatabase(t, ctx, pool)
	otherID, _ := insertValueDatabase(t, ctx, pool)
	graph := insertValueEvidenceGraph(t, ctx, pool, databaseID, "snapshot")
	insertCreditedAction(t, ctx, pool, databaseID, "create_index_concurrently", 45)
	insertRevertedAction(t, ctx, pool, databaseID, "create_index_concurrently", 45)
	insertPendingAction(t, ctx, pool, databaseID, "create_index_concurrently")
	insertCreditedAction(t, ctx, pool, otherID, "analyze_table", 99)
	incident, err := repo.RecordIncident(ctx, IncidentCredit{
		DatabaseID: &databaseID, Kind: "lock_storm", Severity: "prevented",
		CreditedMinutes: 60, EvidenceID: graph.evidenceID + "-incident",
		ModelVersion: 1, DecisionID: graph.decisionID,
		ActionLogID: graph.actionID, VerificationID: graph.verificationID,
		OccurredAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("RecordIncident: %v", err)
	}
	if incident.ID <= 0 || incident.CreditedMinutes != 60 {
		t.Fatalf("incident = %#v", incident)
	}

	snapshot, err := repo.ReadSnapshot(ctx, Filter{Database: databaseName})
	if err != nil {
		t.Fatalf("ReadSnapshot: %v", err)
	}
	assertHonestSnapshot(t, snapshot, databaseName)

	future := time.Now().UTC().Add(time.Hour)
	empty, err := repo.ReadSnapshot(ctx, Filter{
		Database: databaseName, Since: future, Until: future.Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("ReadSnapshot time filter: %v", err)
	}
	assertEmptySnapshot(t, empty)
}

func assertHonestSnapshot(t *testing.T, snapshot Snapshot, databaseName string) {
	t.Helper()
	if snapshot.AllTimeMinutes != 45 || snapshot.MonthMinutes != 45 ||
		snapshot.WeekMinutes != 45 {
		t.Fatalf("realized minutes = %#v, want 45 in current periods", snapshot)
	}
	if snapshot.PotentialMinutes != 45 {
		t.Fatalf("PotentialMinutes = %.2f, want 45", snapshot.PotentialMinutes)
	}
	if snapshot.IncidentMinutes != 60 || len(snapshot.Incidents) != 1 {
		t.Fatalf("incident snapshot = %#v", snapshot)
	}
	if snapshot.ByFeatureMinutes["create_index_concurrently"] != 45 {
		t.Fatalf("ByFeatureMinutes = %#v", snapshot.ByFeatureMinutes)
	}
	if len(snapshot.ByDatabaseMinutes) != 1 ||
		snapshot.ByDatabaseMinutes[0].Name != databaseName ||
		snapshot.ByDatabaseMinutes[0].Minutes != 45 {
		t.Fatalf("ByDatabaseMinutes = %#v", snapshot.ByDatabaseMinutes)
	}
	if len(snapshot.TrendMinutes) != 1 || snapshot.TrendMinutes[0].Minutes != 45 {
		t.Fatalf("TrendMinutes = %#v", snapshot.TrendMinutes)
	}
}

func TestPostgresRepositoryVerifiedCreditIsAtomicAndIdempotent(t *testing.T) {
	pool, ctx := requireValuePostgres(t)
	repo := NewPostgresRepository(pool)
	service := NewService(repo)
	databaseID, _ := insertValueDatabase(t, ctx, pool)
	graph := insertValueEvidenceGraph(t, ctx, pool, databaseID, "credit")

	candidate, err := repo.CreditCandidate(ctx, graph.actionID)
	if err != nil {
		t.Fatalf("CreditCandidate: %v", err)
	}
	if candidate.ActionType != "analyze_table" || candidate.Outcome != "success" ||
		candidate.VerificationState != "verified" || candidate.ModelMinutes != 15 ||
		candidate.ModelVersion != 1 {
		t.Fatalf("candidate = %#v", candidate)
	}
	first, err := service.CreditVerifiedAction(ctx, graph.actionID)
	if err != nil {
		t.Fatalf("CreditVerifiedAction first: %v", err)
	}
	second, err := service.CreditVerifiedAction(ctx, graph.actionID)
	if err != nil {
		t.Fatalf("CreditVerifiedAction second: %v", err)
	}
	if first != second || first.Minutes != 15 || first.ModelVersion != 1 {
		t.Fatalf("credits = first %#v second %#v", first, second)
	}
	assertActionCredit(t, ctx, pool, graph.actionID, 15, 1, "success")

	reverted := insertValueEvidenceGraph(t, ctx, pool, databaseID, "atomic-revert")
	_, err = pool.Exec(ctx, `UPDATE sage.action_log SET outcome='reverted' WHERE id=$1`,
		reverted.actionID)
	if err != nil {
		t.Fatalf("mark action reverted: %v", err)
	}
	err = repo.StampCredit(ctx, reverted.actionID, 15, 1)
	if !errors.Is(err, ErrCreditNotEligible) {
		t.Fatalf("StampCredit on reverted action error = %v", err)
	}
	assertActionCreditNull(t, ctx, pool, reverted.actionID)
}

func TestPostgresRepositoryMissingModelDoesNotPartiallyCredit(t *testing.T) {
	pool, ctx := requireValuePostgres(t)
	repo := NewPostgresRepository(pool)
	databaseID, _ := insertValueDatabase(t, ctx, pool)
	graph := insertValueEvidenceGraph(t, ctx, pool, databaseID, "missing-model")
	_, err := pool.Exec(ctx, `UPDATE sage.action_log SET action_type=$1 WHERE id=$2`,
		"action_without_model", graph.actionID)
	if err != nil {
		t.Fatalf("set unknown action type: %v", err)
	}

	_, err = NewService(repo).CreditVerifiedAction(ctx, graph.actionID)

	if !errors.Is(err, ErrToilModelUnavailable) {
		t.Fatalf("missing-model error = %v", err)
	}
	assertActionCreditNull(t, ctx, pool, graph.actionID)
}

func TestPostgresRepositoryZeroesCreditWhenActionReverts(t *testing.T) {
	pool, ctx := requireValuePostgres(t)
	repo := NewPostgresRepository(pool)
	databaseID, _ := insertValueDatabase(t, ctx, pool)
	graph := insertValueEvidenceGraph(t, ctx, pool, databaseID, "zero-revert")
	if _, err := NewService(repo).CreditVerifiedAction(ctx, graph.actionID); err != nil {
		t.Fatalf("credit before revert: %v", err)
	}

	result, err := repo.ZeroCreditOnRevert(ctx, graph.actionID, "reverted")
	if err != nil {
		t.Fatalf("ZeroCreditOnRevert: %v", err)
	}
	if !result.Applied || result.PreviousMinutes != 15 {
		t.Fatalf("zero result = %#v", result)
	}
	assertActionCredit(t, ctx, pool, graph.actionID, 0, 1, "reverted")
	second, err := repo.ZeroCreditOnRevert(ctx, graph.actionID, "reverted")
	if err != nil {
		t.Fatalf("idempotent ZeroCreditOnRevert: %v", err)
	}
	if second.Applied || second.PreviousMinutes != 0 {
		t.Fatalf("second zero result = %#v", second)
	}
}

func TestPostgresRepositoryCancellationAndMissingRowsPropagate(t *testing.T) {
	pool, ctx := requireValuePostgres(t)
	repo := NewPostgresRepository(pool)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	_, err := repo.ReadSnapshot(cancelled, Filter{})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "snapshot") {
		t.Fatalf("cancelled ReadSnapshot error = %v", err)
	}
	_, err = repo.CreditCandidate(ctx, -1)
	if err == nil || !strings.Contains(err.Error(), "action") {
		t.Fatalf("missing action error = %v", err)
	}
	_, err = repo.RecordIncident(cancelled, IncidentCredit{
		Kind: "lock_storm", Severity: "prevented", EvidenceID: "cancelled",
	})
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "incident") {
		t.Fatalf("cancelled RecordIncident error = %v", err)
	}
}

type valueEvidenceGraph struct {
	decisionID     int64
	actionID       int64
	verificationID int64
	evidenceID     string
}

func requireValuePostgres(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL not set; real-Postgres value test")
	}
	// Bootstrap's migration batch is bounded at 30 seconds. Keep the test
	// deadline above that production contract so it tests behavior, not timing.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect value test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping value test database: %v", err)
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap value test schema: %v", err)
	}
	return pool, ctx
}

func insertValueDatabase(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
) (int64, string) {
	t.Helper()
	name := testEvidenceID("value-db")
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.databases
		(name, host, database_name, username, password_enc)
		VALUES ($1, 'localhost', 'postgres', 'postgres', '\x00') RETURNING id`,
		name).Scan(&id)
	if err != nil {
		t.Fatalf("insert value database: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM sage.databases WHERE id=$1", id) })
	return id, name
}

func insertValueEvidenceGraph(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int64, suffix string,
) valueEvidenceGraph {
	t.Helper()
	graph := valueEvidenceGraph{evidenceID: testEvidenceID("value-" + suffix)}
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision
		(database_id, feature, intent, verdict, risk_tier, reason, evidence_id)
		VALUES ($1, 'index', $2, 'execute', 'safe', 'authorized', $3)
		RETURNING id`, databaseID, suffix, graph.evidenceID).Scan(&graph.decisionID)
	if err != nil {
		t.Fatalf("insert value decision: %v", err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(database_id, action_type, sql_executed, outcome)
		VALUES ($1, 'analyze_table', 'ANALYZE public.orders', 'success')
		RETURNING id`, databaseID).Scan(&graph.actionID)
	if err != nil {
		t.Fatalf("insert value action: %v", err)
	}
	err = pool.QueryRow(ctx, `INSERT INTO sage.verification
		(database_id, decision_id, action_log_id, criterion, baseline,
		 minimum_samples, next_evaluation_at, hard_deadline_at, verdict, completed_at)
		VALUES ($1, $2, $3, '{}', '{}', 30, now(), now()+interval '2 hours',
		 'success', now()) RETURNING id`, databaseID, graph.decisionID,
		graph.actionID).Scan(&graph.verificationID)
	if err != nil {
		t.Fatalf("insert value verification: %v", err)
	}
	t.Cleanup(func() { cleanupValueGraph(pool, graph) })
	return graph
}

func cleanupValueGraph(pool *pgxpool.Pool, graph valueEvidenceGraph) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = pool.Exec(ctx, "DELETE FROM sage.incident_avoided WHERE decision_id=$1",
		graph.decisionID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.verification WHERE id=$1",
		graph.verificationID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id=$1", graph.actionID)
	_, _ = pool.Exec(ctx, "DELETE FROM sage.decision WHERE id=$1", graph.decisionID)
}

func insertCreditedAction(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int64, actionType string, minutes float64,
) {
	t.Helper()
	insertActionValueRow(t, ctx, pool, databaseID, actionType, "success", &minutes)
}

func insertRevertedAction(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int64, actionType string, minutes float64,
) {
	t.Helper()
	insertActionValueRow(t, ctx, pool, databaseID, actionType, "reverted", &minutes)
}

func insertActionValueRow(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int64, actionType, outcome string, minutes *float64,
) {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(database_id, action_type, sql_executed, outcome,
		 toil_minutes_saved, toil_model_version)
		VALUES ($1, $2, 'SELECT 1', $3, $4, 1) RETURNING id`,
		databaseID, actionType, outcome, minutes).Scan(&id)
	if err != nil {
		t.Fatalf("insert action value row: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id=$1", id) })
}

func insertPendingAction(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int64, actionType string,
) {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_queue
		(database_id, proposed_sql, action_risk, status, action_type)
		VALUES ($1, 'SELECT 1', 'safe', 'pending', $2) RETURNING id`,
		databaseID, actionType).Scan(&id)
	if err != nil {
		t.Fatalf("insert pending action: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(ctx, "DELETE FROM sage.action_queue WHERE id=$1", id) })
}

func assertEmptySnapshot(t *testing.T, snapshot Snapshot) {
	t.Helper()
	if snapshot.AllTimeMinutes != 0 || snapshot.MonthMinutes != 0 ||
		snapshot.WeekMinutes != 0 || snapshot.PotentialMinutes != 0 ||
		snapshot.IncidentMinutes != 0 || len(snapshot.Incidents) != 0 {
		t.Fatalf("Snapshot = %#v, want empty", snapshot)
	}
}

func assertActionCredit(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	actionID int64, minutes float64, modelVersion int, outcome string,
) {
	t.Helper()
	var gotMinutes float64
	var gotModel int
	var gotOutcome string
	err := pool.QueryRow(ctx, `SELECT toil_minutes_saved, toil_model_version, outcome
		FROM sage.action_log WHERE id=$1`, actionID).Scan(
		&gotMinutes, &gotModel, &gotOutcome)
	if err != nil {
		t.Fatalf("read action credit: %v", err)
	}
	if gotMinutes != minutes || gotModel != modelVersion || gotOutcome != outcome {
		t.Fatalf("action credit = minutes %.2f model %d outcome %s",
			gotMinutes, gotModel, gotOutcome)
	}
}

func assertActionCreditNull(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, actionID int64,
) {
	t.Helper()
	var minutes *float64
	var modelVersion *int
	err := pool.QueryRow(ctx, `SELECT toil_minutes_saved, toil_model_version
		FROM sage.action_log WHERE id=$1`, actionID).Scan(&minutes, &modelVersion)
	if err != nil {
		t.Fatalf("read null action credit: %v", err)
	}
	if minutes != nil || modelVersion != nil {
		t.Fatalf("failed credit partially wrote: %v %v", minutes, modelVersion)
	}
}

func testEvidenceID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
