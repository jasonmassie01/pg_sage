package facts

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/testdb"
)

// facts-08: the absence of a fact's subject is persisted (missing_since),
// so a restart does not reset it; a confirmed operator or model fact keeps
// binding through a week of absence (a table rebuilt and swapped in, a
// slot recreated after a connector reset), and its confirmation comes back
// with the subject instead of reopening as a proposal.

// createSlot creates a physical replication slot (no logical decoding
// needed) and drops it when the test ends.
func createSlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	execAll(t, ctx, pool, "SELECT pg_create_physical_replication_slot('"+name+"')")
	t.Cleanup(func() { dropSlotIfAny(pool, name) })
}

func dropSlot(t *testing.T, ctx context.Context, pool *pgxpool.Pool, name string) {
	t.Helper()
	execAll(t, ctx, pool, "SELECT pg_drop_replication_slot('"+name+"')")
}

func dropSlotIfAny(pool *pgxpool.Pool, name string) {
	_, _ = pool.Exec(context.Background(), `SELECT pg_drop_replication_slot(slot_name)
		FROM pg_replication_slots WHERE slot_name = $1`, name)
}

func declareSlot(t *testing.T, ctx context.Context, s *Store, slot string) Fact {
	t.Helper()
	p := operatorProposal(TypeSlotConsumer, KindSlot, slot)
	p.Value = map[string]string{"consumer": "debezium"}
	f, err := s.Declare(ctx, p, "alice@example.com", "the CDC connector")
	if err != nil {
		t.Fatalf("declare slot %s: %v", slot, err)
	}
	return f
}

func mustGet(t *testing.T, ctx context.Context, s *Store, id int64) Fact {
	t.Helper()
	f, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("get fact %d: %v", id, err)
	}
	return f
}

func mustReverify(t *testing.T, ctx context.Context, s *Store, at time.Time) []Fact {
	t.Helper()
	got, err := s.Reverify(ctx, at)
	if err != nil {
		t.Fatalf("reverify at %s: %v", at, err)
	}
	return got
}

func sameInstant(a *time.Time, b time.Time) bool {
	return a != nil && a.UTC().Truncate(time.Microsecond).Equal(b.UTC().Truncate(time.Microsecond))
}

// State transitions: present -> absent (missing_since set, still confirmed)
// -> present (cleared) -> absent past the week (expired, confirmation and
// missing_since kept) -> re-proposed (confirmed again, no new card).
func TestConfirmedSlotFactSurvivesAbsenceAndIsRestored(t *testing.T) {
	pool, ctx := livePool(t)
	slot := uniqueName("cdc_absent_")
	createSlot(t, ctx, pool, slot)
	s := NewStore(pool)
	f := declareSlot(t, ctx, s, slot)
	t0 := time.Now()
	if got := mustReverify(t, ctx, s, t0); len(got) != 0 ||
		mustGet(t, ctx, s, f.ID).MissingSince != nil {
		t.Fatalf("present slot: expired %+v or marked missing", got)
	}
	dropSlot(t, ctx, pool, slot)
	mustReverify(t, ctx, s, t0.Add(time.Minute))
	mustReverify(t, ctx, s, t0.Add(2*time.Hour))
	at2h := mustGet(t, ctx, s, f.ID)
	if at2h.Status != StatusConfirmed || !sameInstant(at2h.MissingSince, t0.Add(time.Minute)) ||
		at2h.DecidedBy != "alice@example.com" {
		t.Fatalf("2 h after the drop: %+v (want confirmed, absent since the first miss)", at2h)
	}
	createSlot(t, ctx, pool, slot)
	mustReverify(t, ctx, s, t0.Add(3*time.Hour))
	if back := mustGet(t, ctx, s, f.ID); back.Status != StatusConfirmed ||
		back.MissingSince != nil {
		t.Fatalf("recreated slot: %+v (want confirmed, missing_since cleared)", back)
	}
	dropSlot(t, ctx, pool, slot)
	gone := t0.Add(4 * time.Hour)
	mustReverify(t, ctx, s, gone)
	if got := mustReverify(t, ctx, s, gone.Add(7*24*time.Hour-time.Second)); len(got) != 0 {
		t.Fatalf("expired inside the week: %+v", got)
	}
	expired := mustReverify(t, ctx, s, gone.Add(7*24*time.Hour))
	if len(expired) != 1 || expired[0].ID != f.ID || !expired[0].ConfirmationLost() ||
		!sameInstant(expired[0].MissingSince, gone) ||
		expired[0].DecidedBy != "alice@example.com" {
		t.Fatalf("after the week: %+v", expired)
	}
	if r := readRegistry(t, ctx, pool, slot); !r.registered {
		t.Fatalf("an absence expiry unregistered the slot: %+v", r)
	}
	requireRestored(t, ctx, s, slot, f)
}

