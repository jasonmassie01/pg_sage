package siem

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

func TestMain(m *testing.M) {
	os.Exit(testdb.Run(m.Run, "internal/siem"))
}

func freshDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	dsn := testdb.CreateDatabase(t, label)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 12
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func addActions(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	mustExec(t, pool, `INSERT INTO sage.action_log (action_type, sql_executed, outcome)
		SELECT 'create_index', 'CREATE INDEX i' || g || ' ON t (a)', 'success'
		FROM generate_series(1, $1) g`, n)
}

// memSink records delivered events and can fail on demand.
type memSink struct {
	name     string
	mu       sync.Mutex
	events   []Event
	failNext atomic.Int64 // fail this many sends
	down     atomic.Bool  // fail every send
	block    chan struct{}
	sends    atomic.Int64
}

func (m *memSink) Name() string { return m.name }

func (m *memSink) Send(ctx context.Context, events []Event) error {
	m.sends.Add(1)
	if m.block != nil {
		select {
		case <-m.block:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.down.Load() {
		return errors.New("receiver down")
	}
	if m.failNext.Load() > 0 {
		m.failNext.Add(-1)
		return errors.New("receiver 503")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, events...)
	return nil
}

// delivered returns "chain/seq" of each delivered event, in order.
func (m *memSink) delivered() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.events))
	for _, e := range m.events {
		ext := e["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
		out = append(out, fmt.Sprintf("%s/%d", ext["chain"], ext["seq"]))
	}
	return out
}

func exporter(pool *pgxpool.Pool, allow func() (Fence, bool), sinks ...SinkConfig) *Exporter {
	return NewExporter(func() []Source { return []Source{{Name: "orders", DB: pool}} },
		sinks, NewCursorStore(pool), Options{BatchSize: 7, Interval: 20 * time.Millisecond,
			MaxBackoff: 50 * time.Millisecond}, allow, nil)
}

func unfenced() (Fence, bool) { return Fence{}, true }

// One pass delivers every link of every installed chain in chain order and
// moves the cursor to the head; the next pass delivers nothing new.
func TestExportDeliversEveryLinkOnce(t *testing.T) {
	pool := freshDB(t, "siem_once")
	addActions(t, pool, 20)
	mustExec(t, pool, "UPDATE sage.action_log SET outcome = 'rolled_back' WHERE id <= 3")
	mustExec(t, pool, `INSERT INTO sage.auth_audit (event, detail)
		VALUES ('login_succeeded', '{}')`)
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	got := sink.delivered()
	if len(got) != 24 { // 20 inserts + 3 outcome updates + 1 sign-in
		t.Fatalf("delivered %d events: %v", len(got), got)
	}
	assertChainOrder(t, got)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if n := len(sink.delivered()); n != 24 {
		t.Fatalf("second pass re-delivered: %d events", n)
	}
	var seq int64
	err := pool.QueryRow(context.Background(), `SELECT seq FROM sage.siem_cursor
		WHERE sink = 'soc' AND source = 'orders' AND chain = 'action_log'`).Scan(&seq)
	if err != nil || seq != 23 {
		t.Fatalf("cursor = %d (%v), want 23", seq, err)
	}
}

// A failing sink holds its cursor (backpressure); once it recovers every
// event arrives, none lost, at least once.
func TestFailingSinkLosesNothing(t *testing.T) {
	pool := freshDB(t, "siem_retry")
	addActions(t, pool, 15)
	sink := &memSink{name: "soc"}
	sink.failNext.Store(2)
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	ctx := context.Background()
	if err := e.RunOnce(ctx); err == nil {
		t.Fatalf("a failing pass reported success")
	}
	st := e.Status()[0]
	if st.ConsecutiveFailures != 1 || st.LastError == "" || st.Delivered != 0 {
		t.Fatalf("status after a failure = %+v", st)
	}
	for i := 0; i < 3; i++ {
		_ = e.RunOnce(ctx)
	}
	got := sink.delivered()
	if len(unique(got)) != 15 {
		t.Fatalf("after recovery: %d distinct of 15 (%v)", len(unique(got)), got)
	}
	st = e.Status()[0]
	if st.ConsecutiveFailures != 0 || st.Delivered < 15 || st.LastSuccess.IsZero() {
		t.Fatalf("status after recovery = %+v", st)
	}
}

// Sinks are independent: one down does not hold the other back, and a
// sink's chain filter limits what it receives.
func TestSinksAreIndependentAndFiltered(t *testing.T) {
	pool := freshDB(t, "siem_indep")
	addActions(t, pool, 5)
	mustExec(t, pool, `INSERT INTO sage.auth_audit (event, detail)
		VALUES ('login_failed', '{}')`)
	down := &memSink{name: "down"}
	down.down.Store(true)
	authOnly := &memSink{name: "auth"}
	e := exporter(pool, unfenced, SinkConfig{Sink: down},
		SinkConfig{Sink: authOnly, Chains: []string{"auth_audit"}})
	_ = e.RunOnce(context.Background())
	got := authOnly.delivered()
	if len(got) != 1 || got[0] != "auth_audit/1" {
		t.Fatalf("filtered sink got %v", got)
	}
	if len(down.delivered()) != 0 {
		t.Fatalf("a down sink recorded deliveries")
	}
}

