package main

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/executor"
	"github.com/pg-sage/sidecar/internal/policy"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-09 error propagation: a sidecar role that may not VACUUM the red
// table must not pass silently. A VACUUM by such a role returns with only
// a WARNING, so since vacuum-06 the maintenance precheck withholds it
// before it runs: Route returns the withhold, the decision is blocked
// with capability_missing and its remedy, no action is recorded as run,
// the table stays red and the next scan proposes it again.

// v09RolePool creates a login role that owns nothing and may write
// pg_sage's own bookkeeping (as the sidecar's role does), and returns a
// pool connected as it.
func v09RolePool(t *testing.T, admin *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	role := fmt.Sprintf("v09_noowner_%d", time.Now().UnixNano())
	rq := pgx.Identifier{role}.Sanitize()
	v02Exec(t, admin, "CREATE ROLE "+rq+" LOGIN NOSUPERUSER PASSWORD 'v09-test'")
	v02Exec(t, admin, "GRANT USAGE ON SCHEMA sage TO "+rq+
		"; GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA sage TO "+rq+
		"; GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA sage TO "+rq)
	parsed, err := url.Parse(testdb.SkipUnlessLive(t))
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	parsed.User = url.UserPassword(role, "v09-test")
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect as %s: %v", role, err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP OWNED BY "+rq+"; DROP ROLE IF EXISTS "+rq)
	})
	return pool
}

// v09RoleRouter is v02Router with the executor connected as pool's role.
func v09RoleRouter(t *testing.T, admin, pool *pgxpool.Pool) executorProposalRouter {
	t.Helper()
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ex := autonomousExecutor(pool)
	if err := newAutonomyLedgers(true).install(context.Background(), ex, autonomyBinding{
		database: "testdb", control: admin, monitored: pool,
		settings: config.DefaultConfig().SRE.Autonomy}); err != nil {
		t.Fatalf("install ledger: %v", err)
	}
	ex.EnableStandingPolicyDocument(doc, nil)
	return executorProposalRouter{executor: ex,
		isReplica: func(context.Context) bool { return false }}
}

func TestFreezeRouteWithoutVacuumPrivilegeIsWithheldNotSilent(t *testing.T) {
	admin := autonomyPool(t)
	ctx := context.Background()
	const table = "public.vacuum09_noprivilege"
	const sql = `VACUUM (FREEZE) "public"."vacuum09_noprivilege"`
	redReloptionTable(t, admin, table)
	custodian := autonomy.NewPostgresFreezeCustodian(admin, "testdb", 25)
	proposal := v09OneFreeze(t, v09Scan(t, custodian), table, sql)
	before, _ := v09Ages(t, admin, table)
	since := v02Since(t, admin)

	err := v09RoleRouter(t, admin, v09RolePool(t, admin)).Route(ctx, proposal)

	if !errors.Is(err, executor.ErrMaintenancePrivilegeMissing) {
		t.Fatalf("Route error = %v, want the missing maintenance privilege", err)
	}
	r := v09Ledger(t, admin, table, since)
	var detail string
	if qerr := admin.QueryRow(ctx, `SELECT COALESCE(evidence#>>'{maintenance_privilege,detail}',
		'') FROM sage.decision WHERE id = $1`, r.decisionID).Scan(&detail); qerr != nil {
		t.Fatalf("read the decision's precheck evidence: %v", qerr)
	}
	if r.verdict != "blocked" || detail == "" || r.actionID != 0 || r.credits != 0 {
		t.Fatalf("decision %d: verdict=%q remedy=%q action=%d credits=%d, want blocked "+
			"with a remedy and nothing run", r.decisionID, r.verdict, detail, r.actionID,
			r.credits)
	}
	var runs int
	if qerr := admin.QueryRow(ctx, `SELECT count(*)::int FROM sage.action_log
		WHERE sql_executed = $1 AND outcome = 'success'`, sql).Scan(&runs); qerr != nil ||
		runs != 0 {
		t.Fatalf("successful runs of %s = %d (%v), want none", sql, runs, qerr)
	}
	if after, _ := v09Ages(t, admin, table); after < before {
		t.Fatalf("age(relfrozenxid) %d -> %d: the withheld freeze ran", before, after)
	}
	again, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("Scan after the withhold: %v", err)
	}
	if got := v09For(again, table); len(got) != 1 || got[0].SQL != sql {
		t.Fatalf("after the withhold the scan proposed %#v, want the red table again", got)
	}
}