// requireRestored re-proposes slot's fact and checks the confirmation came
// back as it was, binding at once.
func requireRestored(t *testing.T, ctx context.Context, s *Store, slot string, f Fact) {
	t.Helper()
	if _, err := s.Confirmed(ctx); err != nil { // warm the cache
		t.Fatal(err)
	}
	restored, created, err := s.Propose(ctx, slotFactProposal(slot, "debezium"))
	if err != nil || created {
		t.Fatalf("re-proposal: created=%v err=%v (a restore sends no card)", created, err)
	}
	if restored.Status != StatusConfirmed || restored.DecidedBy != "alice@example.com" ||
		restored.DecidedAt == nil || !restored.DecidedAt.Equal(*f.DecidedAt) ||
		restored.MissingSince != nil || restored.ExpiredReason != "" ||
		!strings.Contains(restored.DecisionNote, "the CDC connector") ||
		!strings.Contains(restored.DecisionNote,
			"subject reappeared; confirmation by alice@example.com kept") ||
		restored.Source != SourceOperator {
		t.Fatalf("restored fact: %+v", restored)
	}
	confirmed, err := s.Confirmed(ctx)
	if err != nil || len(confirmed) != 1 || confirmed[0].ID != f.ID {
		t.Fatalf("the restored fact does not bind at once: %+v %v", confirmed, err)
	}
}

// Boundary: the absence expires at exactly the grace and not a second
// before; a confirmed operator fact gets 7 days by default, a detector's
// proposal (and a confirmed detector fact) 1 hour.
func TestAbsenceGraceBoundaryPerSource(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	operator := declareSlot(t, ctx, s, uniqueName("op_gone_"))
	proposed, _, err := s.Propose(ctx, slotFactProposal(uniqueName("det_gone_"), "x"))
	if err != nil {
		t.Fatal(err)
	}
	detConfirmed := confirmedSlot(t, ctx, s, uniqueName("detc_gone_"))
	t0 := time.Now()
	mustReverify(t, ctx, s, t0)
	if got := mustReverify(t, ctx, s, t0.Add(time.Hour-time.Second)); len(got) != 0 {
		t.Fatalf("expired a second before the hour: %+v", got)
	}
	atHour := mustReverify(t, ctx, s, t0.Add(time.Hour))
	ids := map[int64]bool{}
	for _, e := range atHour {
		ids[e.ID] = true
	}
	if len(atHour) != 2 || !ids[proposed.ID] || !ids[detConfirmed.ID] {
		t.Fatalf("at the hour: %+v (want the detector facts only)", atHour)
	}
	if mustGet(t, ctx, s, operator.ID).Status != StatusConfirmed {
		t.Fatal("the operator fact expired at the detector's hour")
	}
	week := t0.Add(7 * 24 * time.Hour)
	if got := mustReverify(t, ctx, s, week.Add(-time.Second)); len(got) != 0 {
		t.Fatalf("expired a second before the week: %+v", got)
	}
	if got := mustReverify(t, ctx, s, week); len(got) != 1 || got[0].ID != operator.ID {
		t.Fatalf("at the week: %+v", got)
	}
}

// expireByAbsence declares slot's fact and expires it by absence (s has
// no absence grace, so the first pass that finds it absent expires it).
func expireByAbsence(t *testing.T, ctx context.Context, s *Store, slot string) Fact {
	t.Helper()
	f := declareSlot(t, ctx, s, slot)
	if got := mustReverify(t, ctx, s, time.Now()); len(got) != 1 || got[0].ID != f.ID ||
		got[0].MissingSince == nil {
		t.Fatalf("%s did not expire by absence: %+v", slot, got)
	}
	return f
}

// requireReopened proposes p and checks it reopened as a proposal.
func requireReopened(t *testing.T, ctx context.Context, s *Store, name string, p Proposal) {
	t.Helper()
	f, created, err := s.Propose(ctx, p)
	if err != nil || !created || f.Status != StatusProposed || f.DecidedBy != "" ||
		f.MissingSince != nil {
		t.Fatalf("%s: %+v created=%v err=%v (want reopened as proposed)", name, f,
			created, err)
	}
}

