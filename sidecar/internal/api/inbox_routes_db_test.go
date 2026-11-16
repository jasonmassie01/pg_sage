package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/sre"
)

// ux-01a on real tables: one waiting row of each of the seven kinds, read
// through GET /api/v1/inbox, per-database grants, and get_inbox over the
// MCP server answering what the route answers for the same caller.

const inboxDeployment = "6f1c2a4e-0b7d-4c1e-9a55-1f2e3d4c5b6a"

// inboxInstance is a fresh bootstrapped database registered as name, with
// the SRE investigator (runbooks) and a trust ledger.
func inboxInstance(t *testing.T, mgr *fleet.DatabaseManager, reg *earned.Registry,
	name string) *pgxpool.Pool {
	t.Helper()
	pool := planDB(t, "inbox_api_"+name)
	st, err := sre.NewPostgresStore(pool, sre.DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	coord, err := sre.NewCoordinator(sre.CoordinatorDeps{Store: st,
		Runner: lockChainRunner{}, Config: sre.DefaultCoordinatorConfig(
			fmt.Sprintf("api-inbox:%s:%d", name, time.Now().UnixNano()))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := coord.Bind(context.Background()); err != nil {
		t.Fatal(err)
	}
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Status: &fleet.InstanceStatus{}, Investigations: sre.NewService(name, coord, st)})
	ledger, err := earned.NewPostgresStore(pool, inboxDeployment, name)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := earned.NewService(ledger, earned.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	reg.Register(name, earned.RegistryEntry{Service: svc})
	return pool
}

func inboxExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&id); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return id
}

// seedInbox stores one waiting row of each kind on db; it returns the
// action queue id and the runbook id.
func seedInbox(t *testing.T, mgr *fleet.DatabaseManager, pool *pgxpool.Pool,
	db string) (int64, string) {
	t.Helper()
	finding := inboxExec(t, pool, `INSERT INTO sage.findings (category, severity, title,
		detail, status, object_identifier) VALUES ('missing_index', 'warning', 'Missing',
		'{}', 'open', 'public.orders') RETURNING id`)
	action := inboxExec(t, pool, `INSERT INTO sage.action_queue (finding_id, proposed_sql,
		action_risk, status, action_type) VALUES ($1, 'SELECT 1', 'moderate', 'pending',
		'create_index') RETURNING id`, finding)
	inboxExec(t, pool, `INSERT INTO sage.plan (goal, origin, status, plan_hash, created_by)
		VALUES ('speed up', 'person', 'proposed', repeat('b', 64), 'alice') RETURNING id`)
	inboxExec(t, pool, `INSERT INTO sage.facts (fact_type, subject_kind, subject, source)
		VALUES ('append_only', 'table', 'public.events', 'model') RETURNING id`)
	inboxExec(t, pool, `INSERT INTO sage.guard_grant_requests (database_id, principal_id,
		capability, objects, duration_minutes, reason, expires_at) VALUES
		(gen_random_uuid(), 'agp_abcdefghijklmnopqrst', 'read_rows', '[]', 30, 'debug',
		now() + interval '1 hour') RETURNING id`)
	inboxExec(t, pool, `INSERT INTO sage.policy (database_id, version, doc, status,
		proposed_by) VALUES (NULL, -1, '{}', 'proposed', 'alice') RETURNING id`)
	inboxExec(t, pool, `INSERT INTO sage.sre_autonomy_proposals (deployment_id,
		database_name, id, family, action_class, from_level, to_level, evidence,
		evidence_sha256, status, expires_at) VALUES ($1, $2, gen_random_uuid(), 'index',
		'create_index', 1, 2, '{}', sha256('e'::bytea), 'pending',
		clock_timestamp() + interval '1 hour') RETURNING 1`, inboxDeployment, db)
	code, out := rbCall(t, sreRouter(t, mgr, testOperatorUser()), "POST",
		"/api/v1/databases/"+db+"/runbooks", `{"definition":`+rbDef+`}`)
	if code != 201 || out["id"] == nil {
		t.Fatalf("create runbook = %d %v", code, out)
	}
	return action, out["id"].(string)
}

type inboxResult struct {
	Items        []map[string]any `json:"items"`
	CountsByKind map[string]int   `json:"counts_by_kind"`
	Total        int              `json:"total"`
	LowerBound   bool             `json:"lower_bound"`
	Errors       []map[string]any `json:"errors"`
}

