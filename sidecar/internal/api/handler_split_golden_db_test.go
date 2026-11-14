package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/auth"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/store"
)

// apiauth-30, integration: the golden suite on a fresh bootstrapped
// database, so ids and generations are the same on every run and server.

type splitFixture struct {
	pool                                  *pgxpool.Pool
	dbID, openID, warnID, suppressedID    int64
	actionLogID, pendingID, rejectQueueID int64
	incidentID                            string
}

func goldenDBPool(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	pool := auditPool(t, label)
	return pool
}

func seedSplitFixture(t *testing.T, pool *pgxpool.Pool) splitFixture {
	t.Helper()
	ctx := context.Background()
	fx := splitFixture{pool: pool}
	seedCharUser(t, pool)
	ds := store.NewDatabaseStore(pool, p2Key)
	id, err := ds.Create(ctx, store.DatabaseInput{
		Name: "primary", Host: "127.0.0.1", Port: 1, DatabaseName: "app",
		Username: "sage", Password: "golden-pw", SSLMode: "disable",
		MaxConnections: 2, TrustLevel: "advisory", ExecutionMode: "approval",
	}, 0)
	if err != nil {
		t.Fatalf("seed managed database: %v", err)
	}
	fx.dbID = int64(id)
	fx.openID = goldenInsertFinding(t, pool, "index_missing", "critical", "public.orders", "open")
	fx.warnID = goldenInsertFinding(t, pool, "table_bloat", "warning", "public.users", "open")
	fx.suppressedID = goldenInsertFinding(t, pool, "seq_scan", "info", "public.items", "suppressed")
	goldenExec(t, pool, `INSERT INTO sage.action_log (action_type, finding_id,
		sql_executed, rollback_sql, outcome)
		VALUES ('create_index', $1, 'CREATE INDEX golden_i ON public.orders (id)',
		'DROP INDEX public.golden_i', 'success')`, fx.openID)
	fx.actionLogID = goldenScalar(t, pool, `SELECT max(id) FROM sage.action_log`)
	as := store.NewActionStore(pool)
	dbID := int(fx.dbID)
	pending, err := as.Propose(ctx, &dbID, int(fx.openID),
		"CREATE INDEX CONCURRENTLY golden_p ON public.orders (id)",
		"DROP INDEX CONCURRENTLY golden_p", "moderate")
	if err != nil {
		t.Fatalf("seed pending action: %v", err)
	}
	reject, err := as.Propose(ctx, &dbID, int(fx.warnID),
		"VACUUM public.users", "", "safe")
	if err != nil {
		t.Fatalf("seed second pending action: %v", err)
	}
	fx.pendingID, fx.rejectQueueID = int64(pending), int64(reject)
	seedGoldenTelemetry(t, pool)
	var incident string
	if err := pool.QueryRow(ctx, `INSERT INTO sage.incidents
		(severity, root_cause, source, signal_ids, database_name)
		VALUES ('warning', 'golden cause', 'deterministic', ARRAY['sig1'], 'primary')
		RETURNING id::text`).Scan(&incident); err != nil {
		t.Fatalf("seed incident: %v", err)
	}
	fx.incidentID = incident
	return fx
}

// seedCharUser creates users up to injectCharRole's id 9, the actor that
// config writes record.
func seedCharUser(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for i := 0; ; i++ {
		id, err := auth.CreateUser(context.Background(), pool,
			fmt.Sprintf("split-%d@example.test", i), "local-disposable-fixture-only", "admin")
		if err != nil || id > 9 {
			t.Fatalf("seed user %d: id %d, %v", i, id, err)
		}
		if id == 9 {
			return
		}
	}
}

func seedGoldenTelemetry(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	goldenExec(t, pool, `INSERT INTO sage.snapshots (category, data)
		VALUES ('system', '{"connections": 7, "max_connections": 100}')`)
	goldenExec(t, pool, `INSERT INTO sage.health_history
		(recorded_at, database_name, health_score, findings_open, findings_critical)
		VALUES (now() - interval '1 hour', 'primary', 87, 2, 1)`)
}

