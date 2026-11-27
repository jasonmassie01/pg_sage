package autonomy

import (
	"context"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
)

// vacuum-05: the custodian reads the server's freeze table ages with each
// horizon, so a red table past them gets a plain (already aggressive)
// VACUUM; a hidden setting keeps VACUUM (FREEZE).

// No concurrent access tests: aggressiveAges is pure and the live scan is
// one read-only transaction.

func TestAggressiveAgesUnreportedSettingIsUnknown(t *testing.T) {
	age := int64(150_000_000)
	for name, pair := range map[string][2]*int64{
		"xid hidden": {nil, &age}, "multixact hidden": {&age, nil}, "both hidden": {nil, nil},
	} {
		if got := aggressiveAges(pair[0], pair[1], 200_000_000, 400_000_000); got.Known {
			t.Fatalf("%s: ages %+v, want unknown", name, got)
		}
	}
	got := aggressiveAges(&age, &age, 200_000_000, 400_000_000)
	if !got.Known || got.XID != 150_000_000 || got.MultiXact != 150_000_000 {
		t.Fatalf("reported settings: ages %+v, want 150M/150M", got)
	}
}

// The adapter rows of older tests carry no settings: their freeze stays
// VACUUM (FREEZE).
func TestScanHorizonRowWithoutSettingsKeepsFreeze(t *testing.T) {
	row, err := scanHorizonRow(adapterRow{values: []any{"public", "orders", int64(190),
		int64(200), int64(1), int64(400), 0.0}})
	if err != nil {
		t.Fatalf("scanHorizonRow: %v", err)
	}
	if row.sample.Aggressive.Known {
		t.Fatalf("ages %+v from a row without settings, want unknown", row.sample.Aggressive)
	}
}

func TestPostgresFreezeHorizonsReadAggressiveAgesLive(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	const table = "autonomy_aggressive_probe"
	if _, err := pool.Exec(ctx, "CREATE TABLE IF NOT EXISTS "+table+" (id bigint)"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	t.Cleanup(func() { _, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table) })
	var xidTable, xidMax, mxidTable, mxidMax int64
	if err := pool.QueryRow(ctx, `SELECT
		current_setting('vacuum_freeze_table_age')::bigint,
		current_setting('autovacuum_freeze_max_age')::bigint,
		current_setting('vacuum_multixact_freeze_table_age')::bigint,
		current_setting('autovacuum_multixact_freeze_max_age')::bigint`).
		Scan(&xidTable, &xidMax, &mxidTable, &mxidMax); err != nil {
		t.Fatalf("read settings: %v", err)
	}
	want := freeze.AggressiveVacuumAges(xidTable, xidMax, mxidTable, mxidMax)
	custodian := NewPostgresFreezeCustodian(pool, "testdb", 25)
	_, rows, err := custodian.readAges(ctx)
	if err != nil {
		t.Fatalf("readAges: %v", err)
	}
	for _, row := range rows {
		if row.sample.Table != table {
			continue
		}
		if !row.sample.Aggressive.Known || row.sample.Aggressive != want {
			t.Fatalf("probe ages %+v, want %+v", row.sample.Aggressive, want)
		}
		sql := freeze.VacuumSQL(row.sample.Schema, row.sample.Table, want.XID,
			row.sample.MultiXactAge, row.sample.Aggressive)
		if strings.Contains(sql, "FREEZE") {
			t.Fatalf("SQL at the live freeze table age = %q, want plain VACUUM", sql)
		}
		return
	}
	t.Fatalf("probe table %s not among %d horizons", table, len(rows))
}