func readInbox(t *testing.T, h http.Handler, path string, user *auth.User) inboxResult {
	t.Helper()
	code, _, body := inboxGet(t, h, path, user)
	var res inboxResult
	if code != 200 || json.Unmarshal([]byte(body), &res) != nil {
		t.Fatalf("GET %s = %d %s", path, code, body)
	}
	return res
}

func itemOf(res inboxResult, kind string) map[string]any {
	for _, it := range res.Items {
		if it["kind"] == kind {
			return it
		}
	}
	return nil
}

func TestInboxRoutesDB_SevenKindsWithHrefsCountsAndRoles(t *testing.T) {
	mgr, reg := fleet.NewManager(config.DefaultConfig()), earned.NewRegistry(false)
	pool := inboxInstance(t, mgr, reg, "orders")
	action, runbook := seedInbox(t, mgr, pool, "orders")
	h := inboxMux(mgr, reg, nil)
	res := readInbox(t, h, "/api/v1/inbox", testViewerUser())
	if res.Total != 7 || len(res.Items) != 7 || res.LowerBound || len(res.Errors) != 0 {
		t.Fatalf("inbox %+v, want seven exact items", res)
	}
	for kind, n := range res.CountsByKind {
		if n != 1 {
			t.Fatalf("counts_by_kind[%s] = %d, want 1", kind, n)
		}
	}
	if got := itemOf(res, "change_approval")["href"]; got != fmt.Sprintf(
		"#/actions?tab=pending&queue=%d&database=orders", action) {
		t.Fatalf("change approval href %v", got)
	}
	if got := itemOf(res, "runbook_signature")["href"]; got !=
		"#/advanced/runbooks?database=orders&runbook="+runbook {
		t.Fatalf("runbook href %v", got)
	}
	for _, it := range res.Items {
		if it["actionable"] != false {
			t.Fatalf("viewer sees an actionable item: %v", it)
		}
	}
	op := readInbox(t, h, "/api/v1/inbox", testOperatorUser())
	if itemOf(op, "change_approval")["actionable"] != true ||
		itemOf(op, "runbook_signature")["actionable"] != false {
		t.Fatalf("operator actionable wrong: %+v", op.Items)
	}
	count := readInbox(t, h, "/api/v1/inbox/count", testViewerUser())
	if count.Total != 7 || count.Items != nil || count.CountsByKind["fact"] != 1 {
		t.Fatalf("count %+v", count)
	}
}

func TestInboxRoutesDB_ASignedRunbookLeavesTheInbox(t *testing.T) {
	mgr, reg := fleet.NewManager(config.DefaultConfig()), earned.NewRegistry(false)
	pool := inboxInstance(t, mgr, reg, "orders")
	_, runbook := seedInbox(t, mgr, pool, "orders")
	h := inboxMux(mgr, reg, nil)
	sign := fmt.Sprintf(`{"version":1,"content_hash":%q}`, rbHash(t, rbDef))
	if code, out := rbCall(t, sreRouter(t, mgr, testAdminUser()), "POST",
		"/api/v1/databases/orders/runbooks/"+runbook+"/sign", sign); code != 200 {
		t.Fatalf("sign = %d %v", code, out)
	}
	res := readInbox(t, h, "/api/v1/inbox/count", testAdminUser())
	if res.CountsByKind["runbook_signature"] != 0 || res.Total != 6 {
		t.Fatalf("after signing: %+v", res)
	}
}

func TestInboxRoutesDB_GrantsHideAndDoNotCountOtherDatabases(t *testing.T) {
	mgr, reg := fleet.NewManager(config.DefaultConfig()), earned.NewRegistry(false)
	for _, db := range []string{"orders", "billing"} {
		seedInbox(t, mgr, inboxInstance(t, mgr, reg, db), db)
	}
	src := &dbAuthzSource{}
	src.set(2, map[string]string{"orders": auth.RoleOperator})
	h := inboxMux(mgr, reg, src)
	if all := readInbox(t, h, "/api/v1/inbox/count", testAdminUser()); all.Total != 14 {
		t.Fatalf("admin total %d, want 14", all.Total)
	}
	scoped := readInbox(t, h, "/api/v1/inbox", dbOperator(2))
	if scoped.Total != 7 || scoped.LowerBound {
		t.Fatalf("scoped total %d, want orders' 7", scoped.Total)
	}
	for _, it := range scoped.Items {
		if it["database"] != "orders" {
			t.Fatalf("hidden database item %v", it)
		}
	}
	if c := readInbox(t, h, "/api/v1/inbox/count", dbOperator(2)); c.Total != 7 {
		t.Fatalf("scoped count %d, want 7", c.Total)
	}
}

