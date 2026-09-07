package ledger

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/schema"
)

func TestPostgresRepositoryDecisionJSONPersistence(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	repo := NewPostgresRepository(pool)
	databaseID := ledgerDatabaseID()
	hardAt := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Microsecond)
	input := validPostgresDecision(ledgerEvidenceID("roundtrip"))
	input.DatabaseID = &databaseID
	input.TargetObjects = []string{"public.orders", "public.customers"}
	input.Evidence = map[string]any{
		"planner": "hypopg", "selected": true,
		"costs": map[string]any{"before": "100", "after": "60"},
	}
	input.ProposedSQL = "CREATE INDEX CONCURRENTLY idx ON public.orders (status)"
	input.DeadlineKind = "disk"
	input.DeadlineHardAt = &hardAt

	id, err := repo.InsertDecision(ctx, input)
	if err != nil {
		t.Fatalf("InsertDecision: %v", err)
	}
	if id <= 0 {
		t.Fatalf("decision ID = %d, want positive", id)
	}
	t.Cleanup(func() { deleteLedgerDecision(pool, id) })
	assertPersistedDecision(t, ctx, pool, id, input, hardAt)
}

func assertPersistedDecision(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	id int64, input DecisionInput, hardAt time.Time,
) {
	t.Helper()
	var targetJSON, evidenceJSON []byte
	var feature, intent, verdict, risk, reason, evidenceID, deadlineKind string
	var policyVersion int
	var deadlineHardAt time.Time
	err := pool.QueryRow(ctx, `SELECT feature, intent, target_objects, policy_version,
		verdict, risk_tier, reason, deadline_kind, deadline_hard_at,
		evidence, evidence_id FROM sage.decision WHERE id=$1`, id).Scan(
		&feature, &intent, &targetJSON, &policyVersion, &verdict, &risk, &reason,
		&deadlineKind, &deadlineHardAt, &evidenceJSON, &evidenceID)
	if err != nil {
		t.Fatalf("read persisted decision: %v", err)
	}
	if feature != input.Feature || intent != input.Intent || verdict != string(input.Verdict) ||
		risk != input.RiskTier || reason != input.Reason || policyVersion != input.PolicyVersion ||
		evidenceID != input.EvidenceID || deadlineKind != input.DeadlineKind ||
		!deadlineHardAt.Equal(hardAt) {
		t.Fatalf("persisted scalar decision fields do not match input")
	}
	var targets []string
	var evidence map[string]any
	if err := json.Unmarshal(targetJSON, &targets); err != nil {
		t.Fatalf("decode target_objects: %v", err)
	}
	if err := json.Unmarshal(evidenceJSON, &evidence); err != nil {
		t.Fatalf("decode evidence: %v", err)
	}
	wantEvidence := map[string]any{
		"planner": "hypopg", "selected": true,
		"costs":        map[string]any{"before": "100", "after": "60"},
		"proposed_sql": input.ProposedSQL,
	}
	if !reflect.DeepEqual(targets, input.TargetObjects) {
		t.Fatalf("target_objects = %#v, want %#v", targets, input.TargetObjects)
	}
	if !reflect.DeepEqual(evidence, wantEvidence) {
		t.Fatalf("evidence = %#v, want %#v", evidence, wantEvidence)
	}
	if evidence["proposed_sql"] != input.ProposedSQL {
		t.Fatalf("evidence proposed_sql = %v, want exact SQL", evidence["proposed_sql"])
	}
}

