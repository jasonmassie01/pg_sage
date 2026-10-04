package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/fleet"
)

// GET /api/v1/approvals[/{id}] serves approval cards; approve, reject and
// snooze decide on one through the same executor path as the Actions page.
// An approval names the card hash it saw: if the action changed after the
// card was shown, it is refused.

func TestApprovalCardsListAndGet(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.router(t, operatorUser())
	code, body := cardCall(t, h, http.MethodGet, "/api/v1/approvals?database=orders", nil)
	if code != http.StatusOK {
		t.Fatalf("list: %d %v", code, body)
	}
	cards, _ := body["cards"].([]any)
	var mine map[string]any
	for _, c := range cards {
		m, _ := c.(map[string]any)
		if int(m["queue_id"].(float64)) == fx.queueID {
			mine = m
		}
	}
	if mine == nil || body["total"].(float64) < 1 {
		t.Fatalf("list lacks queue item %d: %v", fx.queueID, body)
	}
	if mine["database"] != "orders" || mine["sql"] != fx.sql ||
		mine["action_type"] != "analyze_table" || mine["card_hash"] == "" {
		t.Fatalf("card = %v", mine)
	}
	why, _ := mine["why_approval"].([]any)
	evidence, _ := mine["evidence"].([]any)
	if len(why) == 0 || len(evidence) == 0 {
		t.Fatalf("card has no why or evidence: %v", mine)
	}
	code, one := cardCall(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", fx.queueID), nil)
	card, _ := one["card"].(map[string]any)
	if code != http.StatusOK || card["queue_id"].(float64) != float64(fx.queueID) ||
		card["card_hash"] != mine["card_hash"] {
		t.Fatalf("get: %d %v", code, one)
	}
	if one["eligible"] != true {
		t.Fatalf("an approvable item is not eligible: %v", one)
	}
}

func TestApprovalCardRequestErrors(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.router(t, operatorUser())
	cases := []struct {
		path string
		want int
	}{
		{"/api/v1/approvals/abc?database=orders", http.StatusBadRequest},
		{"/api/v1/approvals/-4?database=orders", http.StatusBadRequest},
		{"/api/v1/approvals/987654321?database=orders", http.StatusNotFound},
		{fmt.Sprintf("/api/v1/approvals/%d?database=nope", fx.queueID), http.StatusNotFound},
		{fmt.Sprintf("/api/v1/approvals/%d?database=bad%%20name", fx.queueID),
			http.StatusBadRequest},
		{"/api/v1/approvals?database=nope", http.StatusNotFound},
	}
	for _, c := range cases {
		if code, body := cardCall(t, h, http.MethodGet, c.path, nil); code != c.want {
			t.Errorf("%s: %d %v, want %d", c.path, code, body, c.want)
		}
	}
	// One database: it is the default.
	if code, _ := cardCall(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d", fx.queueID), nil); code != http.StatusOK {
		t.Fatalf("single database default: %d", code)
	}
	// Two databases: the database must be named for one card.
	fx.mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "billing", Pool: fx.pool,
		Executor: fx.exec, Status: &fleet.InstanceStatus{}})
	if code, _ := cardCall(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d", fx.queueID), nil); code != http.StatusBadRequest {
		t.Fatalf("ambiguous database: %d", code)
	}
}

func TestApprovalCardRoles(t *testing.T) {
	fx := newCardFixture(t)
	path := fmt.Sprintf("/api/v1/approvals/%d", fx.queueID)
	if code, _ := cardCall(t, fx.router(t, viewerUser()), http.MethodGet,
		path+"?database=orders", nil); code != http.StatusForbidden {
		t.Fatalf("viewer read: %d", code)
	}
	if code, _ := cardCall(t, fx.router(t, viewerUser()), http.MethodPost,
		path+"/approve?database=orders", map[string]any{"card_hash": "x"}); code !=
		http.StatusForbidden {
		t.Fatalf("viewer approve: %d", code)
	}
	if code, _ := cardCall(t, fx.router(t, nil), http.MethodPost,
		path+"/approve?database=orders", map[string]any{"card_hash": "x"}); code !=
		http.StatusUnauthorized {
		t.Fatalf("anonymous approve: %d", code)
	}
	if fx.executions(t) != 0 {
		t.Fatal("a refused caller executed the action")
	}
}

func TestApprovalCardApproveExecutesOnce(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.router(t, operatorUser())
	hash := fx.cardHash(t)
	path := fmt.Sprintf("/api/v1/approvals/%d/approve?database=orders", fx.queueID)
	code, body := cardCall(t, h, http.MethodPost, path, map[string]any{"card_hash": hash})
	if code != http.StatusOK || body["executed"] != true || body["ok"] != true ||
		body["database"] != "orders" || body["verification_status"] == "" {
		t.Fatalf("approve: %d %v", code, body)
	}
	if status, _ := fx.status(t); status != "executed" || fx.executions(t) != 1 {
		t.Fatalf("after approve: status %q, executions %d", status, fx.executions(t))
	}
	code, body = cardCall(t, h, http.MethodPost, path, map[string]any{"card_hash": hash})
	if code != http.StatusConflict || fx.executions(t) != 1 {
		t.Fatalf("second approve: %d %v, executions %d", code, body, fx.executions(t))
	}
}

