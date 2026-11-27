package main

import (
	"context"
	"fmt"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autonomy"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/policy"
)

// vacuum-02 end to end on real PostgreSQL: a prepared transaction that
// pins red tables reaches the ledger as one parked freeze_blocker
// decision, whatever the number of tables; the freeze runs only while it
// can still advance relfrozenxid (holder younger than 0.9x the table age).
// Both tests hold the exclusive server (freezeAgeReason). No concurrency
// test: one custodian per database runs in a serial loop.

// freezeAgeReason is why the freeze tests that measure XID ages hold the
// exclusive server (exclusiveAutonomyPool): a holder's share of a table's
// age, a routed freeze's post-check and its green-after credit count the
// whole cluster's XIDs and stop at the cluster's horizon. On the shared
// server other packages burn XIDs by the ten thousand and, on PostgreSQL
// 14, every backend still logging in holds each database's horizon back to
// the oldest transaction running anywhere (CI, PG14: a table frozen just
// before read 161018 XIDs old).
const freezeAgeReason = "XID ages and a freeze's advance of relfrozenxid need a " +
	"cluster no other package ages or holds back"

// v02RedBuffer makes tables red from an XID age of 1e-5 of
// autovacuum_freeze_max_age.
const v02RedBuffer = 99.999

func v02Exec(t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// v02Fixture creates schema with n analyzed one-row tables (the gate
// prices a freeze from reltuples), after skipping when
// the server has no prepared transactions; it returns the tables and the
// XID burn that turns them red.
func v02Fixture(t *testing.T, pool *pgxpool.Pool, schema string, n int) ([]string, int) {
	t.Helper()
	var slots int
	var maxAge int64
	if err := pool.QueryRow(context.Background(), `SELECT
		current_setting('max_prepared_transactions')::int,
		current_setting('autovacuum_freeze_max_age')::bigint`).Scan(&slots, &maxAge); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if slots == 0 {
		t.Skip("max_prepared_transactions = 0: PREPARE TRANSACTION is unavailable here")
	}
	v02Exec(t, pool, "DROP SCHEMA IF EXISTS "+schema+" CASCADE; CREATE SCHEMA "+schema)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	var tables []string
	for i := 0; i < n; i++ {
		table := fmt.Sprintf("%s.t%02d", schema, i)
		v02Exec(t, pool, "CREATE TABLE "+table+
			" (id int) WITH (autovacuum_enabled = false); INSERT INTO "+table+
			" VALUES (1); ANALYZE "+table)
		// Freezing re-dates relfrozenxid from the server's horizon now,
		// not from the RecentXmin the CREATE TABLE took.
		v02Exec(t, pool, "VACUUM (FREEZE) "+table)
		tables = append(tables, table)
	}
	return tables, int(float64(maxAge)*(100-v02RedBuffer)/100) + 600
}

func v02Burn(t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()
	v02Exec(t, pool, fmt.Sprintf(`DO $$ BEGIN
		FOR i IN 1..%d LOOP PERFORM pg_catalog.pg_current_xact_id(); COMMIT; END LOOP;
		END $$`, n))
}

// v02Prepare prepares a transaction that wrote table.
func v02Prepare(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	gid := fmt.Sprintf("vacuum02_e2e_%d", time.Now().UnixNano())
	conn, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	for _, sql := range []string{"BEGIN", "UPDATE " + table + " SET id = id + 1",
		"PREPARE TRANSACTION '" + gid + "'"} {
		if _, err := conn.Exec(context.Background(), sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "ROLLBACK PREPARED '"+gid+"'")
	})
}

// v02Router is a router over an executor whose earned-autonomy ledger
// carries over the autonomous safe tier, so a custodian freeze executes.
func v02Router(t *testing.T, pool *pgxpool.Pool) executorProposalRouter {
	t.Helper()
	doc := policy.UnattendedProfile()
	doc.MaintenanceWindows = []string{"always"}
	ex := autonomousExecutor(pool)
	if err := newAutonomyLedgers(true).install(context.Background(), ex, autonomyBinding{
		database: "testdb", control: pool, monitored: pool, ha: checkedHAMonitor(t, pool),
		settings: config.DefaultConfig().SRE.Autonomy}); err != nil {
		t.Fatalf("install ledger: %v", err)
	}
	ex.EnableStandingPolicyDocument(doc, nil)
	primary := func(context.Context) bool { return false }
	return executorProposalRouter{executor: ex, isReplica: primary}
}

