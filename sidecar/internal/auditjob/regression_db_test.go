package auditjob

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/siem"
)

// fakeCursors serves the SIEM cursors of one sink; err fails every read.
type fakeCursors struct {
	mu      sync.Mutex
	cursors map[string]siem.Cursor // by chain
	err     error
	reads   []string // source/chain of each read
}

func (f *fakeCursors) Cursors(_ context.Context, source, chain string) ([]siem.Cursor,
	error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, source+"/"+chain)
	if f.err != nil {
		return nil, f.err
	}
	c, ok := f.cursors[chain]
	if !ok {
		return nil, nil
	}
	return []siem.Cursor{c}, nil
}

func (f *fakeCursors) set(chain string, seq int64, hash string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.cursors == nil {
		f.cursors = map[string]siem.Cursor{}
	}
	f.cursors[chain] = siem.Cursor{Sink: "soc", Source: "orders", Chain: chain, Seq: seq,
		Hash: hash}
}

func linkHashAt(t *testing.T, pool *pgxpool.Pool, seq int64) string {
	t.Helper()
	var h string
	err := pool.QueryRow(context.Background(), `SELECT hash FROM sage.audit_chain_link
		WHERE chain = 'action_log' AND seq = $1`, seq).Scan(&h)
	if err != nil {
		t.Fatalf("link %d: %v", seq, err)
	}
	return h
}

