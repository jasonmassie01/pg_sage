package main

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/executor"
)

// vacuum-09 end to end on real PostgreSQL: the freeze custodian's output
// for a red table is driven through executorProposalRouter.Route into
// sage.decision, sage.action_log and sage.incident_avoided, and the table
// is measured before and after. Before, the only live custodian test
// passed with zero proposals. Tables are made red by a 100,000 freeze
// maximum reloption and an 80,000 XID burn (redReloptionTable), so every
// other table stays green. The tests hold the exclusive server
// (freezeAgeReason): the post-check needs the freeze to lower the table's
// XID age, and the credit needs it under 50,000. No concurrency test: one
// custodian per database runs in a serial loop
// (TestFreezeCustodianIsBuiltOncePerRuntime pins that).

// v09MaxRouteTime bounds one routed freeze: the VACUUM of a one-row table
// takes milliseconds and the post-check waits at most the executor's
// statsSettle (3 s) for PostgreSQL 14's statistics collector.
const v09MaxRouteTime = 10 * time.Second

// v09Scan scans twice (the first read measures no rate, so no deadline)
// and returns the second scan's proposals.
func v09Scan(t *testing.T, custodian *autonomy.PostgresFreezeCustodian) []autonomy.Proposal {
	t.Helper()
	ctx := context.Background()
	if _, err := custodian.Scan(ctx); err != nil {
		t.Fatalf("first Scan (rate sample): %v", err)
	}
	proposals, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	return proposals
}

// v09For returns the proposals that target exactly target.
func v09For(proposals []autonomy.Proposal, target string) []autonomy.Proposal {
	var out []autonomy.Proposal
	for _, p := range proposals {
		if len(p.TargetObjects) == 1 && p.TargetObjects[0] == target {
			out = append(out, p)
		}
	}
	return out
}

// v09OneFreeze asserts the scan proposed exactly one freeze of target,
// with sql and a hard deadline ahead, and returns it.
func v09OneFreeze(
	t *testing.T, proposals []autonomy.Proposal, target, sql string,
) autonomy.Proposal {
	t.Helper()
	got := v09For(proposals, target)
	if len(got) != 1 || got[0].Feature != "freeze" || got[0].SQL != sql {
		t.Fatalf("proposals for %s = %#v, want exactly one %s", target, got, sql)
	}
	if d := got[0].Deadline; d == nil || !d.HardAt.After(time.Now()) {
		t.Fatalf("freeze deadline = %#v, want a hard deadline after now", d)
	}
	return got[0]
}

// v09Ages reads the XID age of table's heap and of its TOAST relation
// (-1 without one).
func v09Ages(t *testing.T, pool *pgxpool.Pool, table string) (int64, int64) {
	t.Helper()
	var heap, toast int64
	if err := pool.QueryRow(context.Background(), `SELECT age(c.relfrozenxid)::bigint,
		COALESCE((SELECT age(x.relfrozenxid)::bigint FROM pg_class x
		          WHERE x.oid = c.reltoastrelid), -1)
		FROM pg_class c WHERE c.oid = to_regclass($1)`, table).Scan(&heap, &toast); err != nil {
		t.Fatalf("read the ages of %s: %v", table, err)
	}
	return heap, toast
}

// v09Routed is what one routed freeze left in the ledger.
type v09Routed struct {
	decisionID, actionID                      int64
	verdict, risk, threat, outcome, postCheck string
	evidenceKeys, credits                     int
}

