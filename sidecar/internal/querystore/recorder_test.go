package querystore

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// fakeExec records the statements and arguments Record sends.
type fakeExec struct {
	mu    sync.Mutex
	calls int
	args  [][]any
	err   error
}

func (f *fakeExec) Exec(_ context.Context, _ string, args ...any) (pgconn.CommandTag, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.args = append(f.args, args)
	return pgconn.NewCommandTag("INSERT 0 1"), f.err
}

// queryIDs is the queryid array a recorded statement carried.
func (f *fakeExec) queryIDs(i int) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.args[i][0].([]int64)
}

var t0 = time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)

func idle(qid int64) Sample {
	return Sample{QueryID: qid, Calls: 10, TotalExecMs: 100, MeanExecMs: 10, Rows: 10}
}

func TestRecorder_FirstCycleWritesEverySampleInOneStatement(t *testing.T) {
	r, db := NewRecorder(), &fakeExec{}
	n, err := r.Record(context.Background(), db, []Sample{idle(1), idle(2), idle(3)}, t0)
	if err != nil || n != 3 || db.calls != 1 {
		t.Fatalf("Record = %d, %v with %d statements; want 3 rows in 1", n, err, db.calls)
	}
	if got := db.queryIDs(0); len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("queryids = %v", got)
	}
}

func TestRecorder_UnchangedCountersWriteNothing(t *testing.T) {
	r, db := NewRecorder(), &fakeExec{}
	ctx := context.Background()
	samples := []Sample{idle(1), idle(2)}
	if _, err := r.Record(ctx, db, samples, t0); err != nil {
		t.Fatal(err)
	}
	for minute := 1; minute < 60; minute++ {
		n, err := r.Record(ctx, db, samples, t0.Add(time.Duration(minute)*time.Minute))
		if err != nil || n != 0 {
			t.Fatalf("minute %d: Record = %d, %v; want nothing written", minute, n, err)
		}
	}
	if db.calls != 1 {
		t.Fatalf("%d statements for 60 idle cycles, want only the first", db.calls)
	}
}

func TestRecorder_EachChangedCounterIsWritten(t *testing.T) {
	epoch := t0.Add(-time.Hour)
	changes := map[string]func(*Sample){
		"calls":       func(s *Sample) { s.Calls++ },
		"total time":  func(s *Sample) { s.TotalExecMs += 0.5 },
		"stats epoch": func(s *Sample) { s.StatsEpoch = epoch.Add(time.Minute) },
	}
	for name, change := range changes {
		t.Run(name, func(t *testing.T) {
			r, db := NewRecorder(), &fakeExec{}
			base := idle(7)
			base.StatsEpoch = epoch
			if _, err := r.Record(context.Background(), db, []Sample{base, idle(8)}, t0); err != nil {
				t.Fatal(err)
			}
			moved := base
			change(&moved)
			n, err := r.Record(context.Background(), db, []Sample{moved, idle(8)},
				t0.Add(time.Minute))
			if err != nil || n != 1 || db.calls != 2 {
				t.Fatalf("Record = %d, %v (%d statements); want the changed sample only",
					n, err, db.calls)
			}
			if got := db.queryIDs(1); len(got) != 1 || got[0] != 7 {
				t.Fatalf("queryids = %v, want [7]", got)
			}
		})
	}
}

// An idle query is still written once per KeyframeInterval so readers find
// a sample of every observed query within AnchorLookback.
func TestRecorder_IdleQueryKeyframeBoundary(t *testing.T) {
	r, db := NewRecorder(), &fakeExec{}
	ctx := context.Background()
	if _, err := r.Record(ctx, db, []Sample{idle(1)}, t0); err != nil {
		t.Fatal(err)
	}
	just := t0.Add(KeyframeInterval - time.Second)
	if n, _ := r.Record(ctx, db, []Sample{idle(1)}, just); n != 0 {
		t.Fatalf("written %d a second before the keyframe interval", n)
	}
	if n, _ := r.Record(ctx, db, []Sample{idle(1)}, t0.Add(KeyframeInterval)); n != 1 {
		t.Fatalf("written %d at the keyframe interval, want 1", n)
	}
	if n, _ := r.Record(ctx, db, []Sample{idle(1)},
		t0.Add(KeyframeInterval+time.Minute)); n != 0 {
		t.Fatalf("written %d right after a keyframe, want 0", n)
	}
	if AnchorLookback < 2*KeyframeInterval {
		t.Fatalf("AnchorLookback %s must cover two keyframe intervals", AnchorLookback)
	}
}

