package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/inbox"
)

// CHECK-04 (ux-01a): every decision on an inbox item adds one sample to
// pg_sage_inbox_decision_seconds, labelled with the X-Sage-Surface sent.

func inboxDecisions(kind inbox.Kind, surface string) uint64 {
	_, all := inbox.Snapshot()
	for _, s := range all {
		if s.Kind == kind && s.Surface == surface {
			return s.Count
		}
	}
	return 0
}

func surfacePost(t *testing.T, h http.Handler, path, body, surface string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if surface != "" {
		req.Header.Set(inbox.SurfaceHeader, surface)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != 200 {
		t.Fatalf("POST %s = %d %s", path, w.Code, w.Body.String())
	}
	return w.Code
}

func TestInboxDecisionsDB_OneSamplePerDecisionWithItsSurface(t *testing.T) {
	mgr, reg := fleet.NewManager(config.DefaultConfig()), earned.NewRegistry(false)
	pool := inboxInstance(t, mgr, reg, "orders")
	_, runbook := seedInbox(t, mgr, pool, "orders")
	var fact int64
	if err := pool.QueryRow(context.Background(), `UPDATE sage.facts
		SET created_at = now() - interval '2 hours' RETURNING id`).Scan(&fact); err != nil {
		t.Fatal(err)
	}
	factsBefore := inboxDecisions(inbox.KindFact, "chat")
	runbooksBefore := inboxDecisions(inbox.KindRunbookSignature, "inbox")
	apiBefore := inboxDecisions(inbox.KindFact, "api")
	op := actionRouter(t, mgr, testOperatorUser())
	surfacePost(t, op, fmt.Sprintf("/api/v1/facts/%d/confirm?database=orders", fact), "",
		"chat")
	admin := sreRouter(t, mgr, testAdminUser())
	surfacePost(t, admin, "/api/v1/databases/orders/runbooks/"+runbook+"/sign",
		fmt.Sprintf(`{"version":1,"content_hash":%q}`, rbHash(t, rbDef)), "inbox")
	if got := inboxDecisions(inbox.KindFact, "chat"); got != factsBefore+1 {
		t.Fatalf("fact decisions from chat = %d, want %d", got, factsBefore+1)
	}
	if got := inboxDecisions(inbox.KindRunbookSignature, "inbox"); got != runbooksBefore+1 {
		t.Fatalf("runbook decisions from inbox = %d, want %d", got, runbooksBefore+1)
	}
	if got := inboxDecisions(inbox.KindFact, "api"); got != apiBefore {
		t.Fatalf("a chat decision was also counted as api (%d -> %d)", apiBefore, got)
	}
	_, all := inbox.Snapshot()
	for _, s := range all {
		if s.Kind == inbox.KindFact && s.Surface == "chat" && s.Sum < 2*3600-60 {
			t.Fatalf("fact waited %vs in the histogram, want about two hours", s.Sum)
		}
	}
	// A second decision on the decided fact is refused and adds nothing.
	req := httptest.NewRequest(http.MethodPost,
		fmt.Sprintf("/api/v1/facts/%d/confirm?database=orders", fact), nil)
	req.Header.Set(inbox.SurfaceHeader, "chat")
	w := httptest.NewRecorder()
	op.ServeHTTP(w, req)
	if w.Code == 200 || inboxDecisions(inbox.KindFact, "chat") != factsBefore+1 {
		t.Fatalf("re-deciding = %d, count %d", w.Code, inboxDecisions(inbox.KindFact, "chat"))
	}
}

func TestInboxDecisionsDB_ActionRejectIsTimedFromItsProposal(t *testing.T) {
	mgr, reg := fleet.NewManager(config.DefaultConfig()), earned.NewRegistry(false)
	pool := inboxInstance(t, mgr, reg, "orders")
	action, _ := seedInbox(t, mgr, pool, "orders")
	// Rejecting needs the database's executor; it executes nothing.
	mgr.GetInstance("orders").Executor = executor.New(pool, config.DefaultConfig(),
		time.Now().Add(-40*24*time.Hour), func(string, string, ...any) {})
	if _, err := pool.Exec(context.Background(), `UPDATE sage.action_queue
		SET proposed_at = now() - interval '30 minutes' WHERE id = $1`, action); err != nil {
		t.Fatal(err)
	}
	before := inboxDecisions(inbox.KindChangeApproval, "page")
	h := actionRouter(t, mgr, testOperatorUser())
	surfacePost(t, h, fmt.Sprintf("/api/v1/actions/%d/reject?database=orders", action),
		`{"reason":"not now"}`, "page")
	if got := inboxDecisions(inbox.KindChangeApproval, "page"); got != before+1 {
		t.Fatalf("change decisions from page = %d, want %d", got, before+1)
	}
	res := readInbox(t, inboxMux(mgr, reg, nil), "/api/v1/inbox/count", testViewerUser())
	if res.CountsByKind["change_approval"] != 0 {
		t.Fatalf("the rejected action is still in the inbox: %+v", res.CountsByKind)
	}
	if items, _ := inbox.Snapshot(); items == nil || items[inbox.KindChangeApproval] != 0 ||
		items[inbox.KindFact] != 1 {
		t.Fatalf("items gauge after a complete read = %v", items)
	}
}