func goldenInsertFinding(
	t *testing.T, pool *pgxpool.Pool, category, severity, object, status string,
) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(context.Background(), `INSERT INTO sage.findings
		(category, severity, object_type, object_identifier, title, detail,
		 recommendation, recommended_sql, status)
		VALUES ($1, $2, 'table', $3, 'golden ' || $1, '{"why": "golden"}',
		 'fix it', 'ANALYZE '||$3, $4) RETURNING id`,
		category, severity, object, status).Scan(&id)
	if err != nil {
		t.Fatalf("seed finding %s: %v", category, err)
	}
	return id
}

func goldenExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("seed: %v\n%s", err, sql)
	}
}

func goldenScalar(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var v int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("scalar: %v\n%s", err, sql)
	}
	return v
}

// goldenDBRouter serves the fixture as fleet databases on one pool.
func goldenDBRouter(
	t *testing.T, fx splitFixture, names ...string,
) (http.Handler, *fleet.DatabaseManager) {
	t.Helper()
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	for i, name := range names {
		mgr.RegisterInstance(&fleet.DatabaseInstance{
			Name: name, DatabaseID: int(fx.dbID) + i, Pool: fx.pool,
			Config:   config.DatabaseConfig{Name: name, TrustLevel: "advisory"},
			Executor: executor.New(fx.pool, cfg, time.Time{}, func(string, string, ...any) {}),
			Status:   &fleet.InstanceStatus{Connected: true},
		})
	}
	actions := &ActionDeps{Fleet: mgr}
	dbDeps := &DatabaseDeps{Store: store.NewDatabaseStore(fx.pool, p2Key), Fleet: mgr}
	return newTestRouter(mgr, cfg, fx.pool, actions, dbDeps, nil, nil, injectCharRole), mgr
}

// expand fills {db}, {open}, {warn}, {suppressed}, {log}, {pending},
// {reject} and {incident} with the fixture's ids.
func (fx splitFixture) expand(calls []goldenCall) []goldenCall {
	r := strings.NewReplacer("{db}", fmt.Sprint(fx.dbID), "{open}", fmt.Sprint(fx.openID),
		"{warn}", fmt.Sprint(fx.warnID), "{suppressed}", fmt.Sprint(fx.suppressedID),
		"{log}", fmt.Sprint(fx.actionLogID), "{pending}", fmt.Sprint(fx.pendingID),
		"{reject}", fmt.Sprint(fx.rejectQueueID), "{incident}", fx.incidentID)
	out := make([]goldenCall, len(calls))
	for i, c := range calls {
		c.path, c.body = r.Replace(c.path), r.Replace(c.body)
		out[i] = c
	}
	return out
}

func goldenReadCalls() []goldenCall {
	q := "?database=primary"
	return []goldenCall{
		{"findings list", "viewer", "GET", "/api/v1/findings" + q, ""},
		{"findings list all", "viewer", "GET", "/api/v1/findings", ""},
		{"findings list suppressed", "viewer", "GET",
			"/api/v1/findings?database=primary&status=suppressed", ""},
		{"findings stats", "viewer", "GET", "/api/v1/findings/stats" + q, ""},
		{"findings stats all", "viewer", "GET", "/api/v1/findings/stats?database=all", ""},
		{"finding detail", "viewer", "GET", "/api/v1/findings/{open}" + q, ""},
		{"finding detail missing", "viewer", "GET", "/api/v1/findings/999999" + q, ""},
		{"actions list", "viewer", "GET", "/api/v1/actions" + q, ""},
		{"action detail", "viewer", "GET", "/api/v1/actions/{log}" + q, ""},
		{"action detail missing", "viewer", "GET", "/api/v1/actions/999999" + q, ""},
		{"snapshot latest", "viewer", "GET",
			"/api/v1/snapshots/latest?database=primary&metric=system", ""},
		{"snapshot latest empty", "viewer", "GET",
			"/api/v1/snapshots/latest?database=primary&metric=locks", ""},
		{"snapshot history", "viewer", "GET",
			"/api/v1/snapshots/history?database=primary&metric=connections&hours=2", ""},
		{"fleet health", "viewer", "GET", "/api/v1/fleet/health?hours=3", ""},
		{"config get", "viewer", "GET", "/api/v1/config", ""},
		{"config get database", "viewer", "GET", "/api/v1/config" + q, ""},
		{"metrics database", "viewer", "GET", "/api/v1/metrics" + q, ""},
		{"incidents list", "viewer", "GET", "/api/v1/incidents" + q, ""},
		{"incidents active", "viewer", "GET", "/api/v1/incidents/active" + q, ""},
		{"incident detail", "viewer", "GET", "/api/v1/incidents/{incident}" + q, ""},
		{"pending", "operator", "GET", "/api/v1/actions/pending" + q, ""},
		{"pending count", "operator", "GET", "/api/v1/actions/pending/count" + q, ""},
		{"pending by finding", "operator", "GET",
			"/api/v1/findings/{open}/pending-actions" + q, ""},
		{"managed list", "admin", "GET", "/api/v1/databases/managed", ""},
		{"managed get", "admin", "GET", "/api/v1/databases/managed/{db}", ""},
	}
}

