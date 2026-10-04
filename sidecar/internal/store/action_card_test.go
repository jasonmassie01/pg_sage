package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Approval cards: an approval through a card approves exactly the SQL the
// card showed, and an operator may snooze a pending item with a reason.

func queueForCard(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	sql string) int {
	t.Helper()
	var findingID int
	if err := pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, status)
		VALUES ('card_store', 'warning', 'table', $1, 'card', '{}', 'open') RETURNING id`,
		fmt.Sprintf("public.card_%d", time.Now().UnixNano())).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	id, err := NewActionStore(pool).Propose(ctx, nil, findingID, sql, "", "safe")
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestApproveExpectingMatchingSQL(t *testing.T) {
	pool, ctx := recStorePool(t)
	s := NewActionStore(pool)
	id := queueForCard(t, ctx, pool, "ANALYZE public.card_a")
	got, err := s.ApproveExpecting(ctx, id, 3, "ANALYZE public.card_a")
	if err != nil || got.Status != "approved" || got.DecidedBy == nil || *got.DecidedBy != 3 {
		t.Fatalf("approve = %+v, %v", got, err)
	}
}

func TestApproveExpectingRefusesChangedSQL(t *testing.T) {
	pool, ctx := recStorePool(t)
	s := NewActionStore(pool)
	id := queueForCard(t, ctx, pool, "ANALYZE public.card_b")
	if _, err := pool.Exec(ctx, `UPDATE sage.action_queue SET proposed_sql =
		'ANALYZE public.card_changed' WHERE id = $1`, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApproveExpecting(ctx, id, 3, "ANALYZE public.card_b"); err == nil {
		t.Fatal("approved SQL the card never showed")
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM sage.action_queue WHERE id = $1`, id).
		Scan(&status)
	if status != "pending" {
		t.Fatalf("status after refused approval = %q", status)
	}
	// Empty expectation is refused, not treated as "anything".
	if _, err := s.ApproveExpecting(ctx, id, 3, ""); err == nil {
		t.Fatal("approved with no expected SQL")
	}
}

func TestSnoozeStoresWhoWhyUntil(t *testing.T) {
	pool, ctx := recStorePool(t)
	s := NewActionStore(pool)
	id := queueForCard(t, ctx, pool, "ANALYZE public.card_c")
	until := time.Now().Add(4 * time.Hour).UTC().Truncate(time.Second)
	if err := s.Snooze(ctx, id, 9, until, "busy hours"); err != nil {
		t.Fatalf("snooze: %v", err)
	}
	var got time.Time
	var by int
	var reason, status string
	if err := pool.QueryRow(ctx, `SELECT snoozed_until, snoozed_by, snooze_reason, status
		FROM sage.action_queue WHERE id = $1`, id).Scan(&got, &by, &reason,
		&status); err != nil {
		t.Fatal(err)
	}
	if !got.Equal(until) || by != 9 || reason != "busy hours" || status != "pending" {
		t.Fatalf("snooze row = %v %d %q %q", got, by, reason, status)
	}
}

func TestSnoozeRefusesDecidedAndInvalid(t *testing.T) {
	pool, ctx := recStorePool(t)
	s := NewActionStore(pool)
	id := queueForCard(t, ctx, pool, "ANALYZE public.card_d")
	if err := s.Reject(ctx, id, 1, "no"); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	if err := s.Snooze(ctx, id, 9, future, "later"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("snoozing a rejected item: err = %v, want ErrNotPending", err)
	}
	other := queueForCard(t, ctx, pool, "ANALYZE public.card_e")
	if err := s.Snooze(ctx, other, 9, time.Now().Add(-time.Minute), "past"); err == nil {
		t.Fatal("snoozed into the past")
	}
	if err := s.Snooze(ctx, other, 0, future, "nobody"); err == nil {
		t.Fatal("snoozed without a user")
	}
	if err := s.Snooze(ctx, 0, 9, future, "no item"); !errors.Is(err, ErrNotPending) {
		t.Fatalf("missing item: err = %v", err)
	}
}

// A snoozed item stays pending: it is still listed (the card shows it as
// snoozed) and can still be approved before the snooze ends.
func TestSnoozedItemStaysPendingAndApprovable(t *testing.T) {
	pool, ctx := recStorePool(t)
	s := NewActionStore(pool)
	id := queueForCard(t, ctx, pool, "ANALYZE public.card_f")
	if err := s.Snooze(ctx, id, 9, time.Now().Add(time.Hour), "later"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.ListPending(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, a := range pending {
		found = found || a.ID == id
	}
	if !found {
		t.Fatal("a snoozed item disappeared from the pending list")
	}
	if _, err := s.ApproveExpecting(ctx, id, 3, "ANALYZE public.card_f"); err != nil {
		t.Fatalf("approve a snoozed item: %v", err)
	}
}
