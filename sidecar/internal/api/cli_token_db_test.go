package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// cli-01a against a real executor and database (cardFixture): a Bearer
// approval runs through the existing approval path as the token's owner,
// the server caps its tier by notification_policy.chat_approval_max_tier
// (D7) whatever the client did, and two concurrent approvals run once.

// bearerCardRouter serves the card routes behind the CLI token middleware
// with remoteCap as the approval cap.
func bearerCardRouter(fx *cardFixture, remoteCap string) http.Handler {
	mux := http.NewServeMux()
	registerApprovalCardRoutes(mux, fx.mgr, nil, remoteCap)
	tokens := lintTokens(map[string]mcptoken.Grant{
		lintSecret(2): operatorGrant("ap", []string{mcptoken.ScopeRead, mcptoken.ScopeApprove}),
	})
	return withCLIToken(tokens, mux)
}

func bearerApprove(t *testing.T, h http.Handler, queueID int, hash string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/approvals/%d/approve?database=orders", queueID),
		strings.NewReader(`{"card_hash":"`+hash+`"}`))
	r.Header.Set("Authorization", "Bearer "+lintSecret(2))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("X-Sage-Surface", "cli")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	// The body as text with its JSON escapes undone ("&" is "&").
	var decoded map[string]any
	if json.Unmarshal(w.Body.Bytes(), &decoded) == nil {
		return w.Code, fmt.Sprint(decoded)
	}
	return w.Code, w.Body.String()
}

// Happy path: the token's owner (31) is the approver and the change ran.
func TestBearerApprove_RunsAsTheTokenOwner(t *testing.T) {
	fx := newCardFixture(t)
	h := bearerCardRouter(fx, "moderate")
	code, body := bearerApprove(t, h, fx.queueID, fx.cardHash(t))
	if code != http.StatusOK || !strings.Contains(body, "executed:true") {
		t.Fatalf("approve: %d %s", code, body)
	}
	var decidedBy int
	if err := fx.pool.QueryRow(context.Background(), `SELECT COALESCE(decided_by, 0)
		FROM sage.action_queue WHERE id = $1`, fx.queueID).Scan(&decidedBy); err != nil ||
		decidedBy != 31 {
		t.Fatalf("decided_by %d (%v), want the token owner 31", decidedBy, err)
	}
	if n := fx.executions(t); n != 1 {
		t.Fatalf("%d executions, want 1", n)
	}
}

// Boundary (D7): a high-tier change is refused on the server with the app
// link even though the request is a well-formed raw POST with the right
// hash; cap none refuses even a safe one; a session approval is not capped.
func TestBearerApprove_TierCapIsEnforcedByTheServer(t *testing.T) {
	fx := newCardFixture(t)
	if _, err := fx.pool.Exec(context.Background(), `UPDATE sage.action_queue
		SET action_risk = 'high' WHERE id = $1`, fx.queueID); err != nil {
		t.Fatal(err)
	}
	hash := fx.cardHash(t)
	code, body := bearerApprove(t, bearerCardRouter(fx, "moderate"), fx.queueID, hash)
	link := fmt.Sprintf("#/inbox?kind=change_approval&queue=%d", fx.queueID)
	if code != http.StatusForbidden || !strings.Contains(body, "remote_approval_capped") ||
		!strings.Contains(body, link) || !strings.Contains(body, "high") {
		t.Fatalf("high tier by token: %d %s; want 403 remote_approval_capped with %s", code,
			body, link)
	}
	if status, _ := fx.status(t); status != "pending" || fx.executions(t) != 0 {
		t.Fatalf("a capped approval changed the item: %s, %d runs", status, fx.executions(t))
	}
	if _, err := fx.pool.Exec(context.Background(), `UPDATE sage.action_queue
		SET action_risk = 'safe' WHERE id = $1`, fx.queueID); err != nil {
		t.Fatal(err)
	}
	code, body = bearerApprove(t, bearerCardRouter(fx, "none"), fx.queueID, fx.cardHash(t))
	if code != http.StatusForbidden || !strings.Contains(body, "remote_approval_capped") {
		t.Fatalf("cap none: %d %s; want 403", code, body)
	}
	code, out := cardCall(t, fx.router(t, testOperatorUser()), http.MethodPost,
		fmt.Sprintf("/api/v1/approvals/%d/approve?database=orders", fx.queueID),
		map[string]any{"card_hash": fx.cardHash(t)})
	if code != http.StatusOK || out["executed"] != true {
		t.Fatalf("session approval: %d %v; the cap is for remote approvals only", code, out)
	}
}

// Concurrent access: two approvals of one change produce one execution;
// the other is refused as no longer awaiting approval.
func TestBearerApprove_ConcurrentApprovalsRunOnce(t *testing.T) {
	fx := newCardFixture(t)
	h := bearerCardRouter(fx, "moderate")
	hash := fx.cardHash(t)
	var wg sync.WaitGroup
	codes, bodies := make([]int, 2), make([]string, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], bodies[i] = bearerApprove(t, h, fx.queueID, hash)
		}(i)
	}
	wg.Wait()
	ok, refused := 0, 0
	for i, c := range codes {
		switch {
		case c == http.StatusOK:
			ok++
		case c == http.StatusConflict && strings.Contains(bodies[i], "no longer awaiting"):
			refused++
		}
	}
	if ok != 1 || refused != 1 || fx.executions(t) != 1 {
		t.Fatalf("codes %v bodies %v, %d executions; want one 200, one 409, one run", codes,
			bodies, fx.executions(t))
	}
}

// A scheduled change's card carries its window: when it may start and
// whether that time has come, by the server's clock.
func TestBearerApprove_CardShowsTheSchedule(t *testing.T) {
	fx := newCardFixture(t)
	if _, err := fx.pool.Exec(context.Background(), `UPDATE sage.action_queue
		SET status = 'scheduled', scheduled_by = 2, scheduled_at = now(),
		    run_after = now() + interval '2 hours' WHERE id = $1`, fx.queueID); err != nil {
		t.Fatal(err)
	}
	code, out := cardCall(t, fx.router(t, testAdminUser()), http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", fx.queueID), nil)
	sched, _ := out["schedule"].(map[string]any)
	if code != http.StatusOK || sched["due"] != false || sched["run_after"] == nil ||
		sched["run_after_label"] == "" {
		t.Fatalf("scheduled card: %d %v", code, out)
	}
	if _, err := fx.pool.Exec(context.Background(), `UPDATE sage.action_queue
		SET run_after = now() - interval '1 minute' WHERE id = $1`, fx.queueID); err != nil {
		t.Fatal(err)
	}
	_, out = cardCall(t, fx.router(t, testAdminUser()), http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", fx.queueID), nil)
	if sched, _ := out["schedule"].(map[string]any); sched["due"] != true {
		t.Fatalf("a schedule whose time has come is not due: %v", out)
	}
}
