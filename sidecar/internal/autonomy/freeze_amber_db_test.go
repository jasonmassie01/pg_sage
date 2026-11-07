package autonomy

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/pg-sage/sidecar/internal/custodian/freeze"
	"github.com/pg-sage/sidecar/internal/testdb"
)

// vacuum-07 on real PostgreSQL: a table the analyzer tuned to scale
// factor 0.01 (its 50M-row band; the custodian never read the row count,
// so the fixture is small and carries the reloption the analyzer would
// have set) is freeze-amber with half its rows dead. Before, every scan
// proposed raising it back to 0.02; now three scans propose nothing for
// it and its reloptions are untouched. No concurrency test: one custodian
// per database scans in a serial loop.

// amberRedBuffer makes amber start at an XID age of 2e-5 of
// autovacuum_freeze_max_age (4,000 XIDs at the 200M default) and red at
// about half of it, which no table here reaches.
const amberRedBuffer = 49.999

func amberTunedTable(t *testing.T, table string) {
	t.Helper()
	ctx := context.Background()
	pool := requireAutonomyDB(t)
	deadlineExec(t, pool, "DROP TABLE IF EXISTS "+table+"; CREATE TABLE "+table+
		" (id int) WITH (autovacuum_enabled = false, autovacuum_vacuum_scale_factor = 0.01);"+
		" INSERT INTO "+table+" SELECT generate_series(1, 1000)")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP TABLE IF EXISTS "+table)
	})
	deadlineExec(t, pool, "VACUUM (FREEZE) "+table)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "DELETE FROM "+table+" WHERE id % 2 = 0"); err != nil {
		t.Fatalf("delete half the rows: %v", err)
	}
	if err := testdb.FlushStats(ctx, conn.Conn()); err != nil {
		t.Fatalf("flush statistics: %v", err)
	}
	var maxAge int64
	if err := pool.QueryRow(ctx,
		"SELECT current_setting('autovacuum_freeze_max_age')::bigint").Scan(&maxAge); err != nil {
		t.Fatalf("read autovacuum_freeze_max_age: %v", err)
	}
	burn := int(float64(maxAge)*(100-2*amberRedBuffer)/100) + 600
	deadlineExec(t, pool, fmt.Sprintf(`DO $$ BEGIN
		FOR i IN 1..%d LOOP PERFORM pg_catalog.pg_current_xact_id(); COMMIT; END LOOP;
		END $$`, burn))
}

func tableReloptions(t *testing.T, table string) []string {
	t.Helper()
	var opts []string
	err := requireAutonomyDB(t).QueryRow(context.Background(),
		"SELECT COALESCE(reloptions, '{}') FROM pg_class WHERE oid = to_regclass($1)",
		table).Scan(&opts)
	if err != nil {
		t.Fatalf("read reloptions of %s: %v", table, err)
	}
	return opts
}

func requireAmber(t *testing.T, table string, thresholds freeze.Thresholds) {
	t.Helper()
	var age, maxAge, mxid, mxidMax int64
	err := requireAutonomyDB(t).QueryRow(context.Background(), `SELECT
		age(relfrozenxid)::bigint, current_setting('autovacuum_freeze_max_age')::bigint,
		mxid_age(relminmxid)::bigint,
		current_setting('autovacuum_multixact_freeze_max_age')::bigint
		FROM pg_class WHERE oid = to_regclass($1)`, table).Scan(&age, &maxAge, &mxid, &mxidMax)
	if err != nil {
		t.Fatalf("read freeze horizon of %s: %v", table, err)
	}
	if got := freeze.TableUrgency(age, maxAge, mxid, mxidMax, thresholds); got !=
		freeze.UrgencyAmber {
		t.Fatalf("%s urgency = %s at XID age %d of %d; fixture broken", table, got, age, maxAge)
	}
}

func TestFreezeAmberTunedTableIsNeverRetunedOnRealPostgres(t *testing.T) {
	pool := requireAutonomyDB(t)
	ctx := context.Background()
	table := "public.vacuum07_amber_tuned"
	amberTunedTable(t, table)
	before := tableReloptions(t, table)
	want := []string{"autovacuum_enabled=false", "autovacuum_vacuum_scale_factor=0.01"}
	if !reflect.DeepEqual(before, want) {
		t.Fatalf("fixture reloptions = %q, want %q", before, want)
	}
	custodian := NewPostgresFreezeCustodian(pool, "testdb", amberRedBuffer)
	requireAmber(t, table, custodian.threshold)
	for cycle := 1; cycle <= 3; cycle++ {
		proposals, err := custodian.Scan(ctx)
		if err != nil {
			t.Fatalf("cycle %d: Scan: %v", cycle, err)
		}
		for _, proposal := range proposals {
			if strings.Contains(proposal.SQL, "scale_factor") ||
				proposal.Feature == "autovacuum_tuning" {
				t.Fatalf("cycle %d: custodian tuned autovacuum: %#v", cycle, proposal)
			}
		}
		if mine := proposalsFor(proposals, table); len(mine) != 0 {
			t.Fatalf("cycle %d: proposals for %s = %#v, want none", cycle, table, mine)
		}
	}
	if after := tableReloptions(t, table); !reflect.DeepEqual(after, before) {
		t.Fatalf("reloptions after three scans = %q, want %q", after, before)
	}
}
