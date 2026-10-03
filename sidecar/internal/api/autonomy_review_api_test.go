package api

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/earned"
)

// Phase 1.1 (2026-10-02 roadmap): promotion is reachable without curl.
// One review of a finished investigation writes both the shadow review
// (earned autonomy) and the investigation outcome (incident memory); the
// investigation outcome route writes the review too, so the two never
// disagree. "Evaluate now" says, for every pair it did not propose, which
// checks are unmet and how to meet them. P0-5: every route is scoped to
// its database's own ledger.

func (f *autonomyAPIFixture) investigationOutcomes(id string) []string {
	f.t.Helper()
	rows, err := surfacePool(f.t).Query(context.Background(), `SELECT
		verdict || ':' || COALESCE(actual_node, '') || ':' || actor
		FROM sage.sre_investigation_outcomes WHERE investigation_id = $1
		ORDER BY recorded_at`, id)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

func (f *autonomyAPIFixture) shadow(ledger *earned.Service) earned.Shadow {
	f.t.Helper()
	sh, err := ledger.Store().ShadowStats(context.Background(), earned.FamilyLockBlocking,
		f.clock.Now().Add(-time.Hour))
	if err != nil {
		f.t.Fatal(err)
	}
	return sh
}

func TestAutonomyAPI_ReviewWritesTheReviewAndTheInvestigationOutcome(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	h := f.router(testOperatorUser())
	code, body := autonomyCall(t, h, "POST", "/api/v1/sre/autonomy/reviews", fmt.Sprintf(
		`{"database":"orders","investigation_id":%q,"verdict":"rejected",`+
			`"note":"wrong holder","actual_root_cause":"connection_leak"}`, f.orders.ID))
	out, _ := body["investigation_outcome"].(map[string]any)
	if code != 200 || body["family"] != "lock_blocking" || body["verdict"] != "rejected" ||
		body["actual_node"] != "connection_leak" || out["verdict"] != "refuted" {
		t.Fatalf("review = %d %v", code, body)
	}
	if sh := f.shadow(f.ledger); sh.Reviewed != 1 || sh.Accepted != 0 {
		t.Fatalf("shadow = %+v, want one rejected review", sh)
	}
	got := f.investigationOutcomes(string(f.orders.ID))
	if len(got) != 1 || !strings.HasPrefix(got[0], "refuted:connection_leak:user:2") {
		t.Fatalf("investigation outcomes = %v", got)
	}
	// Free text that is not a graph node stays in the note only.
	code, body = autonomyCall(t, h, "POST", "/api/v1/sre/autonomy/reviews", fmt.Sprintf(
		`{"database":"orders","investigation_id":%q,"verdict":"accepted",`+
			`"actual_root_cause":"the nightly batch"}`, f.orders.ID))
	if code != 200 || body["actual_node"] != nil {
		t.Fatalf("free-text review = %d %v", code, body)
	}
	if sh := f.shadow(f.ledger); sh.Reviewed != 1 || sh.Accepted != 1 {
		t.Fatalf("a re-review must replace the verdict: %+v", sh)
	}
	if got := f.investigationOutcomes(string(f.orders.ID)); len(got) != 2 ||
		!strings.HasPrefix(got[1], "confirmed::") {
		t.Fatalf("investigation outcomes = %v", got)
	}
}

func TestAutonomyAPI_ReviewRefusals(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	h := f.router(testOperatorUser())
	for name, c := range map[string]struct {
		body string
		want int
		code string
	}{
		"verdict": {fmt.Sprintf(`{"database":"orders","investigation_id":%q,`+
			`"verdict":"confirmed"}`, f.orders.ID), 400, "invalid_request"},
		"long root cause": {fmt.Sprintf(`{"database":"orders","investigation_id":%q,`+
			`"verdict":"accepted","actual_root_cause":%q}`, f.orders.ID,
			strings.Repeat("r", 401)), 400, "invalid_request"},
		"another database's investigation": {fmt.Sprintf(`{"database":"orders",`+
			`"investigation_id":%q,"verdict":"accepted"}`, f.billing.ID), 404, "not_found"},
		"unknown field": {fmt.Sprintf(`{"database":"orders","investigation_id":%q,`+
			`"verdict":"accepted","reviewer":"someone else"}`, f.orders.ID), 400,
			"invalid_request"},
	} {
		code, body := autonomyCall(t, h, "POST", "/api/v1/sre/autonomy/reviews", c.body)
		if code != c.want || body["code"] != c.code {
			t.Errorf("%s = %d %v, want %d %s", name, code, body, c.want, c.code)
		}
	}
	if sh := f.shadow(f.ledger); sh.Reviewed != 0 {
		t.Fatalf("a refused review was recorded: %+v", sh)
	}
}

func TestAutonomyAPI_OutcomeRouteAlsoRecordsTheReview(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	h := f.router(testOperatorUser())
	path := "/api/v1/databases/orders/investigations/" + string(f.orders.ID) + "/outcome"
	code, body := autonomyCall(t, h, "POST", path, `{"verdict":"confirmed"}`)
	if code != 200 || body["verdict"] != "confirmed" {
		t.Fatalf("outcome = %d %v", code, body)
	}
	if sh := f.shadow(f.ledger); sh.Reviewed != 1 || sh.Accepted != 1 {
		t.Fatalf("shadow after a confirmed outcome = %+v, want one accepted review", sh)
	}
	code, body = autonomyCall(t, h, "POST", path,
		`{"verdict":"refuted","actual_node":"not_a_node"}`)
	if code != 400 {
		t.Fatalf("invalid node = %d %v", code, body)
	}
	if sh := f.shadow(f.ledger); sh.Accepted != 1 {
		t.Fatalf("a refused outcome changed the review: %+v", sh)
	}
}

func TestAutonomyAPI_EvaluateExplainsWhatItDidNotPropose(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	h := f.router(testOperatorUser())
	code, body := autonomyCall(t, h, "POST", "/api/v1/sre/autonomy/evaluate?database=orders",
		"")
	created, _ := body["created"].([]any)
	skipped, _ := body["not_proposed"].([]any)
	if code != 200 || created == nil || len(created) != 0 || len(skipped) == 0 {
		t.Fatalf("evaluate = %d %v", code, body)
	}
	var lock map[string]any
	for _, raw := range skipped {
		p := raw.(map[string]any)
		if p["family"] == "lock_blocking" && p["class"] == "backend_cancel" {
			lock = p
		}
	}
	unmet, _ := lock["unmet"].([]any)
	if lock == nil || lock["reason"] != "evidence_not_met" || lock["target"] != "L2" ||
		len(unmet) == 0 {
		t.Fatalf("lock_blocking/backend_cancel = %v", lock)
	}
	for _, raw := range unmet {
		c := raw.(map[string]any)
		if c["met"] != false || c["how"] == nil || c["how"] == "" {
			t.Fatalf("unmet check without an instruction: %v", c)
		}
	}
}

func TestAutonomyAPI_ViewCarriesInstructionsForUnmetChecks(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	_, body := autonomyCall(t, f.router(testViewerUser()), "GET",
		"/api/v1/sre/autonomy?database=orders", "")
	raw := fmt.Sprint(body["view"])
	if !strings.Contains(raw, "how:") || !strings.Contains(raw, "bench_results_path") {
		t.Fatalf("view without instructions: %s", raw)
	}
}

// P0-5 through the API: orders' evidence and approval never reach billing.
func TestAutonomyAPI_DatabasesHaveTheirOwnLedger(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	billing := f.register("billing")
	admin, operator := f.router(testAdminUser()), f.router(testOperatorUser())
	if code, body := autonomyCall(t, admin, "POST",
		"/api/v1/sre/autonomy/bench-results?database=orders",
		benchBody(f.clock.Now().Add(-time.Hour))); code != 201 {
		t.Fatalf("bench = %d %v", code, body)
	}
	f.seedShadow()
	_, body := autonomyCall(t, operator, "POST",
		"/api/v1/sre/autonomy/evaluate?database=billing", "")
	if created, _ := body["created"].([]any); len(created) != 0 {
		t.Fatalf("billing proposed from orders' reviews: %v", body)
	}
	_, body = autonomyCall(t, operator, "POST",
		"/api/v1/sre/autonomy/evaluate?database=orders", "")
	created, _ := body["created"].([]any)
	if len(created) != 1 {
		t.Fatalf("orders evaluate = %v", body)
	}
	id := created[0].(map[string]any)["id"].(string)
	code, body := autonomyCall(t, admin, "POST",
		"/api/v1/sre/autonomy/proposals/"+id+"/approve?database=billing", `{}`)
	if code != 404 || body["code"] != "not_found" {
		t.Fatalf("approve orders' proposal through billing = %d %v", code, body)
	}
	if code, body = autonomyCall(t, admin, "POST",
		"/api/v1/sre/autonomy/proposals/"+id+"/approve?database=orders", `{}`); code != 200 {
		t.Fatalf("approve on orders = %d %v", code, body)
	}
	st, err := billing.Granted(context.Background(), earned.FamilyLockBlocking,
		earned.ClassBackendCancel)
	if err != nil || st.Level != earned.L1 {
		t.Fatalf("billing after orders' approval = %+v (%v)", st, err)
	}
	if sh := f.shadow(billing); sh.Reviewed != 0 {
		t.Fatalf("billing sees orders' reviews: %+v", sh)
	}
}