// Must not change: a different value reopens an expired confirmed fact as
// a proposal for a person to decide; so does a re-proposal after the
// restore window (boundary: 8 days against the default 7).
func TestReproposalReopensOnAnotherValueOrAfterTheWindow(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool).WithConfirmedAbsence(0)
	// Re-proposed through a store with the default (7 day) restore window.
	reopened := func(name string, p Proposal) {
		requireReopened(t, ctx, NewStore(pool), name, p)
	}
	other := uniqueName("val_")
	expireByAbsence(t, ctx, s, other)
	reopened("different value", slotFactProposal(other, "fivetran"))

	late := uniqueName("late_")
	lf := expireByAbsence(t, ctx, s, late)
	if _, err := pool.Exec(ctx, `UPDATE sage.facts SET updated_at = now() - interval '8 days'
		WHERE id = $1`, lf.ID); err != nil {
		t.Fatalf("age the expiry: %v", err)
	}
	reopened("past the restore window", slotFactProposal(late, "debezium"))
}

// An expiry by date or by a person's choice clears missing_since: the fact
// reopens as a proposal, never restores.
func TestReproposalReopensAfterAnExpiryByHandOrDate(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool) // a week of absence grace: the fact stays live while absent
	// Re-proposed through a store with the default (7 day) restore window.
	reopened := func(name string, p Proposal) {
		requireReopened(t, ctx, NewStore(pool), name, p)
	}
	byHand := uniqueName("hand_")
	hf := declareSlot(t, ctx, s, byHand)
	mustReverify(t, ctx, s, time.Now())
	if mustGet(t, ctx, s, hf.ID).MissingSince == nil {
		t.Fatal("the absent slot was not marked missing")
	}
	if e, err := s.Expire(ctx, hf.ID, "ops", "decommissioned"); err != nil ||
		e.MissingSince != nil {
		t.Fatalf("expire by hand: %+v %v (want missing_since cleared)", e, err)
	}
	reopened("expired by a person", slotFactProposal(byHand, "debezium"))

	dated := uniqueName("dated_")
	p := operatorProposal(TypeSlotConsumer, KindSlot, dated)
	p.Value = map[string]string{"consumer": "debezium"}
	soon := time.Now().Add(time.Minute)
	p.ExpiresAt = &soon
	df, err := s.Declare(ctx, p, "alice@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	mustReverify(t, ctx, s, time.Now())
	if mustGet(t, ctx, s, df.ID).MissingSince == nil {
		t.Fatal("the absent dated slot was not marked missing")
	}
	got := mustReverify(t, ctx, s, time.Now().Add(2*time.Minute))
	if len(got) != 1 || got[0].MissingSince != nil ||
		!strings.Contains(got[0].ExpiredReason, "expiry") {
		t.Fatalf("expiry date: %+v (want missing_since cleared)", got)
	}
	again := slotFactProposal(dated, "debezium")
	later := time.Now().Add(time.Hour)
	again.ExpiresAt = &later
	reopened("expired by date", again)
}

// A rejected fact never reopens, absent or not.
func TestRejectedFactIsNeverRestored(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool).WithConfirmedAbsence(0)
	slot := uniqueName("rej_")
	f, _, err := s.Propose(ctx, slotFactProposal(slot, "debezium"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, f.ID, Decision{Actor: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	mustReverify(t, ctx, s, time.Now().Add(48*time.Hour))
	again, created, err := s.Propose(ctx, slotFactProposal(slot, "debezium"))
	if err != nil || created || again.Status != StatusRejected || again.MissingSince != nil {
		t.Fatalf("rejected fact after re-proposal: %+v created=%v err=%v", again, created, err)
	}
}

// Restart: a new Store on a new pool (fresh process state) reads the
// persisted missing_since, so the grace is not reset.
func TestAbsenceSurvivesARestart(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f, _, err := s.Propose(ctx, slotFactProposal(uniqueName("restart_"), "x"))
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	mustReverify(t, ctx, s, t0)
	second, err := pgxpool.New(ctx, livePoolDSN(t))
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	defer second.Close()
	restarted := NewStore(second)
	var stored *time.Time
	if err := second.QueryRow(ctx, `SELECT missing_since FROM sage.facts WHERE id = $1`,
		f.ID).Scan(&stored); err != nil || !sameInstant(stored, t0) {
		t.Fatalf("missing_since after restart: %v %v, want %s", stored, err, t0)
	}
	got := mustReverify(t, ctx, restarted, t0.Add(time.Hour))
	if len(got) != 1 || got[0].ID != f.ID {
		t.Fatalf("restarted store reset the grace: expired %+v", got)
	}
}

// livePoolDSN is the fixture database's DSN, for a second pool.
func livePoolDSN(t *testing.T) string { return testdb.SkipUnlessLive(t) }
