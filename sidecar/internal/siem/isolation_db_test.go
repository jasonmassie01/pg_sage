package siem

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// chainSink fails, transiently, every batch holding an event of chain
// while refuse is set; it passes every other batch to memSink.
type chainSink struct {
	*memSink
	chain  string
	refuse atomic.Bool
}

func (c *chainSink) Send(ctx context.Context, events []Event) error {
	for _, e := range events {
		ext := e["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
		if ext["chain"] == c.chain && c.refuse.Load() {
			return errors.New("receiver 503 for " + c.chain)
		}
	}
	return c.memSink.Send(ctx, events)
}

// One failing chain of a source no longer holds back the chains after it
// (action_log sorts before auth_audit): auth_audit is delivered, only the
// action_log lane fails and keeps its cursor, and it catches up once the
// receiver takes it.
func TestFailingChainDoesNotStopLaterChains(t *testing.T) {
	pool := freshDB(t, "siem_lane_chain")
	ctx := context.Background()
	addActions(t, pool, 5)
	mustExec(t, pool, `INSERT INTO sage.auth_audit (event, detail)
		VALUES ('login_succeeded', '{}'), ('login_failed', '{}')`)
	sink := &chainSink{memSink: &memSink{name: "soc"}, chain: "action_log"}
	sink.refuse.Store(true)
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	err := e.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "chain action_log") {
		t.Fatalf("pass with a failing chain: err = %v, want one naming action_log", err)
	}
	got := unique(sink.delivered())
	if len(got) != 2 || got[0] != "auth_audit/1" || got[1] != "auth_audit/2" {
		t.Fatalf("the chain after the failing one delivered %v, want auth_audit 1-2", got)
	}
	st := e.Status()[0]
	bad := laneOf(t, st, "orders", "action_log")
	if bad.State != LaneFailing || bad.Cursor != 0 || bad.Head != 5 || bad.Lag != 5 ||
		!strings.Contains(bad.LastError, "503") {
		t.Fatalf("failing chain lane = %+v", bad)
	}
	if ok := laneOf(t, st, "orders", "auth_audit"); ok.State != LaneOK || ok.Lag != 0 ||
		ok.Cursor != 2 {
		t.Fatalf("healthy chain lane = %+v", ok)
	}
	sink.refuse.Store(false)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass after the receiver recovers: %v", err)
	}
	if n := len(unique(sink.delivered())); n != 7 {
		t.Fatalf("after recovery %d distinct events, want 7", n)
	}
	if l := laneOf(t, e.Status()[0], "orders", "action_log"); l.State != LaneOK ||
		l.Lag != 0 || l.ConsecutiveFailures != 0 {
		t.Fatalf("recovered lane = %+v", l)
	}
}

// A source that becomes unreadable keeps its regression on the sink's
// status and its lanes: continuing past it must not drop what is known
// of it until it can be read again.
func TestUnreadableSourceKeepsItsRegressions(t *testing.T) {
	pool := freshDB(t, "siem_lane_keepreg")
	dead := freshDB(t, "siem_lane_keepreg_dead")
	dead.Close()
	ctx := context.Background()
	var mu sync.Mutex
	srcs := []Source{{Name: "orders", DB: pool}}
	sink := &memSink{name: "soc"}
	e := NewExporter(func() []Source { mu.Lock(); defer mu.Unlock(); return srcs },
		[]SinkConfig{{Sink: sink}}, NewCursorStore(pool), Options{BatchSize: 7,
			Interval: 20 * time.Millisecond, MaxBackoff: 50 * time.Millisecond}, unfenced, nil)
	regress(t, pool, e)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("regressed pass: %v", err)
	}
	if st := e.Status()[0]; len(st.Regressions) != 1 {
		t.Fatalf("status before the source goes away = %+v", st)
	}
	mu.Lock()
	srcs = []Source{{Name: "orders", DB: dead}}
	mu.Unlock()
	if err := e.RunOnce(ctx); err == nil || !strings.Contains(err.Error(), "orders") {
		t.Fatalf("pass with the source unreadable: err = %v, want one naming orders", err)
	}
	st := e.Status()[0]
	if len(st.Regressions) != 1 || st.Regressions[0].Source != "orders" ||
		st.Regressions[0].Chain != "action_log" {
		t.Fatalf("an unreadable source dropped its regression: %+v", st)
	}
	if l := laneOf(t, st, "orders", "action_log"); l.State != LaneRegressed || l.Cursor != 10 {
		t.Fatalf("regressed lane of an unreadable source = %+v", l)
	}
	if l := laneOf(t, st, "orders", ""); l.State != LaneFailing ||
		!strings.Contains(l.LastError, "closed") {
		t.Fatalf("source lane = %+v", l)
	}
}
