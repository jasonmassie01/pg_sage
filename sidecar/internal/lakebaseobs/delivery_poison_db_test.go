package lakebaseobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// staticReader serves fixed events by stream and window, like a provider
// table that already holds them.
func staticReader(events []Event) readerFunc {
	return func(_ context.Context, s Stream, _ Scope, w Window) (Batch, error) {
		out := []Event{}
		for _, e := range events {
			if e.Stream == s && !e.OccurredAt.Before(w.From) && e.OccurredAt.Before(w.To) {
				out = append(out, e)
			}
		}
		return Batch{Events: out}, nil
	}
}

func deliveryEvent(t *testing.T, scope Scope, s Stream, at time.Time) Event {
	t.Helper()
	e, err := NewEvent(s, scope, testFields(s, at), at.Add(time.Minute), true)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// A poison event is the oldest pending row, so before this item it came first
// on every poll and its failure stopped every later event behind it.
func TestDBPoisonEventDoesNotStallLaterEventsInSamePoll(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	o := testOptions()
	o.Streams = []Stream{Plans}
	o.Window = SourceRetention
	var events []Event
	delivered := map[string]int{}
	var poisonID string
	consumer := ConsumerFunc{"poison.v1", func(_ context.Context, e Event) error {
		delivered[e.ID]++
		if e.ID == poisonID {
			return errors.New("sink rejects this event")
		}
		return nil
	}}
	reader := readerFunc(func(c context.Context, s Stream, sc Scope, w Window) (Batch, error) {
		return staticReader(events)(c, s, sc, w)
	})
	rt, s := runtimeStore(t, reader, o, consumer)
	scope := rt.Scope()
	for i := range 3 {
		at := now.Add(-time.Hour + time.Duration(i)*time.Second)
		events = append(events, deliveryEvent(t, scope, Plans, at))
	}
	poisonID = events[0].ID
	err := rt.Poll(ctx, now)
	if err == nil || !strings.Contains(err.Error(), "poison.v1") {
		t.Fatalf("a failed delivery must still be reported by the poll, got %v", err)
	}
	if delivered[events[1].ID] != 1 || delivered[events[2].ID] != 1 {
		t.Fatalf("events behind the poison event not delivered in the same poll: %v", delivered)
	}
	pending, err := s.Pending(ctx, rt.Binding(), "poison.v1", 10, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("poison event must wait for its retry time, pending=%d", len(pending))
	}
	pending, err = s.Pending(ctx, rt.Binding(), "poison.v1", 10, now.Add(time.Minute))
	if err != nil || len(pending) != 1 || pending[0].ID != poisonID {
		t.Fatalf("poison event must be retried after its backoff: %d %v", len(pending), err)
	}
}
