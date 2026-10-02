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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/sre/probes"
)

// Sage SRE action routes (AI-SRE-SPEC §9, Codex §6): operators propose an
// evidence-matched cancel and request its execution; the approval itself
// is the existing /api/v1/actions/{id}/approve, so attribution and the
// single-use rule hold. Viewers read only (CHECK-25), every route is
// scoped to its database (CHECK-09/26).

var actionStart = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// activeLockRunner scripts an active root blocker (pid 5151) with an
// ALTER and a reader queued behind it.
type activeLockRunner struct{}

func (activeLockRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	res := probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusEmpty,
		ObservedAt: time.Now()}
	if id != probes.LockGraph {
		return res
	}
	edge := func(waiter, blocker int64, mode string, waiting bool) probes.Row {
		return probes.Row{"waiter_pid": waiter, "waiter_backend_start": actionStart,
			"lock_type": "relation", "requested_mode": mode, "relation": "public.orders",
			"blocker_pid": blocker, "blocker_kind": "backend", "blocker_state": "active",
			"blocker_waiting": waiting, "blocker_xact_age_s": 12.0,
			"blocker_backend_start": actionStart, "blocker_query_id": int64(77)}
	}
	res.Status = probes.StatusOK
	res.Rows = []probes.Row{edge(20, 5151, "AccessExclusiveLock", false),
		edge(30, 20, "AccessShareLock", true)}
	return res
}

// targetRunner answers the action probes for pid 5151.
type targetRunner struct{}

func (targetRunner) Run(_ context.Context, id probes.ID, _ probes.Args) probes.Result {
	res := probes.Result{ProbeID: id, Version: "v1", Status: probes.StatusOK,
		ObservedAt: time.Now()}
	switch id {
	case probes.SignalTarget:
		res.Rows = []probes.Row{{"pid": int64(5151), "backend_start": actionStart,
			"query_start": actionStart.Add(time.Minute), "xact_start": actionStart,
			"datname": "orders", "usename": "app", "state": "active",
			"backend_type": "client backend", "waiting": false, "query_id": int64(77),
			"query_hash": strings.Repeat("a", 64), "blocking": int64(2),
			"in_recovery": false, "in_current_database": true, "privileged_role": false,
			"protected_application": false, "application_hash": strings.Repeat("b", 64)}}
	default:
		res.Status = probes.StatusEmpty
	}
	return res
}

// recordingCanceller records cancels and asks the real executor for the
// policy preview.
type recordingCanceller struct {
	mu    sync.Mutex
	exec  *executor.Executor
	calls []executor.BackendCancel
}

func (c *recordingCanceller) CancelBackend(_ context.Context,
	req executor.BackendCancel) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, req)
	return 0, nil
}

func (c *recordingCanceller) PreviewBackendCancel(ctx context.Context,
	isReplica bool) executor.ActionPolicyDecision {
	return c.exec.PreviewBackendCancel(ctx, isReplica)
}

func (c *recordingCanceller) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.calls)
}

type actionFixture struct {
	pool     *pgxpool.Pool
	inv      sre.Investigation
	actions  *sreaction.ActionService
	exec     *executor.Executor
	canceler *recordingCanceller
}

// actionInstance registers a database with one concluded active-blocker
// investigation, its action service and an executor behind a real gate.
func actionInstance(t *testing.T, mgr *fleet.DatabaseManager, name, trust string) *actionFixture {
	t.Helper()
	pool := surfacePool(t)
	ctx := context.Background()
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st,
		Runner: activeLockRunner{}, Config: sre.DefaultCoordinatorConfig(
			fmt.Sprintf("api-action:%s:%d", name, time.Now().UnixNano()))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Bind(ctx); err != nil {
		t.Fatal(err)
	}
	inv, _, err := coord.Start(ctx, sre.Trigger{CaseID: "incident:" + name + ":lock:9",
		Kind: sre.TriggerLock, Subject: "incident 9", IdempotencyKey: "incident:9"})
	if err != nil || coord.Investigate(ctx, inv.ID) != nil {
		t.Fatalf("investigation: %v", err)
	}
	cfg := config.DefaultConfig()
	cfg.Trust.Level = trust
	exec := executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	exec.EnableStandingPolicyDocument(doc, nil)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	svc := sre.NewService(name, coord, st)
	f := &actionFixture{pool: pool, exec: exec, canceler: &recordingCanceller{exec: exec}}
	ac := sreaction.DefaultActionConfig()
	ac.RequestApproval = false
	f.actions, err = sreaction.NewActionService(sreaction.ActionDeps{Service: svc,
		Targets: targetRunner{}, Queue: sreaction.NewPGApprovalQueue(pool, nil),
		Executor: f.canceler, Config: ac, LogFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatal(err)
	}
	exec.SetApprovedActionRunner(f.actions)
	f.inv = inv
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool, Executor: exec,
		Status: &fleet.InstanceStatus{}, Investigations: svc, Actions: f.actions})
	return f
}

