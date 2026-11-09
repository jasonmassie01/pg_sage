package value

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// An action can have several verification rows; both writers link the
// newest through action_log.verification_id. An older success must not
// earn credit after a newer revert (earned-09).
func TestCreditReadsOnlyTheLinkedVerification(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "credit_linked")
	repo := NewPostgresRepository(source.Pool)
	graph := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "older-success")
	newer := insertVerificationRow(t, ctx, source.Pool, graph, "revert", true)

	candidate, err := repo.CreditCandidate(ctx, graph.actionID)
	if err != nil || candidate.VerificationState != "revert" {
		t.Fatalf("credit candidate = %#v, %v; want the linked revert", candidate, err)
	}
	if err := repo.StampCredit(ctx, graph.actionID, 15, 1); !errors.Is(err,
		ErrCreditNotEligible) {
		t.Fatalf("StampCredit over an older success = %v, want ErrCreditNotEligible", err)
	}
	if _, err := NewService(repo).CreditVerifiedAction(ctx, graph.actionID); !errors.Is(err,
		ErrCreditNotEligible) {
		t.Fatalf("CreditVerifiedAction = %v, want ErrCreditNotEligible", err)
	}
	assertActionCreditNull(t, ctx, source.Pool, graph.actionID)

	incident, err := repo.IncidentCandidate(ctx, graph.actionID)
	if err != nil || incident.VerificationID != newer ||
		incident.VerificationState != "revert" {
		t.Fatalf("incident candidate = %#v, %v; want verification %d (revert)",
			incident, err, newer)
	}
	_, err = NewService(repo).CreditAvoidedIncident(ctx, nearMiss(graph.actionID))
	if !errors.Is(err, ErrCreditNotEligible) ||
		incidentRows(t, ctx, source.Pool, graph.actionID) != 0 {
		t.Fatalf("incident credit over an older success = %v", err)
	}
}

// A newer row that the action does not link (no writer leaves one, so it
// is not evidence) never earns credit; a stale failure before the linked
// success does not block it.
func TestCreditIgnoresUnlinkedVerificationRows(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "credit_unlinked")
	repo := NewPostgresRepository(source.Pool)
	failed := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "linked-failure")
	if _, err := source.Pool.Exec(ctx, `UPDATE sage.verification SET verdict='failed'
		WHERE id=$1`, failed.verificationID); err != nil {
		t.Fatalf("fail the linked verification: %v", err)
	}
	insertVerificationRow(t, ctx, source.Pool, failed, "success", false)
	incident, err := repo.IncidentCandidate(ctx, failed.actionID)
	if err != nil || incident.VerificationState != "failed" ||
		incident.VerificationID != failed.verificationID {
		t.Fatalf("incident candidate = %#v, %v; want the linked failure", incident, err)
	}
	if err := repo.StampCredit(ctx, failed.actionID, 15, 1); !errors.Is(err,
		ErrCreditNotEligible) {
		t.Fatalf("StampCredit with an unlinked success = %v", err)
	}

	verified := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "stale-failure")
	insertVerificationRow(t, ctx, source.Pool, verified, "failed", false)
	credit, err := NewService(repo).CreditVerifiedAction(ctx, verified.actionID)
	if err != nil || credit.Minutes != 15 || credit.ModelVersion != 1 {
		t.Fatalf("credit with a linked success = %#v, %v; want 15 minutes", credit, err)
	}
	assertActionCredit(t, ctx, source.Pool, verified.actionID, 15, 1, "success")
}

// insertVerificationRow adds a completed verification for the graph's
// action and, when link is set, repoints the action at it as the
// verification writers do.
func insertVerificationRow(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	graph valueEvidenceGraph, verdict string, link bool,
) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.verification
		(decision_id, action_log_id, criterion, baseline, minimum_samples,
		 next_evaluation_at, hard_deadline_at, verdict, completed_at)
		VALUES ($1, $2, '{}', '{}', 1, now(), now(), $3, now()) RETURNING id`,
		graph.decisionID, graph.actionID, verdict).Scan(&id); err != nil {
		t.Fatalf("insert %s verification: %v", verdict, err)
	}
	if !link {
		return id
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET verification_id=$1
		WHERE id=$2`, id, graph.actionID); err != nil {
		t.Fatalf("link %s verification: %v", verdict, err)
	}
	return id
}
