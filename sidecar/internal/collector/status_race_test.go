package collector

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// collection-08: Status is read by the API and /metrics while the
// collector's goroutine runs cycles. Run with -race where cgo exists;
// without it the test still checks the counters add up.
func TestStatusConcurrentPollsDuringCycles(t *testing.T) {
	db := &fakeLoadDB{ratio: 0.99}
	c, _ := breakerCollector(t, db)
	const cycles = 200
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				st := c.Status()
				if st.Unavailable == nil || st.SkipsTotal == nil {
					t.Error("Status returned a nil map under concurrency")
					return
				}
				st.Unavailable["x"] = "caller-owned copy"
			}
		}()
	}
	for i := 0; i < cycles; i++ {
		if i%2 == 0 {
			db.set(0.99, nil)
		} else {
			db.set(0, errors.New("connection reset by peer"))
		}
		c.cycle(context.Background(), &fakeTicker{})
		c.notePersistFailure(errors.New("persist"))
		c.noteQueryStoreFailure(errors.New("query_store"))
	}
	close(stop)
	wg.Wait()
	st := c.Status()
	got := st.SkipsTotal[SkipReasonLoad] + st.SkipsTotal[SkipReasonLoadQueryError]
	if got != cycles || st.SkipsTotal[SkipReasonLoad] != cycles/2 {
		t.Errorf("SkipsTotal = %v, want %d of each reason", st.SkipsTotal, cycles/2)
	}
	if st.PersistFailures != cycles || st.QueryStoreFailures != cycles {
		t.Errorf("persist/query_store = %d/%d, want %d each", st.PersistFailures,
			st.QueryStoreFailures, cycles)
	}
	if st.BreakerState != BreakerDormant {
		t.Errorf("BreakerState = %q after %d skips, want dormant", st.BreakerState, cycles)
	}
}
