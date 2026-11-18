package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/store"
)

// Bulk approve on a real database (ux-07b): each item is its own decision
// through approveAndRunExpecting, bound to the card hash the person saw,
// and only safe-tier cards are taken. Skipped without
// SAGE_TEST_DATABASE_URL (live Postgres).

// bulkItem is one queued ANALYZE on its own table.
type bulkItem struct {
	id  int
	sql string
}

// addItem queues one more ANALYZE of risk tier risk on a fresh table.
func (fx *cardFixture) addItem(t *testing.T, risk string) bulkItem {
	t.Helper()
	ctx := context.Background()
	table := fmt.Sprintf("%s_b%d", fx.table, time.Now().UnixNano())
	if _, err := fx.pool.Exec(ctx, "CREATE TABLE public."+table+
		" (id int); INSERT INTO public."+table+" SELECT generate_series(1, 20)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(), `DELETE FROM sage.change_lease
			WHERE intent LIKE '%' || $1 || '%' OR object_key LIKE '%' || $1 || '%'
			   OR COALESCE(object_name, '') LIKE '%' || $1 || '%'`, table)
		_, _ = fx.pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table)
	})
	sql := "ANALYZE public." + table
	var findingID int
	if err := fx.pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql,
		status) VALUES ('stale_statistics', 'warning', 'table', $1,
		'Stale statistics on ' || $1, '{"n_mod_since_analyze": 5000}', 'Run ANALYZE', $2,
		'open') RETURNING id`, "public."+table, sql).Scan(&findingID); err != nil {
		t.Fatal(err)
	}
	id, err := store.NewActionStore(fx.pool).ProposeWithMetadata(ctx, nil, findingID, sql,
		"", risk, store.ActionProposalMetadata{ActionType: "analyze_table",
			PolicyDecision: "queue_approval"})
	if err != nil {
		t.Fatal(err)
	}
	return bulkItem{id: id, sql: sql}
}

