package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/pg-sage/sidecar/internal/cloudtel"
	"github.com/pg-sage/sidecar/internal/sre/changefeed"
)

// guard-02b: the cloud telemetry runtime's provider events reach the
// change feed's provider_api source, mapped to feed kinds.

type eventsSource struct {
	events []cloudtel.ProviderEvent
	err    error
}

func (eventsSource) Provider() string { return "rds" }

func (eventsSource) Collect(context.Context, time.Time) (cloudtel.Sample, error) {
	return cloudtel.Sample{}, errors.New("not collected in this test")
}

func (s eventsSource) ProviderEvents(context.Context, time.Time) ([]cloudtel.ProviderEvent,
	error) {
	return s.events, s.err
}

type plainSource struct{ eventsSource }

func (plainSource) ProviderEvents() {}

func TestProviderEventReader_MapsCloudCategories(t *testing.T) {
	at := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	rt, err := cloudtel.NewRuntime(eventsSource{events: []cloudtel.ProviderEvent{
		{ID: "rds:db:1", Category: cloudtel.EventFailover, Message: "failover", At: at},
		{ID: "rds:db:2", Category: cloudtel.EventVersionChange, Message: "patched", At: at},
		{ID: "rds:db:3", Category: cloudtel.EventRestart, Message: "offline", At: at},
		{ID: "rds:db:4", Category: "notification", Message: "renamed", At: at},
	}}, cloudtel.RuntimeOptions{Database: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	r := providerEventReaderFor(rt)
	if r == nil {
		t.Fatal("no reader for a runtime whose source reads provider events")
	}
	evs, err := r.ProviderEvents(context.Background(), at.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	want := []changefeed.Kind{changefeed.KindFailover, changefeed.KindVersionChange,
		changefeed.KindRestart}
	if len(evs) != len(want) {
		t.Fatalf("events = %+v, want %v (the unmapped category dropped)", evs, want)
	}
	for i, e := range evs {
		if e.Kind != want[i] || e.ID == "" || !e.At.Equal(at) || e.Summary == "" {
			t.Errorf("event %d = %+v, want kind %s", i, e, want[i])
		}
	}
}

func TestProviderEventReader_NilWithoutCapability(t *testing.T) {
	if providerEventReaderFor(nil) != nil {
		t.Fatal("reader without cloud telemetry")
	}
	if providerEventReaderFor(cloudtel.Unavailable("orders", "rds", "no creds")) != nil {
		t.Fatal("reader for unavailable telemetry")
	}
	rt, err := cloudtel.NewRuntime(plainSource{}, cloudtel.RuntimeOptions{Database: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if providerEventReaderFor(rt) != nil {
		t.Fatal("reader for a source without an event API")
	}
}

func TestProviderEventReader_PropagatesErrors(t *testing.T) {
	denied := errors.New("AccessDenied: rds:DescribeEvents")
	rt, err := cloudtel.NewRuntime(eventsSource{err: denied},
		cloudtel.RuntimeOptions{Database: "orders"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := providerEventReaderFor(rt).ProviderEvents(context.Background(),
		time.Now()); !errors.Is(err, denied) {
		t.Fatalf("err = %v, want the provider's error", err)
	}
}