func TestPostgresRepositoryEvidenceIDIsUnique(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	repo := NewPostgresRepository(pool)
	evidenceID := ledgerEvidenceID("unique")
	first := validPostgresDecision(evidenceID)
	id, err := repo.InsertDecision(ctx, first)
	if err != nil {
		t.Fatalf("first InsertDecision: %v", err)
	}
	t.Cleanup(func() { deleteLedgerDecision(pool, id) })
	second := validPostgresDecision(evidenceID)
	second.Intent = "must not overwrite first decision"

	_, err = repo.InsertDecision(ctx, second)

	if !errors.Is(err, ErrEvidenceConflict) ||
		!strings.Contains(err.Error(), evidenceID) {
		t.Fatalf("duplicate evidence error = %v", err)
	}
	var count int
	if scanErr := pool.QueryRow(ctx, `SELECT count(*) FROM sage.decision
		WHERE evidence_id=$1`, evidenceID).Scan(&count); scanErr != nil {
		t.Fatalf("count duplicate evidence: %v", scanErr)
	}
	if count != 1 {
		t.Fatalf("decisions with evidence %q = %d, want 1", evidenceID, count)
	}
}

func TestPostgresRepositorySelfAuditFindsMissingDecisionAndVerification(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	repo := NewPostgresRepository(pool)
	databaseID := ledgerDatabaseID()
	missingDecision := insertAuditAction(t, ctx, pool, databaseID, "success", nil)
	decisionID := insertAuditDecision(t, ctx, pool, databaseID, "missing-verification")
	missingVerification := insertAuditAction(
		t, ctx, pool, databaseID, "success", &decisionID)
	completeDecision := insertAuditDecision(t, ctx, pool, databaseID, "complete")
	completeAction := insertAuditAction(t, ctx, pool, databaseID, "success", &completeDecision)
	insertAuditVerification(t, ctx, pool, databaseID, completeDecision, completeAction)
	revertedDecision := insertAuditDecision(t, ctx, pool, databaseID, "reverted")
	insertAuditAction(t, ctx, pool, databaseID, "reverted", &revertedDecision)

	violations, err := repo.FindAuditViolations(ctx)
	if err != nil {
		t.Fatalf("FindAuditViolations: %v", err)
	}
	ours := violationsForActions(violations, map[int64]bool{
		missingDecision: true, missingVerification: true, completeAction: true,
	})
	want := []AuditViolation{
		{ActionID: missingDecision, Kind: "missing_decision"},
		{ActionID: missingVerification, Kind: "missing_verification"},
	}
	sortViolations(ours)
	sortViolations(want)
	if !reflect.DeepEqual(ours, want) {
		t.Fatalf("audit violations = %#v, want %#v", ours, want)
	}
}

func TestPostgresRepositoryCancellationAndConstraintErrorsPropagate(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	repo := NewPostgresRepository(pool)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	_, err := repo.InsertDecision(cancelled, validPostgresDecision(
		ledgerEvidenceID("cancelled")))
	if !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "persist decision") {
		t.Fatalf("cancelled InsertDecision error = %v", err)
	}
	_, err = repo.FindAuditViolations(cancelled)
	if !errors.Is(err, context.Canceled) ||
		!strings.Contains(err.Error(), "self-audit") {
		t.Fatalf("cancelled FindAuditViolations error = %v", err)
	}

	invalid := validPostgresDecision(ledgerEvidenceID("invalid"))
	invalid.Verdict = Verdict("silently_allow_everything")
	_, err = repo.InsertDecision(ctx, invalid)
	if err == nil || !strings.Contains(err.Error(), "persist decision") ||
		!strings.Contains(err.Error(), "verdict") {
		t.Fatalf("invalid verdict error = %v", err)
	}
	var count int
	if scanErr := pool.QueryRow(ctx, `SELECT count(*) FROM sage.decision
		WHERE evidence_id=$1`, invalid.EvidenceID).Scan(&count); scanErr != nil {
		t.Fatalf("count invalid decision: %v", scanErr)
	}
	if count != 0 {
		t.Fatalf("invalid decision rows = %d, want 0", count)
	}
}

