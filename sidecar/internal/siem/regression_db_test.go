package siem

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// restoreTo simulates restoring the monitored database from a backup or
// PITR: the action_log links after keep and the rows they inserted vanish
// together, so the chain itself still verifies.
func restoreTo(t *testing.T, pool *pgxpool.Pool, keep int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	for _, sql := range []string{"SET LOCAL session_replication_role = replica",
		"DELETE FROM sage.action_log WHERE chain_seq > $1",
		"DELETE FROM sage.audit_chain_link WHERE chain = 'action_log' AND seq > $1"} {
		args := []any{keep}
		if !strings.Contains(sql, "$1") {
			args = nil
		}
		if _, err := tx.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit restore: %v", err)
	}
}

func linkHash(t *testing.T, pool *pgxpool.Pool, seq int64) string {
	t.Helper()
	var h string
	err := pool.QueryRow(context.Background(), `SELECT hash FROM sage.audit_chain_link
		WHERE chain = 'action_log' AND seq = $1`, seq).Scan(&h)
	if err != nil {
		t.Fatalf("link %d: %v", seq, err)
	}
	return h
}

// cursorRow reads a sink's action_log cursor: seq, hash ("" when NULL),
// fork_to (0 when NULL).
func cursorRow(t *testing.T, pool *pgxpool.Pool, sink string) (int64, string, int64) {
	t.Helper()
	var seq, forkTo int64
	var hash string
	err := pool.QueryRow(context.Background(), `SELECT seq, coalesce(hash, ''),
		coalesce(fork_to, 0) FROM sage.siem_cursor WHERE sink = $1 AND source = 'orders'
		AND chain = 'action_log'`, sink).Scan(&seq, &hash, &forkTo)
	if err != nil {
		t.Fatalf("cursor %s: %v", sink, err)
	}
	return seq, hash, forkTo
}

// exported is one delivered event's chain, position, hash and labels.
type exported struct {
	chain  string
	seq    int64
	hash   string
	labels []string
}

func (m *memSink) exported() []exported {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]exported, 0, len(m.events))
	for _, e := range m.events {
		ext := e["unmapped"].(map[string]any)["pg_sage"].(map[string]any)
		md := e["metadata"].(map[string]any)
		labels, _ := md["labels"].([]string)
		out = append(out, exported{chain: ext["chain"].(string), seq: ext["seq"].(int64),
			hash: ext["hash"].(string), labels: labels})
	}
	return out
}

func since(all []exported, n int) []exported { return append([]exported{}, all[n:]...) }

func ofChain(all []exported, chain string) []exported {
	var out []exported
	for _, e := range all {
		if e.chain == chain {
			out = append(out, e)
		}
	}
	return out
}

func isFork(labels []string) bool { return len(labels) == 1 && labels[0] == "fork" }

// regress exports 10 action_log links to sink, restores the chain to 7
// and appends 3 links, which reuse positions 8-10.
func regress(t *testing.T, pool *pgxpool.Pool, e *Exporter) {
	t.Helper()
	addActions(t, pool, 10)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	restoreTo(t, pool, 7)
	addActions(t, pool, 3)
}

// A chain that went back behind the cursor stops for that sink, marked
// regressed and named; other chains keep exporting. A re-anchor re-exports
// the reused positions once, labelled as a fork, and export goes on.
func TestRegressedChainStopsUntilReanchor(t *testing.T) {
	pool := freshDB(t, "siem_regress")
	ctx := context.Background()
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	regress(t, pool, e)
	mustExec(t, pool, `INSERT INTO sage.auth_audit (event, detail)
		VALUES ('login_succeeded', '{}')`)
	before := len(sink.exported())
	for pass := 0; pass < 2; pass++ {
		if err := e.RunOnce(ctx); err != nil {
			t.Fatalf("regressed pass %d: %v", pass, err)
		}
	}
	got := since(sink.exported(), before)
	if n := len(ofChain(got, "action_log")); n != 0 {
		t.Fatalf("a regressed chain delivered %d events: %+v", n, got)
	}
	if auth := ofChain(got, "auth_audit"); len(auth) != 1 || auth[0].seq != 1 {
		t.Fatalf("other chains must keep exporting: %+v", got)
	}
	st := e.Status()[0]
	if st.Status != StatusRegressed || len(st.Regressions) != 1 {
		t.Fatalf("status = %+v, want regressed with one regression", st)
	}
	reg := st.Regressions[0]
	if reg.Source != "orders" || reg.Chain != "action_log" || reg.Seq != 10 ||
		reg.Reason == "" {
		t.Fatalf("regression = %+v", reg)
	}
	for _, want := range []string{"orders", "action_log", "10"} {
		if !strings.Contains(st.LastError, want) {
			t.Fatalf("last error %q does not name %q", st.LastError, want)
		}
	}
	if seq, _, _ := cursorRow(t, pool, "soc"); seq != 10 {
		t.Fatalf("a regressed chain moved its cursor to %d", seq)
	}
	assertReanchorExports(t, pool, e, sink)
}

