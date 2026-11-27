package lakebaseobs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func insertBacklogRows(t *testing.T, s *Store, binding string, base time.Time, from, to int) {
	t.Helper()
	_, err := s.pool.Exec(context.Background(), `INSERT INTO sage.lakebaseobs_inbox
 (binding,event_id,stream,occurred_at,received_at,payload_until,identity_until,event)
 SELECT $1,'b'||g,CASE WHEN g%2=0 THEN 'plan_history' ELSE 'postgres_logs' END,
 $2::timestamptz+g*interval '1 second',$2,$2+interval '5 hour',$2+interval '9 day','{}'::jsonb
 FROM generate_series($3::int,$4::int) g`, binding, base, from, to)
	if err != nil {
		t.Fatal(err)
	}
}

func TestDBBacklogCapsAtLimitAndReportsOldest(t *testing.T) {
	s, binding := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	base := now.Add(-time.Hour)
	b, err := s.Backlog(ctx, binding, "backlog.v1", nil, now)
	if err != nil || b.Rows != 0 || b.Capped || !b.Oldest.IsZero() {
		t.Fatalf("empty backlog %+v %v", b, err)
	}
	insertBacklogRows(t, s, binding, base, 1, 10000)
	b, err = s.Backlog(ctx, binding, "backlog.v1", nil, now)
	if err != nil || b.Rows != 10000 || b.Capped || !b.Oldest.Equal(base.Add(time.Second)) {
		t.Fatalf("10000 rows reported as %+v %v", b, err)
	}
	b, err = s.Backlog(ctx, binding, "backlog.v1", []Stream{Plans}, now)
	if err != nil || b.Rows != 5000 || b.Capped || !b.Oldest.Equal(base.Add(2*time.Second)) {
		t.Fatalf("plan-only backlog %+v %v", b, err)
	}
	insertBacklogRows(t, s, binding, base, 10001, 10001)
	b, err = s.Backlog(ctx, binding, "backlog.v1", nil, now)
	if err != nil || b.Rows != backlogLimit || !b.Capped {
		t.Fatalf("10001 rows reported as %+v %v", b, err)
	}
	insertBacklogRows(t, s, binding, base, 10002, 10100)
	b, err = s.Backlog(ctx, binding, "backlog.v1", nil, now)
	if err != nil || b.Rows != backlogLimit || !b.Capped || !b.Oldest.Equal(base.Add(time.Second)) {
		t.Fatalf("10100 rows reported as %+v %v", b, err)
	}
	if err = s.Ack(ctx, binding, "b1", "backlog.v1"); err != nil {
		t.Fatal(err)
	}
	b, err = s.Backlog(ctx, binding, "backlog.v1", nil, now)
	if err != nil || !b.Oldest.Equal(base.Add(2*time.Second)) {
		t.Fatalf("acknowledged row still counted as oldest: %+v %v", b, err)
	}
	// Rows whose payload expired can never be delivered, so they are no backlog.
	b, err = s.Backlog(ctx, binding, "backlog.v1", nil, base.Add(6*time.Hour))
	if err != nil || b.Rows != 0 || !b.Oldest.IsZero() {
		t.Fatalf("expired rows counted: %+v %v", b, err)
	}
}

func TestDBAtCommitAckOncePerStoredEvent(t *testing.T) {
	s, _, binding, e := fenceOwner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	acks := []string{"fenced_ack.v1"}
	inserted, err := s.CommitBatchAcked(ctx, binding, Plans, Cursor{}, Cursor{}, []Event{e, e},
		SourceRetention, acks)
	if err != nil || len(inserted) != 1 || inserted[0].ID != e.ID {
		t.Fatalf("first commit inserted %d events: %v", len(inserted), err)
	}
	c, err := s.LoadCursor(ctx, binding, Plans)
	if err != nil {
		t.Fatal(err)
	}
	inserted, err = s.CommitBatchAcked(ctx, binding, Plans, c, c, []Event{e}, SourceRetention, acks)
	if err != nil || len(inserted) != 0 {
		t.Fatalf("a re-read event was reported as new: %d %v", len(inserted), err)
	}
	if n := ackCount(t, s, binding, "fenced_ack.v1"); n != 1 {
		t.Fatalf("acks after re-read = %d", n)
	}
}

