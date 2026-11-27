package lakebaseobs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestDeliveryFailureClassification(t *testing.T) {
	cases := []struct {
		name        string
		err         error
		class       string
		unavailable bool
	}{
		{"plain sink error", errors.New("boom"), "sink_error", false},
		{"deadline", fmt.Errorf("sink: %w", context.DeadlineExceeded), "timeout", true},
		{"canceled", context.Canceled, "canceled", true},
		{"connection exception", &pgconn.PgError{Code: "08006"}, "sqlstate_08006", true},
		{"operator intervention", fmt.Errorf("x: %w", &pgconn.PgError{Code: "57P01"}),
			"sqlstate_57P01", true},
		{"deadlock", &pgconn.PgError{Code: "40P01"}, "sqlstate_40P01", false},
		{"constraint", &pgconn.PgError{Code: "23505"}, "sqlstate_23505", false},
		{"connect", fmt.Errorf("acquire: %w", &pgconn.ConnectError{}), "connect", true},
		{"schema", ErrSchema, "schema", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := failureClass(c.err); got != c.class {
				t.Fatalf("failureClass = %q, want %q", got, c.class)
			}
			if got := sinkUnavailable(c.err); got != c.unavailable {
				t.Fatalf("sinkUnavailable = %v, want %v", got, c.unavailable)
			}
		})
	}
	// nil is never a failure; asking for its class is a caller bug that must
	// not panic or masquerade as a sink outage.
	if failureClass(nil) != "sink_error" || sinkUnavailable(nil) {
		t.Fatal("nil error classified as an outage")
	}
}

func TestRetryDelayDoublesAndIsCapped(t *testing.T) {
	want := []time.Duration{30 * time.Second, time.Minute, 2 * time.Minute,
		4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := retryDelay(i + 1); got != w {
			t.Fatalf("retryDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
	if retryDelay(0) != retryBaseDelay || retryDelay(-3) != retryBaseDelay {
		t.Fatal("non-positive attempt counts must use the base delay")
	}
}

// Validation runs before any database access, so a Store without a pool
// proves no statement is sent for invalid input.
func TestStoreDeliveryInputValidation(t *testing.T) {
	s := &Store{}
	ctx := context.Background()
	now := time.Now()
	if _, err := s.PendingStreams(ctx, "b", "bad name!", nil, 10, now); !errors.Is(err, ErrScope) {
		t.Fatalf("invalid consumer accepted: %v", err)
	}
	_, err := s.PendingStreams(ctx, "b", "ok.v1", []Stream{"no_such_stream"}, 10, now)
	if !errors.Is(err, ErrScope) {
		t.Fatalf("unknown stream accepted: %v", err)
	}
	if _, err := s.PendingStreams(ctx, "b", "ok.v1", nil, 0, now); !errors.Is(err, ErrScope) {
		t.Fatalf("zero limit accepted: %v", err)
	}
	invalid := [][3]string{{"b", "e", "bad name!"}, {"", "e", "ok.v1"}, {"b", "", "ok.v1"}}
	for _, in := range invalid {
		_, err = s.RecordDeliveryFailure(ctx, in[0], in[1], in[2], "sink_error", now)
		if !errors.Is(err, ErrScope) {
			t.Fatalf("invalid failure record %v accepted: %v", in, err)
		}
	}
	if _, err := s.Backlog(ctx, "b", "bad name!", nil, now); !errors.Is(err, ErrScope) {
		t.Fatalf("invalid consumer accepted: %v", err)
	}
	if _, err := s.Backlog(ctx, "", "ok.v1", nil, now); !errors.Is(err, ErrScope) {
		t.Fatalf("empty binding accepted: %v", err)
	}
	_, err = s.CommitBatchAcked(ctx, "b", Plans, Cursor{}, Cursor{}, nil, SourceRetention,
		[]string{"bad name!"})
	if !errors.Is(err, ErrScope) {
		t.Fatalf("invalid at-commit consumer accepted: %v", err)
	}
}

type streamsConsumer struct {
	ConsumerFunc
	streams []Stream
}

func (c streamsConsumer) Streams() []Stream { return c.streams }

func TestNewRuntimeSubscriptions(t *testing.T) {
	sink := func(context.Context, Event) error { return nil }
	o := Options{WarehouseID: "test", Streams: []Stream{Plans, Logs, ASH}}
	unknown := streamsConsumer{ConsumerFunc{"unknown.v1", sink}, []Stream{"no_such_stream"}}
	if _, err := NewRuntime(readerFunc(nil), &Store{}, testScope(), o, unknown); err == nil {
		t.Fatal("subscription to an unknown stream accepted")
	}
	plans := streamsConsumer{ConsumerFunc{"plans.v1", sink}, []Stream{Plans}}
	all := ConsumerFunc{"all.v1", sink}
	empty := streamsConsumer{ConsumerFunc{"empty.v1", sink}, []Stream{}}
	rt, err := NewRuntime(readerFunc(nil), &Store{}, testScope(), o, plans, all, empty)
	if err != nil {
		t.Fatal(err)
	}
	if got := rt.subscribed["plans.v1"]; len(got) != 1 || got[0] != Plans {
		t.Fatalf("plans consumer subscription = %v", got)
	}
	if rt.subscribed["all.v1"] != nil || rt.subscribed["empty.v1"] != nil {
		t.Fatal("a consumer without streams must keep all-stream delivery")
	}
	for _, s := range []Stream{Logs, ASH} {
		if got := rt.atCommit[s]; len(got) != 1 || got[0].Name() != "plans.v1" {
			t.Fatalf("stream %s at-commit consumers = %v", s, got)
		}
	}
	if len(rt.atCommit[Plans]) != 0 {
		t.Fatal("a subscribed stream must not be acknowledged at commit")
	}
	if d := rt.Deliveries(); d == nil || len(d) != 0 {
		t.Fatalf("no backlog is known before the first poll: %v", d)
	}
}

// memCursors cannot acknowledge inside a commit; a subscription that needs
// it must fail the commit rather than silently leave events unacknowledged.
func TestAtCommitAckRequiresAnAckingStore(t *testing.T) {
	now := time.Now().UTC()
	o := Options{WarehouseID: "test", AllowPayload: true, Streams: []Stream{Logs},
		Window: SourceRetention}
	e := eventFor(t, Logs, now.Add(-time.Hour))
	plans := streamsConsumer{ConsumerFunc{"plans.v1", func(context.Context, Event) error {
		return nil
	}}, []Stream{Plans}}
	rt, err := NewRuntime(staticReader([]Event{e}), &Store{}, testScope(), o, plans)
	if err != nil {
		t.Fatal(err)
	}
	mem := newMemCursors()
	errs := rt.collect(context.Background(), mem, now)
	if len(errs) == 0 || !errors.Is(errors.Join(errs...), errAckUnsupported) {
		t.Fatalf("commit without at-commit acknowledgement accepted: %v", errs)
	}
	if mem.cursor(Logs).Revision != 0 {
		t.Fatal("cursor advanced past events that were never acknowledged")
	}
}
