package drill

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/clone"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// drill-01d integration: reading a database's drill history and its open
// recoverability_unknown reason back from the control database. No
// concurrent access tests: each read is one SELECT; the API test runs
// concurrent requests over these reads.

func recordAll(t *testing.T, s *Store, rows ...Result) {
	t.Helper()
	for _, r := range rows {
		if _, err := s.Record(context.Background(), r); err != nil {
			t.Fatalf("record %+v: %v", r, err)
		}
	}
}

func TestHistoryDB_NewestFirstWithEveryFieldRoundTripped(t *testing.T) {
	s := NewStore(controlPool(t))
	ctx := context.Background()
	base := time.Now().UTC().Add(-3 * time.Hour).Truncate(time.Millisecond)
	ready := base.Add(9 * time.Minute)
	point := base.Add(-4 * time.Minute)
	pass := Result{Database: "orders", DatabaseID: 7, Provider: "rds_restore",
		Kind: clone.KindFullCopy, ReceiptID: 0, RequestedAt: base, ReadyAt: &ready,
		FinishedAt: base.Add(11 * time.Minute), RecoveryPoint: point, RTOSeconds: rto(660),
		RPOSeconds: rto(240), Passed: true, CostLabel: "about $1.20 (estimate)",
		Invariants: []Invariant{{Name: "catalog", Table: "app.t", Status: StatusPass,
			Evidence: "same columns"}}}
	noMarker := Result{Database: "orders", Provider: "rds_restore", Kind: clone.KindFullCopy,
		RequestedAt: base.Add(time.Hour), FinishedAt: base.Add(70 * time.Minute),
		Passed: true, Invariants: []Invariant{{Name: "marker", Status: StatusUnknown,
			Evidence: "no sage.snapshots and no drill.marker_query"}}}
	fail := Result{Database: "orders", Provider: "rds_restore", Kind: clone.KindFullCopy,
		RequestedAt: base.Add(2 * time.Hour), FinishedAt: base.Add(130 * time.Minute),
		Failure: "row_count app.t: 90 rows, source 100"}
	other := Result{Database: "billing", Provider: "dle", Kind: clone.KindThinClone,
		RequestedAt: base.Add(3 * time.Hour), FinishedAt: base.Add(181 * time.Minute)}
	recordAll(t, s, pass, noMarker, fail, other)

	got, err := s.History(ctx, "orders", 2)
	if err != nil || len(got) != 2 {
		t.Fatalf("History(orders, 2) = %d rows %v, want 2", len(got), err)
	}
	if got[0].Failure != fail.Failure || got[0].Passed ||
		!got[1].RequestedAt.Equal(noMarker.RequestedAt) {
		t.Fatalf("order = %+v / %+v, want the failure then the unmarked pass", got[0], got[1])
	}
	if got[1].RPOSeconds != nil || got[1].RPOReason != noMarker.Invariants[0].Evidence {
		t.Fatalf("RPO unknown row = %v %q, want nil and the marker's evidence",
			got[1].RPOSeconds, got[1].RPOReason)
	}
	all, err := s.History(ctx, "orders", MaxHistoryLimit)
	if err != nil || len(all) != 3 {
		t.Fatalf("History(orders, max) = %d %v, want 3 (billing's row stays out)", len(all), err)
	}
	assertRoundTrip(t, all[2], pass)
}

