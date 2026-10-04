package executor

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/analyzer"
	"github.com/pg-sage/sidecar/internal/approvalcard"
	"github.com/pg-sage/sidecar/internal/store"
)

// Dogfood round 2 item 5 (lifeos 2026-10-04, read-only): two pending index
// approvals queued by 1.8.5 should not exist. Queue 6 proposed
// graph_nodes (node_type, name) beside idx_graph_nodes_node_type_name_
// covering (node_type, name) INCLUDE (id); queue 5's finding (18007) was
// already resolved. The coverage rule must apply on every path that queues
// or executes an index create, and pending items whose reason is gone are
// superseded automatically.

type hygieneFixture struct {
	exec  *Executor
	ctx   context.Context
	table string
}

func newHygieneFixture(t *testing.T) hygieneFixture {
	t.Helper()
	table, exec, ctx := verifiedTable(t, "public")
	for _, sql := range []string{
		"ALTER TABLE public." + table + " ADD COLUMN node_type text, ADD COLUMN name text",
		"CREATE INDEX " + table + "_cov ON public." + table +
			" (node_type, name) INCLUDE (a)",
	} {
		if _, err := exec.pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = exec.pool.Exec(ctx, "DELETE FROM sage.action_queue WHERE proposed_sql LIKE $1",
			"%"+table+"%")
		_, _ = exec.pool.Exec(ctx, "DELETE FROM sage.findings WHERE object_identifier LIKE $1",
			"%"+table+"%")
	})
	return hygieneFixture{exec: exec, ctx: ctx, table: table}
}