func actionRouter(t *testing.T, mgr *fleet.DatabaseManager, user *auth.User) http.Handler {
	t.Helper()
	inject := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if user != nil {
				r = r.WithContext(context.WithValue(r.Context(), userContextKey, user))
			}
			next.ServeHTTP(w, r)
		})
	}
	return NewRouterFullRuntime(mgr, config.DefaultConfig(), nil,
		&ActionDeps{Fleet: mgr}, nil, nil, nil, inject)
}

func actionCall(t *testing.T, h http.Handler, method, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	var out map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &out)
	return w.Code, out
}

func proposalsPath(db string, inv sre.UUID) string {
	return "/api/v1/databases/" + db + "/investigations/" + string(inv) + "/proposals"
}

func TestSREActionAPI_ProposeRequestApproveThroughTheExistingQueue(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	f := actionInstance(t, mgr, "orders", "advisory")
	h := actionRouter(t, mgr, testOperatorUser())

	code, body := actionCall(t, h, "POST", proposalsPath("orders", f.inv.ID))
	if code != 200 || body["state"] != "proposed" || body["sql"] !=
		"SELECT pg_cancel_backend(5151)" {
		t.Fatalf("propose = %d %v", code, body)
	}
	target, _ := body["target"].(map[string]any)
	contract, _ := body["contract"].(map[string]any)
	policyView, _ := body["policy"].(map[string]any)
	if target["pid"] != float64(5151) || contract["reversibility"] != "mitigation_only" ||
		policyView["decision"] != "execute" {
		t.Fatalf("proposal view: target %v contract %v policy %v", target, contract,
			policyView)
	}
	pid, _ := body["id"].(string)
	code, body = actionCall(t, h, "POST", proposalsPath("orders", f.inv.ID)+"/"+pid+"/request")
	approval, _ := body["approval"].(map[string]any)
	if code != 200 || body["state"] != "requested" || approval["status"] != "pending" {
		t.Fatalf("request = %d %v", code, body)
	}
	queueID := int(approval["queue_id"].(float64))
	if f.canceler.count() != 0 {
		t.Fatal("requesting execution executed")
	}
	code, body = actionCall(t, h, "POST",
		fmt.Sprintf("/api/v1/actions/%d/approve?database=orders", queueID))
	if code != 200 || body["executed"] != true ||
		body["verification_status"] != "monitoring" {
		t.Fatalf("approve = %d %v", code, body)
	}
	if f.canceler.count() != 1 || f.canceler.calls[0].ApprovedBy != testOperatorUser().ID {
		t.Fatalf("cancels %d (approver %v)", f.canceler.count(), f.canceler.calls)
	}
	code, list := actionCall(t, h, "GET", proposalsPath("orders", f.inv.ID))
	items, _ := list["items"].([]any)
	if code != 200 || len(items) != 1 || items[0].(map[string]any)["state"] != "executed" {
		t.Fatalf("list = %d %v", code, list)
	}
	code, _ = actionCall(t, h, "POST",
		fmt.Sprintf("/api/v1/actions/%d/approve?database=orders", queueID))
	if code == 200 || f.canceler.count() != 1 {
		t.Fatalf("a second approval = %d, cancels %d; want refused, 1", code,
			f.canceler.count())
	}
}