func goldenWriteCalls() []goldenCall {
	q := "?database=primary"
	cfgPath := "/api/v1/config/databases/{db}"
	return []goldenCall{
		{"suppress", "operator", "POST", "/api/v1/findings/{warn}/suppress" + q, "{}"},
		{"suppress again", "operator", "POST", "/api/v1/findings/{warn}/suppress" + q, "{}"},
		{"unsuppress", "operator", "POST", "/api/v1/findings/{warn}/unsuppress" + q, "{}"},
		{"suppress implicit", "operator", "POST", "/api/v1/findings/{warn}/suppress", "{}"},
		{"unsuppress implicit", "operator", "POST", "/api/v1/findings/{warn}/unsuppress", "{}"},
		{"config db get", "admin", "GET", cfgPath, ""},
		{"config db put", "admin", "PUT", cfgPath,
			`{"expected_generation":1,"trust.level":"advisory","execution_mode":"manual"}`},
		{"config db put stale", "admin", "PUT", cfgPath,
			`{"expected_generation":1,"execution_mode":"approval"}`},
		{"config db put rules", "admin", "PUT", cfgPath,
			`{"expected_generation":2,"analyzer.disabled_rules":["seq_scan_heavy"]}`},
		{"config db get after", "admin", "GET", cfgPath, ""},
		{"config db delete", "admin", "DELETE",
			cfgPath + "/analyzer.disabled_rules?expected_generation=2", ""},
		{"config db delete stale", "admin", "DELETE",
			cfgPath + "/analyzer.disabled_rules?expected_generation=2", ""},
		{"config audit", "admin", "GET", "/api/v1/config/audit?limit=2", ""},
		{"reject", "operator", "POST", "/api/v1/actions/{reject}/reject" + q,
			`{"reason":"golden"}`},
		{"reject again", "operator", "POST", "/api/v1/actions/{reject}/reject" + q,
			`{"reason":"x"}`},
		{"rollback", "operator", "POST", "/api/v1/actions/{log}/rollback" + q, "{}"},
		{"incident resolve", "operator", "POST",
			"/api/v1/incidents/{incident}/resolve" + q, `{"reason":"golden fixed"}`},
		{"incident resolve again", "operator", "POST",
			"/api/v1/incidents/{incident}/resolve" + q, `{"reason":"again"}`},
		{"emergency stop", "operator", "POST", "/api/v1/emergency-stop" + q, "{}"},
		{"emergency stop again", "operator", "POST", "/api/v1/emergency-stop" + q, "{}"},
		{"resume", "operator", "POST", "/api/v1/resume" + q, "{}"},
	}
}

func goldenManagedWriteCalls() []goldenCall {
	return []goldenCall{
		{"managed create", "admin", "POST", "/api/v1/databases/managed",
			`{"name":"golden2","host":"127.0.0.1","port":1,"database_name":"app2",` +
				`"username":"sage","password":"pw2","sslmode":"disable",` +
				`"trust_level":"observation","execution_mode":"approval"}`},
		{"managed create duplicate", "admin", "POST", "/api/v1/databases/managed",
			`{"name":"golden2","host":"127.0.0.1","port":1,"database_name":"app2",` +
				`"username":"sage","password":"pw2","sslmode":"disable",` +
				`"trust_level":"observation","execution_mode":"approval"}`},
		{"managed update", "admin", "PUT", "/api/v1/databases/managed/{db}",
			`{"name":"primary","host":"127.0.0.1","port":1,"database_name":"app",` +
				`"username":"sage","sslmode":"disable","max_connections":3,` +
				`"trust_level":"advisory","execution_mode":"manual"}`},
		{"managed test", "admin", "POST", "/api/v1/databases/managed/{db}/test", ""},
		{"managed test json", "admin", "POST", "/api/v1/databases/managed/{db}/test", "{}"},
		{"managed test preview", "admin", "POST", "/api/v1/databases/managed/{db}/test",
			`{"host":"127.0.0.1","port":1,"database_name":"app","username":"sage"}`},
	}
}