func TestDBAtCommitAckIsFencedByRevocation(t *testing.T) {
	s, scope, binding, e := fenceOwner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	revoke, err := s.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = revoke.Rollback(ctx) }()
	err = s.setOwnerPolicy(ctx, revoke, scope.DeploymentID, scope.DatabaseID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, cerr := s.CommitBatchAcked(ctx, binding, Plans, Cursor{}, Cursor{}, []Event{e},
			SourceRetention, []string{"fenced_ack.v1"})
		done <- cerr
	}()
	waitForPolicyLock(t, s)
	if err = revoke.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = <-done; !errors.Is(err, ErrPolicy) {
		t.Fatalf("at-commit ack accepted after revoke: %v", err)
	}
	var rows int
	err = s.pool.QueryRow(ctx, `SELECT count(*) FROM sage.lakebaseobs_inbox WHERE binding=$1`,
		binding).Scan(&rows)
	if err != nil || rows != 0 {
		t.Fatalf("revoked batch stored its event: %d %v", rows, err)
	}
	if n := ackCount(t, s, binding, "fenced_ack.v1"); n != 0 {
		t.Fatalf("revoked batch acknowledged: acks = %d", n)
	}
}

func TestDBTwoRuntimesOneBindingNeverDoubleAck(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	o := Options{WarehouseID: "test", AllowPayload: true, Streams: []Stream{Plans, Logs},
		Window: SourceRetention}
	var events []Event
	first := newRecording("shared.v1", []Stream{Plans})
	second := newRecording("shared.v1", []Stream{Plans})
	rtA, s := runtimeStore(t, sliceReader(&events), o, first)
	rtB, err := NewRuntime(sliceReader(&events), s, rtA.Scope(), o, second)
	if err != nil || rtB.Binding() != rtA.Binding() {
		t.Fatalf("second runtime binding: %v", err)
	}
	streams := []Stream{Plans, Logs, Logs, Logs, Logs, Logs, Logs}
	events = mixedEvents(t, rtA.Scope(), streams, now.Add(-time.Hour), 70)
	logs := 60
	for round := range 2 {
		pollConcurrently(t, now.Add(time.Duration(round)*time.Minute), rtA, rtB)
	}
	_, observedA := first.totals()
	_, observedB := second.totals()
	if observedA+observedB != logs {
		t.Fatalf("non-plan outcomes counted %d times for %d events", observedA+observedB, logs)
	}
	var acks, distinct, attempts int
	err = s.pool.QueryRow(ctx, `SELECT count(*),count(DISTINCT event_id),
 (SELECT count(*) FROM sage.lakebaseobs_attempts WHERE binding=$1)
 FROM sage.lakebaseobs_acks WHERE binding=$1 AND consumer='shared.v1'`,
		rtA.Binding()).Scan(&acks, &distinct, &attempts)
	if err != nil || acks != len(events) || distinct != acks || attempts != 0 {
		t.Fatalf("acks=%d distinct=%d attempts=%d err=%v, want %d", acks, distinct, attempts,
			err, len(events))
	}
	for _, e := range events {
		if e.Stream == Plans && !first.sink[e.ID] && !second.sink[e.ID] {
			t.Fatalf("plan %s acknowledged without reaching a sink", e.ID)
		}
	}
}

// pollConcurrently polls every runtime at once. A cursor conflict between
// them is expected; any other error fails the test.
func pollConcurrently(t *testing.T, now time.Time, runtimes ...*Runtime) {
	t.Helper()
	var wg sync.WaitGroup
	errs := make(chan error, len(runtimes))
	for _, rt := range runtimes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- rt.Poll(context.Background(), now)
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil && !errors.Is(err, ErrConflict) {
			t.Fatal(err)
		}
	}
}

