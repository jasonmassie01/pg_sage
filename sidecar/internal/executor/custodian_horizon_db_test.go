package executor

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/config"
	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-09: the executor's before/after freeze measurement (incident
// credit and the post-check) must read the horizon the custodian ranked
// the table by: the table's effective maximum (its reloption when lower
// than the setting) and the more advanced of its heap and TOAST. Before,
// it read the server settings and the heap alone, so a table the
// custodian proposed as red measured green and its near miss was never
// credited. No concurrency test: one read of one table's catalog row.

// horizonTable creates public.table with the storage options withOpts,
// one wide row (so a TOAST relation holds data), freezes it and burns
// burn XIDs.
func horizonTable(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table, withOpts string, burn int,
) {
	t.Helper()
	for _, sql := range []string{"DROP TABLE IF EXISTS public." + table + " CASCADE",
		"CREATE TABLE public." + table + " (id int, body text) WITH (" + withOpts + ")",
		"INSERT INTO public." + table + " SELECT 1, string_agg(md5(i::text), '')" +
			" FROM generate_series(1, 400) i",
		"VACUUM (FREEZE) public." + table} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public."+table+" CASCADE")
	})
	testdb.BurnXIDs(t, ctx, pool, burn)
}

func horizonExecutor(pool *pgxpool.Pool) *Executor {
	return New(pool, config.DefaultConfig(), zeroTime(), func(string, string, ...any) {})
}

// horizonAgeReason is why a test that bounds a table's XID age from above
// holds the exclusive server: the age counts the whole cluster's XIDs and
// the freeze stops at the cluster's horizon, and on the shared server
// other packages burn XIDs by the ten thousand and, on PostgreSQL 14, hold
// every database's horizon through each backend still logging in (CI,
// PG14: a table frozen just before read 161018 XIDs old).
const horizonAgeReason = "an upper bound on relfrozenxid age needs a cluster " +
	"no other package ages or holds back"

// A heap reloption lowers the maximum the horizon is measured against:
// 80,000 of 100,000 is red, as the custodian measured it.
func TestReadFreezeHorizonUsesTheTablesReloption(t *testing.T) {
	pool, ctx := exclusiveDB(t, horizonAgeReason)
	horizonTable(t, ctx, pool, "horizon_reloption",
		"autovacuum_enabled = false, autovacuum_freeze_max_age = 100000", 80_000)

	h, err := horizonExecutor(pool).readFreezeHorizon(ctx, "public.horizon_reloption")

	if err != nil {
		t.Fatalf("readFreezeHorizon: %v", err)
	}
	if h.xidMax != 100_000 || h.xidAge < 80_000 || h.xidAge > 90_000 || h.mxidMax <= 0 {
		t.Fatalf("horizon = %+v, want age ~80000 of the 100000 reloption", h)
	}
	if got := h.urgency(freeze.BufferThresholds(25)); got != freeze.UrgencyRed {
		t.Fatalf("urgency = %s, want red (the custodian's measure)", got)
	}
}

// A red TOAST relation (toast.autovacuum_freeze_max_age) under a green
// heap is the table's horizon: the owner's VACUUM is what freezes it.
func TestReadFreezeHorizonTakesTheMoreAdvancedTOAST(t *testing.T) {
	pool, ctx := requireDB(t)
	horizonTable(t, ctx, pool, "horizon_toast", "autovacuum_enabled = false,"+
		" toast.autovacuum_enabled = false, toast.autovacuum_freeze_max_age = 100000", 80_000)

	h, err := horizonExecutor(pool).readFreezeHorizon(ctx, "public.horizon_toast")

	if err != nil {
		t.Fatalf("readFreezeHorizon: %v", err)
	}
	if h.xidMax != 100_000 || h.xidAge < 80_000 {
		t.Fatalf("horizon = %+v, want the TOAST relation's ~80000 of 100000", h)
	}
	if got := h.urgency(freeze.BufferThresholds(25)); got != freeze.UrgencyRed {
		t.Fatalf("urgency = %s, want red", got)
	}
}

