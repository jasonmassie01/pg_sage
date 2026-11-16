package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/changeplan"
	"github.com/pg-sage/sidecar/internal/mcptoken"
)

// plan-03a: the plan routes over a fake plan store: request validation,
// response shapes, error mapping and who may read and decide. The real
// tables are in plan_routes_db_test.go. No concurrency test here: the
// handlers keep no state between requests (two approvers racing is the
// store's, approval_db_test.go in changeplan).

const goldenPath = "testdata/plan_detail.golden.json"

// The detail response is the golden ux-03's mocks copy. Set
// PLAN_GOLDEN_UPDATE=1 to rewrite it after an intended change.
func TestPlanDetail_MatchesTheGolden(t *testing.T) {
	f := newPlanFixture(t, "orders")
	code, _, body := planCall(t, f.router(testViewerUser(), false), http.MethodGet,
		"/api/v1/plans/7", "", "")
	if code != http.StatusOK {
		t.Fatalf("get: %d %s", code, body)
	}
	var pretty map[string]any
	if err := json.Unmarshal([]byte(body), &pretty); err != nil {
		t.Fatal(err)
	}
	got, _ := json.MarshalIndent(pretty, "", "  ")
	if os.Getenv("PLAN_GOLDEN_UPDATE") == "1" {
		if err := os.WriteFile(goldenPath, append(got, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read the golden: %v", err)
	}
	if strings.TrimSpace(strings.ReplaceAll(string(want), "\r\n", "\n")) != string(got) {
		t.Fatalf("detail changed from %s:\n%s", goldenPath, got)
	}
	cov := pretty["coverage"].([]any)
	if len(cov) != 3 || cov[2].(map[string]any)["covered"] != false ||
		pretty["plan_hash"] != goldenHash || pretty["database"] != "orders" {
		t.Fatalf("detail %v, want three coverage rows with step 3 uncovered", pretty)
	}
}

func TestPlanList_NamesEachPlansDatabaseAndValidatesFilters(t *testing.T) {
	f := newPlanFixture(t, "orders", "billing")
	h := f.router(testViewerUser(), false)
	code, out, body := planCall(t, h, http.MethodGet, "/api/v1/plans?status=approved", "", "")
	plans, _ := out["plans"].([]any)
	if code != http.StatusOK || len(plans) != 2 || out["total"] != float64(2) {
		t.Fatalf("list: %d %s, want both databases' plans", code, body)
	}
	first := plans[0].(map[string]any)
	if first["database"] != "billing" || first["id"] != float64(7) ||
		first["plan_hash"] != goldenHash {
		t.Fatalf("first plan %v, want billing's plan 7", first)
	}
	for _, path := range []string{"/api/v1/plans?status=lost", "/api/v1/plans?limit=0",
		"/api/v1/plans?limit=501"} {
		if code, _, body := planCall(t, h, http.MethodGet, path, "", ""); code != 400 {
			t.Errorf("%s: %d %s, want 400", path, code, body)
		}
	}
	if code, _, _ := planCall(t, h, http.MethodGet, "/api/v1/plans?database=nope", "",
		""); code != http.StatusNotFound {
		t.Errorf("unknown database: %d, want 404", code)
	}
}

// Happy path: each decision reaches the store as the signed-in operator,
// with the decision as sent (via ui by default for a session).
func TestPlanDecisions_ReachTheStoreAsTheOperator(t *testing.T) {
	f := newPlanFixture(t, "orders")
	h := f.router(testOperatorUser(), false)
	calls := []struct{ path, body, want string }{
		{"/api/v1/plans/7/approve", `{"plan_hash":"` + goldenHash + `","mode":"up_to","up_to":2}`,
			"approve"},
		{"/api/v1/plans/7/steps/72/approve", `{"plan_hash":"` + goldenHash + `"}`,
			"approve_step"},
		{"/api/v1/plans/7/reject", `{"reason":"wrong column"}`, "reject"},
		{"/api/v1/plans/7/resume", `{"plan_hash":"` + goldenHash + `"}`, "resume"},
		{"/api/v1/plans/7/cancel", "", "cancel"},
	}
	for _, c := range calls {
		code, out, body := planCall(t, h, http.MethodPost, c.path, c.body, "")
		if code != http.StatusOK || out["ok"] != true || out["database"] != "orders" {
			t.Errorf("%s: %d %s", c.path, code, body)
		}
	}
	want := []string{"approve", "approve_step", "reject", "resume", "cancel"}
	if strings.Join(f.fake.decisions, ",") != strings.Join(want, ",") {
		t.Fatalf("decisions %v, want %v", f.fake.decisions, want)
	}
	for _, u := range f.fake.users {
		if u != testOperatorUser().ID {
			t.Fatalf("decided as users %v, want the operator %d", f.fake.users,
				testOperatorUser().ID)
		}
	}
	if d := f.fake.decided; d.Mode != changeplan.ModeUpTo || d.UpTo != 2 ||
		d.Via != changeplan.ViaUI || d.PlanHash != goldenHash {
		t.Fatalf("decision %+v, want up_to 2 via ui at the golden hash", d)
	}
}

// Invalid input and nil/empty: each refusal is 400 with its code, and the
// store is never asked.
func TestPlanDecisions_RefuseBadInputWithItsCode(t *testing.T) {
	f := newPlanFixture(t, "orders")
	h := f.router(testOperatorUser(), false)
	hash := `"plan_hash":"` + goldenHash + `"`
	cases := []struct{ path, body, code string }{
		{"/api/v1/plans/7/approve", `{` + hash + `,"mode":"most"}`, changeplan.CodeInvalidMode},
		{"/api/v1/plans/7/approve", `{` + hash + `,"mode":"up_to","up_to":0}`,
			changeplan.CodeInvalidUpTo},
		{"/api/v1/plans/7/approve", `{"mode":"all"}`, changeplan.CodeHashRequired},
		{"/api/v1/plans/7/approve", `{` + hash + `,"mode":"all","via":"cli"}`,
			changeplan.CodeInvalidVia},
		{"/api/v1/plans/7/approve", ``, "invalid_request"},
		{"/api/v1/plans/7/approve", `{` + hash + `,"mode":"all","extra":1}`, "invalid_request"},
		{"/api/v1/plans/x/approve", `{` + hash + `,"mode":"all"}`, "invalid_request"},
		{"/api/v1/plans/7/steps/0/approve", `{` + hash + `}`, "invalid_request"},
		{"/api/v1/plans/7/steps/72/approve", `{}`, changeplan.CodeHashRequired},
		{"/api/v1/plans/7/reject", `{"reason":"  "}`, changeplan.CodeReasonRequired},
		{"/api/v1/plans/7/reject", `{}`, changeplan.CodeReasonRequired},
		{"/api/v1/plans/7/resume", `{}`, changeplan.CodeHashRequired},
		{"/api/v1/plans/7/cancel", `{"reason":`, "invalid_request"},
	}
	for _, c := range cases {
		code, out, body := planCall(t, h, http.MethodPost, c.path, c.body, "")
		if code != http.StatusBadRequest || out["code"] != c.code {
			t.Errorf("%s %s: %d %s, want 400 %s", c.path, c.body, code, body, c.code)
		}
	}
	if len(f.fake.decisions) != 0 {
		t.Fatalf("the store was asked %v", f.fake.decisions)
	}
}

// Error propagation: the store's refusals map to their status and code; a
// store failure is 500 with nothing internal in the body.
func TestPlanDecisions_MapStoreErrors(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("wrap: %w", changeplan.ErrStaleHash), 409, "content_changed"},
		{fmt.Errorf("wrap: %w", changeplan.ErrNotFound), 404, "not_found"},
		{fmt.Errorf("wrap: %w", changeplan.ErrNotApprovable), 409, "not_pending"},
		{fmt.Errorf("wrap: %w", changeplan.ErrNotCancellable), 409, "not_cancellable"},
		{fmt.Errorf("wrap: %w", changeplan.ErrNotResumable), 409, "not_halted"},
		{fmt.Errorf("wrap: %w", changeplan.ErrStepNotApprovable), 409, "step_not_pending"},
		{fmt.Errorf("wrap: %w", &changeplan.DecisionError{Code: changeplan.CodeStepNotInPlan,
			Detail: "plan 7 has no step 99"}), 400, changeplan.CodeStepNotInPlan},
		{fmt.Errorf("wrap: %w", &changeplan.DecisionError{Code: changeplan.CodeInvalidUpTo,
			Detail: "up_to 4 is past the plan's 3 steps"}), 400, changeplan.CodeInvalidUpTo},
		{&changeplan.CapError{Cap: changeplan.CapIndexesPerTable, Limit: 2, Got: 3}, 409,
			"over_cap"},
	}
	for _, c := range cases {
		f := newPlanFixture(t, "orders")
		f.fake.err = c.err
		code, out, body := planCall(t, f.router(testOperatorUser(), false), http.MethodPost,
			"/api/v1/plans/7/approve", `{"plan_hash":"`+goldenHash+`","mode":"all"}`, "")
		if code != c.status || out["code"] != c.code {
			t.Errorf("%v: %d %s, want %d %s", c.err, code, body, c.status, c.code)
		}
	}
	f := newPlanFixture(t, "orders")
	f.fake.err = errors.New("pgx: relation sage.plan_secret_internal does not exist")
	for _, req := range [][2]string{{http.MethodGet, "/api/v1/plans/7"},
		{http.MethodPost, "/api/v1/plans/7/resume"}} {
		code, _, body := planCall(t, f.router(testOperatorUser(), false), req[0], req[1],
			`{"plan_hash":"`+goldenHash+`"}`, "")
		if code != http.StatusInternalServerError || strings.Contains(body, "secret_internal") {
			t.Errorf("%s %s with a store failure: %d %s, want 500 without internals",
				req[0], req[1], code, body)
		}
	}
}

