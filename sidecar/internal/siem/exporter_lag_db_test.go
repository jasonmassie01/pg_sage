package siem

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
)

// audit-12: pg_sage_siem_lag_links is each lane's Lag, the chain head
// minus the sink's saved cursor at the last attempt. These pin it on a
// real chain and cursor store.

// quotaSink takes the first allow sends, then fails every send.
type quotaSink struct {
	*memSink
	allow atomic.Int64
}

func (q *quotaSink) Send(ctx context.Context, events []Event) error {
	if q.allow.Add(-1) < 0 {
		return errors.New("receiver 503")
	}
	return q.memSink.Send(ctx, events)
}

func savedCursor(t *testing.T, e *Exporter, sink, chain string) int64 {
	t.Helper()
	cur, err := e.cursors.LoadCursor(context.Background(), sink, "orders", chain)
	if err != nil {
		t.Fatalf("load cursor: %v", err)
	}
	return cur.Seq
}

// Boundary: a sink that took one batch (7 of 10 links) lags by exactly
// head minus its saved cursor; once caught up its lag is 0; new links
// show as lag at the next attempt that cannot deliver them.
func TestSIEMLagIsHeadMinusCursor(t *testing.T) {
	pool := freshDB(t, "siem_lag_head_cursor")
	ctx := context.Background()
	addActions(t, pool, 10)
	sink := &quotaSink{memSink: &memSink{name: "soc"}}
	sink.allow.Store(1)
	e := exporter(pool, unfenced, SinkConfig{Sink: sink, Chains: []string{"action_log"}})
	if err := e.RunOnce(ctx); err == nil {
		t.Fatal("pass with a failing second batch: no error")
	}
	head := chainHead(t, pool)
	cursor := savedCursor(t, e, "soc", "action_log")
	l := laneOf(t, e.Status()[0], "orders", "action_log")
	if cursor != 7 || head != 10 || l.Cursor != cursor || l.Head != head ||
		l.Lag != head-cursor || l.Lag != 3 {
		t.Fatalf("partial lane = %+v (saved cursor %d, head %d), want lag 3", l, cursor, head)
	}
	sink.allow.Store(1 << 30)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass after recovery: %v", err)
	}
	if l := laneOf(t, e.Status()[0], "orders", "action_log"); l.Lag != 0 || l.Cursor != 10 {
		t.Fatalf("caught-up lane = %+v, want lag 0 at 10", l)
	}
	addActions(t, pool, 4)
	sink.allow.Store(0)
	_ = e.RunOnce(ctx)
	if l := laneOf(t, e.Status()[0], "orders", "action_log"); l.Lag != 4 || l.Head != 14 {
		t.Fatalf("lane after 4 new links it could not send = %+v, want lag 4", l)
	}
}

// Concurrent access: status reads (a scrape) run while passes drain; the
// lag read never goes negative and ends at 0.
func TestSIEMLagReadDuringDrain(t *testing.T) {
	pool := freshDB(t, "siem_lag_concurrent")
	ctx := context.Background()
	addActions(t, pool, 30)
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	var wg sync.WaitGroup
	stop := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			for _, st := range e.Status() {
				for _, l := range st.Lanes {
					if l.Lag < 0 || l.Lag != max(l.Head-l.Cursor, 0) {
						t.Errorf("lane %+v: lag is not head minus cursor", l)
						return
					}
				}
			}
		}
	}()
	for i := 0; i < 3; i++ {
		if err := e.RunOnce(ctx); err != nil {
			t.Errorf("pass %d: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	for _, l := range e.Status()[0].Lanes {
		if l.Lag != 0 {
			t.Fatalf("lane %s/%s lag %d after draining, want 0", l.Source, l.Chain, l.Lag)
		}
	}
}
