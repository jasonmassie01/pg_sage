package autonomy

import (
	"context"
	"strings"
	"testing"
)

// T6 test round 1: pg_sage's own relations (the sage schema and its
// TOAST), which the custodian leaves to autovacuum, are read on top of the
// custodian's horizon limit, so they never crowd a user table out.
// No concurrent-access test here: the shared-reading tests in
// freeze_sampler_test.go cover parallel readers of the same reading.

func TestFreezeSamplerReadsTheOwnRelationAllowanceOnTopOfTheLimit(t *testing.T) {
	db := &fakeSamplerDB{}
	if _, err := custodianOver(db).Sampler().Read(context.Background()); err != nil {
		t.Fatalf("Read: %v", err)
	}
	var limits []any
	for _, s := range db.statements() {
		if s.method == "Query" && strings.Contains(s.sql, "freeze_horizons") {
			limits = append(limits, s.args[0])
		}
	}
	want := freezeHorizonLimit + ownRelationAllowance
	if len(limits) != 1 || limits[0] != want {
		t.Fatalf("horizon read limits = %v, want one read of %d", limits, want)
	}
	if freezeHorizonLimit <= 0 || ownRelationAllowance <= 0 {
		t.Fatalf("limit %d and allowance %d must both be positive",
			freezeHorizonLimit, ownRelationAllowance)
	}
}

// ownFreezeRelationsSQL counts the sage schema's relations that carry a
// freeze horizon, their TOAST included: what the allowance must cover.
const ownFreezeRelationsSQL = `SELECT count(*) + count(*) FILTER (WHERE c.reltoastrelid <> 0)
FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname = 'sage' AND c.relkind IN ('r', 'm') AND c.relfrozenxid <> '0'::xid`

func TestOwnRelationAllowanceCoversTheSageSchema(t *testing.T) {
	pool := requireAutonomyDB(t)
	var own int
	if err := pool.QueryRow(context.Background(), ownFreezeRelationsSQL).Scan(&own); err != nil {
		t.Fatalf("count the sage schema's freeze relations: %v", err)
	}
	if own < 100 {
		t.Fatalf("the bootstrapped sage schema has %d freeze relations; the count is broken",
			own)
	}
	if own > ownRelationAllowance {
		t.Fatalf("the sage schema has %d freeze relations, over ownRelationAllowance %d: "+
			"raise the allowance so they cannot crowd user tables out of a reading",
			own, ownRelationAllowance)
	}
}
