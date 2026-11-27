package value

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// A rollback of a credited action voids its incident credit in the same
// statement that zeroes its toil credit: the next report drops the
// incident minutes, and the row keeps credited_minutes as evidence.
func TestRevertVoidsTheActionsIncidentCredit(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "incident_revert")
	repo := NewPostgresRepository(source.Pool)
	service := NewService(repo)
	wal := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "wal-bound")
	kept := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "kept")
	for _, graph := range []valueEvidenceGraph{wal, kept} {
		incident := nearMiss(graph.actionID)
		incident.Kind = IncidentDiskFullSlot
		if _, err := service.CreditAvoidedIncident(ctx, incident); err != nil {
			t.Fatalf("credit action %d: %v", graph.actionID, err)
		}
	}
	if got := incidentSnapshot(t, ctx, source); got.IncidentMinutes != 240 ||
		len(got.Incidents) != 2 {
		t.Fatalf("before the rollback = %v %+v, want two incidents",
			got.IncidentMinutes, got.Incidents)
	}

	if _, err := repo.ZeroCreditOnRevert(ctx, wal.actionID, "rolled_back"); err != nil {
		t.Fatalf("ZeroCreditOnRevert: %v", err)
	}
	keptEvidence := "incident:disk_full_slot:action:" + strconv.FormatInt(kept.actionID, 10)
	got := incidentSnapshot(t, ctx, source)
	if got.IncidentMinutes != NearMissMinutes || len(got.Incidents) != 1 ||
		got.Incidents[0].EvidenceID != keptEvidence {
		t.Fatalf("after the rollback = %v %+v, want only %s",
			got.IncidentMinutes, got.Incidents, keptEvidence)
	}
	minutes, revertedAt := incidentRow(t, ctx, source, wal.actionID)
	if minutes != NearMissMinutes || revertedAt == nil {
		t.Fatalf("voided row = %v minutes, reverted_at %v; want 120 and a stamp",
			minutes, revertedAt)
	}
	if _, keptAt := incidentRow(t, ctx, source, kept.actionID); keptAt != nil {
		t.Fatalf("the other action's incident was stamped %v", keptAt)
	}

	assertSecondRevertKeepsTheStamp(t, ctx, source, wal.actionID, *revertedAt)
}

// A second rollback keeps the first stamp; a new credit is refused.
func assertSecondRevertKeepsTheStamp(
	t *testing.T, ctx context.Context, source Source, actionID int64, first time.Time,
) {
	t.Helper()
	repo := NewPostgresRepository(source.Pool)
	if _, err := repo.ZeroCreditOnRevert(ctx, actionID, "rolled_back"); err != nil {
		t.Fatalf("second ZeroCreditOnRevert: %v", err)
	}
	if _, again := incidentRow(t, ctx, source, actionID); again == nil ||
		!again.Equal(first) {
		t.Fatalf("second rollback moved reverted_at %v -> %v", first, again)
	}
	_, err := NewService(repo).CreditAvoidedIncident(ctx, nearMiss(actionID))
	if !errors.Is(err, ErrCreditNotEligible) {
		t.Fatalf("credit after the rollback = %v, want ErrCreditNotEligible", err)
	}
}

// The narrowed CHECKs refuse the retired kind and severity at the ledger,
// whatever the caller (earned-09).
func TestIncidentLedgerRefusesRetiredKindAndSeverity(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "incident_retired")
	repo := NewPostgresRepository(source.Pool)
	graph := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "retired")
	for _, tc := range []struct{ kind, severity, constraint string }{
		{"lock_storm", SeverityNearMiss, "incident_avoided_kind_produced"},
		{IncidentXIDWraparound, "prevented", "incident_avoided_severity_produced"},
	} {
		_, err := repo.RecordIncident(ctx, IncidentCredit{
			Kind: tc.kind, Severity: tc.severity, CreditedMinutes: 480,
			EvidenceID: graph.evidenceID + tc.kind + tc.severity, ModelVersion: 1,
			DecisionID: graph.decisionID, ActionLogID: graph.actionID,
			VerificationID: graph.verificationID, OccurredAt: time.Now().UTC(),
		})
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" ||
			pgErr.ConstraintName != tc.constraint {
			t.Fatalf("%s/%s: err = %v, want a %s CHECK violation",
				tc.kind, tc.severity, err, tc.constraint)
		}
	}
	if rows := incidentRows(t, ctx, source.Pool, graph.actionID); rows != 0 {
		t.Fatalf("incident rows = %d, want 0", rows)
	}
}

func incidentSnapshot(t *testing.T, ctx context.Context, source Source) Snapshot {
	t.Helper()
	results, err := fleetOf(source).Read(ctx, Filter{})
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("Read: %+v err=%v", results, err)
	}
	return results[0].Snapshot
}

func incidentRow(
	t *testing.T, ctx context.Context, source Source, actionID int64,
) (float64, *time.Time) {
	t.Helper()
	var minutes float64
	var revertedAt *time.Time
	if err := source.Pool.QueryRow(ctx, `SELECT credited_minutes::float8, reverted_at
		FROM sage.incident_avoided WHERE action_log_id=$1`, actionID).Scan(
		&minutes, &revertedAt); err != nil {
		t.Fatalf("read incident of action %d: %v", actionID, err)
	}
	return minutes, revertedAt
}
