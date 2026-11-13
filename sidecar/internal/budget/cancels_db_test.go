package budget

import (
	"context"
	"testing"
	"time"
)

// market-08b on a live server: the ledger adds the log's cancels to a
// budget's day, records their source and a lower bound, and finds the
// last revert of an enforce statement in sage.action_log.

// Happy path and boundary: cancels add up on their own day's row; a day
// without a row (pg_sage did not measure it) adds to today's instead of
// inventing a measured day; databases stay apart; History reads them.
func TestStore_RecordCancelsAddsPerDay(t *testing.T) {
	pool := sagePool(t, "budgetcancels")
	ctx := context.Background()
	s, other := NewStore(pool, "shop"), NewStore(pool, "billing")
	today := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	yesterday := today.Add(-24 * time.Hour)
	for _, st := range []*Store{s, other} {
		if err := st.Record(ctx, today, []Tally{{Budget: "app", Definition: "d"}}); err != nil {
			t.Fatal(err)
		}
	}
	rows := []CancelRow{{Budget: "app", Definition: "d", Day: today, Count: 4},
		{Budget: "app", Definition: "d", Day: yesterday, Count: 2}}
	if err := s.RecordCancels(ctx, today, CancelSourceLog, rows); err != nil {
		t.Fatalf("record cancels: %v", err)
	}
	if err := s.RecordCancels(ctx, today, CancelSourceLog, []CancelRow{{Budget: "app",
		Definition: "d", Day: today, Count: 1, LowerBound: true}}); err != nil {
		t.Fatalf("record cancels again: %v", err)
	}
	h, err := s.History(ctx, "app", "d", yesterday)
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 1 {
		t.Fatalf("history = %+v, want only today's measured row", h)
	}
	if h[0].ActualCancels != 7 || h[0].CancelSource != CancelSourceLog ||
		!h[0].CancelsLowerBound {
		t.Fatalf("today = %+v, want 7 cancels from the log, a lower bound", h[0])
	}
	o, err := other.History(ctx, "app", "d", yesterday)
	if err != nil || len(o) != 1 || o[0].ActualCancels != 0 ||
		o[0].CancelSource != CancelSourceNone {
		t.Fatalf("billing = %+v (%v), want its own row untouched", o, err)
	}
}

// Nil/empty: no rows with source none still marks today's rows' source,
// so a lost log source shows on the ledger.
func TestStore_RecordCancelsSourceNone(t *testing.T) {
	pool := sagePool(t, "budgetcancelsnone")
	ctx := context.Background()
	s := NewStore(pool, "shop")
	today := time.Date(2026, 11, 12, 0, 0, 0, 0, time.UTC)
	if err := s.Record(ctx, today, []Tally{{Budget: "app", Definition: "d"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCancels(ctx, today, CancelSourceLog, []CancelRow{{Budget: "app",
		Definition: "d", Day: today}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordCancels(ctx, today, CancelSourceNone, nil); err != nil {
		t.Fatalf("record source none: %v", err)
	}
	h, err := s.History(ctx, "app", "d", today)
	if err != nil || len(h) != 1 || h[0].CancelSource != CancelSourceNone {
		t.Fatalf("history = %+v (%v), want source none", h, err)
	}
}

// LastRevert is the latest rollback of the exact enforce statement, by
// when it was recorded; other statements and kept actions do not count.
func TestStore_LastRevert(t *testing.T) {
	pool := sagePool(t, "budgetrevert")
	ctx := context.Background()
	s := NewStore(pool, "shop")
	sql := RoleSettingSQL("app", "shop", SettingStatementTimeout, 6000)
	none, err := s.LastRevert(ctx, sql)
	if err != nil || !none.IsZero() {
		t.Fatalf("no actions: %v / %v, want zero time", none, err)
	}
	insert := `INSERT INTO sage.action_log (action_type, sql_executed, outcome, executed_at,
		measured_at) VALUES ('set_role_timeout', $1, $2, now() - interval '3 hours', $3)`
	at := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	for _, row := range []struct {
		sql, outcome string
		measured     *time.Time
	}{{sql, "success", nil}, {sql, "rolled_back", &at},
		{RoleSettingSQL("app", "shop", SettingStatementTimeout, 7000), "rolled_back", nil}} {
		if _, err := pool.Exec(ctx, insert, row.sql, row.outcome, row.measured); err != nil {
			t.Fatalf("insert action: %v", err)
		}
	}
	got, err := s.LastRevert(ctx, sql)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(at) {
		t.Fatalf("last revert = %v, want %v", got, at)
	}
}
