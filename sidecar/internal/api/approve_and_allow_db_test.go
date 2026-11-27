package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/policy"
)

// ux-07a end to end: POST /api/v1/approvals/{id}/approve-and-allow runs
// the card's action through the executor, then approves the class's L3
// promotion (creating it from the approval history when none waits).
// The action approval stands when the promotion fails; neither runs when
// the card, the body or the caller is refused.

// allowFixture is the card fixture whose executor has observed the
// database for 60 days, with a ledger where hygiene/analyze stands at L2
// on 3 + extra person-approved ANALYZE runs.
type allowFixture struct {
	*cardFixture
	ledger *earned.Service
	reg    *earned.Registry
}

var allowActionID int64 = 900000

func newAllowFixture(t *testing.T, extra int) *allowFixture {
	t.Helper()
	fx := newCardFixture(t)
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	exec := executor.New(fx.pool, cfg, time.Now().Add(-60*24*time.Hour),
		func(string, string, ...any) {})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.BlastRadius.MaxTablesPerWindow = 1 << 30
	doc.RateLimits.MaxSelfInitiatedChangesPerWindow = 1 << 30
	doc.BlastRadius.Hygiene = policy.KindBudget{MaxTablesPerWindow: 1 << 30,
		MaxChangesPerWindow: 1 << 30}
	exec.EnableStandingPolicyDocument(doc, nil)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	fx.exec = exec
	fx.mgr = fleet.NewManager(cfg)
	fx.mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: fx.pool,
		Executor: exec, Status: &fleet.InstanceStatus{}})
	af := &allowFixture{cardFixture: fx}
	af.ledger, af.reg = allowLedger(t, fx)
	af.approved(3)
	if _, err := af.ledger.ProposePromotions(context.Background()); err != nil {
		t.Fatal(err)
	}
	af.approvePending(t, earned.L2)
	af.approved(extra)
	return af
}

func allowLedger(t *testing.T, fx *cardFixture) (*earned.Service, *earned.Registry) {
	t.Helper()
	st, err := earned.NewPostgresStore(fx.pool, apiUUID(t), "orders")
	if err != nil {
		t.Fatal(err)
	}
	cfg := earned.DefaultConfig()
	cfg.EvidenceCacheTTL = 0
	ledger, err := earned.NewService(st, cfg)
	if err != nil {
		t.Fatal(err)
	}
	ledger.WithRamp(func() earned.RampFloor {
		return earned.RampFloor{Start: time.Now().Add(-60 * 24 * time.Hour)}
	})
	reg := earned.NewRegistry(true)
	// No limiter: its HA and budget inputs are another item's concern, and
	// without them it caps every class at L1 (fail closed).
	reg.Register("orders", earned.RegistryEntry{Service: ledger})
	return ledger, reg
}

// approved records n person-approved, improved ANALYZE runs.
func (af *allowFixture) approved(n int) {
	for i := 0; i < n; i++ {
		allowActionID++
		at := time.Now().UTC()
		if err := af.ledger.RecordOutcome(context.Background(), earned.Outcome{
			Database: "orders", ActionLogID: allowActionID, Family: earned.FamilyHygiene,
			Class: earned.ClassAnalyze, Level: earned.L2,
			Result: earned.ResultVerifiedRecovery, Source: earned.SourceExecutor,
			Verdict: "improved", ObservedAt: &at, Actor: earned.ActorPgSage}); err != nil {
			panic(err)
		}
	}
}