// v02Route scans once and routes the proposals that name a schema table.
func v02Route(t *testing.T, pool *pgxpool.Pool, schema string) []autonomy.Proposal {
	t.Helper()
	ctx := context.Background()
	proposals, err := autonomy.NewPostgresFreezeCustodian(pool, "testdb", v02RedBuffer).Scan(ctx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	router := v02Router(t, pool)
	var routed []autonomy.Proposal
	for _, p := range proposals {
		if v02InSchema(p.TargetObjects, schema) == 0 {
			continue
		}
		routed = append(routed, p)
		if err := router.Route(ctx, p); err != nil {
			var last string
			_ = pool.QueryRow(ctx, `SELECT evidence::text FROM sage.decision
				ORDER BY id DESC LIMIT 1`).Scan(&last)
			t.Fatalf("Route %s %s: %v (last decision evidence %s)", p.Feature, p.ResponseKind,
				err, last)
		}
	}
	return routed
}

// v02InSchema counts the targets in schema (a holder pins every red
// table of the database, other tests' too).
func v02InSchema(targets []string, schema string) int {
	n := 0
	for _, target := range targets {
		if strings.HasPrefix(target, schema+".") {
			n++
		}
	}
	return n
}

func v02Since(t *testing.T, pool *pgxpool.Pool) time.Time {
	t.Helper()
	var now time.Time
	err := pool.QueryRow(context.Background(), "SELECT clock_timestamp()").Scan(&now)
	if err != nil {
		t.Fatal(err)
	}
	return now
}

// v02Counts are CHECK-01 and CHECK-02: the freeze_blocker decisions
// since, and the VACUUM rows (and failed ones) on the schema's tables.
func v02Counts(t *testing.T, pool *pgxpool.Pool, schema string, since time.Time) (int, int, int) {
	t.Helper()
	var decisions, vacuums, failed int
	err := pool.QueryRow(context.Background(), `SELECT
		(SELECT count(*) FROM sage.decision WHERE feature = 'freeze_blocker'
		   AND created_at > $1 AND risk_tier = 'high' AND verdict = 'parked'
		   AND evidence->>'action_type' = 'diagnose_freeze_blockers'),
		(SELECT count(*) FROM sage.action_log
		   WHERE sql_executed LIKE 'VACUUM (FREEZE) "' || $2 || '".%'),
		(SELECT count(*) FROM sage.action_log
		   WHERE sql_executed LIKE 'VACUUM (FREEZE) "' || $2 || '".%'
		     AND outcome IN ('failed', 'rollback_failed'))`, since, schema).
		Scan(&decisions, &vacuums, &failed)
	if err != nil {
		t.Fatalf("read ledger counts: %v", err)
	}
	return decisions, vacuums, failed
}

func TestFreezeHolderFiftyTablesOneDecisionNoFutileVacuum(t *testing.T) {
	pool := exclusiveAutonomyPool(t, freezeAgeReason)
	tables, burn := v02Fixture(t, pool, "v02a", 50)
	v02Prepare(t, pool, tables[0])
	v02Burn(t, pool, burn)
	v02Settle(t, pool, "v02a", 0.93, 100)
	since := v02Since(t, pool)

	routed := v02Route(t, pool, "v02a")

	if len(routed) != 1 || routed[0].Feature != "freeze_blocker" ||
		v02InSchema(routed[0].TargetObjects, "v02a") != len(tables) {
		t.Fatalf("routed = %#v, want one escalation over the 50 tables", routed)
	}
	decisions, vacuums, failed := v02Counts(t, pool, "v02a", since)
	if decisions != 1 || vacuums != 0 || failed != 0 {
		t.Fatalf("CHECK-01/02: freeze_blocker decisions=%d VACUUM rows=%d failed=%d, "+
			"want 1, 0, 0", decisions, vacuums, failed)
	}
}

// v02Settle consumes XIDs until this database's prepared transaction is
// at least lo of every schema table's XID age, failing once one reaches
// hi. The shares are measured, not computed from the burns (the horizon
// at each table's freeze dates its relfrozenxid), and each step burns what
// the measured gap still needs.
func v02Settle(t *testing.T, pool *pgxpool.Pool, schema string, lo, hi float64) {
	t.Helper()
	for step := 0; step < 20; step++ {
		var holder, oldest, youngest float64
		if err := pool.QueryRow(context.Background(), `SELECT h.age,
			max(age(c.relfrozenxid))::float8, min(age(c.relfrozenxid))::float8
			FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
			CROSS JOIN (SELECT max(age(transaction))::float8 AS age FROM pg_prepared_xacts
			            WHERE gid LIKE 'vacuum02_e2e_%' AND database = current_database()) h
			WHERE n.nspname = $1 AND c.relkind = 'r' GROUP BY h.age`, schema).
			Scan(&holder, &oldest, &youngest); err != nil {
			t.Fatalf("read the holder's shares: %v", err)
		}
		if holder >= hi*youngest {
			t.Fatalf("fixture drifted: holder is %.2f of a %s table's age, want under %.2f",
				holder/youngest, schema, hi)
		}
		need, ceiling := v02BurnToShare(holder, oldest, lo), 2_000_000
		if hi < 1 {
			ceiling = v02BurnToShare(holder, youngest, hi) - 1
		}
		if need == 0 {
			return
		}
		if ceiling < need {
			t.Fatalf("holder cannot reach %.2f of every %s table's age below %.2f: "+
				"needs %d XIDs, at most %d fit", lo, schema, hi, need, ceiling)
		}
		v02Burn(t, pool, min(need+20, ceiling))
	}
	t.Fatalf("holder never reached %.2f of every %s table's age", lo, schema)
}

// v02BurnToShare is how many XIDs raise a holder at holderAge to share of
// a table at tableAge (every XID ages both by one); 0 once it is there.
func v02BurnToShare(holderAge, tableAge, share float64) int {
	if share >= 1 || holderAge >= share*tableAge {
		return 0
	}
	return int(math.Ceil((share*tableAge - holderAge) / (1 - share)))
}

func TestFreezeHolderYoungerThanTheTableStillFreezes(t *testing.T) {
	pool := exclusiveAutonomyPool(t, freezeAgeReason)
	tables, burn := v02Fixture(t, pool, "v02b", 1)
	v02Burn(t, pool, burn)
	v02Prepare(t, pool, tables[0])
	v02Burn(t, pool, burn)
	v02Settle(t, pool, "v02b", 0.6, 0.85)
	since := v02Since(t, pool)

	routed := v02Route(t, pool, "v02b")

	if len(routed) != 2 || routed[0].Feature != "freeze_blocker" || routed[1].Feature != "freeze" {
		t.Fatalf("routed = %#v, want the escalation then the freeze", routed)
	}
	decisions, vacuums, failed := v02Counts(t, pool, "v02b", since)
	if decisions != 1 || vacuums != 1 || failed != 0 {
		t.Fatalf("CHECK-02: freeze_blocker decisions=%d VACUUM rows=%d failed=%d, "+
			"want 1, 1, 0", decisions, vacuums, failed)
	}
	var tableAge, holderAge, minAge int64
	var outcome string
	if err := pool.QueryRow(context.Background(), `SELECT
		(SELECT age(relfrozenxid)::bigint FROM pg_class WHERE oid = $1::regclass),
		(SELECT max(age(transaction))::bigint FROM pg_prepared_xacts
		 WHERE gid LIKE 'vacuum02_e2e_%' AND database = current_database()),
		current_setting('vacuum_freeze_min_age')::bigint,
		(SELECT outcome FROM sage.action_log WHERE sql_executed LIKE 'VACUUM (FREEZE) "v02b".%'
		 ORDER BY id DESC LIMIT 1)`, tables[0]).
		Scan(&tableAge, &holderAge, &minAge, &outcome); err != nil {
		t.Fatalf("read freeze result: %v", err)
	}
	if outcome != "success" || tableAge > holderAge+minAge {
		t.Fatalf("freeze outcome %q: table age %d, holder age %d (+%d), want success "+
			"and the table frozen up to the horizon", outcome, tableAge, holderAge, minAge)
	}
}
