package value

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func nearMiss(actionID int64) AvoidedIncident {
	return AvoidedIncident{ActionID: actionID, Kind: IncidentXIDWraparound,
		Severity: SeverityNearMiss, OccurredAt: time.Now().UTC()}
}

func incidentRows(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, actionID int64,
) int {
	t.Helper()
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.incident_avoided
		WHERE action_log_id=$1`, actionID).Scan(&count); err != nil {
		t.Fatalf("count incidents: %v", err)
	}
	return count
}

func TestIncidentCandidateReadsTheVerifiedEvidenceGraph(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "incident_candidate")
	repo := NewPostgresRepository(source.Pool)
	graph := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "candidate")

	candidate, err := repo.IncidentCandidate(ctx, graph.actionID)
	if err != nil {
		t.Fatalf("IncidentCandidate: %v", err)
	}
	if candidate.ActionID != graph.actionID || candidate.DecisionID != graph.decisionID ||
		candidate.VerificationID != graph.verificationID ||
		candidate.Outcome != "success" || candidate.VerificationState != "verified" {
		t.Fatalf("candidate = %#v, graph = %#v", candidate, graph)
	}
	if candidate.DatabaseID == nil || *candidate.DatabaseID != 7 {
		t.Fatalf("database id = %v, want 7", candidate.DatabaseID)
	}
}

func TestIncidentCandidateReportsUnverifiedAndMissingActions(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "incident_unverified")
	repo := NewPostgresRepository(source.Pool)
	graph := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "unverified")
	if _, err := source.Pool.Exec(ctx, `UPDATE sage.verification
		SET verdict='pending', completed_at=NULL WHERE id=$1`, graph.verificationID); err != nil {
		t.Fatalf("reopen verification: %v", err)
	}

	candidate, err := repo.IncidentCandidate(ctx, graph.actionID)
	if err != nil || candidate.VerificationState == "verified" {
		t.Fatalf("pending candidate = %#v, %v", candidate, err)
	}
	_, err = NewService(repo).CreditAvoidedIncident(ctx, nearMiss(graph.actionID))
	if !errors.Is(err, ErrCreditNotEligible) || incidentRows(t, ctx, source.Pool, graph.actionID) != 0 {
		t.Fatalf("pending credit error = %v", err)
	}

	var unverifiedID int64
	if err := source.Pool.QueryRow(ctx, `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome) VALUES ('vacuum', 'VACUUM t', 'success')
		RETURNING id`).Scan(&unverifiedID); err != nil {
		t.Fatalf("insert bare action: %v", err)
	}
	bare, err := repo.IncidentCandidate(ctx, unverifiedID)
	if err != nil || bare.VerificationState != "missing" || bare.VerificationID != 0 ||
		bare.DatabaseID != nil {
		t.Fatalf("bare candidate = %#v, %v", bare, err)
	}

	_, err = repo.IncidentCandidate(ctx, 1<<40)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("missing action error = %v", err)
	}
}

func TestCreditAvoidedIncidentWritesOneLedgerRowPerAction(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "incident_once")
	service := NewService(NewPostgresRepository(source.Pool))
	graph := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "once")

	record, err := service.CreditAvoidedIncident(ctx, nearMiss(graph.actionID))
	if err != nil || record.ID <= 0 || record.CreditedMinutes != 120 {
		t.Fatalf("first credit = %#v, %v", record, err)
	}
	var kind, severity, evidence string
	var minutes float64
	var decisionID, verificationID int64
	var databaseID *int64
	if err := source.Pool.QueryRow(ctx, `SELECT kind, severity, evidence_id,
		credited_minutes::float8, decision_id, verification_id, database_id
		FROM sage.incident_avoided WHERE id=$1`, record.ID).Scan(&kind, &severity,
		&evidence, &minutes, &decisionID, &verificationID, &databaseID); err != nil {
		t.Fatalf("read incident: %v", err)
	}
	if kind != "xid_wraparound" || severity != "near_miss" || minutes != 120 ||
		decisionID != graph.decisionID || verificationID != graph.verificationID ||
		databaseID == nil || *databaseID != 7 ||
		!strings.HasSuffix(evidence, ":action:"+strconv.FormatInt(graph.actionID, 10)) {
		t.Fatalf("row = %s/%s/%s/%v decision=%d verification=%d db=%v",
			kind, severity, evidence, minutes, decisionID, verificationID, databaseID)
	}

	again, err := service.CreditAvoidedIncident(ctx, nearMiss(graph.actionID))
	if !errors.Is(err, ErrIncidentAlreadyCredited) || again != (IncidentRecord{}) {
		t.Fatalf("second credit = %#v, %v", again, err)
	}
	if rows := incidentRows(t, ctx, source.Pool, graph.actionID); rows != 1 {
		t.Fatalf("incident rows = %d, want 1", rows)
	}

	results, err := fleetOf(source).Read(ctx, Filter{})
	if err != nil || len(results) != 1 || results[0].Err != nil {
		t.Fatalf("Read: %+v err=%v", results, err)
	}
	snapshot := results[0].Snapshot
	if snapshot.IncidentMinutes != 120 || len(snapshot.Incidents) != 1 ||
		snapshot.Incidents[0].Kind != "xid_wraparound" || snapshot.AllTimeMinutes != 0 {
		t.Fatalf("snapshot = %#v, want one 120-minute incident and no toil", snapshot)
	}
}

func TestCreditAvoidedIncidentConcurrentCallersCreditOnce(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "incident_race")
	service := NewService(NewPostgresRepository(source.Pool))
	graph := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "race")

	const callers = 8
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func(slot int) {
			defer wg.Done()
			_, errs[slot] = service.CreditAvoidedIncident(ctx, nearMiss(graph.actionID))
		}(i)
	}
	wg.Wait()
	credited, duplicates := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			credited++
		case errors.Is(err, ErrIncidentAlreadyCredited):
			duplicates++
		default:
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if credited != 1 || duplicates != callers-1 {
		t.Fatalf("credited=%d duplicates=%d, want 1/%d", credited, duplicates, callers-1)
	}
	if rows := incidentRows(t, ctx, source.Pool, graph.actionID); rows != 1 {
		t.Fatalf("incident rows = %d, want 1", rows)
	}
}

func TestCreditAvoidedIncidentReportsLostConnection(t *testing.T) {
	ctx := context.Background()
	source := newLedgerSource(t, "incident_closed")
	graph := insertValueEvidenceGraph(t, ctx, source.Pool, 7, "closed")
	cancelled, cancel := context.WithCancel(ctx)
	cancel()

	_, err := NewService(NewPostgresRepository(source.Pool)).
		CreditAvoidedIncident(cancelled, nearMiss(graph.actionID))
	if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), "incident candidate") {
		t.Fatalf("cancelled credit error = %v", err)
	}
	if rows := incidentRows(t, ctx, source.Pool, graph.actionID); rows != 0 {
		t.Fatalf("incident rows after cancelled credit = %d, want 0", rows)
	}
}
