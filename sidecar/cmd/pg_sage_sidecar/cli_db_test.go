package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/agentguard"
	"github.com/pg-sage/sidecar/internal/api"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcptoken"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/store"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// cli-01a (CHECK-01, CHECK-02): every decision-loop subcommand in process
// through runCLI against the real router (real session middleware, token
// store, approval cards, executor and why-not chain) on a fresh database.
// Run on PG14 and PG17.

type cliDB struct {
	pool    *pgxpool.Pool
	url     string
	table   string
	finding int
}

func newCLIDB(t *testing.T) *cliDB {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, "cli_loop"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	d := &cliDB{pool: pool, table: fmt.Sprintf("cli_%d", time.Now().UnixNano())}
	if _, err := pool.Exec(ctx, "CREATE TABLE public."+d.table+
		" (id int); INSERT INTO public."+d.table+" SELECT generate_series(1, 50)"); err != nil {
		t.Fatal(err)
	}
	d.url = cliDBSidecar(t, pool)
	return d
}

// cliDBSidecar serves the real router over one database ("orders") with
// an executor behind the unattended standing policy.
func cliDBSidecar(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Trust.Level = "advisory"
	exec := executor.New(pool, cfg, time.Time{}, func(string, string, ...any) {})
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	doc.BlastRadius.MaxTablesPerWindow = 1 << 30
	doc.RateLimits.MaxSelfInitiatedChangesPerWindow = 1 << 30
	doc.BlastRadius.Hygiene = policy.KindBudget{MaxTablesPerWindow: 1 << 30,
		MaxChangesPerWindow: 1 << 30}
	exec.EnableStandingPolicyDocument(doc, nil)
	exec.WithEmergencyStopCheck(func(context.Context) bool { return false })
	mgr := fleet.NewManager(cfg)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "orders", Pool: pool, Executor: exec,
		Status: &fleet.InstanceStatus{}})
	router, err := api.NewRouterFullRuntime(mgr, cfg, pool, &api.ActionDeps{Fleet: mgr}, nil,
		nil, &api.RuntimeDeps{ConfigController: config.NewConfigController(cfg, nil)},
		api.SessionAuthMiddleware(pool))
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	srv := httptest.NewServer(router)
	t.Cleanup(srv.Close)
	return srv.URL
}

