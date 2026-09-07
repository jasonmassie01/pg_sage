package verify

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresAdaptersFailClosedWithoutPool(t *testing.T) {
	ctx := context.Background()
	source := NewPostgresObservationSource(nil)
	store := NewPostgresStateStore(nil, 30)
	checks := []struct {
		name string
		run  func() error
	}{
		{"queries", func() error {
			_, err := source.QueryMeasurements(ctx, nil, time.Time{}, time.Time{})
			return err
		}},
		{"writes", func() error {
			_, err := source.WriteMeasurements(ctx, "t", time.Time{}, time.Time{})
			return err
		}},
		{"index", func() error { _, err := source.IndexValid(ctx, "idx"); return err }},
		{"load", func() error { _, err := source.CurrentLoad(ctx); return err }},
		{"create", func() error { return store.Create(ctx, WatchState{}) }},
		{"update", func() error { return store.Update(ctx, WatchState{}) }},
		{"get", func() error { _, err := store.Get(ctx, "watch"); return err }},
		{"list", func() error { _, err := store.ListDue(ctx, time.Now()); return err }},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			if err := check.run(); err == nil {
				t.Fatal("adapter admitted operation without a database pool")
			}
		})
	}
}

func TestPostgresStateEncodingRoundTripsWatchIdentity(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	want := WatchState{
		ID: "watch-42", ActionID: 42, ExecutedAt: now,
		Table: "public.orders", IndexName: "idx_orders_customer",
		Criterion: Criterion{
			Kind: "per_query_latency", TargetIDs: []int64{7, 9},
			Window: 15 * time.Minute, HardMax: time.Hour,
		},
		Status: "extended", Reason: "insufficient_samples",
		Window: 30 * time.Minute, NextEvaluationAt: now.Add(30 * time.Minute),
	}
	criterion, baseline, err := marshalState(want)
	if err != nil {
		t.Fatalf("marshalState() error = %v", err)
	}
	scanner := &fakeStateScanner{
		actionID: want.ActionID, criterion: criterion, baseline: baseline,
		status: want.Status, reason: want.Reason, due: want.NextEvaluationAt,
	}
	got, err := (&PostgresStateStore{}).scanState(scanner)
	if err != nil {
		t.Fatalf("scanState() error = %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scanState() = %#v, want %#v", got, want)
	}
}

func TestPostgresStateEncodingRejectsMalformedJSON(t *testing.T) {
	scanner := &fakeStateScanner{
		actionID: 1, criterion: []byte("{"), baseline: []byte("{}"),
	}
	if _, err := (&PostgresStateStore{}).scanState(scanner); err == nil {
		t.Fatal("scanState() accepted malformed criterion JSON")
	}
	scanner.criterion = []byte("{}")
	scanner.baseline = []byte("{")
	if _, err := (&PostgresStateStore{}).scanState(scanner); err == nil {
		t.Fatal("scanState() accepted malformed watch JSON")
	}
}

func TestDatabaseVerdictNormalizesPersistenceValues(t *testing.T) {
	for input, want := range map[string]string{
		"": "pending", "reverted": "revert", "success": "success",
	} {
		if got := databaseVerdict(input); got != want {
			t.Fatalf("databaseVerdict(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestPostgresObservationSourceReadsCollectorEvidence(t *testing.T) {
	queryer := &fakeRowQuerier{rows: []pgx.Row{
		&valueRow{values: []any{int64(40), float64(12.5)}},
		&valueRow{values: []any{int64(25), float64(8)}},
		&valueRow{values: []any{4, float64(30)}},
		&valueRow{values: []any{true}},
	}}
	source := &PostgresObservationSource{queryer: queryer}
	from, to := time.Now().Add(-time.Hour), time.Now()
	queries, err := source.QueryMeasurements(
		context.Background(), []int64{7, 9}, from, to,
	)
	if err != nil || queries[7].Samples != 40 || queries[9].Samples != 25 {
		t.Fatalf("QueryMeasurements() = %#v, %v", queries, err)
	}
	writes, err := source.WriteMeasurements(context.Background(), "orders", from, to)
	if err != nil || writes.Samples != 4 || writes.AverageLatency != 10*time.Millisecond {
		t.Fatalf("WriteMeasurements() = %#v, %v", writes, err)
	}
	valid, err := source.IndexValid(context.Background(), "idx_orders")
	if err != nil || !valid {
		t.Fatalf("IndexValid() = %v, %v", valid, err)
	}
	load, err := source.CurrentLoad(context.Background())
	if !errors.Is(err, ErrLoadTelemetryUnavailable) || load != (LoadSample{}) {
		t.Fatalf("CurrentLoad() = %#v, %v", load, err)
	}
}

func TestPostgresObservationSourcePropagatesQueryErrors(t *testing.T) {
	want := errors.New("query failed")
	source := &PostgresObservationSource{
		queryer: &fakeRowQuerier{rows: []pgx.Row{&valueRow{err: want}}},
	}
	_, err := source.QueryMeasurements(
		context.Background(), []int64{7}, time.Time{}, time.Now(),
	)
	if !errors.Is(err, want) {
		t.Fatalf("QueryMeasurements() error = %v, want %v", err, want)
	}
}

type fakeStateScanner struct {
	actionID  int64
	criterion []byte
	baseline  []byte
	status    string
	reason    string
	completed bool
	due       time.Time
	err       error
}

type fakeRowQuerier struct {
	rows []pgx.Row
	next int
}

func (q *fakeRowQuerier) QueryRow(context.Context, string, ...any) pgx.Row {
	row := q.rows[q.next]
	q.next++
	return row
}

type valueRow struct {
	values []any
	err    error
}

func (r *valueRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	for index, value := range r.values {
		switch target := dest[index].(type) {
		case *int:
			*target = value.(int)
		case *int64:
			*target = value.(int64)
		case *float64:
			*target = value.(float64)
		case *bool:
			*target = value.(bool)
		}
	}
	return nil
}

func (s *fakeStateScanner) Scan(dest ...any) error {
	if s.err != nil {
		return s.err
	}
	if len(dest) != 7 {
		return errors.New("unexpected scan destination count")
	}
	*(dest[0].(*int64)) = s.actionID
	*(dest[1].(*[]byte)) = s.criterion
	*(dest[2].(*[]byte)) = s.baseline
	*(dest[3].(*string)) = s.status
	*(dest[4].(*string)) = s.reason
	*(dest[5].(*bool)) = s.completed
	*(dest[6].(*time.Time)) = s.due
	return nil
}
