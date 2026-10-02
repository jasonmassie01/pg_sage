package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/sre"
	sreaction "github.com/pg-sage/sidecar/internal/sre/action"
	"github.com/pg-sage/sidecar/internal/sre/probes"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// Sage SRE M5 composed: CHECK-39 on the real mounted router with real
// sessions and the production MCP backend. sre_request_execution creates
// exactly one approval item in the database's existing queue, executes
// nothing (the blocking statement keeps running), and both action tools
// refuse a viewer.

type m5Chain struct {
	holderPID int
	done      chan error
}

// startM5ActiveChain runs a long statement holding ACCESS SHARE, with an
// ALTER TABLE and a reader queued behind it.
func startM5ActiveChain(t *testing.T, ctx context.Context, dsn string,
	pool *pgxpool.Pool) *m5Chain {
	t.Helper()
	table := fmt.Sprintf("sre_m5_mcp_%d", time.Now().UnixNano())
	ident := pgx.Identifier{table}.Sanitize()
	if _, err := pool.Exec(ctx, "CREATE TABLE "+ident+" (id int); INSERT INTO "+ident+
		" VALUES (1)"); err != nil {
		t.Fatalf("chain table: %v", err)
	}
	var conns []*pgx.Conn
	dial := func() *pgx.Conn {
		c, err := pgx.Connect(ctx, dsn)
		if err != nil {
			t.Fatalf("dial: %v", err)
		}
		conns = append(conns, c)
		return c
	}
	ch := &m5Chain{done: make(chan error, 1)}
	holder := dial()
	_ = holder.QueryRow(ctx, "SELECT pg_backend_pid()").Scan(&ch.holderPID)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := holder.Exec(context.Background(), "SELECT pg_sleep(30) FROM "+ident)
		ch.done <- err
	}()
	for deadline := time.Now().Add(5 * time.Second); ; {
		var state string
		_ = pool.QueryRow(ctx, "SELECT state FROM pg_stat_activity WHERE pid = $1",
			ch.holderPID).Scan(&state)
		if state == "active" || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for i, sql := range []string{"ALTER TABLE " + ident + " ADD COLUMN v int",
		"SELECT count(*) FROM " + ident} {
		c := dial()
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.Exec(context.Background(), sql) }()
		waitComposedWaiters(t, ctx, pool, i+1)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "SELECT pg_cancel_backend($1)", ch.holderPID)
		wg.Wait()
		for _, c := range conns {
			_ = c.Close(context.Background())
		}
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+ident)
	})
	return ch
}

type m5Fixture struct {
	pool     *pgxpool.Pool
	inv      sre.Investigation
	actions  *sreaction.ActionService
	router   http.Handler
	operator string
	viewer   string
	chain    *m5Chain
}

func newM5Fixture(t *testing.T) *m5Fixture {
	t.Helper()
	dsn := testdb.SkipUnlessLive(t)
	ctx := context.Background()
	c := composedConfig("standalone")
	c.MCP.Enabled, c.MCP.Transport = true, "http"
	installComposedGlobals(t, c)
	pool := openComposedPool(t, ctx, dsn)
	f := &m5Fixture{pool: pool, chain: startM5ActiveChain(t, ctx, dsn, pool)}
	svc, err := newSREInvestigator(sreInvestigatorDeps{control: pool, monitored: pool,
		runner: probes.NewRunner(pool, probes.Catalog(), probes.NewLimiter(1)),
		name:   "m5_db", runtimeKey: fmt.Sprintf("m5:%d", time.Now().UnixNano()),
		settings: c.SRE, logFn: func(string, string, ...any) {}})
	if err != nil {
		t.Fatalf("investigator: %v", err)
	}
	inv, _, err := svc.Coordinator().Start(ctx, sre.Trigger{CaseID: "case:m5",
		Kind: sre.TriggerLock, Subject: "incident m5", IdempotencyKey: "m5:lock"})
	if err != nil || svc.Coordinator().Investigate(ctx, inv.ID) != nil {
		t.Fatalf("investigation: %v", err)
	}
	f.inv = inv
	execCfg := config.DefaultConfig()
	execCfg.Trust.Level = "advisory"
	exec := executor.New(pool, execCfg, time.Time{}, func(string, string, ...any) {})
	exec.EnableStandingPolicyDocument(policy.StaffedProfile(), nil)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	settings := c.SRE.Actions
	settings.RequestApproval = false
	if f.actions, err = newSREActions(sreActionDeps{service: svc, monitored: pool,
		executor: exec, name: "m5_db", settings: settings,
		logFn: func(string, string, ...any) {}}); err != nil {
		t.Fatalf("actions: %v", err)
	}
	mgr := fleet.NewManager(c)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "m5_db", Pool: pool,
		Executor: exec, Status: &fleet.InstanceStatus{}, Investigations: svc,
		Actions: f.actions})
	f.router = m5Router(t, c, mgr, pool)
	f.operator = m5Session(t, pool, auth.RoleOperator)
	f.viewer = m5Session(t, pool, auth.RoleViewer)
	return f
}