func TestRecorder_FailedWriteIsRetriedNextCycle(t *testing.T) {
	r := NewRecorder()
	ctx := context.Background()
	broken := &fakeExec{err: errors.New("connection refused")}
	n, err := r.Record(ctx, broken, []Sample{idle(1), idle(2)}, t0)
	if err == nil || n != 0 {
		t.Fatalf("Record over a failing database = %d, %v; want an error", n, err)
	}
	if !errors.Is(err, broken.err) {
		t.Fatalf("error %v does not wrap the cause", err)
	}
	ok := &fakeExec{}
	if n, err := r.Record(ctx, ok, []Sample{idle(1), idle(2)}, t0.Add(time.Minute)); err != nil ||
		n != 2 {
		t.Fatalf("retry = %d, %v; want both samples again", n, err)
	}
}

// pg_stat_statements splits one statement by user and top level: the parts
// are summed before the comparison, so an unchanged split is unchanged.
func TestRecorder_AggregatesSplitQueryIDsBeforeComparing(t *testing.T) {
	r, db := NewRecorder(), &fakeExec{}
	ctx := context.Background()
	split := []Sample{
		{QueryID: 5, Calls: 3, TotalExecMs: 30},
		{QueryID: 5, Calls: 4, TotalExecMs: 50},
	}
	if n, err := r.Record(ctx, db, split, t0); err != nil || n != 1 {
		t.Fatalf("first = %d, %v; want one aggregated sample", n, err)
	}
	if got := db.args[0][1].([]int64); got[0] != 7 {
		t.Fatalf("calls = %v, want the sum 7", got)
	}
	if n, _ := r.Record(ctx, db, split, t0.Add(time.Minute)); n != 0 {
		t.Fatalf("unchanged split written %d", n)
	}
}

func TestRecorder_EmptyAndZeroInput(t *testing.T) {
	r, db := NewRecorder(), &fakeExec{}
	for _, in := range [][]Sample{nil, {}} {
		if n, err := r.Record(context.Background(), db, in, t0); err != nil || n != 0 {
			t.Fatalf("Record(%v) = %d, %v", in, n, err)
		}
	}
	if db.calls != 0 {
		t.Fatalf("empty input sent %d statements", db.calls)
	}
	if _, err := r.Record(context.Background(), nil, []Sample{idle(1)}, t0); err == nil {
		t.Fatal("Record with no database succeeded")
	}
	if _, err := r.Record(context.Background(), db, []Sample{idle(1)}, time.Time{}); err == nil {
		t.Fatal("Record with a zero time succeeded")
	}
}

// Queries that leave pg_stat_statements are forgotten, so the recorder's
// memory is bounded by the live statement set.
func TestRecorder_ForgetsQueriesNotSeenForTwoKeyframes(t *testing.T) {
	r, db := NewRecorder(), &fakeExec{}
	ctx := context.Background()
	if _, err := r.Record(ctx, db, []Sample{idle(1), idle(2)}, t0); err != nil {
		t.Fatal(err)
	}
	later := t0.Add(2*KeyframeInterval + time.Minute)
	if _, err := r.Record(ctx, db, []Sample{idle(2)}, later); err != nil {
		t.Fatal(err)
	}
	if got := r.tracked(); got != 1 {
		t.Fatalf("tracking %d queryids, want 1", got)
	}
}

func TestRecorder_ConcurrentCyclesAreSafe(t *testing.T) {
	r, db := NewRecorder(), &fakeExec{}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				s := idle(int64(g))
				s.Calls += int64(i)
				_, _ = r.Record(context.Background(), db, []Sample{s},
					t0.Add(time.Duration(i)*time.Minute))
			}
		}(g)
	}
	wg.Wait()
	if got := r.tracked(); got != 8 {
		t.Fatalf("tracking %d queryids, want 8", got)
	}
}