func TestHandlerSplitGolden_DB(t *testing.T) {
	fx := seedSplitFixture(t, goldenDBPool(t, "api_handler_golden"))
	h, _ := goldenDBRouter(t, fx, "primary")
	checkHandlerGolden(t, "db_reads.json", runGoldenCalls(h, fx.expand(goldenReadCalls())))
	checkHandlerGolden(t, "db_writes.json", runGoldenCalls(h,
		fx.expand(append(goldenWriteCalls(), goldenManagedWriteCalls()...))))
	fleetH, _ := goldenDBRouter(t, fx, "primary", "replica")
	checkHandlerGolden(t, "db_fleet.json", runGoldenCalls(fleetH, fx.expand([]goldenCall{
		{"fleet findings all", "viewer", "GET", "/api/v1/findings?database=all", ""},
		{"fleet stats all", "viewer", "GET", "/api/v1/findings/stats?database=all", ""},
		{"fleet health all", "viewer", "GET", "/api/v1/fleet/health?database=all", ""},
		{"fleet pending by finding", "operator", "GET",
			"/api/v1/findings/{open}/pending-actions", ""},
		{"fleet incidents", "viewer", "GET", "/api/v1/incidents", ""},
	})))
}

// Nil/empty: a fleet with no databases answers the empty findings body.
func TestHandlerSplitGolden_EmptyFleet(t *testing.T) {
	cfg := config.DefaultConfig()
	cfg.Mode = "fleet"
	mgr := fleet.NewManager(cfg)
	h := newTestRouter(mgr, cfg, nil, &ActionDeps{Fleet: mgr}, nil, nil, nil, injectCharRole)
	recs := runGoldenCalls(h, []goldenCall{
		{"findings empty fleet", "viewer", "GET", "/api/v1/findings", ""},
		{"stats empty fleet", "viewer", "GET", "/api/v1/findings/stats", ""},
		{"actions empty fleet", "viewer", "GET", "/api/v1/actions", ""},
		{"incidents empty fleet", "viewer", "GET", "/api/v1/incidents", ""},
		{"health empty fleet", "viewer", "GET", "/api/v1/fleet/health", ""},
		{"snapshot empty fleet", "viewer", "GET", "/api/v1/snapshots/latest", ""},
		{"suppress empty fleet", "operator", "POST", "/api/v1/findings/1/suppress", "{}"},
	})
	checkHandlerGolden(t, "empty_fleet.json", recs)
	body, _ := recs[0].Body.(map[string]any)
	if recs[0].Status != http.StatusOK || body["total"] != float64(0) ||
		fmt.Sprint(body["findings"]) != "[]" {
		t.Fatalf("empty fleet findings = %d %v, want the empty response", recs[0].Status, body)
	}
}

