package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/pg-sage/sidecar/internal/autoexplain"
	"github.com/pg-sage/sidecar/internal/lakebaseobs"
)

func TestLakebaseHistoryOutcomeClassification(t *testing.T) {
	transient := []error{
		context.DeadlineExceeded,
		context.Canceled,
		fmt.Errorf("store captured provider plan: %w", &pgconn.PgError{Code: "08006"}),
		fmt.Errorf("store captured provider plan: %w", &pgconn.PgError{Code: "57P01"}),
		fmt.Errorf("store captured provider plan: %w", &pgconn.PgError{Code: "40001"}),
		errors.New("unclassified sink failure"),
	}
	for _, err := range transient {
		if outcome, got := lakebaseHistoryOutcome(err); got == nil || outcome != "" {
			t.Fatalf("%v must stay transient, got outcome %q", err, outcome)
		}
	}
	terminal := map[error]string{
		nil:                          "normalized",
		lakebaseobs.ErrMasked:        "payload_unavailable",
		lakebaseobs.ErrMalformed:     "rejected_row",
		lakebaseobs.ErrBudget:        "rejected_row",
		lakebaseobs.ErrSelfStatement: "self_excluded",
		autoexplain.ErrSelfStatement: "self_excluded",
		fmt.Errorf("x: %w", autoexplain.ErrInvalidProviderPlan): "rejected_row",
	}
	for err, want := range terminal {
		outcome, got := lakebaseHistoryOutcome(err)
		if got != nil || outcome != want {
			t.Fatalf("%v classified as %q (%v), want %q", err, outcome, got, want)
		}
	}
}

// unhashablePlanJSON has a root that normalizes and a child planhash rejects.
const unhashablePlanJSON = `{"Plan":{"Node Type":"Result","Total Cost":1.25,` +
	`"Plans":[{"Total Cost":1}]}}`

type emptyHistoryReader struct{ events []lakebaseobs.Event }

func (r *emptyHistoryReader) ReadWindow(_ context.Context, s lakebaseobs.Stream,
	_ lakebaseobs.Scope, w lakebaseobs.Window,
) (lakebaseobs.Batch, error) {
	out := []lakebaseobs.Event{}
	for _, e := range r.events {
		if e.Stream == s && !e.OccurredAt.Before(w.From) && e.OccurredAt.Before(w.To) {
			out = append(out, e)
		}
	}
	return lakebaseobs.Batch{Events: out}, nil
}

func TestLakebaseHistoryConsumerRejectsInvalidPlanAndDeliversNext(t *testing.T) {
	f := newHistoryWiringFixture(t)
	rt, scope, _ := f.registration(t)
	st := &lakebaseTelemetryState{state: "polling", payload: true, outcomes: map[string]uint64{}}
	consumer := newLakebaseHistoryConsumer(f.monitored, st)
	stamp := f.now.Add(-time.Hour)
	poison := historyWiringEvent(t, scope, lakebaseobs.Plans, stamp, f.now, map[string]string{
		"plan_json": unhashablePlanJSON})
	good := historyWiringEvent(t, scope, lakebaseobs.Plans, stamp.Add(time.Second), f.now, nil)
	logEvent := historyWiringEvent(t, scope, lakebaseobs.Logs, stamp.Add(2*time.Second), f.now, nil)
	reader := &emptyHistoryReader{events: []lakebaseobs.Event{poison, good, logEvent}}
	runtime, err := lakebaseobs.NewRuntime(reader, f.store, scope, lakebaseobs.Options{
		WarehouseID: "synthetic-warehouse", AllowPayload: true, Window: lakebaseobs.SourceRetention,
		Streams: []lakebaseobs.Stream{lakebaseobs.Plans, lakebaseobs.Logs}}, consumer)
	if err != nil {
		t.Fatal(err)
	}
	st.runtime, st.store = runtime, f.store
	registerHistoryWiring(t, rt.spec.Name, st)
	if err = runtime.Poll(f.ctx, f.now); err != nil {
		t.Fatalf("an invalid plan must be rejected, not fail the poll: %v", err)
	}
	var stored, poisoned int
	err = f.monitored.QueryRow(f.ctx, `SELECT count(*) FILTER (WHERE provider_event_id=$1),
 count(*) FILTER (WHERE provider_event_id=$2) FROM sage.explain_cache`,
		good.ID, poison.ID).Scan(&stored, &poisoned)
	if err != nil || stored != 1 || poisoned != 0 {
		t.Fatalf("explain_cache good=%d poison=%d err=%v", stored, poisoned, err)
	}
	assertHistoryOutcomes(t, st, map[string]uint64{"plan_history:rejected_row": 1,
		"plan_history:normalized": 1, "postgres_logs:normalized": 1})
	pending, err := f.store.Pending(f.ctx, runtime.Binding(), "normalized_history_v1", 10, f.now)
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after delivery: %d %v", len(pending), err)
	}
	row := lakebaseTelemetryStatuses().(map[string]any)[rt.spec.Name].(map[string]any)
	delivery, ok := row["delivery"].([]lakebaseobs.DeliveryStatus)
	if !ok || len(delivery) != 1 || delivery[0].Consumer != "normalized_history_v1" ||
		delivery[0].PendingRows != 0 || delivery[0].PendingRowsCapped {
		t.Fatalf("status delivery block = %#v", row["delivery"])
	}
	consumer.DeliveryFailed(good)
	assertHistoryOutcomes(t, st, map[string]uint64{"plan_history:rejected_row": 1,
		"plan_history:normalized": 1, "postgres_logs:normalized": 1,
		"plan_history:delivery_failed": 1})
}

func assertHistoryOutcomes(t *testing.T, st *lakebaseTelemetryState, want map[string]uint64) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.outcomes) != len(want) {
		t.Fatalf("outcomes %v, want %v", st.outcomes, want)
	}
	for k, v := range want {
		if st.outcomes[k] != v {
			t.Fatalf("outcome %s = %d, want %d (all: %v)", k, st.outcomes[k], v, st.outcomes)
		}
	}
}