// Fleet mode: a decision needs ?database= with more than one database.
func TestPlanDecisions_FleetNeedsTheDatabase(t *testing.T) {
	f := newPlanFixture(t, "orders", "billing")
	h := f.router(testOperatorUser(), false)
	body := `{"plan_hash":"` + goldenHash + `","mode":"all"}`
	if code, out, _ := planCall(t, h, http.MethodPost, "/api/v1/plans/7/approve", body,
		""); code != 400 || out["error"] != "database is required" {
		t.Fatalf("no database in a fleet: %d %v", code, out)
	}
	if code, _, _ := planCall(t, h, http.MethodPost,
		"/api/v1/plans/7/approve?database=nope", body, ""); code != http.StatusNotFound {
		t.Fatalf("unknown database: %d, want 404", code)
	}
	if code, out, _ := planCall(t, h, http.MethodPost,
		"/api/v1/plans/7/approve?database=billing", body, ""); code != 200 ||
		out["database"] != "billing" {
		t.Fatalf("billing: %d %v", code, out)
	}
}

func TestPlanRoutes_RolesGateReadsAndDecisions(t *testing.T) {
	f := newPlanFixture(t, "orders")
	approve := `{"plan_hash":"` + goldenHash + `","mode":"all"}`
	if code, _, _ := planCall(t, f.router(nil, false), http.MethodGet, "/api/v1/plans", "",
		""); code != http.StatusUnauthorized {
		t.Errorf("nobody reads: %d, want 401", code)
	}
	if code, _, _ := planCall(t, f.router(testViewerUser(), false), http.MethodGet,
		"/api/v1/plans/7", "", ""); code != http.StatusOK {
		t.Errorf("viewer reads: %d, want 200", code)
	}
	for _, path := range []string{"/api/v1/plans/7/approve", "/api/v1/plans/7/cancel",
		"/api/v1/plans/7/steps/72/approve", "/api/v1/plans/7/reject",
		"/api/v1/plans/7/resume"} {
		if code, _, _ := planCall(t, f.router(testViewerUser(), false), http.MethodPost, path,
			approve, ""); code != http.StatusForbidden {
			t.Errorf("viewer %s: %d, want 403", path, code)
		}
	}
	if len(f.fake.decisions) != 0 {
		t.Fatalf("a viewer decided %v", f.fake.decisions)
	}
	if code, _, _ := planCall(t, f.router(testAdminUser(), false), http.MethodPost,
		"/api/v1/plans/7/approve", approve, ""); code != http.StatusOK {
		t.Errorf("admin approves: %d, want 200", code)
	}
}

