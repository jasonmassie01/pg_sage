package inbox

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/earned"
	"github.com/pg-sage/sidecar/internal/facts"
	"github.com/pg-sage/sidecar/internal/fleet"
	"github.com/pg-sage/sidecar/internal/review/modelgate"
	"github.com/pg-sage/sidecar/internal/schema"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// The six SQL-backed sources on real pg_sage tables (runbooks need the SRE
// investigator and are covered through the API in inbox_routes_db_test).

const testDeployment = "6f1c2a4e-0b7d-4c1e-9a55-1f2e3d4c5b6a"

func inboxDB(t *testing.T, label string) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, testdb.CreateDatabase(t, label))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	if err := schema.Bootstrap(ctx, pool); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func scalarInt(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
	return n
}

// seedSix stores one waiting row of each SQL-backed kind; it returns the
// action queue, fact and grant request ids.
func seedSix(t *testing.T, pool *pgxpool.Pool, db string) (int64, int64, int64) {
	t.Helper()
	finding := scalarInt(t, pool, `INSERT INTO sage.findings (category, severity, title,
		detail, status, object_identifier) VALUES ('missing_index', 'warning',
		'Missing index', '{}', 'open', 'public.orders') RETURNING id`)
	action := scalarInt(t, pool, `INSERT INTO sage.action_queue (finding_id, proposed_sql,
		action_risk, status, action_type, reason) VALUES ($1,
		'CREATE INDEX CONCURRENTLY i ON public.orders (a)', 'high_risk', 'pending',
		'create_index', 'seq scans on orders') RETURNING id`, finding)
	mustExec(t, pool, `INSERT INTO sage.plan (goal, origin, status, plan_hash, created_by)
		VALUES ('speed up orders', 'person', 'proposed', repeat('a', 64), 'alice')`)
	fact := scalarInt(t, pool, `INSERT INTO sage.facts (fact_type, subject_kind, subject,
		source, proposed_by, rationale) VALUES ('append_only', 'table', 'public.events',
		'model', 'model:x', 'no updates in 30 days') RETURNING id`)
	grant := scalarInt(t, pool, `INSERT INTO sage.guard_grant_requests (database_id,
		principal_id, capability, objects, duration_minutes, reason, expires_at)
		VALUES (gen_random_uuid(), 'agp_abcdefghijklmnopqrst', 'read_rows', '[]', 30,
		'debug a slow report', now() + interval '1 hour') RETURNING id`)
	mustExec(t, pool, `INSERT INTO sage.policy (database_id, version, doc, status,
		proposed_by, widening) VALUES (NULL, -1, '{}', 'proposed', 'mcp:token:1', true)`)
	mustExec(t, pool, `INSERT INTO sage.sre_autonomy_proposals (deployment_id,
		database_name, id, family, action_class, from_level, to_level, evidence,
		evidence_sha256, status, expires_at) VALUES ($1, $2, gen_random_uuid(),
		'index', 'create_index', 2, 3, '{}', sha256('e'::bytea), 'pending',
		clock_timestamp() + interval '1 hour')`, testDeployment, db)
	return action, fact, grant
}

func inboxFleet(t *testing.T, pool *pgxpool.Pool, db string) (*Service, *fleet.DatabaseManager) {
	t.Helper()
	mgr := fleet.NewManager(config.DefaultConfig())
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: db, Pool: pool,
		Status: &fleet.InstanceStatus{}})
	st, err := earned.NewPostgresStore(pool, testDeployment, db)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := earned.NewService(st, earned.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}
	reg := earned.NewRegistry(false)
	reg.Register(db, earned.RegistryEntry{Service: svc})
	return &Service{Sources: FleetSources(mgr, reg)}, mgr
}

func byKind(res Result) map[Kind]Item {
	out := map[Kind]Item{}
	for _, it := range res.Items {
		out[it.Kind] = it
	}
	return out
}

