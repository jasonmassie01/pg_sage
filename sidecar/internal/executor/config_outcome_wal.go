package executor

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/verify"
)

// The requested-checkpoints metric (advisors-05): a max_wal_size or
// checkpoint_timeout change is meant to stop WAL volume from forcing
// checkpoints, so it is judged by the requested share of the checkpoints
// in the monitor window against the share before the change.

const (
	// requestedShareHealthy is the share under which checkpoints keep up
	// (the WAL advisor's threshold).
	requestedShareHealthy = 0.20
	// requestedShareDrop: the window's share must at most halve the baseline.
	requestedShareDrop = 0.5
	// minWindowCheckpoints is the checkpoints a window needs to be judged:
	// three timed ones at the default 5 min checkpoint_timeout in 15 min.
	minWindowCheckpoints = 3
)

// The checkpointer's counters moved to pg_stat_checkpointer in PostgreSQL
// 17. epoch is the later of the view's stats_reset and the postmaster
// start, in Unix seconds: a reset or a crash between the readings moves it.
const (
	checkpointCountersKey14 = metricRequestedCheckpoints + ":bgwriter"
	checkpointCountersKey17 = metricRequestedCheckpoints + ":checkpointer"
	checkpointEpochExpr     = `EXTRACT(EPOCH FROM GREATEST(COALESCE(stats_reset,
		'-infinity'::timestamptz), pg_postmaster_start_time()))::float8`
	checkpointCountersSQL14 = `/* pg_sage executor */ SELECT checkpoints_req::float8,
		checkpoints_timed::float8, ` + checkpointEpochExpr + ` FROM pg_stat_bgwriter`
	checkpointCountersSQL17 = `/* pg_sage executor */ SELECT num_requested::float8,
		num_timed::float8, ` + checkpointEpochExpr + ` FROM pg_stat_checkpointer`
)

// checkpointCountersKey picks the server version's checkpoint counters.
func checkpointCountersKey(ctx context.Context, pool *pgxpool.Pool) (string, error) {
	var version int
	if err := pool.QueryRow(ctx, `/* pg_sage executor */ SELECT
		current_setting('server_version_num')::int`).Scan(&version); err != nil {
		return "", fmt.Errorf("read the server version for the checkpoint counters: %w", err)
	}
	if version >= 170000 {
		return checkpointCountersKey17, nil
	}
	return checkpointCountersKey14, nil
}

// judgeRequestedCheckpoints credits a WAL change when the window's
// requested share fell under 20% or to at most half the share before.
// It never judges a regression: more forced checkpoints after a raise are
// the workload's, and lowering the value again would not help.
func judgeRequestedCheckpoints(before, after map[string]float64) metricJudgement {
	b, ok1 := counters(before, "requested", "timed", "epoch")
	a, ok2 := counters(after, "requested", "timed", "epoch")
	if !ok1 || !ok2 {
		return metricJudgement{Verdict: verify.OutcomeUnverifiable, Terminal: true,
			Reason: "checkpoint counters missing; not credited"}
	}
	if a[2] != b[2] || a[0] < b[0] || a[1] < b[1] {
		return insufficient("checkpoint statistics were reset during the window; "+
			"not credited", true)
	}
	if b[0]+b[1] <= 0 {
		return insufficient("no checkpoints before the change; nothing to compare", true)
	}
	r0 := b[0] / (b[0] + b[1])
	if r0 < requestedShareHealthy {
		return insufficient(fmt.Sprintf("requested checkpoints were %.0f%% before the "+
			"change, under 20%%; nothing to improve", r0*100), true)
	}
	req, n := a[0]-b[0], a[0]-b[0]+a[1]-b[1]
	if n < minWindowCheckpoints {
		return insufficient(fmt.Sprintf("too few checkpoints in the window (%.0f) to judge",
			n), false)
	}
	r1 := req / n
	change := (r1/r0 - 1) * 100
	j := metricJudgement{ObservedPct: &change, Before: r0, After: r1}
	if r1 < requestedShareHealthy || r1 <= requestedShareDrop*r0+judgeEpsilon {
		j.Verdict = verify.OutcomeImproved
		j.Reason = fmt.Sprintf("requested share of checkpoints fell from %.3f to %.3f",
			r0, r1)
		return j
	}
	j.Verdict = verify.OutcomeNeutral
	j.Reason = fmt.Sprintf("requested share of checkpoints did not fall enough "+
		"(%.3f -> %.3f)", r0, r1)
	return j
}