// v09Ledger reads the newest freeze decision on table since, its action,
// the action's post-check verdict and its incident credits.
func v09Ledger(t *testing.T, pool *pgxpool.Pool, table string, since time.Time) v09Routed {
	t.Helper()
	var r v09Routed
	err := pool.QueryRow(context.Background(), `SELECT d.id, d.verdict, d.risk_tier,
		COALESCE(d.evidence->>'deadline_threat', ''),
		(SELECT count(*) FROM jsonb_object_keys(d.evidence))::int,
		COALESCE(a.id, 0), COALESCE(a.outcome, ''), COALESCE(v.verdict, ''),
		(SELECT count(*) FROM sage.incident_avoided i WHERE i.action_log_id = a.id)::int
		FROM sage.decision d
		LEFT JOIN sage.action_log a ON a.decision_id = d.id
		LEFT JOIN sage.verification v ON v.action_log_id = a.id
		WHERE d.feature = 'freeze' AND d.created_at > $1
		  AND d.target_objects @> to_jsonb(ARRAY[$2::text])
		ORDER BY d.id DESC LIMIT 1`, since, table).Scan(&r.decisionID, &r.verdict, &r.risk,
		&r.threat, &r.evidenceKeys, &r.actionID, &r.outcome, &r.postCheck, &r.credits)
	if err != nil {
		t.Fatalf("read the freeze ledger of %s: %v", table, err)
	}
	return r
}

// v09RouteAndAssert routes proposal and asserts the ledger CHECKs: an
// executed decision at the contract's risk tier with deadline evidence,
// a successful action whose post-check passed, and one near-miss credit.
func v09RouteAndAssert(
	t *testing.T, pool *pgxpool.Pool, proposal autonomy.Proposal, table string,
) v09Routed {
	t.Helper()
	since := v02Since(t, pool)
	start := time.Now()
	if err := v02Router(t, pool).Route(context.Background(), proposal); err != nil {
		t.Fatalf("Route: %v", err)
	}
	if took := time.Since(start); took > v09MaxRouteTime {
		t.Fatalf("Route took %s, want under %s (post-check settles in 3 s)",
			took, v09MaxRouteTime)
	}
	contract, ok := executor.ContractForActionType("vacuum_table")
	if !ok {
		t.Fatal("no vacuum_table action contract")
	}
	r := v09Ledger(t, pool, table, since)
	if r.verdict != "execute" || r.risk != contract.BaseRiskTier || r.evidenceKeys == 0 ||
		(r.threat != "xid" && r.threat != "multixact") {
		t.Fatalf("decision %d: verdict=%q risk=%q threat=%q evidence keys=%d, want execute "+
			"at %q with deadline evidence", r.decisionID, r.verdict, r.risk, r.threat,
			r.evidenceKeys, contract.BaseRiskTier)
	}
	if r.actionID == 0 || r.outcome != "success" || r.postCheck != "success" {
		t.Fatalf("action %d: outcome=%q post-check=%q, want success/success",
			r.actionID, r.outcome, r.postCheck)
	}
	if r.credits != 1 {
		t.Fatalf("action %d earned %d near-miss credits, want 1 (red before, green after)",
			r.actionID, r.credits)
	}
	return r
}