func TestInboxDB_EverySQLKindListsWithItsHref(t *testing.T) {
	pool := inboxDB(t, "inbox_six")
	action, fact, grant := seedSix(t, pool, "orders")
	svc, _ := inboxFleet(t, pool, "orders")
	res := svc.List(context.Background(), Request{Databases: []string{"orders"}})
	if res.Total != 6 || res.LowerBound || len(res.Errors) != 0 {
		t.Fatalf("result %+v, want six exact items", res)
	}
	got := byKind(res)
	wantHref := map[Kind]string{
		KindChangeApproval: fmt.Sprintf("#/actions?tab=pending&queue=%d&database=orders",
			action),
		KindFact: fmt.Sprintf("#/facts?database=orders&fact=%d", fact),
		KindAgentRequest: fmt.Sprintf(
			"#/agents?agent=agp_abcdefghijklmnopqrst&request=%d&database=orders", grant),
	}
	for k, want := range wantHref {
		if got[k].Href != want {
			t.Errorf("%s href = %q, want %q", k, got[k].Href, want)
		}
	}
	for _, k := range []Kind{KindChangeApproval, KindPlanApproval, KindAutonomyPromotion,
		KindFact, KindAgentRequest, KindPolicyException} {
		if res.CountsByKind[k] != 1 || got[k].Database != "orders" || got[k].Title == "" {
			t.Errorf("%s: count %d item %+v", k, res.CountsByKind[k], got[k])
		}
	}
	if res.CountsByKind[KindRunbookSignature] != 0 {
		t.Fatalf("no investigator: runbooks %d, want 0", res.CountsByKind[KindRunbookSignature])
	}
	if got[KindChangeApproval].Risk != RiskHigh || got[KindAutonomyPromotion].Risk != RiskHigh ||
		got[KindFact].Risk != RiskSafe || got[KindPolicyException].Risk != RiskHigh {
		t.Fatalf("risks %+v", got)
	}
	if got[KindChangeApproval].DueAt == nil || got[KindAgentRequest].DueAt == nil {
		t.Fatalf("expiring kinds need a due time: %+v", got)
	}
}

func TestInboxDB_DecidedItemsLeaveOnTheNextRead(t *testing.T) {
	pool := inboxDB(t, "inbox_leave")
	action, fact, grant := seedSix(t, pool, "orders")
	svc, _ := inboxFleet(t, pool, "orders")
	ctx := context.Background()
	req := Request{Databases: []string{"orders"}}
	mustExec(t, pool, `UPDATE sage.action_queue SET status = 'approved', decided_at = now()
		WHERE id = $1`, action)
	if res := svc.List(ctx, req); res.CountsByKind[KindChangeApproval] != 0 || res.Total != 5 {
		t.Fatalf("after approve: %+v", res.CountsByKind)
	}
	if _, err := facts.NewStore(pool).Decide(ctx, fact,
		facts.Decision{Confirm: true, Actor: "user:2"}); err != nil {
		t.Fatalf("confirm fact: %v", err)
	}
	if res := svc.List(ctx, req); res.CountsByKind[KindFact] != 0 || res.Total != 4 {
		t.Fatalf("after confirm: %+v", res.CountsByKind)
	}
	mustExec(t, pool, `UPDATE sage.guard_grant_requests SET status = 'denied',
		decided_at = now(), decided_by = 2 WHERE id = $1`, grant)
	if res := svc.List(ctx, req); res.CountsByKind[KindAgentRequest] != 0 || res.Total != 3 {
		t.Fatalf("after deny: %+v", res.CountsByKind)
	}
	mustExec(t, pool, `UPDATE sage.plan SET status = 'halted'`)
	res := svc.List(ctx, req)
	if it := byKind(res)[KindPlanApproval]; it.Risk != RiskHigh || res.Total != 3 {
		t.Fatalf("a halted plan stays as one high item: %+v", res.Items)
	}
}

func TestInboxDB_ExpiredRowsAreNotOffered(t *testing.T) {
	pool := inboxDB(t, "inbox_expired")
	seedSix(t, pool, "orders")
	mustExec(t, pool, `UPDATE sage.guard_grant_requests SET expires_at = now()`)
	mustExec(t, pool, `UPDATE sage.action_queue SET expires_at = now()`)
	// The ledger judges expiry by the sidecar's clock, so the row expired an
	// hour ago: a Docker server clock a little ahead must not matter.
	mustExec(t, pool, `UPDATE sage.sre_autonomy_proposals
		SET proposed_at = now() - interval '2 hours', expires_at = now() - interval '1 hour'`)
	svc, _ := inboxFleet(t, pool, "orders")
	res := svc.List(context.Background(), Request{Databases: []string{"orders"}})
	if res.CountsByKind[KindAgentRequest] != 0 || res.CountsByKind[KindChangeApproval] != 0 ||
		res.CountsByKind[KindAutonomyPromotion] != 0 || res.Total != 3 {
		t.Fatalf("expired rows offered: %+v", res.CountsByKind)
	}
	// The read expired nothing: the ledger row is still pending.
	if n := scalarInt(t, pool, `SELECT count(*) FROM sage.sre_autonomy_proposals
		WHERE status = 'pending'`); n != 1 {
		t.Fatalf("pending promotions after the read = %d, want 1 (a read never writes)", n)
	}
}