func TestSREActionAPI_ViewerReadsOnly(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	f := actionInstance(t, mgr, "orders", "advisory")
	p, err := f.actions.Propose(context.Background(), f.inv.ID, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	h := actionRouter(t, mgr, testViewerUser())
	if code, _ := actionCall(t, h, "GET", proposalsPath("orders", f.inv.ID)); code != 200 {
		t.Fatalf("viewer list = %d", code)
	}
	for _, path := range []string{proposalsPath("orders", f.inv.ID),
		proposalsPath("orders", f.inv.ID) + "/" + string(p.ID) + "/request"} {
		if code, _ := actionCall(t, h, "POST", path); code != http.StatusForbidden {
			t.Fatalf("viewer POST %s = %d, want 403", path, code)
		}
	}
	got, _ := f.actions.Get(context.Background(), p.ID)
	if got.State != sreaction.ProposalProposed {
		t.Fatalf("viewer changed the proposal to %s", got.State)
	}
}

func TestSREActionAPI_ScopedToItsDatabase(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	orders := actionInstance(t, mgr, "orders", "advisory")
	actionInstance(t, mgr, "billing", "advisory")
	p, err := orders.actions.Propose(context.Background(), orders.inv.ID, "user:1")
	if err != nil {
		t.Fatal(err)
	}
	h := actionRouter(t, mgr, testOperatorUser())
	code, body := actionCall(t, h, "POST",
		proposalsPath("billing", orders.inv.ID)+"/"+string(p.ID)+"/request")
	if code != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("cross-database request = %d %v, want 404", code, body)
	}
	if code, _ := actionCall(t, h, "POST", proposalsPath("billing", orders.inv.ID)); code !=
		http.StatusNotFound {
		t.Fatalf("cross-database propose = %d, want 404", code)
	}
}

func TestSREActionAPI_CanonicalErrors(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	f := actionInstance(t, mgr, "orders", "observation")
	h := actionRouter(t, mgr, testOperatorUser())
	code, body := actionCall(t, h, "POST", proposalsPath("orders", f.inv.ID))
	if code != 200 {
		t.Fatalf("propose = %d %v", code, body)
	}
	pid, _ := body["id"].(string)
	code, body = actionCall(t, h, "POST", proposalsPath("orders", f.inv.ID)+"/"+pid+"/request")
	if code != http.StatusConflict || body["code"] != "policy_blocked" ||
		!strings.Contains(fmt.Sprint(body["error"]), "observe") {
		t.Fatalf("request under observation trust = %d %v", code, body)
	}
	code, body = actionCall(t, h, "POST", proposalsPath("orders", f.inv.ID)+"/"+
		string(sre.NewUUID())+"/request")
	if code != http.StatusNotFound || body["code"] != "not_found" {
		t.Fatalf("unknown proposal = %d %v", code, body)
	}
	code, body = actionCall(t, h, "POST", proposalsPath("orders", f.inv.ID)+"/nope/request")
	if code != http.StatusBadRequest || body["code"] != "invalid_request" {
		t.Fatalf("malformed proposal id = %d %v", code, body)
	}
	mgr2 := fleet.NewManager(config.DefaultConfig())
	sreInstance(t, mgr2, "plain")
	h2 := actionRouter(t, mgr2, testOperatorUser())
	code, body = actionCall(t, h2, "POST", proposalsPath("plain", sre.NewUUID()))
	if code != http.StatusServiceUnavailable || body["code"] != "metadata_unavailable" {
		t.Fatalf("database without actions = %d %v", code, body)
	}
}

// CHECK-18: a policy change between the request and the approval blocks
// the approval itself; the item stays pending and nothing is signalled.
func TestSREActionAPI_ChangedPolicyBlocksTheApproval(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	f := actionInstance(t, mgr, "orders", "advisory")
	h := actionRouter(t, mgr, testOperatorUser())
	code, body := actionCall(t, h, "POST", proposalsPath("orders", f.inv.ID))
	if code != 200 {
		t.Fatalf("propose = %d %v", code, body)
	}
	pid, _ := body["id"].(string)
	code, body = actionCall(t, h, "POST", proposalsPath("orders", f.inv.ID)+"/"+pid+"/request")
	approval, _ := body["approval"].(map[string]any)
	if code != 200 || approval["queue_id"] == nil {
		t.Fatalf("request = %d %v", code, body)
	}
	queueID := int(approval["queue_id"].(float64))
	if err := f.exec.SetTrustLevel("observation"); err != nil {
		t.Fatal(err)
	}
	code, body = actionCall(t, h, "POST",
		fmt.Sprintf("/api/v1/actions/%d/approve?database=orders", queueID))
	if code != http.StatusConflict || !strings.Contains(fmt.Sprint(body["error"]),
		"not eligible") {
		t.Fatalf("approve after the trust drop = %d %v, want 409 not eligible", code, body)
	}
	var status string
	if err := f.pool.QueryRow(context.Background(), `SELECT status FROM sage.action_queue
		WHERE id = $1`, queueID).Scan(&status); err != nil || status != "pending" {
		t.Fatalf("queue item = %q (%v), want still pending", status, err)
	}
	if f.canceler.count() != 0 {
		t.Fatal("a cancel was signalled after the policy withdrew it")
	}
}
