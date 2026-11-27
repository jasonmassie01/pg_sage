package facts

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// facts-07: every status or decision change of a fact appends one row to
// the chained, append-only sage.fact_events, written by a trigger on
// sage.facts so every writer (REST, chat cards, MCP, the verifier, column
// classes) is covered inside its own transaction.

type factEvent struct {
	Actor, Verb, From, To, Hash, Note string
}

func factEvents(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	factID int64) []factEvent {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT actor, verb, from_status, to_status, fact_hash,
		note FROM sage.fact_events WHERE fact_id = $1 ORDER BY id`, factID)
	if err != nil {
		t.Fatalf("read events of fact %d: %v", factID, err)
	}
	defer rows.Close()
	var out []factEvent
	for rows.Next() {
		var e factEvent
		if err := rows.Scan(&e.Actor, &e.Verb, &e.From, &e.To, &e.Hash, &e.Note); err != nil {
			t.Fatalf("scan event: %v", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read events: %v", err)
	}
	return out
}

func wantEvent(t *testing.T, got factEvent, verb, actor, from, to string, f Fact) {
	t.Helper()
	if got.Verb != verb || got.Actor != actor || got.From != from || got.To != to {
		t.Fatalf("event = %+v, want %s by %s %q -> %q", got, verb, actor, from, to)
	}
	if got.Hash != f.Hash() {
		t.Fatalf("event %s fact_hash %q, want the fact's hash %q", verb, got.Hash, f.Hash())
	}
}

// State transitions: propose, confirm, reject (revoke) and declare give
// four events with their actors; the first confirmer stays readable after
// the reject overwrote decided_by.
func TestFactEventsRecordEveryDecision(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	c, err := s.Decide(ctx, p.ID, Decision{Confirm: true, Actor: "alice@example.com",
		Note: "the app owns it"})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Decide(ctx, p.ID, Decision{Actor: "bob@example.com", Note: "not any more"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := s.Declare(ctx, operatorProposal(TypeAppMigrations, KindIndex,
		p.Subject), "carol@example.com", "re-declared")
	if err != nil || d.ID != p.ID {
		t.Fatalf("declare over the rejection: %+v %v", d, err)
	}
	ev := factEvents(t, ctx, pool, p.ID)
	if len(ev) != 4 {
		t.Fatalf("events = %+v, want 4", ev)
	}
	wantEvent(t, ev[0], "proposed", "detector:app_managed_index", "", "proposed", p)
	wantEvent(t, ev[1], "confirmed", "alice@example.com", "proposed", "confirmed", c)
	wantEvent(t, ev[2], "rejected", "bob@example.com", "confirmed", "rejected", r)
	wantEvent(t, ev[3], "declared", "carol@example.com", "rejected", "confirmed", d)
	if ev[1].Note != "the app owns it" || ev[2].Note != "not any more" ||
		ev[3].Note != "re-declared" {
		t.Fatalf("event notes = %q %q %q", ev[1].Note, ev[2].Note, ev[3].Note)
	}
	if r.DecidedBy != "bob@example.com" || ev[1].Actor != "alice@example.com" {
		t.Fatalf("first confirmer lost: row says %q, history %q", r.DecidedBy, ev[1].Actor)
	}
}

// Declaring a fact that never existed is one event, by the declarer.
func TestFactEventsDeclareNewFact(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	d, err := s.Declare(ctx, operatorProposal(TypeTestFixture, KindSchema, "fx_*"),
		"carol@example.com", "")
	if err != nil {
		t.Fatal(err)
	}
	ev := factEvents(t, ctx, pool, d.ID)
	if len(ev) != 1 {
		t.Fatalf("events = %+v, want 1", ev)
	}
	wantEvent(t, ev[0], "declared", "carol@example.com", "", "confirmed", d)
}

// State transitions: an operator's expiry names its actor (expired_by) and
// a re-proposal that reopens the expired fact writes reopened.
func TestFactEventsExpireAndReopen(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.Expire(ctx, p.ID, "dave@example.com", "index dropped")
	if err != nil || e.ExpiredBy != "dave@example.com" || e.ExpiredReason != "index dropped" {
		t.Fatalf("expire: %+v %v", e, err)
	}
	again := validProposal()
	again.ProposedBy = "detector:second_pass"
	o, created, err := s.Propose(ctx, again)
	if err != nil || !created || o.Status != StatusProposed || o.ExpiredBy != "" {
		t.Fatalf("reopen: %+v created=%v %v", o, created, err)
	}
	ev := factEvents(t, ctx, pool, p.ID)
	if len(ev) != 3 {
		t.Fatalf("events = %+v, want 3", ev)
	}
	wantEvent(t, ev[1], "expired", "dave@example.com", "proposed", "expired", e)
	if ev[1].Note != "index dropped" {
		t.Fatalf("expiry note %q", ev[1].Note)
	}
	wantEvent(t, ev[2], "reopened", "detector:second_pass", "expired", "proposed", o)
}

// A confirmation that expired by absence and comes back with its subject
// (facts-08) is restored by pg_sage, not by a person.
func TestFactEventsRestoredConfirmation(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, p.ID, Decision{Confirm: true,
		Actor: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	execAll(t, ctx, pool, `UPDATE sage.facts SET missing_since = now() - interval '1 hour'`)
	if _, err := s.expire(ctx, p.ID, verifierActor, "absent", false, true); err != nil {
		t.Fatal(err)
	}
	back, _, err := s.Propose(ctx, validProposal())
	if err != nil || back.Status != StatusConfirmed {
		t.Fatalf("restore: %+v %v", back, err)
	}
	ev := factEvents(t, ctx, pool, p.ID)
	if len(ev) != 4 {
		t.Fatalf("events = %+v, want 4", ev)
	}
	if ev[2].Verb != "expired" || ev[2].Actor != verifierActor {
		t.Fatalf("absence expiry event %+v", ev[2])
	}
	wantEvent(t, ev[3], "restored", "pg_sage", "expired", "confirmed", back)
	if !strings.Contains(ev[3].Note, "alice@example.com") {
		t.Fatalf("restored note %q does not name the kept confirmation", ev[3].Note)
	}
}

// Nil/empty: re-proposing a confirmed fact changes only its proposal count
// and evidence, and re-verification only its verification marks; neither
// is a decision, so neither writes an event.
func TestFactEventsIgnoreNonDecisionUpdates(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Decide(ctx, p.ID, Decision{Confirm: true,
		Actor: "alice@example.com"}); err != nil {
		t.Fatal(err)
	}
	again, _, err := s.Propose(ctx, validProposal())
	if err != nil || again.Proposals != 2 || again.Status != StatusConfirmed {
		t.Fatalf("re-proposal: %+v %v", again, err)
	}
	execAll(t, ctx, pool, `UPDATE sage.facts SET last_verified_at = now(),
		missing_since = now()`, `UPDATE sage.facts SET missing_since = NULL`)
	if ev := factEvents(t, ctx, pool, p.ID); len(ev) != 2 {
		t.Fatalf("events = %+v, want only the proposal and the confirmation", ev)
	}
}

// Invalid input: an expiry names who made it.
func TestFactEventsExpireRefusesAnEmptyActor(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	for _, actor := range []string{"", "   ", strings.Repeat("a", maxProposedBy+1)} {
		if _, err := s.Expire(ctx, p.ID, actor, "gone"); !errors.Is(err, ErrInvalidValue) {
			t.Fatalf("expire with actor %q: %v, want ErrInvalidValue", actor, err)
		}
	}
	got, err := s.Get(ctx, p.ID)
	if err != nil || got.Status != StatusProposed {
		t.Fatalf("fact after refused expiries: %+v %v", got, err)
	}
	if ev := factEvents(t, ctx, pool, p.ID); len(ev) != 1 {
		t.Fatalf("events = %+v, want only the proposal", ev)
	}
}

// Error propagation: when the history refuses the event, the decision
// fails and the fact keeps its status.
func TestFactEventsRefusedInsertFailsTheDecision(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, ctx, pool, `ALTER TABLE sage.fact_events
		ADD CONSTRAINT fact_events_refuse_test CHECK (false) NOT VALID`)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `ALTER TABLE sage.fact_events
			DROP CONSTRAINT IF EXISTS fact_events_refuse_test`)
	})
	_, err = s.Decide(ctx, p.ID, Decision{Confirm: true, Actor: "alice@example.com"})
	if err == nil || !strings.Contains(err.Error(), "fact_events_refuse_test") ||
		!strings.Contains(err.Error(), "decision on fact") {
		t.Fatalf("decide with a refusing history: %v", err)
	}
	if _, err := s.Expire(ctx, p.ID, "dave@example.com", "x"); err == nil {
		t.Fatalf("expire with a refusing history succeeded")
	}
	got, err := s.Get(ctx, p.ID)
	if err != nil || got.Status != StatusProposed || got.DecidedBy != "" {
		t.Fatalf("fact after the refused decision: %+v %v", got, err)
	}
}

// Concurrent access: parallel confirmations of one fact make one decision
// and one event; the others are refused as invalid transitions.
func TestFactEventsParallelDecisionsWriteOneEvent(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	const n = 8
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = s.Decide(ctx, p.ID, Decision{Confirm: true, Actor: "alice@example.com"})
		}(i)
	}
	close(start)
	wg.Wait()
	ok := 0
	for _, err := range errs {
		switch {
		case err == nil:
			ok++
		case !errors.Is(err, ErrInvalidTransition):
			t.Fatalf("parallel decide: %v", err)
		}
	}
	ev := factEvents(t, ctx, pool, p.ID)
	if ok != 1 || len(ev) != 2 || ev[1].Verb != "confirmed" {
		t.Fatalf("%d decisions succeeded, events %+v; want 1 and proposal+confirmation", ok, ev)
	}
}

// The verifier's expiry is recorded as pg_sage's, in expired_by and in
// the history.
func TestFactEventsVerifierExpiryNamesThePgSageVerifier(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	f, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, ctx, pool, `UPDATE sage.facts SET expires_at = now() - interval '1 hour'`)
	expired, err := s.Reverify(ctx, time.Now())
	if err != nil || len(expired) != 1 || expired[0].ExpiredBy != verifierActor {
		t.Fatalf("reverify: %+v %v", expired, err)
	}
	ev := factEvents(t, ctx, pool, f.ID)
	if len(ev) != 2 {
		t.Fatalf("events = %+v, want 2", ev)
	}
	wantEvent(t, ev[1], "expired", verifierActor, "proposed", "expired", expired[0])
	if ev[1].Note != expiryDatePassed {
		t.Fatalf("verifier expiry note %q", ev[1].Note)
	}
}

// The history outlives the fact: deleting a fact (retention) keeps its
// events, which reference no row.
func TestFactEventsSurviveTheFactsDeletion(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	p, _, err := s.Propose(ctx, validProposal())
	if err != nil {
		t.Fatal(err)
	}
	execAll(t, ctx, pool, `DELETE FROM sage.facts`)
	if ev := factEvents(t, ctx, pool, p.ID); len(ev) != 1 || ev[0].Verb != "proposed" {
		t.Fatalf("events after the fact's deletion = %+v", ev)
	}
}