func TestInboxDB_UnconnectedAndUnknownDatabasesAreLowerBounds(t *testing.T) {
	pool := inboxDB(t, "inbox_offline")
	seedSix(t, pool, "orders")
	svc, mgr := inboxFleet(t, pool, "orders")
	mgr.RegisterInstance(&fleet.DatabaseInstance{Name: "offline",
		Status: &fleet.InstanceStatus{}})
	res := svc.List(context.Background(), Request{Databases: []string{"orders", "offline"}})
	if res.Total != 6 || !res.LowerBound {
		t.Fatalf("result %+v, want six items as a lower bound", res)
	}
	notConnected := 0
	for _, e := range res.Errors {
		if e.Database == "offline" && e.Error == "not connected" {
			notConnected++
		}
	}
	// Every kind but the ledger reads the pool; the ledger has no entry.
	if notConnected != 6 {
		t.Fatalf("errors %+v, want six not-connected errors for offline", res.Errors)
	}
}

func TestInboxDB_ABrokenTableFailsOnlyItsSource(t *testing.T) {
	pool := inboxDB(t, "inbox_broken")
	seedSix(t, pool, "orders")
	mustExec(t, pool, `ALTER TABLE sage.facts RENAME TO facts_gone`)
	svc, _ := inboxFleet(t, pool, "orders")
	res := svc.List(context.Background(), Request{Databases: []string{"orders"}})
	if res.Total != 5 || len(res.Errors) != 1 || res.Errors[0].Source != "fact" ||
		res.Errors[0].Error != "read failed; the sidecar log has the cause" {
		t.Fatalf("result %+v, want five items and one fact error", res)
	}
}

func TestInboxDB_ThousandItemsOverFiveHundredDatabases(t *testing.T) {
	pool := inboxDB(t, "inbox_scale")
	mustExec(t, pool, `INSERT INTO sage.facts (fact_type, subject_kind, subject, source)
		SELECT 'append_only', 'table', 'public.t' || g, 'model' FROM generate_series(1, 2) g`)
	mgr := fleet.NewManager(config.DefaultConfig())
	dbs := make([]string, 500)
	for i := range dbs {
		dbs[i] = fmt.Sprintf("tenant_%03d", i)
		mgr.RegisterInstance(&fleet.DatabaseInstance{Name: dbs[i], Pool: pool,
			Status: &fleet.InstanceStatus{}})
	}
	svc := &Service{Sources: map[Kind]Source{KindFact: Facts(mgr)}}
	start := time.Now()
	res := svc.List(context.Background(), Request{Databases: dbs, Kinds: []Kind{KindFact},
		Limit: MaxLimit})
	took := time.Since(start)
	t.Logf("1000 items across 500 databases in %s", took)
	if res.Total != 1000 || res.LowerBound {
		t.Fatalf("total %d lower %v errors %v", res.Total, res.LowerBound, res.Errors)
	}
	if took > 3*time.Second {
		t.Fatalf("took %s, over the 3s bound", took)
	}
}

// review-05a on a real server: a pending G-M1 row of the gate is one
// autonomy promotion beside the trust ledger's, and approving it (the
// gate's own route) takes it out of the inbox.
func TestInboxDB_ModelGatePromotionListsUntilApproved(t *testing.T) {
	pool := inboxDB(t, "inbox_gate")
	mustExec(t, pool, `INSERT INTO sage.review_gate (model, rule, tick, verdict, reason)
		VALUES ('m1', 'G-M1', now(), 'pending_approval', 'G-M1: 50 of 50 reviews')`)
	svc, _ := inboxFleet(t, pool, "shop")
	res := svc.List(context.Background(), Request{Databases: []string{"shop"},
		Kinds: []Kind{KindAutonomyPromotion}})
	if res.Total != 1 || len(res.Errors) != 0 || res.Items[0].Source != "sage.review_gate" ||
		res.Items[0].Risk != RiskHigh || res.Items[0].Why != "G-M1: 50 of 50 reviews" {
		t.Fatalf("result %+v, want the gate's one promotion", res)
	}
	id := scalarInt(t, pool, `SELECT id FROM sage.review_gate`)
	if _, err := modelgate.New(pool, "shop", modelgate.Options{}).Approve(
		context.Background(), id, "admin"); err != nil {
		t.Fatalf("approve: %v", err)
	}
	res = svc.List(context.Background(), Request{Databases: []string{"shop"},
		Kinds: []Kind{KindAutonomyPromotion}})
	if res.Total != 0 || len(res.Errors) != 0 {
		t.Fatalf("after the approval: %+v, want an empty inbox", res)
	}
}