func (fx *cardFixture) hashOf(t *testing.T, id int) string {
	t.Helper()
	code, body := cardCall(t, fx.router(t, testAdminUser()), http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", id), nil)
	card, _ := body["card"].(map[string]any)
	hash, _ := card["card_hash"].(string)
	if code != http.StatusOK || hash == "" {
		t.Fatalf("card %d: %d %v", id, code, body)
	}
	return hash
}

func (fx *cardFixture) runsOf(t *testing.T, sql string) int {
	t.Helper()
	var n int
	if err := fx.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = $1`, sql).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (fx *cardFixture) statusOf(t *testing.T, id int) string {
	t.Helper()
	var status string
	if err := fx.pool.QueryRow(context.Background(),
		`SELECT status FROM sage.action_queue WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func (fx *cardFixture) bulkBody(t *testing.T, items ...bulkItem) []map[string]any {
	t.Helper()
	out := make([]map[string]any, len(items))
	for i, it := range items {
		out[i] = map[string]any{"id": it.id, "database": "orders",
			"card_hash": fx.hashOf(t, it.id)}
	}
	return out
}

func TestBulkApprove_ThreeSafeItemsEachRunOnce(t *testing.T) {
	fx := newCardFixture(t)
	items := []bulkItem{{fx.queueID, fx.sql}, fx.addItem(t, "safe"), fx.addItem(t, "safe")}
	code, body := bulkCall(t, fx.router(t, operatorUser()), fx.bulkBody(t, items...))
	if code != http.StatusOK || body["approved"] != float64(3) || body["refused"] != float64(0) {
		t.Fatalf("bulk: %d %v", code, body)
	}
	results := bulkResults(t, body)
	for i, it := range items {
		r := results[i]
		if r["id"] != float64(it.id) || r["status"] != "approved" || r["executed"] != true ||
			r["database"] != "orders" || r["action_log_id"] == nil {
			t.Fatalf("result %d = %v", i, r)
		}
		if n := fx.runsOf(t, it.sql); n != 1 {
			t.Fatalf("%s ran %d times, want once", it.sql, n)
		}
		if s := fx.statusOf(t, it.id); s != "executed" {
			t.Fatalf("item %d status %q, want executed", it.id, s)
		}
	}
	// A second bulk of the same items decides nothing again.
	code, body = bulkCall(t, fx.router(t, operatorUser()), []map[string]any{
		{"id": items[0].id, "database": "orders", "card_hash": "stale"}})
	r := bulkResults(t, body)[0]
	if code != http.StatusOK || r["status"] != "refused" || r["code"] != "not_pending" ||
		fx.runsOf(t, items[0].sql) != 1 {
		t.Fatalf("repeat bulk: %d %v", code, body)
	}
}

func TestBulkApprove_RefusesANonSafeItemAndRunsTheRest(t *testing.T) {
	fx := newCardFixture(t)
	moderate := fx.addItem(t, "moderate")
	high := fx.addItem(t, "high_risk")
	safe := bulkItem{fx.queueID, fx.sql}
	code, body := bulkCall(t, fx.router(t, operatorUser()), fx.bulkBody(t, moderate, safe, high))
	if code != http.StatusOK || body["approved"] != float64(1) || body["refused"] != float64(2) {
		t.Fatalf("bulk: %d %v", code, body)
	}
	results := bulkResults(t, body)
	for _, i := range []int{0, 2} {
		r := results[i]
		if r["status"] != "refused" || r["code"] != "not_safe_tier" ||
			!strings.Contains(fmt.Sprint(r["reason"]), "bulk approve is safe tier only") {
			t.Fatalf("non-safe result %d = %v", i, r)
		}
	}
	if results[1]["status"] != "approved" || fx.runsOf(t, safe.sql) != 1 {
		t.Fatalf("safe result = %v", results[1])
	}
	for _, it := range []bulkItem{moderate, high} {
		if fx.runsOf(t, it.sql) != 0 || fx.statusOf(t, it.id) != "pending" {
			t.Fatalf("non-safe item %d ran or left pending", it.id)
		}
	}
}

func TestBulkApprove_GateFailureOnOneItemRunsTheOthers(t *testing.T) {
	fx := newCardFixture(t)
	first, second, third := bulkItem{fx.queueID, fx.sql}, fx.addItem(t, "safe"),
		fx.addItem(t, "safe")
	items := fx.bulkBody(t, first, second, third)
	// The second item's finding resolves after selection: the gate refuses it.
	if _, err := fx.pool.Exec(context.Background(), `UPDATE sage.findings
		SET status = 'resolved', resolved_at = now()
		WHERE id = (SELECT finding_id FROM sage.action_queue WHERE id = $1)`,
		second.id); err != nil {
		t.Fatal(err)
	}
	code, body := bulkCall(t, fx.router(t, operatorUser()), items)
	if code != http.StatusOK || body["approved"] != float64(2) || body["refused"] != float64(1) {
		t.Fatalf("bulk: %d %v", code, body)
	}
	r := bulkResults(t, body)
	if r[1]["status"] != "refused" || r[1]["code"] != "not_eligible" ||
		strings.TrimSpace(fmt.Sprint(r[1]["reason"])) == "" || r[1]["executed"] != false {
		t.Fatalf("gated result = %v", r[1])
	}
	if r[0]["status"] != "approved" || r[2]["status"] != "approved" {
		t.Fatalf("items 1 and 3 = %v, %v", r[0], r[2])
	}
	if fx.runsOf(t, first.sql) != 1 || fx.runsOf(t, third.sql) != 1 ||
		fx.runsOf(t, second.sql) != 0 {
		t.Fatal("runs: want items 1 and 3 once, item 2 never")
	}
}

func TestBulkApprove_ChangedCardRefusesThatItemOnly(t *testing.T) {
	fx := newCardFixture(t)
	first, second := bulkItem{fx.queueID, fx.sql}, fx.addItem(t, "safe")
	items := fx.bulkBody(t, first, second)
	if _, err := fx.pool.Exec(context.Background(), `UPDATE sage.action_queue
		SET rollback_sql = 'SELECT 1' WHERE id = $1`, second.id); err != nil {
		t.Fatal(err)
	}
	code, body := bulkCall(t, fx.router(t, operatorUser()), items)
	r := bulkResults(t, body)
	if code != http.StatusOK || r[0]["status"] != "approved" || r[1]["status"] != "refused" ||
		r[1]["code"] != "content_changed" {
		t.Fatalf("bulk: %d %v", code, body)
	}
	if fx.runsOf(t, second.sql) != 0 || fx.statusOf(t, second.id) != "pending" {
		t.Fatal("a changed card ran or left pending")
	}
}

func TestBulkApprove_FiftyMissingItemsEachAnswered(t *testing.T) {
	fx := newCardFixture(t)
	items := make([]map[string]any, 50)
	for i := range items {
		items[i] = map[string]any{"id": 900000000 + i, "database": "orders", "card_hash": "x"}
	}
	code, body := bulkCall(t, fx.router(t, operatorUser()), items)
	results := bulkResults(t, body)
	if code != http.StatusOK || len(results) != 50 || body["refused"] != float64(50) {
		t.Fatalf("bulk of 50: %d, %d results", code, len(results))
	}
	for _, r := range results {
		if r["code"] != "not_found" || r["executed"] != false {
			t.Fatalf("missing item result = %v", r)
		}
	}
}

// A bulk approve and a single card approve of the same item race: the
// item executes once and the loser is told it is no longer awaiting
// approval.
func TestBulkApprove_OverlapsASingleApproveExecutesOnce(t *testing.T) {
	for round := 0; round < 3; round++ {
		fx := newCardFixture(t)
		hash := fx.cardHash(t)
		h := fx.router(t, operatorUser())
		var wg sync.WaitGroup
		var single, bulk map[string]any
		var singleCode, bulkCode int
		wg.Add(2)
		go func() {
			defer wg.Done()
			singleCode, single = cardCall(t, h, http.MethodPost,
				fmt.Sprintf("/api/v1/approvals/%d/approve?database=orders", fx.queueID),
				map[string]any{"card_hash": hash})
		}()
		go func() {
			defer wg.Done()
			bulkCode, bulk = bulkCall(t, h, []map[string]any{
				{"id": fx.queueID, "database": "orders", "card_hash": hash}})
		}()
		wg.Wait()
		if fx.executions(t) != 1 || bulkCode != http.StatusOK {
			t.Fatalf("round %d: executions %d, single %d %v, bulk %d %v", round,
				fx.executions(t), singleCode, single, bulkCode, bulk)
		}
		r := bulkResults(t, bulk)[0]
		bulkWon := r["status"] == "approved"
		singleWon := singleCode == http.StatusOK
		if bulkWon == singleWon {
			t.Fatalf("round %d: want exactly one winner: single %d %v, bulk %v", round,
				singleCode, single, r)
		}
		// The bulk loser always names it; the single route's loser is a 409
		// (its wording is the single route's, tested in its own file).
		if singleWon && (r["code"] != "not_pending" ||
			!strings.Contains(fmt.Sprint(r["reason"]), "no longer awaiting approval")) {
			t.Fatalf("round %d: bulk loser said %v", round, r)
		}
		if bulkWon && singleCode != http.StatusConflict {
			t.Fatalf("round %d: single loser got %d %v", round, singleCode, single)
		}
	}
}

// An item whose SQL fails when it runs is reported failed with the error;
// the items after it still run.
func TestBulkApprove_ExecutionFailureIsReportedAndTheRestRun(t *testing.T) {
	fx := newCardFixture(t)
	broken, after := fx.addItem(t, "safe"), fx.addItem(t, "safe")
	items := fx.bulkBody(t, broken, after)
	table := strings.TrimPrefix(broken.sql, "ANALYZE ")
	if _, err := fx.pool.Exec(context.Background(), "DROP TABLE "+table); err != nil {
		t.Fatal(err)
	}
	code, body := bulkCall(t, fx.router(t, operatorUser()), items)
	r := bulkResults(t, body)
	if code != http.StatusOK || r[0]["status"] != "failed" || r[0]["executed"] != false ||
		!strings.Contains(fmt.Sprint(r[0]["reason"]), "does not exist") {
		t.Fatalf("broken item: %d %v", code, body)
	}
	if r[1]["status"] != "approved" || fx.runsOf(t, after.sql) != 1 ||
		body["failed"] != float64(1) {
		t.Fatalf("item after the failure: %v", body)
	}
}

// The card's risk tier is the policy decision's when there is one, so a
// queue item filed as safe whose decision says moderate is refused.
func TestBulkApprove_DecisionRiskTierOverridesTheQueueRisk(t *testing.T) {
	fx := newCardFixture(t)
	if _, err := fx.pool.Exec(context.Background(), `INSERT INTO sage.decision (feature,
		intent, verdict, risk_tier, reason, evidence_id, queue_id) VALUES ('analyze',
		'analyze', 'queue_approval', 'moderate', 'test: moderate on its decision', $1, $2)`,
		fmt.Sprintf("bulk-tier-%d", time.Now().UnixNano()), fx.queueID); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = fx.pool.Exec(context.Background(),
			`DELETE FROM sage.decision WHERE queue_id = $1`, fx.queueID)
	})
	code, body := bulkCall(t, fx.router(t, operatorUser()),
		fx.bulkBody(t, bulkItem{fx.queueID, fx.sql}))
	r := bulkResults(t, body)[0]
	if code != http.StatusOK || r["code"] != "not_safe_tier" ||
		!strings.Contains(fmt.Sprint(r["reason"]), "moderate") {
		t.Fatalf("bulk: %d %v", code, body)
	}
	if fx.runsOf(t, fx.sql) != 0 || fx.statusOf(t, fx.queueID) != "pending" {
		t.Fatal("a safe queue item with a moderate decision ran or left pending")
	}
}

