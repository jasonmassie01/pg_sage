package siem

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// receiver is an HTTP SIEM receiver that refuses (413) every batch holding
// a marked link, stubs included when stubsToo, and answers 503 while
// unavailable is above zero.
type receiver struct {
	mu          sync.Mutex
	poison      map[int64]bool
	stubsToo    bool
	unavailable atomic.Int64
	events      []map[string]any
	batches     int
}

func newReceiver(t *testing.T, poison ...int64) (*receiver, Sink) {
	t.Helper()
	r := &receiver{poison: map[int64]bool{}}
	for _, p := range poison {
		r.poison[p] = true
	}
	srv := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(srv.Close)
	return r, NewHTTPSink("soc", srv.URL, "", 5*time.Second, nil)
}

func (r *receiver) serve(w http.ResponseWriter, req *http.Request) {
	if r.unavailable.Load() > 0 {
		r.unavailable.Add(-1)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	var batch []map[string]any
	if err := json.NewDecoder(req.Body).Decode(&batch); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range batch {
		if r.poison[eventSeq(e)] && (!isStub(e) || r.stubsToo) {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
	}
	r.batches++
	r.events = append(r.events, batch...)
}

func eventSeq(e map[string]any) int64 {
	ext, _ := e["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
	f, _ := ext["seq"].(float64)
	return int64(f)
}

func isStub(e map[string]any) bool {
	md, _ := e["metadata"].(map[string]any)
	labels, _ := md["labels"].([]any)
	for _, l := range labels {
		if l == UndeliverableLabel {
			return true
		}
	}
	return false
}

// kinds maps each received seq to "event" or "stub" (the last received).
func (r *receiver) kinds() map[int64]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[int64]string{}
	for _, e := range r.events {
		k := "event"
		if isStub(e) {
			k = "stub"
		}
		out[eventSeq(e)] = k
	}
	return out
}

func (r *receiver) find(seq int64) map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, e := range r.events {
		if eventSeq(e) == seq {
			return e
		}
	}
	return nil
}

// hookLog records the undeliverable events reported, failing on demand.
type hookLog struct {
	mu   sync.Mutex
	got  []Undeliverable
	fail atomic.Int64
}

func (h *hookLog) record(_ context.Context, u Undeliverable) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.got = append(h.got, u)
	if h.fail.Load() > 0 {
		h.fail.Add(-1)
		return errors.New("findings table locked")
	}
	return nil
}

func (h *hookLog) seqs() []int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := []int64{}
	for _, u := range h.got {
		out = append(out, u.Seq)
	}
	return out
}

func poisonExporter(pool *pgxpool.Pool, sink Sink, batch int, hook *hookLog) *Exporter {
	opts := Options{BatchSize: batch, Interval: 10 * time.Millisecond,
		MaxBackoff: 20 * time.Millisecond}
	if hook != nil {
		opts.OnUndeliverable = hook.record
	}
	return NewExporter(func() []Source { return []Source{{Name: "orders", DB: pool}} },
		[]SinkConfig{{Sink: sink}}, NewCursorStore(pool), opts, unfenced, nil)
}

func chainHead(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var head int64
	if err := pool.QueryRow(context.Background(), `SELECT coalesce(max(seq), 0)
		FROM sage.audit_chain_link WHERE chain = 'action_log'`).Scan(&head); err != nil {
		t.Fatalf("chain head: %v", err)
	}
	return head
}

func laneOf(t *testing.T, st SinkStatus, source, chain string) LaneStatus {
	t.Helper()
	for _, l := range st.Lanes {
		if l.Source == source && l.Chain == chain {
			return l
		}
	}
	t.Fatalf("no lane %s/%s in %+v", source, chain, st.Lanes)
	return LaneStatus{}
}

// assertKinds checks every seq 1..n arrived, as a stub exactly when marked.
func assertKinds(t *testing.T, r *receiver, n int64, stubs ...int64) {
	t.Helper()
	want := map[int64]bool{}
	for _, s := range stubs {
		want[s] = true
	}
	got := r.kinds()
	for seq := int64(1); seq <= n; seq++ {
		k := "event"
		if want[seq] {
			k = "stub"
		}
		if got[seq] != k {
			t.Fatalf("seq %d arrived as %q, want %q (all: %v)", seq, got[seq], k, got)
		}
	}
	if len(got) != int(n) {
		t.Fatalf("received %d positions, want %d: %v", len(got), n, got)
	}
}