// The session middleware leaves a Bearer request to a plan route to the
// route, and only that.
func TestSkipsSessionAuthForPlanBearerOnly(t *testing.T) {
	for _, path := range []string{"/api/v1/plans", "/api/v1/plans/7/approve"} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		req.Header.Set("Authorization", "Bearer "+lintSecret(1))
		if !skipsSessionAuth(req) {
			t.Errorf("%s with Bearer does not skip session auth", path)
		}
	}
	refused := map[string]string{"no header": "/api/v1/plans/7/approve",
		"prefix only": "/api/v1/plansx", "traversal": "/api/v1/plans/../users",
		"dot": "/api/v1/plans/./7"}
	for name, path := range refused {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		if name != "no header" {
			req.Header.Set("Authorization", "Bearer "+lintSecret(1))
		}
		if skipsSessionAuth(req) {
			t.Errorf("%s (%s) skipped session auth", name, path)
		}
	}
}

func TestPlanRoutes_TokenAuthorization(t *testing.T) {
	approve := `{"plan_hash":"` + goldenHash + `","mode":"all","via":"cli"}`
	agent := mcptoken.Grant{TokenID: "ag", Kind: mcptoken.KindAgent,
		Scopes: []string{mcptoken.ScopeRead, mcptoken.ScopeApprove}, PrincipalID: "agp_1"}
	cases := []struct {
		name   string
		grant  mcptoken.Grant
		status int
	}{
		{"agent claiming approve", agent, http.StatusForbidden},
		{"operator read only", operatorGrant("r", []string{mcptoken.ScopeRead}),
			http.StatusForbidden},
		{"owner now a viewer", func() mcptoken.Grant {
			g := operatorGrant("v", []string{mcptoken.ScopeApprove})
			g.OwnerRole = "viewer"
			return g
		}(), http.StatusForbidden},
		{"operator approve", operatorGrant("a", []string{mcptoken.ScopeApprove}), 200},
	}
	for i, c := range cases {
		f := newPlanFixture(t, "orders")
		f.tokens = lintTokens(map[string]mcptoken.Grant{lintSecret(i): c.grant})
		// A viewer session beside the token is ignored: the token decides.
		code, _, body := planCall(t, f.router(testViewerUser(), false), http.MethodPost,
			"/api/v1/plans/7/approve", approve, "Bearer "+lintSecret(i))
		if code != c.status {
			t.Errorf("%s: %d %s, want %d", c.name, code, body, c.status)
		}
		if c.status == 200 && (len(f.fake.users) != 1 || f.fake.users[0] != 31 ||
			f.fake.decided.Via != changeplan.ViaCLI) {
			t.Errorf("%s: decided as %v via %s, want the owner 31 via cli", c.name,
				f.fake.users, f.fake.decided.Via)
		}
		if c.status != 200 && len(f.fake.decisions) != 0 {
			t.Errorf("%s: the store was asked %v", c.name, f.fake.decisions)
		}
	}
}