func assertReanchorExports(t *testing.T, pool *pgxpool.Pool, e *Exporter, sink *memSink) {
	t.Helper()
	ctx := context.Background()
	res, err := e.Reanchor(ctx, ReanchorRequest{Sink: "soc", Source: "orders",
		Chain: "action_log", FromSeq: 8})
	if err != nil {
		t.Fatalf("reanchor: %v", err)
	}
	if res.PreviousSeq != 10 || res.FromSeq != 8 || res.ForkTo != 10 {
		t.Fatalf("reanchor result = %+v", res)
	}
	if st := e.Status()[0]; st.Status != StatusOK || len(st.Regressions) != 0 {
		t.Fatalf("status after reanchor = %+v", st)
	}
	before := len(sink.exported())
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass after reanchor: %v", err)
	}
	got := ofChain(since(sink.exported(), before), "action_log")
	if len(got) != 3 {
		t.Fatalf("after reanchor delivered %+v, want links 8-10 once each", got)
	}
	for i, ev := range got {
		if ev.seq != int64(8+i) || !isFork(ev.labels) || ev.hash != linkHash(t, pool, ev.seq) {
			t.Fatalf("re-exported event %d = %+v, want seq %d labelled fork", i, ev, 8+i)
		}
	}
	addActions(t, pool, 1)
	before = len(sink.exported())
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass after new link: %v", err)
	}
	got = since(sink.exported(), before)
	if len(got) != 1 || got[0].seq != 11 || len(got[0].labels) != 0 {
		t.Fatalf("a new position after the fork = %+v, want 11 unlabelled", got)
	}
	if seq, hash, forkTo := cursorRow(t, pool, "soc"); seq != 11 ||
		hash != linkHash(t, pool, 11) || forkTo != 10 {
		t.Fatalf("cursor = %d %q fork_to %d", seq, hash, forkTo)
	}
}

// A chain that only went back (no new links yet) is regressed too: the
// link at the cursor is gone.
func TestTruncatedChainWithoutNewLinksIsRegressed(t *testing.T) {
	pool := freshDB(t, "siem_regress_short")
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	addActions(t, pool, 10)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	restoreTo(t, pool, 7)
	if err := e.RunOnce(context.Background()); err != nil {
		t.Fatalf("pass: %v", err)
	}
	st := e.Status()[0]
	if st.Status != StatusRegressed || len(st.Regressions) != 1 ||
		!strings.Contains(st.Regressions[0].Reason, "missing") {
		t.Fatalf("status = %+v, want regressed (link missing)", st)
	}
}

// Sinks are independent: a regression stops only the sink whose cursor
// passed the restore point.
func TestRegressionIsPerSink(t *testing.T) {
	pool := freshDB(t, "siem_regress_sinks")
	ctx := context.Background()
	soc := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: soc})
	addActions(t, pool, 10)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	late := &memSink{name: "late"}
	both := exporter(pool, unfenced, SinkConfig{Sink: soc}, SinkConfig{Sink: late})
	restoreTo(t, pool, 7)
	addActions(t, pool, 3)
	if err := both.RunOnce(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	st := both.Status()
	if st[0].Status != StatusRegressed || st[1].Status != StatusOK {
		t.Fatalf("statuses = %+v, want soc regressed and late ok", st)
	}
	if got := late.exported(); len(got) != 10 {
		t.Fatalf("a new sink got %d of the 10 current links", len(got))
	}
}

// Boundary: a cursor at the head with a matching hash is healthy.
func TestCursorAtHeadIsHealthy(t *testing.T) {
	pool := freshDB(t, "siem_at_head")
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	addActions(t, pool, 5)
	for pass := 0; pass < 2; pass++ {
		if err := e.RunOnce(context.Background()); err != nil {
			t.Fatalf("pass %d: %v", pass, err)
		}
	}
	st := e.Status()[0]
	if st.Status != StatusOK || len(st.Regressions) != 0 || st.LastError != "" {
		t.Fatalf("status at head = %+v", st)
	}
	if seq, hash, _ := cursorRow(t, pool, "soc"); seq != 5 || hash != linkHash(t, pool, 5) {
		t.Fatalf("cursor = %d %q, want 5 with link 5's hash", seq, hash)
	}
}

