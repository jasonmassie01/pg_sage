package tuning

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/pg-sage/sidecar/internal/verify"
)

// SettingActions reads the setting changes from the real ledger, with the
// verification verdict, since the window start.

func seedSettingAction(t *testing.T, pool *pgxpool.Pool, sql, rollback string,
	at time.Time) int64 {
	t.Helper()
	var id int64
	if err := pool.QueryRow(context.Background(), `INSERT INTO sage.action_log
		(action_type, sql_executed, rollback_sql, outcome, executed_at)
		VALUES ('alter_system', $1, $2, 'success', $3) RETURNING id`, sql, rollback,
		at).Scan(&id); err != nil {
		t.Fatalf("seed action: %v", err)
	}
	return id
}

func TestPostgresStore_SettingActions(t *testing.T) {
	pool := dbPool(t)
	ctx := context.Background()
	now := time.Now()
	old := seedSettingAction(t, pool, "ALTER SYSTEM SET work_mem = '3MB'",
		"ALTER SYSTEM SET work_mem = '2MB'", now.Add(-8*24*time.Hour))
	decided := seedSettingAction(t, pool, "ALTER SYSTEM SET work_mem = '9MB'",
		"ALTER SYSTEM SET work_mem = '8MB'", now.Add(-2*time.Hour))
	open := seedSettingAction(t, pool, "ALTER SYSTEM SET work_mem = '10MB'",
		"ALTER SYSTEM SET work_mem = '9MB'", now.Add(-time.Hour))
	store := verify.NewOutcomeStore(pool)
	for _, id := range []int64{decided, open} {
		if err := store.RecordPrediction(ctx, id, verify.Prediction{Class: verify.ClassGUC,
			Method: verify.MethodModel, Metric: "temp_spills",
			ExpectedChangePct: pct(-30), Source: PredictionSource}); err != nil {
			t.Fatalf("record prediction: %v", err)
		}
	}
	observed := -100.0
	if err := store.RecordVerdict(ctx, verify.Outcome{ActionLogID: decided,
		Class: verify.ClassGUC, Verdict: verify.OutcomeImproved,
		Observed: verify.Observed{Metric: "temp_spills", Before: 62, After: 0,
			ChangePct: &observed}}); err != nil {
		t.Fatalf("record verdict: %v", err)
	}
	acts, err := pgStore(t, pool).SettingActions(ctx, now.Add(-7*24*time.Hour))
	if err != nil {
		t.Fatalf("setting actions: %v", err)
	}
	got := map[int64]SettingAction{}
	for _, a := range acts {
		got[a.ActionID] = a
	}
	if _, seen := got[old]; seen {
		t.Fatal("a change older than the window is not read")
	}
	d, o := got[decided], got[open]
	if d.Key != "work_mem" || d.From != "8MB" || d.To != "9MB" ||
		d.Verdict != verify.OutcomeImproved || d.DecidedAt == nil {
		t.Fatalf("decided = %+v", d)
	}
	if o.To != "10MB" || !o.pending() {
		t.Fatalf("open = %+v", o)
	}
}
