package lakebaseobs

import (
	"context"
	"sync"
	"testing"
	"time"
)

// Concurrent access on real PostgreSQL: Status and Deliveries readers while
// Poll runs see statement counters that only grow, and every read Poll made
// is counted once.
func TestDBStatusWhilePollRunsCountsEveryStatement(t *testing.T) {
	o := testOptions()
	o.Streams = []Stream{Plans, Logs}
	var mu sync.Mutex
	calls := 0
	reader := readerFunc(func(context.Context, Stream, Scope, Window) (Batch, error) {
		mu.Lock()
		calls++
		mu.Unlock()
		return Batch{Bytes: 7}, nil
	})
	rt, _ := runtimeStore(t, reader, o)
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		watchStatements(t, rt, done)
	}()
	now := time.Now().UTC()
	for i := range 5 {
		if err := rt.Poll(context.Background(), now.Add(time.Duration(i)*time.Minute)); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}
	close(done)
	wg.Wait()
	var statements, bytes int64
	for _, st := range rt.Status() {
		statements += st.Statements
		bytes += st.Bytes
	}
	mu.Lock()
	defer mu.Unlock()
	if calls == 0 || statements != int64(calls) || bytes != 7*int64(calls) {
		t.Fatalf("statements=%d bytes=%d; want %d reads counted", statements, bytes, calls)
	}
}

// watchStatements reads Status and Deliveries until done, failing if any
// stream's statement count goes backwards.
func watchStatements(t *testing.T, rt *Runtime, done <-chan struct{}) {
	last := map[Stream]int64{}
	for {
		select {
		case <-done:
			return
		default:
		}
		_ = rt.Deliveries()
		for _, st := range rt.Status() {
			if st.Statements < last[st.Stream] {
				t.Errorf("%s statements went back to %d", st.Stream, st.Statements)
			}
			last[st.Stream] = st.Statements
		}
	}
}