// A cursor that went back because the control database was restored
// re-exports what follows it, unflagged.
func TestControlRestoreReexportsUnflagged(t *testing.T) {
	pool := freshDB(t, "siem_control_restore")
	ctx := context.Background()
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	addActions(t, pool, 10)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	mustExec(t, pool, `UPDATE sage.siem_cursor SET seq = 5, hash = $1
		WHERE sink = 'soc' AND chain = 'action_log'`, linkHash(t, pool, 5))
	before := len(sink.exported())
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass after control restore: %v", err)
	}
	got := since(sink.exported(), before)
	if len(got) != 5 || got[0].seq != 6 || got[4].seq != 10 {
		t.Fatalf("re-exported %+v, want 6-10", got)
	}
	for _, ev := range got {
		if len(ev.labels) != 0 {
			t.Fatalf("a control restore labelled event %d: %v", ev.seq, ev.labels)
		}
	}
	if st := e.Status()[0]; st.Status != StatusOK {
		t.Fatalf("control restore flagged: %+v", st)
	}
}

// A cursor saved before the upgrade has no hash: it passes when its link
// exists, and gets the link's hash; one past the head is still regressed.
func TestPreUpgradeCursorWithoutHash(t *testing.T) {
	pool := freshDB(t, "siem_null_hash")
	ctx := context.Background()
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	addActions(t, pool, 5)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	mustExec(t, pool, "UPDATE sage.siem_cursor SET hash = NULL")
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass on a hashless cursor: %v", err)
	}
	if st := e.Status()[0]; st.Status != StatusOK {
		t.Fatalf("hashless cursor flagged: %+v", st)
	}
	if seq, hash, _ := cursorRow(t, pool, "soc"); seq != 5 || hash != linkHash(t, pool, 5) {
		t.Fatalf("cursor = %d %q, want link 5's hash backfilled", seq, hash)
	}
	mustExec(t, pool, "UPDATE sage.siem_cursor SET hash = NULL")
	restoreTo(t, pool, 3)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("pass: %v", err)
	}
	if st := e.Status()[0]; st.Status != StatusRegressed {
		t.Fatalf("a hashless cursor past the head = %+v, want regressed", st)
	}
	if _, hash, _ := cursorRow(t, pool, "soc"); hash != "" {
		t.Fatalf("a regressed cursor got hash %q", hash)
	}
}

// failingLookup fails the link lookup at the cursor and passes every
// other statement through.
type failingLookup struct{ *pgxpool.Pool }

func (f failingLookup) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	if strings.Contains(sql, "AND seq = $2") {
		return errRow{errors.New("connection reset by peer")}
	}
	return f.Pool.QueryRow(ctx, sql, args...)
}

func (f failingLookup) Query(ctx context.Context, sql string, args ...any) (pgx.Rows,
	error) {
	return f.Pool.Query(ctx, sql, args...)
}

type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// A failed lookup of the cursor's link is a delivery failure: reported,
// nothing sent, the cursor where it was.
func TestFailedAnchorLookupHoldsTheCursor(t *testing.T) {
	pool := freshDB(t, "siem_lookup_fail")
	ctx := context.Background()
	sink := &memSink{name: "soc"}
	e := exporter(pool, unfenced, SinkConfig{Sink: sink})
	addActions(t, pool, 5)
	if err := e.RunOnce(ctx); err != nil {
		t.Fatalf("first pass: %v", err)
	}
	addActions(t, pool, 2)
	broken := NewExporter(func() []Source {
		return []Source{{Name: "orders", DB: failingLookup{pool}}}
	}, []SinkConfig{{Sink: sink}}, NewCursorStore(pool), Options{BatchSize: 7}, unfenced,
		nil)
	err := broken.RunOnce(ctx)
	if err == nil || !strings.Contains(err.Error(), "connection reset") {
		t.Fatalf("pass with a failing lookup: err = %v", err)
	}
	st := broken.Status()[0]
	for _, want := range []string{"orders", "action_log", "5", "connection reset"} {
		if !strings.Contains(st.LastError, want) {
			t.Fatalf("last error %q does not name %q", st.LastError, want)
		}
	}
	if st.Status != StatusFailing || st.ConsecutiveFailures != 1 {
		t.Fatalf("status = %+v, want failing once", st)
	}
	if n := len(sink.exported()); n != 5 {
		t.Fatalf("delivered %d events without checking the anchor", n)
	}
	if seq, _, _ := cursorRow(t, pool, "soc"); seq != 5 {
		t.Fatalf("cursor moved to %d after a failed lookup", seq)
	}
}