func TestInboxRoutesDB_AFailingTrustSourceIsALowerBound(t *testing.T) {
	mgr, reg := fleet.NewManager(config.DefaultConfig()), earned.NewRegistry(false)
	pool := inboxInstance(t, mgr, reg, "orders")
	seedInbox(t, mgr, pool, "orders")
	broken, err := earned.NewPostgresStore(unreachablePool(t), inboxDeployment, "orders")
	if err != nil {
		t.Fatal(err)
	}
	svc, err := earned.NewService(broken, earned.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	reg.Register("orders", earned.RegistryEntry{Service: svc})
	res := readInbox(t, inboxMux(mgr, reg, nil), "/api/v1/inbox/count", testViewerUser())
	if res.Total != 6 || !res.LowerBound || len(res.Errors) != 1 ||
		res.Errors[0]["source"] != "autonomy_promotion" {
		t.Fatalf("count %+v, want total 6 as a lower bound naming autonomy_promotion", res)
	}
}

// inboxOnlyBackend is an MCP backend serving get_inbox only.
type inboxOnlyBackend struct{ mcp.InboxBackend }

func (inboxOnlyBackend) GetPolicy(context.Context, mcp.PolicyRequest) (mcp.PolicyResult,
	error) {
	return mcp.PolicyResult{}, mcp.ErrProductionDependencyUnavailable
}

func (inboxOnlyBackend) ProposePolicyChange(context.Context,
	mcp.PolicyProposalRequest) (mcp.PolicyProposalResult, error) {
	return mcp.PolicyProposalResult{}, mcp.ErrProductionDependencyUnavailable
}

func (inboxOnlyBackend) RequestChange(context.Context, mcp.ChangeRequest) (mcp.ChangeResult,
	error) {
	return mcp.ChangeResult{}, mcp.ErrProductionDependencyUnavailable
}

func (inboxOnlyBackend) GetLedger(context.Context, mcp.LedgerRequest) (mcp.LedgerResult,
	error) {
	return mcp.LedgerResult{}, mcp.ErrProductionDependencyUnavailable
}

func TestInboxRoutesDB_GetInboxAnswersWhatTheRouteAnswers(t *testing.T) {
	mgr, reg := fleet.NewManager(config.DefaultConfig()), earned.NewRegistry(false)
	for _, db := range []string{"orders", "billing"} {
		seedInbox(t, mgr, inboxInstance(t, mgr, reg, db), db)
	}
	src := &dbAuthzSource{}
	src.set(3, map[string]string{"orders": auth.RoleViewer})
	viewer := &auth.User{ID: 3, Email: "v@example.test", Role: auth.RoleViewer}
	httpRes := readInbox(t, inboxMux(mgr, reg, src), "/api/v1/inbox", viewer)
	server := mcp.NewServer(inboxOnlyBackend{InboxMCP(mgr, reg)})
	ctx := mcp.WithPrincipal(context.Background(), mcp.Principal{Actor: "user:3",
		Role: auth.RoleViewer, Databases: []string{"orders"}})
	raw := server.Handle(ctx, json.RawMessage(`{"jsonrpc":"2.0","id":1,`+
		`"method":"tools/call","params":{"name":"get_inbox","arguments":{}}}`))
	var rpc struct {
		Result struct {
			Structured inboxResult `json:"structuredContent"`
			IsError    bool        `json:"isError"`
		} `json:"result"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil || rpc.Result.IsError {
		t.Fatalf("get_inbox = %s (%v)", raw, err)
	}
	want, _ := json.Marshal(httpRes)
	got, _ := json.Marshal(rpc.Result.Structured)
	if string(want) != string(got) || httpRes.Total != 7 {
		t.Fatalf("get_inbox differs from the route:\nroute %s\nmcp   %s", want, got)
	}
	if strings.Contains(string(got), `"actionable":true`) {
		t.Fatal("get_inbox offered a decision to an agent")
	}
}
