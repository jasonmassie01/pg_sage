package changefeed

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// The provider_api source (guard-02b): the cloud provider's maintenance
// and failover events, read through a ProviderEventReader, become feed
// events with source provider_api.

type fakeReader struct {
	mu     sync.Mutex
	events []ProviderEvent
	err    error
	since  []time.Time
}

func (r *fakeReader) ProviderEvents(_ context.Context, since time.Time) ([]ProviderEvent,
	error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.since = append(r.since, since)
	return r.events, r.err
}

func apiPoller(r ProviderEventReader) *Poller {
	return &Poller{providerAPI: r, logf: func(string, string, ...any) {},
		now: func() time.Time { return pollAt }}
}

// Happy path and first sight: the first read reaches back a day; mapped
// kinds become provider_api events; the cursor moves to the latest event.
func TestProviderEvent_APIFirstReadAndCursor(t *testing.T) {
	at := pollAt.Add(-2 * time.Hour)
	r := &fakeReader{events: []ProviderEvent{
		{ID: "rds:orders-db:1", Kind: KindFailover, Summary: "Multi-AZ failover", At: at},
		{ID: "rds:orders-db:2", Kind: KindVersionChange, Summary: "patched",
			At: at.Add(time.Minute)},
		{ID: "rds:orders-db:3", Kind: KindDeploy, Summary: "not a provider kind", At: at},
		{ID: "", Kind: KindRestart, Summary: "no id", At: at},
	}}
	p := apiPoller(r)
	evs, state, err := p.pollProviderAPI(context.Background(), nil, nil, pollAt)
	if err != nil || len(evs) != 2 {
		t.Fatalf("events = %+v, err = %v, want the failover and the version change", evs, err)
	}
	if !r.since[0].Equal(pollAt.Add(-providerAPIFirstLook)) {
		t.Fatalf("first read since %v, want a day back", r.since[0])
	}
	for _, e := range evs {
		if e.Source != SourceProviderAPI || e.Signature != SignatureInternal || !e.Scoped ||
			e.Hash != hashOf(e) {
			t.Fatalf("event = %+v", e)
		}
	}
	st := state.(providerAPIState)
	if st.Since != at.Add(time.Minute).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("cursor = %q, want the latest event", st.Since)
	}
	r.events = r.events[:1]
	evs, _, err = p.pollProviderAPI(context.Background(), nil, mustState(t, st), pollAt)
	if err != nil || len(evs) != 0 {
		t.Fatalf("re-read before the cursor: events %+v err %v", evs, err)
	}
	if !r.since[1].Equal(at.Add(time.Minute)) {
		t.Fatalf("second read since %v, want the cursor", r.since[1])
	}
}

// Error propagation, empty reads and a corrupt cursor.
func TestProviderEvent_APIErrorsAndEmpty(t *testing.T) {
	denied := errors.New("AccessDenied: rds:DescribeEvents")
	p := apiPoller(&fakeReader{err: denied})
	if _, _, err := p.pollProviderAPI(context.Background(), nil, nil, pollAt); !errors.Is(
		err, denied) {
		t.Fatalf("err = %v, want the reader's error", err)
	}
	p = apiPoller(&fakeReader{})
	evs, state, err := p.pollProviderAPI(context.Background(), nil, nil, pollAt)
	if err != nil || len(evs) != 0 {
		t.Fatalf("empty read: %+v %v", evs, err)
	}
	if st := state.(providerAPIState); st.Since !=
		pollAt.Add(-providerAPIFirstLook).UTC().Format(time.RFC3339Nano) {
		t.Fatalf("empty read cursor = %q, want the first-look start", st.Since)
	}
	if _, _, err := p.pollProviderAPI(context.Background(), nil, []byte(`{`),
		pollAt); err == nil {
		t.Fatal("a corrupt cursor was accepted")
	}
}

// The source is listed only with a reader.
func TestProviderEvent_APISourceOnlyWithReader(t *testing.T) {
	for _, withReader := range []bool{false, true} {
		p := &Poller{providerOnly: true}
		if withReader {
			p.providerAPI = &fakeReader{}
		}
		var found bool
		for _, s := range p.sources() {
			found = found || s.name == SourceProviderAPI
		}
		if found != withReader {
			t.Fatalf("reader %v: provider_api listed %v", withReader, found)
		}
	}
}