// m5Router mounts the production MCP backend on the real router behind
// real session authentication.
func m5Router(t *testing.T, c *config.Config, mgr *fleet.DatabaseManager,
	pool *pgxpool.Pool) http.Handler {
	t.Helper()
	access := &fleetMCPAccess{manager: mgr, fallback: pool}
	gate := &fleetStandingPolicyGate{manager: mgr}
	backend, err := mcp.NewProductionBackend(mcp.ProductionDependencies{
		Gate: gate, Planner: mcp.DeterministicIntentPlanner{},
		Executor: &fleetIntentExecutor{access: access, gate: gate, cloneConfig: c.Clone,
			migrationFactory: configuredMCPMigrationRuntime},
		Policy: access, Ledger: access, Value: access, Guarantees: access,
		Investigations: access, Actions: access,
	})
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	runtime, err := mcp.NewRuntime(c.MCP, mcp.NewServer(backend), nil, nil)
	if err != nil {
		t.Fatalf("runtime: %v", err)
	}
	return api.NewRouterFullRuntime(mgr, c, pool, nil, nil, nil,
		&api.RuntimeDeps{MCPHandler: runtime.HTTPHandler()}, api.SessionAuthMiddleware(pool))
}

func m5Session(t *testing.T, pool *pgxpool.Pool, role string) string {
	t.Helper()
	ctx := context.Background()
	id, err := auth.CreateUser(ctx, pool, fmt.Sprintf("m5-%s-%d@test.local", role,
		time.Now().UnixNano()), "correct horse battery", role)
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	session, err := auth.CreateSession(ctx, pool, id)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	return session
}

// mcpTool calls one tool as session; it returns the HTTP status, the
// structured content and the JSON-RPC error.
func (f *m5Fixture) mcpTool(t *testing.T, session, tool string,
	args map[string]any) (int, map[string]any, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1,
		"method": "tools/call", "params": map[string]any{"name": tool, "arguments": args}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if session != "" {
		req.AddCookie(&http.Cookie{Name: "sage_session", Value: session})
	}
	w := httptest.NewRecorder()
	f.router.ServeHTTP(w, req)
	var resp struct {
		Result map[string]any `json:"result"`
		Error  map[string]any `json:"error"`
	}
	_ = json.Unmarshal(w.Body.Bytes(), &resp)
	content, _ := resp.Result["structuredContent"].(map[string]any)
	return w.Code, content, resp.Error
}

func (f *m5Fixture) queueRows(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.action_queue
		WHERE identity_key LIKE 'sre_proposal:%' AND finding_id IN (SELECT id
		FROM sage.findings WHERE detail->>'investigation_id' = $1)`,
		string(f.inv.ID)).Scan(&n); err != nil {
		t.Fatalf("count queue: %v", err)
	}
	return n
}

func TestComposedSRE_M5_MCPRequestExecutionQueuesOnceAndNeverExecutes(t *testing.T) {
	f := newM5Fixture(t)
	inv := map[string]any{"database": "m5_db", "investigation_id": string(f.inv.ID)}
	if code, _, _ := f.mcpTool(t, "", "sre_propose_action", inv); code !=
		http.StatusUnauthorized {
		t.Fatalf("no session = %d, want 401", code)
	}
	if _, _, rpcErr := f.mcpTool(t, f.viewer, "sre_propose_action", inv); rpcErr == nil ||
		rpcErr["code"] != float64(-32001) {
		t.Fatalf("viewer propose = %v, want -32001", rpcErr)
	}
	_, proposal, rpcErr := f.mcpTool(t, f.operator, "sre_propose_action", inv)
	if rpcErr != nil || proposal["state"] != "proposed" {
		t.Fatalf("operator propose = %v / %v", proposal, rpcErr)
	}
	target, _ := proposal["target"].(map[string]any)
	if target["pid"] != float64(f.chain.holderPID) {
		t.Fatalf("proposal target = %v, want the holder %d", target, f.chain.holderPID)
	}
	req := map[string]any{"database": "m5_db", "proposal_id": proposal["id"]}
	if _, _, rpcErr := f.mcpTool(t, f.viewer, "sre_request_execution", req); rpcErr == nil ||
		rpcErr["code"] != float64(-32001) {
		t.Fatalf("viewer request = %v, want -32001", rpcErr)
	}
	if n := f.queueRows(t); n != 0 {
		t.Fatalf("a refused viewer created %d approval items", n)
	}
	var queueID any
	for i := 0; i < 2; i++ {
		_, got, rpcErr := f.mcpTool(t, f.operator, "sre_request_execution", req)
		approval, _ := got["approval"].(map[string]any)
		if rpcErr != nil || got["state"] != "requested" || approval["queue_id"] == nil {
			t.Fatalf("operator request %d = %v / %v", i, got, rpcErr)
		}
		if queueID != nil && approval["queue_id"] != queueID {
			t.Fatalf("second request queued %v, first %v", approval["queue_id"], queueID)
		}
		queueID = approval["queue_id"]
	}
	if n := f.queueRows(t); n != 1 {
		t.Fatalf("approval items = %d, want exactly 1", n)
	}
	select {
	case err := <-f.chain.done:
		t.Fatalf("request_execution executed: the blocker ended (%v)", err)
	case <-time.After(300 * time.Millisecond):
	}
	var cancels int
	_ = f.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.action_log
		WHERE action_type = 'cancel_backend' AND sql_executed = $1`,
		fmt.Sprintf("SELECT pg_cancel_backend(%d)", f.chain.holderPID)).Scan(&cancels)
	if cancels != 0 {
		t.Fatalf("action_log records %d cancels after request_execution", cancels)
	}
}

func TestSREActionsWiringNeedsItsDependencies(t *testing.T) {
	if _, err := newSREActions(sreActionDeps{}); err == nil {
		t.Fatal("actions without a service, pool or executor were built")
	}
	if d := config.DefaultConfig().SRE.Actions; !d.Proposals || !d.RequestApproval {
		t.Fatalf("defaults = %+v, want proposals and approval requests on", d)
	}
}
