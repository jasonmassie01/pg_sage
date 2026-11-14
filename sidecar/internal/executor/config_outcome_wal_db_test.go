package executor

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pg-sage/sidecar/internal/verify"
)

// advisors-05 CHECK-07 on real PostgreSQL: an executed max_wal_size change
// is judged by the checkpointer's counters (pg_stat_bgwriter before 17,
// pg_stat_checkpointer from 17) and ends with a verification verdict, not
// "unverifiable". checkpoint_timeout is lowered to 30 s so timed
// checkpoints happen within the test.
func TestOutcome_MaxWALSizeChangeIsVerified(t *testing.T) {
	pool, ctx := exclusiveDB(t, "timed and requested checkpoint counts")
	shortCheckpointTimeout(t, ctx, pool)
	for i := 0; i < 3; i++ { // requested checkpoints before the change
		if _, err := pool.Exec(ctx, `CHECKPOINT`); err != nil {
			t.Fatalf("checkpoint: %v", err)
		}
	}
	start, err := readOutcomeCounters(ctx, pool, metricRequestedCheckpoints, "")
	if err != nil {
		t.Fatalf("read checkpoint counters: %v", err)
	}
	if start["requested"] < 3 || start["epoch"] <= 0 {
		t.Fatalf("counters = %v, want the 3 CHECKPOINTs and a known epoch", start)
	}
	waitTimedCheckpoints(t, ctx, pool, start["timed"]+3)
	at := time.Now().Add(-10 * time.Minute).UTC()
	baseline := outcomeBaseline{Metric: metricRequestedCheckpoints, At: at, Counters: start}
	p := configPrediction(metricRequestedCheckpoints)
	p.Class = verify.ClassGUC
	raw, _ := json.Marshal(map[string]any{"predicted_effect": p,
		"config_change": map[string]any{"kind": "guc", "name": "max_wal_size",
			"outcome": baseline}})
	id := insertVerifiedAction(t, pool, verifiedActionRow{
		sql:      "ALTER SYSTEM SET max_wal_size = '2GB'",
		rollback: "ALTER SYSTEM RESET max_wal_size", before: string(raw), executedAt: at,
		decisionID: insertParkDecision(t, ctx, pool)})
	rec := &recordedRollback{}

	MonitorAndRollback(ctx, pool, id, "ALTER SYSTEM RESET max_wal_size", monitorConfig(rec),
		nopLog, nil)

	got := storedOutcome(t, pool, id)
	if got.Observed.Metric != metricRequestedCheckpoints || got.Verdict == "" ||
		got.Verdict == verify.OutcomeUnverifiable {
		t.Fatalf("outcome = %+v (%s), want a verdict on requested_checkpoints", got,
			actionReason(t, ctx, pool, id))
	}
	if got.Verdict != verify.OutcomeImproved || rec.calls != 0 {
		t.Fatalf("outcome %s rollbacks %d, want improved (only timed checkpoints in "+
			"the window) and kept", got.Verdict, rec.calls)
	}
	if v := verificationVerdict(t, pool, id); v == "unverifiable" || v == "" {
		t.Fatalf("verification verdict = %q, want one other than unverifiable", v)
	}
}

// shortCheckpointTimeout sets checkpoint_timeout to its 30 s minimum for
// the test and restores it after.
func shortCheckpointTimeout(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, sql := range []string{`ALTER SYSTEM SET checkpoint_timeout = '30s'`,
		`SELECT pg_reload_conf()`} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `ALTER SYSTEM RESET checkpoint_timeout`)
		_, _ = pool.Exec(bg, `SELECT pg_reload_conf()`)
	})
}

// waitTimedCheckpoints waits (at most 3 minutes) for the timed count.
func waitTimedCheckpoints(t *testing.T, ctx context.Context, pool *pgxpool.Pool,
	want float64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		c, err := readOutcomeCounters(ctx, pool, metricRequestedCheckpoints, "")
		if err != nil {
			t.Fatalf("read checkpoint counters: %v", err)
		}
		if c["timed"] >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed checkpoints %v, want %v within 3 minutes", c["timed"], want)
		}
		time.Sleep(2 * time.Second)
	}
}