// Without reloptions the server settings remain the maxima (boundary:
// a 200M setting reloption equal to the setting changes nothing).
func TestReadFreezeHorizonWithoutReloptionUsesTheSettings(t *testing.T) {
	pool, ctx := exclusiveDB(t, horizonAgeReason)
	var xidMax, mxidMax int64
	if err := pool.QueryRow(ctx, `SELECT current_setting('autovacuum_freeze_max_age')::bigint,
		current_setting('autovacuum_multixact_freeze_max_age')::bigint`).
		Scan(&xidMax, &mxidMax); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	horizonTable(t, ctx, pool, "horizon_settings", "autovacuum_enabled = false", 10)

	h, err := horizonExecutor(pool).readFreezeHorizon(ctx, "public.horizon_settings")

	if err != nil || h.xidMax != xidMax || h.mxidMax != mxidMax || h.xidAge < 10 ||
		h.xidAge > 100_000 {
		t.Fatalf("horizon = %+v (%v), want a young table against %d/%d", h, err,
			xidMax, mxidMax)
	}
}

// A partitioned parent has no storage of its own (relfrozenxid 0, whose
// age() is 2^31-1): its horizon is an error, never a red measurement.
func TestReadFreezeHorizonOfAPartitionedParentIsAnError(t *testing.T) {
	pool, ctx := requireDB(t)
	for _, sql := range []string{"DROP TABLE IF EXISTS public.horizon_parts CASCADE",
		"CREATE TABLE public.horizon_parts (k int) PARTITION BY RANGE (k)"} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS public.horizon_parts")
	})

	h, err := horizonExecutor(pool).readFreezeHorizon(ctx, "public.horizon_parts")

	if err == nil || !strings.Contains(err.Error(), "freeze horizon of public.horizon_parts") {
		t.Fatalf("horizon = %+v, err = %v, want a freeze horizon error naming the table", h, err)
	}
}

// freezePostCheck runs the freeze post-check on target against a baseline
// measured at baselineAge.
func freezePostCheck(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, target string, baselineAge int64,
) (string, error) {
	t.Helper()
	baseline := custodianBaseline{horizon: &freezeHorizon{xidAge: baselineAge,
		xidMax: 200_000_000, mxidMax: 400_000_000}}
	return horizonExecutor(pool).verifyCustodianAction(ctx, CustodianProposal{
		Feature: "freeze", SQL: "VACUUM (FREEZE) " + target,
		TargetObjects: []string{target}}, baseline)
}

// The post-check reads the same horizon as the baseline: a freeze that
// left the table older than its baseline fails (a pinned horizon must not
// pass silently) and one that brought it back passes. One XID past the
// baseline is the boundary that fails (XIDs other sessions consume can
// only age the table between the reads, so the case is not flaky).
func TestFreezePostCheckComparesTheRankedHorizon(t *testing.T) {
	pool, ctx := requireDB(t)
	horizonTable(t, ctx, pool, "horizon_postcheck", "autovacuum_enabled = false,"+
		" toast.autovacuum_enabled = false", 1_000)
	h, err := horizonExecutor(pool).readFreezeHorizon(ctx, "public.horizon_postcheck")
	if err != nil {
		t.Fatalf("readFreezeHorizon: %v", err)
	}

	criterion, err := freezePostCheck(t, ctx, pool, "public.horizon_postcheck", h.xidAge-1)
	if criterion != "custodian_freeze" || err == nil ||
		!strings.Contains(err.Error(), "did not improve") {
		t.Fatalf("older than baseline: criterion=%q err=%v, want a failed custodian_freeze",
			criterion, err)
	}
	if criterion, err = freezePostCheck(t, ctx, pool, "public.horizon_postcheck",
		h.xidAge+1_000_000); criterion != "custodian_freeze" || err != nil {
		t.Fatalf("younger than baseline: criterion=%q err=%v, want a passed check",
			criterion, err)
	}
}

// A target that is gone after the action fails the post-check (fail
// closed) instead of reading as improved.
func TestFreezePostCheckOfAMissingTableFails(t *testing.T) {
	pool, ctx := requireDB(t)

	criterion, err := freezePostCheck(t, ctx, pool, "public.horizon_postcheck_gone", 1<<30)

	if criterion != "custodian_freeze" || err == nil {
		t.Fatalf("criterion=%q err=%v, want a failed custodian_freeze", criterion, err)
	}
}
