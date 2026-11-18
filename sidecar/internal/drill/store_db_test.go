package drill

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
)

// drill-01a integration: the result store and the drill findings on the
// control database. No concurrent access tests: each statement is one
// INSERT or UPDATE, which PostgreSQL serializes.

func TestStoreDB_AFailAfterAPassKeepsThePass(t *testing.T) {
	pool := controlPool(t)
	s := NewStore(pool)
	ctx := context.Background()
	if last, err := s.LastPassed(ctx, "orders"); err != nil || !last.IsZero() {
		t.Fatalf("never drilled = %v %v, want zero", last, err)
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	pass := Result{Database: "orders", Provider: "dle", Kind: clone.KindThinClone,
		RequestedAt: base, FinishedAt: base.Add(time.Minute), Passed: true}
	if _, err := s.Record(ctx, pass); err != nil {
		t.Fatal(err)
	}
	fail := pass
	fail.Passed, fail.Failure = false, "row_count app.t"
	fail.RequestedAt, fail.FinishedAt = base.Add(10*time.Minute), base.Add(11*time.Minute)
	if _, err := s.Record(ctx, fail); err != nil {
		t.Fatal(err)
	}
	last, err := s.LastPassed(ctx, "orders")
	if err != nil || !last.Equal(pass.FinishedAt) {
		t.Fatalf("LastPassed = %v %v, want the earlier pass %v", last, err, pass.FinishedAt)
	}
	attempt, ok, err := s.LastAttempt(ctx, "orders")
	if err != nil || !ok || attempt.Passed || !attempt.RequestedAt.Equal(fail.RequestedAt) {
		t.Fatalf("LastAttempt = %+v %v %v, want the failed attempt", attempt, ok, err)
	}
	if last, _ := s.LastPassed(ctx, "other"); !last.IsZero() {
		t.Fatal("another database's pass leaked")
	}
	if _, err := NewStore(nil).LastPassed(ctx, "orders"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store = %v, want ErrNoStore", err)
	}
}

func TestReportDB_PassResolvesTheFailureAndUnknownOpensInfo(t *testing.T) {
	pool := controlPool(t)
	ctx := context.Background()
	failed := Result{Database: "orders", Provider: "dle", Failure: "row_count app.t"}
	if err := ReportResult(ctx, pool, nil, failed); err != nil {
		t.Fatal(err)
	}
	if err := ReportResult(ctx, pool, nil, Result{Database: "orders", Passed: true}); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM sage.findings
		WHERE category = 'restore_drill_failed'`).Scan(&status); err != nil ||
		status != "resolved" {
		t.Fatalf("after a pass the finding is %q (%v), want resolved", status, err)
	}
	if err := ReportUnknown(ctx, pool, nil, "orders", "clone.provider is none"); err != nil {
		t.Fatal(err)
	}
	var severity, rec string
	if err := pool.QueryRow(ctx, `SELECT severity, recommendation FROM sage.findings
		WHERE category = 'recoverability_unknown' AND status = 'open'`).Scan(&severity,
		&rec); err != nil {
		t.Fatal(err)
	}
	if severity != "info" || !strings.Contains(rec, "clone.provider") {
		t.Fatalf("unknown finding = %s %q, want info naming clone.provider", severity, rec)
	}
}