// A receiver that refuses (413) every batch holding a marked event gets
// every other event plus one stub per marked event, the cursor reaches the
// chain's head, and each stub is reported once (CHECK-24).
func TestPoisonEventIsStubbedAndCursorReachesHead(t *testing.T) {
	pool := freshDB(t, "siem_poison")
	ctx := context.Background()
	addActions(t, pool, 20)
	rcv, sink := newReceiver(t, 3, 12)
	hook := &hookLog{}
	e := poisonExporter(pool, sink, 7, hook)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass with poison events: %v", err)
	}
	assertKinds(t, rcv, 20, 3, 12)
	if seq, _, _ := cursorRow(t, pool, "soc"); seq != chainHead(t, pool) || seq != 20 {
		t.Fatalf("cursor = %d, want the head %d", seq, chainHead(t, pool))
	}
	stub := rcv.find(3)
	ext := stub["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
	if stub["status"] != "Failure" || ext["hash"] != linkHash(t, pool, 3) ||
		ext["row"] != nil || ext["op"] != "I" {
		t.Fatalf("stub of 3 = %v", stub)
	}
	if got := hook.seqs(); len(got) != 2 || got[0] != 3 || got[1] != 12 {
		t.Fatalf("reported undeliverable seqs = %v, want [3 12]", got)
	}
	u := hook.got[0]
	if u.Sink != "soc" || u.Source != "orders" || u.Chain != "action_log" ||
		u.Hash != linkHash(t, pool, 3) || !strings.Contains(u.Reason, "413") {
		t.Fatalf("undeliverable report = %+v", u)
	}
	st := e.Status()[0]
	l := laneOf(t, st, "orders", "action_log")
	if st.Status != StatusOK || st.Undeliverable != 2 || l.State != LaneOK ||
		l.Cursor != 20 || l.Head != 20 || l.Lag != 0 {
		t.Fatalf("status = %+v, lane %+v", st, l)
	}
	before := rcv.batches
	if err := e.RunOnce(ctx); err != nil || rcv.batches != before {
		t.Fatalf("a caught-up pass sent %d batches (%v)", rcv.batches-before, err)
	}
}

// Boundaries: batch size 1, a poison link at a batch's last and first
// position, a batch made only of poison links; no hook configured.
func TestPoisonBoundaries(t *testing.T) {
	for _, c := range []struct {
		name   string
		batch  int
		poison []int64
	}{{"batch_one", 1, []int64{4}}, {"last_of_batch", 7, []int64{7, 14}},
		{"first_of_batch", 7, []int64{1, 8}}, {"whole_batch", 4, []int64{5, 6, 7, 8}}} {
		t.Run(c.name, func(t *testing.T) {
			pool := freshDB(t, "siem_pb_"+c.name)
			addActions(t, pool, 14)
			rcv, sink := newReceiver(t, c.poison...)
			e := poisonExporter(pool, sink, c.batch, nil)
			if err := e.RunOnce(context.Background()); err != nil {
				t.Fatalf("pass: %v", err)
			}
			assertKinds(t, rcv, 14, c.poison...)
			if seq, _, _ := cursorRow(t, pool, "soc"); seq != 14 {
				t.Fatalf("cursor = %d, want 14", seq)
			}
			if st := e.Status()[0]; st.Undeliverable != int64(len(c.poison)) {
				t.Fatalf("undeliverable = %d, want %d", st.Undeliverable, len(c.poison))
			}
		})
	}
}

