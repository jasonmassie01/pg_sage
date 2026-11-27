package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/autonomy"
)

// vacuum-03 end to end on real PostgreSQL: a red table's freeze reaches
// sage.decision as a budget bypass ("xid runway critical") with its hard
// deadline ahead of now. Before the fix the multixact deadline at the
// 0.001/s rate floor overflowed time.Duration into the far past and won
// the earlier-deadline race, so the bypass never fired. The test holds the
// exclusive server (freezeAgeReason): the routed freeze's post-check fails
// unless the VACUUM advances relfrozenxid. No concurrency test: one
// custodian per database runs in a serial loop.

func TestFreezeRedTableRecordsWraparoundBypassDecision(t *testing.T) {
	pool := exclusiveAutonomyPool(t, freezeAgeReason)
	ctx := context.Background()
	schema := "v03a"
	table := schema + ".t00"
	v02Exec(t, pool, "DROP SCHEMA IF EXISTS "+schema+" CASCADE; CREATE SCHEMA "+schema)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
	})
	v02Exec(t, pool, "CREATE TABLE "+table+" (id int) WITH (autovacuum_enabled = false);"+
		" INSERT INTO "+table+" VALUES (1); ANALYZE "+table)
	v02Exec(t, pool, "VACUUM (FREEZE) "+table)
	var maxAge int64
	if err := pool.QueryRow(ctx, "SELECT current_setting('autovacuum_freeze_max_age')::bigint").
		Scan(&maxAge); err != nil {
		t.Fatalf("read autovacuum_freeze_max_age: %v", err)
	}
	v02Burn(t, pool, int(float64(maxAge)*(100-v02RedBuffer)/100)+600)

	custodian := autonomy.NewPostgresFreezeCustodian(pool, "testdb", v02RedBuffer)
	if _, err := custodian.Scan(ctx); err != nil {
		t.Fatalf("first Scan (rate sample): %v", err)
	}
	proposals, err := custodian.Scan(ctx)
	if err != nil {
		t.Fatalf("second Scan: %v", err)
	}
	since := v02Since(t, pool)
	router := v02Router(t, pool)
	routed := 0
	for _, p := range proposals {
		if len(p.TargetObjects) != 1 || p.TargetObjects[0] != table || p.Feature != "freeze" {
			continue
		}
		routed++
		if err := router.Route(ctx, p); err != nil {
			t.Fatalf("Route: %v", err)
		}
	}
	if routed != 1 {
		t.Fatalf("routed %d freeze proposals for %s, want 1", routed, table)
	}
	assertWraparoundBypass(t, ctx, pool, table, since)
}

// assertWraparoundBypass is CHECK-02: the freeze decision's hard deadline
// is ahead and the decision is a budget bypass for the xid runway.
func assertWraparoundBypass(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, table string, since time.Time,
) {
	t.Helper()
	var reason, verdict, threat string
	var ahead bool
	err := pool.QueryRow(ctx, `SELECT reason, verdict, COALESCE(evidence->>'deadline_threat', ''),
		COALESCE(deadline_hard_at > now(), false)
		FROM sage.decision
		WHERE feature = 'freeze' AND created_at > $1
		  AND target_objects @> to_jsonb(ARRAY[$2::text])
		ORDER BY id DESC LIMIT 1`, since, table).Scan(&reason, &verdict, &threat, &ahead)
	if err != nil {
		t.Fatalf("read the freeze decision: %v", err)
	}
	if !ahead || !strings.HasPrefix(reason, "budget bypass: xid runway critical") ||
		verdict != "execute" || (threat != "xid" && threat != "multixact") {
		t.Fatalf("freeze decision: reason=%q verdict=%q deadline_threat=%q ahead=%v, "+
			"want an executed xid-runway budget bypass with the deadline ahead",
			reason, verdict, threat, ahead)
	}
}
