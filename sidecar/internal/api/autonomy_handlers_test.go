package api

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/sre"
)

// Sage SRE M7 autonomy routes (AI-SRE-SPEC §7.3, §9): every signed-in
// role reads the ledger, its evidence and history; operators review
// packets, flag harm, ask for an evaluation, reject proposals and
// downgrade (restricting is always allowed); only an admin approves a
// promotion or uploads benchmark evidence, and the approver is recorded.

type autonomyClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *autonomyClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *autonomyClock) set(at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = at
}

type autonomyAPIFixture struct {
	t          *testing.T
	mgr        *fleet.DatabaseManager
	orders     sre.Investigation
	billing    sre.Investigation
	deployment string
	ledger     *earned.Service
	reg        *earned.Registry
	clock      *autonomyClock
}

func apiUUID(t *testing.T) string {
	t.Helper()
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		t.Fatal(err)
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func newAutonomyAPIFixture(t *testing.T) *autonomyAPIFixture {
	t.Helper()
	mgr, orders, billing := sreFixture(t)
	f := &autonomyAPIFixture{t: t, mgr: mgr, orders: orders, billing: billing,
		deployment: apiUUID(t), reg: earned.NewRegistry(true),
		clock: &autonomyClock{now: time.Now().UTC()}}
	f.ledger = f.register("orders")
	return f
}

// register binds database's own ledger (P0-5: one per database of the
// fixture's deployment).
func (f *autonomyAPIFixture) register(database string) *earned.Service {
	f.t.Helper()
	store, err := earned.NewPostgresStore(surfacePool(f.t), f.deployment, database)
	if err != nil {
		f.t.Fatal(err)
	}
	cfg := earned.DefaultConfig()
	cfg.Now, cfg.EvidenceCacheTTL = f.clock.Now, 0
	ledger, err := earned.NewService(store, cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	f.reg.Register(database, earned.RegistryEntry{Service: ledger,
		Limiter: ledger.Limiter(earned.Binding{Database: database})})
	return ledger
}

func (f *autonomyAPIFixture) router(user *auth.User) http.Handler {
	f.t.Helper()
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(f.mgr, config.DefaultConfig(), nil, nil, nil, nil,
		&RuntimeDeps{Autonomy: &AutonomyDeps{Ledgers: f.reg}}, inject)
}

func autonomyCall(t *testing.T, h http.Handler, method, path, body string) (int,
	map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if method == http.MethodPost {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	out := map[string]any{}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("%s %s: non-JSON body %q", method, path, w.Body.String())
	}
	return w.Code, out
}

func TestAutonomyAPI_ViewerReadsTheLedger(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	h := f.router(testViewerUser())
	code, body := autonomyCall(t, h, "GET", "/api/v1/sre/autonomy?database=orders", "")
	view, _ := body["view"].(map[string]any)
	families, _ := view["families"].([]any)
	if code != 200 || body["database"] != "orders" || body["enforced"] != true ||
		len(families) != len(earned.Families()) {
		t.Fatalf("view = %d %v", code, body)
	}
	first := families[0].(map[string]any)["classes"].([]any)[0].(map[string]any)
	if first["granted"] != "L1" || first["effective"] == nil {
		t.Fatalf("first row = %v", first)
	}
	for _, path := range []string{"/api/v1/sre/autonomy/history?database=orders",
		"/api/v1/sre/autonomy/proposals?database=orders"} {
		code, body := autonomyCall(t, h, "GET", path, "")
		if items, ok := body["items"].([]any); code != 200 || !ok || items == nil {
			t.Fatalf("%s = %d %v", path, code, body)
		}
	}
}

func TestAutonomyAPI_DatabaseResolution(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	h := f.router(testViewerUser())
	if code, body := autonomyCall(t, h, "GET", "/api/v1/sre/autonomy", ""); code != 200 ||
		body["database"] != "orders" {
		t.Fatalf("single database default = %d %v", code, body)
	}
	code, body := autonomyCall(t, h, "GET", "/api/v1/sre/autonomy?database=nope", "")
	if code != 404 || body["code"] != "not_found" {
		t.Fatalf("unknown database = %d %v", code, body)
	}
	f.register("billing")
	if code, body := autonomyCall(t, h, "GET", "/api/v1/sre/autonomy", ""); code != 400 ||
		body["code"] != "invalid_request" {
		t.Fatalf("ambiguous database = %d %v", code, body)
	}
}

func TestAutonomyAPI_RolesPerRoute(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	id := apiUUID(t)
	cases := []struct {
		method, path, body string
		user               *auth.User
		want               int
	}{
		{"POST", "/api/v1/sre/autonomy/downgrade?database=orders",
			`{"family":"lock_blocking","class":"backend_cancel","level":"L0","reason":"r"}`,
			testViewerUser(), 403},
		{"POST", "/api/v1/sre/autonomy/proposals/" + id + "/approve?database=orders", `{}`,
			testOperatorUser(), 403},
		{"POST", "/api/v1/sre/autonomy/proposals/" + id + "/approve?database=orders", `{}`,
			testViewerUser(), 403},
		{"POST", "/api/v1/sre/autonomy/proposals/" + id + "/reject?database=orders",
			`{"note":"n"}`, testViewerUser(), 403},
		{"POST", "/api/v1/sre/autonomy/reviews", `{}`, testViewerUser(), 403},
		{"POST", "/api/v1/sre/autonomy/outcomes", `{}`, testViewerUser(), 403},
		{"POST", "/api/v1/sre/autonomy/evaluate?database=orders", ``, testViewerUser(), 403},
		{"POST", "/api/v1/sre/autonomy/bench-results?database=orders", `{}`,
			testOperatorUser(), 403},
		{"POST", "/api/v1/sre/autonomy/game-days?database=orders", ``, testOperatorUser(), 403},
		{"POST", "/api/v1/sre/autonomy/rollouts", `{}`, testOperatorUser(), 403},
		{"GET", "/api/v1/sre/autonomy?database=orders", ``, nil, 401},
		{"POST", "/api/v1/sre/autonomy/proposals/" + id + "/approve?database=orders", `{}`,
			testAdminUser(), 404},
		{"POST", "/api/v1/sre/autonomy/evaluate?database=orders", ``, testOperatorUser(), 200},
	}
	for _, c := range cases {
		code, body := autonomyCall(t, f.router(c.user), c.method, c.path, c.body)
		if code != c.want {
			t.Errorf("%s %s as %v = %d %v, want %d", c.method, c.path, c.user, code, body,
				c.want)
		}
	}
}

func TestAutonomyAPI_ReviewResolvesTheFamilyFromTheInvestigation(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	h := f.router(testOperatorUser())
	code, body := autonomyCall(t, h, "POST", "/api/v1/sre/autonomy/reviews", fmt.Sprintf(
		`{"database":"orders","investigation_id":%q,"verdict":"accepted","note":"right"}`,
		f.orders.ID))
	if code != 200 || body["family"] != "lock_blocking" {
		t.Fatalf("review = %d %v", code, body)
	}
	sh, err := f.ledger.Store().ShadowStats(context.Background(), earned.FamilyLockBlocking,
		f.clock.Now().Add(-time.Hour))
	if err != nil || sh.Reviewed != 1 || sh.Accepted != 1 {
		t.Fatalf("shadow = %+v (%v)", sh, err)
	}
	for name, c := range map[string]struct {
		body string
		want int
	}{
		"unknown investigation": {fmt.Sprintf(`{"database":"orders","investigation_id":%q,`+
			`"verdict":"accepted"}`, apiUUID(t)), 404},
		"bad verdict": {fmt.Sprintf(`{"database":"orders","investigation_id":%q,`+
			`"verdict":"meh"}`, f.orders.ID), 400},
		"unknown database": {fmt.Sprintf(`{"database":"nope","investigation_id":%q,`+
			`"verdict":"accepted"}`, f.orders.ID), 404},
		"not json": {`{`, 400},
	} {
		if code, body := autonomyCall(t, h, "POST", "/api/v1/sre/autonomy/reviews", c.body); code !=
			c.want {
			t.Errorf("%s = %d %v, want %d", name, code, body, c.want)
		}
	}
}

func benchBody(at time.Time) string {
	return fmt.Sprintf(`{"schema":"pg_sage.pgincidentbench.v1","generated_at":%q,`+
		`"gated_arms":["causal-graph"],"cells":[{"arm":"causal-graph",`+
		`"family":"lock_blocking","runs":12,"safe_pass":{"k":12,"n":12},`+
		`"top1":{"k":12,"n":12},"mechanism_precision":0.97,"forbidden_actions":0}]}`,
		at.UTC().Format(time.RFC3339))
}

// seedShadow records 25 accepted reviews on orders, the first 31 days ago.
func (f *autonomyAPIFixture) seedShadow() {
	f.t.Helper()
	f.seedShadowOn(f.ledger)
}

func (f *autonomyAPIFixture) seedShadowOn(ledger *earned.Service) {
	f.t.Helper()
	now := f.clock.Now()
	for i := 0; i < 25; i++ {
		at := now.Add(-time.Hour)
		if i == 0 {
			at = now.Add(-31 * 24 * time.Hour)
		}
		f.clock.set(at)
		if err := ledger.RecordReview(context.Background(), earned.Review{
			Database: ledger.Database(), InvestigationID: apiUUID(f.t),
			Family: earned.FamilyLockBlocking, Verdict: earned.VerdictAccepted,
			Reviewer: "user:2:operator@test.com"}); err != nil {
			f.t.Fatal(err)
		}
	}
	f.clock.set(now)
}

func TestAutonomyAPI_PromotionIsProposedThenApprovedByAnAdmin(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	admin, operator := f.router(testAdminUser()), f.router(testOperatorUser())
	code, body := autonomyCall(t, admin, "POST", "/api/v1/sre/autonomy/bench-results?database=orders",
		benchBody(f.clock.Now().Add(-time.Hour)))
	if code != 201 {
		t.Fatalf("bench upload = %d %v", code, body)
	}
	if code, body = autonomyCall(t, admin, "POST",
		"/api/v1/sre/autonomy/bench-results?database=orders",
		benchBody(f.clock.Now().Add(-time.Hour))); code != 200 {
		t.Fatalf("duplicate upload = %d %v", code, body)
	}
	f.seedShadow()
	code, body = autonomyCall(t, operator, "POST", "/api/v1/sre/autonomy/evaluate?database=orders", "")
	created, _ := body["created"].([]any)
	if code != 200 || len(created) != 1 {
		t.Fatalf("evaluate = %d %v", code, body)
	}
	p := created[0].(map[string]any)
	if p["class"] != "backend_cancel" || p["to"] != "L2" {
		t.Fatalf("proposal = %v", p)
	}
	path := "/api/v1/sre/autonomy/proposals/" + p["id"].(string) + "/approve?database=orders"
	code, body = autonomyCall(t, admin, "POST", path, `{"note":"replay reviewed"}`)
	state, _ := body["state"].(map[string]any)
	if code != 200 || state["level"] != "L2" || state["changed_by"] != "user:1:admin@test.com" {
		t.Fatalf("approve = %d %v", code, body)
	}
	if code, body = autonomyCall(t, admin, "POST", path, `{}`); code != 409 ||
		body["code"] != "conflict" {
		t.Fatalf("second approve = %d %v", code, body)
	}
	down := `{"family":"lock_blocking","class":"backend_cancel","level":"L1","reason":"drill"}`
	if code, body = autonomyCall(t, operator, "POST", "/api/v1/sre/autonomy/downgrade?database=orders",
		down); code != 200 {
		t.Fatalf("downgrade = %d %v", code, body)
	}
	if code, body = autonomyCall(t, operator, "POST", "/api/v1/sre/autonomy/downgrade?database=orders",
		down); code != 409 || body["code"] != "not_a_downgrade" {
		t.Fatalf("repeat downgrade = %d %v", code, body)
	}
	evs, _ := f.ledger.History(context.Background(), earned.EventFilter{
		Family: earned.FamilyLockBlocking, Class: earned.ClassBackendCancel, Limit: 1})
	if len(evs) != 1 || evs[0].Actor != "user:2:operator@test.com" {
		t.Fatalf("downgrade actor = %+v", evs)
	}
}

func TestAutonomyAPI_OperatorsFlagHarmOnly(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	h := f.router(testOperatorUser())
	code, body := autonomyCall(t, h, "POST", "/api/v1/sre/autonomy/outcomes",
		`{"database":"orders","family":"lock_blocking","class":"backend_cancel",`+
			`"action_log_id":77,"result":"harmful","detail":"cancelled the wrong session"}`)
	if code != 200 {
		t.Fatalf("flag harm = %d %v", code, body)
	}
	n, err := f.ledger.Store().FamilyViolations(context.Background(),
		earned.FamilyLockBlocking, f.clock.Now().Add(-time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("violations = %d (%v)", n, err)
	}
	if code, body = autonomyCall(t, h, "POST", "/api/v1/sre/autonomy/outcomes",
		`{"database":"orders","family":"lock_blocking","class":"backend_cancel",`+
			`"result":"verified_recovery"}`); code != 400 {
		t.Fatalf("an operator recorded a recovery: %d %v", code, body)
	}
}

func TestAutonomyAPI_GameDaysAndRolloutsWhenNotConfigured(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	admin := f.router(testAdminUser())
	code, body := autonomyCall(t, admin, "GET", "/api/v1/sre/autonomy/game-days?database=orders", "")
	if code != 200 || body["enabled"] != false {
		t.Fatalf("game days = %d %v", code, body)
	}
	if code, body = autonomyCall(t, admin, "POST", "/api/v1/sre/autonomy/game-days?database=orders",
		""); code != 409 || body["code"] != "not_configured" {
		t.Fatalf("start game day = %d %v", code, body)
	}
	if code, body = autonomyCall(t, admin, "GET", "/api/v1/sre/autonomy/rollouts", ""); code != 200 {
		t.Fatalf("rollouts = %d %v", code, body)
	}
	if code, body = autonomyCall(t, admin, "POST", "/api/v1/sre/autonomy/rollouts",
		`{"source_database":"orders","action_log_id":1,"targets":["billing"]}`); code != 409 ||
		body["code"] != "not_configured" {
		t.Fatalf("start rollout = %d %v", code, body)
	}
}

func TestAutonomyAPI_RejectsMalformedInput(t *testing.T) {
	f := newAutonomyAPIFixture(t)
	admin := f.router(testAdminUser())
	for path, body := range map[string]string{
		"/api/v1/sre/autonomy/proposals/not-a-uuid/approve?database=orders": `{}`,
		"/api/v1/sre/autonomy/downgrade?database=orders": `{"family":"lock_blocking",` +
			`"class":"backend_cancel","level":"L9","reason":"r"}`,
		"/api/v1/sre/autonomy/bench-results?database=orders": `{"schema":"x"}`,
		"/api/v1/sre/autonomy/history?database=orders&limit=9999": ``,
	} {
		method := "POST"
		if strings.Contains(path, "history") {
			method = "GET"
		}
		if code, out := autonomyCall(t, admin, method, path, body); code != 400 ||
			out["code"] != "invalid_request" {
			t.Errorf("%s = %d %v", path, code, out)
		}
	}
}
