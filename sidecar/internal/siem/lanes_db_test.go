package siem

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/pg-sage/sidecar/internal/auditchain"
)

// countingQuerier counts the statements sent to a source.
type countingQuerier struct {
	auditchain.Querier
	calls atomic.Int64
}

func (c *countingQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows,
	error) {
	c.calls.Add(1)
	return c.Querier.Query(ctx, sql, args...)
}

func (c *countingQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.calls.Add(1)
	return c.Querier.QueryRow(ctx, sql, args...)
}

// bySource returns "source/chain/seq" of each event a memSink received.
func bySource(m *memSink) map[string]bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]bool{}
	for _, e := range m.events {
		ext := e["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
		out[fmt.Sprintf("%s/%s/%d", ext["source"], ext["chain"], ext["seq"])] = true
	}
	return out
}

// One unreachable source no longer stops export of the sources after it:
// the second is delivered, the first's lane shows its error, and a source
// removed from the fleet drops its lane (CHECK-23).
func TestFailedSourceDoesNotStopOtherSources(t *testing.T) {
	pool := freshDB(t, "siem_lane_src")
	dead := freshDB(t, "siem_lane_dead")
	dead.Close()
	ctx := context.Background()
	addActions(t, pool, 9)
	sink := &memSink{name: "soc"}
	var mu sync.Mutex
	srcs := []Source{{Name: "billing", DB: dead}, {Name: "orders", DB: pool}}
	e := NewExporter(func() []Source { mu.Lock(); defer mu.Unlock(); return srcs },
		[]SinkConfig{{Sink: sink}}, NewCursorStore(pool), Options{BatchSize: 4}, unfenced, nil)
	err := e.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "billing") {
		t.Fatalf("pass with a dead source: err = %v, want one naming billing", err)
	}
	if got := unique(sink.delivered()); len(got) != 9 {
		t.Fatalf("the source after the dead one delivered %d of 9: %v", len(got), got)
	}
	st := e.Status()[0]
	bad := laneOf(t, st, "billing", "")
	if bad.State != LaneFailing || !strings.Contains(bad.LastError, "closed") ||
		bad.ConsecutiveFailures != 1 {
		t.Fatalf("dead source lane = %+v", bad)
	}
	good := laneOf(t, st, "orders", "action_log")
	if good.State != LaneOK || good.Cursor != 9 || good.Head != 9 || good.Lag != 0 {
		t.Fatalf("live source lane = %+v", good)
	}
	if st.Status != StatusFailing || st.ConsecutiveFailures != 1 ||
		!strings.Contains(st.LastError, "billing") {
		t.Fatalf("sink status = %+v", st)
	}
	mu.Lock()
	srcs = srcs[1:]
	mu.Unlock()
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass without the dead source: %v", err)
	}
	st = e.Status()[0]
	if st.Status != StatusOK || st.ConsecutiveFailures != 0 || len(st.Lanes) == 0 {
		t.Fatalf("status after removing the dead source = %+v", st)
	}
	for _, l := range st.Lanes {
		if l.Source != "orders" {
			t.Fatalf("a removed source kept its lane: %+v", st.Lanes)
		}
	}
}

// No sources is a clean, empty pass.
func TestNoSourcesIsAnEmptyPass(t *testing.T) {
	pool := freshDB(t, "siem_lane_none")
	sink := &memSink{name: "soc"}
	e := NewExporter(func() []Source { return nil }, []SinkConfig{{Sink: sink}},
		NewCursorStore(pool), Options{}, unfenced, nil)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("empty pass: %v", err)
	}
	st := e.Status()[0]
	if st.Status != StatusOK || st.Lanes == nil || len(st.Lanes) != 0 || sink.sends.Load() != 0 {
		t.Fatalf("empty pass status = %+v, sends %d", st, sink.sends.Load())
	}
}

// maxAttempts is the most attempts a lane backing off per e.delay can make
// in elapsed.
func maxAttempts(e *Exporter, elapsed time.Duration) int64 {
	n, at := int64(1), time.Duration(0)
	for {
		at += e.delay(int(n))
		if at > elapsed {
			return n
		}
		n++
	}
}

// Backoff is per lane: with two sinks and three sources, one dead, the
// dead lane is retried no more often than its backoff allows while the
// live sources keep flowing every interval on both sinks, and Status is
// safe to read concurrently.
func TestPerLaneBackoffTwoSinksThreeSources(t *testing.T) {
	a, b := freshDB(t, "siem_lane_a"), freshDB(t, "siem_lane_b")
	dead := freshDB(t, "siem_lane_gone")
	dead.Close()
	addActions(t, a, 6)
	addActions(t, b, 6)
	deadQ := &countingQuerier{Querier: dead}
	srcs := []Source{{Name: "a", DB: a}, {Name: "dead", DB: deadQ}, {Name: "b", DB: b}}
	one, two := &memSink{name: "one"}, &memSink{name: "two"}
	e := NewExporter(func() []Source { return srcs },
		[]SinkConfig{{Sink: one}, {Sink: two}}, NewCursorStore(a), Options{BatchSize: 4,
			Interval: 5 * time.Millisecond, MaxBackoff: 5 * time.Second}, unfenced, nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	start := time.Now()
	go func() { e.Run(ctx); close(done) }()
	var readers sync.WaitGroup
	readers.Add(1)
	go func() {
		defer readers.Done()
		for ctx.Err() == nil {
			_ = e.Status()
			time.Sleep(time.Millisecond)
		}
	}()
	waitDelivered(t, []*memSink{one, two}, 12, 5*time.Second)
	time.Sleep(time.Second) // the dead lane backs off past a second
	addActions(t, a, 2)
	addActions(t, b, 2)
	waitDelivered(t, []*memSink{one, two}, 16, time.Second)
	d := time.Since(start)
	limit := 2 * maxAttempts(e, d)
	if n := deadQ.calls.Load(); n > limit || n < 2 {
		t.Fatalf("dead source tried %d times in %v, want 2..%d", n, d, limit)
	}
	cancel()
	<-done
	readers.Wait()
	for _, st := range e.Status() {
		l := laneOf(t, st, "dead", "")
		if l.State != LaneFailing || l.ConsecutiveFailures < 2 {
			t.Fatalf("%s: dead lane = %+v", st.Name, l)
		}
	}
}

// waitDelivered waits until every sink holds n distinct events, or fails
// after within: live lanes run every interval, never at a dead lane's
// backoff.
func waitDelivered(t *testing.T, sinks []*memSink, n int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		ok := true
		for _, s := range sinks {
			if len(bySource(s)) < n {
				ok = false
			}
		}
		if ok {
			return
		}
		if time.Now().After(deadline) {
			for _, s := range sinks {
				t.Logf("%s: %v", s.name, bySource(s))
			}
			t.Fatalf("live sources were not delivered within %v (want %d each)", within, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
