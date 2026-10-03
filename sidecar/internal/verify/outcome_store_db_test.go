package verify

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func insertOutcomeAction(t *testing.T, pool *pgxpool.Pool, sql string) int64 {
	t.Helper()
	var id int64
	err := pool.QueryRow(t.Context(), `INSERT INTO sage.action_log
		(action_type, sql_executed, outcome, database_id)
		VALUES ('drop_index', $1, 'monitoring', 4242) RETURNING id`, sql).Scan(&id)
	if err != nil {
		t.Fatalf("insert action: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), "DELETE FROM sage.action_log WHERE id=$1", id)
	})
	return id
}

func dropPrediction() Prediction {
	return Prediction{Class: ClassIndexDrop, Method: MethodRule,
		Metric: MetricMeanExecTime, TargetQueryIDs: []int64{11, 12},
		ExpectedChangePct: pct(0), Source: "analyzer", Note: "unused index"}
}

func TestOutcomeStoreRecordsPredictionAsPending(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertOutcomeAction(t, pool, "DROP INDEX CONCURRENTLY public.os_a")
	store := NewOutcomeStore(pool)

	if err := store.RecordPrediction(ctx, id, dropPrediction()); err != nil {
		t.Fatalf("RecordPrediction: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Verdict != OutcomePending || got.Tolerance != OutcomePending ||
		got.Class != ClassIndexDrop || got.DecidedAt != nil {
		t.Fatalf("pending outcome = %+v", got)
	}
	if got.DatabaseID == nil || *got.DatabaseID != 4242 {
		t.Fatalf("database_id = %v, want 4242 copied from the action", got.DatabaseID)
	}
	if got.Predicted.Method != MethodRule || len(got.Predicted.TargetQueryIDs) != 2 ||
		got.Predicted.ExpectedChangePct == nil || *got.Predicted.ExpectedChangePct != 0 {
		t.Fatalf("stored prediction = %+v", got.Predicted)
	}
}

func TestOutcomeStorePredictionIsImmutable(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertOutcomeAction(t, pool, "DROP INDEX CONCURRENTLY public.os_b")
	store := NewOutcomeStore(pool)
	if err := store.RecordPrediction(ctx, id, dropPrediction()); err != nil {
		t.Fatalf("first RecordPrediction: %v", err)
	}
	other := dropPrediction()
	other.ExpectedChangePct = pct(-99)
	if err := store.RecordPrediction(ctx, id, other); err != nil {
		t.Fatalf("second RecordPrediction must be a no-op, got %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil || *got.Predicted.ExpectedChangePct != 0 {
		t.Fatalf("prediction changed after the fact: %+v (%v)", got.Predicted, err)
	}
}

func TestOutcomeStoreRecordPredictionUnknownAction(t *testing.T) {
	pool := verifyIntegrationPool(t)
	err := NewOutcomeStore(pool).RecordPrediction(t.Context(), -1, dropPrediction())
	if !errors.Is(err, ErrOutcomeNotFound) {
		t.Fatalf("RecordPrediction(unknown) = %v, want ErrOutcomeNotFound", err)
	}
}

func TestOutcomeStoreRecordsVerdictAgainstStoredPrediction(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertOutcomeAction(t, pool, "DROP INDEX CONCURRENTLY public.os_c")
	store := NewOutcomeStore(pool)
	if err := store.RecordPrediction(ctx, id, dropPrediction()); err != nil {
		t.Fatalf("RecordPrediction: %v", err)
	}
	start := time.Now().Add(-time.Hour).UTC().Truncate(time.Second)
	end := time.Now().UTC().Truncate(time.Second)
	err := store.RecordVerdict(ctx, Outcome{ActionLogID: id, Verdict: OutcomeRegressed,
		Observed: Observed{Metric: MetricMeanExecTime, Before: 10, After: 25,
			ChangePct: pct(150)},
		Evidence: map[string]any{"soft_drop": map[string]any{"recreated": true}},
		Reason:   "query 11 regressed", WindowStart: &start, WindowEnd: &end})
	if err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Verdict != OutcomeRegressed || got.Tolerance != ToleranceMissed ||
		got.DecidedAt == nil || got.Reason != "query 11 regressed" {
		t.Fatalf("decided outcome = %+v", got)
	}
	if got.Observed.After != 25 || *got.Observed.ChangePct != 150 {
		t.Fatalf("observed = %+v", got.Observed)
	}
	soft, _ := got.Evidence["soft_drop"].(map[string]any)
	if soft["recreated"] != true {
		t.Fatalf("evidence = %+v", got.Evidence)
	}
	if got.WindowStart == nil || !got.WindowStart.Equal(start) || !got.WindowEnd.Equal(end) {
		t.Fatalf("window = %v..%v, want %v..%v", got.WindowStart, got.WindowEnd, start, end)
	}
}

func TestOutcomeStoreVerdictWithoutPredictionRecordsNoPrediction(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertOutcomeAction(t, pool, "ALTER SYSTEM SET random_page_cost = 1.1")
	store := NewOutcomeStore(pool)
	err := store.RecordVerdict(ctx, Outcome{ActionLogID: id, Class: ClassGUC,
		Verdict: OutcomeUnverifiable, Reason: "no prediction, no regression"})
	if err != nil {
		t.Fatalf("RecordVerdict: %v", err)
	}
	got, err := store.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Predicted.Method != MethodNone || got.Tolerance != ToleranceNoPrediction ||
		got.Class != ClassGUC {
		t.Fatalf("outcome without prediction = %+v", got)
	}
}

func TestOutcomeStoreRejectsUndecidedVerdicts(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertOutcomeAction(t, pool, "DROP INDEX CONCURRENTLY public.os_d")
	store := NewOutcomeStore(pool)
	for _, verdict := range []string{"", OutcomePending, "success", "Improved"} {
		err := store.RecordVerdict(ctx, Outcome{ActionLogID: id, Verdict: verdict})
		if !errors.Is(err, ErrInvalidOutcome) {
			t.Errorf("RecordVerdict(%q) = %v, want ErrInvalidOutcome", verdict, err)
		}
	}
	if _, err := store.Get(ctx, id); !errors.Is(err, ErrOutcomeNotFound) {
		t.Fatalf("rejected verdicts must write nothing, Get = %v", err)
	}
}

func TestOutcomeStoreListFilters(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	store := NewOutcomeStore(pool)
	regressed := insertOutcomeAction(t, pool, "DROP INDEX CONCURRENTLY public.os_e")
	improved := insertOutcomeAction(t, pool, "DROP INDEX CONCURRENTLY public.os_f")
	for id, verdict := range map[int64]string{regressed: OutcomeRegressed,
		improved: OutcomeImproved} {
		if err := store.RecordPrediction(ctx, id, dropPrediction()); err != nil {
			t.Fatalf("RecordPrediction: %v", err)
		}
		if err := store.RecordVerdict(ctx, Outcome{ActionLogID: id, Verdict: verdict,
			Observed: Observed{Metric: MetricMeanExecTime, ChangePct: pct(1)}}); err != nil {
			t.Fatalf("RecordVerdict: %v", err)
		}
	}
	got, err := store.List(ctx, OutcomeFilter{Class: ClassIndexDrop,
		Verdict: OutcomeRegressed, Since: time.Now().Add(-time.Hour), Limit: 1000})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	found := false
	for _, o := range got {
		if o.Verdict != OutcomeRegressed || o.Class != ClassIndexDrop {
			t.Fatalf("filter leaked %+v", o)
		}
		found = found || o.ActionLogID == regressed
	}
	if !found {
		t.Fatalf("List did not return the regressed outcome %d: %+v", regressed, got)
	}
	future, err := store.List(ctx, OutcomeFilter{Since: time.Now().Add(time.Hour)})
	if err != nil || len(future) != 0 {
		t.Fatalf("List(since future) = %d rows, %v", len(future), err)
	}
}

func TestOutcomeStoreListRejectsBadLimit(t *testing.T) {
	pool := verifyIntegrationPool(t)
	for _, limit := range []int{-1, 1001} {
		_, err := NewOutcomeStore(pool).List(t.Context(), OutcomeFilter{Limit: limit})
		if !errors.Is(err, ErrInvalidOutcome) {
			t.Errorf("List(limit=%d) = %v, want ErrInvalidOutcome", limit, err)
		}
	}
}

func TestOutcomeStoreCascadesWithTheAction(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertOutcomeAction(t, pool, "DROP INDEX CONCURRENTLY public.os_g")
	store := NewOutcomeStore(pool)
	if err := store.RecordPrediction(ctx, id, dropPrediction()); err != nil {
		t.Fatalf("RecordPrediction: %v", err)
	}
	if _, err := pool.Exec(ctx, "DELETE FROM sage.action_log WHERE id=$1", id); err != nil {
		t.Fatalf("delete action (retention): %v", err)
	}
	if _, err := store.Get(ctx, id); !errors.Is(err, ErrOutcomeNotFound) {
		t.Fatalf("outcome outlived its action: %v", err)
	}
}

func TestOutcomeStoreConcurrentVerdictsKeepOneRow(t *testing.T) {
	pool := verifyIntegrationPool(t)
	ctx := t.Context()
	id := insertOutcomeAction(t, pool, "DROP INDEX CONCURRENTLY public.os_h")
	store := NewOutcomeStore(pool)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- store.RecordVerdict(ctx, Outcome{ActionLogID: id, Class: ClassIndexDrop,
				Verdict: OutcomeNeutral})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent RecordVerdict: %v", err)
		}
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM sage.action_outcome
		WHERE action_log_id=$1`, id).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d (%v), want exactly one", n, err)
	}
}