// A bad token is never retried as a session, even an operator's; a token
// limited to one database sees the others as unknown; a token cannot
// claim the dashboard as its channel.
func TestPlanRoutes_TokenIsTheOnlyCredential(t *testing.T) {
	f := newPlanFixture(t, "orders", "billing")
	f.tokens = lintTokens(map[string]mcptoken.Grant{
		lintSecret(1): operatorGrant("o", []string{mcptoken.ScopeRead, mcptoken.ScopeApprove},
			"orders")})
	h := f.router(testOperatorUser(), false)
	approve := `{"plan_hash":"` + goldenHash + `","mode":"all"}`
	if code, _, _ := planCall(t, h, http.MethodPost, "/api/v1/plans/7/approve?database=orders",
		approve, "Bearer "+lintSecret(99)); code != http.StatusUnauthorized {
		t.Fatalf("unknown token beside an operator session: %d, want 401", code)
	}
	if code, _, _ := planCall(t, h, http.MethodPost,
		"/api/v1/plans/7/approve?database=billing", approve,
		"Bearer "+lintSecret(1)); code != http.StatusNotFound {
		t.Fatalf("a database outside the token's list: %d, want 404", code)
	}
	code, out, _ := planCall(t, h, http.MethodGet, "/api/v1/plans", "", "Bearer "+lintSecret(1))
	if plans, _ := out["plans"].([]any); code != 200 || len(plans) != 1 ||
		plans[0].(map[string]any)["database"] != "orders" {
		t.Fatalf("token list: %d %v, want only orders", code, out)
	}
	ui := `{"plan_hash":"` + goldenHash + `","mode":"all","via":"ui"}`
	if code, out, _ := planCall(t, h, http.MethodPost, "/api/v1/plans/7/approve?database=orders",
		ui, "Bearer "+lintSecret(1)); code != 400 || out["code"] != changeplan.CodeInvalidVia {
		t.Fatalf("a token claiming via ui: %d %v, want 400 invalid_via", code, out)
	}
	f.tokens.err = errors.New("control database down")
	if code, _, _ := planCall(t, h, http.MethodGet, "/api/v1/plans", "",
		"Bearer "+lintSecret(1)); code != http.StatusServiceUnavailable {
		t.Fatalf("token store down: %d, want 503", code)
	}
}

// In a Databricks App the platform owns the Authorization header: the
// routes are session-only and never validate a token.
func TestPlanRoutes_AppModeIsSessionOnly(t *testing.T) {
	f := newPlanFixture(t, "orders")
	code, _, _ := planCall(t, f.router(testOperatorUser(), true), http.MethodPost,
		"/api/v1/plans/7/approve", `{"plan_hash":"`+goldenHash+`","mode":"all"}`,
		"Bearer platform-token")
	if code != http.StatusOK || f.tokens.calls.Load() != 0 || f.fake.users[0] != 2 {
		t.Fatalf("app mode: %d, %d token checks, users %v; want the session operator",
			code, f.tokens.calls.Load(), f.fake.users)
	}
}