func TestDBConsumerWithoutStreamsKeepsAllStreamDelivery(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	o := Options{WarehouseID: "test", AllowPayload: true, Streams: []Stream{Plans, Logs},
		Window: SourceRetention}
	var events []Event
	var mu sync.Mutex
	legacy := map[Stream]int{}
	plain := ConsumerFunc{"legacy.v1", func(_ context.Context, e Event) error {
		mu.Lock()
		defer mu.Unlock()
		legacy[e.Stream]++
		return nil
	}}
	empty := newRecording("empty.v1", []Stream{})
	rt, s := runtimeStore(t, sliceReader(&events), o, plain, empty)
	for i := range 5 {
		stream := Logs
		if i < 2 {
			stream = Plans
		}
		at := now.Add(-time.Hour + time.Duration(i)*time.Second)
		events = append(events, deliveryEvent(t, rt.Scope(), stream, at))
	}
	if err := rt.Poll(ctx, now); err != nil {
		t.Fatal(err)
	}
	if legacy[Plans] != 2 || legacy[Logs] != 3 {
		t.Fatalf("consumer without Streams() lost streams: %v", legacy)
	}
	delivered, observed := empty.totals()
	if delivered != 5 || observed != 0 {
		t.Fatalf("empty Streams() must mean all streams: delivered=%d observed=%d",
			delivered, observed)
	}
	for _, name := range []string{"legacy.v1", "empty.v1"} {
		if n := ackCount(t, s, rt.Binding(), name); n != 5 {
			t.Fatalf("%s acks = %d", name, n)
		}
	}
}

func TestDBRecordDeliveryFailureConcurrentCapAndVanished(t *testing.T) {
	s, binding := dbStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	e := eventFor(t, Plans, now.Add(-time.Hour))
	err := s.CommitBatch(ctx, binding, Plans, Cursor{}, Cursor{}, []Event{e}, SourceRetention)
	if err != nil {
		t.Fatal(err)
	}
	if seen := recordConcurrently(t, s, binding, e.ID, now); !seen[1] || !seen[2] {
		t.Fatalf("concurrent failures lost an attempt: %v", seen)
	}
	var next time.Time
	if err := s.pool.QueryRow(ctx, `SELECT next_at FROM sage.lakebaseobs_attempts
WHERE binding=$1 AND event_id=$2`, binding, e.ID).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if want := now.Add(retryDelay(2)); next.Sub(want).Abs() > time.Millisecond {
		t.Fatalf("next_at = %v, want %v", next, want)
	}
	for want := 3; want <= MaxDeliveryAttempts; want++ {
		n, err := s.RecordDeliveryFailure(ctx, binding, e.ID, "race.v1", "sink_error", now)
		if err != nil || n != want {
			t.Fatalf("attempt %d recorded as %d: %v", want, n, err)
		}
	}
	if acks := ackCount(t, s, binding, "race.v1"); acks != 1 {
		t.Fatalf("exhausted event acks = %d", acks)
	}
	pending, err := s.Pending(ctx, binding, "race.v1", 10, now.Add(time.Hour))
	if err != nil || len(pending) != 0 {
		t.Fatalf("exhausted event still pending: %d %v", len(pending), err)
	}
	vanished := "vanished-" + uuid.NewString()
	n, err := s.RecordDeliveryFailure(ctx, binding, vanished, "race.v1", "sink_error", now)
	if err != nil || n != 0 {
		t.Fatalf("a purged event must record nothing: %d %v", n, err)
	}
}

func recordConcurrently(t *testing.T, s *Store, binding, id string, now time.Time) map[int]bool {
	t.Helper()
	var wg sync.WaitGroup
	results := make(chan int, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n, err := s.RecordDeliveryFailure(context.Background(), binding, id, "race.v1",
				"sink_error", now)
			if err != nil {
				t.Error(err)
			}
			results <- n
		}()
	}
	wg.Wait()
	close(results)
	seen := map[int]bool{}
	for n := range results {
		seen[n] = true
	}
	return seen
}

func TestDBSchemaUpgradeFromV5RunsTwice(t *testing.T) {
	s, _ := dbStore(t)
	ctx := context.Background()
	_, err := s.pool.Exec(ctx, `DROP TABLE sage.lakebaseobs_attempts;
DROP INDEX sage.lakebaseobs_inbox_delivery_stream;
DELETE FROM sage.lakebaseobs_schema WHERE version=6`)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err = s.EnsureSchema(ctx); err != nil {
			t.Fatal(err)
		}
	}
	var version int
	var table, index bool
	err = s.pool.QueryRow(ctx, `SELECT (SELECT max(version) FROM sage.lakebaseobs_schema),
 to_regclass('sage.lakebaseobs_attempts') IS NOT NULL,
 to_regclass('sage.lakebaseobs_inbox_delivery_stream') IS NOT NULL`).Scan(&version, &table, &index)
	if err != nil || version != 6 || !table || !index {
		t.Fatalf("upgrade: version=%d attempts=%v index=%v err=%v", version, table, index, err)
	}
}