// queue inserts an open stale-statistics finding and its pending ANALYZE
// at risk, returning the queue id.
func (d *cliDB) queue(t *testing.T, risk string) int {
	t.Helper()
	ctx := context.Background()
	sql := "ANALYZE public." + d.table
	if err := d.pool.QueryRow(ctx, `INSERT INTO sage.findings (category, severity,
		object_type, object_identifier, title, detail, recommendation, recommended_sql,
		status) VALUES ('stale_statistics', 'warning', 'table', $1,
		'Stale statistics on ' || $1, '{"n_mod_since_analyze": 5000}', 'Run ANALYZE', $2,
		'open') RETURNING id`, "public."+d.table, sql).Scan(&d.finding); err != nil {
		t.Fatal(err)
	}
	id, err := store.NewActionStore(d.pool).ProposeWithMetadata(ctx, nil, d.finding, sql, "",
		risk, store.ActionProposalMetadata{ActionType: "analyze_table",
			PolicyDecision: "queue_approval"})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (d *cliDB) executions(t *testing.T) int {
	t.Helper()
	var n int
	if err := d.pool.QueryRow(context.Background(), `SELECT count(*) FROM sage.action_log
		WHERE sql_executed = $1`, "ANALYZE public."+d.table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (d *cliDB) run(t *testing.T, token string, args ...string) (int, string, string) {
	t.Helper()
	return runCLIEnv(t, map[string]string{"SAGE_API_URL": d.url, "SAGE_API_TOKEN": token},
		"", args...)
}

// State transitions: plan lists the waiting change (exit 2), approve runs
// it as the token's owner, and plan then has nothing (exit 0).
func TestCLIDB_ApproveThenPlanNoLongerListsIt(t *testing.T) {
	d := newCLIDB(t)
	id := d.queue(t, "safe")
	token, owner := cliToken(t, d.pool, []string{mcptoken.ScopeRead, mcptoken.ScopeApprove})
	code, out, errOut := d.run(t, token, "plan")
	if code != statusExitNeedsPerson || !strings.Contains(out, "change_approval") ||
		!strings.Contains(out, strconv.Itoa(id)) {
		t.Fatalf("plan before: exit %d, out %q, err %q", code, out, errOut)
	}
	code, out, errOut = d.run(t, token, "approve", strconv.Itoa(id), "--yes")
	if code != planExitOK || !strings.Contains(out, "approved") {
		t.Fatalf("approve: exit %d, out %q, err %q", code, out, errOut)
	}
	var decidedBy int
	if err := d.pool.QueryRow(context.Background(), `SELECT COALESCE(decided_by, 0)
		FROM sage.action_queue WHERE id = $1`, id).Scan(&decidedBy); err != nil ||
		decidedBy != owner || d.executions(t) != 1 {
		t.Fatalf("decided_by %d (%v), %d runs; want owner %d and one run", decidedBy, err,
			d.executions(t), owner)
	}
	code, out, errOut = d.run(t, token, "plan")
	if code != statusExitClear || !strings.Contains(out, "Nothing needs you.") {
		t.Fatalf("plan after: exit %d, out %q, err %q", code, out, errOut)
	}
}

// Concurrent access: two approve runs for one change produce one
// execution; the other exits 1 with "no longer awaiting approval".
func TestCLIDB_ConcurrentApprovesRunOnce(t *testing.T) {
	d := newCLIDB(t)
	id := strconv.Itoa(d.queue(t, "safe"))
	token, _ := cliToken(t, d.pool, []string{mcptoken.ScopeRead, mcptoken.ScopeApprove})
	var wg sync.WaitGroup
	codes, errs := make([]int, 2), make([]string, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], _, errs[i] = d.run(t, token, "approve", id, "--yes")
		}(i)
	}
	wg.Wait()
	ok, refused := 0, 0
	for i, c := range codes {
		if c == planExitOK {
			ok++
		}
		if c == planExitRefused && strings.Contains(errs[i], "no longer awaiting approval") {
			refused++
		}
	}
	if ok != 1 || refused != 1 || d.executions(t) != 1 {
		t.Fatalf("exits %v, errors %q, %d runs; want one 0, one 1, one run", codes, errs,
			d.executions(t))
	}
}

// CHECK-02: an agent token gets 403 approval_reserved_for_humans from the
// approve route and the CLI prints it; a read-only operator token is
// refused for the missing scope; neither changes anything.
func TestCLIDB_AgentAndReadTokensCannotApprove(t *testing.T) {
	d := newCLIDB(t)
	id := strconv.Itoa(d.queue(t, "safe"))
	ctx := context.Background()
	agents := agentguard.NewStore(d.pool)
	p, err := agents.Create(ctx, agentguard.CreateRequest{Name: "cli-bot",
		Profile: "readonly-analyst", EnvCeiling: agentguard.EnvProd, CreatedBy: "cli@test.local"})
	if err != nil {
		t.Fatalf("agent principal: %v", err)
	}
	agentTok, err := agentguard.IssueToken(ctx, agents, mcptoken.NewStore(d.pool), p.ID,
		agentguard.TokenRequest{Name: "cli-bot-token", Scopes: []string{"read"},
			Databases: []string{"orders"}, ExpiresIn: time.Hour, CreatedBy: "cli@test.local"})
	if err != nil {
		t.Fatalf("agent token: %v", err)
	}
	code, _, errOut := d.run(t, agentTok.Secret, "approve", id, "--yes", "--database",
		"orders")
	if code != planExitAuth || !strings.Contains(errOut, "approval_reserved_for_humans") {
		t.Fatalf("agent token: exit %d, err %q; want 3 approval_reserved_for_humans", code,
			errOut)
	}
	raw, _ := http.NewRequest(http.MethodPost, d.url+"/api/v1/approvals/"+id+
		"/approve?database=orders", strings.NewReader(`{"card_hash":"x"}`))
	raw.Header.Set("Authorization", "Bearer "+agentTok.Secret)
	raw.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(raw)
	if err != nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("raw agent POST: %v %v, want 403", resp, err)
	}
	_ = resp.Body.Close()
	readTok, _ := cliToken(t, d.pool, []string{mcptoken.ScopeRead})
	code, _, errOut = d.run(t, readTok, "approve", id, "--yes")
	if code != planExitAuth || !strings.Contains(errOut, "approve scope") {
		t.Fatalf("read token: exit %d, err %q; want 3 naming the approve scope", code, errOut)
	}
	if code, _, _ := d.run(t, "pgs_mcp_not_a_real_token", "plan"); code != statusExitAuth {
		t.Fatalf("unknown token: exit %d, want 3", code)
	}
	if d.executions(t) != 0 {
		t.Fatalf("a refused token ran the change")
	}
}

// Boundary (D7): a high-tier change is refused by the server with the app
// link; the CLI exits 1 and nothing runs.
func TestCLIDB_HighTierIsApprovedInTheAppOnly(t *testing.T) {
	d := newCLIDB(t)
	id := strconv.Itoa(d.queue(t, "high"))
	token, _ := cliToken(t, d.pool, []string{mcptoken.ScopeRead, mcptoken.ScopeApprove})
	code, _, errOut := d.run(t, token, "approve", id, "--yes")
	if code != planExitRefused || !strings.Contains(errOut,
		"#/inbox?kind=change_approval&queue="+id) {
		t.Fatalf("high tier: exit %d, err %q; want 1 with the app link", code, errOut)
	}
	if d.executions(t) != 0 {
		t.Fatal("a capped approval ran the change")
	}
}

// explain prints the real ten-link chain; apply reports a pending change
// as awaiting approval and posts nothing.
func TestCLIDB_ExplainAndApply(t *testing.T) {
	d := newCLIDB(t)
	id := strconv.Itoa(d.queue(t, "safe"))
	token, _ := cliToken(t, d.pool, []string{mcptoken.ScopeRead})
	code, out, errOut := d.run(t, token, "explain", "--database", "orders", "--finding",
		strconv.Itoa(d.finding))
	if code != planExitOK || !strings.Contains(out, "emergency_stop") ||
		!strings.Contains(out, "observed") {
		t.Fatalf("explain: exit %d, out %q, err %q", code, out, errOut)
	}
	code, _, errOut = d.run(t, token, "apply", id)
	if code != planExitRefused || !strings.Contains(errOut, "awaiting approval") {
		t.Fatalf("apply pending: exit %d, err %q", code, errOut)
	}
	if d.executions(t) != 0 {
		t.Fatal("apply ran a change")
	}
}
