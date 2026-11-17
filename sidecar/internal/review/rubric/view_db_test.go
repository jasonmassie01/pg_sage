package rubric

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// review-01b on a real server: the read receipt (sage.review_view) keeps a
// person's first opening of a finished run, once per run and user, and the
// running run the Review page shows as a banner.

func finishedRun(t *testing.T, ctx context.Context, s *Store, db string,
	status RunStatus) int64 {
	t.Helper()
	id, err := s.Start(ctx, spec1(db))
	if err != nil {
		t.Fatalf("start %s: %v", db, err)
	}
	var g *Grade
	runErr := "boom"
	if status != RunFailed {
		gr := Compute(V1(), nil)
		g, runErr = &gr, ""
	}
	if err := s.Finish(ctx, id, status, g, runErr); err != nil {
		t.Fatalf("finish %d as %s: %v", id, status, err)
	}
	return id
}

func TestStoreViewDB_FirstViewOnceThenTheSameTime(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	id := finishedRun(t, ctx, s, "alpha", RunDone)
	if at, err := s.ViewedAt(ctx, id, 7); err != nil || at != nil {
		t.Fatalf("viewed_at before opening = %v %v, want nil", at, err)
	}
	first, err := s.MarkViewed(ctx, "alpha", id, 7)
	if err != nil || !first.First || first.RunID != id || first.ViewedAt.IsZero() {
		t.Fatalf("first view = %+v %v, want first with a time", first, err)
	}
	again, err := s.MarkViewed(ctx, "alpha", id, 7)
	if err != nil || again.First || !again.ViewedAt.Equal(first.ViewedAt) {
		t.Fatalf("second view = %+v %v, want not first and the first time %v", again, err,
			first.ViewedAt)
	}
	at, err := s.ViewedAt(ctx, id, 7)
	if err != nil || at == nil || !at.Equal(first.ViewedAt) {
		t.Fatalf("viewed_at = %v %v, want %v", at, err, first.ViewedAt)
	}
	other, err := s.MarkViewed(ctx, "alpha", id, 8)
	if err != nil || !other.First {
		t.Fatalf("another user's view = %+v %v, want their own first view", other, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_view WHERE run_id = $1`,
		id).Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("view rows = %d %v, want one per user (2)", rows, err)
	}
}

func TestStoreViewDB_PartialRunCountsAsFinished(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	id := finishedRun(t, ctx, s, "alpha", RunPartial)
	if v, err := s.MarkViewed(ctx, "alpha", id, 7); err != nil || !v.First {
		t.Fatalf("view of a partial run = %+v %v, want recorded", v, err)
	}
}

func TestStoreViewDB_ConcurrentTabsGiveOneRow(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	id := finishedRun(t, ctx, s, "alpha", RunDone)
	const tabs = 8
	views := make([]View, tabs)
	errs := make([]error, tabs)
	var wg sync.WaitGroup
	for i := range tabs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			views[i], errs[i] = s.MarkViewed(ctx, "alpha", id, 7)
		}()
	}
	wg.Wait()
	firsts := 0
	for i := range tabs {
		if errs[i] != nil {
			t.Fatalf("tab %d: %v", i, errs[i])
		}
		if views[i].First {
			firsts++
		}
		if !views[i].ViewedAt.Equal(views[0].ViewedAt) {
			t.Fatalf("tab %d saw %v, tab 0 %v: want one first time", i, views[i].ViewedAt,
				views[0].ViewedAt)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_view WHERE run_id = $1`,
		id).Scan(&rows); err != nil || rows != 1 || firsts != 1 {
		t.Fatalf("rows = %d %v, firsts = %d; want one row and one first view", rows, err,
			firsts)
	}
}

func TestStoreViewDB_RefusesRunsThatAreNotFinishedReviewsOfTheDatabase(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	failed := finishedRun(t, ctx, s, "alpha", RunFailed)
	done := finishedRun(t, ctx, s, "alpha", RunDone)
	running, err := s.Start(ctx, spec1("alpha"))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		db   string
		id   int64
		want error
	}{
		{"running", "alpha", running, ErrNotFinished},
		{"failed", "alpha", failed, ErrNotFinished},
		{"another database's run", "beta", done, ErrRunNotFound},
		{"no such run", "alpha", done + 1000, ErrRunNotFound},
	}
	for _, c := range cases {
		if _, err := s.MarkViewed(ctx, c.db, c.id, 7); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", c.name, err, c.want)
		}
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_view`).Scan(&rows); err !=
		nil || rows != 0 {
		t.Fatalf("view rows = %d %v, want none recorded", rows, err)
	}
}

func TestStoreView_InvalidInputAndNoStore(t *testing.T) {
	var none *Store
	if _, err := none.MarkViewed(context.Background(), "a", 1, 1); !errors.Is(err,
		ErrNoStore) {
		t.Fatalf("nil store = %v, want ErrNoStore", err)
	}
	if _, err := none.ViewedAt(context.Background(), 1, 1); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store viewed_at = %v, want ErrNoStore", err)
	}
	if _, err := none.Running(context.Background(), "a"); !errors.Is(err, ErrNoStore) {
		t.Fatalf("nil store running = %v, want ErrNoStore", err)
	}
	s := NewStore(nil)
	for _, c := range []struct {
		db     string
		run    int64
		userID int
	}{{"", 1, 1}, {"a", 0, 1}, {"a", -3, 1}, {"a", 1, 0}, {"a", 1, -1}} {
		// Validation comes first: a bad id never reaches a pool.
		_, err := s.MarkViewed(context.Background(), c.db, c.run, c.userID)
		if !errors.Is(err, ErrInvalidRun) {
			t.Errorf("%+v: err = %v, want ErrInvalidRun", c, err)
		}
	}
}

func TestStoreRunningDB_NoneThenTheRunThenNoneAfterFinish(t *testing.T) {
	pool, ctx := livePool(t)
	s := NewStore(pool)
	if r, err := s.Running(ctx, "alpha"); err != nil || r != nil {
		t.Fatalf("running before any run = %+v %v, want nil", r, err)
	}
	id, err := s.Start(ctx, RunSpec{Database: "alpha", Kind: KindGrade,
		Triggers: []string{"daily"}, RubricVersion: Version})
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Running(ctx, "alpha")
	if err != nil || r == nil || r.ID != id || r.Status != RunRunning || r.Kind != KindGrade ||
		r.StartedAt.IsZero() || len(r.Triggers) != 1 || r.Triggers[0] != "daily" {
		t.Fatalf("running = %+v %v, want run %d running", r, err, id)
	}
	if other, err := s.Running(ctx, "beta"); err != nil || other != nil {
		t.Fatalf("another database's running = %+v %v, want nil", other, err)
	}
	g := Compute(V1(), nil)
	if err := s.Finish(ctx, id, RunDone, &g, ""); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Running(ctx, "alpha"); err != nil || r != nil {
		t.Fatalf("running after finish = %+v %v, want nil", r, err)
	}
}
