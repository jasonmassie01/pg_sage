package receipt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/changereview"
	"github.com/pg-sage/sidecar/internal/testsupport/rvfixture"
)

// The receipt fixes on real PostgreSQL: a change applied before the first
// cycle lands (bracketed from the review's own unmet check), the awaiting
// queue rotates past MaxAwaitingPerCycle, and observation reads and a
// database's cycle are bounded.

// A review whose post-conditions were unmet at completion and hold at the
// first cycle landed in between: never not applicable.
func TestChangeAppliedBeforeTheFirstCycleLands(t *testing.T) {
	rv := rvfixture.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newFixtureOn(t, ctx, rv.Pool, rv.Name)
	r := f.seed(func(r *changereview.Review) {
		statusIndexReview(r, "rv_orders_status_idx")
		r.Expectations.AtReview = changereview.AtReviewUnmet
		concurred(r)
	})
	completed := r.CompletedAt.UTC()
	if checked := f.checkedAt(r.ID); checked == nil || !checked.Equal(completed) {
		t.Fatalf("landing_checked_at %v, want the completion %v", checked, completed)
	}
	rv.Exec("CREATE INDEX CONCURRENTLY rv_orders_status_idx ON rv.rv_orders (status)")
	rep := f.run()
	got := f.get(r.ID)
	if rep.Landed != 1 || rep.NotApplicable != 0 || got.LandingState !=
		changereview.LandingLanded || !got.LandedLower.Equal(completed) {
		t.Fatalf("landing %s lower %v, report %+v; want landed from the completion %v",
			got.LandingState, got.LandedLower, rep, completed)
	}
	if got.CreditState != changereview.CreditPending {
		t.Fatalf("credit %s, want still pending until the receipt", got.CreditState)
	}
}

// A review never checked (its landing at review was not recorded) that
// the first cycle finds landed is bracketed from its creation: a reviewed
// change is presumed to land after its review, not before.
func TestUncheckedReviewFoundLandedIsBracketedFromCreation(t *testing.T) {
	rv := rvfixture.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	f := newFixtureOn(t, ctx, rv.Pool, rv.Name)
	r := f.seed(func(r *changereview.Review) {
		statusIndexReview(r, "rv_orders_created_idx")
		concurred(r)
	})
	rep := f.run()
	got := f.get(r.ID)
	if rep.Landed != 1 || got.LandingState != changereview.LandingLanded ||
		!got.LandedLower.Equal(r.CreatedAt) {
		t.Fatalf("landing %s lower %v, report %+v; want landed from creation %v",
			got.LandingState, got.LandedLower, rep, r.CreatedAt)
	}
}

// More awaiting reviews than one cycle evaluates: the next cycle takes the
// ones not yet checked, so every review is checked within two cycles.
func TestAwaitingQueueRotates(t *testing.T) {
	f := newFixture(t)
	const n = MaxAwaitingPerCycle + 5
	ids := make([]string, 0, n)
	for i := 0; i < n; i++ {
		r := f.seed(func(r *changereview.Review) {
			r.Expectations.PostConditions = []changereview.PostCondition{{StatementIndex: 1,
				Kind: changereview.PostIndexValid, Relation: "sage.change_review",
				Index: "never_built_" + randomHex(8)}}
			r.CompletedAt = ptr(r.CompletedAt.Add(time.Duration(i) * time.Second))
		})
		ids = append(ids, r.ID)
	}
	if rep := f.run(); rep.Evaluated != MaxAwaitingPerCycle {
		t.Fatalf("first cycle %+v", rep)
	}
	f.clock.advance(time.Minute)
	if rep := f.run(); rep.Evaluated != MaxAwaitingPerCycle {
		t.Fatalf("second cycle %+v", rep)
	}
	for _, id := range ids {
		if f.checkedAt(id) == nil {
			t.Fatalf("review %s was never checked in two cycles", id)
		}
	}
}

// lockQueryStore holds ACCESS EXCLUSIVE on sage.query_store from its own
// connection until the returned release runs (or after hold).
func lockQueryStore(t *testing.T, f *fixture, hold time.Duration) func() {
	t.Helper()
	conn, err := pgx.Connect(f.ctx, f.pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	tx, err := conn.Begin(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(f.ctx, "LOCK TABLE sage.query_store IN ACCESS EXCLUSIVE MODE");
		err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	release := func() {
		select {
		case <-done:
			return
		default:
			close(done)
		}
		_ = tx.Rollback(context.Background())
		_ = conn.Close(context.Background())
	}
	time.AfterFunc(hold, release)
	t.Cleanup(release)
	return release
}

// A watched-statement read that cannot finish is cut off at ReadTimeout:
// the receipt waits for the next cycle instead of holding the leader.
func TestObservationReadsAreBounded(t *testing.T) {
	f := newFixture(t)
	r, at := f.landedReview(changereview.VerdictSafe, 801)
	f.beforeAfter(801, at, at, 10, 5, 5)
	release := lockQueryStore(t, f, 20*time.Second)
	defer release()
	obs := Observer{Pool: f.pool, ReadTimeout: 300 * time.Millisecond}
	w, err := WindowsOf(r, 24)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = obs.Observe(f.ctx, r, w)
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("Observe took %s against a locked query_store", took)
	}
	if !errors.Is(err, changereview.ErrUnavailable) {
		t.Fatalf("Observe = %v, want ErrUnavailable", err)
	}
}

// One database's cycle stops at its budget; what is left waits.
func TestDatabaseCycleStopsAtItsBudget(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 5; i++ {
		_, at := f.landedReview(changereview.VerdictSafe, int64(900+i))
		f.beforeAfter(int64(900+i), at, at, 10, 5, 5)
	}
	release := lockQueryStore(t, f, 20*time.Second)
	defer release()
	job := NewJob(Deps{Now: f.clock.Now, Logf: f.t.Logf, ReadTimeout: 400 * time.Millisecond,
		DatabaseBudget: time.Second})
	start := time.Now()
	rep, err := job.RunDatabase(f.ctx, f.db())
	if took := time.Since(start); took > 6*time.Second {
		t.Fatalf("the cycle took %s with a 1 s budget", took)
	}
	if err != nil || rep.Decided != 0 {
		t.Fatalf("report %+v, %v; want nothing decided and no database error", rep, err)
	}
	budget := false
	for _, e := range rep.Errors {
		budget = budget || strings.Contains(e, "budget")
	}
	if !budget {
		t.Fatalf("errors %v, want one saying the cycle budget was reached", rep.Errors)
	}
}