// A follower sends nothing; a leader that lost its lease mid-pass cannot
// move the cursor.
func TestLeaderGateAndFence(t *testing.T) {
	pool := freshDB(t, "siem_fence")
	addActions(t, pool, 3)
	sink := &memSink{name: "soc"}
	follower := exporter(pool, func() (Fence, bool) { return Fence{}, false },
		SinkConfig{Sink: sink})
	if err := follower.RunOnce(context.Background()); err != nil {
		t.Fatalf("follower pass: %v", err)
	}
	if sink.sends.Load() != 0 {
		t.Fatalf("a follower sent %d batches", sink.sends.Load())
	}
	stale := exporter(pool, func() (Fence, bool) {
		return Fence{Scope: "standalone:x", Holder: "me", Epoch: 4}, true
	}, SinkConfig{Sink: sink})
	err := stale.RunOnce(context.Background())
	if !errors.Is(err, ErrFenced) {
		t.Fatalf("stale leader: err = %v, want ErrFenced", err)
	}
	var n int
	_ = pool.QueryRow(context.Background(), "SELECT count(*) FROM sage.siem_cursor").Scan(&n)
	if n != 0 {
		t.Fatalf("a fenced pass wrote %d cursors", n)
	}
	mustExec(t, pool, `INSERT INTO sage.fleet_leader_lease (scope, holder, epoch,
		expires_at) VALUES ('standalone:x', 'me', 4, now() + interval '1 minute')`)
	if err := stale.RunOnce(context.Background()); err != nil {
		t.Fatalf("leader with a live lease: %v", err)
	}
}

// Writers never wait on the exporter: with the sink hung, audit writes
// commit promptly, and cancelling the exporter returns promptly.
func TestExportNeverBlocksWriters(t *testing.T) {
	pool := freshDB(t, "siem_block")
	addActions(t, pool, 2)
	sink := &memSink{name: "soc", block: make(chan struct{})}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	deadline := time.Now().Add(2 * time.Second)
	for sink.sends.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	start := time.Now()
	addActions(t, pool, 50)
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("audit writes took %v while the sink hung", d)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("exporter did not stop after cancel")
	}
	close(sink.block)
}

// Concurrent audit writes during export: everything committed is delivered
// exactly in chain order, nothing skipped.
func TestConcurrentWritesDuringExport(t *testing.T) {
	pool := freshDB(t, "siem_concurrent")
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 10; i++ {
				_, _ = pool.Exec(context.Background(), `INSERT INTO sage.action_log
					(action_type, sql_executed) VALUES ('vacuum', 'VACUUM t')`)
			}
		}()
	}
	wg.Wait()
	deadline := time.Now().Add(5 * time.Second)
	for len(sink.delivered()) < 40 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	got := sink.delivered()
	if len(got) != 40 {
		t.Fatalf("delivered %d of 40", len(got))
	}
	assertChainOrder(t, got)
}

// The cursor store: a missing cursor reads 0, saves round-trip, and the
// fence refuses a save without the lease.
func TestCursorStore(t *testing.T) {
	pool := freshDB(t, "siem_cursor")
	ctx := context.Background()
	c := NewCursorStore(pool)
	if seq, err := c.Load(ctx, "soc", "orders", "action_log"); err != nil || seq != 0 {
		t.Fatalf("missing cursor = %d, %v", seq, err)
	}
	if err := c.Save(ctx, Fence{}, "soc", "orders", "action_log", 42); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := c.Save(ctx, Fence{}, "soc", "orders", "action_log", 43); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if seq, err := c.Load(ctx, "soc", "orders", "action_log"); err != nil || seq != 43 {
		t.Fatalf("cursor = %d, %v, want 43", seq, err)
	}
	if err := c.Save(ctx, Fence{Scope: "s", Holder: "h", Epoch: 1}, "soc", "orders",
		"action_log", 99); !errors.Is(err, ErrFenced) {
		t.Fatalf("unleased save: err = %v", err)
	}
	if err := NewCursorStore(nil).Save(ctx, Fence{}, "a", "b", "c", 1); err == nil {
		t.Fatalf("nil pool save succeeded")
	}
}

func assertChainOrder(t *testing.T, got []string) {
	t.Helper()
	last := map[string]int64{}
	for _, g := range got {
		var chain string
		var seq int64
		for i := len(g) - 1; i >= 0; i-- {
			if g[i] == '/' {
				chain = g[:i]
				_, _ = fmt.Sscan(g[i+1:], &seq)
				break
			}
		}
		if seq != last[chain]+1 {
			t.Fatalf("chain %s: seq %d after %d (order or gap): %v", chain, seq,
				last[chain], got)
		}
		last[chain] = seq
	}
}

func unique(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}