// approvePending approves the waiting hygiene/analyze proposal to want.
func (af *allowFixture) approvePending(t *testing.T, want earned.Level) {
	t.Helper()
	ps, err := af.ledger.PendingProposals(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range ps {
		if p.Family == earned.FamilyHygiene && p.Class == earned.ClassAnalyze {
			st, err := af.ledger.Approve(context.Background(), p.ID,
				"user:1:admin@test.com", "seed")
			if err != nil || st.Level != want {
				t.Fatalf("seed approve = %+v, %v", st, err)
			}
			return
		}
	}
	t.Fatalf("no hygiene/analyze proposal among %+v", ps)
}

func (af *allowFixture) level(t *testing.T) earned.Level {
	t.Helper()
	st, err := af.ledger.Granted(context.Background(), earned.FamilyHygiene,
		earned.ClassAnalyze)
	if err != nil {
		t.Fatal(err)
	}
	return st.Level
}

func injectCaller(user *auth.User, p *mcp.Principal) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx := context.WithValue(r.Context(), userContextKey, user)
			if p != nil {
				ctx = mcp.WithPrincipal(ctx, *p)
			}
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func (af *allowFixture) routerAs(user *auth.User, p *mcp.Principal) http.Handler {
	return newTestRouter(af.mgr, config.DefaultConfig(), nil, &ActionDeps{Fleet: af.mgr},
		nil, nil, &RuntimeDeps{Autonomy: &AutonomyDeps{Ledgers: af.reg}},
		injectCaller(user, p))
}

func allowPath(id int) string {
	return fmt.Sprintf("/api/v1/approvals/%d/approve-and-allow?database=orders", id)
}

func cardTrust(t *testing.T, h http.Handler, id int) map[string]any {
	t.Helper()
	code, body := cardCall(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", id), nil)
	card, _ := body["card"].(map[string]any)
	if code != http.StatusOK || card == nil {
		t.Fatalf("get card: %d %v", code, body)
	}
	tr, _ := card["trust"].(map[string]any)
	return tr
}

func (af *allowFixture) hashOf(t *testing.T, h http.Handler, id int) string {
	t.Helper()
	code, body := cardCall(t, h, http.MethodGet,
		fmt.Sprintf("/api/v1/approvals/%d?database=orders", id), nil)
	card, _ := body["card"].(map[string]any)
	hash, _ := card["card_hash"].(string)
	if code != http.StatusOK || hash == "" {
		t.Fatalf("card hash: %d %v", code, body)
	}
	return hash
}

// promotionsToL3 counts the ledger's approved L3 promotions of analyze.
func (af *allowFixture) promotionsToL3(t *testing.T) int {
	t.Helper()
	evs, err := af.ledger.History(context.Background(), earned.EventFilter{
		Family: earned.FamilyHygiene, Class: earned.ClassAnalyze, Database: "orders",
		Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range evs {
		if e.Type == earned.EventPromotionApproved && e.To != nil && *e.To == earned.L3 {
			n++
		}
	}
	return n
}

func TestApproveAndAllow_ExecutesAndGrantsL3(t *testing.T) {
	af := newAllowFixture(t, 9) // 12 approvals
	h := af.routerAs(testAdminUser(), nil)
	tr := cardTrust(t, h, af.queueID)
	if tr == nil || tr["allow_always"] != true || tr["level"] != "L2" {
		t.Fatalf("card trust = %v, want allow_always at L2", tr)
	}
	label, _ := tr["allow_always_label"].(string)
	if !strings.Contains(label, "stop asking for analyze on orders") {
		t.Fatalf("label = %q", label)
	}
	code, body := cardCall(t, h, http.MethodPost, allowPath(af.queueID),
		map[string]any{"card_hash": af.hashOf(t, h, af.queueID)})
	if code != http.StatusOK || body["executed"] != true || body["allow_always"] != true ||
		body["autonomy_level"] != "L3" {
		t.Fatalf("approve-and-allow: %d %v", code, body)
	}
	if n := af.executions(t); n != 1 {
		t.Fatalf("executions = %d, want 1", n)
	}
	if af.level(t) != earned.L3 || af.promotionsToL3(t) != 1 {
		t.Fatalf("level %v, promotions %d; want L3 once", af.level(t), af.promotionsToL3(t))
	}
	_, view := cardCall(t, h, http.MethodGet, "/api/v1/trust?database=orders", nil)
	raw, _ := json.Marshal(view)
	var got struct {
		Databases []earned.TrustView `json:"databases"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || len(got.Databases) != 1 {
		t.Fatalf("trust view: %v %s", err, raw)
	}
	for _, row := range got.Databases[0].Rows {
		if row.Family == earned.FamilyHygiene && row.Class == earned.ClassAnalyze {
			// ANALYZE is a SAFE class: L3 acts, then tells.
			if row.Level != earned.L3 || row.Mode != earned.ModeActsThenTells {
				t.Fatalf("analyze row = %+v", row)
			}
			return
		}
	}
	t.Fatal("no hygiene/analyze row in the trust view")
}

func TestApproveAndAllow_RefusalsRunNeitherStep(t *testing.T) {
	af := newAllowFixture(t, 9)
	h := af.routerAs(testAdminUser(), nil)
	hash := af.hashOf(t, h, af.queueID)
	cases := []struct {
		name string
		body map[string]any
		code int
	}{
		{"stale hash", map[string]any{"card_hash": "deadbeef"}, http.StatusConflict},
		{"no hash", map[string]any{}, http.StatusBadRequest},
		{"another class", map[string]any{"card_hash": hash, "class": "vacuum"},
			http.StatusBadRequest},
		{"another family", map[string]any{"card_hash": hash, "family": "tuning",
			"class": "analyze"}, http.StatusBadRequest},
		{"long note", map[string]any{"card_hash": hash, "note": strings.Repeat("n", 2001)},
			http.StatusBadRequest},
	}
	for _, tc := range cases {
		code, body := cardCall(t, h, http.MethodPost, allowPath(af.queueID), tc.body)
		if code != tc.code {
			t.Errorf("%s: %d %v, want %d", tc.name, code, body, tc.code)
		}
	}
	if n := af.executions(t); n != 0 {
		t.Fatalf("a refused request executed the action %d times", n)
	}
	if st, _ := af.status(t); st != "pending" || af.level(t) != earned.L2 {
		t.Fatalf("after refusals: status %s, level %v", st, af.level(t))
	}
}

// CHECK-02: an agent principal calling the route gets
// ErrHumanApprovalRequired, and nothing runs.
func TestApproveAndAllow_AnAgentPrincipalIsRefused(t *testing.T) {
	af := newAllowFixture(t, 9)
	hash := af.hashOf(t, af.routerAs(testAdminUser(), nil), af.queueID)
	agent := &mcp.Principal{Actor: "token:9", Role: "admin", Kind: mcp.KindAgent}
	code, body := cardCall(t, af.routerAs(testAdminUser(), agent), http.MethodPost,
		allowPath(af.queueID), map[string]any{"card_hash": hash})
	msg := fmt.Sprint(body)
	if code != http.StatusForbidden || !strings.Contains(msg, "human_approval_required") ||
		!strings.Contains(msg, earned.ErrHumanApprovalRequired.Error()) {
		t.Fatalf("agent: %d %v", code, body)
	}
	if af.executions(t) != 0 || af.level(t) != earned.L2 {
		t.Fatalf("an agent's request ran: %d executions, level %v", af.executions(t),
			af.level(t))
	}
	// An operator may approve actions but not grant autonomy.
	code, _ = cardCall(t, af.routerAs(operatorUser(), nil), http.MethodPost,
		allowPath(af.queueID), map[string]any{"card_hash": hash})
	if code != http.StatusForbidden || af.executions(t) != 0 {
		t.Fatalf("operator: %d, executions %d", code, af.executions(t))
	}
}

func TestApproveAndAllow_NoLedgerHidesTheButton(t *testing.T) {
	fx := newCardFixture(t)
	h := fx.router(t, testAdminUser())
	if tr := cardTrust(t, h, fx.queueID); tr != nil {
		t.Fatalf("trust without a ledger = %v", tr)
	}
	code, body := cardCall(t, h, http.MethodPost, allowPath(fx.queueID),
		map[string]any{"card_hash": fx.cardHash(t)})
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(body), "no ledger") {
		t.Fatalf("no ledger: %d %v", code, body)
	}
	if fx.executions(t) != 0 {
		t.Fatal("the action ran without a ledger")
	}
}

func TestApproveAndAllow_TooFewApprovalsIsRefusedBeforeTheAction(t *testing.T) {
	af := newAllowFixture(t, 0) // 3 approvals
	h := af.routerAs(testAdminUser(), nil)
	if tr := cardTrust(t, h, af.queueID); tr == nil || tr["allow_always"] == true {
		t.Fatalf("3 approvals: trust %v, want no allow_always", tr)
	}
	code, body := cardCall(t, h, http.MethodPost, allowPath(af.queueID),
		map[string]any{"card_hash": af.hashOf(t, h, af.queueID)})
	if code != http.StatusConflict || af.executions(t) != 0 {
		t.Fatalf("3 approvals: %d %v, executions %d", code, body, af.executions(t))
	}
}

// failingAllower fails every promotion, as a broken ledger store would.
type failingAllower struct{}

func (failingAllower) AllowAlways(context.Context, earned.Family, earned.ActionClass, string,
	string) (earned.State, error) {
	return earned.State{}, errors.New("record autonomy level: connection reset")
}

func TestApproveAndAllow_APromotionFailureLeavesTheApproval(t *testing.T) {
	af := newAllowFixture(t, 9)
	h := &approvalCardHandlers{mgr: af.mgr, ledgers: af.reg,
		allower: func(string) (alwaysAllower, bool) { return failingAllower{}, true }}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/approvals/{id}/approve-and-allow", h.approveAndAllow)
	handler := injectCaller(testAdminUser(), nil)(mux)
	hash := af.hashOf(t, af.routerAs(testAdminUser(), nil), af.queueID)
	code, body := cardCall(t, handler, http.MethodPost, allowPath(af.queueID),
		map[string]any{"card_hash": hash})
	msg, _ := body["autonomy_error"].(string)
	if code != http.StatusOK || body["executed"] != true || body["allow_always"] != false ||
		!strings.HasPrefix(msg, "approved; autonomy unchanged: ") ||
		!strings.Contains(msg, "connection reset") {
		t.Fatalf("promotion failure: %d %v", code, body)
	}
	var approvedBy *int
	if err := af.pool.QueryRow(context.Background(), `SELECT approved_by FROM
		sage.action_log WHERE sql_executed = $1`, af.sql).Scan(&approvedBy); err != nil ||
		approvedBy == nil || *approvedBy != testAdminUser().ID {
		t.Fatalf("action_log approval = %v, %v; want the admin", approvedBy, err)
	}
	if af.level(t) != earned.L2 || af.promotionsToL3(t) != 0 {
		t.Fatalf("level %v, promotions %d; want L2 and none", af.level(t),
			af.promotionsToL3(t))
	}
}

func TestApproveAndAllow_TwoCardsAtOnceApproveOnePromotion(t *testing.T) {
	af := newAllowFixture(t, 9)
	second := fmt.Sprintf("card_api_b_%d", time.Now().UnixNano())
	ctx := context.Background()
	if _, err := af.pool.Exec(ctx, "CREATE TABLE public."+second+" (id int)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = af.pool.Exec(ctx, "DROP TABLE IF EXISTS public."+second) })
	first := af.table
	af.table = second // the finding of the second card names its own table
	_, otherID := af.queue(t, "ANALYZE public."+second)
	af.table = first
	h := af.routerAs(testAdminUser(), nil)
	ids := []int{af.queueID, otherID}
	hashes := []string{af.hashOf(t, h, ids[0]), af.hashOf(t, h, ids[1])}
	bodies := make([]map[string]any, 2)
	codes := make([]int, 2)
	var wg sync.WaitGroup
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], bodies[i] = cardCall(t, h, http.MethodPost, allowPath(ids[i]),
				map[string]any{"card_hash": hashes[i]})
		}(i)
	}
	wg.Wait()
	// The loser either read L3 already (409, its action not run) or lost
	// the proposal race (its action approved, autonomy unchanged).
	granted, refused := 0, 0
	for i := range ids {
		msg, _ := bodies[i]["autonomy_error"].(string)
		switch {
		case codes[i] == http.StatusOK && bodies[i]["allow_always"] == true:
			granted++
		case codes[i] == http.StatusOK && bodies[i]["executed"] == true &&
			strings.HasPrefix(msg, "approved; autonomy unchanged: "):
			refused++
		case codes[i] == http.StatusConflict:
			refused++
		default:
			t.Fatalf("card %d: %d %v", ids[i], codes[i], bodies[i])
		}
	}
	if granted != 1 || refused != 1 || af.promotionsToL3(t) != 1 ||
		af.level(t) != earned.L3 {
		t.Fatalf("granted %d refused %d promotions %d level %v", granted, refused,
			af.promotionsToL3(t), af.level(t))
	}
}

// The button is offered only to a caller who could press it.
func TestApproveAndAllow_OnlyAnAdminPersonIsOffered(t *testing.T) {
	af := newAllowFixture(t, 9)
	agent := &mcp.Principal{Actor: "token:9", Role: "admin", Kind: mcp.KindAgent}
	for name, h := range map[string]http.Handler{
		"operator": af.routerAs(operatorUser(), nil),
		"agent":    af.routerAs(testAdminUser(), agent),
	} {
		tr := cardTrust(t, h, af.queueID)
		why, _ := tr["allow_always_why"].(string)
		if tr == nil || tr["allow_always"] != false || tr["allow_always_label"] != nil ||
			!strings.Contains(why, "only an admin") {
			t.Fatalf("%s: trust = %v, want no allow_always", name, tr)
		}
	}
	if tr := cardTrust(t, af.routerAs(testAdminUser(), nil), af.queueID); tr == nil ||
		tr["allow_always"] != true {
		t.Fatalf("admin: trust = %v, want allow_always", tr)
	}
}