// Nil manager and nil store where a handler accepts one.
func TestHandlerSplit_NilDependencies(t *testing.T) {
	controller := config.NewConfigController(config.DefaultConfig(), nil)
	rec := serveGolden(withCharRole(configReadOnlyDBGetHandler(nil, controller)),
		goldenCall{"nil mgr", "admin", "GET", "/api/v1/config/databases/1", ""})
	if rec.Status != http.StatusBadRequest {
		// The pattern {id} is not bound outside a mux: today's 400.
		t.Fatalf("unbound id = %d %v, want 400", rec.Status, rec.Body)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /x/{id}", configReadOnlyDBGetHandler(nil, controller))
	mux.Handle("GET /p/{id}", findingPendingActionsHandler(&ActionDeps{}))
	rec = serveGolden(mux, goldenCall{"nil mgr", "admin", "GET", "/x/1", ""})
	if rec.Status != http.StatusServiceUnavailable ||
		fmt.Sprint(rec.Body) != "map[error:fleet manager is unavailable]" {
		t.Fatalf("nil manager = %d %v, want 503 fleet manager is unavailable", rec.Status, rec.Body)
	}
	rec = serveGolden(mux, goldenCall{"nil store", "operator", "GET", "/p/5", ""})
	if rec.Status != http.StatusOK || fmt.Sprint(rec.Body) != "map[pending:[] total:0]" {
		t.Fatalf("nil store = %d %v, want 200 with no pending", rec.Status, rec.Body)
	}
}

func withCharRole(h http.Handler) http.Handler { return injectCharRole(h) }

// Concurrency: two puts with the same expected generation; one wins and
// the other gets today's conflict body. The generation moves once.
func TestHandlerSplit_ConcurrentDBConfigPut(t *testing.T) {
	fx := seedSplitFixture(t, goldenDBPool(t, "api_handler_cas"))
	h, _ := goldenDBRouter(t, fx, "primary")
	path := fmt.Sprintf("/api/v1/config/databases/%d", fx.dbID)
	var wg sync.WaitGroup
	recs := make([]goldenRecord, 2)
	for i, mode := range []string{"manual", "auto"} {
		wg.Add(1)
		go func(i int, mode string) {
			defer wg.Done()
			recs[i] = serveGolden(h, goldenCall{mode, "admin", "PUT", path,
				`{"expected_generation":1,"execution_mode":"` + mode + `"}`})
		}(i, mode)
	}
	wg.Wait()
	codes := map[int]int{}
	for _, rec := range recs {
		codes[rec.Status]++
		if rec.Status == http.StatusConflict &&
			!strings.Contains(fmt.Sprint(rec.Body), "generation") {
			t.Errorf("conflict body %v does not name the generation", rec.Body)
		}
	}
	if codes[http.StatusOK] != 1 || codes[http.StatusConflict] != 1 {
		t.Fatalf("statuses %v, want one 200 and one 409: %+v", codes, recs)
	}
	gen, err := store.NewConfigStore(fx.pool).GetGeneration(context.Background(), int(fx.dbID))
	if err != nil || gen != 2 {
		t.Fatalf("generation = %d (%v) after one winning put, want 2", gen, err)
	}
}

// State transitions: suppress, unsuppress, suppress leaves the finding
// suppressed; emergency stop then resume flips the latch both ways.
func TestHandlerSplit_StateTransitions(t *testing.T) {
	fx := seedSplitFixture(t, goldenDBPool(t, "api_handler_states"))
	h, mgr := goldenDBRouter(t, fx, "primary")
	steps := []struct{ verb, want string }{
		{"suppress", "suppressed"}, {"unsuppress", "open"}, {"suppress", "suppressed"},
	}
	for _, step := range steps {
		rec := serveGolden(h, goldenCall{step.verb, "operator", "POST",
			fmt.Sprintf("/api/v1/findings/%d/%s?database=primary", fx.warnID, step.verb), "{}"})
		var status string
		if err := fx.pool.QueryRow(context.Background(),
			`SELECT status FROM sage.findings WHERE id = $1`, fx.warnID).Scan(&status); err != nil {
			t.Fatal(err)
		}
		if rec.Status != http.StatusOK || status != step.want {
			t.Fatalf("%s: %d, status %q, want 200 and %q", step.verb, rec.Status, status, step.want)
		}
	}
	for _, step := range []struct {
		path string
		want bool
	}{{"/api/v1/emergency-stop", true}, {"/api/v1/resume", false}} {
		rec := serveGolden(h, goldenCall{step.path, "operator", "POST",
			step.path + "?database=primary", "{}"})
		row, _ := mgr.InstanceStatus("primary")
		if rec.Status != http.StatusOK || row.EmergencyStopped != step.want {
			body, _ := json.Marshal(rec.Body)
			t.Fatalf("%s: %d %s, stopped %v, want 200 and %v",
				step.path, rec.Status, body, row.EmergencyStopped, step.want)
		}
	}
}
