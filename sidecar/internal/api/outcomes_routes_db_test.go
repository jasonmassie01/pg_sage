package api

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/mcp"
	"github.com/pg-sage/sidecar/internal/outcomes"
)

// ux-02a on real tables: the route and get_outcomes return the rows the
// shared read returns, across two databases, newest first.

func outcomesSeed(t *testing.T, mgr *fleet.DatabaseManager, name string,
	ago time.Duration) int64 {
	t.Helper()
	pool := planDB(t, "outcomes_api_"+name)
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: name, Pool: pool,
		Config: config.DatabaseConfig{Name: name}, Status: &fleet.InstanceStatus{}})
	act := inboxExec(t, pool, `INSERT INTO sage.action_log (executed_at, action_type,
		sql_executed, rollback_sql, outcome) VALUES (now() - $1::interval, 'create_index',
		'CREATE INDEX i ON t (a)', 'DROP INDEX i', 'success') RETURNING id`, ago.String())
	inboxExec(t, pool, `INSERT INTO sage.action_outcome (action_log_id, action_class,
		predicted, prediction_method, verdict, observed) VALUES ($1, 'index_create',
		'{"method":"none"}', 'none', 'improved',
		'{"metric":"blocks_per_call","before":10,"after":5}') RETURNING action_log_id`, act)
	return act
}

func TestOutcomesRouteDB_MatchesTheSharedReadAcrossDatabases(t *testing.T) {
	mgr := fleet.NewManager(config.DefaultConfig())
	older := outcomesSeed(t, mgr, "orders", 2*time.Hour)
	newer := outcomesSeed(t, mgr, "billing", time.Hour)
	h := outcomesMux(mgr, nil)
	code, _, body := inboxGet(t, h, "/api/v1/outcomes?since=24h", testViewerUser())
	if code != 200 {
		t.Fatalf("GET = %d %s", code, body)
	}
	var got outcomes.Result
	if err := json.Unmarshal([]byte(body), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Rows) != 2 || got.Rows[0].Database != "billing" ||
		got.Rows[0].ActionID != newer || got.Rows[1].ActionID != older ||
		got.Rows[1].Database != "orders" || len(got.Errors) != 0 {
		t.Fatalf("rows %+v errors %+v, want billing then orders", got.Rows, got.Errors)
	}
	if got.Summary.Changes != 2 || got.Summary.MeasuredShare == nil ||
		*got.Summary.MeasuredShare != 1 || got.Rows[0].Measured.ChangePct != -50 {
		t.Fatalf("summary %+v, first %+v", got.Summary, got.Rows[0])
	}
	q := outcomes.Query{Since: 24 * time.Hour, Now: time.Now(), Limit: outcomes.DefaultLimit}
	direct, err := outcomes.Read(context.Background(), mgr.GetInstance("orders").Pool,
		"orders", q)
	if err != nil || len(direct.Rows) != 1 {
		t.Fatalf("direct read %+v %v", direct, err)
	}
	a, _ := json.Marshal(direct.Rows[0])
	b, _ := json.Marshal(got.Rows[1])
	if string(a) != string(b) {
		t.Fatalf("route row %s, shared read %s", b, a)
	}
	_, _, one := inboxGet(t, h, "/api/v1/outcomes?database=orders&verdict=improved",
		testViewerUser())
	var only outcomes.Result
	if err := json.Unmarshal([]byte(one), &only); err != nil || len(only.Rows) != 1 ||
		only.Rows[0].ActionID != older {
		t.Fatalf("?database=orders = %s", one)
	}
	viaMCP, err := OutcomesMCP(mgr).Outcomes(context.Background(), mcp.OutcomesRequest{
		Since: 24 * time.Hour, Limit: 1})
	raw, _ := json.Marshal(viaMCP)
	var m outcomes.Result
	if err != nil || json.Unmarshal(raw, &m) != nil || len(m.Rows) != 1 ||
		m.Rows[0].ActionID != newer || m.Summary.Changes != 2 {
		t.Fatalf("get_outcomes = %s %v", raw, err)
	}
}