func (h hygieneFixture) finding(t *testing.T, category, sql, status string) int64 {
	t.Helper()
	var id int64
	err := h.exec.pool.QueryRow(h.ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommended_sql, status, last_seen,
		resolved_at)
		VALUES ($1, 'warning', 'table', $2, 'Index recommendation', '{}', $3, $4, now(),
		        CASE WHEN $4 = 'resolved' THEN now() END) RETURNING id`,
		category, "public."+h.table+"|"+category+"|"+sql, sql, status).Scan(&id)
	if err != nil {
		t.Fatalf("insert finding: %v", err)
	}
	return id
}

func (h hygieneFixture) queue(t *testing.T, findingID int64, sql, identity string) int {
	t.Helper()
	var id int
	err := h.exec.pool.QueryRow(h.ctx, `INSERT INTO sage.action_queue (finding_id,
		proposed_sql, action_risk, action_type, identity_key, expires_at)
		VALUES ($1, $2, 'moderate', 'create_index_concurrently', NULLIF($3, ''),
		        now() + interval '1 day') RETURNING id`, findingID, sql, identity).Scan(&id)
	if err != nil {
		t.Fatalf("insert queue item: %v", err)
	}
	return id
}

func queueState(t *testing.T, pool *pgxpool.Pool, id int) (string, string) {
	t.Helper()
	var status, reason string
	if err := pool.QueryRow(context.Background(), `SELECT status, COALESCE(reason, '')
		FROM sage.action_queue WHERE id = $1`, id).Scan(&status, &reason); err != nil {
		t.Fatalf("read queue item %d: %v", id, err)
	}
	return status, reason
}

// The coverage rule on real catalog definitions (pg_get_indexdef output),
// with INCLUDE, partial and expression semantics.
func TestCoveringIndexOnRealCatalog(t *testing.T) {
	h := newHygieneFixture(t)
	for _, sql := range []string{
		"CREATE INDEX " + h.table + "_part ON public." + h.table + " (b) WHERE a > 0",
		"CREATE INDEX " + h.table + "_expr ON public." + h.table + " (lower(name), b)",
	} {
		if _, err := h.exec.pool.Exec(h.ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	tbl := "public." + h.table
	cases := []struct {
		name, sql, want string
	}{
		{"lifeos queue 6: covered by an INCLUDE index",
			"CREATE INDEX CONCURRENTLY x ON " + tbl + " (node_type, name);", h.table + "_cov"},
		{"key prefix", "CREATE INDEX CONCURRENTLY x ON " + tbl + " (node_type)",
			h.table + "_cov"},
		{"include carried", "CREATE INDEX CONCURRENTLY x ON " + tbl +
			" (node_type) INCLUDE (name)", h.table + "_cov"},
		{"include not carried", "CREATE INDEX CONCURRENTLY x ON " + tbl +
			" (node_type) INCLUDE (b)", ""},
		{"same predicate", "CREATE INDEX CONCURRENTLY x ON " + tbl + " (b) WHERE a > 0",
			h.table + "_part"},
		{"other predicate", "CREATE INDEX CONCURRENTLY x ON " + tbl + " (b) WHERE a > 1", ""},
		{"partial index does not cover a full one",
			"CREATE INDEX CONCURRENTLY x ON " + tbl + " (b)", ""},
		{"expression prefix", "CREATE INDEX CONCURRENTLY x ON " + tbl + " (lower(name))",
			h.table + "_expr"},
		{"plain column is not the expression",
			"CREATE INDEX CONCURRENTLY x ON " + tbl + " (name)", ""},
		{"not a prefix", "CREATE INDEX CONCURRENTLY x ON " + tbl + " (name)", ""},
		{"unique needs a unique index", "CREATE UNIQUE INDEX CONCURRENTLY x ON " + tbl +
			" (node_type, name)", ""},
		{"not an index create", "VACUUM " + tbl, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := h.exec.coveringIndex(h.ctx, tc.sql)
			if err != nil || got != tc.want {
				t.Fatalf("coveringIndex = %q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// An invalid index (a failed CONCURRENTLY build) covers nothing.
func TestCoveringIndexIgnoresInvalidIndexes(t *testing.T) {
	h := newHygieneFixture(t)
	tbl := "public." + h.table
	for _, sql := range []string{"INSERT INTO " + tbl + " (b) VALUES (1), (1)"} {
		if _, err := h.exec.pool.Exec(h.ctx, sql); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := h.exec.pool.Exec(h.ctx, "CREATE UNIQUE INDEX CONCURRENTLY "+h.table+
		"_ub ON "+tbl+" (b)"); err == nil {
		t.Fatal("unique build over duplicates must fail")
	}
	got, err := h.exec.coveringIndex(h.ctx, "CREATE INDEX CONCURRENTLY x ON "+tbl+" (b)")
	if err != nil || got != "" {
		t.Fatalf("coveringIndex = %q, %v; an invalid index must cover nothing", got, err)
	}
}

// The background path: a covered candidate is never queued nor run, makes
// no decision, and its finding resolves with the covering index named.
func TestProcessFindingSkipsAndResolvesACoveredCreate(t *testing.T) {
	h := newHygieneFixture(t)
	sql := fmt.Sprintf("CREATE INDEX CONCURRENTLY %s_nt ON public.%s (node_type, name);",
		h.table, h.table)
	id := h.finding(t, "composite_index", sql, "open")
	f := analyzer.Finding{Category: "composite_index", Title: "Index recommendation",
		ObjectIdentifier: "public." + h.table + "|composite_index|" + sql,
		RecommendedSQL:   sql, ActionRisk: "moderate"}
	var decisionsBefore int
	_ = h.exec.pool.QueryRow(h.ctx, "SELECT count(*) FROM sage.decision").
		Scan(&decisionsBefore)
	h.exec.processFinding(h.ctx, f, false, nil)

	var status, detail string
	if err := h.exec.pool.QueryRow(h.ctx, `SELECT status, COALESCE(detail->>'covered_by',
		'') FROM sage.findings WHERE id = $1`, id).Scan(&status, &detail); err != nil {
		t.Fatal(err)
	}
	if status != "resolved" || detail != h.table+"_cov" {
		t.Fatalf("finding status=%s covered_by=%q, want resolved by %s_cov", status, detail,
			h.table)
	}
	var queued, decisions int
	_ = h.exec.pool.QueryRow(h.ctx, "SELECT count(*) FROM sage.action_queue WHERE "+
		"finding_id = $1", id).Scan(&queued)
	_ = h.exec.pool.QueryRow(h.ctx, "SELECT count(*) FROM sage.decision").Scan(&decisions)
	if queued != 0 || decisions != decisionsBefore {
		t.Fatalf("covered create: %d queue items, %d new decisions; want none", queued,
			decisions-decisionsBefore)
	}
}

func TestSupersedeStaleApprovals(t *testing.T) {
	h := newHygieneFixture(t)
	tbl := "public." + h.table
	covered := "CREATE INDEX CONCURRENTLY " + h.table + "_nt ON " + tbl + " (node_type, name);"
	fresh := "CREATE INDEX CONCURRENTLY " + h.table + "_b ON " + tbl + " (b);"
	resolvedID := h.queue(t, h.finding(t, "partial_index", fresh+" -- r", "resolved"),
		fresh, "")
	coveredID := h.queue(t, h.finding(t, "composite_index", covered, "open"), covered, "")
	openFinding := h.finding(t, "missing_index", fresh, "open")
	olderID := h.queue(t, openFinding, fresh, "missing_index:"+tbl+"|b")
	newerID := h.queue(t, openFinding, fresh, "missing_index:"+tbl+"|b")
	approvedID := h.queue(t, h.finding(t, "x_index", covered+" ", "resolved"), covered, "")
	if _, err := h.exec.pool.Exec(h.ctx, "UPDATE sage.action_queue SET status = "+
		"'approved' WHERE id = $1", approvedID); err != nil {
		t.Fatal(err)
	}

	n, err := h.exec.supersedeStaleApprovals(h.ctx)
	if err != nil || n != 3 {
		t.Fatalf("superseded %d (%v), want 3", n, err)
	}
	want := map[int][2]string{
		resolvedID: {"superseded", "is no longer open"},
		coveredID:  {"superseded", h.table + "_cov"},
		olderID:    {"superseded", fmt.Sprintf("queue item %d", newerID)},
		newerID:    {"pending", ""},
		approvedID: {"approved", ""},
	}
	for id, w := range want {
		status, reason := queueState(t, h.exec.pool, id)
		if status != w[0] || !strings.Contains(reason, w[1]) {
			t.Errorf("queue %d = %s (%q), want %s containing %q", id, status, reason, w[0],
				w[1])
		}
	}
	// The approval card's follow-up reads the closed item with its reason.
	o, err := approvalcard.ReadOutcome(h.ctx, h.exec.pool, coveredID)
	if err != nil || !o.Final || o.Verdict != "superseded" ||
		!strings.Contains(o.Detail, h.table+"_cov") {
		t.Fatalf("card outcome = %+v (%v), want final superseded with the reason", o, err)
	}
	if again, err := h.exec.supersedeStaleApprovals(h.ctx); err != nil || again != 0 {
		t.Fatalf("second sweep superseded %d (%v), want 0", again, err)
	}
	// Once superseded, an operator can no longer reject it (no demerit).
	if err := store.NewActionStore(h.exec.pool).Reject(h.ctx, coveredID, 7,
		"not now"); err == nil {
		t.Fatal("rejecting a superseded item must fail")
	}
	if status, _ := queueState(t, h.exec.pool, coveredID); status != "superseded" {
		t.Fatalf("a refused rejection changed the item to %s", status)
	}
}

// Supersession must not wait for the executor to be allowed to act: in
// manual mode the sweep still runs with the cycle.
func TestRunCycleSupersedesEvenInManualMode(t *testing.T) {
	h := newHygieneFixture(t)
	fresh := "CREATE INDEX CONCURRENTLY " + h.table + "_b ON public." + h.table + " (b);"
	id := h.queue(t, h.finding(t, "partial_index", fresh, "resolved"), fresh, "")
	h.exec.SetExecutionMode("manual")
	h.exec.RunCycle(h.ctx, false)
	if status, reason := queueState(t, h.exec.pool, id); status != "superseded" ||
		reason == "" {
		t.Fatalf("queue item = %s (%q), want superseded with a reason", status, reason)
	}
}

// The operator path uses the same rule: an approved create an existing
// index covers is recorded as done without DDL; one it does not cover
// (expression, predicate) is not mistaken for covered (the old manual check
// dropped expression keys and ignored the candidate's predicate).
func TestManualCreateIndexUsesTheCoverageRule(t *testing.T) {
	h := newHygieneFixture(t)
	tbl := "public." + h.table
	if _, err := h.exec.pool.Exec(h.ctx, "CREATE INDEX "+h.table+"_expr ON "+tbl+
		" (lower(name), b)"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		sql  string
		done bool
	}{
		{"CREATE INDEX CONCURRENTLY " + h.table + "_m1 ON " + tbl + " (node_type, name)", true},
		{"CREATE INDEX CONCURRENTLY " + h.table + "_m2 ON " + tbl + " (b)", false},
		{"CREATE INDEX CONCURRENTLY " + h.table + "_m3 ON " + tbl +
			" (node_type) WHERE a > 0", false},
	}
	for _, tc := range cases {
		done, _, err := h.exec.prepareManualCreateIndex(h.ctx, 0, tc.sql, "", map[string]any{},
			nil, 0)
		if err != nil || done != tc.done {
			t.Errorf("%s: done=%v err=%v, want done=%v", tc.sql, done, err, tc.done)
		}
	}
}
