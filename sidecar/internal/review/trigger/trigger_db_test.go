package trigger

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The queue on a real server: constraints, dedupe, the state machine,
// two sidecars claiming at once, an expired lease and a restart.

func TestTriggerDB_CheckConstraintsRefuseUnknownKindAndNonObjectDetail(t *testing.T) {
	pool, ctx := livePool(t, "checks")
	for _, c := range []struct{ kind, detail string }{
		{"nightly", `{}`}, {"calendar", `[1]`}, {"calendar", `"text"`}, {"calendar", `3`},
	} {
		_, err := pool.Exec(ctx, `INSERT INTO sage.review_trigger
			(database_name, kind, dedupe_key, detail) VALUES ('shop', $1, 'k', $2::jsonb)`,
			c.kind, c.detail)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23514" {
			t.Errorf("kind %q detail %s: err %v, want a check violation", c.kind, c.detail,
				err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sage.review_trigger (database_name, kind,
		dedupe_key, detail) VALUES ('shop', 'calendar', 'k', '{}')`); err != nil {
		t.Fatalf("a valid row was refused: %v", err)
	}
}

func TestTriggerDB_FireDedupesByKindAndKey(t *testing.T) {
	pool, ctx := livePool(t, "dedupe")
	s := NewStore(pool)
	tr := Trigger{Database: "shop", Kind: KindUnresolvedRegression, Key: "finding:7",
		Detail: map[string]any{"finding_id": 7}}
	id, fired, err := s.Fire(ctx, tr)
	if err != nil || !fired || id == 0 {
		t.Fatalf("first fire: %d %v %v", id, fired, err)
	}
	again, fired, err := s.Fire(ctx, tr)
	if err != nil || fired || again != 0 {
		t.Fatalf("second fire: %d %v %v, want a quiet no-op", again, fired, err)
	}
	other := tr
	other.Database = "crm"
	if _, fired, err := s.Fire(ctx, other); err != nil || !fired {
		t.Fatalf("same key on another database: %v %v, want fired", fired, err)
	}
	got, err := s.Pending(ctx, "shop")
	if err != nil || len(got) != 1 || got[0].ID != id || got[0].Kind != tr.Kind ||
		got[0].Detail["finding_id"] != float64(7) || got[0].State != StateFired {
		t.Fatalf("pending = %+v (%v), want the one finding trigger", got, err)
	}
}

func TestTriggerDB_NoTriggersMeansNoRuns(t *testing.T) {
	pool, ctx := livePool(t, "empty")
	rv := &sqlReviewer{pool: pool}
	c := &clock{now: t0}
	res, err := scheduler(NewStore(pool), "shop", "a", rv, c).Tick(ctx)
	if err != nil || res.RunID != 0 || rv.count() != 0 {
		t.Fatalf("tick %+v %v, %d reviews: want none", res, err, rv.count())
	}
	if runs := reviewRuns(t, ctx, pool, "shop"); len(runs) != 0 {
		t.Fatalf("review runs %v, want none", runs)
	}
}

// Boundary on the server: 23h59m apart is one run listing both; 24h apart
// is two runs.
func TestTriggerDB_CoalescingBoundary(t *testing.T) {
	pool, ctx := livePool(t, "coalesce")
	s := NewStore(pool)
	rv := &sqlReviewer{pool: pool}
	c := &clock{now: t0}
	a := fire(t, ctx, s, "near", KindUnresolvedRegression, t0)
	b := fire(t, ctx, s, "near", KindCostGrowth, t0.Add(23*time.Hour+59*time.Minute))
	c.set(t0.Add(24 * time.Hour))
	res, err := scheduler(s, "near", "a", rv, c).Tick(ctx)
	if err != nil || res.RunID == 0 || len(res.Triggers) != 2 {
		t.Fatalf("near tick %+v %v, want one run of both", res, err)
	}
	st := states(t, ctx, pool)
	if st[a] != fmt.Sprintf("consumed run=%d into=0", res.RunID) ||
		st[b] != fmt.Sprintf("coalesced run=%d into=%d", res.RunID, a) {
		t.Fatalf("states %v, want a consumed and b coalesced into it", st)
	}
	runs := reviewRuns(t, ctx, pool, "near")
	if len(runs) != 1 || !hasAll(runs[0], "cost_growth_anomaly", "unresolved_regression") {
		t.Fatalf("runs %v, want one listing both kinds", runs)
	}

	fire(t, ctx, s, "far", KindUnresolvedRegression, t0)
	fire(t, ctx, s, "far", KindCostGrowth, t0.Add(24*time.Hour))
	far := scheduler(s, "far", "a", rv, c)
	if res, err = far.Tick(ctx); err != nil || len(res.Triggers) != 1 {
		t.Fatalf("far tick 1 %+v %v, want the first alone", res, err)
	}
	c.set(t0.Add(48 * time.Hour))
	if res, err = far.Tick(ctx); err != nil || len(res.Triggers) != 1 ||
		res.Triggers[0].Kind != KindCostGrowth {
		t.Fatalf("far tick 2 %+v %v, want the second alone", res, err)
	}
	if runs := reviewRuns(t, ctx, pool, "far"); len(runs) != 2 {
		t.Fatalf("far runs %v, want two", runs)
	}
}

// State transitions: fired -> claimed -> consumed or coalesced, and a
// release returns a claim to fired.
func TestTriggerDB_StateTransitions(t *testing.T) {
	pool, ctx := livePool(t, "states")
	s := NewStore(pool)
	a := fire(t, ctx, s, "shop", KindCalendar, t0)
	b := fire(t, ctx, s, "shop", KindEnrollment, t0.Add(time.Minute))
	got, err := s.Claim(ctx, "shop", []int64{a, b}, "owner-1", t0.Add(time.Hour))
	if err != nil || len(got) != 2 || got[0].State != StateClaimed {
		t.Fatalf("claim = %+v %v", got, err)
	}
	if err := s.Release(ctx, []int64{b}, "owner-2"); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("release by another owner = %v, want ErrLeaseLost", err)
	}
	if err := s.Release(ctx, []int64{b}, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if st := states(t, ctx, pool); st[b] != "fired run=0 into=0" ||
		st[a] != "claimed run=0 into=0" {
		t.Fatalf("after release %v", st)
	}
	if again, err := s.Claim(ctx, "shop", []int64{a, b}, "owner-1",
		t0.Add(time.Hour)); err != nil || len(again) != 1 || again[0].ID != b {
		t.Fatalf("reclaim = %+v %v, want only the released one", again, err)
	}
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		rubric_version) VALUES ('shop', 'review', 'v') RETURNING id`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(ctx, []int64{a, b}, run, "owner-1"); err != nil {
		t.Fatal(err)
	}
	if err := s.Consume(ctx, []int64{a, b}, run); err != nil {
		t.Fatal(err)
	}
	want := map[int64]string{a: fmt.Sprintf("consumed run=%d into=0", run),
		b: fmt.Sprintf("coalesced run=%d into=%d", run, a)}
	if st := states(t, ctx, pool); st[a] != want[a] || st[b] != want[b] {
		t.Fatalf("states %v, want %v", st, want)
	}
	if err := s.Consume(ctx, []int64{a}, run+1); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("consuming a consumed trigger = %v, want ErrLeaseLost", err)
	}
	why, err := s.WhyNow(ctx, run)
	if err != nil || len(why) != 2 || why[0].Kind != KindCalendar ||
		why[1].Kind != KindEnrollment || why[0].Detail["seeded"] != true {
		t.Fatalf("why now = %+v %v, want both triggers with details", why, err)
	}
}

// Concurrency: two sidecars (two pools) claiming the same queue at once
// get disjoint claims that cover every trigger.
func TestTriggerDB_TwoPoolsClaimEachTriggerOnce(t *testing.T) {
	pool, ctx := livePool(t, "skiplocked")
	other, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	s := NewStore(pool)
	var all []int64
	for i := 0; i < 60; i++ {
		all = append(all, fire(t, ctx, s, "shop", KindCostGrowth,
			t0.Add(time.Duration(i)*time.Second)))
	}
	var mu sync.Mutex
	seen := map[int64]string{}
	var wg sync.WaitGroup
	for i, p := range []*pgxpool.Pool{pool, other, pool, other} {
		wg.Add(1)
		go func(owner string, st *Store) {
			defer wg.Done()
			got, err := st.Claim(ctx, "shop", all, owner, t0.Add(time.Hour))
			if err != nil {
				t.Errorf("%s: %v", owner, err)
				return
			}
			mu.Lock()
			defer mu.Unlock()
			for _, g := range got {
				if prev, dup := seen[g.ID]; dup {
					t.Errorf("trigger %d claimed by %s and %s", g.ID, prev, owner)
				}
				seen[g.ID] = owner
			}
		}(fmt.Sprintf("sidecar-%d", i), NewStore(p))
	}
	wg.Wait()
	if len(seen) != len(all) {
		t.Fatalf("%d of %d triggers claimed", len(seen), len(all))
	}
}

// A claim whose leader died returns to fired once the lease expires, and
// the next leader runs it once.
func TestTriggerDB_ExpiredLeaseIsReclaimedWithoutADoubleRun(t *testing.T) {
	pool, ctx := livePool(t, "lease")
	s := NewStore(pool)
	a := fire(t, ctx, s, "shop", KindPreUpgrade, t0)
	got, err := s.Claim(ctx, "shop", []int64{a}, "dead", t0.Add(10*time.Minute))
	if err != nil || len(got) != 1 {
		t.Fatalf("claim %v %v", got, err)
	}
	rv := &sqlReviewer{pool: pool}
	c := &clock{now: t0.Add(5 * time.Minute)}
	next := scheduler(s, "shop", "next", rv, c)
	if res, err := next.Tick(ctx); err != nil || res.RunID != 0 || rv.count() != 0 {
		t.Fatalf("inside the lease: %+v %v, %d reviews, want none", res, err, rv.count())
	}
	c.set(t0.Add(11 * time.Minute))
	res, err := next.Tick(ctx)
	if err != nil || res.RunID == 0 || rv.count() != 1 {
		t.Fatalf("after the lease: %+v %v, %d reviews, want one", res, err, rv.count())
	}
	c.set(t0.Add(30 * time.Minute))
	if res, err := next.Tick(ctx); err != nil || res.RunID != 0 || rv.count() != 1 {
		t.Fatalf("later tick: %+v %v, %d reviews, want still one", res, err, rv.count())
	}
}

// CHECK-34: the loop is killed mid-claim (after its run started, and
// after its run finished but before the claim was consumed); a restart
// gives exactly one finished run per trigger.
func TestTriggerDB_RestartAfterAKillGivesOneRunPerTrigger(t *testing.T) {
	pool, ctx := livePool(t, "restart")
	s := NewStore(pool)
	a := fire(t, ctx, s, "shop", KindPreUpgrade, t0)
	killed, kill := context.WithCancel(ctx)
	held := &sqlReviewer{pool: pool, hold: true}
	c := &clock{now: t0}
	done := make(chan error, 1)
	go func() {
		_, err := scheduler(s, "shop", "first", held, c).Tick(killed)
		done <- err
	}()
	waitState(t, ctx, pool, a, "claimed")
	kill()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("killed tick = %v, want context.Canceled", err)
	}
	// The reaper fails the run the killed sidecar left running.
	if _, err := pool.Exec(ctx, `UPDATE sage.review_run SET status = 'failed',
		finished_at = now(), error = 'sidecar stopped during run'
		WHERE status = 'running'`); err != nil {
		t.Fatal(err)
	}
	rv := &sqlReviewer{pool: pool}
	c.set(t0.Add(11 * time.Minute))
	restarted := scheduler(s, "shop", "second", rv, c)
	for i := 0; i < 3; i++ {
		if _, err := restarted.Tick(ctx); err != nil {
			t.Fatalf("restarted tick %d: %v", i, err)
		}
		c.set(c.Now().Add(15 * time.Minute))
	}
	runs := reviewRuns(t, ctx, pool, "shop")
	if rv.count() != 1 || len(runs) != 2 || runs[0] != "pre_upgrade:failed" ||
		runs[1] != "pre_upgrade:done" {
		t.Fatalf("%d reviews, runs %v, want the killed run failed and one done", rv.count(),
			runs)
	}
	assertResumeWithoutRerun(t, ctx, pool, s)
}