func assertRoundTrip(t *testing.T, got, want Result) {
	t.Helper()
	if got.ID == 0 || got.DatabaseID != 7 || got.Provider != want.Provider ||
		got.Kind != want.Kind || !got.RequestedAt.Equal(want.RequestedAt) ||
		got.ReadyAt == nil || !got.ReadyAt.Equal(*want.ReadyAt) ||
		!got.FinishedAt.Equal(want.FinishedAt) || !got.RecoveryPoint.Equal(want.RecoveryPoint) ||
		*got.RTOSeconds != 660 || *got.RPOSeconds != 240 || !got.Passed ||
		got.CostLabel != want.CostLabel || got.RPOReason != "" || got.Failure != "" {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
	if len(got.Invariants) != 1 || got.Invariants[0] != want.Invariants[0] {
		t.Fatalf("invariants = %+v, want %+v", got.Invariants, want.Invariants)
	}
	if got.RequestedAt.Location() != time.UTC {
		t.Fatalf("times are %v, want UTC", got.RequestedAt.Location())
	}
}

func TestHistoryDB_EmptyLimitAndNilStore(t *testing.T) {
	s := NewStore(controlPool(t))
	ctx := context.Background()
	got, err := s.History(ctx, "never", 20)
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("never drilled = %v %v, want an empty, non-nil list", got, err)
	}
	for _, n := range []int{0, MaxHistoryLimit + 1} {
		if _, err := s.History(ctx, "orders", n); err == nil {
			t.Errorf("History limit %d = nil error, want a limit error", n)
		}
	}
	if _, err := NewStore(nil).History(ctx, "orders", 20); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store History = %v, want ErrNoStore", err)
	}
	if _, err := NewStore(nil).LastPassedResult(ctx, "orders"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store LastPassedResult = %v, want ErrNoStore", err)
	}
}

func TestLastPassedResultDB_AFailAfterAPassReturnsThePass(t *testing.T) {
	s := NewStore(controlPool(t))
	ctx := context.Background()
	if got, err := s.LastPassedResult(ctx, "orders"); err != nil || got != nil {
		t.Fatalf("never passed = %+v %v, want nil", got, err)
	}
	base := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	older := Result{Database: "orders", Provider: "dle", Kind: clone.KindThinClone,
		RequestedAt: base.Add(-time.Hour), FinishedAt: base.Add(-50 * time.Minute),
		Passed: true, RTOSeconds: rto(600)}
	newer := older
	newer.RequestedAt, newer.FinishedAt, newer.RTOSeconds = base, base.Add(time.Minute), rto(60)
	fail := older
	fail.Passed, fail.Failure = false, "readable app.t"
	fail.RequestedAt, fail.FinishedAt = base.Add(10*time.Minute), base.Add(11*time.Minute)
	recordAll(t, s, older, newer, fail)
	got, err := s.LastPassedResult(ctx, "orders")
	if err != nil || got == nil || !got.FinishedAt.Equal(newer.FinishedAt) ||
		*got.RTOSeconds != 60 || !got.Passed {
		t.Fatalf("LastPassedResult = %+v %v, want the newer pass", got, err)
	}
}

func TestUnknownReasonDB_ReadsOnlyThisDatabasesOpenFinding(t *testing.T) {
	pool := controlPool(t)
	ctx := context.Background()
	if got, err := UnknownReason(ctx, pool, "orders"); err != nil || got != "" {
		t.Fatalf("no finding = %q %v, want empty", got, err)
	}
	if err := ReportUnknown(ctx, pool, nil, "orders", "clone.provider is none"); err != nil {
		t.Fatal(err)
	}
	if got, err := UnknownReason(ctx, pool, "orders"); err != nil ||
		got != "clone.provider is none" {
		t.Fatalf("open finding = %q %v, want its reason", got, err)
	}
	if got, _ := UnknownReason(ctx, pool, "billing"); got != "" {
		t.Fatalf("another database's finding leaked: %q", got)
	}
	mustExec(t, pool, `UPDATE sage.findings SET status = 'resolved'
		WHERE category = 'recoverability_unknown'`)
	if got, _ := UnknownReason(ctx, pool, "orders"); got != "" {
		t.Fatalf("resolved finding = %q, want empty", got)
	}
	if _, err := UnknownReason(ctx, nil, "orders"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil pool = %v, want ErrNoStore", err)
	}
}

func TestHistoryDB_ClosedPoolNamesTheDatabase(t *testing.T) {
	pool := openPool(t, testdb.SkipUnlessLive(t))
	pool.Close()
	_, err := NewStore(pool).History(context.Background(), "orders", 20)
	if err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("closed pool = %v, want an error naming orders", err)
	}
}