func requireLedgerPostgres(t *testing.T) (*pgxpool.Pool, context.Context) {
	t.Helper()
	dsn := os.Getenv("SAGE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SAGE_TEST_DATABASE_URL not set; real-Postgres ledger test")
	}
	// Bootstrap's migration batch is bounded at 30 seconds. Keep the test
	// deadline above that production contract so it tests behavior, not timing.
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect ledger test database: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := pool.Ping(ctx); err != nil {
		t.Fatalf("ping ledger test database: %v", err)
	}
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap ledger test schema: %v", err)
	}
	return pool, ctx
}

func validPostgresDecision(evidenceID string) DecisionInput {
	return DecisionInput{
		Feature: "index", Intent: "test decision",
		Evidence:    map[string]any{"source": "postgres_test"},
		ProposedSQL: "CREATE INDEX CONCURRENTLY idx ON public.orders (status)",
		Verdict:     VerdictExecute, Reason: "authorized", RiskTier: "safe",
		PolicyVersion: 1, TargetObjects: []string{"public.orders"},
		EvidenceID: evidenceID,
	}
}

func violationsForActions(
	violations []AuditViolation, actionIDs map[int64]bool,
) []AuditViolation {
	result := make([]AuditViolation, 0, len(actionIDs))
	for _, violation := range violations {
		if actionIDs[violation.ActionID] {
			violation.Detail = ""
			result = append(result, violation)
		}
	}
	return result
}

func sortViolations(violations []AuditViolation) {
	sort.Slice(violations, func(i, j int) bool {
		if violations[i].ActionID == violations[j].ActionID {
			return violations[i].Kind < violations[j].Kind
		}
		return violations[i].ActionID < violations[j].ActionID
	})
}

func insertAuditDecision(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int, suffix string,
) int64 {
	t.Helper()
	var id int64
	evidenceID := ledgerEvidenceID("audit-" + suffix)
	err := pool.QueryRow(ctx, `INSERT INTO sage.decision
		(database_id, feature, intent, policy_version, verdict, risk_tier,
		 reason, evidence_id) VALUES ($1, 'index', $2, 1, 'execute',
		 'safe', 'authorized', $3) RETURNING id`, databaseID, suffix,
		evidenceID).Scan(&id)
	if err != nil {
		t.Fatalf("insert audit decision: %v", err)
	}
	t.Cleanup(func() { deleteLedgerDecision(pool, id) })
	return id
}

func insertAuditAction(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int, outcome string, decisionID *int64,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(database_id, action_type, sql_executed, outcome, decision_id)
		VALUES ($1, 'create_index_concurrently', 'SELECT 1', $2, $3)
		RETURNING id`, databaseID, outcome, decisionID).Scan(&id)
	if err != nil {
		t.Fatalf("insert audit action: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM sage.verification WHERE action_log_id=$1", id)
		_, _ = pool.Exec(cleanupCtx, "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	return id
}

func insertAuditVerification(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	databaseID int, decisionID, actionID int64,
) {
	t.Helper()
	var id int64
	err := pool.QueryRow(ctx, `INSERT INTO sage.verification
		(database_id, decision_id, action_log_id, criterion, baseline,
		 minimum_samples, next_evaluation_at, hard_deadline_at, verdict, completed_at)
		VALUES ($1, $2, $3, '{}', '{}', 30, now(), now()+interval '2 hours',
		 'success', now()) RETURNING id`, databaseID, decisionID, actionID).Scan(&id)
	if err != nil {
		t.Fatalf("insert audit verification: %v", err)
	}
	_, err = pool.Exec(ctx, "UPDATE sage.action_log SET verification_id=$1 WHERE id=$2",
		id, actionID)
	if err != nil {
		t.Fatalf("link audit verification: %v", err)
	}
}

func deleteLedgerDecision(pool *pgxpool.Pool, decisionID int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _ = pool.Exec(ctx, "DELETE FROM sage.decision WHERE id=$1", decisionID)
}

func ledgerDatabaseID() int {
	return 950000 + int(time.Now().UnixNano()%40000)
}

func ledgerEvidenceID(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}