func TestApprovalCardApproveRefusesChangedContent(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.router(t, operatorUser())
	hash := fx.cardHash(t)
	path := fmt.Sprintf("/api/v1/approvals/%d/approve?database=orders", fx.queueID)
	if code, body := cardCall(t, h, http.MethodPost, path, map[string]any{}); code !=
		http.StatusBadRequest {
		t.Fatalf("approve without a hash: %d %v", code, body)
	}
	if code, body := cardCall(t, h, http.MethodPost, path, nil); code !=
		http.StatusBadRequest {
		t.Fatalf("approve without a body: %d %v", code, body)
	}
	if _, err := fx.pool.Exec(context.Background(), `UPDATE sage.action_queue
		SET rollback_sql = 'SELECT 1' WHERE id = $1`, fx.queueID); err != nil {
		t.Fatal(err)
	}
	code, body := cardCall(t, h, http.MethodPost, path, map[string]any{"card_hash": hash})
	if code != http.StatusConflict || body["code"] != "content_changed" {
		t.Fatalf("stale card approve: %d %v", code, body)
	}
	if status, _ := fx.status(t); status != "pending" || fx.executions(t) != 0 {
		t.Fatalf("after refusal: status %q executions %d", status, fx.executions(t))
	}
}

func TestApprovalCardApproveNotEligibleSaysWhy(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.router(t, operatorUser())
	hash := fx.cardHash(t)
	if _, err := fx.pool.Exec(context.Background(), `UPDATE sage.findings
		SET status = 'resolved', resolved_at = now() WHERE id = $1`, fx.findingID); err != nil {
		t.Fatal(err)
	}
	code, body := cardCall(t, h, http.MethodPost,
		fmt.Sprintf("/api/v1/approvals/%d/approve?database=orders", fx.queueID),
		map[string]any{"card_hash": hash})
	msg, _ := body["error"].(string)
	if code != http.StatusConflict || msg == "" || body["executed"] == true {
		t.Fatalf("approve of a resolved finding: %d %v", code, body)
	}
	if fx.executions(t) != 0 {
		t.Fatal("executed an action whose finding is resolved")
	}
}

func TestApprovalCardRejectStoresTheReason(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.router(t, operatorUser())
	path := fmt.Sprintf("/api/v1/approvals/%d/reject?database=orders", fx.queueID)
	if code, _ := cardCall(t, h, http.MethodPost, path,
		map[string]any{"reason": "   "}); code != http.StatusBadRequest {
		t.Fatalf("reject without reason: %d", code)
	}
	code, body := cardCall(t, h, http.MethodPost, path,
		map[string]any{"reason": "app owns this table's stats"})
	if code != http.StatusOK || body["status"] != "rejected" {
		t.Fatalf("reject: %d %v", code, body)
	}
	status, reason := fx.status(t)
	if status != "rejected" || reason != "app owns this table's stats" || fx.executions(t) != 0 {
		t.Fatalf("after reject: %q %q", status, reason)
	}
	if code, _ := cardCall(t, h, http.MethodPost, path,
		map[string]any{"reason": "again"}); code != http.StatusConflict {
		t.Fatalf("second reject: %d", code)
	}
}

func TestApprovalCardSnooze(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.router(t, operatorUser())
	path := fmt.Sprintf("/api/v1/approvals/%d/snooze?database=orders", fx.queueID)
	for _, hours := range []any{0, -1, 169, "four"} {
		if code, _ := cardCall(t, h, http.MethodPost, path,
			map[string]any{"hours": hours, "reason": "later"}); code != http.StatusBadRequest {
			t.Errorf("hours %v: %d, want 400", hours, code)
		}
	}
	code, body := cardCall(t, h, http.MethodPost, path,
		map[string]any{"hours": 4, "reason": "busy hours"})
	if code != http.StatusOK || body["status"] != "snoozed" {
		t.Fatalf("snooze: %d %v", code, body)
	}
	var until time.Time
	var reason string
	var by int
	if err := fx.pool.QueryRow(context.Background(), `SELECT snoozed_until, snooze_reason,
		snoozed_by FROM sage.action_queue WHERE id = $1`, fx.queueID).
		Scan(&until, &reason, &by); err != nil {
		t.Fatal(err)
	}
	if d := time.Until(until); d < 3*time.Hour+50*time.Minute || d > 4*time.Hour+time.Minute ||
		reason != "busy hours" || by != operatorUser().ID {
		t.Fatalf("snooze row: until in %v, %q, by %d", d, reason, by)
	}
	_, one := cardCall(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", fx.queueID), nil)
	card, _ := one["card"].(map[string]any)
	if card["snoozed_until"] == nil || card["snooze_reason"] != "busy hours" {
		t.Fatalf("snoozed card = %v", card)
	}
	if status, _ := fx.status(t); status != "pending" {
		t.Fatalf("a snoozed item must stay pending, got %q", status)
	}
}

// Two operators press Approve on the same card at once: one execution.
func TestApprovalCardConcurrentApprovesExecuteOnce(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.router(t, operatorUser())
	hash := fx.cardHash(t)
	path := fmt.Sprintf("/api/v1/approvals/%d/approve?database=orders", fx.queueID)
	var wg sync.WaitGroup
	codes := make([]int, 6)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _ = cardCall(t, h, http.MethodPost, path,
				map[string]any{"card_hash": hash})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, c := range codes {
		if c == http.StatusOK {
			ok++
		}
	}
	if ok != 1 || fx.executions(t) != 1 {
		t.Fatalf("codes %v, executions %d: want exactly one", codes, fx.executions(t))
	}
	if !strings.Contains(fmt.Sprint(codes), "409") {
		t.Fatalf("losers were not refused: %v", codes)
	}
}
