package rollout

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// setTargets replaces action 101's stored target queryids (predicted and
// before_state alike) with the JSON array ids.
func setTargets(t *testing.T, pool *pgxpool.Pool, ids string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE sage.action_outcome SET predicted =
		jsonb_set(predicted, '{target_queryids}', $1::jsonb) WHERE action_log_id = 101`,
		ids); err != nil {
		t.Fatalf("set predicted targets: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET before_state =
		jsonb_set(before_state, '{target_queryids}', $1::jsonb) WHERE id = 101`,
		ids); err != nil {
		t.Fatalf("set before_state targets: %v", err)
	}
}

// Integration: a real pg_stat_statements queryid is past float64's exact
// range; the action's target query text is still found, and its change
// read, by that exact id.
func TestEffectPoolReadsALargeQueryIDExactly(t *testing.T) {
	const big = int64(-3420843250382719386)
	pool, _ := freshRunPool(t, "effect large queryid")
	from, to := effectWindow(t, pool)
	seedOutcome(t, pool, from, to, v2Evidence, "null")
	setTargets(t, pool, `[-3420843250382719386]`)
	seedQuery(t, pool, big, "SELECT * FROM orders WHERE status = $1", from,
		to.Add(-3*time.Hour), 10, 7)
	// Its float64 neighbour is another query, which must not be read.
	seedQuery(t, pool, -3420843250382719488, "SELECT * FROM items WHERE id = $1", from,
		to.Add(-2*time.Hour), 10, 50)
	// The newest snapshot holds both, as a real queries snapshot does.
	if _, err := pool.Exec(context.Background(), `INSERT INTO sage.snapshots (collected_at,
		category, data) VALUES ($1, 'queries', '[{"queryid": -3420843250382719386,
		"query": "SELECT * FROM orders WHERE status = $1"}, {"queryid":
		-3420843250382719488, "query": "SELECT * FROM items WHERE id = $1"}]'::jsonb)`,
		to.Add(-time.Hour)); err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}
	eff, err := NewPoolEffects(pool).ActionEffect(context.Background(), 101)
	if err != nil || !eff.Measured || len(eff.Queries) != 1 || eff.Queries[0] != effectQuery {
		t.Fatalf("action effect = %+v (%v), want the orders query only", eff, err)
	}
	change, ran, err := NewPoolEffects(pool).QueryChange(context.Background(), eff.Queries,
		from, to)
	if err != nil || !ran || !near(&change, -30) {
		t.Fatalf("change = %v %v (%v), want -30", change, ran, err)
	}
}

// Invalid input: target queryids that are not integers leave the measured
// v2 effect standing (it still decides) with no target queries, and say
// why; they never turn a measured number into flag only.
func TestEffectPoolMalformedTargetsKeepTheV2Effect(t *testing.T) {
	pool, _ := freshRunPool(t, "effect malformed targets")
	from, to := effectWindow(t, pool)
	seedOutcome(t, pool, from, to, v2Evidence, "null")
	setTargets(t, pool, `["not-an-id"]`)
	eff, err := NewPoolEffects(pool).ActionEffect(context.Background(), 101)
	if err != nil || !eff.Measured || eff.Malformed || eff.EffectPct != -28 ||
		len(eff.Queries) != 0 || !strings.Contains(eff.Reason, `"not-an-id" is not a number`) {
		t.Fatalf("action effect = %+v (%v), want the v2 effect with no queries", eff, err)
	}
}
