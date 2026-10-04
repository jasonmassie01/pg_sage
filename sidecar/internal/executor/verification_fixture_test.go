package executor

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Phase 1.3 replaced the global cache-hit/write-latency monitor with a
// call-weighted comparison of the queries an action targets. The old
// fixtures forced a regression with an impossible before-state cache hit
// ratio ({"cache_hit_ratio": 2.0}); these sentinels now seed real
// evidence instead: a frozen per-query baseline in before_state and
// query_store samples after the action.
const (
	regressedBeforeState = "fixture:regressed"
	improvedBeforeState  = "fixture:improved"
	// fixtureBaselineMs is the targeted query's frozen pre-action mean.
	fixtureBaselineMs = 10.0
)

// fixtureQueryID gives every action its own queryid so fixtures never
// interleave cumulative counters (which would read as a stats reset).
func fixtureQueryID(actionID int64) int64 { return 8_800_000_000 + actionID }

// fixtureBeforeState is a before_state with a hypopg prediction, the
// target and a frozen baseline of 600 calls at fixtureBaselineMs.
func fixtureBeforeState(class string, qid int64) string {
	baseline := map[string]any{"calls": 600,
		"mean_ns":   int64(fixtureBaselineMs * float64(time.Millisecond)),
		"stderr_ns": int64(0.05 * float64(time.Millisecond)), "buckets": 30}
	state := map[string]any{
		"target_queryids": []int64{qid},
		"predicted_effect": map[string]any{"class": class, "method": "hypopg",
			"metric": "mean_exec_time", "target_queryids": []int64{qid},
			"expected_change_pct": -30.0, "source": "test"},
		"verify_baseline": map[string]any{strconv.FormatInt(qid, 10): baseline},
	}
	raw, _ := json.Marshal(state)
	return string(raw)
}

// seedQuerySamples writes cumulative query_store samples for qid, one a
// minute from start, with calls per minute at meanMs (a little jitter so
// the spread is not zero).
func seedQuerySamples(
	t *testing.T, ctx context.Context, pool *pgxpool.Pool, qid int64,
	start time.Time, minutes int, callsPerMinute int64, meanMs float64,
) {
	t.Helper()
	var calls int64 = 1000
	total := 1000 * meanMs
	for i := 0; i <= minutes; i++ {
		if i > 0 {
			jitter := 1 + 0.02*float64(i%3-1)
			calls += callsPerMinute
			total += float64(callsPerMinute) * meanMs * jitter
		}
		if _, err := pool.Exec(ctx, `INSERT INTO sage.query_store
			(captured_at, queryid, calls, total_exec_time, mean_exec_time)
			VALUES ($1, $2, $3, $4, $5)`, start.Add(time.Duration(i)*time.Minute), qid,
			calls, total, total/float64(calls)); err != nil {
			t.Fatalf("seed query_store: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			"DELETE FROM sage.query_store WHERE queryid=$1", qid)
	})
}

// applyVerificationFixture makes action id a 30-minute-old action whose
// targeted query regressed (3x) or improved (halved) since.
func applyVerificationFixture(t *testing.T, pool *pgxpool.Pool, id int64, kind string) {
	t.Helper()
	ctx := context.Background()
	var sql string
	if err := pool.QueryRow(ctx, "SELECT sql_executed FROM sage.action_log WHERE id=$1",
		id).Scan(&sql); err != nil {
		t.Fatalf("read fixture action: %v", err)
	}
	qid := fixtureQueryID(id)
	executedAt := time.Now().Add(-30 * time.Minute).UTC()
	if _, err := pool.Exec(ctx, `UPDATE sage.action_log SET before_state=$2::jsonb,
		executed_at=$3 WHERE id=$1`, id, fixtureBeforeState(verificationClass(sql), qid),
		executedAt); err != nil {
		t.Fatalf("apply fixture before_state: %v", err)
	}
	mean := fixtureBaselineMs * 3
	if kind == improvedBeforeState {
		mean = fixtureBaselineMs / 2
	}
	seedQuerySamples(t, ctx, pool, qid, executedAt.Add(time.Second), 29, 40, mean)
}

func isVerificationFixture(beforeState string) bool {
	return strings.HasPrefix(beforeState, "fixture:")
}