// restoreTo drops the action_log links after keep together with their rows
// and the verification checkpoints past them (a backup or PITR restore
// takes the whole database back, sage.audit_chain_checkpoint included):
// the chain itself still verifies.
func restoreTo(t *testing.T, pool *pgxpool.Pool, keep int64) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, "SET LOCAL session_replication_role = replica"); err != nil {
		t.Fatalf("replica role: %v", err)
	}
	for _, sql := range []string{"DELETE FROM sage.action_log WHERE chain_seq > $1",
		"DELETE FROM sage.audit_chain_link WHERE chain = 'action_log' AND seq > $1",
		"DELETE FROM sage.audit_chain_checkpoint WHERE chain = 'action_log' AND seq > $1"} {
		if _, err := tx.Exec(ctx, sql, keep); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func addActions(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed) SELECT 'vacuum', 'VACUUM t' || g
		FROM generate_series(1, $1::int) g`, n); err != nil {
		t.Fatalf("insert actions: %v", err)
	}
}

// regressedFindings returns the open regression findings' count,
// severity, identifier and chain (from the detail).
func regressedFindings(t *testing.T, pool *pgxpool.Pool) (int, string, string, string) {
	t.Helper()
	var n int
	var sev, ident, chain string
	err := pool.QueryRow(context.Background(), `SELECT count(*),
		coalesce(max(severity), ''), coalesce(max(object_identifier), ''),
		coalesce(max(detail->>'chain'), '') FROM sage.findings
		WHERE category = $1 AND status = 'open'`, RegressedCategory).Scan(&n, &sev, &ident,
		&chain)
	if err != nil {
		t.Fatalf("read findings: %v", err)
	}
	return n, sev, ident, chain
}

// A chain whose positions the SIEM already holds were reused (restored
// and appended) raises one critical regression on its trail, though the
// chain itself verifies; it resolves once the cursor is re-anchored.
func TestRegressionFindingLifecycle(t *testing.T) {
	pool := fresh(t, "auditjob_regress", true)
	ctx := context.Background()
	cur := &fakeCursors{}
	src := Source{Name: "orders", Pool: pool, Cursors: cur}
	addActions(t, pool, 10)
	cur.set("action_log", 10, linkHashAt(t, pool, 10))
	if _, err := Run(ctx, src); err != nil {
		t.Fatalf("healthy run: %v", err)
	}
	if n, _, _, _ := regressedFindings(t, pool); n != 0 {
		t.Fatalf("a cursor at the head raised %d regressions", n)
	}
	restoreTo(t, pool, 7)
	addActions(t, pool, 3)
	reports, err := Run(ctx, src)
	if err != nil {
		t.Fatalf("regressed run: %v", err)
	}
	for _, rep := range reports {
		if rep.Chain == "action_log" && !rep.OK() {
			t.Fatalf("a consistent restore must verify clean: %+v", rep.Problems)
		}
	}
	n, sev, ident, chain := regressedFindings(t, pool)
	if n != 1 || sev != "critical" || ident != "audit_trail:action_log" ||
		chain != "action_log" {
		t.Fatalf("regression finding = %d %q %q %q", n, sev, ident, chain)
	}
	if broken, _, _ := openFindings(t, pool); broken != 0 {
		t.Fatalf("a consistent restore raised %d broken-chain findings", broken)
	}
	if _, err := Run(ctx, src); err != nil {
		t.Fatalf("repeat run: %v", err)
	}
	if n, _, _, _ := regressedFindings(t, pool); n != 1 {
		t.Fatalf("repeat run left %d open regressions, want the same one", n)
	}
	cur.set("action_log", 7, linkHashAt(t, pool, 7))
	if _, err := Run(ctx, src); err != nil {
		t.Fatalf("re-anchored run: %v", err)
	}
	if n, _, _, _ := regressedFindings(t, pool); n != 0 {
		t.Fatalf("a re-anchored cursor left %d open regressions", n)
	}
}

// head < cursor (restored, nothing appended yet) is a regression; a
// cursor at 0 or with no hash on an existing link is not.
func TestRegressionBoundaries(t *testing.T) {
	pool := fresh(t, "auditjob_regress_edges", true)
	ctx := context.Background()
	cur := &fakeCursors{}
	src := Source{Name: "orders", Pool: pool, Cursors: cur}
	addActions(t, pool, 5)
	cur.set("action_log", 5, "")
	cur.set("auth_audit", 0, "")
	if _, err := Run(ctx, src); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n, _, _, _ := regressedFindings(t, pool); n != 0 {
		t.Fatalf("a hashless cursor on an existing link raised %d regressions", n)
	}
	restoreTo(t, pool, 3)
	cur.set("action_log", 5, linkHashAt(t, pool, 3))
	if _, err := Run(ctx, src); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n, _, ident, _ := regressedFindings(t, pool); n != 1 ||
		ident != "audit_trail:action_log" {
		t.Fatalf("head 3 < cursor 5: %d regressions on %q", n, ident)
	}
	var detail string
	if err := pool.QueryRow(ctx, `SELECT detail::text FROM sage.findings
		WHERE category = $1 AND status = 'open'`, RegressedCategory).Scan(&detail); err != nil {
		t.Fatalf("read detail: %v", err)
	}
	for _, want := range []string{`"sink": "soc"`, `"seq": 5`, `"head_seq": 3`} {
		if !strings.Contains(detail, want) {
			t.Fatalf("detail %s does not carry %s", detail, want)
		}
	}
}

// A failed cursor read is an error naming the source; the chain checks
// are still recorded, and an open regression is not resolved by a check
// that could not run.
func TestRegressionCheckErrors(t *testing.T) {
	pool := fresh(t, "auditjob_regress_err", true)
	ctx := context.Background()
	cur := &fakeCursors{}
	src := Source{Name: "orders", Pool: pool, Cursors: cur}
	addActions(t, pool, 3)
	cur.set("action_log", 9, "deadbeef")
	if _, err := Run(ctx, src); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n, _, _, _ := regressedFindings(t, pool); n != 1 {
		t.Fatalf("setup: %d regressions, want 1", n)
	}
	mustExec(t, pool, "UPDATE sage.action_log SET sql_executed = 'SELECT 1' WHERE id = 1")
	cur.err = errors.New("control database unreachable")
	_, err := Run(ctx, src)
	if err == nil || !strings.Contains(err.Error(), "orders") ||
		!strings.Contains(err.Error(), "unreachable") {
		t.Fatalf("failed cursor read: err = %v", err)
	}
	if n, _, _ := openFindings(t, pool); n != 1 {
		t.Fatalf("the chain check was not recorded: %d broken findings", n)
	}
	if n, _, _, _ := regressedFindings(t, pool); n != 1 {
		t.Fatalf("a check that could not run resolved the regression (%d open)", n)
	}
}

// Without a running SIEM exporter there are no cursors to check, which
// proves nothing: an open regression from an earlier export stays open
// (fail closed) until a check against the cursors clears it.
func TestNoCursorsKeepsRegressions(t *testing.T) {
	pool := fresh(t, "auditjob_regress_off", true)
	ctx := context.Background()
	cur := &fakeCursors{}
	addActions(t, pool, 2)
	cur.set("action_log", 9, "")
	if _, err := Run(ctx, Source{Name: "orders", Pool: pool, Cursors: cur}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if n, _, _, _ := regressedFindings(t, pool); n != 1 {
		t.Fatalf("setup: %d regressions, want 1", n)
	}
	if len(cur.reads) == 0 || cur.reads[0] != "orders/action_log" {
		t.Fatalf("cursor reads = %v, want orders/<chain>", cur.reads)
	}
	if _, err := Run(ctx, Source{Name: "orders", Pool: pool}); err != nil {
		t.Fatalf("run without export: %v", err)
	}
	if n, _, ident, _ := regressedFindings(t, pool); n != 1 || ident == "" {
		t.Fatalf("export not running: %d regressions open (%q), want the one kept", n,
			ident)
	}
	cur.set("action_log", 2, "")
	if _, err := Run(ctx, Source{Name: "orders", Pool: pool, Cursors: cur}); err != nil {
		t.Fatalf("run with export back: %v", err)
	}
	if n, _, _, _ := regressedFindings(t, pool); n != 0 {
		t.Fatalf("an anchored cursor left %d regressions open", n)
	}
}
