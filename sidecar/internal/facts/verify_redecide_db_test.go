package facts

import (
	"testing"
	"time"
)

// facts-08 review: a person's new decision about a fact starts its
// absence afresh. A stale missing_since left by an expiry by absence (or by
// an absence before a rejection) must not expire the re-declared or
// re-confirmed fact on the very next pass.

// Re-declaring a fact that expired by absence, while its subject is still
// absent, gives the declaration its full grace.
func TestRedeclaredFactStartsItsAbsenceAfresh(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	slot := uniqueName("redeclare_")
	f := declareSlot(t, ctx, s, slot)
	t0 := time.Now()
	mustReverify(t, ctx, s, t0)
	week := t0.Add(7 * 24 * time.Hour)
	if got := mustReverify(t, ctx, s, week); len(got) != 1 || got[0].ID != f.ID {
		t.Fatalf("setup: the absent fact did not expire at the week: %+v", got)
	}
	again := declareSlot(t, ctx, s, slot)
	if again.ID != f.ID || again.Status != StatusConfirmed || again.MissingSince != nil {
		t.Fatalf("re-declared: %+v (want the same fact, confirmed, no absence)", again)
	}
	next := week.Add(time.Minute)
	if got := mustReverify(t, ctx, s, next); len(got) != 0 {
		t.Fatalf("the re-declared fact expired on the next pass: %+v", got)
	}
	if cur := mustGet(t, ctx, s, f.ID); cur.Status != StatusConfirmed ||
		!sameInstant(cur.MissingSince, next) {
		t.Fatalf("after the next pass: %+v (want confirmed, absent since %s)", cur, next)
	}
}

// Rejecting an absent confirmed fact and confirming it again later does not
// carry the old absence into the new confirmation.
func TestReconfirmedFactStartsItsAbsenceAfresh(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f := declareSlot(t, ctx, s, uniqueName("reconfirm_"))
	t0 := time.Now()
	mustReverify(t, ctx, s, t0)
	if mustGet(t, ctx, s, f.ID).MissingSince == nil {
		t.Fatal("setup: the absent slot was not marked missing")
	}
	rejected, err := s.Decide(ctx, f.ID, Decision{Actor: "alice@example.com"})
	if err != nil || rejected.Status != StatusRejected || rejected.MissingSince != nil {
		t.Fatalf("reject: %+v %v (want rejected, no absence)", rejected, err)
	}
	c, err := s.Decide(ctx, f.ID, Decision{Confirm: true, Actor: "alice@example.com"})
	if err != nil || c.Status != StatusConfirmed || c.MissingSince != nil {
		t.Fatalf("confirm again: %+v %v (want confirmed, no absence)", c, err)
	}
	later := t0.Add(8 * 24 * time.Hour)
	if got := mustReverify(t, ctx, s, later); len(got) != 0 {
		t.Fatalf("the re-confirmed fact expired on the next pass: %+v", got)
	}
	if cur := mustGet(t, ctx, s, f.ID); !sameInstant(cur.MissingSince, later) {
		t.Fatalf("after the next pass: missing_since %v, want %s", cur.MissingSince, later)
	}
}
