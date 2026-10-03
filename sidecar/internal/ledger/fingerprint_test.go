package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Dogfood lifeos: the policy gate inserted a new sage.decision row (with a
// random evidence ID) on every evaluation, so an unchanged parked or queued
// candidate added a row every cycle. A non-execute verdict now carries a
// fingerprint, and repeats of an open fingerprint update one row.

func parkedInput(tag string, databaseID int) DecisionInput {
	input := validPostgresDecision("")
	input.DatabaseID = &databaseID
	input.Verdict = VerdictPark
	input.Reason = "outside maintenance window"
	input.TargetObjects = []string{"public." + tag}
	input.Fingerprint = DecisionFingerprint(input)
	return input
}

func TestDecisionFingerprintIsStableAndSpecific(t *testing.T) {
	base := parkedInput("orders", 7)
	if base.Fingerprint == "" || DecisionFingerprint(base) != base.Fingerprint {
		t.Fatalf("fingerprint %q is empty or unstable", base.Fingerprint)
	}
	reordered := base
	reordered.TargetObjects = []string{"public.b", "public.a"}
	sorted := base
	sorted.TargetObjects = []string{"public.a", "public.b"}
	if DecisionFingerprint(reordered) != DecisionFingerprint(sorted) {
		t.Fatal("target order changed the fingerprint")
	}
	reason := base
	reason.Reason = "blast radius exceeded"
	reason.EvidenceID = "ev_other"
	if DecisionFingerprint(reason) != base.Fingerprint {
		t.Fatal("reason or evidence ID changed the fingerprint")
	}
	other := 8
	changes := map[string]func(*DecisionInput){
		"database":       func(in *DecisionInput) { in.DatabaseID = &other },
		"no database":    func(in *DecisionInput) { in.DatabaseID = nil },
		"feature":        func(in *DecisionInput) { in.Feature = "vacuum" },
		"intent":         func(in *DecisionInput) { in.Intent = "operator_approved" },
		"target":         func(in *DecisionInput) { in.TargetObjects = []string{"public.x"} },
		"verdict":        func(in *DecisionInput) { in.Verdict = VerdictQueueApproval },
		"policy version": func(in *DecisionInput) { in.PolicyVersion = 2 },
		"sql":            func(in *DecisionInput) { in.ProposedSQL = "VACUUM public.orders" },
	}
	for name, change := range changes {
		changed := base
		change(&changed)
		if DecisionFingerprint(changed) == base.Fingerprint {
			t.Errorf("%s change kept the fingerprint", name)
		}
	}
}

func decisionRow(
	t *testing.T, pool *pgxpool.Pool, id int64,
) (repeats int, lastSeen *time.Time, reason string) {
	t.Helper()
	err := pool.QueryRow(context.Background(), `SELECT repeat_count, last_seen_at, reason
		FROM sage.decision WHERE id=$1`, id).Scan(&repeats, &lastSeen, &reason)
	if err != nil {
		t.Fatalf("read decision %d: %v", id, err)
	}
	return repeats, lastSeen, reason
}

func countFingerprint(t *testing.T, pool *pgxpool.Pool, fingerprint string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.decision
		WHERE fingerprint=$1`, fingerprint).Scan(&n); err != nil {
		t.Fatalf("count fingerprint: %v", err)
	}
	return n
}

func cleanupFingerprint(t *testing.T, pool *pgxpool.Pool, fingerprint string) {
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.decision WHERE fingerprint=$1", fingerprint)
	})
}

func TestRepeatedNonExecuteVerdictUpdatesOneRow(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	input := parkedInput(fmt.Sprintf("fp_%d", time.Now().UnixNano()), ledgerDatabaseID())
	cleanupFingerprint(t, pool, input.Fingerprint)
	first, err := service.RecordDecision(ctx, input)
	if err != nil {
		t.Fatalf("first RecordDecision: %v", err)
	}
	input.Reason = "blast radius exceeded"
	for i := 0; i < 2; i++ {
		again, err := service.RecordDecision(ctx, input)
		if err != nil {
			t.Fatalf("repeat RecordDecision: %v", err)
		}
		if again.ID != first.ID || again.EvidenceID != first.EvidenceID {
			t.Fatalf("repeat = %d/%s, want the first row %d/%s", again.ID,
				again.EvidenceID, first.ID, first.EvidenceID)
		}
	}
	repeats, lastSeen, reason := decisionRow(t, pool, first.ID)
	if countFingerprint(t, pool, input.Fingerprint) != 1 || repeats != 3 ||
		lastSeen == nil || reason != "blast radius exceeded" {
		t.Fatalf("rows=%d repeats=%d last_seen=%v reason=%q, want 1 row seen 3 times "+
			"with the latest reason", countFingerprint(t, pool, input.Fingerprint),
			repeats, lastSeen, reason)
	}
}

func TestChangedPolicyVersionStartsANewRow(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	input := parkedInput(fmt.Sprintf("fpv_%d", time.Now().UnixNano()), ledgerDatabaseID())
	cleanupFingerprint(t, pool, input.Fingerprint)
	first, err := service.RecordDecision(ctx, input)
	if err != nil {
		t.Fatalf("RecordDecision v1: %v", err)
	}
	input.PolicyVersion = 2
	input.Fingerprint = DecisionFingerprint(input)
	cleanupFingerprint(t, pool, input.Fingerprint)
	second, err := service.RecordDecision(ctx, input)
	if err != nil || second.ID == first.ID {
		t.Fatalf("v2 decision = %d (%v), want a new row beside %d", second.ID, err, first.ID)
	}
}

// Execute verdicts back actions: each keeps its own row.
func TestExecuteVerdictsWithoutFingerprintKeepOwnRows(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	ids := map[int64]bool{}
	for i := 0; i < 3; i++ {
		input := validPostgresDecision("")
		decision, err := service.RecordDecision(ctx, input)
		if err != nil {
			t.Fatalf("RecordDecision: %v", err)
		}
		t.Cleanup(func() { deleteLedgerDecision(pool, decision.ID) })
		ids[decision.ID] = true
		repeats, lastSeen, _ := decisionRow(t, pool, decision.ID)
		if repeats != 1 || lastSeen != nil {
			t.Fatalf("execute row repeats=%d last_seen=%v, want 1 and NULL", repeats, lastSeen)
		}
	}
	if len(ids) != 3 {
		t.Fatalf("3 execute verdicts made %d rows, want 3", len(ids))
	}
}

// A resolved decision is history: the next equal verdict opens a new row.
func TestResolvedFingerprintRowIsNotReused(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	input := parkedInput(fmt.Sprintf("fpr_%d", time.Now().UnixNano()), ledgerDatabaseID())
	cleanupFingerprint(t, pool, input.Fingerprint)
	first, err := service.RecordDecision(ctx, input)
	if err != nil {
		t.Fatalf("RecordDecision: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE sage.decision SET resolved_at=now() WHERE id=$1",
		first.ID); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	second, err := service.RecordDecision(ctx, input)
	if err != nil || second.ID == first.ID {
		t.Fatalf("after resolve = %d (%v), want a new row", second.ID, err)
	}
}

func TestConcurrentRepeatsShareOneRow(t *testing.T) {
	pool, ctx := requireLedgerPostgres(t)
	service := NewService(NewPostgresRepository(pool))
	input := parkedInput(fmt.Sprintf("fpc_%d", time.Now().UnixNano()), ledgerDatabaseID())
	cleanupFingerprint(t, pool, input.Fingerprint)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.RecordDecision(ctx, input)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent RecordDecision: %v", err)
		}
	}
	var repeats int
	if err := pool.QueryRow(ctx, `SELECT sum(repeat_count) FROM sage.decision
		WHERE fingerprint=$1`, input.Fingerprint).Scan(&repeats); err != nil {
		t.Fatalf("sum repeats: %v", err)
	}
	if n := countFingerprint(t, pool, input.Fingerprint); n != 1 || repeats != 8 {
		t.Fatalf("rows=%d repeats=%d, want 1 row seen 8 times", n, repeats)
	}
}

// A repository without fingerprint support (tests, fakes) still records
// every decision.
func TestServiceInsertsWhenRepositoryCannotUpsert(t *testing.T) {
	repo := &insertOnlyRepository{}
	service := NewService(repo)
	input := parkedInput("orders", 1)
	input.EvidenceID = "ev_fixed"
	decision, err := service.RecordDecision(context.Background(), input)
	if err != nil || repo.inserts != 1 || decision.EvidenceID != "ev_fixed" {
		t.Fatalf("decision=%+v inserts=%d err=%v", decision, repo.inserts, err)
	}
}

type insertOnlyRepository struct{ inserts int }

func (r *insertOnlyRepository) InsertDecision(context.Context, DecisionInput) (int64, error) {
	r.inserts++
	return int64(r.inserts), nil
}

func (*insertOnlyRepository) FindAuditViolations(context.Context) ([]AuditViolation, error) {
	return nil, nil
}

// Without its unique index (dropped, or INVALID after a failed build)
// PostgreSQL rejects ON CONFLICT with 42P10; the repository then inserts.
func TestMissingConflictIndexIsRecognized(t *testing.T) {
	missing := &pgconn.PgError{Code: "42P10"}
	if !missingConflictIndex(fmt.Errorf("upsert: %w", missing)) {
		t.Fatal("42P10 was not recognized as a missing conflict index")
	}
	for _, err := range []error{nil, errors.New("connection refused"),
		&pgconn.PgError{Code: "23505"}} {
		if missingConflictIndex(err) {
			t.Fatalf("%v recognized as a missing conflict index", err)
		}
	}
}