// v09Credits counts every near-miss credit of sql's actions.
func v09Credits(t *testing.T, pool *pgxpool.Pool, sql string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*)::int
		FROM sage.incident_avoided i JOIN sage.action_log a ON a.id = i.action_log_id
		WHERE a.sql_executed = $1`, sql).Scan(&n); err != nil {
		t.Fatalf("count the credits of %s: %v", sql, err)
	}
	return n
}

// CHECK-01 (with the mutation in the report) and the state transitions:
// red is proposed once, executed, verified and credited once; the table
// is then green and the next scan proposes nothing.
func TestFreezeRouteRedTableExecutesVerifiesAndCredits(t *testing.T) {
	pool := exclusiveAutonomyPool(t, freezeAgeReason)
	const table = "public.vacuum09_red"
	const sql = `VACUUM (FREEZE) "public"."vacuum09_red"`
	redReloptionTable(t, pool, table)
	custodian := autonomy.NewPostgresFreezeCustodian(pool, "testdb", 25)
	proposal := v09OneFreeze(t, v09Scan(t, custodian), table, sql)
	before, _ := v09Ages(t, pool, table)
	if before < 75_000 {
		t.Fatalf("age(relfrozenxid) = %d before the freeze, want red (>= 75%% of 100000)",
			before)
	}

	v09RouteAndAssert(t, pool, proposal, table)

	after, _ := v09Ages(t, pool, table)
	if after >= before || after >= 50_000 {
		t.Fatalf("age(relfrozenxid) %d -> %d, want a drop to green (< 50000)", before, after)
	}
	again, err := custodian.Scan(context.Background())
	if err != nil {
		t.Fatalf("Scan after the freeze: %v", err)
	}
	if got := v09For(again, table); len(got) != 0 {
		t.Fatalf("the frozen table was proposed again: %#v", got)
	}
	if n := v09Credits(t, pool, sql); n != 1 {
		t.Fatalf("near-miss credits for %s = %d, want exactly 1", table, n)
	}
}

// A partitioned parent has no storage (relfrozenxid 0) and is never
// proposed; its red leaf is, and its freeze runs and verifies.
func TestFreezeRoutePartitionedParentNeverProposedLeafIs(t *testing.T) {
	pool := exclusiveAutonomyPool(t, freezeAgeReason)
	const parent, leaf = "public.vacuum09_parts", "public.vacuum09_parts_a"
	v02Exec(t, pool, "DROP TABLE IF EXISTS "+parent+" CASCADE")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+parent+" CASCADE")
	})
	v02Exec(t, pool, "CREATE TABLE "+parent+" (id int, k int) PARTITION BY RANGE (k)")
	v02Exec(t, pool, "CREATE TABLE "+leaf+" PARTITION OF "+parent+
		" FOR VALUES FROM (0) TO (100) WITH (autovacuum_enabled = false,"+
		" autovacuum_freeze_max_age = 100000)")
	v02Exec(t, pool, "INSERT INTO "+parent+" VALUES (1, 1)")
	v02Exec(t, pool, "VACUUM (FREEZE) "+leaf)
	v02Burn(t, pool, 80_000)
	proposals := v09Scan(t, autonomy.NewPostgresFreezeCustodian(pool, "testdb", 25))
	if got := v09For(proposals, parent); len(got) != 0 {
		t.Fatalf("the partitioned parent was proposed: %#v", got)
	}
	proposal := v09OneFreeze(t, proposals, leaf, `VACUUM (FREEZE) "public"."vacuum09_parts_a"`)
	v09RouteAndAssert(t, pool, proposal, leaf)
}

// A table whose TOAST relation is red (toast.autovacuum_freeze_max_age)
// while its heap is green is proposed as its owner; the owner's freeze
// brings the TOAST horizon back and earns the near-miss credit.
func TestFreezeRouteRedTOASTProposesAndCreditsTheOwner(t *testing.T) {
	pool := exclusiveAutonomyPool(t, freezeAgeReason)
	const table = "public.vacuum09_toast"
	v02Exec(t, pool, "DROP TABLE IF EXISTS "+table)
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	v02Exec(t, pool, "CREATE TABLE "+table+" (id int, body text) WITH ("+
		"autovacuum_enabled = false, toast.autovacuum_enabled = false,"+
		" toast.autovacuum_freeze_max_age = 100000)")
	v02Exec(t, pool, "INSERT INTO "+table+" SELECT 1, string_agg(md5(i::text || random()::text),"+
		" '') FROM generate_series(1, 400) i")
	v02Exec(t, pool, "VACUUM (FREEZE) "+table)
	v02Burn(t, pool, 80_000)
	if _, toast := v09Ages(t, pool, table); toast < 75_000 {
		t.Fatalf("TOAST age = %d, want red (>= 75%% of 100000)", toast)
	}
	custodian := autonomy.NewPostgresFreezeCustodian(pool, "testdb", 25)
	proposal := v09OneFreeze(t, v09Scan(t, custodian), table,
		`VACUUM (FREEZE) "public"."vacuum09_toast"`)

	v09RouteAndAssert(t, pool, proposal, table)

	if _, toast := v09Ages(t, pool, table); toast >= 50_000 {
		t.Fatalf("TOAST age = %d after the owner's freeze, want green (< 50000)", toast)
	}
}
