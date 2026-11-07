package siem

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Re-anchoring refuses what it cannot do safely, each distinguishably,
// and changes nothing when it refuses.
func TestReanchorRefusals(t *testing.T) {
	pool := freshDB(t, "siem_reanchor_refuse")
	ctx := context.Background()
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	req := func(sink, source, chain string, from int64) ReanchorRequest {
		return ReanchorRequest{Sink: sink, Source: source, Chain: chain, FromSeq: from}
	}
	if _, err := e.Reanchor(ctx, req("soc", "orders", "action_log", 1)); !errors.Is(err,
		ErrNoCursor) {
		t.Fatalf("no cursor yet: err = %v, want ErrNoCursor", err)
	}
	addActions(t, pool, 10)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	restoreTo(t, pool, 7)
	cases := []struct {
		name string
		req  ReanchorRequest
		want error
	}{
		{"unknown sink", req("nope", "orders", "action_log", 1), ErrUnknownSink},
		{"unknown source", req("soc", "nope", "action_log", 1), ErrUnknownSource},
		{"unknown chain", req("soc", "orders", "nope", 1), ErrInvalidReanchor},
		{"zero from_seq", req("soc", "orders", "action_log", 0), ErrInvalidReanchor},
		{"negative from_seq", req("soc", "orders", "action_log", -3), ErrInvalidReanchor},
		{"skips unexported", req("soc", "orders", "action_log", 12), ErrInvalidReanchor},
		{"anchor link missing", req("soc", "orders", "action_log", 10), ErrInvalidReanchor},
		{"chain not exported", req("soc", "orders", "config_audit", 1), ErrNoCursor},
	}
	for _, tc := range cases {
		if _, err := e.Reanchor(ctx, tc.req); !errors.Is(err, tc.want) {
			t.Fatalf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	assertLeaderOnlyReanchor(t, pool, sink, req("soc", "orders", "action_log", 8))
}

// assertLeaderOnlyReanchor: a follower and a leader that lost its lease
// cannot re-anchor, and the cursor stays where it was.
func assertLeaderOnlyReanchor(t *testing.T, pool *pgxpool.Pool, sink *memSink,
	req ReanchorRequest) {
	t.Helper()
	ctx := context.Background()
	follower := exporter(pool, func() (Fence, bool) { return Fence{}, false },
		SinkConfig{Sink: sink})
	if _, err := follower.Reanchor(ctx, req); !errors.Is(err, ErrNotLeader) {
		t.Fatalf("follower: err = %v, want ErrNotLeader", err)
	}
	stale := exporter(pool, func() (Fence, bool) {
		return Fence{Scope: "standalone:x", Holder: "me", Epoch: 4}, true
	}, SinkConfig{Sink: sink})
	if _, err := stale.Reanchor(ctx, req); !errors.Is(err, ErrFenced) {
		t.Fatalf("lost lease: err = %v, want ErrFenced", err)
	}
	if seq, _, forkTo := cursorRow(t, pool, "soc"); seq != 10 || forkTo != 0 {
		t.Fatalf("a refused reanchor changed the cursor: %d fork_to %d", seq, forkTo)
	}
}

// Re-anchoring at 1 re-exports the whole chain, the positions already
// exported labelled as a fork.
func TestReanchorFromGenesis(t *testing.T) {
	pool := freshDB(t, "siem_reanchor_genesis")
	ctx := context.Background()
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	regress(t, pool, e)
	res, err := e.Reanchor(ctx, ReanchorRequest{Sink: "soc", Source: "orders",
		Chain: "action_log", FromSeq: 1})
	if err != nil || res.PreviousSeq != 10 || res.ForkTo != 10 {
		t.Fatalf("reanchor = %+v, %v", res, err)
	}
	if seq, hash, forkTo := cursorRow(t, pool, "soc"); seq != 0 || hash != "" ||
		forkTo != 10 {
		t.Fatalf("cursor = %d %q fork_to %d, want 0, no hash, 10", seq, hash, forkTo)
	}
	before := len(sink.exported())
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	got := since(sink.exported(), before)
	if len(got) != 10 {
		t.Fatalf("re-exported %d events, want 10", len(got))
	}
	for i, ev := range got {
		if ev.seq != int64(i+1) || !isFork(ev.labels) {
			t.Fatalf("event %d = %+v, want seq %d labelled fork", i, ev, i+1)
		}
	}
}

// The cursor store: hashes round-trip, Save never moves back (seq nor
// hash), Reanchor is the only way back and keeps the furthest fork, and a
// backfill fills only a missing hash.
func TestCursorStoreAnchors(t *testing.T) {
	pool := freshDB(t, "siem_cursor_anchor")
	ctx := context.Background()
	c := NewCursorStore(pool)
	if cur, err := c.LoadCursor(ctx, "soc", "orders", "action_log"); err != nil ||
		cur.Seq != 0 || cur.Hash != "" || cur.ForkTo != 0 {
		t.Fatalf("missing cursor = %+v, %v", cur, err)
	}
	if err := c.Save(ctx, Fence{}, "soc", "orders", "action_log", 9, "h9"); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := c.Save(ctx, Fence{}, "soc", "orders", "action_log", 4, "h4"); err != nil {
		t.Fatalf("stale save: %v", err)
	}
	cur, err := c.LoadCursor(ctx, "soc", "orders", "action_log")
	if err != nil || cur.Seq != 9 || cur.Hash != "h9" {
		t.Fatalf("after a stale save = %+v, %v, want 9/h9", cur, err)
	}
	prev, err := c.Reanchor(ctx, Fence{}, "soc", "orders", "action_log", 3, "h3")
	if err != nil || prev.Seq != 9 || prev.Hash != "h9" {
		t.Fatalf("reanchor prev = %+v, %v", prev, err)
	}
	if err := c.Save(ctx, Fence{}, "soc", "orders", "action_log", 5, "h5"); err != nil {
		t.Fatalf("save after reanchor: %v", err)
	}
	if _, err := c.Reanchor(ctx, Fence{}, "soc", "orders", "action_log", 1,
		"h1"); err != nil {
		t.Fatalf("second reanchor: %v", err)
	}
	cur, _ = c.LoadCursor(ctx, "soc", "orders", "action_log")
	if cur.Seq != 1 || cur.Hash != "h1" || cur.ForkTo != 9 {
		t.Fatalf("after two reanchors = %+v, want 1/h1 fork_to 9", cur)
	}
	assertCursorReanchorRefusals(t, c)
	assertBackfill(t, c)
}

// assertCursorReanchorRefusals: with the cursor at 1/h1, the store
// refuses a forward move, another link at the cursor, a missing cursor
// and a lost lease, and accepts the cursor's own link.
func assertCursorReanchorRefusals(t *testing.T, c *CursorStore) {
	t.Helper()
	ctx := context.Background()
	if _, err := c.Reanchor(ctx, Fence{}, "soc", "orders", "action_log", 2,
		"h2"); !errors.Is(err, ErrInvalidReanchor) {
		t.Fatalf("forward reanchor: err = %v, want ErrInvalidReanchor", err)
	}
	if _, err := c.Reanchor(ctx, Fence{}, "soc", "orders", "action_log", 1,
		"reused"); !errors.Is(err, ErrInvalidReanchor) {
		t.Fatalf("reanchor onto another link at the cursor: err = %v", err)
	}
	if _, err := c.Reanchor(ctx, Fence{}, "soc", "orders", "action_log", 1,
		"h1"); err != nil {
		t.Fatalf("reanchor onto the cursor's own link: %v", err)
	}
	if _, err := c.Reanchor(ctx, Fence{}, "soc", "orders", "nope", 0,
		""); !errors.Is(err, ErrNoCursor) {
		t.Fatalf("reanchor without a cursor: err = %v", err)
	}
	if _, err := c.Reanchor(ctx, Fence{Scope: "s", Holder: "h", Epoch: 1}, "soc",
		"orders", "action_log", 0, ""); !errors.Is(err, ErrFenced) {
		t.Fatalf("unleased reanchor: err = %v", err)
	}
}

func assertBackfill(t *testing.T, c *CursorStore) {
	t.Helper()
	ctx := context.Background()
	mustExec(t, c.pool, "UPDATE sage.siem_cursor SET hash = NULL")
	if err := c.BackfillHash(ctx, Fence{}, "soc", "orders", "action_log", 7,
		"x7"); err != nil {
		t.Fatalf("backfill at another position: %v", err)
	}
	if cur, _ := c.LoadCursor(ctx, "soc", "orders", "action_log"); cur.Hash != "" {
		t.Fatalf("a backfill for another position wrote %q", cur.Hash)
	}
	if err := c.BackfillHash(ctx, Fence{}, "soc", "orders", "action_log", 1,
		"x1"); err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if err := c.BackfillHash(ctx, Fence{}, "soc", "orders", "action_log", 1,
		"other"); err != nil {
		t.Fatalf("second backfill: %v", err)
	}
	if cur, _ := c.LoadCursor(ctx, "soc", "orders", "action_log"); cur.Hash != "x1" {
		t.Fatalf("backfilled hash = %q, want x1 (only a missing hash is filled)", cur.Hash)
	}
	if err := c.BackfillHash(ctx, Fence{Scope: "s", Holder: "h", Epoch: 1}, "soc",
		"orders", "action_log", 1, "x"); !errors.Is(err, ErrFenced) {
		t.Fatalf("unleased backfill: err = %v", err)
	}
	if err := NewCursorStore(nil).BackfillHash(ctx, Fence{}, "a", "b", "c", 1,
		"x"); err == nil {
		t.Fatalf("nil pool backfill succeeded")
	}
	if _, err := NewCursorStore(nil).Reanchor(ctx, Fence{}, "a", "b", "c", 0,
		""); err == nil {
		t.Fatalf("nil pool reanchor succeeded")
	}
}

// Cursors lists each sink's cursor for one chain of one source, leaving
// out sinks that do not receive the chain.
func TestExporterCursors(t *testing.T) {
	pool := freshDB(t, "siem_cursors")
	ctx := context.Background()
	soc := &memSink{name: "soc"}
	authOnly := &memSink{name: "auth"}
	e := exporter(pool, unfenced, SinkConfig{Sink: soc},
		SinkConfig{Sink: authOnly, Chains: []string{"auth_audit"}})
	if got, err := e.Cursors(ctx, "orders", "action_log"); err != nil || len(got) != 1 ||
		got[0].Seq != 0 || got[0].Sink != "soc" {
		t.Fatalf("before export: %+v, %v", got, err)
	}
	addActions(t, pool, 3)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	got, err := e.Cursors(ctx, "orders", "action_log")
	if err != nil || len(got) != 1 || got[0].Seq != 3 || got[0].Hash != linkHash(t, pool, 3) {
		t.Fatalf("cursors = %+v, %v", got, err)
	}
	if got[0].Source != "orders" || got[0].Chain != "action_log" {
		t.Fatalf("cursor names = %+v", got[0])
	}
	var nilExporter *Exporter
	if got, err := nilExporter.Cursors(ctx, "orders", "action_log"); err != nil ||
		len(got) != 0 {
		t.Fatalf("nil exporter: %+v, %v", got, err)
	}
}
