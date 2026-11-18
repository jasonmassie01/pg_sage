package candidate

import (
	"errors"
	"reflect"
	"sync"
	"testing"
)

func TestStoreDB_SaveListRoundTripsEveryField(t *testing.T) {
	pool, ctx := livePool(t)
	run := newRun(t, ctx, pool, "db1")
	c := dropCandidate()
	pct := 25.0
	c.Prediction.SizePct = &pct
	c.Prediction.Quantities = map[string]float64{"bytes_reclaimed": 8192}
	s := NewStore(pool)
	n, err := s.Save(ctx, run, []Candidate{c})
	if err != nil || n != 1 {
		t.Fatalf("save: n=%d err=%v, want 1 row", n, err)
	}
	got, err := s.List(ctx, run)
	if err != nil || len(got) != 1 {
		t.Fatalf("list: %d rows, err %v", len(got), err)
	}
	if got[0].Status != StatusProposed {
		t.Errorf("status %q, want proposed", got[0].Status)
	}
	if !reflect.DeepEqual(got[0].Candidate, c) {
		t.Errorf("round trip changed the candidate:\n got %+v\nwant %+v", got[0].Candidate, c)
	}
}

func TestStoreDB_SavingTwiceKeepsOneRow(t *testing.T) {
	pool, ctx := livePool(t)
	run := newRun(t, ctx, pool, "db1")
	s := NewStore(pool)
	if _, err := s.Save(ctx, run, []Candidate{dropCandidate()}); err != nil {
		t.Fatal(err)
	}
	n, err := s.Save(ctx, run, []Candidate{dropCandidate()})
	if err != nil || n != 0 {
		t.Fatalf("second save: n=%d err=%v, want 0 new rows", n, err)
	}
	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.review_candidate
		WHERE run_id = $1`, run).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("rows = %d (%v), want 1", rows, err)
	}
}

func TestStoreDB_SaveRefusesATamperedIDAndAnUnknownRun(t *testing.T) {
	pool, ctx := livePool(t)
	run := newRun(t, ctx, pool, "db1")
	s := NewStore(pool)
	bad := dropCandidate()
	bad.ID = "abc"
	if _, err := s.Save(ctx, run, []Candidate{bad}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("tampered id: err = %v, want ErrInvalid", err)
	}
	_, err := s.Save(ctx, run+1000, []Candidate{dropCandidate()})
	if err == nil {
		t.Fatal("a candidate of an unknown run was saved")
	}
}

func TestStoreDB_StatusMovesOnlyAlongTheContract(t *testing.T) {
	pool, ctx := livePool(t)
	run := newRun(t, ctx, pool, "db1")
	s := NewStore(pool)
	c := dropCandidate()
	if _, err := s.Save(ctx, run, []Candidate{c}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus(ctx, run, c.ID, StatusSuperseded); err != nil {
		t.Fatalf("proposed -> superseded: %v", err)
	}
	err := s.SetStatus(ctx, run, c.ID, StatusPlanned)
	if !errors.Is(err, ErrTransition) {
		t.Fatalf("superseded -> planned: err = %v, want ErrTransition", err)
	}
	got, err := s.List(ctx, run)
	if err != nil || len(got) != 1 || got[0].Status != StatusSuperseded {
		t.Fatalf("after the refused move: %+v (%v), want superseded", got, err)
	}
	if err := s.SetStatus(ctx, run, "nope", StatusPlanned); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown candidate: err = %v, want ErrNotFound", err)
	}
}

func TestStoreDB_ConcurrentMovesFromProposedHaveOneWinner(t *testing.T) {
	pool, ctx := livePool(t)
	run := newRun(t, ctx, pool, "db1")
	s := NewStore(pool)
	c := dropCandidate()
	if _, err := s.Save(ctx, run, []Candidate{c}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, to := range []Status{StatusPlanned, StatusRejected} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = s.SetStatus(ctx, run, c.ID, to)
		}()
	}
	wg.Wait()
	wins := 0
	for _, err := range errs {
		if err == nil {
			wins++
		} else if !errors.Is(err, ErrTransition) {
			t.Errorf("loser err = %v, want ErrTransition", err)
		}
	}
	if wins != 1 {
		t.Fatalf("%d moves won (%v), want exactly 1", wins, errs)
	}
}