// The second kill point: the run finished but the claim was not consumed.
// The next leader consumes it into that run and runs nothing.
func assertResumeWithoutRerun(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	s *Store) {
	t.Helper()
	b := fire(t, ctx, s, "crm", KindPreUpgrade, t0)
	if _, err := s.Claim(ctx, "crm", []int64{b}, "dead", t0.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	var run int64
	if err := pool.QueryRow(ctx, `INSERT INTO sage.review_run (database_name, kind,
		triggers, rubric_version, status, finished_at) VALUES ('crm', 'review',
		'{pre_upgrade}', 'v', 'done', now()) RETURNING id`).Scan(&run); err != nil {
		t.Fatal(err)
	}
	if err := s.Attach(ctx, []int64{b}, run, "dead"); err != nil {
		t.Fatal(err)
	}
	rv := &sqlReviewer{pool: pool}
	res, err := scheduler(s, "crm", "next", rv, &clock{now: t0.Add(11 * time.Minute)}).
		Tick(ctx)
	if err != nil || !res.Resumed || res.RunID != run || rv.count() != 0 {
		t.Fatalf("resume %+v %v, %d reviews, want run %d consumed without a rerun", res,
			err, rv.count(), run)
	}
	if st := states(t, ctx, pool)[b]; st != fmt.Sprintf("consumed run=%d into=0", run) {
		t.Fatalf("state %q", st)
	}
}

func waitState(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id int64, want string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		var st string
		var run *int64
		if err := pool.QueryRow(ctx, `SELECT state, run_id FROM sage.review_trigger
			WHERE id = $1`, id).Scan(&st, &run); err == nil && st == want && run != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("trigger %d never reached %s with a run", id, want)
}

// A grade already running: the claim is released and the trigger waits
// for the next tick.
func TestTriggerDB_BusyReviewerReleasesTheClaim(t *testing.T) {
	pool, ctx := livePool(t, "busy")
	s := NewStore(pool)
	a := fire(t, ctx, s, "shop", KindPreUpgrade, t0)
	rv := &sqlReviewer{pool: pool, busy: true}
	res, err := scheduler(s, "shop", "a", rv, &clock{now: t0}).Tick(ctx)
	if err != nil || !res.Busy || res.RunID != 0 {
		t.Fatalf("busy tick %+v %v, want busy and no run", res, err)
	}
	if st := states(t, ctx, pool)[a]; st != "fired run=0 into=0" {
		t.Fatalf("state %q, want fired again", st)
	}
}

// Error propagation: a failed insert names the kind and the database, is
// returned to the hook, and is counted.
func TestTriggerDB_FailedInsertIsReturnedNamedAndCounted(t *testing.T) {
	pool, ctx := livePool(t, "insertfail")
	closed, err := pgxpool.New(ctx, pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	h := NewHub()
	h.Register("shop", NewStore(closed), 1<<30)
	before := FireErrors()[KindPreUpgrade]
	err = h.PreUpgrade(ctx, "shop", 18, "pg_upgrade", t0)
	if err == nil || !hasAll(err.Error(), "pre_upgrade", "shop", "sage.review_trigger") {
		t.Fatalf("err = %v, want the kind, database and table named", err)
	}
	if got := FireErrors()[KindPreUpgrade]; got != before+1 {
		t.Fatalf("errors %d, want %d", got, before+1)
	}
	h.Register("shop", NewStore(pool), 1<<30)
	if err := h.PreUpgrade(ctx, "shop", 18, "pg_upgrade", t0); err != nil {
		t.Fatalf("working store: %v", err)
	}
	got, _ := NewStore(pool).Pending(ctx, "shop")
	if len(got) != 1 || got[0].Detail["target_major"] != float64(18) {
		t.Fatalf("pending %+v, want the pre_upgrade with its target", got)
	}
	kinds := Run{Triggers: got}.Kinds()
	sort.Strings(kinds)
	if strings.Join(kinds, ",") != "pre_upgrade" {
		t.Fatalf("kinds %v", kinds)
	}
}
