package facts

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// failingCatalog answers the exists-queries from the database except for
// the subject name it fails, as a catalog read that times out would.
type failingCatalog struct {
	pool *pgxpool.Pool
	fail string
}

func (c failingCatalog) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if len(args) == 2 && args[1] == likePattern(c.fail) {
		return errRow{err: errors.New("canceling statement due to statement timeout")}
	}
	return c.pool.QueryRow(ctx, sql, args...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

func insertConfirmedRaw(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	subject string) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
		value, source, status, decided_by, decided_at) VALUES ('slot_consumer', 'slot', $1,
		'{"consumer":"x"}', 'operator', 'confirmed', 'alice@example.com', now())
		RETURNING id`, subject).Scan(&id); err != nil {
		t.Fatalf("insert %q: %v", subject, err)
	}
	return id
}

// Error propagation: an unparsable confirmed subject and a failing
// exists-query are each reported (by fact id) without stopping the facts
// after them; the unparsable confirmed fact keeps binding.
func TestReverifyCollectsPerFactErrorsAndContinues(t *testing.T) {
	pool, ctx := livePool(t)
	present := uniqueName("ok_")
	createSlot(t, ctx, pool, present)
	failing := uniqueName("timeout_")
	s := NewStore(pool)
	s.catalog = failingCatalog{pool: pool, fail: failing}
	badID := insertConfirmedRaw(t, ctx, pool, "Bad-Slot!")
	failID := declareSlot(t, ctx, s, failing).ID
	presentID := declareSlot(t, ctx, s, present).ID
	absentID := declareSlot(t, ctx, s, uniqueName("absent_")).ID
	now := time.Now()
	expired, err := s.Reverify(ctx, now)
	if err == nil || len(expired) != 0 {
		t.Fatalf("reverify: expired %+v err %v (want both failures reported)", expired, err)
	}
	for _, id := range []int64{badID, failID} {
		if !strings.Contains(err.Error(), fmt.Sprintf("fact %d", id)) {
			t.Fatalf("error %q does not name fact %d", err, id)
		}
	}
	if !strings.Contains(err.Error(), "statement timeout") {
		t.Fatalf("error %q lost the cause", err)
	}
	if f := mustGet(t, ctx, s, presentID); f.LastVerifiedAt == nil {
		t.Fatalf("the present fact after the failures was not verified: %+v", f)
	}
	if f := mustGet(t, ctx, s, absentID); !sameInstant(f.MissingSince, now) {
		t.Fatalf("the absent fact after the failures was not marked: %+v", f)
	}
	if f := mustGet(t, ctx, s, badID); f.Status != StatusConfirmed {
		t.Fatalf("an unparsable confirmed subject was expired: %+v", f)
	}
}

// Nil/empty: no live facts is no work and no error.
func TestReverifyWithNoFacts(t *testing.T) {
	pool, ctx := livePool(t)
	got, err := NewStore(pool).Reverify(ctx, time.Now())
	if err != nil || len(got) != 0 {
		t.Fatalf("empty store: %+v %v", got, err)
	}
}

// Concurrency: two sidecars (two pools, so no shared process state)
// re-verifying the same absent facts at once expire each fact once and
// notify it once; the row guard makes the loser skip it quietly.
func TestConcurrentReverifyExpiresAndNotifiesOnce(t *testing.T) {
	pool, ctx := livePool(t)
	other, err := pgxpool.New(ctx, livePoolDSN(t))
	if err != nil {
		t.Fatalf("second pool: %v", err)
	}
	defer other.Close()
	stores := []*Store{NewStore(pool).WithConfirmedAbsence(time.Hour),
		NewStore(other).WithConfirmedAbsence(time.Hour)}
	const n = 6
	t0 := time.Now()
	for i := 0; i < n; i++ {
		declareSlot(t, ctx, stores[0], uniqueName(fmt.Sprintf("race%d_", i)))
	}
	mustReverify(t, ctx, stores[0], t0)
	var notified, expiredTotal atomic.Int64
	var wg sync.WaitGroup
	errs := make([]string, 0)
	var mu sync.Mutex
	for _, st := range stores {
		w := NewWorker(st, WorkerOptions{Now: func() time.Time { return t0.Add(2 * time.Hour) },
			NotifyExpired: func(context.Context, Fact) { notified.Add(1) }})
		wg.Add(1)
		go func() {
			defer wg.Done()
			stats := w.RunOnce(ctx)
			expiredTotal.Add(int64(stats.Expired))
			mu.Lock()
			errs = append(errs, stats.Errors...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(errs) != 0 || expiredTotal.Load() != n || notified.Load() != n {
		t.Fatalf("expired %d, notified %d, errors %v; want %d, %d, none",
			expiredTotal.Load(), notified.Load(), errs, n, n)
	}
}

// The worker notifies the expiry of a confirmed fact, never a proposal's.
func TestWorkerNotifiesOnlyLostConfirmations(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool).WithConfirmedAbsence(time.Second).WithMissingGrace(time.Second)
	confirmed := declareSlot(t, ctx, s, uniqueName("lost_"))
	if _, _, err := s.Propose(ctx, slotFactProposal(uniqueName("prop_"), "x")); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	var got []Fact
	var logged []string
	w := NewWorker(s, WorkerOptions{Now: func() time.Time { return now },
		NotifyExpired: func(_ context.Context, f Fact) { got = append(got, f) },
		LogFn: func(level, format string, args ...any) {
			logged = append(logged, level+" "+fmt.Sprintf(format, args...))
		}})
	if first := w.RunOnce(ctx); first.Expired != 0 {
		t.Fatalf("first pass marks absence only: %+v", first)
	}
	now = now.Add(time.Second)
	stats := w.RunOnce(ctx)
	if stats.Expired != 2 || len(got) != 1 || got[0].ID != confirmed.ID ||
		got[0].Status != StatusExpired || !got[0].ConfirmationLost() {
		t.Fatalf("stats %+v notified %+v", stats, got)
	}
	w.log(stats)
	if !strings.Contains(strings.Join(logged, "\n"), fmt.Sprintf("fact #%d", confirmed.ID)) {
		t.Fatalf("a lost confirmation is not logged by id: %v", logged)
	}
}