// A 503 is transient: the cursor holds, nothing is stubbed or reported,
// and the retry delivers everything.
func TestTransientRefusalHoldsTheCursor(t *testing.T) {
	pool := freshDB(t, "siem_transient")
	ctx := context.Background()
	addActions(t, pool, 10)
	rcv, sink := newReceiver(t)
	rcv.unavailable.Store(1)
	hook := &hookLog{}
	e := poisonExporter(pool, sink, 4, hook)
	err := e.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "503") || errors.Is(err, ErrPermanent) {
		t.Fatalf("pass against a 503: err = %v", err)
	}
	var n int
	_ = pool.QueryRow(ctx, "SELECT count(*) FROM sage.siem_cursor").Scan(&n)
	if n != 0 || len(rcv.kinds()) != 0 || len(hook.seqs()) != 0 {
		t.Fatalf("a 503 moved %d cursors, delivered %v, reported %v", n, rcv.kinds(),
			hook.seqs())
	}
	l := laneOf(t, e.Status()[0], "orders", "action_log")
	if l.State != LaneFailing || !strings.Contains(l.LastError, "503") || l.Lag != 10 ||
		l.ConsecutiveFailures != 1 {
		t.Fatalf("lane after a 503 = %+v", l)
	}
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertKinds(t, rcv, 10)
	l = laneOf(t, e.Status()[0], "orders", "action_log")
	if l.State != LaneOK || l.LastError != "" || l.Cursor != 10 || l.Lag != 0 {
		t.Fatalf("lane after the retry = %+v", l)
	}
}

// A refused stub poisons the lane: the cursor holds before the link, the
// lane says poisoned, later passes hold too; once the receiver takes the
// stub, export resumes.
func TestRefusedStubPoisonsTheLane(t *testing.T) {
	pool := freshDB(t, "siem_poisoned")
	ctx := context.Background()
	addActions(t, pool, 10)
	rcv, sink := newReceiver(t, 6)
	rcv.stubsToo = true
	hook := &hookLog{}
	e := poisonExporter(pool, sink, 4, hook)
	for pass := 1; pass <= 2; pass++ {
		err := e.RunOnce(ctx)
		if !errors.Is(err, errPoisoned) || !strings.Contains(err.Error(), "6") {
			t.Fatalf("pass %d: err = %v, want the lane poisoned at 6", pass, err)
		}
		if seq, _, _ := cursorRow(t, pool, "soc"); seq != 5 {
			t.Fatalf("pass %d: cursor = %d, want 5", pass, seq)
		}
		l := laneOf(t, e.Status()[0], "orders", "action_log")
		if l.State != LanePoisoned || l.ConsecutiveFailures != pass || l.Lag != 5 {
			t.Fatalf("pass %d: lane = %+v", pass, l)
		}
	}
	if k := rcv.kinds(); len(k) != 5 || len(hook.seqs()) != 0 {
		t.Fatalf("poisoned lane delivered %v, reported %v", k, hook.seqs())
	}
	rcv.mu.Lock()
	rcv.stubsToo = false
	rcv.mu.Unlock()
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass after the receiver takes stubs: %v", err)
	}
	assertKinds(t, rcv, 10, 6)
	if l := laneOf(t, e.Status()[0], "orders", "action_log"); l.State != LaneOK {
		t.Fatalf("lane after recovery = %+v", l)
	}
}

// A stub whose report fails is not final: the cursor holds before it and
// the next pass re-sends and reports it (at least once).
func TestUndeliverableReportFailureHoldsTheCursor(t *testing.T) {
	pool := freshDB(t, "siem_hook_fail")
	ctx := context.Background()
	addActions(t, pool, 5)
	rcv, sink := newReceiver(t, 3)
	hook := &hookLog{}
	hook.fail.Store(1)
	e := poisonExporter(pool, sink, 5, hook)
	err := e.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "findings table locked") {
		t.Fatalf("pass with a failing report: err = %v", err)
	}
	if seq, _, _ := cursorRow(t, pool, "soc"); seq != 2 {
		t.Fatalf("cursor = %d, want 2 (before the unreported stub)", seq)
	}
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("retry: %v", err)
	}
	assertKinds(t, rcv, 5, 3)
	if seq, _, _ := cursorRow(t, pool, "soc"); seq != 5 {
		t.Fatalf("cursor after the retry = %d, want 5", seq)
	}
	if got := hook.seqs(); len(got) != 2 || got[0] != 3 || got[1] != 3 {
		t.Fatalf("reports = %v, want 3 twice", got)
	}
}
